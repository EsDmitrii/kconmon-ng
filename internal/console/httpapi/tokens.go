package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// TokenAdmin is the subset of store.TokenStore the tokens admin API needs.
// store.TokenStore's GetTokenByHash and TouchTokenLastUsed are deliberately
// excluded -- those back token AUTHENTICATION (authn.NewTokenFallback), not
// this admin CRUD surface.
type TokenAdmin interface {
	CreateToken(ctx context.Context, name string, hash []byte, owner string, expiresAt *time.Time) (store.Token, error)
	ListTokens(ctx context.Context) ([]store.Token, error)
	RevokeToken(ctx context.Context, id string) error
}

// tokenSecretBytes mirrors authn.EncodeToken's own 32-byte contract
// (authn/token.go's tokenSecretBytes) -- kept as a local constant, not
// exported from authn, since this is the only other place in the codebase
// that mints a token's raw secret.
const tokenSecretBytes = 32

// tokensUnavailable answers 503 and reports true when s.tokens is nil
// (database.mode=disabled).
func (s *Server) tokensUnavailable(w http.ResponseWriter) bool {
	if s.tokens == nil {
		writeProblem(w, http.StatusServiceUnavailable, "token admin not available",
			"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/tokens")
		return true
	}
	return false
}

// tokenResponse is GET /api/v1/tokens's per-row shape. It structurally
// cannot carry a hash or a secret: store.Token (store/auth.go) has no such
// field to map from -- the same guarantee store.TokenStore.ListTokens'
// own doc comment describes.
type tokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Owner      string     `json:"owner"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

func tokenResponseFrom(t *store.Token) tokenResponse {
	return tokenResponse{
		ID: t.ID, Name: t.Name, Owner: t.Owner,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt,
		CreatedAt: t.CreatedAt,
	}
}

func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	if s.tokensUnavailable(w) {
		return
	}
	tokens, err := s.tokens.ListTokens(r.Context())
	if err != nil {
		slog.Error("list tokens failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "tokens unavailable", "failed to list tokens")
		return
	}
	out := make([]tokenResponse, 0, len(tokens))
	for i := range tokens {
		out = append(out, tokenResponseFrom(&tokens[i]))
	}
	writeJSON(w, map[string]any{"tokens": out})
}

type tokenCreateRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// tokenCreateResponse is POST /api/v1/tokens's body: the ONLY place the raw
// wire-form token ("kcm_...") is ever handed back -- task-17-brief.md
// verbatim, "exactly once, in the creation response only". It is never
// stored (only its hash is, via authn.HashTokenSecret) and never logged.
type tokenCreateResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"` //nolint:gosec // G117: the one-time wire form of a token this handler itself just minted, not a hardcoded credential
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ownerFor derives api_tokens.owner (migration 00002: "creator subject id
// (users.id UUID for local users, token id for token-created tokens) or
// 'system'") from the creating subject alone: subject.ID for both
// SubjectUser and SubjectToken, else the literal "system" for the
// degenerate case of no resolvable identity at all (unreachable in practice
// -- reaching this handler already required holding tokens:manage, which
// needs a real subject.Roles binding).
//
// ownerFor is deliberately pure -- no store access, so it cannot walk a
// token-minted-token chain back to its root user by itself. handleTokensCreate
// does that: when subject.Kind is SubjectToken, it takes ownerFor's return
// value only as a fallback and tries to replace it with the PARENT token's
// own Owner (resolveInheritedOwner, below), so a freshly minted token
// inherits the root user directly (chain collapses to depth 1) instead of
// one more link. ownerFor's subject.ID answer for SubjectToken is only ever
// actually stored when that lookup fails (parent not found, or the parent's
// own Owner is empty) or for rows created before this inheritance existed.
//
// DisplayName is NEVER used here, unlike the pre-fix version of this
// function: it is a human-facing display string (for SubjectUser, users.
// display_name; for header mode, whatever the trusted proxy sent), not a
// stable, lookupable identifier -- it can collide, change, or simply not
// name any row at all. authn.WithOwnerDisabledCheck (authn/token.go) needs
// to look owner back up as a users.id UUID to answer "is this token's
// creator currently disabled", which subject.ID (User.ID for local mode,
// authz.go's local-auth Subject construction) provides and DisplayName does
// not. For SubjectUser under header/OIDC auth, subject.ID is a proxy
// username or IdP subject with no users row at all -- correct, since that
// caller's disabled state lives at the proxy/IdP, not in this database, and
// GetUserByID simply returns ErrNotFound for it (treated as "allow": see
// authenticateToken). For SubjectToken (a token minting another token),
// subject.ID is the parent token's own id -- a canonical UUID, exactly like
// a users.id (store.formatUUID mints both the same way), NOT a non-UUID
// string that the owner-disabled check's uuid.Parse guard would skip.
// Without handleTokensCreate's inheritance, that UUID reaches a real
// GetUserByID call, gets store.ErrNotFound (no users row has a token's id),
// and is treated as "allow" -- letting a token minted by a token escape the
// disable chain entirely. Closing that gap is what the inheritance in
// handleTokensCreate is for; ownerFor's own return value here is only ever
// the pre-inheritance fallback.
func ownerFor(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	if (subject.Kind == authz.SubjectUser || subject.Kind == authz.SubjectToken) && subject.ID != "" {
		return subject.ID
	}
	return "system"
}

// handleTokensCreate mints a new API token. MUST use authn.HashTokenSecret
// + authn.EncodeToken (task-17-brief.md hard carry-forward): the single
// source of truth for the hash input (SHA-256 of the raw 32 secret bytes)
// and the wire encoding, the exact two functions authenticateToken
// (authn/token.go) verifies a presented token against -- a divergent
// implementation here would silently invalidate every token this endpoint
// issues.
func (s *Server) handleTokensCreate(w http.ResponseWriter, r *http.Request) {
	if s.tokensUnavailable(w) {
		return
	}
	var req tokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with a non-empty "name"`)
		return
	}

	secret := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(secret); err != nil {
		slog.Error("mint token secret failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "token creation failed", "")
		return
	}
	hash, err := hex.DecodeString(authn.HashTokenSecret(secret))
	if err != nil {
		// Unreachable: HashTokenSecret always returns a valid hex string
		// (its own doc comment) -- see authenticateToken's identical,
		// identically-unreachable branch (authn/token.go).
		slog.Error("decode token hash failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "token creation failed", "")
		return
	}

	subject, _ := SubjectFrom(r.Context())
	owner := ownerFor(subject)
	if subject.Kind == authz.SubjectToken {
		owner = s.resolveInheritedOwner(r.Context(), subject.ID, owner)
	}
	tok, err := s.tokens.CreateToken(r.Context(), req.Name, hash, owner, req.ExpiresAt)
	if err != nil {
		slog.Error("create token failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "tokens unavailable", "failed to create token")
		return
	}

	// The only response in the API that carries a plaintext secret: keep it
	// out of every cache on the way back (shared proxies, the browser's own
	// bfcache/disk cache). Pragma is the HTTP/1.0 spelling, harmless and still
	// honoured by some intermediaries.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, tokenCreateResponse{
		ID:        tok.ID,
		Name:      tok.Name,
		Token:     authn.EncodeToken(secret),
		ExpiresAt: tok.ExpiresAt,
	})
}

// resolveInheritedOwner is the mint-time half of the owner-inheritance fix:
// when a NEW token is minted by a SubjectToken (i.e. an existing token is
// being used to create another token), parentID is that existing token's own
// id (subject.ID) and fallback is ownerFor's pre-inheritance answer -- the
// same parentID, since that is what ownerFor returns for SubjectToken. This
// looks the parent token up (via the same admin-scale ListTokens tokens:manage
// callers already pay for through GET /api/v1/tokens -- no new store query)
// and, if found with a non-empty Owner, returns THAT owner instead: the new
// token is attributed directly to whatever the parent was ultimately
// attributed to (a users.id UUID, for a chain rooted at a local user), not to
// the parent's own id one more link down. This collapses an arbitrarily deep
// token-mints-token chain to depth 1, so disabling the root user immediately
// invalidates every token minted anywhere in that chain, not just the
// immediate child -- see authn.checkOwnerDisabled's doc comment for what
// happens without this (a parent-token-id owner is a real UUID that reaches
// GetUserByID, gets ErrNotFound, and is wrongly allowed).
//
// Falls back to fallback whenever the parent cannot be attributed with
// confidence: ListTokens itself failing, no row matching parentID (should
// not happen -- the parent token that just authenticated this very request
// must exist -- but a fake/incomplete TokenAdmin or a race is not worth
// failing the mint over), or a parent whose own Owner is empty. Minting a
// token must never fail over attribution bookkeeping.
func (s *Server) resolveInheritedOwner(ctx context.Context, parentID, fallback string) string {
	tokens, err := s.tokens.ListTokens(ctx)
	if err != nil {
		slog.Error("resolve inherited token owner: list tokens failed", "error", err, "parent_token_id", parentID) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return fallback
	}
	for i := range tokens {
		if tokens[i].ID == parentID {
			if tokens[i].Owner != "" {
				return tokens[i].Owner
			}
			break
		}
	}
	return fallback
}

func (s *Server) handleTokensDelete(w http.ResponseWriter, r *http.Request) {
	if s.tokensUnavailable(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.tokens.RevokeToken(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "not found", "no active token with that id")
			return
		}
		slog.Error("revoke token failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "tokens unavailable", "failed to revoke token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
