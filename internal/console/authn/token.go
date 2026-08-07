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

// tokenPrefix and tokenSecretBytes together pin the wire format: "kcm_"
// followed by a base64url (RawURLEncoding, no padding) encoding of
// tokenSecretBytes*8 = 256 random bits, which is exactly 43 characters --
// the same "32 raw bytes -> 43-char base64url id" convention session.go's
// sessionIDBytes already uses.
const (
	tokenPrefix      = "kcm_"
	tokenSecretBytes = 32
)

// bearerScheme is the only Authorization scheme this authenticator
// recognizes; anything else (including no Authorization header at all)
// means "no PAT here". The scheme token itself is matched case-insensitively
// by splitBearer below, per RFC 7235 §2.1 ("auth-scheme ... is case
// insensitive"), so "bearer kcm_..." and "BEARER kcm_..." are recognized
// exactly like "Bearer kcm_..." -- only the literal spelling here is
// canonical, used for comparison, not as a strict prefix.
const bearerScheme = "Bearer"

// touchDebounce is the "at most once per minute per token" ceiling
// store.TokenStore.TouchTokenLastUsed's doc comment requires the authn layer
// to enforce, so that a hot token does not serialize every authenticated
// request behind the store's write connection pool.
const touchDebounce = 60 * time.Second

// touchTimeout bounds how long a detached, asynchronous
// TouchTokenLastUsed write is allowed to run for. It exists only to stop a
// stuck store connection from leaking goroutines forever -- it is not part
// of the request's own deadline (the touch is deliberately detached from
// that, see touch's doc comment) and has no bearing on whether the request
// itself succeeds.
const touchTimeout = 5 * time.Second

// TokenStore is the subset of store.TokenStore the token authenticator
// needs.
type TokenStore interface {
	// GetTokenByHash returns store.ErrNotFound for an unknown hash. Like
	// store.TokenStore, it does not collapse revoked/expired/unknown into
	// one outcome -- that collapsing happens here, in authenticateToken, so
	// the response to an untrusted caller cannot be used as a
	// token-enumeration oracle while the audit log can still record which
	// case actually happened.
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

// TokenFallbackOption configures optional behavior of NewTokenFallback. The
// only implementation today is WithOwnerDisabledCheck; the type exists as a
// variadic functional-option seam so a future addition does not need to
// break NewTokenFallback's signature a second time.
type TokenFallbackOption func(*tokenAuthenticator)

// WithOwnerDisabledCheck closes the gap store.TokenStore's doc comment
// (store/auth.go) records: without this option, a disabled user's still
// valid, unexpired tokens keep authenticating forever, because token
// verification (GetTokenByHash) never joins users at all. With it,
// authenticateToken performs one extra lookup -- GetUserByID(tok.Owner) --
// after the token itself is confirmed live, and fails the request with
// ErrDisabled if that owner is currently Disabled.
//
// This is opt-in, not the default, because NewTokenFallback is called in
// every auth.mode (SECURITY.md §10.1), including database.mode=disabled,
// where there is no UserStore to check against at all -- cmd/console/main.go
// only passes this option inside its `db != nil` branch.
func WithOwnerDisabledCheck(users UserStore) TokenFallbackOption {
	return func(t *tokenAuthenticator) { t.users = users }
}

// NewTokenFallback returns an Authenticator that wraps inner: a request
// carrying `Authorization: Bearer kcm_...` is resolved as a token subject on
// its own, without ever consulting inner; every other request (no
// Authorization header, a different scheme, or a value that does not even
// have the right shape to be a kcm_ token) is passed to inner.Authenticate
// untouched and inner's result is returned as-is. This is what makes
// SECURITY.md §10.1's "API tokens (PATs) work in every mode" true by
// composition -- one Authenticator wraps whichever of anonymous/local/header
// (or, later, oidc) is configured -- rather than four call sites each
// special-casing a bearer token by hand.
//
// opts is currently just WithOwnerDisabledCheck; existing call sites that
// pass none stay source-compatible.
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
		// A well-formed kcm_ token that failed verification (ErrInvalid) is
		// answered here, directly -- it must NOT fall through to inner. A
		// request that explicitly presents a bad PAT should not get a
		// second, silent chance via a cookie or trusted-proxy header.
		return authz.Subject{}, err
	}
}

// authenticateToken is the token-only half of Authenticate. It returns
// ErrNoCredentials for anything that isn't recognizably a kcm_ bearer token
// (so Authenticate knows to fall through to inner), and ErrInvalid --
// uniformly, indistinguishably -- for a token that IS well-formed but
// unknown, revoked, or expired.
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
		// HashTokenSecret always returns hex.EncodeToString of a fixed-size
		// sha256.Sum256 output, which hex.DecodeString can always decode
		// back -- this branch is unreachable in practice, but a decode
		// failure must still surface as a real error, never a panic or a
		// silently-empty hash reaching GetTokenByHash.
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
		// Unknown (above), revoked, and expired all collapse to the same
		// ErrInvalid -- the response must not be a token-enumeration oracle.
		// The audit log records the real distinction; this return value
		// deliberately does not.
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

// checkOwnerDisabled is authenticateToken's owner-disabled gate: a no-op
// unless WithOwnerDisabledCheck supplied a UserStore, otherwise the sole
// place that decision is made. It costs one extra PK lookup per
// authenticated token request -- the same per-request re-query cost
// local.go's Authenticate already pays against GetUserByUsername on every
// call, for the identical reason: only a live re-query catches a disable
// flip on the very next request, not just at token-creation time.
//
// owner is api_tokens.owner (ownerFor, httpapi/tokens.go): a users.id UUID
// for a token minted by a local-mode user (directly, or -- since
// handleTokensCreate's mint-time owner inheritance, httpapi/tokens.go -- via
// any chain of token-minted tokens rooted at that user), the literal
// "system", or a UUID that names no users row at all for a token minted by a
// header/OIDC subject (SubjectUser with no users row) or, for tokens created
// before that mint-time inheritance existed, a token minted by another
// token. Token ids ARE canonical UUIDs -- store.formatUUID mints every id,
// both users.id and api_tokens.id, the exact same way -- so there is no
// "not a UUID" case among SubjectUser/SubjectToken owners; only "system" and
// empty/legacy data fail uuid.Parse. Only the "parses as a UUID AND names a
// real users row" case can answer the question this check exists to ask,
// so:
//   - owner does not even parse as a UUID ("system", empty/legacy data) --
//     skip the lookup entirely, allow. There is no users row this string
//     could possibly name.
//   - owner parses as a UUID but GetUserByID returns store.ErrNotFound --
//     allow. This is the header/OIDC-created-token case (that subject's
//     disable state lives upstream, not in this database, and
//     store.UserStore's own doc comment on GetUserByID never claims to
//     answer for it) and, for rows written before the mint-time owner
//     inheritance above existed, a pre-inheritance token-minted-by-token row
//     whose owner is a parent token's own id: a real UUID that simply names
//     no users row, so it reaches this exact branch and is allowed -- not
//     skipped at the uuid.Parse step above, and not rejected either.
//   - owner parses as a UUID and names a Disabled user -- ErrDisabled.
//   - owner parses as a UUID and names an enabled user -- allow.
//   - any other store error -- propagated as-is (fail closed on infra
//     errors: this must NOT collapse into ErrInvalid, which would make a
//     transient store outage indistinguishable from "this token was never
//     valid" to an operator reading logs, the same enumeration-oracle
//     concern the revoked/expired/unknown collapse above is deliberately
//     narrower than).
func (t *tokenAuthenticator) checkOwnerDisabled(ctx context.Context, tokenID, owner string) error {
	if t.users == nil {
		return nil
	}
	if _, err := uuid.Parse(owner); err != nil {
		// Deliberate: see the doc comment above -- a non-UUID owner ("system",
		// empty/legacy data) names no possible users row, so there is nothing
		// to check; this is "allow", not a parse failure to surface. A token
		// id is NOT an example of this case: token ids are UUIDs too, so a
		// token-minted-token's owner parses here just fine and falls through
		// to the real GetUserByID call below (its ErrNotFound branch is what
		// handles it).
		return nil //nolint:nilerr // intentional: a non-UUID owner means "allow", not "no error occurred"
	}
	user, err := t.users.GetUserByID(ctx, owner)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deliberate: see the doc comment above -- an owner UUID with no
			// users row (header/OIDC-created token, or a pre-inheritance
			// token-minted-by-token row) means "not this store's call to
			// make", not an error.
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

// touch calls TouchTokenLastUsed for id at most once per touchDebounce,
// guarded by touchMu, on a detached goroutine.
//
// store.TokenStore's own doc comment (auth.go) forbids ever calling
// TouchTokenLastUsed synchronously on the request path -- a write per
// authenticated request would serialize every token-authenticated request
// behind the store's write connection pool -- so this dispatches the actual
// write on its own goroutine and returns immediately. That goroutine's
// context is deliberately NOT r.Context() (context.WithoutCancel strips the
// request's own cancellation): the touch must survive the response already
// being written and the request's context being canceled a moment later, or
// every touch would race the response and usually lose. touchTimeout is
// layered on top of that detached context purely so a stuck store
// connection cannot leak goroutines forever -- it bounds the write's own
// lifetime, not the request's.
//
// touchedAt[id] records the last ATTEMPT, and it is stamped inside the same
// critical section that decides the debounce, before the write is
// dispatched. Recording only successes would re-open the write-per-request
// storm the debounce exists to prevent: with a broken write path (pool
// exhaustion, read-only failover) no attempt would ever be recorded, so
// every authenticated request would dispatch another failing write — a
// self-reinforcing failure. Stamping pre-dispatch also closes the TOCTOU
// where two concurrent first-requests both pass the check and both fire.
// The cost — last-used accuracy lags up to touchDebounce after a failed
// write — is the correct side to err on: bookkeeping is not part of the
// trust decision, and a failed touch is logged, never surfaced.
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

// splitBearer reports whether auth (the raw Authorization header value) is a
// well-formed "<scheme> <token>" credential using the Bearer scheme, and if
// so returns the token substring. The scheme token is matched
// case-insensitively (strings.EqualFold) per RFC 7235 §2.1, so "bearer",
// "Bearer", and "BEARER" are all accepted identically -- only the literal
// spelling of bearerScheme is canonical. Exactly one space is tolerated
// between the scheme and the token (RFC 7235's credentials ABNF is
// "auth-scheme [ 1*SP ( token68 / ... ) ]", but this authenticator only ever
// issues single-space Authorization headers itself, so a second space or a
// tab is treated as "no PAT here" -- ErrNoCredentials, not ErrInvalid --
// exactly like any other scheme/shape mismatch).
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

// EncodeToken renders raw (the token's random secret bytes, tokenSecretBytes
// = 32 of them) into the wire format a caller presents in an Authorization
// header: "kcm_" followed by a base64url (RawURLEncoding, no padding)
// encoding of those 32 bytes, which is exactly 43 characters -- the same
// "32 raw bytes -> 43-char base64url id" convention session.go's
// sessionIDBytes already uses. decodeToken is its inverse.
func EncodeToken(raw []byte) string {
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// HashTokenSecret is the single source of truth for turning a token's raw
// 32-byte secret into the value stored in and looked up against
// api_tokens.token_hash: SHA-256 of the RAW secret bytes (Decision 11,
// migration 00002's token_hash column comment: "SHA-256 of 256 random
// bits") -- of the secret itself, never of its base64url text encoding or
// the "kcm_" prefix -- hex-encoded, since api_tokens.token_hash is a BYTEA
// and store.TokenStore's methods take/return raw []byte, not a text column,
// so a caller that needs those raw bytes decodes this string with
// hex.DecodeString (see authenticateToken below).
//
// This is exported specifically so a token-issuing admin API (Task 17) can
// import and call it directly instead of re-deriving the same
// sha256.Sum256(secret) computation independently: authenticateToken below
// calls this exact function too, so the two can never silently diverge. A
// creation path that computed its own hash differently (wrong byte range,
// wrong algorithm, wrong encoding) would silently invalidate every token it
// issues -- GetTokenByHash would simply never find a match -- without either
// side ever producing an error that points at the real cause.
//
// Deliberately NOT compared with crypto/subtle.ConstantTimeCompare against a
// second, stored hash here: store.TokenStore.GetTokenByHash (Task 12,
// frozen) is a single indexed lookup -- WHERE token_hash = $1 -- and its
// query comment (queries/auth.sql) records exactly why it never returns the
// row's token_hash back out: "the caller already knows the hash it looked
// up by, and there is never a reason to hand a hash value back across this
// boundary." That leaves no two independent hash byte-slices in this
// package for ConstantTimeCompare to operate on -- the match is delegated
// entirely to Postgres's own unique-index equality check on a fixed-size
// SHA-256 digest, which never performs a variable-length secret compare
// against attacker-influenced input the way a naive Go bytes.Equal on a raw
// token would. ConstantTimeCompare's actual job in this package is in
// password.go, comparing two argon2id digests computed from a
// caller-supplied plaintext -- a materially different shape of comparison.
func HashTokenSecret(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
