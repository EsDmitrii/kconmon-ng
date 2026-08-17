package authn_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
)

func newHeaderFixture(t *testing.T, delimiter string) authn.Authenticator {
	t.Helper()
	return authn.NewHeader(config.HeaderConfig{
		UserHeader:        "X-Remote-User",
		GroupsHeader:      "X-Remote-Groups",
		GroupsDelimiter:   delimiter,
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
	})
}

func TestHeaderAuthenticateTrustedPeerResolvesSubjectWithGroups(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:54321"
	r.Header.Set("X-Remote-User", "alice")
	r.Header.Set("X-Remote-Groups", "a,b")

	subject, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := authz.Subject{Kind: authz.SubjectUser, ID: "alice", DisplayName: "alice", Groups: []string{"a", "b"}}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("got %+v, want %+v", subject, want)
	}
}

func TestHeaderAuthenticateUntrustedPeerIsNoCredentialsEvenWithHeadersSet(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:54321" // outside 10.0.0.0/8
	r.Header.Set("X-Remote-User", "alice")
	r.Header.Set("X-Remote-Groups", "a,b")

	_, err := a.Authenticate(r)
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials (this is the security-critical case: an untrusted peer must never be trusted, headers or not)", err)
	}
}

func TestHeaderAuthenticateEmptyUserHeaderFromTrustedProxyIsNoCredentials(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	// X-Remote-User deliberately left unset.
	r.Header.Set("X-Remote-Groups", "a,b")

	_, err := a.Authenticate(r)
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// A trusted proxy asserts a USERNAME. It does not get to assert an identity minted by another auth
// mode: "oidc:<sub>" through this header, on a deployment that switched away from OIDC and still
// has the old bindings, would hand the caller everything that subject held (authn/identity.go).
func TestHeaderAuthenticateRefusesAReservedIdentityNamespace(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"oidc:user-sub-1", "local:0a5f", "token:0a5f", "header:svc"} {
		t.Run(user, func(t *testing.T) {
			t.Parallel()
			a := newHeaderFixture(t, ",")

			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.1:1"
			r.Header.Set("X-Remote-User", user)

			if _, err := a.Authenticate(r); !errors.Is(err, authn.ErrNoCredentials) {
				t.Fatalf("Authenticate with X-Remote-User=%q = %v, want ErrNoCredentials", user, err)
			}
		})
	}
}

func TestHeaderAuthenticateCustomDelimiter(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, "|")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Remote-User", "bob")
	r.Header.Set("X-Remote-Groups", "x|y|z")

	subject, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !reflect.DeepEqual(subject.Groups, []string{"x", "y", "z"}) {
		t.Errorf("Groups = %v, want [x y z]", subject.Groups)
	}
}

// TestHeaderAuthenticateSpoofedXForwardedForDoesNotGrantTrust is the other security-critical case.
func TestHeaderAuthenticateSpoofedXForwardedForDoesNotGrantTrust(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:54321" // untrusted peer
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Remote-User", "alice")

	_, err := a.Authenticate(r)
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// TestHeaderAuthenticateDuplicateUserHeaderIsNoCredentials proves an append-not-overwrite proxy bug
// (or a client that smuggled its own copy of the identity header ahead of the trusted proxy, which
// then appended rather than replaced) cannot slip a second X-Remote-User value past this
// authenticator: net/http merges repeated headers, so r.Header.Get alone would silently see only
// the first value and hide the second one entirely -- that must be treated as "no credentials", not
// resolved by picking either value.
func TestHeaderAuthenticateDuplicateUserHeaderIsNoCredentials(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Add("X-Remote-User", "alice")
	r.Header.Add("X-Remote-User", "mallory")

	_, err := a.Authenticate(r)
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials for a duplicated user header", err)
	}
}

// TestHeaderAuthenticateDuplicateGroupsHeaderIsNoCredentials is the same
// defense as the user-header case above, for X-Remote-Groups.
func TestHeaderAuthenticateDuplicateGroupsHeaderIsNoCredentials(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Remote-User", "alice")
	r.Header.Add("X-Remote-Groups", "a,b")
	r.Header.Add("X-Remote-Groups", "admin")

	_, err := a.Authenticate(r)
	if !errors.Is(err, authn.ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials for a duplicated groups header", err)
	}
}

func TestHeaderAuthenticateModeIsHeader(t *testing.T) {
	t.Parallel()
	a := newHeaderFixture(t, ",")
	if got := a.Mode(); got != "header" {
		t.Errorf("Mode() = %q, want %q", got, "header")
	}
}
