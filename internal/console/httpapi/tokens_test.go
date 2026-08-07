package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// fakeTokenStore is a store.TokenStore double, mutex-guarded. It implements
// the full interface (not just TokenAdmin) so the SAME instance can back
// both the tokens admin API (Deps.Tokens) and authn.NewTokenFallback's
// TokenStore -- letting TestTokensCreatedTokenAuthenticates prove a token
// this endpoint mints actually authenticates, against the real hashing
// path, with no separate/divergent fake.
type fakeTokenStore struct {
	mu     sync.Mutex
	tokens map[string]store.Token
	hashes map[string]string // hex(hash) -> id
	nextN  int
}

func newFakeTokenStore() *fakeTokenStore {
	return &fakeTokenStore{tokens: map[string]store.Token{}, hashes: map[string]string{}}
}

func (f *fakeTokenStore) CreateToken(_ context.Context, name string, hash []byte, owner string, expiresAt *time.Time) (store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextN++
	id := fmt.Sprintf("tok-%d", f.nextN)
	tok := store.Token{ID: id, Name: name, Owner: owner, ExpiresAt: expiresAt, CreatedAt: time.Now()}
	f.tokens[id] = tok
	f.hashes[hex.EncodeToString(hash)] = id
	return tok, nil
}

func (f *fakeTokenStore) ListTokens(context.Context) ([]store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Token, 0, len(f.tokens))
	for _, t := range f.tokens {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeTokenStore) RevokeToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	if !ok || t.RevokedAt != nil {
		return store.ErrNotFound
	}
	now := time.Now()
	t.RevokedAt = &now
	f.tokens[id] = t
	return nil
}

func (f *fakeTokenStore) GetTokenByHash(_ context.Context, hash []byte) (store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.hashes[hex.EncodeToString(hash)]
	if !ok {
		return store.Token{}, store.ErrNotFound
	}
	return f.tokens[id], nil
}

func (f *fakeTokenStore) TouchTokenLastUsed(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now()
	t.LastUsedAt = &now
	f.tokens[id] = t
	return nil
}

// newTokensTestServer wires a Server holding tokens:manage (via role
// "tester") and the given TokenAdmin.
func newTokensTestServer(t *testing.T, tokens TokenAdmin) *Server {
	t.Helper()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTokensManage}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1", DisplayName: "admin"}}
	return newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, Tokens: tokens})
}

func TestTokensWithoutStoreReturns503(t *testing.T) {
	s := newTokensTestServer(t, nil)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/tokens"},
		{http.MethodPost, "/api/v1/tokens"},
		{http.MethodDelete, "/api/v1/tokens/tok-1"},
	} {
		var body func(*http.Request)
		if isMutatingMethod(c.method) {
			body = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(`{}`), body)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a TokenAdmin = %d, want 503", c.method, c.path, w.Code)
		}
	}
}

func TestTokensCreateRejectsEmptyName(t *testing.T) {
	s := newTokensTestServer(t, newFakeTokenStore())
	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":""}`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
	}
}

// TestTokensCreateResponseShapeAndSecrecy pins the brief's response shape
// verbatim -- {"id","name","token":"kcm_...","expiresAt"} -- and that the
// token appears here, in the CREATION response, exactly once.
func TestTokensCreateResponseShapeAndSecrecy(t *testing.T) {
	s := newTokensTestServer(t, newFakeTokenStore())
	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"ci"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var body struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID == "" || body.Name != "ci" {
		t.Fatalf("body = %+v, want id set and name=ci", body)
	}
	if !strings.HasPrefix(body.Token, "kcm_") {
		t.Errorf("token = %q, want the kcm_ wire prefix", body.Token)
	}
	// This is the one response in the API that carries a plaintext secret, so
	// it must not be cacheable by a shared proxy or by the browser itself.
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
	if got := w.Header().Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want %q", got, "no-cache")
	}
}

// TestTokensCreatedTokenAuthenticates is the brief's last failing test:
// POST /api/v1/tokens returns a token that then authenticates successfully
// against a permitted route, and a second GET /api/v1/tokens does not
// contain it.
func TestTokensCreatedTokenAuthenticates(t *testing.T) {
	tokens := newFakeTokenStore()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTokensManage}})
	inner := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectUser, ID: "u1", DisplayName: "admin"}}
	authr := authn.NewTokenFallback(tokens, inner)
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, Tokens: tokens})

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"ci"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The minted token authenticates on its own, with no cookie at all,
	// against a route requiring the same permission (GET /api/v1/tokens
	// itself -- tokens:manage).
	w = doRequest(t, s, http.MethodGet, "/api/v1/tokens", nil, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+created.Token)
	})
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d with the minted token: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), created.Token) {
		t.Errorf("second GET /api/v1/tokens body contains the raw token: %s", w.Body)
	}
	var list struct {
		Tokens []tokenResponse `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].Name != "ci" {
		t.Fatalf("tokens = %+v, want the one ci token, no hash/secret field", list.Tokens)
	}
}

func TestTokensDeleteRevokesNotDeletes(t *testing.T) {
	tokens := newFakeTokenStore()
	s := newTokensTestServer(t, tokens)

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"ci"}`), mutateWithCSRF)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	w = doRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+created.ID, nil, mutateWithCSRF)
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status %d: %s", w.Code, w.Body)
	}

	// The row is kept (revoked, not deleted) -- it still shows up in
	// ListTokens with RevokedAt set.
	got, err := tokens.ListTokens(context.Background())
	if err != nil || len(got) != 1 || got[0].RevokedAt == nil {
		t.Fatalf("tokens after revoke = %+v, err=%v, want one row with RevokedAt set", got, err)
	}
}

// TestOwnerFor pins ownerFor's contract directly, table-style, against the
// regression that would silently no-op the whole owner-disabled check: a
// SubjectUser with a non-empty DisplayName must still return ID, never
// DisplayName -- DisplayName is a human-facing string (users.display_name,
// or whatever a trusted header proxy sent) that authn.checkOwnerDisabled
// cannot look up as a users.id UUID, so returning it here would make every
// disable check silently pass every token through.
func TestOwnerFor(t *testing.T) {
	const userID = "11111111-1111-1111-1111-111111111111"
	const tokenID = "tok-parent-1"

	cases := []struct {
		name    string
		subject authz.Subject
		want    string
	}{
		{
			name:    "SubjectUser with non-empty DisplayName returns ID not DisplayName",
			subject: authz.Subject{Kind: authz.SubjectUser, ID: userID, DisplayName: "Alice Example"},
			want:    userID,
		},
		{
			name:    "SubjectToken returns ID",
			subject: authz.Subject{Kind: authz.SubjectToken, ID: tokenID, DisplayName: "ci"},
			want:    tokenID,
		},
		{
			name:    "SubjectAnonymous returns system",
			subject: authz.Subject{Kind: authz.SubjectAnonymous, ID: "anonymous", DisplayName: "Anonymous"},
			want:    "system",
		},
		{
			name:    "empty ID returns system",
			subject: authz.Subject{Kind: authz.SubjectUser, ID: "", DisplayName: "no id"},
			want:    "system",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ownerFor(c.subject); got != c.want {
				t.Errorf("ownerFor(%+v) = %q, want %q", c.subject, got, c.want)
			}
		})
	}
}

// TestTokensCreateInheritsOwnerFromParentToken is I-3's structural fix: a
// token minted by a request authenticated AS an existing token (subject.Kind
// == authz.SubjectToken) must inherit that PARENT token's own Owner, not the
// parent token's id -- collapsing an arbitrarily deep token-mints-token
// chain to depth 1, so disabling the root user invalidates every descendant
// in one step.
func TestTokensCreateInheritsOwnerFromParentToken(t *testing.T) {
	tokens := newFakeTokenStore()
	const rootUserID = "22222222-2222-2222-2222-222222222222"
	parent, err := tokens.CreateToken(context.Background(), "parent", []byte("parent-hash"), rootUserID, nil)
	if err != nil {
		t.Fatalf("seed parent token: %v", err)
	}

	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTokensManage}})
	// The creating subject IS the parent token above -- simulating a token
	// minting another token.
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectToken, ID: parent.ID, DisplayName: parent.Name}}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, Tokens: tokens})

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"child"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if decodeErr := json.Unmarshal(w.Body.Bytes(), &created); decodeErr != nil {
		t.Fatalf("decode: %v", decodeErr)
	}

	got, err := tokens.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var child *store.Token
	for i := range got {
		if got[i].ID == created.ID {
			child = &got[i]
		}
	}
	if child == nil {
		t.Fatalf("created token %q not found in ListTokens: %+v", created.ID, got)
	}
	if child.Owner != rootUserID {
		t.Errorf("child token Owner = %q, want the root user UUID %q (inherited from the parent token, not the parent's own id %q)", child.Owner, rootUserID, parent.ID)
	}
}

// TestTokensCreateFallsBackToSubjectIDWhenParentTokenNotFound proves the
// mint must never fail over attribution bookkeeping: when the creating
// subject is a SubjectToken whose id names no row in the store at all (e.g.
// a fake/incomplete TokenAdmin, or a genuine race), ownerFor's original
// answer -- the subject's own id -- is still stored, exactly as before this
// fix.
func TestTokensCreateFallsBackToSubjectIDWhenParentTokenNotFound(t *testing.T) {
	tokens := newFakeTokenStore()
	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTokensManage}})
	const missingParentID = "tok-does-not-exist"
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectToken, ID: missingParentID, DisplayName: "ghost"}}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, Tokens: tokens})

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"child"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got, err := tokens.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var child *store.Token
	for i := range got {
		if got[i].ID == created.ID {
			child = &got[i]
		}
	}
	if child == nil {
		t.Fatalf("created token %q not found in ListTokens: %+v", created.ID, got)
	}
	if child.Owner != missingParentID {
		t.Errorf("child token Owner = %q, want fallback to subject.ID %q (parent not found)", child.Owner, missingParentID)
	}
}

func TestTokensDeleteNotFound(t *testing.T) {
	s := newTokensTestServer(t, newFakeTokenStore())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/does-not-exist", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}
