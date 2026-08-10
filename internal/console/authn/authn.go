// Package authn resolves an HTTP request into an authz.Subject.
package authn

import (
	"errors"
	"net/http"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// Authenticator turns a request into a Subject; one implementation per mode, plus
// TokenAuthenticator (token.go).
type Authenticator interface {
	Authenticate(r *http.Request) (authz.Subject, error)
	Mode() string
}

// Sentinel errors every Authenticator implementation returns from
// Authenticate. A caller distinguishes them with errors.Is; the exact
// wording is not a stable API.
var (
	// ErrNoCredentials means the request carried nothing this authenticator can act on (no cookie, no
	// trusted peer, no bearer token in its namespace) -> 401; it is also what a stale or
	// otherwise-recoverable situation (unknown session id, deleted user behind a live session)
	// degrades.
	ErrNoCredentials = errors.New("no credentials")
	// ErrInvalid means credentials were presented and are wrong (bad password, unknown/revoked/expired
	// token).
	ErrInvalid = errors.New("invalid credentials")
	// ErrExpired means a credential was valid but has expired in a way its own authenticator can still
	// detect and distinguish from ErrInvalid (for example an OIDC access token past its lifetime,
	// ahead of a refresh); no authenticator in this task returns ErrExpired.
	ErrExpired = errors.New("credentials expired")
	// ErrDisabled means the credentials resolved to a real, otherwise-valid
	// account that is administratively disabled.
	ErrDisabled = errors.New("account disabled")
)

// anonymousAuthenticator implements NewAnonymous: see its doc comment.
type anonymousAuthenticator struct {
	subject authz.Subject
}

// NewAnonymous returns an Authenticator that yields the same fixed Subject for every request.
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
