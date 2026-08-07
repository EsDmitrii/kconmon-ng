// Package authn resolves an HTTP request into an authz.Subject. session.go
// (Task 13) is storage only; this file and its siblings (password.go,
// local.go, header.go, token.go) are the four authenticator implementations
// SECURITY.md §10.1 requires: anonymous (fixed subject), local (argon2id
// password + session cookie), header (trusted-proxy headers), and PAT
// (bearer token, composed on top of any of the other three).
package authn

import (
	"errors"
	"net/http"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// Authenticator turns a request into a Subject. One implementation per mode,
// plus TokenAuthenticator (token.go), which composes another Authenticator
// rather than being a fifth mode.
//
// A nil Subject with a nil error is IMPOSSIBLE: every implementation below
// returns either a Subject or one of the errors declared here (or, for a
// backend failure such as a database error, a wrapped non-sentinel error) —
// never both zero values. That is what lets a caller treat "err == nil" as
// the entire authenticated/unauthenticated decision, with no risk of
// silently treating an unauthenticated request as authenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (authz.Subject, error)
	Mode() string
}

// Sentinel errors every Authenticator implementation returns from
// Authenticate. A caller distinguishes them with errors.Is; the exact
// wording is not a stable API.
var (
	// ErrNoCredentials means the request carried nothing this authenticator
	// can act on (no cookie, no trusted peer, no bearer token in its
	// namespace) -> 401, prompts the login flow. It is also what a stale or
	// otherwise-recoverable situation (unknown session id, deleted user
	// behind a live session) degrades to, deliberately: those should
	// re-prompt login, not read as an attack.
	ErrNoCredentials = errors.New("no credentials")
	// ErrInvalid means credentials were presented and are wrong (bad
	// password, unknown/revoked/expired token). For tokens specifically,
	// unknown/revoked/expired are all reported as this one error,
	// indistinguishably -- see token.go.
	ErrInvalid = errors.New("invalid credentials")
	// ErrExpired means a credential was valid but has expired in a way its
	// own authenticator can still detect and distinguish from ErrInvalid
	// (for example an OIDC access token past its lifetime, ahead of a
	// refresh). Session expiry and token expiry both collapse into a
	// different sentinel by design -- see session.go's loadFresh (expired
	// session = clean miss = ErrNoCredentials from local.go) and token.go
	// (expired token = ErrInvalid, indistinguishable from unknown/revoked).
	// No authenticator in this task returns ErrExpired; it is declared here,
	// as part of the shared taxonomy, for a later authenticator (OIDC) that
	// can tell "expired" apart from "never valid".
	ErrExpired = errors.New("credentials expired")
	// ErrDisabled means the credentials resolved to a real, otherwise-valid
	// account that is administratively disabled.
	ErrDisabled = errors.New("account disabled")
)

// anonymousAuthenticator implements NewAnonymous: see its doc comment.
type anonymousAuthenticator struct {
	subject authz.Subject
}

// NewAnonymous returns an Authenticator that yields the same fixed Subject
// for every request, regardless of what the request carries. This is what
// keeps the M1/M2 default working under auth.mode=anonymous: it is a real
// Subject that goes through the real authorize middleware like any other,
// not a bypass of it.
func NewAnonymous(role string) Authenticator {
	return &anonymousAuthenticator{
		subject: authz.Subject{
			Kind:  authz.SubjectAnonymous,
			ID:    "anonymous",
			Roles: []string{role},
		},
	}
}

func (a *anonymousAuthenticator) Authenticate(_ *http.Request) (authz.Subject, error) {
	return a.subject, nil
}

func (a *anonymousAuthenticator) Mode() string { return "anonymous" }
