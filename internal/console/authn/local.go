package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// UserStore is the subset of store.UserStore the authn package needs; GetUserByUsername is
// re-queried on EVERY Authenticate call, not just at login.
type UserStore interface {
	// GetUserByID returns store.ErrNotFound when id does not name a user.
	// Used only by WithOwnerDisabledCheck (token.go) -- the local
	// authenticator above never calls this, it re-resolves by username.
	GetUserByID(ctx context.Context, id string) (store.User, error)
	// GetUserByUsername returns store.ErrNotFound when no user has that
	// username.
	GetUserByUsername(ctx context.Context, username string) (store.User, error)
}

// localAuthenticator implements NewLocal: see its doc comment.
type localAuthenticator struct {
	users      UserStore
	sessions   *SessionStore
	cookieName string
}

// NewLocal returns an Authenticator for auth.mode=local; it does NOT verify a password -- that only
// happens once.
func NewLocal(users UserStore, sessions *SessionStore, cookieName string) Authenticator {
	return &localAuthenticator{users: users, sessions: sessions, cookieName: cookieName}
}

func (l *localAuthenticator) Mode() string { return "local" }

func (l *localAuthenticator) Authenticate(r *http.Request) (authz.Subject, error) {
	cookie, err := r.Cookie(l.cookieName)
	if err != nil || cookie.Value == "" {
		return authz.Subject{}, ErrNoCredentials
	}

	sess, ok, err := l.sessions.Get(r.Context(), cookie.Value)
	if err != nil {
		return authz.Subject{}, fmt.Errorf("authn: local: get session: %w", err)
	}
	if !ok {
		// An id that is absent, corrupted, or already past its own ExpiresAt (SessionStore.Get collapses
		// all three into ok=false) is a stale cookie.
		return authz.Subject{}, ErrNoCredentials
	}

	user, err := l.users.GetUserByUsername(r.Context(), sess.Username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The account behind an otherwise-live session was deleted.
			// Same "re-prompt login, don't treat as an attack" reasoning as
			// an unknown session id above.
			return authz.Subject{}, ErrNoCredentials
		}
		return authz.Subject{}, fmt.Errorf("authn: local: get user: %w", err)
	}
	if user.Disabled {
		return authz.Subject{}, ErrDisabled
	}

	return authz.Subject{
		Kind:        authz.SubjectUser,
		ID:          user.ID,
		DisplayName: user.DisplayName,
		Groups:      sess.Groups,
	}, nil
}
