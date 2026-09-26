package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// repeatedByte is an endless body of one byte, so a test can send 16 MiB without holding 16 MiB.
type repeatedByte byte

func (b repeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// hugeLoginBody is a login body whose password is n bytes, generated as it is read.
func hugeLoginBody(n int64) io.Reader {
	return io.MultiReader(
		strings.NewReader(`{"username":"alice","password":"`),
		io.LimitReader(repeatedByte('a'), n),
		strings.NewReader(`"}`),
	)
}

// allocatedDuring reports how many bytes the process allocated while fn ran.
func allocatedDuring(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

const hugeBodyBytes = 16 << 20

/*
An unauthenticated login is public, and the audit middleware used to buffer its whole body before
any rate limit: a 16 MiB body cost about 50 MiB of heap per request, so a few concurrent requests
took a 256Mi console past its limit. A login body is a username and a password.
*/
func TestPublicLoginBodyIsBoundedBeforeTheAuditReadsIt(t *testing.T) {
	hash, err := authn.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"oidc", "local"} {
		t.Run(mode, func(t *testing.T) {
			cfg := authTestConfig(mode)
			reg := prometheus.NewRegistry()
			audit := &fakeAuditStore{}
			s := NewServer(Deps{
				Config: cfg, Metrics: metrics.New(cfg.MetricsPrefix, reg), PromRegistry: reg,
				UI: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("spa")) }),
				Users: fakeUserStore{users: map[string]store.User{
					"alice": {ID: "u-1", Username: "alice", PasswordHash: hash},
				}},
				Sessions:      authn.NewSessionStore(cache.NewInProcessKV(), time.Hour, 0),
				Authenticator: fakeAuthenticator{err: authn.ErrNoCredentials, mode: mode},
				Audit:         audit,
			})

			var code int
			allocated := allocatedDuring(func() {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login", hugeLoginBody(hugeBodyBytes))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				code = rec.Code
			})
			if allocated > 2<<20 {
				t.Errorf("one unauthenticated 16 MiB login allocated %d MiB, want under 2 MiB", allocated>>20)
			}
			if code == http.StatusNoContent {
				t.Errorf("a 16 MiB login body was accepted")
			}

			// The attempt is still on record, and says the body was not described.
			entries := waitForOneAuditEntry(t, audit)
			if got := string(entries[0].Detail); got != `{"truncated":true}` {
				t.Errorf("audit detail = %s, want {\"truncated\":true}", got)
			}
		})
	}
}

// A real login body still fits the public limit with room to spare, escapes and all.
func TestLoginWithAnEscapeHeavyPasswordStillFitsThePublicLimit(t *testing.T) {
	s := newLocalAuthServer(t)
	escaped := strings.Repeat(`é`, maxPasswordBytes)
	body := `{"username":"` + strings.Repeat(`a`, maxLoginUsernameBytes) + `","password":"` + escaped + `"}`
	if len(body) > publicRouteBodyBytes {
		t.Fatalf("the largest real login body is %d bytes, over the %d-byte public limit", len(body), publicRouteBodyBytes)
	}
	w := doRequest(t, s, http.MethodPost, "/api/v1/auth/login", strings.NewReader(body), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("login with a long wrong password = %d, want 401: %s", w.Code, w.Body)
	}
}

/*
On a signed-in route the handler reads the body anyway, but the audit used to hold its own full copy
of it next to the handler's. It now reads a bounded prefix and hands the handler the rest unread.
*/
func TestAuditCaptureHoldsABoundedPrefixAndReplaysTheWholeBody(t *testing.T) {
	s := newAuditTestServer(t, &fakeAuditStore{}, nil, Deps{})
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/api/v1/rbac/roles"}
	body := io.MultiReader(
		strings.NewReader(`{"name":"`),
		io.LimitReader(repeatedByte('x'), hugeBodyBytes),
		strings.NewReader(`","permissions":[]}`),
	)
	req := httptest.NewRequestWithContext(context.WithValue(t.Context(), chi.RouteCtxKey, rctx),
		http.MethodPost, "/api/v1/rbac/roles", body)

	var detail json.RawMessage
	allocated := allocatedDuring(func() { detail = s.captureAuditDetail(req) })
	if allocated > 2<<20 {
		t.Errorf("capturing the audit detail of a 16 MiB body allocated %d MiB, want under 2 MiB", allocated>>20)
	}
	if string(detail) != `{"truncated":true}` {
		t.Errorf("detail = %s, want {\"truncated\":true} for a body too large to describe", detail)
	}

	n, err := io.Copy(io.Discard, req.Body)
	if err != nil {
		t.Fatalf("read the replayed body: %v", err)
	}
	if want := int64(len(`{"name":"`) + hugeBodyBytes + len(`","permissions":[]}`)); n != want {
		t.Errorf("handler would read %d bytes, want the whole body of %d", n, want)
	}
}

// A body inside the capture bound is described exactly as before, and the handler reads it intact.
func TestAuditCaptureStillDescribesAnOrdinaryBody(t *testing.T) {
	s := newAuditTestServer(t, &fakeAuditStore{}, nil, Deps{})
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/api/v1/rbac/roles"}
	const body = `{"name":"ops","permissions":["runs:read"]}`
	req := httptest.NewRequestWithContext(context.WithValue(t.Context(), chi.RouteCtxKey, rctx),
		http.MethodPost, "/api/v1/rbac/roles", strings.NewReader(body))

	detail := s.captureAuditDetail(req)
	if string(detail) != `{"name":"ops","permissions":["runs:read"]}` {
		t.Errorf("detail = %s", detail)
	}
	rest, err := io.ReadAll(req.Body)
	if err != nil || string(rest) != body {
		t.Errorf("replayed body = %q (%v), want %q", rest, err, body)
	}
}

// The password change is public too: a signed-in caller's body gets the same bound.
func TestPublicPasswordChangeBodyIsBounded(t *testing.T) {
	hash, err := authn.HashPassword("the current secret")
	if err != nil {
		t.Fatal(err)
	}
	alice := store.User{ID: "u-1", Username: "alice", PasswordHash: hash}
	admin := newFakeUserAdmin(alice)
	s := newLocalAuthServer(t)
	s.userAdmin = admin
	s.users = admin
	mine, err := s.sessions.Create(t.Context(), authn.Session{Username: "alice", PasswordStamp: authn.PasswordStamp(hash)})
	if err != nil {
		t.Fatal(err)
	}

	var code int
	allocated := allocatedDuring(func() {
		body := io.MultiReader(
			strings.NewReader(`{"currentPassword":"`),
			io.LimitReader(repeatedByte('a'), hugeBodyBytes),
			strings.NewReader(`","newPassword":"the next long secret"}`),
		)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/password", body)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: s.cfg.Auth.Session.CookieName, Value: mine})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		code = rec.Code
	})
	if allocated > 2<<20 {
		t.Errorf("one 16 MiB password change allocated %d MiB, want under 2 MiB", allocated>>20)
	}
	if code != http.StatusBadRequest {
		t.Errorf("16 MiB password change = %d, want 400", code)
	}
}

// A header-mode identity is the proxy's bytes; one that is not UTF-8 must not cost the row.
func TestAuditRowSurvivesASubjectIDThatIsNotUTF8(t *testing.T) {
	fs := &fakeAuditStore{}
	s := newAuditTestServer(t, fs, nil, Deps{})
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{"/api/v1/annotations"}
	req := httptest.NewRequestWithContext(context.WithValue(t.Context(), chi.RouteCtxKey, rctx),
		http.MethodPost, "/api/v1/annotations", http.NoBody)
	s.recordAudit(req, authz.Subject{Kind: authz.SubjectUser, ID: "bob\xff"}, auditOutcomeDenied, emptyDetail)
	if got := waitForOneAuditEntry(t, fs)[0].SubjectID; !utf8.ValidString(got) {
		t.Fatalf("subject id %q is not UTF-8, PostgreSQL refuses the row", got)
	}
}
