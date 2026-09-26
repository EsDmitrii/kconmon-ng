package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

func TestRateLimitAddrKeysIPv6PerSlash64(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"198.51.100.7", "198.51.100.7"},
		{"::ffff:198.51.100.7", "198.51.100.7"},
		{"2001:db8::1", "2001:db8::/64"},
		{"2001:db8::ffff:1234", "2001:db8::/64"},
		{"2001:db8:0:1::1", "2001:db8:0:1::/64"},
		{"not-an-ip", "not-an-ip"},
	} {
		if got := rateLimitAddr(tc.in); got != tc.want {
			t.Errorf("rateLimitAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// oidcStartFrom runs GET /api/v1/auth/oidc/start from remoteAddr, with the given X-Forwarded-For line.
func oidcStartFrom(t *testing.T, s *Server, remoteAddr, xff string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/auth/oidc/start", http.NoBody)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

func newOIDCStartServer(t *testing.T, trusted ...string) *Server {
	t.Helper()
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "oidc", config.RateLimitConfig{LoginPerMinute: 1}, kv, Deps{
		OIDC: fakeOIDCFlow{authorizeURL: "https://idp.test/authorize?client_id=x&state=abc123"},
	})
	ts.srv.trustedProxies = parseCIDRs(trusted)
	return ts.srv
}

/*
A pod inside the trusted range can write any X-Forwarded-For it likes. It is not bound by a budget of
its own, which the real ingress, the same kind of peer, would share with every user behind it; a
start stores nothing, so the pod spends only the budgets of the addresses it names, and those bind.
*/
func TestTrustedPodRotatingForwardedForSpendsOnlyTheBudgetsItNames(t *testing.T) {
	s := newOIDCStartServer(t, "10.244.0.0/16")

	for i := range 2 * proxySignInBudget {
		if code := oidcStartFrom(t, s, "10.244.37.12:40000", fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)); code != http.StatusFound {
			t.Fatalf("start %d from the pod = %d, want 302", i, code)
		}
	}
	for i := range loginIPBurstFactor - 1 {
		if code := oidcStartFrom(t, s, "10.244.37.12:40000", "198.51.0.1"); code != http.StatusFound {
			t.Fatalf("start %d naming one address = %d, want 302 within its budget", i+2, code)
		}
	}
	if code := oidcStartFrom(t, s, "10.244.37.12:40000", "198.51.0.1"); code != http.StatusTooManyRequests {
		t.Fatalf("a named address past its budget = %d, want 429", code)
	}
	if code := oidcStartFrom(t, s, "10.244.1.7:40000", "203.0.113.5"); code != http.StatusFound {
		t.Fatalf("a real client behind the ingress = %d, want 302", code)
	}
}

// One host holds a whole IPv6 /64; keyed per /128 it could spend a fresh budget from every address.
func TestIPv6ClientsShareTheBudgetOfTheirSlash64(t *testing.T) {
	s := newOIDCStartServer(t)
	for i := range loginIPBurstFactor {
		if code := oidcStartFrom(t, s, fmt.Sprintf("[2001:db8::%x]:1111", i+1), ""); code != http.StatusFound {
			t.Fatalf("start %d = %d, want 302", i, code)
		}
	}
	if code := oidcStartFrom(t, s, "[2001:db8::ffff]:1111", ""); code != http.StatusTooManyRequests {
		t.Fatalf("another address in the same /64 = %d, want 429", code)
	}
	if code := oidcStartFrom(t, s, "[2001:db8:0:1::1]:1111", ""); code != http.StatusFound {
		t.Fatalf("an address in another /64 = %d, want 302", code)
	}
}

// Anonymous visitors are budgeted per address, so the same rotation reset the PromQL budget too.
func TestAnonymousBudgetBehindATrustedProxyIsBoundByTheProxy(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "anonymous", config.RateLimitConfig{PromQLPerMinute: 1}, kv, Deps{
		Prometheus: newFakePrometheus(t, promVector(1)),
	})
	ts.srv.trustedProxies = parseCIDRs([]string{"10.244.0.0/16"})
	query := func(remoteAddr, xff string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/promql/query",
			strings.NewReader(`{"query":"up"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = remoteAddr
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(w, req)
		return w.Code
	}

	for i := range forwardedPeerBurstFactor {
		if code := query("10.244.37.12:40000", fmt.Sprintf("198.51.100.%d", i+1)); code != http.StatusOK {
			t.Fatalf("query %d = %d, want 200 within the peer budget", i, code)
		}
	}
	if code := query("10.244.37.12:40000", "198.51.100.250"); code != http.StatusTooManyRequests {
		t.Fatalf("query from a fresh forwarded address past the peer budget = %d, want 429", code)
	}
	if code := query("10.244.1.7:40000", "203.0.113.5"); code != http.StatusOK {
		t.Fatalf("a visitor behind another proxy = %d, want 200", code)
	}
}

// clientAddress.trustedProxyCIDRs decides whose X-Forwarded-For names the client, and it replaces
// auth.header.trustedProxyCIDRs for that rather than adding to it. Needs NewServer to read the list
// through config.ForwardingProxyCIDRs.
func TestClientAddressProxiesComeFromTheirOwnKey(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	cfg := authTestConfig("oidc")
	cfg.RateLimit = config.RateLimitConfig{LoginPerMinute: 1}
	cfg.ClientAddress.TrustedProxyCIDRs = []string{"10.244.0.0/16"}
	cfg.Auth.Header.TrustedProxyCIDRs = []string{"10.0.0.5/32"}
	s := newServerWithConfig(t, cfg, kv, Deps{
		OIDC: fakeOIDCFlow{authorizeURL: "https://idp.test/authorize?client_id=x&state=abc123"},
	})

	for i := range loginIPBurstFactor {
		if code := oidcStartFrom(t, s, "10.244.1.7:40000", "203.0.113.66"); code != http.StatusFound {
			t.Fatalf("start %d = %d, want 302", i, code)
		}
	}
	if code := oidcStartFrom(t, s, "10.244.1.7:40000", "203.0.113.66"); code != http.StatusTooManyRequests {
		t.Fatalf("the forwarded client over its budget = %d, want 429", code)
	}
	if code := oidcStartFrom(t, s, "10.244.1.7:40000", "198.51.100.20"); code != http.StatusFound {
		t.Fatalf("another client behind the same proxy = %d, want 302: clientAddress.trustedProxyCIDRs was not read", code)
	}

	// The header-mode proxy is not in the client-address list, so its forwarding header is not read.
	for i := range loginIPBurstFactor {
		if code := oidcStartFrom(t, s, "10.0.0.5:40000", fmt.Sprintf("192.0.2.%d", i+1)); code != http.StatusFound {
			t.Fatalf("start %d from the header proxy = %d, want 302", i, code)
		}
	}
	if code := oidcStartFrom(t, s, "10.0.0.5:40000", "192.0.2.200"); code != http.StatusTooManyRequests {
		t.Fatalf("the header proxy with a fresh X-Forwarded-For = %d, want 429: its header was believed", code)
	}
}

// newServerWithConfig wires NewServer around a config the test built itself.
func newServerWithConfig(t *testing.T, cfg *config.Config, kv cache.KV, extra Deps) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	reg := prometheus.NewRegistry()
	extra.Config = cfg
	extra.Metrics = metrics.New(cfg.MetricsPrefix, reg)
	extra.PromRegistry = reg
	extra.UI = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	extra.KV = kv
	return NewServer(extra)
}
