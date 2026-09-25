package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
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
	SetUserDisabled(ctx context.Context, id string, disabled bool) error
	SetUserRole(ctx context.Context, id, role string) error
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
)

// usernamePattern keeps names printable in the audit log, the user menu and a session key.
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

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
		return "password must be at least 12 characters"
	}
	if len(p) > maxPasswordBytes {
		return "password must be at most 256 bytes"
	}
	return ""
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
			"local users live in the console database; set console.database.mode to enable /api/v1/users")
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

// canManageUsers reports whether an enabled user holds users:manage through the same resolution a
// request of theirs would get.
func (s *Server) canManageUsers(ctx context.Context, u *store.User) bool {
	if u.Disabled {
		return false
	}
	return s.policy.Can(s.resolveRoles(ctx, authz.Subject{Kind: authz.SubjectUser, ID: u.ID}), authz.PermUsersManage)
}

// wouldLockOut reports whether taking users:manage away from target, by disabling them or by a role
// without it, leaves no enabled user who can manage users.
func (s *Server) wouldLockOut(ctx context.Context, target *store.User) (bool, error) {
	if !s.canManageUsers(ctx, target) {
		return false, nil
	}
	users, err := s.userAdmin.ListUsers(ctx)
	if err != nil {
		return false, err
	}
	for i := range users {
		if users[i].ID != target.ID && s.canManageUsers(ctx, &users[i]) {
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
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, bodyShape)
		return
	}
	if !usernamePattern.MatchString(req.Username) {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			"username must be 1-64 characters of letters, digits and . _ @ -")
		return
	}
	if msg := validatePassword(req.Password); msg != "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", msg)
		return
	}
	known, err := s.roleKnown(r.Context(), req.Role)
	if err != nil {
		slog.Error("read roles failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "roles unavailable", "failed to read roles")
		return
	}
	if !known {
		writeProblem(w, http.StatusBadRequest, "invalid request", "role must be a built-in or an existing custom role")
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
	u, err := s.userAdmin.CreateUserWithRole(r.Context(), req.Username, hash, display, req.Role)
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

func (s *Server) handleUsersPatch(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	var req userPatchRequest
	const bodyShape = `body must be JSON with "disabled" (boolean), "role" (string), or both`
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil || (req.Disabled == nil && req.Role == nil) {
		badBody(w, err, bodyShape)
		return
	}
	u, ok := s.userOr404(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if req.Role != nil {
		known, err := s.roleKnown(r.Context(), *req.Role)
		if err != nil {
			slog.Error("read roles failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "roles unavailable", "failed to read roles")
			return
		}
		if !known {
			writeProblem(w, http.StatusBadRequest, "invalid request", "role must be a built-in or an existing custom role")
			return
		}
	}
	// Losing users:manage, by being disabled or by a role without it, must leave someone who has it.
	losing := (req.Disabled != nil && *req.Disabled) ||
		(req.Role != nil && !s.policy.Can(authz.Subject{Kind: authz.SubjectUser, Roles: []string{*req.Role}}, authz.PermUsersManage))
	if losing {
		lock, err := s.wouldLockOut(r.Context(), &u)
		if err != nil {
			slog.Error("last-admin check failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "users unavailable", "failed to read users")
			return
		}
		if lock {
			writeProblem(w, http.StatusConflict, "last administrator",
				"this is the last enabled user who can manage users; grant users:manage to someone else first")
			return
		}
	}
	if req.Role != nil {
		if err := s.userAdmin.SetUserRole(r.Context(), u.ID, *req.Role); err != nil {
			slog.Error("set user role failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "user update failed", "")
			return
		}
	}
	if req.Disabled != nil {
		if err := s.userAdmin.SetUserDisabled(r.Context(), u.ID, *req.Disabled); err != nil {
			slog.Error("set user disabled failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "user update failed", "")
			return
		}
		u.Disabled = *req.Disabled
	}
	writeJSON(w, s.userView(r.Context(), &u))
}

func (s *Server) handleUsersPassword(w http.ResponseWriter, r *http.Request) {
	if s.usersUnavailable(w) {
		return
	}
	var req passwordSetRequest
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		badBody(w, err, `body must be JSON with "password"`)
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
	// The current password is verified with argon2id, exactly like a login, so the attempt spends
	// login's per-username budget: a stolen session must not guess the password, or burn the pod's
	// memory, any faster than the login form allows.
	if !s.rateLimitAllow(r.Context(), rateLimitLogin, s.cfg.RateLimit.LoginPerMinute, loginUserRateLimitKey(sess.Username)) {
		writeRateLimited(w, passwordRateLimitDetail)
		return
	}
	var req passwordChangeRequest
	if err = strictJSONDecoder(r.Body).Decode(&req); err != nil || req.CurrentPassword == "" {
		badBody(w, err, `body must be JSON with "currentPassword" and "newPassword"`)
		return
	}
	user, err := s.users.GetUserByUsername(r.Context(), sess.Username)
	if err != nil || user.Disabled {
		writeProblem(w, http.StatusUnauthorized, "not signed in", "")
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
		PasswordStamp: authn.PasswordStamp(hash),
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
