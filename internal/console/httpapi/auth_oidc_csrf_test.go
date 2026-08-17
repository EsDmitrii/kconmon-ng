package httpapi

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

/*
Login CSRF. The callback used to accept any state the console had minted, from any user agent: an
attacker runs the flow as far as holding a valid state+code pair, then makes the victim's browser
follow the callback URL, and the victim is silently signed in AS THE ATTACKER. Everything they
record afterwards lands in the attacker's account.

The state is now bound to the browser that started the flow (RFC 6749 §10.12).
*/

func oidcCSRFServer(t *testing.T, flow fakeOIDCFlow) *Server {
	t.Helper()
	cfg := authTestConfig("oidc")
	reg := prometheus.NewRegistry()
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) })
	return NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg, UI: ui, OIDC: flow,
	})
}

func TestOIDCCallbackRefusesAStateThisBrowserDidNotStart(t *testing.T) {
	s := oidcCSRFServer(t, fakeOIDCFlow{sessionID: "sess-1", returnTo: "/dashboard"})

	// The attacker's state, delivered to a browser that never started a flow.
	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=attacker&code=c", nil, nil)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("callback with no binding cookie = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "kconmon_session" && c.Value != "" {
			t.Fatal("a session cookie was issued for a flow this browser never started")
		}
	}
}

func TestOIDCCallbackRefusesAStateFromAnotherBrowsersFlow(t *testing.T) {
	s := oidcCSRFServer(t, fakeOIDCFlow{sessionID: "sess-1", returnTo: "/dashboard"})

	// This browser started its own flow; the URL carries somebody else's state.
	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=attacker&code=c", nil, withOIDCState("mine"))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("callback with a mismatched state = %d, want 401; body=%s", w.Code, w.Body.String())
	}
}

// The binding cookie is consumed whatever happens, so a refusal cannot be retried against it.
func TestOIDCCallbackClearsTheBindingCookieOnRefusal(t *testing.T) {
	s := oidcCSRFServer(t, fakeOIDCFlow{sessionID: "sess-1", returnTo: "/dashboard"})

	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=attacker&code=c", nil, withOIDCState("mine"))

	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the binding cookie survived a refused callback")
	}
}

// The other half: the flow the console itself starts must still complete.
func TestOIDCStartSetsTheBindingCookieItsCallbackWants(t *testing.T) {
	authURL := "https://idp.test/authorize?client_id=x&state=abc123&code_challenge=y"
	s := oidcCSRFServer(t, fakeOIDCFlow{authorizeURL: authURL, sessionID: "sess-1", returnTo: "/dashboard"})

	w := doRequest(t, s, http.MethodGet, "/api/v1/auth/oidc/start?returnTo=/matrix", nil, nil)
	if w.Code != http.StatusFound {
		t.Fatalf("oidc start = %d, want 302; body=%s", w.Code, w.Body.String())
	}

	want := ""
	if u, err := url.Parse(authURL); err == nil {
		want = u.Query().Get("state")
	}
	var got string
	for _, c := range w.Result().Cookies() {
		if c.Name == oidcStateCookieName {
			got = c.Value
			if !c.HttpOnly {
				t.Error("the binding cookie is readable from script")
			}
		}
	}
	if got != want || got == "" {
		t.Fatalf("binding cookie = %q, want the state the authorize URL carries (%q)", got, want)
	}

	// And that cookie satisfies the callback.
	cb := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state="+want+"&code=c", nil, withOIDCState(want))
	if cb.Code != http.StatusFound {
		t.Fatalf("callback for the flow this browser started = %d, want 302; body=%s", cb.Code, cb.Body.String())
	}
}
