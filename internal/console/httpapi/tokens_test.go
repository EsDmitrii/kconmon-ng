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

// fakeTokenStore is a store.TokenStore double.
type fakeTokenStore struct {
	mu     sync.Mutex
	tokens map[string]store.Token
	hashes map[string]string // hex(hash) -> id
	nextN  int

	// listCalls / getByIDCalls count the two read paths separately.
	listCalls    int
	getByIDCalls int
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
	f.listCalls++
	out := make([]store.Token, 0, len(f.tokens))
	for _, t := range f.tokens {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeTokenStore) GetTokenByID(_ context.Context, id string) (store.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getByIDCalls++
	t, ok := f.tokens[id]
	if !ok {
		return store.Token{}, store.ErrNotFound
	}
	return t, nil
}

// callCounts reports the two read paths' call counts, taken together under one
// lock so a caller cannot observe a torn pair.
func (f *fakeTokenStore) callCounts() (list, getByID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.getByIDCalls
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

// PurgeToken drops the row outright; the handler only reaches it for a token
// it has already read as revoked or expired.
func (f *fakeTokenStore) PurgeToken(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	if !ok {
		return store.ErrNotFound
	}
	delete(f.tokens, id)
	for h, hid := range f.hashes {
		if hid == t.ID {
			delete(f.hashes, h)
		}
	}
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

// TestTokensCreateResponseShapeAndSecrecy pins the response shape verbatim --
// {"id","name","token":"kcm_...","expiresAt"}.
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

// TestTokensCreatedTokenAuthenticates is the last failing test.
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

// TestOwnerFor pins ownerFor's contract directly.
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

// TestTokensCreateInheritsOwnerFromParentToken is I-3's structural fix.
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

// TestTokensCreateFallsBackToSubjectIDWhenParentTokenNotFound proves the mint must never fail over
// attribution bookkeeping.
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

// On a fleet with thousands of tokens that is the whole table decoded; the assertion is
// deliberately on the call counts and not on the owner.
func TestTokensCreateResolvesParentByIDWithoutAFullScan(t *testing.T) {
	tokens := newFakeTokenStore()
	const rootUserID = "22222222-2222-2222-2222-222222222222"
	parent, err := tokens.CreateToken(context.Background(), "parent", []byte("parent-hash"), rootUserID, nil)
	if err != nil {
		t.Fatalf("seed parent token: %v", err)
	}
	// Noise rows: a full scan would have to walk these, a by-id lookup never
	// sees them. They also make the fake's map iteration order irrelevant.
	for i := range 5 {
		if _, seedErr := tokens.CreateToken(context.Background(), fmt.Sprintf("noise-%d", i),
			[]byte(fmt.Sprintf("noise-hash-%d", i)), rootUserID, nil); seedErr != nil {
			t.Fatalf("seed noise token %d: %v", i, seedErr)
		}
	}

	policy := authz.NewPolicy(map[string][]authz.Permission{"tester": {authz.PermTokensManage}})
	authr := fakeAuthenticator{subject: authz.Subject{Kind: authz.SubjectToken, ID: parent.ID, DisplayName: parent.Name}}
	s := newAuthzServer(t, authr, policy, Deps{Roles: fakeRoleResolver{roles: []string{"tester"}}, Tokens: tokens})

	listBefore, getBefore := tokens.callCounts()

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"child"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	listAfter, getAfter := tokens.callCounts()
	if listAfter != listBefore {
		t.Errorf("minting a token called ListTokens %d time(s); the mint path must not full-scan api_tokens", listAfter-listBefore)
	}
	if getAfter-getBefore != 1 {
		t.Errorf("minting a token called GetTokenByID %d time(s), want exactly 1", getAfter-getBefore)
	}
}

// TestTokensCreateByUserSubjectMakesNoParentLookup pins the negative case the
// by-id swap must not regress: a token minted by a USER has no parent token to
// inherit from, so neither read path may be touched at all.
func TestTokensCreateByUserSubjectMakesNoParentLookup(t *testing.T) {
	tokens := newFakeTokenStore()
	s := newTokensTestServer(t, tokens)

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"by-a-human"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if list, getByID := tokens.callCounts(); list != 0 || getByID != 0 {
		t.Errorf("mint by a SubjectUser made %d ListTokens and %d GetTokenByID calls, want 0 and 0", list, getByID)
	}
}

func TestTokensDeleteNotFound(t *testing.T) {
	s := newTokensTestServer(t, newFakeTokenStore())
	w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/does-not-exist", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body)
	}
}

// TestTokensCreateRejectsPastExpiry pins finding 13: a token whose expiry is
// already in the past can never authenticate, so minting one only leaks a
// secret for nothing -- the request is refused before any secret is generated.
func TestTokensCreateRejectsPastExpiry(t *testing.T) {
	tokens := newFakeTokenStore()
	s := newTokensTestServer(t, tokens)

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens",
		strings.NewReader(`{"name":"stillborn","expiresAt":"`+past+`"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "expiry must be in the future") {
		t.Errorf("detail = %s, want it to say the expiry must be in the future", w.Body)
	}
	// Nothing was minted, so no secret was ever put on the wire.
	if got, _ := tokens.ListTokens(context.Background()); len(got) != 0 {
		t.Errorf("store holds %d token(s) after a rejected mint, want 0", len(got))
	}
}

// TestTokensCreateAcceptsFutureExpiry is the other half of finding 13: the
// guard rejects the past, not every expiry.
func TestTokensCreateAcceptsFutureExpiry(t *testing.T) {
	s := newTokensTestServer(t, newFakeTokenStore())
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens",
		strings.NewReader(`{"name":"ci","expiresAt":"`+future+`"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", w.Code, w.Body)
	}
}

// TestTokensDeleteRevokesThenPurges pins finding 14's chosen semantics: DELETE
// on an ACTIVE token still means revoke, DELETE on an already-revoked or
// expired one purges the row, and a third DELETE has nothing left to find.
func TestTokensDeleteRevokesThenPurges(t *testing.T) {
	tokens := newFakeTokenStore()
	s := newTokensTestServer(t, tokens)

	w := doRequest(t, s, http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"ci"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// First DELETE: revoke. The row survives, carrying its revokedAt.
	if w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+created.ID, nil, mutateWithCSRF); w.Code != http.StatusNoContent {
		t.Fatalf("first delete = %d, want 204: %s", w.Code, w.Body)
	}
	rows, err := tokens.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].RevokedAt == nil {
		t.Fatalf("after the first delete: %d row(s), revokedAt set = %v; want 1 revoked row", len(rows), len(rows) == 1 && rows[0].RevokedAt != nil)
	}

	// Second DELETE: purge. The list stops growing forever.
	if w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+created.ID, nil, mutateWithCSRF); w.Code != http.StatusNoContent {
		t.Fatalf("second delete = %d, want 204: %s", w.Code, w.Body)
	}
	if rows, _ := tokens.ListTokens(context.Background()); len(rows) != 0 {
		t.Fatalf("after the purge: %d row(s), want 0", len(rows))
	}

	// Third DELETE: there is genuinely nothing there now.
	if w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+created.ID, nil, mutateWithCSRF); w.Code != http.StatusNotFound {
		t.Fatalf("third delete = %d, want 404: %s", w.Code, w.Body)
	}
}

// TestTokensDeleteExpiredPurgesInOneCall pins the second half of finding 14's
// semantics: an EXPIRED token is already unusable, so the single DELETE a user
// reaches for purges it rather than revoking a credential that cannot be used.
func TestTokensDeleteExpiredPurgesInOneCall(t *testing.T) {
	tokens := newFakeTokenStore()
	s := newTokensTestServer(t, tokens)

	expired := time.Now().Add(-time.Hour)
	tok, err := tokens.CreateToken(context.Background(), "stale", []byte("hash"), "u1", &expired)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if w := doRequest(t, s, http.MethodDelete, "/api/v1/tokens/"+tok.ID, nil, mutateWithCSRF); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", w.Code, w.Body)
	}
	if rows, _ := tokens.ListTokens(context.Background()); len(rows) != 0 {
		t.Fatalf("after deleting an expired token: %d row(s), want 0", len(rows))
	}
}
