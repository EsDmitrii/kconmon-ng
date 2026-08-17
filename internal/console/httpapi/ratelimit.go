package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// This is a small helper the two call sites reach for directly, NOT a piece of middleware;
// middleware is the obvious shape and the wrong one here.

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
	return "rl:runs:" + string(subject.Kind) + ":" + subject.ID
}

// promqlRateLimitKey is the per-subject counter for the PromQL proxy; same shape and same reasoning
// as the runs key. Anonymous subjects share one bucket, which is the honest reading of "everybody
// is the same subject".
func promqlRateLimitKey(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	return "rl:promql:" + string(subject.Kind) + ":" + subject.ID
}

// loginUserRateLimitKey is the per-username login counter; the username is caller-supplied and
// unvalidated at this point (no user lookup has happened yet -- that is the point).
func loginUserRateLimitKey(username string) string { return "rl:login:u:" + username }

// loginIPRateLimitKey is the per-source-IP login counter.
func loginIPRateLimitKey(clientIP string) string {
	return "rl:login:ip:" + clientIP
}

// loginIPBurstFactor is how much larger the per-ADDRESS login budget is than the per-username one.
//
// One address is not one person. Behind the chart's own Ingress every request in the world arrives
// from the ingress-controller pod, and a shared NAT does the same on a corporate network — so a
// per-address counter at the per-username limit meant six bogus attempts a minute locked out
// everybody, correct password included, with no credentials required to do it. The address counter
// still exists (it is what bounds a single host hammering many usernames) but it is a wide net, and
// the per-username counter stays the narrow one that actually protects an account.
const loginIPBurstFactor = 20

// remoteAddrHost is the host part of r.RemoteAddr.
func remoteAddrHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// clientIP is the address a rate limiter should count against.
//
// r.RemoteAddr is the truth unless the request came from a proxy the operator has NAMED as trusted
// (auth.header.trustedProxyCIDRs — the same list the header auth mode gates on, and the only place
// this codebase accepts a forwarding header at all). From a trusted peer, the rightmost
// X-Forwarded-For hop that is NOT itself trusted is the client: rightmost because a client can
// prepend anything it likes to that header, and only the hops appended by trusted proxies can be
// believed.
//
// With no trusted CIDRs configured — the default — this is exactly r.RemoteAddr, and no header is
// consulted at all.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	addr := remoteAddrHost(r.RemoteAddr)
	if len(trusted) == 0 || !ipInAny(addr, trusted) {
		return addr
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" {
			continue
		}
		if !ipInAny(hop, trusted) {
			return hop
		}
	}
	// Every hop is a trusted proxy (or the header is absent): the nearest one is all there is.
	return addr
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
// limit.
func (s *Server) rateLimitAllow(ctx context.Context, limit string, perMinute int, keys ...string) bool {
	if perMinute <= 0 || s.kv == nil {
		return true
	}

	allowed := true
	for _, key := range keys {
		n, err := s.kv.IncrWithTTL(ctx, key, rateLimitWindow)
		if err != nil {
			// A Valkey outage lasts minutes and every request in it would otherwise log.
			s.metrics.RateLimitFailOpen.WithLabelValues(limit).Inc()
			s.rateLimitWarnOnce.Do(func() {
				slog.Warn("httpapi: rate limit backend unavailable, failing open for the rest of this process",
					"limit", limit, "error", err)
			})
			continue
		}
		if n > int64(perMinute) {
			allowed = false
		}
	}

	if !allowed {
		s.metrics.RateLimited.WithLabelValues(limit).Inc()
	}
	return allowed
}

// writeRateLimited answers a refused request.
func writeRateLimited(w http.ResponseWriter, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(rateLimitRetryAfterSeconds))
	writeProblem(w, http.StatusTooManyRequests, "too many requests", detail)
}
