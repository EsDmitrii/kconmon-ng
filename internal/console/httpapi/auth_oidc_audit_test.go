package httpapi

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
)

func oidcAuditServer(t *testing.T, flow fakeOIDCFlow, audit Auditor, sessions *authn.SessionStore) *Server { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	cfg := authTestConfig("oidc")
	reg := prometheus.NewRegistry()
	return NewServer(Deps{
		Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
		UI:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
		OIDC: flow, Audit: audit, Sessions: sessions,
	})
}

/*
Local mode records every sign-in, failed or not; an OIDC sign-in left no row at all, so an operator
investigating a compromised account could not see when or from where that identity signed in.
*/
func TestOIDCSignInIsAuditedWithTheSignedInIdentity(t *testing.T) {
	sessions := authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0)
	id, err := sessions.Create(t.Context(), authn.Session{Username: "oidc:sub-42", DisplayName: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	audit := &fakeAuditStore{}
	s := oidcAuditServer(t, fakeOIDCFlow{sessionID: id, returnTo: "/"}, audit, sessions)

	w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=st&code=c", nil, withOIDCState("st"))
	if w.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302: %s", w.Code, w.Body)
	}
	e := waitForOneAuditEntry(t, audit)[0]
	if e.Action != "GET "+config.OIDCCallbackPath || e.Outcome != auditOutcomeAllowed ||
		e.SubjectKind != "user" || e.SubjectID != "oidc:sub-42" {
		t.Fatalf("sign-in row = %+v, want an allowed row for user oidc:sub-42", e)
	}
}

func TestRefusedOIDCCallbacksAreAudited(t *testing.T) {
	for name, c := range map[string]struct {
		flow  fakeOIDCFlow
		state string
	}{
		"state from another browser": {flow: fakeOIDCFlow{sessionID: "x"}, state: "other"},
		"token exchange failed":      {flow: fakeOIDCFlow{callbackErr: errors.New("exchange failed")}, state: "st"},
	} {
		t.Run(name, func(t *testing.T) {
			audit := &fakeAuditStore{}
			s := oidcAuditServer(t, c.flow, audit, authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0))
			w := doRequest(t, s, http.MethodGet, config.OIDCCallbackPath+"?state=st&code=c", nil, withOIDCState(c.state))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("callback = %d, want 401", w.Code)
			}
			e := waitForOneAuditEntry(t, audit)[0]
			if e.Action != "GET "+config.OIDCCallbackPath || e.Outcome == auditOutcomeAllowed {
				t.Fatalf("refused callback row = %+v, want a refused row", e)
			}
		})
	}
}
