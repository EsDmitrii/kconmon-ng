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
type TokenAdmin interface {
	CreateToken(ctx context.Context, name string, hash []byte, owner string, expiresAt *time.Time) (store.Token, error)
	ListTokens(ctx context.Context) ([]store.Token, error)
	// GetTokenByID backs resolveInheritedOwner and the DELETE state read; it is not a route.
	GetTokenByID(ctx context.Context, id string) (store.Token, error)
	RevokeToken(ctx context.Context, id string) error
	// PurgeToken hard-deletes a row and is only ever called for a token already read as
	// revoked or expired -- see handleTokensDelete.
	PurgeToken(ctx context.Context, id string) error
}

// tokenSecretBytes mirrors authn.EncodeToken's own 32-byte contract (authn/token.go's
// tokenSecretBytes).
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

// tokenResponse is GET /api/v1/tokens's per-row shape; it structurally cannot carry a hash or a
// secret.
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

// tokenCreateResponse is POST /api/v1/tokens's body: the ONLY place the raw wire-form token
// ("kcm_...") is ever handed back.
type tokenCreateResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Token     string     `json:"token"` //nolint:gosec // G117: the one-time wire form of a token this handler itself just minted, not a hardcoded credential
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ownerFor derives api_tokens.owner (migration 00002: "creator subject id (users.id UUID for local
// users, token id for token-created tokens) or 'system'") from the creating subject alone;
// DisplayName is NEVER used here, unlike the pre-fix version of this function.
func ownerFor(subject authz.Subject) string { //nolint:gocritic // Subject is a value type by design
	if (subject.Kind == authz.SubjectUser || subject.Kind == authz.SubjectToken) && subject.ID != "" {
		return subject.ID
	}
	return "system"
}

// handleTokensCreate mints a new API token; MUST use authn.HashTokenSecret + authn.EncodeToken.
func (s *Server) handleTokensCreate(w http.ResponseWriter, r *http.Request) {
	if s.tokensUnavailable(w) {
		return
	}
	var req tokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", `body must be JSON with a non-empty "name"`)
		return
	}
	// An expiry already in the past mints a credential that can never authenticate, so the
	// only thing such a request produces is a leaked secret; refuse before generating one.
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid token",
			"token: expiry must be in the future")
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

	// The only response in the API that carries a plaintext secret.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, tokenCreateResponse{
		ID:        tok.ID,
		Name:      tok.Name,
		Token:     authn.EncodeToken(secret),
		ExpiresAt: tok.ExpiresAt,
	})
}

// resolveInheritedOwner is the mint-time half of the owner-inheritance fix; falls back to fallback
// whenever the parent cannot be attributed with confidence.
func (s *Server) resolveInheritedOwner(ctx context.Context, parentID, fallback string) string {
	parent, err := s.tokens.GetTokenByID(ctx, parentID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		slog.Warn("resolve inherited token owner: parent token not found", "parent_token_id", parentID) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return fallback
	case err != nil:
		slog.Error("resolve inherited token owner: get token by id failed", "error", err, "parent_token_id", parentID) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return fallback
	case parent.Owner != "":
		return parent.Owner
	default:
		return fallback
	}
}

// tokenIsSpent reports whether a token can no longer authenticate anything -- revoked outright,
// or past an expiry it carries.
func tokenIsSpent(t *store.Token, now time.Time) bool {
	return t.RevokedAt != nil || (t.ExpiresAt != nil && !t.ExpiresAt.After(now))
}

// handleTokensDelete reads the token's state and acts on it: DELETE on an ACTIVE token revokes,
// which is what it has always meant; DELETE on one already revoked or expired purges the row, so
// the list stops growing forever.
func (s *Server) handleTokensDelete(w http.ResponseWriter, r *http.Request) {
	if s.tokensUnavailable(w) {
		return
	}
	id := chi.URLParam(r, "id")

	tok, err := s.tokens.GetTokenByID(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not found", "no token with that id")
		return
	case err != nil:
		slog.Error("get token failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "tokens unavailable", "failed to read token")
		return
	}

	if tokenIsSpent(&tok, time.Now()) {
		if err := s.tokens.PurgeToken(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeProblem(w, http.StatusNotFound, "not found", "no token with that id")
				return
			}
			slog.Error("purge token failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "tokens unavailable", "failed to delete token")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

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
