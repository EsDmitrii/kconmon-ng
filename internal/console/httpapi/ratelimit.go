package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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
	rateLimitRuns  = "runs"
	rateLimitLogin = "login"
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

// loginUserRateLimitKey is the per-username login counter; the username is caller-supplied and
// unvalidated at this point (no user lookup has happened yet -- that is the point).
func loginUserRateLimitKey(username string) string { return "rl:login:u:" + username }

// loginIPRateLimitKey is the per-source-IP login counter.
func loginIPRateLimitKey(remoteAddr string) string {
	return "rl:login:ip:" + remoteAddrHost(remoteAddr)
}

// remoteAddrHost is the host part of r.RemoteAddr; trusting a forwarding header is a trust decision
// this codebase has made exactly once.
func remoteAddrHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
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
