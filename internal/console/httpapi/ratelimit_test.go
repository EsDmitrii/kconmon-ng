package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// failingKV is a cache.KV whose IncrWithTTL always fails -- the Valkey-outage
// fake the fail-open tests need. Everything else delegates to a real
// InProcessKV so a SessionStore built on it still works.
type failingKV struct {
	cache.KV
	mu    sync.Mutex
	calls int
}

func newFailingKV() *failingKV { return &failingKV{KV: cache.NewInProcessKV()} }

func (f *failingKV) IncrWithTTL(_ context.Context, _ string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return 0, errors.New("valkey: connection refused")
}

func (f *failingKV) incrCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// countingUserStore records whether the login handler ever reached the user lookup.
type countingUserStore struct {
	fakeUserStore
	mu      sync.Mutex
	lookups int
}

func (c *countingUserStore) GetUserByUsername(ctx context.Context, username string) (store.User, error) {
	c.mu.Lock()
	c.lookups++
	c.mu.Unlock()
	return c.fakeUserStore.GetUserByUsername(ctx, username)
}

func (c *countingUserStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lookups
}

// rateLimitTestServer pairs a Server built with explicit rate limits and a
// given KV with the *metrics.Metrics registered for it, so a test can read
// the new counters back without reaching into the Server.
type rateLimitTestServer struct {
	srv *Server
	m   *metrics.Metrics
}

func newRateLimitServer(t *testing.T, mode string, rl config.RateLimitConfig, kv cache.KV, extra Deps) rateLimitTestServer { //nolint:gocritic // hugeParam: test helper, not the hot path
	t.Helper()
	cfg := authTestConfig(mode)
	cfg.RateLimit = rl
	reg := prometheus.NewRegistry()
	m := metrics.New(cfg.MetricsPrefix, reg)
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })

	extra.Config = cfg
	extra.Metrics = m
	extra.PromRegistry = reg
	extra.UI = ui
	extra.KV = kv
	return rateLimitTestServer{srv: NewServer(extra), m: m}
}

// newRunsRateLimitServer is newRateLimitServer wired for POST /api/v1/runs:
// a fixed operator subject and a fake runner.
func newRunsRateLimitServer(t *testing.T, rl config.RateLimitConfig, kv cache.KV) (rateLimitTestServer, *fakeRunner) { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	runner := newFakeRunner()
	ts := newRateLimitServer(t, "local", rl, kv, Deps{
		Runner:        runner,
		Authenticator: fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1"}},
		Policy:        authz.NewPolicy(nil),
		Roles:         fakeRoleResolver{roles: []string{"operator"}},
	})
	return ts, runner
}

// newLoginRateLimitServer is newRateLimitServer wired for POST
// /api/v1/auth/login in auth.mode=local, with one real (argon2id-hashed)
// user "alice".
func newLoginRateLimitServer(t *testing.T, rl config.RateLimitConfig, kv cache.KV) (rateLimitTestServer, *countingUserStore) { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	hash, err := authn.HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	users := &countingUserStore{fakeUserStore: fakeUserStore{users: map[string]store.User{
		"alice": {ID: "u-1", Username: "alice", PasswordHash: hash, DisplayName: "Alice"},
	}}}
	ts := newRateLimitServer(t, "local", rl, kv, Deps{
		Users:         users,
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), time.Hour),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
	return ts, users
}

// postLoginFrom posts to /api/v1/auth/login from a caller-chosen source address.
func postLoginFrom(t *testing.T, s *Server, remoteAddr, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// --- the primitive: keys, fail-open, and the closed metric labels ---------

func TestRateLimitAllowFailsOpenOnKVError(t *testing.T) {
	kv := newFailingKV()
	ts := newRateLimitServer(t, "anonymous", config.RateLimitConfig{RunsPerMinute: 1}, kv, Deps{})

	logs := captureLogs(t)

	// Ten calls against a limit of 1: every one of them must be allowed,
	// because the backend never answered.
	for i := range 10 {
		if !ts.srv.rateLimitAllow(t.Context(), rateLimitRuns, 1, "rl:runs:user:u1") {
			t.Fatalf("call %d was refused; a KV outage must fail OPEN", i)
		}
	}

	if got := kv.incrCalls(); got != 10 {
		t.Errorf("IncrWithTTL calls = %d, want 10", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimitFailOpen.WithLabelValues("runs")); got != 10 {
		t.Errorf("RateLimitFailOpen(runs) = %v, want 10", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("runs")); got != 0 {
		t.Errorf("RateLimited(runs) = %v, want 0 -- a fail-open is not a refusal", got)
	}
	if n := strings.Count(logs.String(), "rate limit backend unavailable"); n != 1 {
		t.Errorf("fail-open was logged %d times, want exactly 1 (ten outage hits must not flood the log)", n)
	}
}

func TestRateLimitAllowDisabledWhenLimitIsZero(t *testing.T) {
	kv := newFailingKV()
	ts := newRateLimitServer(t, "anonymous", config.RateLimitConfig{}, kv, Deps{})

	for i := range 5 {
		if !ts.srv.rateLimitAllow(t.Context(), rateLimitRuns, 0, "rl:runs:user:u1") {
			t.Fatalf("call %d was refused with the limit disabled", i)
		}
	}
	if got := kv.incrCalls(); got != 0 {
		t.Errorf("IncrWithTTL calls = %d, want 0 -- a disabled limit must not touch the KV at all", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimitFailOpen.WithLabelValues("runs")); got != 0 {
		t.Errorf("RateLimitFailOpen(runs) = %v, want 0 -- a disabled limit cannot fail open", got)
	}
}

// TestRateLimitAllowCountsEveryKeyIndependently is the login shape at the primitive level: two
// keys, one over its limit.
func TestRateLimitAllowCountsEveryKeyIndependently(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "anonymous", config.RateLimitConfig{}, kv, Deps{})

	// Burn the username key's allowance of 2 on its own.
	for range 2 {
		if !ts.srv.rateLimitAllow(t.Context(), rateLimitLogin, 2, "rl:login:u:alice") {
			t.Fatal("under-limit call was refused")
		}
	}
	if ts.srv.rateLimitAllow(t.Context(), rateLimitLogin, 2, "rl:login:u:alice") {
		t.Fatal("third call on a limit of 2 was allowed")
	}

	// A different key is untouched by that.
	if !ts.srv.rateLimitAllow(t.Context(), rateLimitLogin, 2, "rl:login:ip:203.0.113.7") {
		t.Fatal("an unrelated key was refused because another key was over its limit")
	}

	// And when both are passed together, the over-limit one refuses the pair
	// while the healthy one still counts up.
	if ts.srv.rateLimitAllow(t.Context(), rateLimitLogin, 2, "rl:login:u:alice", "rl:login:ip:203.0.113.7") {
		t.Fatal("a pair containing an over-limit key was allowed")
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("login")); got != 2 {
		t.Errorf("RateLimited(login) = %v, want 2 (one per refused call, not per refused key)", got)
	}
}

func TestRemoteAddrHostStripsPortAndFallsBack(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"203.0.113.7:54321", "203.0.113.7"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"203.0.113.7", "203.0.113.7"}, // no port at all: use it whole
		{"", ""},
	} {
		if got := remoteAddrHost(tc.in); got != tc.want {
			t.Errorf("remoteAddrHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRateLimitKeysNamespaceEachLimit(t *testing.T) {
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "u1"}
	if got, want := runsRateLimitKey(subject), "rl:runs:user:u1"; got != want {
		t.Errorf("runsRateLimitKey = %q, want %q", got, want)
	}
	if got, want := loginUserRateLimitKey("alice"), "rl:login:u:alice"; got != want {
		t.Errorf("loginUserRateLimitKey = %q, want %q", got, want)
	}
	if got, want := loginIPRateLimitKey("203.0.113.7:1234"), "rl:login:ip:203.0.113.7"; got != want {
		t.Errorf("loginIPRateLimitKey = %q, want %q", got, want)
	}
}

// --- POST /api/v1/runs ----------------------------------------------------

const runsRateLimitBody = `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":30000000000}`

func TestRunsCreateUnderLimitPasses(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, runner := newRunsRateLimitServer(t, config.RateLimitConfig{RunsPerMinute: 3}, kv)

	for i := range 3 {
		w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF)
		if w.Code != http.StatusAccepted {
			t.Fatalf("run %d = %d, want 202: %s", i, w.Code, w.Body)
		}
	}
	if got := len(runner.started); got != 3 {
		t.Errorf("runner.Start calls = %d, want 3", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("runs")); got != 0 {
		t.Errorf("RateLimited(runs) = %v, want 0", got)
	}
}

func TestRunsCreateOverLimitReturns429WithRetryAfter(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, runner := newRunsRateLimitServer(t, config.RateLimitConfig{RunsPerMinute: 2}, kv)

	for i := range 2 {
		if w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF); w.Code != http.StatusAccepted {
			t.Fatalf("run %d = %d, want 202: %s", i, w.Code, w.Body)
		}
	}

	w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third run = %d, want 429: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	retryAfter := w.Header().Get("Retry-After")
	secs, err := strconv.Atoi(retryAfter)
	if err != nil || secs <= 0 || secs > 60 {
		t.Errorf("Retry-After = %q, want a positive number of seconds no larger than the window", retryAfter)
	}

	if got := len(runner.started); got != 2 {
		t.Errorf("runner.Start calls = %d, want 2 -- the limiter must refuse BEFORE Start", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("runs")); got != 1 {
		t.Errorf("RateLimited(runs) = %v, want 1", got)
	}
}

// TestRunsCreateLimitIsPerSubject pins the key: a second subject gets its own
// fresh allowance against the same server and the same KV.
func TestRunsCreateLimitIsPerSubject(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, _ := newRunsRateLimitServer(t, config.RateLimitConfig{RunsPerMinute: 1}, kv)

	if w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF); w.Code != http.StatusAccepted {
		t.Fatalf("first run = %d, want 202: %s", w.Code, w.Body)
	}
	if w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second run for the same subject = %d, want 429: %s", w.Code, w.Body)
	}

	// u2's own counter is untouched: same key prefix, different subject id.
	if n, err := kv.IncrWithTTL(t.Context(), runsRateLimitKey(authz.Subject{Kind: authz.SubjectUser, ID: "u2"}), time.Minute); err != nil || n != 1 {
		t.Errorf("a second subject's counter = %d (err=%v), want a fresh 1", n, err)
	}
}

func TestRunsCreateFailsOpenWhenKVIsDown(t *testing.T) {
	kv := newFailingKV()
	ts, runner := newRunsRateLimitServer(t, config.RateLimitConfig{RunsPerMinute: 1}, kv)

	for i := range 3 {
		w := doRequest(t, ts.srv, http.MethodPost, "/api/v1/runs", strings.NewReader(runsRateLimitBody), mutateWithCSRF)
		if w.Code != http.StatusAccepted {
			t.Fatalf("run %d = %d, want 202 (KV outage must fail open): %s", i, w.Code, w.Body)
		}
	}
	if got := len(runner.started); got != 3 {
		t.Errorf("runner.Start calls = %d, want 3", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimitFailOpen.WithLabelValues("runs")); got != 3 {
		t.Errorf("RateLimitFailOpen(runs) = %v, want 3", got)
	}
}

// --- POST /api/v1/auth/login ---------------------------------------------

const loginRateLimitBody = `{"username":"alice","password":"wrong"}`

func TestAuthLoginOverUsernameLimitReturns429BeforeArgon2(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, users := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 2}, kv)

	for i := range 2 {
		w := postLoginFrom(t, ts.srv, "203.0.113.7:1111", loginRateLimitBody)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("login %d = %d, want 401: %s", i, w.Code, w.Body)
		}
	}

	w := postLoginFrom(t, ts.srv, "203.0.113.7:1111", loginRateLimitBody)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third login = %d, want 429: %s", w.Code, w.Body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 has no Retry-After header")
	}
	if got := users.count(); got != 2 {
		t.Errorf("user lookups = %d, want 2 -- the limiter must refuse BEFORE the argon2id verify, "+
			"which is the whole point (64MiB per verification vs a 256Mi pod)", got)
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("login")); got != 1 {
		t.Errorf("RateLimited(login) = %v, want 1", got)
	}
}

// TestAuthLoginUsernameAndIPCountedIndependently is the explicit requirement.
func TestAuthLoginUsernameAndIPCountedIndependently(t *testing.T) {
	t.Run("hot username does not lock out another user from the same IP", func(t *testing.T) {
		kv := cache.NewInProcessKV()
		t.Cleanup(kv.Close)
		// loginPerMinute 3, but we only ever spend 2 of the IP's allowance on
		// alice, so bob's own request is still under BOTH counters.
		ts, _ := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 2}, kv)

		for range 2 {
			postLoginFrom(t, ts.srv, "198.51.100.5:2222", `{"username":"alice","password":"x"}`)
		}
		// alice is now over her username limit from this IP.
		if w := postLoginFrom(t, ts.srv, "198.51.100.5:2222", `{"username":"alice","password":"x"}`); w.Code != http.StatusTooManyRequests {
			t.Fatalf("alice's third attempt = %d, want 429", w.Code)
		}
		// A DIFFERENT username from a DIFFERENT ip is untouched by alice's
		// counter.
		if w := postLoginFrom(t, ts.srv, "198.51.100.9:2222", `{"username":"bob","password":"x"}`); w.Code == http.StatusTooManyRequests {
			t.Fatal("an unrelated username from an unrelated IP was rate-limited by alice's counter")
		}
	})

	t.Run("hot IP locks out that IP whatever username it sprays", func(t *testing.T) {
		kv := cache.NewInProcessKV()
		t.Cleanup(kv.Close)
		ts, _ := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 3}, kv)

		// Three DIFFERENT usernames from one source: every username counter
		// sits at 1, but the IP counter is at 3.
		for i := range 3 {
			body := fmt.Sprintf(`{"username":"sprayed-%d","password":"x"}`, i)
			if w := postLoginFrom(t, ts.srv, "198.51.100.13:3333", body); w.Code == http.StatusTooManyRequests {
				t.Fatalf("spray %d was refused too early", i)
			}
		}
		if w := postLoginFrom(t, ts.srv, "198.51.100.13:3333", `{"username":"sprayed-9","password":"x"}`); w.Code != http.StatusTooManyRequests {
			t.Fatalf("fourth username from the same IP = %d, want 429 -- the per-IP counter is what "+
				"catches a username spray", w.Code)
		}
		// The same fresh username from a different IP still gets through.
		if w := postLoginFrom(t, ts.srv, "198.51.100.77:3333", `{"username":"sprayed-9","password":"x"}`); w.Code == http.StatusTooManyRequests {
			t.Fatal("an unrelated source IP was locked out by another IP's counter")
		}
	})
}

func TestAuthLoginFailsOpenWhenKVIsDown(t *testing.T) {
	kv := newFailingKV()
	ts, users := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 1}, kv)

	for i := range 3 {
		w := postLoginFrom(t, ts.srv, "203.0.113.7:1111", loginRateLimitBody)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("login %d = %d, want 401 (a Valkey outage must not become a login outage): %s", i, w.Code, w.Body)
		}
	}
	if got := users.count(); got != 3 {
		t.Errorf("user lookups = %d, want 3 -- every request must have reached the real login path", got)
	}
	// Two keys per login (username + ip), three logins.
	if got := testutil.ToFloat64(ts.m.RateLimitFailOpen.WithLabelValues("login")); got != 6 {
		t.Errorf("RateLimitFailOpen(login) = %v, want 6 (two keys x three requests)", got)
	}
}

// TestAuthLoginSucceedsUnderTheLimit is the guard against the limiter
// breaking the happy path: a correct password inside the allowance still
// mints a session.
func TestAuthLoginSucceedsUnderTheLimit(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, _ := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 5}, kv)

	w := postLoginFrom(t, ts.srv, "203.0.113.7:1111", `{"username":"alice","password":"s3cret!"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login = %d, want 204: %s", w.Code, w.Body)
	}
}

// TestAuthLoginLimitDisabledByZero pins the documented escape hatch: 0 turns
// THAT limit off entirely.
func TestAuthLoginLimitDisabledByZero(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts, _ := newLoginRateLimitServer(t, config.RateLimitConfig{LoginPerMinute: 0}, kv)

	for i := range 12 {
		if w := postLoginFrom(t, ts.srv, "203.0.113.7:1111", loginRateLimitBody); w.Code != http.StatusUnauthorized {
			t.Fatalf("login %d = %d, want 401 with the limit disabled", i, w.Code)
		}
	}
	if got := testutil.ToFloat64(ts.m.RateLimited.WithLabelValues("login")); got != 0 {
		t.Errorf("RateLimited(login) = %v, want 0", got)
	}
}
