package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// A client without credentials writes audit rows at a bounded rate per address: past its budget the
// rows are counted as dropped, and another address still gets its own.
func TestCredentialLessFailuresAreAuditedAtABoundedRatePerAddress(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	fs := &fakeAuditStore{}
	s := newRateLimitServer(t, "local", config.RateLimitConfig{}, kv, Deps{
		Audit:         fs,
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
		Policy:        authz.NewPolicy(nil),
	}).srv
	probe := func(addr string) {
		t.Helper()
		w := doRequest(t, s, http.MethodGet, "/api/v1/topology", nil, func(r *http.Request) { r.RemoteAddr = addr })
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("credential-less GET from %s = %d, want 401", addr, w.Code)
		}
	}

	for i := range auditCredentialLessPerMinute {
		probe("203.0.113.9:40000")
		waitForAuditEntries(t, fs, i+1)
	}
	for range 10 {
		probe("203.0.113.9:40000")
	}
	probe("198.51.100.7:40000")

	rows := waitForAuditEntries(t, fs, auditCredentialLessPerMinute+1)
	if len(rows) != auditCredentialLessPerMinute+1 || rows[len(rows)-1].RemoteAddr != "198.51.100.7:40000" {
		t.Errorf("stored %d rows, last from %q; want %d, the last from the second address",
			len(rows), rows[len(rows)-1].RemoteAddr, auditCredentialLessPerMinute+1)
	}
	if got := s.auditDropped.Load(); got != 10 {
		t.Errorf("dropped %d rows past the address's budget, want 10", got)
	}
}

// The OIDC callback answers a flood from one address like the start does, so refused callbacks
// cannot write audit rows at will.
func TestOIDCCallbackIsRateLimitedPerAddress(t *testing.T) {
	s := newOIDCStartServer(t)
	callback := func(addr string) int {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, config.OIDCCallbackPath+"?state=s&code=c", http.NoBody)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for i := range loginIPBurstFactor {
		if code := callback("203.0.113.9:40000"); code != http.StatusUnauthorized {
			t.Fatalf("callback %d without a state cookie = %d, want 401", i, code)
		}
	}
	if code := callback("203.0.113.9:40000"); code != http.StatusTooManyRequests {
		t.Errorf("callback past the address's budget = %d, want 429", code)
	}
	if code := callback("198.51.100.7:40000"); code != http.StatusUnauthorized {
		t.Errorf("callback from another address = %d, want 401", code)
	}
}
