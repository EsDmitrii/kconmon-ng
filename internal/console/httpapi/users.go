package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// UserAdmin is the local user administration seam; *store.DB satisfies it.
type UserAdmin interface {
	ListUsers(ctx context.Context) ([]store.User, error)
	GetUserByID(ctx context.Context, id string) (store.User, error)
	GetUserByUsername(ctx context.Context, username string) (store.User, error)
	CreateUserWithRole(ctx context.Context, username, passwordHash, displayName, role string) (store.User, error)
	UpdateUserPassword(ctx context.Context, id, passwordHash string) error
	// UpdateUserGuarded writes change in one transaction, running guard under a lock every such
	// change shares; see store.DB.UpdateUserGuarded.
	UpdateUserGuarded(ctx context.Context, id string, change store.UserChange, guard func(context.Context) error) error
	// DeleteUserGuarded removes the user and their direct bindings under the same lock; see
	// store.DB.DeleteUserGuarded.
	DeleteUserGuarded(ctx context.Context, id string, guard func(context.Context) error) error
}

var _ UserAdmin = (*store.DB)(nil)

// badBody answers 400 for a body that failed to decode (naming an unknown field) or decoded but lacks
// what the route needs (the shape hint alone). unknownFieldDetail dereferences its error, so it must
// never see a nil one.
func badBody(w http.ResponseWriter, err error, shape string) {
	detail := shape
	if err != nil {
		detail = unknownFieldDetail(err, shape)
	}
	writeProblem(w, http.StatusBadRequest, "invalid request", detail)
}

// passwordRateLimitDetail is the 429 detail for POST /api/v1/auth/password.
const passwordRateLimitDetail = "too many password attempts; retry shortly " +
	"(limit: console.rateLimit.loginPerMinute per username per minute, shared with login)"

const (
	minPasswordRunes = 12
	// maxPasswordBytes bounds what a request body can make the server feed argon2id.
	maxPasswordBytes = 256
	maxUsernameLen   = 64
)

// usernamePattern keeps names printable in the audit log, the user menu and a session key.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,` + strconv.Itoa(maxUsernameLen) + `}$`)

type userResponse struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	Roles       []string  `json:"roles"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"createdAt"`
}

type userCreateRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
	Role        string `json:"role"`
}

type userPatchRequest struct {
	Disabled *bool   `json:"disabled"`
	Role     *string `json:"role"`
}

type passwordSetRequest struct {
	Password string `json:"password"`
}

type passwordChangeRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func validatePassword(p string) string {
	if utf8.RuneCountInString(p) < minPasswordRunes {
		return fmt.Sprintf("password must be at least %d characters", minPasswordRunes)
	}
	if len(p) > maxPasswordBytes {
		return fmt.Sprintf("password must be at most %d bytes", maxPasswordBytes)
	}
	return ""
}

// maxDisplayNameRunes bounds a display name, which is copied into every session and /auth/me.
const maxDisplayNameRunes = 128

func validDisplayName(name string) bool {
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxDisplayNameRunes {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

// usersUnavailable answers for the whole family: user administration exists only in auth.mode=local,
// where the console itself is the identity provider.
func (s *Server) usersUnavailable(w http.ResponseWriter) bool {
	if s.cfg.Auth.Mode != "local" {
		writeProblem(w, http.StatusNotFound, "not found", "user management exists only in auth.mode=local")
		return true
	}
	if s.userAdmin == nil {
		writeProblem(w, http.StatusServiceUnavailable, "user admin not available",
			"local users live in the console database; "+databaseKnob+" to enable /api/v1/users")
		return true
	}
	return false
}

func (s *Server) userView(ctx context.Context, u *store.User) userResponse {
	roles := s.resolveRoles(ctx, authz.Subject{Kind: authz.SubjectUser, ID: u.ID}).Roles
	if roles == nil {
		roles = []string{}
	}
	return userResponse{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Roles: roles,
		Disabled: u.Disabled, CreatedAt: u.CreatedAt}
}

// lastAdminPolicy is the policy the last-admin guard checks against: custom roles read from the
// store, as usersManageHolders does, not s.policy, which can lag a role change made on another replica.
func (s *Server) lastAdminPolicy(ctx context.Context) (*authz.Policy, error) {
	if s.roleAdmin == nil {
		return s.policy, nil
	}
	roles, err := s.roleAdmin.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	return authz.NewPolicy(customRolePermissions(roles)), nil
}

// canManageUsers reports whether an enabled user holds users:manage through the same resolution a
// request of theirs would get. Unlike resolveRoles it returns a role-store error instead of
// narrowing the roles, so the last-admin guard can fail closed.
func (s *Server) canManageUsers(ctx context.Context, policy *authz.Policy, u *store.User) (bool, error) {
	if u.Disabled {
		return false, nil
	}
	subject := authz.Subject{Kind: authz.SubjectUser, ID: u.ID}
	// Local users carry no groups, so the bound roles are all there is.
	var roles []string
	if s.roles != nil {
		bound, err := s.roles.RolesFor(ctx, subject)
		if err != nil {
			return false, err
		}
		roles = bound
	}
	if len(roles) == 0 {
		roles = s.defaultRoles()
	}
	subject.Roles = roles
	return policy.Can(subject, authz.PermUsersManage), nil
}

// wouldLockOut reports whether taking users:manage away from target, by disabling them or by a role
// without it, leaves no enabled user who can manage users.
func (s *Server) wouldLockOut(ctx context.Context, policy *authz.Policy, target *store.User) (bool, error) {
	can, err := s.canManageUsers(ctx, policy, target)
	if err != nil || !can {
		return false, err
	}
	users, err := s.userAdmin.ListUsers(ctx)
	if err != nil {
		return false, err
	}
	for i := range users {
		if users[i].ID == target.ID {
			continue
		}
		other, err := s.canManageUsers(ctx, policy, &users[i])
		if err != nil {
			return false, err
		}
		if other {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	users, err := s.userAdmin.ListUsers(r.Context())
	if err != nil {
		slog.Error("list users failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "users unavailable", "failed to list users")
		return
	}
	slices.SortFunc(users, func(a, b store.User) int { return strings.Compare(a.Username, b.Username) })
	out := make([]userResponse, 0, len(users))
	for i := range users {
		out = append(out, s.userView(r.Context(), &users[i]))
	}
	writeJSON(w, map[string]any{"users": out})
}

func (s *Server) handleUsersCreate(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	var req userCreateRequest
	const bodyShape = `body must be JSON with "username", "password" and "role", and an optional "displayName"`
	if !decodeMutationBody(w, r, &req, bodyShape) {
		return
	}
	if !usernamePattern.MatchString(req.Username) {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			fmt.Sprintf("username must be 1-%d characters of letters, digits and . _ @ -", maxUsernameLen))
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", msg)
		return
	}
	if !validDisplayName(req.DisplayName) {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			fmt.Sprintf("displayName must be at most %d characters, with no control characters", maxDisplayNameRunes))
		return
	}
	if writeUserRoleRefusal(w, s.requireKnownRole(r.Context(), req.Role)) {
		return
	}
	hash, err := authn.HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "user creation failed", "")
		return
	}
	display := req.DisplayName
	if display == "" {
		display = req.Username
	}
	u, err := s.createUserWithRole(r.Context(), req.Username, hash, display, req.Role)
	if writeUserRoleRefusal(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusConflict, "username taken", "a user with this username already exists")
		return
	case err != nil:
		slog.Error("create user failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "user creation failed", "")
		return
	}
	writeJSONStatus(w, http.StatusCreated, s.userView(r.Context(), &u))
}

// writeUserRoleRefusal answers requireKnownRole's refusals the way the users routes document them,
// and reports whether it did.
func writeUserRoleRefusal(w http.ResponseWriter, err error) bool {
	var readErr rbacReadError
	switch {
	case errors.Is(err, errUnknownRole):
		writeProblem(w, http.StatusBadRequest, "invalid request", "role must be a built-in or an existing custom role")
		return true
	case errors.As(err, &readErr):
		slog.Error("read roles failed", "error", readErr.err)
		writeProblem(w, http.StatusBadGateway, "roles unavailable", readErr.detail)
		return true
	}
	return false
}

// guardedUserCreator is a user store that creates a user and its binding under the lock a role
// delete takes, running guard first.
type guardedUserCreator interface {
	CreateUserWithRoleGuarded(ctx context.Context, username, passwordHash, displayName, role string,
		guard func(context.Context) error) (store.User, error)
}

var _ guardedUserCreator = (*store.DB)(nil)

// createUserWithRole re-checks the role under that lock when the store offers it, so a role deleted
// since handleUsersCreate checked it is not bound; the argon2 hash in between is a wide window.
func (s *Server) createUserWithRole(ctx context.Context, username, hash, display, role string) (store.User, error) {
	guarded, ok := s.userAdmin.(guardedUserCreator)
	if !ok {
		return s.userAdmin.CreateUserWithRole(ctx, username, hash, display, role)
	}
	return guarded.CreateUserWithRoleGuarded(ctx, username, hash, display, role, func(ctx context.Context) error {
		return s.requireKnownRole(ctx, role)
	})
}

func (s *Server) handleUsersPatch(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	var req userPatchRequest
	const bodyShape = `body must be JSON with "disabled" (boolean), "role" (string), or both`
	dec := strictJSONDecoder(r.Body)
	if err := dec.Decode(&req); err != nil || (req.Disabled == nil && req.Role == nil) {
		badBody(w, err, bodyShape)
		return
	}
	if refuseTrailingJSON(w, dec) {
		return
	}
	u, ok := s.userOr404(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if req.Role != nil && writeUserRoleRefusal(w, s.requireKnownRole(r.Context(), *req.Role)) {
		return
	}
	// Losing users:manage, by being disabled or by a role without it, must leave someone who has it.
	// The check runs inside the store's guarded transaction, so two admins demoting each other at
	// once cannot both pass it; both fields are written together or not at all.
	disabling := req.Disabled != nil && *req.Disabled
	var guard func(context.Context) error
	if disabling || req.Role != nil {
		guard = func(ctx context.Context) error {
			// Again under the lock a role delete takes: one landing since the check above would
			// otherwise leave a binding to a role that no longer exists.
			if req.Role != nil {
				if err := s.requireKnownRole(ctx, *req.Role); err != nil {
					return err
				}
			}
			policy, err := s.lastAdminPolicy(ctx)
			if err != nil {
				return lastAdminCheckError{err}
			}
			if !disabling && policy.Can(authz.Subject{Kind: authz.SubjectUser, Roles: []string{*req.Role}}, authz.PermUsersManage) {
				return nil
			}
			current, err := s.userAdmin.GetUserByID(ctx, u.ID)
			if err != nil {
				return lastAdminCheckError{err}
			}
			lock, err := s.wouldLockOut(ctx, policy, &current)
			if err != nil {
				return lastAdminCheckError{err}
			}
			if lock {
				return errLastAdmin
			}
			return nil
		}
	}
	// A re-enable must not revive a token minted before the disable, by whoever held the account.
	if req.Disabled != nil && !*req.Disabled && u.Disabled {
		if err := s.revokeOwnedTokens(r.Context(), u.ID); err != nil {
			slog.Error("revoke tokens before re-enable failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "tokens unavailable",
				"failed to revoke the user's API tokens; the user stays disabled")
			return
		}
	}
	err := s.userAdmin.UpdateUserGuarded(r.Context(), u.ID, store.UserChange{Disabled: req.Disabled, Role: req.Role}, guard)
	if writeUserRoleRefusal(w, err) {
		return
	}
	var checkErr lastAdminCheckError
	switch {
	case errors.Is(err, errLastAdmin):
		writeProblem(w, http.StatusConflict, "last administrator",
			"this is the last enabled user who can manage users; grant users:manage to someone else first")
		return
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "user not found", "")
		return
	case errors.As(err, &checkErr):
		slog.Error("last-admin check failed", "error", checkErr.err)
		writeProblem(w, http.StatusBadGateway, "users unavailable", "failed to read users")
		return
	case err != nil:
		slog.Error("update user failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "user update failed", "")
		return
	}
	if disabling {
		// Refused while the owner is disabled anyway, and a re-enable revokes them first.
		if err := s.revokeOwnedTokens(r.Context(), u.ID); err != nil {
			slog.Warn("revoke tokens of a disabled user failed", "error", err)
		}
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	writeJSON(w, s.userView(r.Context(), &u))
}

// handleUsersDelete removes a local user. Their tokens are revoked FIRST and the delete is refused if
// that fails: a token whose owner row is gone passes the owner check. Their sessions need nothing: the
// local authenticator re-reads the user on every request, and a new account reusing the username has
// a different password stamp.
func (s *Server) handleUsersDelete(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	u, ok := s.userOr404(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if err := s.revokeOwnedTokens(r.Context(), u.ID); err != nil {
		slog.Error("revoke tokens before delete failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "tokens unavailable",
			"failed to revoke the user's API tokens; the user was not deleted")
		return
	}
	err := s.userAdmin.DeleteUserGuarded(r.Context(), u.ID, func(ctx context.Context) error {
		policy, err := s.lastAdminPolicy(ctx)
		if err != nil {
			return lastAdminCheckError{err}
		}
		current, err := s.userAdmin.GetUserByID(ctx, u.ID)
		if err != nil {
			return lastAdminCheckError{err}
		}
		lock, err := s.wouldLockOut(ctx, policy, &current)
		if err != nil {
			return lastAdminCheckError{err}
		}
		if lock {
			return errLastAdmin
		}
		return nil
	})
	var checkErr lastAdminCheckError
	switch {
	case errors.Is(err, errLastAdmin):
		writeProblem(w, http.StatusConflict, "last administrator",
			"this is the last enabled user who can manage users; grant users:manage to someone else first")
		return
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "user not found", "")
		return
	case errors.As(err, &checkErr):
		slog.Error("last-admin check failed", "error", checkErr.err)
		writeProblem(w, http.StatusBadGateway, "users unavailable", "failed to read users")
		return
	case err != nil:
		slog.Error("delete user failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "user delete failed", "")
		return
	}
	// A token minted between the revoke above and the delete would otherwise outlive its owner.
	if err := s.revokeOwnedTokens(r.Context(), u.ID); err != nil {
		slog.Warn("revoke tokens after delete failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// errLastAdmin is the last-admin guard's refusal.
var errLastAdmin = errors.New("last enabled user who can manage users")

// lastAdminCheckError is a guard that could not read the state it checks.
type lastAdminCheckError struct{ err error }

func (e lastAdminCheckError) Error() string { return "last-admin check: " + e.err.Error() }
func (e lastAdminCheckError) Unwrap() error { return e.err }

func (s *Server) handleUsersPassword(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	var req passwordSetRequest
	if !decodeMutationBody(w, r, &req, `body must be JSON with "password"`) {
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", msg)
		return
	}
	u, ok := s.userOr404(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	hash, err := authn.HashPassword(req.Password)
	if err != nil {
		slog.Error("hash password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "password reset failed", "")
		return
	}
	if err := s.userAdmin.UpdateUserPassword(r.Context(), u.ID, hash); err != nil {
		slog.Error("update password failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "password reset failed", "")
		return
	}
	// Every session the user had is now stale: the stamp no longer matches.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) userOr404(w http.ResponseWriter, r *http.Request, id string) (store.User, bool) {
	u, err := s.userAdmin.GetUserByID(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "user not found", "")
		return store.User{}, false
	case err != nil && !isUUID(id):
		// The store refuses a malformed id before it queries; that is a missing user, not a fault.
		writeProblem(w, http.StatusNotFound, "user not found", "")
		return store.User{}, false
	case err != nil:
		slog.Error("get user failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "users unavailable", "")
		return store.User{}, false
	}
	return u, true
}

func isUUID(id string) bool {
	_, err := uuid.Parse(id)
	return err == nil
}

// handleAuthPassword changes the CALLER's own password. The route is public in routeTable (a user
// without users:manage must be able to change their own password), so it carries login's own
// protections: same origin, a JSON content type, and a live local session it resolves itself.
func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth.Mode != "local" {
		writeProblem(w, http.StatusNotFound, "not found", "")
		return
	}
	if s.users == nil || s.sessions == nil || s.userAdmin == nil {
		writeProblem(w, http.StatusServiceUnavailable, "local auth not available", "no user or session store wired")
		return
	}
	if !sameOriginRequest(r) {
		writeProblem(w, http.StatusForbidden, "cross-origin request refused", "a password change must come from this console's own origin")
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported media type", "the body must be sent as application/json")
		return
	}
	cookie, err := r.Cookie(s.cfg.Auth.Session.CookieName)
	if err != nil || cookie.Value == "" {
		writeProblem(w, http.StatusUnauthorized, "not signed in", "")
		return
	}
	sess, ok, err := s.sessions.Get(r.Context(), cookie.Value)
	if err != nil || !ok {
		writeProblem(w, http.StatusUnauthorized, "not signed in", "")
		return
	}
	// The same checks the local authenticator makes: a session a reset or change left stale is
	// signed out here too.
	user, err := s.users.GetUserByUsername(r.Context(), sess.Username)
	if err != nil || user.Disabled ||
		(sess.PasswordStamp != "" && sess.PasswordStamp != authn.SessionStamp(user.PasswordHash, user.SessionEpoch)) {
		writeProblem(w, http.StatusUnauthorized, "not signed in", "")
		return
	}
	// The current password is verified with argon2id, exactly like a login, so the attempt spends
	// login's per-username budget: a stolen session must not guess the password, or burn the pod's
	// memory, any faster than the login form allows.
	if !s.rateLimitSpend(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute, nil, loginUserRateLimitKey(sess.Username)) {
		writeRateLimited(w, passwordRateLimitDetail)
		return
	}
	var req passwordChangeRequest
	dec := strictJSONDecoder(r.Body)
	if err = dec.Decode(&req); err != nil || req.CurrentPassword == "" {
		badBody(w, err, `body must be JSON with "currentPassword" and "newPassword"`)
		return
	}
	if refuseTrailingJSON(w, dec) {
		return
	}
	if match, verr := authn.VerifyPassword(user.PasswordHash, req.CurrentPassword); verr != nil || !match {
		s.metrics.AuthRequests.WithLabelValues("local", "invalid").Inc()
		writeProblem(w, http.StatusUnauthorized, "invalid credentials", "the current password does not match")
		return
	}
	if msg := validatePassword(req.NewPassword); msg != "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", msg)
		return
	}
	hash, err := authn.HashPassword(req.NewPassword)
	if err != nil {
		slog.Error("hash password failed", "error", err)
		writeProblem(w, http.StatusInternalServerError, "password change failed", "")
		return
	}
	if err = s.userAdmin.UpdateUserPassword(r.Context(), user.ID, hash); err != nil {
		slog.Error("update password failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "password change failed", "")
		return
	}
	// Every other session is stale now; the caller keeps working on a fresh one.
	if err = s.sessions.Delete(r.Context(), cookie.Value); err != nil {
		slog.Warn("httpapi: delete old session after password change failed", "error", err)
	}
	id, err := s.sessions.Create(r.Context(), authn.Session{
		Username: user.Username, DisplayName: user.DisplayName, Groups: sess.Groups,
		PasswordStamp: authn.SessionStamp(hash, user.SessionEpoch),
	})
	if err != nil {
		slog.Warn("httpapi: reissue session after password change failed", "error", err)
		s.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.setSessionCookie(w, id)
	w.WriteHeader(http.StatusNoContent)
}
