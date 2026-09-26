package httpapi

import (
	"net/http"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

// Header mode keeps two trusts apart: identity headers only from auth.header.trustedProxyCIDRs,
// X-Forwarded-For only from clientAddress.trustedProxyCIDRs, and the header list stands in for the
// client-address list only while that one is empty.
func TestHeaderModeIdentityAndClientAddressTrustStaySeparate(t *testing.T) {
	const authProxy, ingressPod = "10.0.0.5:40000", "10.244.1.7:40000"
	for _, tc := range []struct {
		name          string
		clientAddress []string
		// The audit remoteAddr for a request from the ingress pod and from the auth proxy.
		wantIngressAddr, wantProxyAddr string
	}{
		{"clientAddress set", []string{"10.244.0.0/16"}, "203.0.113.9", authProxy},
		{"clientAddress empty", nil, ingressPod, "198.51.100.20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kv := cache.NewInProcessKV()
			t.Cleanup(kv.Close)
			cfg := authTestConfig("header")
			cfg.Auth.Header = config.HeaderConfig{
				UserHeader: "X-Remote-User", GroupsHeader: "X-Remote-Groups", GroupsDelimiter: ",",
				TrustedProxyCIDRs: []string{"10.0.0.5/32"},
			}
			cfg.ClientAddress.TrustedProxyCIDRs = tc.clientAddress
			audit := &fakeAuditStore{}
			s := newServerWithConfig(t, cfg, kv, Deps{
				Authenticator: authn.NewHeader(cfg.Auth.Header),
				Roles:         fakeRoleResolver{roles: []string{"viewer"}},
				Audit:         audit,
			})

			deleteBinding := func(remoteAddr, xff string) int {
				t.Helper()
				return doRequest(t, s, http.MethodDelete, "/api/v1/rbac/bindings/b1", nil, func(r *http.Request) {
					r.RemoteAddr = remoteAddr
					r.Header.Set("X-Remote-User", "alice")
					r.Header.Set("X-Forwarded-For", xff)
				}).Code
			}
			if code := deleteBinding(ingressPod, "203.0.113.9"); code != http.StatusUnauthorized {
				t.Fatalf("identity headers from the client-address proxy = %d, want 401", code)
			}
			if code := deleteBinding(authProxy, "198.51.100.20"); code != http.StatusForbidden {
				t.Fatalf("identity headers from the auth proxy = %d, want 403 (alice as viewer)", code)
			}

			rows := waitForAuditEntries(t, audit, 2)
			if rows[0].SubjectID != "" || rows[0].RemoteAddr != tc.wantIngressAddr {
				t.Errorf("ingress row = subject %q from %q, want no subject from %q", rows[0].SubjectID, rows[0].RemoteAddr, tc.wantIngressAddr)
			}
			if rows[1].SubjectID != "alice" || rows[1].RemoteAddr != tc.wantProxyAddr {
				t.Errorf("auth proxy row = subject %q from %q, want alice from %q", rows[1].SubjectID, rows[1].RemoteAddr, tc.wantProxyAddr)
			}
		})
	}
}
