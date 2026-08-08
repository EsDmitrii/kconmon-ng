package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// MaintenanceService is the subset of *store.DB httpapi needs for
// /api/v1/maintenance: the read seam and the write seam together, in
// AnnotationService's shape. There is no update half because there is no
// update -- a window is two timestamps and a reason, so delete-and-recreate is
// both the correction path and the whole of it (M6 Task 1's own note on
// DeleteMaintenanceWindow).
type MaintenanceService interface {
	store.MaintenanceReader
	store.MaintenanceStore
}

var _ MaintenanceService = (*store.DB)(nil)

// maintenanceUnavailableDetail is served whenever s.maintenance is nil, in
// annotationsUnavailableDetail's shape: a declared window whose whole job is
// to explain a chart weeks later cannot live in a map that vanishes on pod
// restart, so there is no in-memory fallback.
const maintenanceUnavailableDetail = "maintenance windows are persisted operator declarations with no " +
	"in-memory fallback: set console.database.mode in the console config (Helm: console.database.mode) " +
	"to enable /api/v1/maintenance"

// maintenanceValidationPrefix is the prefix store.MaintenanceInput.Validate
// builds every one of its errors with -- the only discriminator there is, same
// as annotationValidationPrefix.
const maintenanceValidationPrefix = "store: maintenance window: "

// maintenanceUnavailable answers 503 and reports true when no
// MaintenanceService is wired.
func (s *Server) maintenanceUnavailable(w http.ResponseWriter) bool {
	if s.maintenance == nil {
		writeProblem(w, http.StatusServiceUnavailable, "maintenance windows not available",
			maintenanceUnavailableDetail)
		return true
	}
	return false
}

// maintenanceResponse is one declared window on the wire. Both ends are
// present and required: unlike an annotation, a window with no end is not a
// window (the table's own CHECK says end_at > start_at).
type maintenanceResponse struct {
	ID string `json:"id"`
	// Scope is "" for a global window; any other value names a node, a pair or
	// a target and is matched exactly -- the annotations scope vocabulary
	// (M6 Decision 6). A filter key, never a Prometheus label value.
	Scope     string    `json:"scope"`
	StartAt   time.Time `json:"startAt"`
	EndAt     time.Time `json:"endAt"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

func maintenanceResponseFrom(m *store.MaintenanceWindow) maintenanceResponse {
	return maintenanceResponse{
		ID: m.ID, Scope: m.Scope, StartAt: m.StartAt, EndAt: m.EndAt,
		Reason: m.Reason, CreatedBy: m.CreatedBy, CreatedAt: m.CreatedAt,
	}
}

// maintenanceListResponse is GET /api/v1/maintenance's body -- the same
// keyset-cursor shape as annotationsListResponse.
type maintenanceListResponse struct {
	Windows    []maintenanceResponse `json:"windows"`
	NextCursor string                `json:"nextCursor"`
}

// maintenanceRequest is POST /api/v1/maintenance's body. No createdBy, for
// annotationRequest's reason: attribution is the SERVER's view of who declared
// the window, never something a client can state.
type maintenanceRequest struct {
	Scope   string    `json:"scope"`
	StartAt time.Time `json:"startAt"`
	EndAt   time.Time `json:"endAt"`
	Reason  string    `json:"reason"`
}

// maintenanceIDFrom resolves the {id} path parameter, answering 404 and
// reporting false for anything that is not a canonical UUID -- targetIDFrom's
// reasoning, verbatim.
func maintenanceIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "maintenance window not found", "no maintenance window with that id")
		return "", false
	}
	return id, true
}

// isMaintenanceValidationError reports whether err came from
// store.MaintenanceInput.Validate rather than from the database.
func isMaintenanceValidationError(err error) bool {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInUse) {
		return false
	}
	return strings.HasPrefix(err.Error(), maintenanceValidationPrefix)
}

// handleMaintenanceList serves one page of windows, newest-STARTING first,
// behind an opaque keyset cursor.
//
// from/to bound the window a maintenance window must OVERLAP, not contain: one
// that opened before the range and is still running inside it is exactly the
// one that explains what the operator is looking at. ?scope= carries the same
// THREE states as annotations (absent = every scope, present-but-empty = the
// GLOBAL ones, anything else exact), which is what lets a scoped chart ask for
// "global windows plus mine" in two cheap queries instead of filtering the
// whole table client-side. An inverted range is an empty page, not a 400 --
// handleAnnotationsList's reasoning, and for the same caller (a chart's
// visible extent).
func (s *Server) handleMaintenanceList(w http.ResponseWriter, r *http.Request) {
	if s.maintenanceUnavailable(w) {
		return
	}
	q := r.URL.Query()

	from, ok := parseEventsTime(w, q.Get("from"), "from")
	if !ok {
		return
	}
	to, ok := parseEventsTime(w, q.Get("to"), "to")
	if !ok {
		return
	}

	var scope *string
	if q.Has("scope") {
		v := q.Get("scope")
		scope = &v
	}

	cursor := q.Get("cursor")
	if cursor != "" {
		if _, _, _, err := store.DecodeUUIDCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	page, err := s.maintenance.ListMaintenanceWindows(r.Context(), store.MaintenanceFilter{
		Scope:  scope,
		From:   from,
		To:     to,
		Cursor: cursor,
		Limit:  clampPageLimit(parsePageLimit(q.Get("limit"))),
	})
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN.
		slog.Error("list maintenance windows failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "maintenance windows unavailable",
			"failed to query maintenance windows")
		return
	}

	out := make([]maintenanceResponse, 0, len(page.Windows))
	for i := range page.Windows {
		out = append(out, maintenanceResponseFrom(&page.Windows[i]))
	}
	writeJSON(w, maintenanceListResponse{Windows: out, NextCursor: page.NextCursor})
}

// handleMaintenanceCreate declares one window: 201 with a Location header
// naming the new row, matching POST /api/v1/annotations.
//
// Validation is delegated to store.MaintenanceInput.Validate so the
// end-after-start rule and the 512-byte reason bound cannot drift from the
// ones the database layer (and the table's own CHECK) actually enforce.
func (s *Server) handleMaintenanceCreate(w http.ResponseWriter, r *http.Request) {
	if s.maintenanceUnavailable(w) {
		return
	}
	var req maintenanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`a maintenance window body must be JSON with "startAt" and "endAt" (RFC3339) and "reason", `+
				`plus an optional "scope"`)
		return
	}

	subject, _ := SubjectFrom(r.Context())
	in := store.MaintenanceInput{
		Scope: req.Scope, StartAt: req.StartAt, EndAt: req.EndAt, Reason: req.Reason,
		CreatedBy: annotationAuthor(subject),
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid maintenance window", publicValidationDetail(err))
		return
	}

	win, err := s.maintenance.CreateMaintenanceWindow(r.Context(), in)
	if err != nil {
		if isMaintenanceValidationError(err) {
			// Validate ran above, so this can only mean store's rules moved
			// ahead of this package's copy -- report it as the rejected value
			// it is, not as a backend outage.
			writeProblem(w, http.StatusUnprocessableEntity, "invalid maintenance window", publicValidationDetail(err))
			return
		}
		slog.Error("create maintenance window failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "maintenance windows unavailable",
			"failed to create the maintenance window")
		return
	}

	w.Header().Set("Location", "/api/v1/maintenance/"+win.ID)
	writeJSONStatus(w, http.StatusCreated, maintenanceResponseFrom(&win))
}

// handleMaintenanceDelete removes one window -- which is also how a window is
// CORRECTED, there being no update route. Deleting one that is not there is
// 404, not success.
func (s *Server) handleMaintenanceDelete(w http.ResponseWriter, r *http.Request) {
	if s.maintenanceUnavailable(w) {
		return
	}
	id, ok := maintenanceIDFrom(w, r)
	if !ok {
		return
	}
	if err := s.maintenance.DeleteMaintenanceWindow(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "maintenance window not found", "no maintenance window with that id")
			return
		}
		slog.Error("delete maintenance window failed", "window", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "maintenance windows unavailable",
			"failed to delete the maintenance window")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
