package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

/*
Targets are dynamic -- an operator creates them here, at runtime -- while the two gates that decide
whether an agent can reach one are static: config.checkers.external.allowedCidrs (enforced by the
agent) and networkPolicy.externalEgress (enforced by the cluster). A target outside them is accepted
today and then fails as a bare timeout on every probe, naming nothing.

This refuses it at the moment it is created, which is the only moment the operator is looking. The
Console cannot write NetworkPolicies to fix it for them and should not hold the RBAC to try: the two
values keys are named in the refusal instead.
*/

// lookupNetIP is net.DefaultResolver's, behind a var so the multi-address decision below can be
// tested without DNS -- the same seam checker.Allowlist takes as its Resolver parameter.
var lookupNetIP = net.DefaultResolver.LookupNetIP

// allowlistTTL keeps the controller's answer for a while: it changes only when the operator rolls
// a new config, and a target write must not wait on a controller round trip every time.
const allowlistTTL = 30 * time.Second

// allowlistGrace is how long the last authoritative list keeps answering while the controller is
// unreachable -- a rollout must not silently suspend the guard.
const allowlistGrace = 5 * allowlistTTL

// externalAllowlist is the CIDR set the fleet's agents probe by, as last read from the controller.
type externalAllowlist struct {
	prefixes []netip.Prefix
	raw      []string
	fetched  time.Time
	// known is false when the controller could not be asked, or answered with no list at all: the
	// Console then makes no claim about reachability rather than guessing one.
	known bool
}

// targetAllowlist reads the allowlist the controller publishes on GET /api/v1/version, cached.
func (s *Server) targetAllowlist(ctx context.Context) externalAllowlist {
	s.allowlistMu.Lock()
	cached := s.allowlist
	s.allowlistMu.Unlock()

	// `fetched`, not `known`: "the external checker is off" is as much an answer as a list, and
	// caching only the positive one meant every target write re-asked the controller.
	if !cached.fetched.IsZero() && time.Since(cached.fetched) < allowlistTTL {
		return cached
	}
	if s.ctrl == nil {
		return externalAllowlist{}
	}
	// Asked WITHOUT the lock: it is one process-global mutex, and holding it across a controller
	// round trip serialized every concurrent target write behind that round trip.
	v, err := s.ctrl.Version(ctx)
	if err != nil {
		// A controller we cannot reach does not un-know the list we already had: serving the last
		// authoritative answer for a while keeps the refusal alive across a rollout instead of
		// dropping it for the length of the outage. `fetched` is left untouched, so the grace
		// window expires on its own.
		if cached.known && time.Since(cached.fetched) < allowlistGrace {
			return cached
		}
		return externalAllowlist{}
	}
	out := externalAllowlist{raw: nil, fetched: time.Now()}
	if v != nil {
		for _, c := range v.ExternalAllowedCIDRs {
			if p, perr := netip.ParsePrefix(strings.TrimSpace(c)); perr == nil {
				out.prefixes = append(out.prefixes, p)
				out.raw = append(out.raw, c)
			}
		}
	}
	// Known only with a usable list; an empty answer is still cached, so the next write does not
	// re-ask, but it makes no claim about any address.
	out.known = len(out.prefixes) > 0

	s.allowlistMu.Lock()
	s.allowlist = out
	s.allowlistMu.Unlock()
	return out
}

/*
hostOf reduces a target address to the host an agent will actually resolve.

A URL target is the case SplitHostPort silently gets wrong rather than rejects: "https://h/x" splits
at the LAST colon into host "https" and port "//h/x", with no error, because SplitHostPort never
validates the port. The refusal below then judged the label "https" and never fired for any
kind=url target at all. url.Hostname() is the same extraction the checker's own allowlist does, so
the two agree on what is being authorized.
*/
func hostOf(address string) string {
	a := strings.TrimSpace(address)
	// u.Host, not u.Scheme: "example.test:8080" also parses as a URL (scheme
	// "example.test", opaque "8080"), and only a "//" form carries a host.
	if u, err := url.Parse(a); err == nil && u.Host != "" {
		return u.Hostname()
	}
	if h, _, err := net.SplitHostPort(a); err == nil {
		return h
	}
	return strings.Trim(a, "[]")
}

/*
targetOutsideAllowlist answers "no agent can ever probe this address", and only when it is certain.

A hostname is resolved HERE, once, and judged on what it resolves to now. That is a snapshot: a name
can move tomorrow, and the agent resolves it again at probe time anyway. So a name that resolves to
nothing at all, or cannot be resolved from the Console's own network, is NOT refused -- the refusal
is reserved for an address that is provably outside every allowed prefix.
*/
func (s *Server) targetOutsideAllowlist(ctx context.Context, address string) (allow externalAllowlist, outside bool) {
	list := s.targetAllowlist(ctx)
	if !list.known {
		return list, false
	}
	host := hostOf(address)
	if host == "" {
		return list, false
	}

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		resolveCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		names, err := lookupNetIP(resolveCtx, "ip", host)
		if err != nil || len(names) == 0 {
			return list, false
		}
		addrs = names
	}

	// All-must-pass, because that is the agent's rule: checker.Allowlist refuses the whole name on
	// the FIRST resolved address outside the list, so one uncovered A or AAAA record makes the name
	// unprobeable however many others are covered. Matching that polarity here is what keeps the
	// create-time answer and the probe-time answer the same answer.
	for _, a := range addrs {
		if !allowlistCoversAddr(&list, a) {
			return list, true
		}
	}
	return list, false
}

// allowlistCoversAddr is the containment question alone; an unknown list answers no to everything,
// which is why callers must check `known` before reading anything into a false.
func allowlistCoversAddr(list *externalAllowlist, addr netip.Addr) bool {
	if !list.known {
		return false
	}
	for _, p := range list.prefixes {
		if p.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}

// allowlistCovers is allowlistCoversAddr for a host that is already a literal address; a name (which
// has to be resolved, and can move) is not answerable here.
func allowlistCovers(list *externalAllowlist, host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return allowlistCoversAddr(list, addr)
}

// refuseUnreachableTarget writes the 422 naming both halves of the decision, and reports whether it
// wrote anything.
func (s *Server) refuseUnreachableTarget(w http.ResponseWriter, r *http.Request, address string) bool {
	list, outside := s.targetOutsideAllowlist(r.Context(), address)
	if !outside {
		return false
	}
	writeProblem(w, http.StatusUnprocessableEntity, "target unreachable",
		"target: "+strconv.Quote(address)+" is outside the addresses this fleet's agents may probe ("+
			strings.Join(list.raw, ", ")+"), so every check against it would time out. "+
			"Add it to config.checkers.external.allowedCidrs AND to networkPolicy.externalEgress -- "+
			"they are two halves of one decision: the first lets the agent try, the second lets the packet leave.")
	return true
}
