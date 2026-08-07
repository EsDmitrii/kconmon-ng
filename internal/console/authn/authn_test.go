package authn_test

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

func TestNewAnonymousYieldsFixedSubjectForEveryRequest(t *testing.T) {
	t.Parallel()

	a := authn.NewAnonymous("viewer")
	want := authz.Subject{Kind: authz.SubjectAnonymous, ID: "anonymous", Roles: []string{"viewer"}}

	if got := a.Mode(); got != "anonymous" {
		t.Errorf("Mode() = %q, want %q", got, "anonymous")
	}

	plain := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	subject, err := a.Authenticate(plain)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("got %+v, want %+v", subject, want)
	}

	// Even a request carrying junk auth headers (a cookie, a bearer token, a
	// spoofed trusted-proxy header) resolves to the exact same fixed
	// Subject: NewAnonymous is a real Subject through the real authorize
	// middleware, not a conditional bypass that a crafted request could
	// influence.
	junk := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	junk.Header.Set("Authorization", "Bearer kcm_totally-not-a-real-token")
	junk.Header.Set("X-Remote-User", "someone-else")
	junk.Header.Set("X-Remote-Groups", "admins")
	junk.AddCookie(&http.Cookie{Name: "kconmon_session", Value: "forged"})
	junk.RemoteAddr = "10.0.0.1:1"

	subject, err = a.Authenticate(junk)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("with junk headers set: got %+v, want %+v", subject, want)
	}
}
