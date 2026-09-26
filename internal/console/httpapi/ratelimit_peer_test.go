package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// loginFrom posts one login from remoteAddr for the given forwarded client and username.
func loginFrom(t *testing.T, s *Server, remoteAddr, xff, username string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"`+username+`","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remoteAddr
	req.Header.Set("X-Forwarded-For", xff)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

/*
Behind the ingress every login arrives from one pod. Its own budget was charged by the per-USERNAME
call, at the per-username limit times the peer factor: one client spending its own per-address
allowance used the whole ingress budget, and every other user was locked out of password login for
the rest of the minute. The peer budget is the per-address one times the peer factor, the same
ceiling GET /api/v1/auth/oidc/start spends on the same counter.
*/
func TestLoginThroughTheIngressHasThePeerBudgetOfAddressesNotOfUsernames(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "local", config.RateLimitConfig{LoginPerMinute: 1}, kv, Deps{
		Users:         erroringUserStore{},
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), 0, 0),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
	ts.srv.trustedProxies = parseCIDRs([]string{"10.244.0.0/16"})

	// One outside client spends its whole per-address allowance, a fresh username each time.
	for i := range loginIPBurstFactor {
		if code := loginFrom(t, ts.srv, "10.244.1.7:40000", "203.0.113.9", fmt.Sprintf("guess%d", i)); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d of the one client = 429, want it within its per-address budget", i)
		}
	}
	// Everybody else behind the same ingress still signs in.
	for i := range 3 * loginIPBurstFactor {
		client := fmt.Sprintf("198.51.%d.%d", i/250, i%250+1)
		if code := loginFrom(t, ts.srv, "10.244.1.7:40000", client, fmt.Sprintf("user%d", i)); code == http.StatusTooManyRequests {
			t.Fatalf("login %d of another user behind the ingress = 429: the ingress budget is too small", i)
		}
	}
}

// proxySignInBudget is what a trusted proxy's own budget would admit at LoginPerMinute 1, had sign-in
// still charged it.
const proxySignInBudget = loginIPBurstFactor * forwardedPeerBurstFactor

/*
A budget the whole ingress shares is a lever for anyone behind it: one address that kept going after
its own 429, or one-shot attempts from fresh /64s of a single /48, would refuse every colleague's
sign-in for the rest of the minute. Sign-in budgets are the client's and the account's own; what a
start or a login costs is bounded where it is spent (a start stores nothing, argon2 runs in a fixed
number of slots).
*/
func TestOneClientBehindTheIngressCannotLockEveryoneOutOfSignIn(t *testing.T) {
	for name, attacker := range map[string]func(i int) string{
		"one address past its own budget": func(int) string { return "203.0.113.9" },
		"a fresh /64 every time":          func(i int) string { return fmt.Sprintf("2001:db8:77:%x::1", i) },
	} {
		t.Run(name, func(t *testing.T) {
			kv := cache.NewInProcessKV()
			t.Cleanup(kv.Close)
			local := newRateLimitServer(t, "local", config.RateLimitConfig{LoginPerMinute: 1}, kv, Deps{
				Users:         erroringUserStore{},
				Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), 0, 0),
				Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
			}).srv
			local.trustedProxies = parseCIDRs([]string{"10.244.0.0/16"})
			oidc := newOIDCStartServer(t, "10.244.0.0/16")

			for i := range proxySignInBudget + 1 {
				loginFrom(t, local, "10.244.1.7:40000", attacker(i), fmt.Sprintf("guess%d", i))
				oidcStartFrom(t, oidc, "10.244.1.7:40000", attacker(i))
			}
			if code := loginFrom(t, local, "10.244.1.7:40000", "198.51.100.7", "alice"); code == http.StatusTooManyRequests {
				t.Errorf("alice's login from an untouched address = 429")
			}
			if code := oidcStartFrom(t, oidc, "10.244.1.7:40000", "198.51.100.7"); code != http.StatusFound {
				t.Errorf("a colleague's sign-in start = %d, want 302", code)
			}
		})
	}
}

/*
Behind a proxy that appends the address it saw, whatever a client writes into X-Forwarded-For itself
lands left of that address and is never read, so one client is one budget however it rotates the
header. The proxy's own requests name nobody and spend the proxy's address budget like any client's.
*/
func TestAClientBehindTheIngressIsOneClientWhateverItForwards(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "local", config.RateLimitConfig{LoginPerMinute: 1}, kv, Deps{
		Users:         erroringUserStore{},
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), 0, 0),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
	ts.srv.trustedProxies = parseCIDRs([]string{"10.244.0.0/16"})

	for name, xff := range map[string]func(i int) string{
		"a client rotating its own hops": func(i int) string { return fmt.Sprintf("198.51.100.%d, 10.244.9.%d, 203.0.113.9", i+1, i+1) },
		"the proxy itself":               func(int) string { return "" },
	} {
		for i := range loginIPBurstFactor {
			if code := loginFrom(t, ts.srv, "10.244.1.7:40000", xff(i), fmt.Sprintf("%s-%d", name, i)); code == http.StatusTooManyRequests {
				t.Fatalf("%s: attempt %d = 429, want it within one address budget", name, i)
			}
		}
		if code := loginFrom(t, ts.srv, "10.244.1.7:40000", xff(loginIPBurstFactor), name+"-over"); code != http.StatusTooManyRequests {
			t.Fatalf("%s: attempt past one address budget = %d, want 429", name, code)
		}
	}
}

// A pod inside the trusted range names whatever client it likes, and each named address has its own
// wide net; the account is protected by its per-username budget, which no address reaches.
func TestARotatingPodIsStillBoundByTheAccountsBudget(t *testing.T) {
	kv := cache.NewInProcessKV()
	t.Cleanup(kv.Close)
	ts := newRateLimitServer(t, "local", config.RateLimitConfig{LoginPerMinute: 1}, kv, Deps{
		Users:         erroringUserStore{},
		Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), 0, 0),
		Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: "local"},
	})
	ts.srv.trustedProxies = parseCIDRs([]string{"10.244.0.0/16"})

	if code := loginFrom(t, ts.srv, "10.244.37.12:40000", "198.51.100.1", "alice"); code == http.StatusTooManyRequests {
		t.Fatalf("the first guess at alice = 429")
	}
	if code := loginFrom(t, ts.srv, "10.244.37.12:40000", "198.51.100.2", "alice"); code != http.StatusTooManyRequests {
		t.Fatalf("a second guess at alice from a fresh forwarded address = %d, want 429", code)
	}
}
