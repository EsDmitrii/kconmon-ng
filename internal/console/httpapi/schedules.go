package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// ScheduleService is the subset of *store.DB httpapi needs for CRUD /api/v1/schedules; those two
// belong to the scheduler loop, and the alternative.
type ScheduleService interface {
	store.ScheduleReader
	store.ScheduleStore
}

var _ ScheduleService = (*store.DB)(nil)

// schedulesUnavailableDetail is served whenever s.schedules is nil; same reasoning as targets and
// definitions.
const schedulesUnavailableDetail = "schedules are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/schedules"

// minScheduleInterval is the floor an interval cadence is CLAMPED UP to; a one-second schedule
// against a definition that fans out to hundreds of pairs is a self-DoS with no legitimate use.
const minScheduleInterval = 10 * time.Second

// cronDeferredDetail is the honest, specific refusal.
const cronDeferredDetail = "schedule: cron schedules land in a later milestone: " +
	`use "kind":"interval" with "intervalNs", or "kind":"once" with "runAt"`

// schedulesUnavailable answers 503 and reports true when no ScheduleService
// is wired.
func (s *Server) schedulesUnavailable(w http.ResponseWriter) bool {
	if s.schedules == nil {
		writeProblem(w, http.StatusServiceUnavailable, "schedules not available", schedulesUnavailableDetail)
		return true
	}
	return false
}

// scheduleResponse is one schedule on the wire; IntervalNs is nanoseconds, the repo-wide duration
// convention (API.md).
type scheduleResponse struct {
	ID           string     `json:"id"`
	DefinitionID string     `json:"definitionId"`
	Kind         string     `json:"kind"`
	IntervalNs   int64      `json:"intervalNs"`
	RunAt        *time.Time `json:"runAt"`
	Enabled      bool       `json:"enabled"`
	LastFiredAt  *time.Time `json:"lastFiredAt"`
	NextFireAt   *time.Time `json:"nextFireAt"`
	LastError    string     `json:"lastError"`
	LastErrorAt  *time.Time `json:"lastErrorAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

func scheduleResponseFrom(s *store.Schedule) scheduleResponse {
	return scheduleResponse{
		ID: s.ID, DefinitionID: s.DefinitionID, Kind: s.Kind, IntervalNs: s.IntervalNs,
		RunAt: s.RunAt, Enabled: s.Enabled, LastFiredAt: s.LastFiredAt, NextFireAt: s.NextFireAt,
		LastError: s.LastError, LastErrorAt: s.LastErrorAt,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

// schedulesListResponse is GET /api/v1/schedules's body.
type schedulesListResponse struct {
	Schedules  []scheduleResponse `json:"schedules"`
	NextCursor string             `json:"nextCursor"`
}

// scheduleRequest is POST /api/v1/schedules's and PUT /api/v1/schedules/{id}'s body; NextFireAt is
// deliberately absent: it is scheduler bookkeeping.
type scheduleRequest struct {
	DefinitionID string     `json:"definitionId"`
	Kind         string     `json:"kind"`
	IntervalNs   int64      `json:"intervalNs,omitempty"`
	RunAt        *time.Time `json:"runAt,omitempty"`
	Enabled      bool       `json:"enabled"`
}

// decodeScheduleRequest reads a create/update body into a store.ScheduleInput with every rule
// applied.
func decodeScheduleRequest(w http.ResponseWriter, r *http.Request, defID string) (store.ScheduleInput, bool) {
	var req scheduleRequest
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", unknownFieldDetail(err,
			`body must be JSON with "definitionId", "kind" ("once", "interval" or "continuous"), `+
				`"intervalNs" in nanoseconds for kind interval, "runAt" for kind once, and an optional "enabled" flag`))
		return store.ScheduleInput{}, false
	}
	if req.Kind == "cron" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule", cronDeferredDetail)
		return store.ScheduleInput{}, false
	}

	switch {
	case defID == "":
		// Create: the body is the only source there is.
		defID = req.DefinitionID
	case req.DefinitionID != "" && req.DefinitionID != defID:
		// Update: a body that names a DIFFERENT definition is refused rather
		// than ignored -- see handleSchedulesUpdate. An omitted or matching
		// one is fine and keeps the stored value.
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule",
			"schedule: definition id is not updatable; this schedule belongs to definition "+strconv.Quote(defID)+
				", create a new schedule to bind another definition")
		return store.ScheduleInput{}, false
	}
	in := store.ScheduleInput{
		DefinitionID: defID,
		Kind:         req.Kind,
		IntervalNs:   clampScheduleInterval(req.Kind, req.IntervalNs),
		RunAt:        req.RunAt,
		Enabled:      req.Enabled,
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule", publicValidationDetail(err))
		return store.ScheduleInput{}, false
	}
	if in.Kind == "once" && !in.RunAt.After(time.Now()) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule",
			"schedule: kind once requires a run at time in the future")
		return store.ScheduleInput{}, false
	}
	in.NextFireAt = seedNextFireAt(&in)
	return in, true
}

// clampScheduleInterval raises a positive sub-floor interval to minScheduleInterval; a zero or
// negative one is passed through untouched so store's own Validate produces the message ("kind
// interval requires a positive interval" / "kind once must not carry an interval").
func clampScheduleInterval(kind string, ns int64) int64 {
	if kind == "interval" && ns > 0 && ns < int64(minScheduleInterval) {
		return int64(minScheduleInterval)
	}
	return ns
}

// seedNextFireAt gives a new or edited schedule its place in the due index
// (check_schedules_due_idx, WHERE enabled); the scheduler advances it from there through
// MarkScheduleFired.
func seedNextFireAt(in *store.ScheduleInput) *time.Time {
	switch in.Kind {
	case "once":
		return in.RunAt
	case "interval":
		now := time.Now().UTC()
		return &now
	default: // continuous
		return nil
	}
}

// scheduleIDFrom resolves the {id} path parameter, answering 404 for anything
// that is not a canonical UUID -- same guard, same reasoning as targetIDFrom.
func scheduleIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "schedule not found", "no schedule with that id")
		return "", false
	}
	return id, true
}

// writeScheduleStoreError maps a ScheduleService error for a request that
// names a schedule by id: ErrNotFound is then genuinely a missing schedule.
func writeScheduleStoreError(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "schedule not found", "no schedule with that id")
		return
	}
	slog.Error("httpapi: schedule store call failed", "schedule", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
	writeProblem(w, http.StatusBadGateway, "schedules unavailable", "failed to reach the schedules store")
}

// writeScheduleCreateError is the CREATE path's mapping, where ErrNotFound means something
// completely different.
func writeScheduleCreateError(w http.ResponseWriter, defID string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid schedule",
			"schedule: definition id "+strconv.Quote(defID)+" names no check definition")
		return
	}
	writeScheduleStoreError(w, "", err)
}

// handleSchedulesList serves one page of schedules, optionally filtered by
// ?definitionId=.
func (s *Server) handleSchedulesList(w http.ResponseWriter, r *http.Request) {
	if s.schedulesUnavailable(w) {
		return
	}

	q := r.URL.Query()

	cursor := q.Get("cursor")
	if cursor != "" {
		if _, _, _, err := store.DecodeUUIDCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}
	// Query-filter twin of the {id} routes' uuid pre-check — a typo'd
	// definitionId is a 400, never a 502 (see handleChecksList).
	if did := q.Get("definitionId"); did != "" {
		if _, err := uuid.Parse(did); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid definitionId", "definitionId must be a UUID")
			return
		}
	}

	page, err := s.schedules.ListSchedules(r.Context(), store.ScheduleFilter{
		DefinitionID: q.Get("definitionId"),
		Cursor:       cursor,
		Limit:        clampTargetsLimit(parseTargetsLimit(q.Get("limit"))),
	})
	if err != nil {
		slog.Error("list schedules failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "schedules unavailable", "failed to query schedules")
		return
	}

	out := make([]scheduleResponse, 0, len(page.Schedules))
	for i := range page.Schedules {
		out = append(out, scheduleResponseFrom(&page.Schedules[i]))
	}
	writeJSON(w, schedulesListResponse{Schedules: out, NextCursor: page.NextCursor})
}

// handleSchedulesCreate creates one schedule: 201 plus a Location header.
func (s *Server) handleSchedulesCreate(w http.ResponseWriter, r *http.Request) {
	if s.schedulesUnavailable(w) {
		return
	}
	in, ok := decodeScheduleRequest(w, r, "")
	if !ok {
		return
	}

	sched, err := s.schedules.CreateSchedule(r.Context(), in)
	if err != nil {
		writeScheduleCreateError(w, in.DefinitionID, err)
		return
	}

	w.Header().Set("Location", "/api/v1/schedules/"+sched.ID)
	writeJSONStatus(w, http.StatusCreated, scheduleResponseFrom(&sched))
}

// handleSchedulesGet serves one schedule.
func (s *Server) handleSchedulesGet(w http.ResponseWriter, r *http.Request) {
	if s.schedulesUnavailable(w) {
		return
	}
	id, ok := scheduleIDFrom(w, r)
	if !ok {
		return
	}

	sched, err := s.schedules.GetSchedule(r.Context(), id)
	if err != nil {
		writeScheduleStoreError(w, id, err)
		return
	}
	writeJSON(w, scheduleResponseFrom(&sched))
}

// handleSchedulesUpdate replaces one schedule's cadence in full; accepting a different definitionId
// in the body and ignoring it would leave a client convinced it had moved the schedule.
func (s *Server) handleSchedulesUpdate(w http.ResponseWriter, r *http.Request) {
	if s.schedulesUnavailable(w) {
		return
	}
	id, ok := scheduleIDFrom(w, r)
	if !ok {
		return
	}
	existing, err := s.schedules.GetSchedule(r.Context(), id)
	if err != nil {
		writeScheduleStoreError(w, id, err)
		return
	}

	in, ok := decodeScheduleRequest(w, r, existing.DefinitionID)
	if !ok {
		return
	}

	sched, err := s.schedules.UpdateSchedule(r.Context(), id, in)
	if err != nil {
		writeScheduleStoreError(w, id, err)
		return
	}
	writeJSON(w, scheduleResponseFrom(&sched))
}

// handleSchedulesDelete removes one schedule. Nothing references a schedule,
// so there is no ErrInUse case and no 409 -- unlike a target, which check
// definitions point at.
func (s *Server) handleSchedulesDelete(w http.ResponseWriter, r *http.Request) {
	if s.schedulesUnavailable(w) {
		return
	}
	id, ok := scheduleIDFrom(w, r)
	if !ok {
		return
	}

	if err := s.schedules.DeleteSchedule(r.Context(), id); err != nil {
		writeScheduleStoreError(w, id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
