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

// This is a small helper the two call sites reach for directly, NOT a piece
// of middleware. Middleware is the obvious shape and the wrong one here:
// there are exactly two limited endpoints and they are limited on different
// things -- POST /api/v1/runs on the authenticated SUBJECT, POST
// /api/v1/auth/login on two unauthenticated dimensions (username and source
// IP) that only exist after the body has been decoded. A generic middleware
// would therefore need a per-route config table plus a per-route key
// extractor, i.e. all of the below plus an indirection, for two callers.
//
// FAIL-OPEN (plan line 521, the same Decision 8 posture as the projection
// guard): when the KV backend errors, the request is ALLOWED and the
// fail-open metric is incremented. A rate limit is a safeguard against abuse;
// a Valkey outage must not be able to turn it into a total login outage. The
// cost of the choice is that an attacker who can take Valkey down also takes
// the limit down -- accepted, because the alternative hands them a
// console-wide denial of service for the same effort.

// rateLimitWindow is the fixed window both limits count in. Both config keys
// are spelled "...PerMinute", so this is not independently tunable on
// purpose: a window and a count are one setting, and letting an operator
// change one without the other only produces a limit that no longer means
// what its name says.
const rateLimitWindow = time.Minute

// The CLOSED value set for the {limit} label on the rate-limit metrics. A
// username, subject ID or source IP must NEVER become a label value -- all
// three are unbounded and attacker-chosen.
const (
	rateLimitRuns  = "runs"
	rateLimitLogin = "login"
)

// rateLimitRetryAfterSeconds is what a 429 advertises. It is the WHOLE
// window, not the time actually left in it: cache.KV has no "read the
// remaining TTL" operation, and adding one purely to shave a few seconds off
// a hint header would buy a second round trip per refused request on a path
// that is refusing requests precisely because it is under load. Over-stating
// the wait is the safe direction -- a client that honors it comes back after
// the window has certainly rolled over.
const rateLimitRetryAfterSeconds = int(rateLimitWindow / time.Second)

// runsRateLimitKey is the per-subject counter for POST /api/v1/runs. Kind is
// part of the key so a user and a token that happen to share an ID cannot
// collide.
func runsRateLimitKey(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	return "rl:runs:" + string(subject.Kind) + ":" + subject.ID
}

// loginUserRateLimitKey is the per-username login counter. The username is
// caller-supplied and unvalidated at this point (no user lookup has happened
// yet -- that is the point), so it only ever appears inside a KV key, never
// in a metric label and never in a log line.
func loginUserRateLimitKey(username string) string { return "rl:login:u:" + username }

// loginIPRateLimitKey is the per-source-IP login counter.
func loginIPRateLimitKey(remoteAddr string) string {
	return "rl:login:ip:" + remoteAddrHost(remoteAddr)
}

// remoteAddrHost is the host part of r.RemoteAddr, falling back to the whole
// string when it does not parse as host:port.
//
// X-Forwarded-For is deliberately NOT consulted. Trusting a forwarding header
// is a trust decision this codebase has made exactly once, explicitly, and
// narrowly: authn/header.go decides trust on r.RemoteAddr ONLY, against an
// operator-configured CIDR allowlist, and audit.go records r.RemoteAddr raw.
// Honoring X-Forwarded-For here without that allowlist would let any client
// pick its own rate-limit bucket by setting a header, which is worse than no
// per-IP limit at all. If the console ever grows a general trusted-proxy
// notion beyond header auth, this is one of the places that should adopt it.
func remoteAddrHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// rateLimitAllow increments every key within the current fixed window and
// reports whether the request may proceed. perMinute <= 0 disables the check
// entirely (the documented "0 disables that limit" config contract) and does
// not touch the KV at all. limit is the metric label, not a threshold.
//
// EVERY key is incremented even when an earlier one has already blown its
// limit: the login path passes a username key and a source-IP key that are
// counted INDEPENDENTLY, and short-circuiting would let an attacker keep the
// IP counter permanently below its threshold by first tripping a throwaway
// username's counter on every request.
//
// The metrics are counted per CALL, not per key: one refused request is one
// increment of RateLimited no matter how many of its keys were over.
func (s *Server) rateLimitAllow(ctx context.Context, limit string, perMinute int, keys ...string) bool {
	if perMinute <= 0 || s.kv == nil {
		return true
	}

	allowed := true
	for _, key := range keys {
		n, err := s.kv.IncrWithTTL(ctx, key, rateLimitWindow)
		if err != nil {
			// Fail open, but visibly: the counter says a limit was not
			// enforced, and the log says why -- once. A Valkey outage lasts
			// minutes and every request in it would otherwise log, which is
			// how an outage becomes a second outage in the log pipeline.
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

// writeRateLimited answers a refused request: 429 with an RFC 7807 body (the
// same problem+json shape every other error in this package uses) and a
// Retry-After header. detail never names the counter that tripped -- telling
// a caller whether it was the username or the IP bucket would hand an
// attacker a probe for which usernames are being targeted.
func writeRateLimited(w http.ResponseWriter, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(rateLimitRetryAfterSeconds))
	writeProblem(w, http.StatusTooManyRequests, "too many requests", detail)
}
