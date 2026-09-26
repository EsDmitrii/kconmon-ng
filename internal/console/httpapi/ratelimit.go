package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// rateLimitWindow is the fixed window both limits count in; both config keys are spelled
// "...PerMinute".
const rateLimitWindow = time.Minute

// The CLOSED value set for the {limit} label on the rate-limit metrics. A
// username, subject ID or source IP must NEVER become a label value -- all
// three are unbounded and attacker-chosen.
const (
	rateLimitRuns   = "runs"
	rateLimitLogin  = "login"
	rateLimitPromQL = "promql"
)

// rateLimitRetryAfterSeconds is what a 429 advertises; it is the WHOLE window, not the time
// actually left.
const rateLimitRetryAfterSeconds = int(rateLimitWindow / time.Second)

// runsRateLimitKey is the per-subject counter for POST /api/v1/runs. Kind is
// part of the key so a user and a token that happen to share an ID cannot
// collide.
func runsRateLimitKey(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	return "rl:runs:" + subjectRateLimitID(subject)
}

// promqlRateLimitKey is the per-subject counter for the PromQL proxy; same shape and same reasoning
// as the runs key.
func promqlRateLimitKey(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	return "rl:promql:" + subjectRateLimitID(subject)
}

// subjectRateLimitID names whose budget a request spends. Every anonymous visitor is the same
// subject, so for them the client address stands in for the identity; otherwise one open console
// tab could spend the budget of everybody else.
func subjectRateLimitID(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	if subject.Kind == authz.SubjectAnonymous && subject.ClientAddr != "" {
		return string(subject.Kind) + ":" + subject.ID + ":" + subject.ClientAddr
	}
	return string(subject.Kind) + ":" + subject.ID
}

// maxLoginUsernameBytes bounds the username a login may present, and so the per-username key it
// stores; created usernames are at most 64 characters (usernamePattern).
const maxLoginUsernameBytes = 256

// loginUserRateLimitKey is the per-username login counter; the username is caller-supplied and
// unvalidated at this point (no user lookup has happened yet -- that is the point).
func loginUserRateLimitKey(username string) string { return "rl:login:u:" + username }

// loginIPRateLimitKey is the per-source-IP login counter.
func loginIPRateLimitKey(clientIP string) string {
	return "rl:login:ip:" + clientIP
}

// oidcStartIPRateLimitKey is the per-source-IP counter for GET /api/v1/auth/oidc/start.
func oidcStartIPRateLimitKey(clientIP string) string {
	return "rl:oidcstart:ip:" + clientIP
}

// oidcCallbackIPRateLimitKey is the per-source-IP counter for GET /api/v1/auth/oidc/callback.
func oidcCallbackIPRateLimitKey(clientIP string) string {
	return "rl:oidccallback:ip:" + clientIP
}

// forwardedPeerRateLimitKey is the counter a trusted proxy's own budget for limit is spent on.
func forwardedPeerRateLimitKey(limit, proxyAddr string) string {
	return "rl:" + limit + ":peer:" + proxyAddr
}

// forwardedPeerBurstFactor is how much larger a trusted proxy's own budget is than the per-subject
// budget of one anonymous or credential-less client behind it. Those budgets are keyed on whatever
// X-Forwarded-For a trusted peer writes, so a pod inside the trusted range could name a fresh address
// per request; each request a client's own budget admits is counted against the peer as well, which
// bounds that pod, while the real ingress, which carries every user, keeps plenty of room. Sign-in
// spends no peer budget: see handleAuthLogin.
const forwardedPeerBurstFactor = 20

// loginIPBurstFactor is how much larger the per-address sign-in budget is than the per-username one.
// One address can be a whole ingress or NAT, so the address counter is a wide net for one host
// spraying usernames; the per-username counter is the narrow one that protects an account.
const loginIPBurstFactor = 20

// remoteAddrHost is the host part of r.RemoteAddr.
func remoteAddrHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// rateLimitAddr is the form a client address takes in a per-address budget key: an IPv4 address as
// it is, an IPv6 address as its /64. One host routinely holds a whole /64, and keyed per /128 it could
// spend a fresh budget from every address in it.
func rateLimitAddr(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// requestAddrs is r's client address in budget form (rateLimitAddr) and, when a trusted proxy named
// that client in X-Forwarded-For, the proxy's own address in the same form; proxyAddr is "" when the
// client connected directly.
func requestAddrs(r *http.Request, trusted []*net.IPNet) (clientAddr, proxyAddr string) {
	peer := remoteAddrHost(r.RemoteAddr)
	client := clientIP(r, trusted)
	if client == peer {
		return rateLimitAddr(client), ""
	}
	return rateLimitAddr(client), rateLimitAddr(peer)
}

// clientIP is the address a rate limiter should count against.
//
// r.RemoteAddr is the truth unless the request came from a proxy the operator has NAMED as trusted
// (clientAddress.trustedProxyCIDRs, or auth.header.trustedProxyCIDRs while that is empty; the only
// place this codebase accepts a forwarding header at all). From a trusted peer, the rightmost
// X-Forwarded-For hop that is NOT itself trusted is the client: rightmost because a client can
// prepend anything it likes to that header, and only the hops appended by trusted proxies can be
// believed.
//
// With no trusted CIDRs configured — the default — this is exactly r.RemoteAddr, and no header is
// consulted at all.
//
// Every X-Forwarded-For line counts, as one list: a proxy may append its own line instead of
// extending the client's, and the first line alone would be the client's choice. A hop that is not
// an address stops the walk, since nothing to its left was written by a trusted proxy.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	addr := remoteAddrHost(r.RemoteAddr)
	if len(trusted) == 0 || !ipInAny(addr, trusted) {
		return addr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for _, raw := range slices.Backward(hops) {
		hop := strings.TrimSpace(raw)
		if hop == "" {
			continue
		}
		ip := parseForwardedHop(hop)
		if ip == nil {
			break
		}
		if !ipInAny(ip.String(), trusted) {
			return ip.String()
		}
	}
	// Every hop is a trusted proxy (or the header is absent or unusable): the nearest one is all
	// there is.
	return addr
}

// parseForwardedHop reads one X-Forwarded-For hop, which some proxies write with a port.
func parseForwardedHop(hop string) net.IP {
	if ip := net.ParseIP(hop); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(hop); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

// ipInAny reports whether addr parses and falls inside one of the networks.
func ipInAny(addr string, networks []*net.IPNet) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	for _, n := range networks {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseCIDRs turns the configured trusted-proxy list into networks, dropping anything unparseable
// (config.Validate already refuses those at boot for any config that went through Load).
func parseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, raw := range cidrs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		out = append(out, network)
	}
	return out
}

// rateLimitAllow increments every key within the current fixed window and reports whether the
// request may proceed; EVERY key is incremented even when an earlier one has already blown its
// limit. A request that carries a forwardingProxy and that its own keys admit also spends that
// proxy's budget, once per limit.
func (s *Server) rateLimitAllow(ctx context.Context, limit string, perMinute int, keys ...string) bool {
	proxy, _ := ctx.Value(forwardingProxyKey{}).(*forwardingProxy)
	return s.rateLimitSpend(ctx, limit, perMinute, proxy, keys...)
}

// rateLimitSpend is rateLimitAllow with the proxy to charge, if any, named by the caller. A request
// its own keys refuse leaves the proxy's budget alone: charged anyway, one client that kept going
// after its 429 spent the budget of everyone behind the same proxy.
func (s *Server) rateLimitSpend(ctx context.Context, limit string, perMinute int, proxy *forwardingProxy, keys ...string) bool {
	if perMinute <= 0 || s.kv == nil {
		return true
	}

	allowed := true
	for _, key := range keys {
		if !s.spendRateLimit(ctx, limit, perMinute, key) {
			allowed = false
		}
	}
	if allowed && proxy != nil && !slices.Contains(proxy.charged, limit) {
		proxy.charged = append(proxy.charged, limit)
		allowed = s.spendRateLimit(ctx, limit, perMinute*forwardedPeerBurstFactor, forwardedPeerRateLimitKey(limit, proxy.addr))
	}

	if !allowed {
		s.metrics.RateLimited.WithLabelValues(limit).Inc()
	}
	return allowed
}

// forwardingProxyKey carries a *forwardingProxy on the context of a request whose per-subject budgets
// are keyed on a client address a trusted proxy named: a caller with no credentials, or an anonymous
// visitor.
type forwardingProxyKey struct{}

// forwardingProxy is that proxy's address and the limits whose proxy budget this request has already
// spent: rateLimitAllow spends it once per limit, however many keys the handler counts. One request's
// handlers run on one goroutine, so it needs no lock.
type forwardingProxy struct {
	addr    string
	charged []string
}

func contextWithForwardingProxy(ctx context.Context, proxyAddr string) context.Context {
	return context.WithValue(ctx, forwardingProxyKey{}, &forwardingProxy{addr: proxyAddr})
}

// spendRateLimit counts one request against key and reports whether it is still within max; a KV
// error fails open.
func (s *Server) spendRateLimit(ctx context.Context, limit string, maxPerWindow int, key string) bool {
	n, err := s.kv.IncrWithTTL(ctx, key, rateLimitWindow)
	if err != nil {
		// A Valkey outage lasts minutes and every request in it would otherwise log.
		s.metrics.RateLimitFailOpen.WithLabelValues(limit).Inc()
		s.rateLimitWarnOnce.Do(func() {
			slog.Warn("httpapi: rate limit backend unavailable, failing open while it is; logged once per process, "+
				"the *_console_rate_limit_failopen_total metric counts every request", "limit", limit, "error", err)
		})
		return true
	}
	return n <= int64(maxPerWindow)
}

// writeRateLimited answers a refused request.
func writeRateLimited(w http.ResponseWriter, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(rateLimitRetryAfterSeconds))
	writeProblem(w, http.StatusTooManyRequests, "too many requests", detail)
}
