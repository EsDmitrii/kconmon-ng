package authn

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// tokenPrefix and tokenSecretBytes together pin the wire format.
const (
	tokenPrefix      = "kcm_"
	tokenSecretBytes = 32
)

// bearerScheme is the only Authorization scheme this authenticator recognizes.
const bearerScheme = "Bearer"

// touchDebounce is the "at most once per minute per token" ceiling
// store.TokenStore.TouchTokenLastUsed's doc comment requires the authn layer to enforce.
const touchDebounce = 60 * time.Second

// touchTimeout bounds how long a detached, asynchronous TouchTokenLastUsed write is allowed to run
// for; it exists only to stop a stuck store connection from leaking goroutines forever.
const touchTimeout = 5 * time.Second

// TokenStore is the subset of store.TokenStore the token authenticator
// needs.
type TokenStore interface {
	// GetTokenByHash returns store.ErrNotFound for an unknown hash; like store.TokenStore, it does not
	// collapse revoked/expired/unknown into one outcome.
	GetTokenByHash(ctx context.Context, hash []byte) (store.Token, error)
	// TouchTokenLastUsed returns store.ErrNotFound when id does not name a
	// token. Never called more than once per touchDebounce per token id --
	// see the touch method.
	TouchTokenLastUsed(ctx context.Context, id string) error
}

// tokenAuthenticator implements NewTokenFallback: see its doc comment.
type tokenAuthenticator struct {
	tokens TokenStore
	inner  Authenticator
	users  UserStore // nil unless WithOwnerDisabledCheck was passed to NewTokenFallback

	touchMu   sync.Mutex
	touchedAt map[string]time.Time
}

// TokenFallbackOption configures optional behavior of NewTokenFallback; the only implementation
// today is WithOwnerDisabledCheck.
type TokenFallbackOption func(*tokenAuthenticator)

// WithOwnerDisabledCheck closes the gap store.TokenStore's doc comment (store/auth.go) records:
// without this option.
func WithOwnerDisabledCheck(users UserStore) TokenFallbackOption {
	return func(t *tokenAuthenticator) { t.users = users }
}

// NewTokenFallback returns an Authenticator that wraps inner; this is what makes SECURITY.md
// §10.1's "API tokens (PATs) work in every mode" true by composition.
func NewTokenFallback(tokens TokenStore, inner Authenticator, opts ...TokenFallbackOption) Authenticator {
	t := &tokenAuthenticator{
		tokens:    tokens,
		inner:     inner,
		touchedAt: make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Mode reports inner's mode: a wrapped PAT capability is not a fifth
// configured auth.mode the way anonymous/local/header/oidc are, it is a
// capability layered on top of whichever of those four is active.
func (t *tokenAuthenticator) Mode() string { return t.inner.Mode() }

func (t *tokenAuthenticator) Authenticate(r *http.Request) (authz.Subject, error) {
	subject, err := t.authenticateToken(r)
	switch {
	case err == nil:
		return subject, nil
	case errors.Is(err, ErrNoCredentials):
		// Nothing shaped like a kcm_ token was presented (or the
		// Authorization header carries some other scheme/value entirely) --
		// this is not this authenticator's request to answer.
		return t.inner.Authenticate(r)
	default:
		// A well-formed kcm_ token that failed verification (ErrInvalid) is answered here.
		return authz.Subject{}, err
	}
}

// authenticateToken is the token-only half of Authenticate.
func (t *tokenAuthenticator) authenticateToken(r *http.Request) (authz.Subject, error) {
	token, ok := splitBearer(r.Header.Get("Authorization"))
	if !ok {
		return authz.Subject{}, ErrNoCredentials
	}

	secret, ok := decodeToken(token)
	if !ok {
		// Right scheme, wrong shape (missing kcm_ prefix, wrong length, not
		// valid base64url): still "no PAT here", not an attack worth an
		// ErrInvalid -- the wrapped mode still gets its chance.
		return authz.Subject{}, ErrNoCredentials
	}

	hash, err := hex.DecodeString(HashTokenSecret(secret))
	if err != nil {
		// HashTokenSecret always returns hex.EncodeToString of a fixed-size sha256.Sum256 output.
		return authz.Subject{}, fmt.Errorf("authn: token: decode hash: %w", err)
	}
	tok, err := t.tokens.GetTokenByHash(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authz.Subject{}, ErrInvalid
		}
		return authz.Subject{}, fmt.Errorf("authn: token: get token by hash: %w", err)
	}

	now := time.Now()
	if tok.RevokedAt != nil || (tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt)) {
		// Unknown (above), revoked, and expired all collapse to the same ErrInvalid.
		return authz.Subject{}, ErrInvalid
	}

	if err := t.checkOwnerDisabled(r.Context(), tok.ID, tok.Owner); err != nil {
		return authz.Subject{}, err
	}

	t.touch(r.Context(), tok.ID)

	return authz.Subject{
		Kind:        authz.SubjectToken,
		ID:          tok.ID,
		DisplayName: tok.Name,
	}, nil
}

// checkOwnerDisabled is authenticateToken's owner-disabled gate: a no-op unless
// WithOwnerDisabledCheck supplied a UserStore.
func (t *tokenAuthenticator) checkOwnerDisabled(ctx context.Context, tokenID, owner string) error {
	if t.users == nil {
		return nil
	}
	if _, err := uuid.Parse(owner); err != nil {
		// Deliberate: see the doc comment above -- a non-UUID owner ("system", empty/legacy data) names
		// no possible users row.
		return nil //nolint:nilerr // intentional: a non-UUID owner means "allow", not "no error occurred"
	}
	user, err := t.users.GetUserByID(ctx, owner)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deliberate: see the doc comment above -- an owner UUID with no users row (header/OIDC-created
			// token, or a pre-inheritance token-minted-by-token row) means "not this store's call to make".
			return nil //nolint:nilerr // intentional: ErrNotFound here means "allow", not "no error occurred"
		}
		return fmt.Errorf("authn: token: check owner disabled: %w", err)
	}
	if user.Disabled {
		slog.Warn("authn: rejected token of a disabled user", "token_id", tokenID, "owner", owner) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return ErrDisabled
	}
	return nil
}

// touch calls TouchTokenLastUsed for id at most once per touchDebounce, guarded by touchMu; that
// goroutine's context is deliberately NOT r.Context (context.WithoutCancel strips the request's own
// cancellation).
func (t *tokenAuthenticator) touch(ctx context.Context, id string) {
	now := time.Now()

	t.touchMu.Lock()
	last, seen := t.touchedAt[id]
	if seen && now.Sub(last) < touchDebounce {
		t.touchMu.Unlock()
		return
	}
	t.touchedAt[id] = now
	t.touchMu.Unlock()

	touchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), touchTimeout)
	go func() {
		defer cancel()
		if err := t.tokens.TouchTokenLastUsed(touchCtx, id); err != nil {
			slog.Warn("authn: touch token last used failed", "token_id", id, "error", err)
		}
	}()
}

// splitBearer reports whether auth (the raw Authorization header value) is a well-formed "<scheme>
// <token>" credential using the Bearer scheme; the scheme token is matched case-insensitively
// (strings.EqualFold) per RFC 7235 §2.1.
func splitBearer(auth string) (token string, ok bool) {
	scheme, rest, found := strings.Cut(auth, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	return rest, true
}

// decodeToken reports whether raw has the exact kcm_<43-char base64url>
// shape EncodeToken produces and, if so, returns its decoded 32-byte
// secret -- decodeToken is EncodeToken's inverse.
func decodeToken(raw string) ([]byte, bool) {
	if !strings.HasPrefix(raw, tokenPrefix) {
		return nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, tokenPrefix))
	if err != nil || len(secret) != tokenSecretBytes {
		return nil, false
	}
	return secret, true
}

// EncodeToken renders raw (the token's random secret bytes, tokenSecretBytes = 32 of them) into the
// wire format a caller presents in an Authorization header.
func EncodeToken(raw []byte) string {
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// HashTokenSecret is the single source of truth for turning a token's raw 32-byte secret into the
// value stored in and looked up against api_tokens.token_hash.
func HashTokenSecret(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
