package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// IncidentService is the subset of *store.DB httpapi needs for
// /api/v1/incidents: the read seam and the write seam together, composed in
// AnnotationService's shape so a test can substitute a fake with no database
// at all, and so this package's contract with store cannot widen every time
// *store.DB grows a method for some other caller.
type IncidentService interface {
	store.IncidentReader
	store.IncidentStore
}

var _ IncidentService = (*store.DB)(nil)

// IncidentNotifier is the outbound half of the incident lifecycle: the seam
// M6 Task 5's dispatcher plugs into so a created, resolved or reopened
// incident reaches whatever endpoints an admin configured (M6 Decision 5).
//
// It returns NOTHING, and that is the contract, not an omission. Delivery is
// asynchronous with a retry ladder, so there is no outcome a handler could
// usefully wait for -- and a handler that could fail on it would answer 502 to
// an incident that WAS recorded, turning someone else's unreachable chat
// endpoint into an outage of the console during exactly the incident it was
// opened for. Notify is expected to enqueue and return; the only thing it may
// use the request's ctx for is its own lookup of who to notify.
//
// nil is the ordinary state: no encryption key, or no database, means no
// dispatcher, and every incident route behaves exactly as it did before this
// interface existed.
type IncidentNotifier interface {
	Notify(ctx context.Context, event string, inc store.Incident)
}

// incidentsUnavailableDetail is served whenever s.incidents is nil, in
// annotationsUnavailableDetail's shape and for the same reason: a saved
// investigation whose whole value is its permalink cannot live in a map that
// vanishes on pod restart, so there is no in-memory fallback and the honest
// answer names the value that turns persistence on.
const incidentsUnavailableDetail = "incidents are persisted investigations with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/incidents"

// incidentValidationPrefix is the prefix every store.IncidentInput.Validate,
// store.ValidatePinned and narrow-update validation error is built with.
// store returns plain errors, no sentinel, so the prefix is the only
// discriminator there is -- isAnnotationValidationError's reasoning, verbatim.
const incidentValidationPrefix = "store: incident: "

// incidentNotesMaxLen mirrors store's own private bound (store/incidents.go).
// A hand-kept-in-sync copy, like eventsMaxLimit is of store's clampLimit: the
// store enforces it too, so a drift here can only make this package's 422
// arrive one byte late, never let an over-long note through.
const incidentNotesMaxLen = 16384

// incidentsUnavailable answers 503 and reports true when no IncidentService is
// wired.
func (s *Server) incidentsUnavailable(w http.ResponseWriter) bool {
	if s.incidents == nil {
		writeProblem(w, http.StatusServiceUnavailable, "incidents not available", incidentsUnavailableDetail)
		return true
	}
	return false
}

// incidentResponse is one saved investigation on the wire. ToAt is omitted for
// an OPEN-ENDED range ("still going"), which is the opposite of what an absent
// annotation endAt means (an instant mark) -- the two columns look alike and
// mean opposite things, so both are documented at every layer. ResolvedAt is
// omitted for an open incident, where it is not merely unknown but forbidden
// (store.validateIncidentStatus).
//
// Pinned is passed through as raw JSON, never re-marshalled through a slice,
// so key order and any field a later milestone adds to a ref survive a
// round-trip -- targetResponse.Labels' reasoning.
type incidentResponse struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Scope      string          `json:"scope"`
	FromAt     time.Time       `json:"fromAt"`
	ToAt       *time.Time      `json:"toAt,omitempty"`
	Status     string          `json:"status"`
	Notes      string          `json:"notes"`
	Pinned     json.RawMessage `json:"pinned"`
	CreatedBy  string          `json:"createdBy"`
	CreatedAt  time.Time       `json:"createdAt"`
	ResolvedAt *time.Time      `json:"resolvedAt,omitempty"`
}

func incidentResponseFrom(i *store.Incident) incidentResponse {
	pinned := i.Pinned
	if len(pinned) == 0 {
		// Defensive: *store.DB always stores [] (orEmptyPinnedArray), but a nil
		// here would marshal as JSON null and the frontend iterates pinned.
		pinned = json.RawMessage(`[]`)
	}
	return incidentResponse{
		ID: i.ID, Title: i.Title, Scope: i.Scope, FromAt: i.FromAt, ToAt: i.ToAt,
		Status: i.Status, Notes: i.Notes, Pinned: pinned,
		CreatedBy: i.CreatedBy, CreatedAt: i.CreatedAt, ResolvedAt: i.ResolvedAt,
	}
}

// incidentsListResponse is GET /api/v1/incidents's body -- the same
// keyset-cursor shape as annotationsListResponse.
type incidentsListResponse struct {
	Incidents  []incidentResponse `json:"incidents"`
	NextCursor string             `json:"nextCursor"`
}

// incidentRequest is POST /api/v1/incidents's body. There is deliberately no
// createdBy (attribution is the SERVER's view of the subject) and no
// status/resolvedAt: an incident is always CREATED open. A client that could
// post a resolved one would be recording an investigation that never happened
// here, and the resolve path -- the one that stamps resolvedAt from the
// server's clock -- is PATCH.
type incidentRequest struct {
	Title  string          `json:"title"`
	Scope  string          `json:"scope"`
	FromAt time.Time       `json:"fromAt"`
	ToAt   *time.Time      `json:"toAt"`
	Notes  string          `json:"notes"`
	Pinned json.RawMessage `json:"pinned"`
}

// incidentPatchRequest is PATCH /api/v1/incidents/{id}'s body: any SUBSET of
// the three fields an incident evolves through. Every field is a pointer so
// "absent" and "set to the zero value" are distinguishable -- absent means
// leave alone, and `"notes":""` means genuinely clear the notes.
type incidentPatchRequest struct {
	Status *string          `json:"status"`
	Notes  *string          `json:"notes"`
	Pinned *json.RawMessage `json:"pinned"`
}

// incidentIDFrom resolves the {id} path parameter, answering 404 and reporting
// false for anything that is not a canonical UUID -- targetIDFrom's reasoning,
// verbatim: an unparseable id and an unknown one are indistinguishable to a
// caller, and letting a malformed one reach pgx would turn a 404 into a 502.
func incidentIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "incident not found", "no incident with that id")
		return "", false
	}
	return id, true
}

// isIncidentValidationError reports whether err came from store's incident
// validation rather than from the database. Deliberately narrow, for
// isAnnotationValidationError's reason: reporting an outage as "your incident
// was rejected" would send the operator to fix something that was never the
// problem.
func isIncidentValidationError(err error) bool {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInUse) {
		return false
	}
	return strings.HasPrefix(err.Error(), incidentValidationPrefix)
}

// writeIncidentStoreError maps an IncidentService error to a response:
// ErrNotFound is 404, a validation error that slipped past this package's own
// pre-checks is 422 (store's rules moved ahead of ours), and anything else is
// an opaque backend failure and therefore 502.
func writeIncidentStoreError(w http.ResponseWriter, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "incident not found", "no incident with that id")
	case isIncidentValidationError(err):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid incident", publicValidationDetail(err))
	default:
		slog.Error("httpapi: incident store call failed", "incident", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "incidents unavailable", "failed to reach the incidents store")
	}
}

// handleIncidentsList serves one page of incidents, newest-CREATED first,
// behind an opaque keyset cursor.
//
// ?scope= carries the annotations THREE states (absent = every scope,
// present-but-empty = the GLOBAL ones, anything else exact), which is why
// store.IncidentFilter.Scope is a pointer. ?status= is an exact match against
// the closed open|resolved vocabulary; an unknown value is a 400 rather than a
// silently empty page, on handleEvents' ?type= precedent -- it can never match
// a row, so it is a typo worth naming.
//
// from/to bound the window an incident's OWN RANGE must OVERLAP, not the
// window it was created in: an incident that began before the range and is
// still open is exactly the one an operator looking at that range needs. Like
// annotations and unlike events, an inverted window is NOT a 400 -- the range
// comes from a chart's visible extent, and a degenerate one is simply a range
// with nothing in it.
func (s *Server) handleIncidentsList(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	q := r.URL.Query()

	status := q.Get("status")
	if status != "" && status != store.IncidentStatusOpen && status != store.IncidentStatusResolved {
		writeProblem(w, http.StatusBadRequest, "invalid status",
			"status must be one of open, resolved")
		return
	}

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

	page, err := s.incidents.ListIncidents(r.Context(), store.IncidentFilter{
		Status: status,
		Scope:  scope,
		From:   from,
		To:     to,
		Cursor: cursor,
		Limit:  clampPageLimit(parsePageLimit(q.Get("limit"))),
	})
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN or other
		// upstream detail that has no business in an HTTP response body.
		slog.Error("list incidents failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "incidents unavailable", "failed to query incidents")
		return
	}

	out := make([]incidentResponse, 0, len(page.Incidents))
	for i := range page.Incidents {
		out = append(out, incidentResponseFrom(&page.Incidents[i]))
	}
	writeJSON(w, incidentsListResponse{Incidents: out, NextCursor: page.NextCursor})
}

// handleIncidentsCreate opens one investigation: 201 with a Location header
// naming the new row, matching POST /api/v1/annotations.
//
// The incident is always created OPEN with no resolvedAt, whatever the body
// says -- see incidentRequest. Validation is delegated to
// store.IncidentInput.Validate so the title bound, the notes bound and the
// pinned vocabulary cannot drift from the ones the database layer enforces.
func (s *Server) handleIncidentsCreate(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	var req incidentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`an incident body must be JSON with "title" and "fromAt" (RFC3339), `+
				`plus optional "toAt", "scope", "notes" and "pinned"`)
		return
	}

	subject, _ := SubjectFrom(r.Context())
	in := store.IncidentInput{
		Title: req.Title, Scope: req.Scope, FromAt: req.FromAt, ToAt: req.ToAt,
		Status: store.IncidentStatusOpen, Notes: req.Notes, Pinned: req.Pinned,
		CreatedBy: annotationAuthor(subject),
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid incident", publicValidationDetail(err))
		return
	}

	inc, err := s.incidents.CreateIncident(r.Context(), in)
	if err != nil {
		writeIncidentStoreError(w, "", err)
		return
	}

	// Notified AFTER the row exists and BEFORE the 201 is written: the
	// database is the source of truth, so nothing is announced that was not
	// recorded, and the dispatcher enqueues rather than delivering, so the
	// caller waits on one unpaged SELECT rather than on anyone's endpoint.
	s.notifyIncident(r.Context(), store.WebhookEventIncidentCreated, &inc)

	w.Header().Set("Location", "/api/v1/incidents/"+inc.ID)
	writeJSONStatus(w, http.StatusCreated, incidentResponseFrom(&inc))
}

// notifyIncident hands one lifecycle event to the dispatcher, if there is one.
// The nil check lives HERE rather than at each call site so "no key
// configured" cannot become a nil-pointer panic in a handler that is otherwise
// working perfectly.
func (s *Server) notifyIncident(ctx context.Context, event string, inc *store.Incident) {
	if s.notifier == nil {
		return
	}
	s.notifier.Notify(ctx, event, *inc)
}

// handleIncidentsGet serves one incident. An unknown id and a malformed one
// are both 404 (incidentIDFrom).
func (s *Server) handleIncidentsGet(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	id, ok := incidentIDFrom(w, r)
	if !ok {
		return
	}
	inc, err := s.incidents.GetIncident(r.Context(), id)
	if err != nil {
		writeIncidentStoreError(w, id, err)
		return
	}
	writeJSON(w, incidentResponseFrom(&inc))
}

// handleIncidentsUpdate is THE ONE PATCH in this API, and the one exception to
// the repo's full-replace PUT convention (targets, checks, schedules,
// webhooks). It is deliberate, not an oversight:
//
// An incident EVOLVES under collaboration. One operator is typing notes while
// another pins a finding and a third resolves it -- and a full-replace PUT
// would make every one of those a read-modify-write over the whole row, so the
// last writer silently discards the other two's work. A PATCH naming only what
// it changes is the shape that does not race, and it is exactly why store
// exposes THREE narrow updates (status, notes, pinned) instead of one
// UpdateIncident: each PATCH touches only the columns it was asked about.
//
// Semantics:
//   - body is any SUBSET of {status, notes, pinned}; an empty subset is 422,
//     because a PATCH that asks for nothing is far more likely a typo'd field
//     name than a deliberate no-op, and answering 200 would hide it.
//   - status is the closed open|resolved vocabulary; anything else is 422.
//   - open -> resolved stamps resolvedAt from the SERVER's clock; resolved ->
//     open clears it (a reopened incident is not resolved). A status equal to
//     the current one is a no-op, NOT a re-stamp: the timestamp records when
//     the incident was resolved, and a second PATCH did not re-resolve it.
//
// The whole body is validated BEFORE the first store call, so the only way to
// half-apply a multi-field PATCH is a backend failure between two of the three
// statements -- an outage, reported as 502, not a rejected value.
func (s *Server) handleIncidentsUpdate(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	id, ok := incidentIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeIncidentPatch(w, r)
	if !ok {
		return
	}

	// The current row is read first for two reasons: it turns an unknown id
	// into a 404 before anything is written, and the status transition needs
	// to know where it is coming FROM to decide whether it is a transition at
	// all.
	current, err := s.incidents.GetIncident(r.Context(), id)
	if err != nil {
		writeIncidentStoreError(w, id, err)
		return
	}

	// lifecycleEvent is set ONLY by a real transition, which is what makes the
	// no-op PATCH (status equal to the current one) silent on the wire as well
	// as in the database: re-sending "resolved" must not re-announce a
	// resolution that already happened.
	var lifecycleEvent string
	if req.Status != nil && *req.Status != current.Status {
		var resolvedAt *time.Time
		if *req.Status == store.IncidentStatusResolved {
			now := time.Now().UTC()
			resolvedAt = &now
			lifecycleEvent = store.WebhookEventIncidentResolved
		} else {
			lifecycleEvent = store.WebhookEventIncidentReopened
		}
		current, err = s.incidents.UpdateIncidentStatus(r.Context(), id, *req.Status, resolvedAt)
		if err != nil {
			writeIncidentStoreError(w, id, err)
			return
		}
	}
	if req.Notes != nil {
		current, err = s.incidents.UpdateIncidentNotes(r.Context(), id, *req.Notes)
		if err != nil {
			writeIncidentStoreError(w, id, err)
			return
		}
	}
	if req.Pinned != nil {
		current, err = s.incidents.UpdateIncidentPinned(r.Context(), id, *req.Pinned)
		if err != nil {
			writeIncidentStoreError(w, id, err)
			return
		}
	}

	// Notified after EVERY named field has been applied, and with the FINAL
	// row: a PATCH that both resolves an incident and edits its notes must
	// announce the incident as it now stands, not as it stood one statement
	// ago. A store failure on a later field returns above, so nothing is
	// announced from a half-applied patch.
	if lifecycleEvent != "" {
		s.notifyIncident(r.Context(), lifecycleEvent, &current)
	}

	writeJSON(w, incidentResponseFrom(&current))
}

// decodeIncidentPatch reads and validates a PATCH body in full before the
// caller writes anything. A body that is not JSON at all is a 400; a
// well-formed body whose VALUES break an incident rule -- or that names none
// of the three patchable fields -- is a 422, the same distinction
// decodeTargetRequest draws.
func decodeIncidentPatch(w http.ResponseWriter, r *http.Request) (incidentPatchRequest, bool) {
	var req incidentPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`an incident patch body must be JSON with any subset of "status", "notes" and "pinned"`)
		return incidentPatchRequest{}, false
	}
	if req.Status == nil && req.Notes == nil && req.Pinned == nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid incident",
			`incident: a patch must name at least one of "status", "notes" or "pinned"`)
		return incidentPatchRequest{}, false
	}
	if req.Status != nil &&
		*req.Status != store.IncidentStatusOpen && *req.Status != store.IncidentStatusResolved {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid incident",
			"incident: status "+strconv.Quote(*req.Status)+" must be one of open, resolved")
		return incidentPatchRequest{}, false
	}
	if req.Notes != nil && len(*req.Notes) > incidentNotesMaxLen {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid incident",
			fmt.Sprintf("incident: notes are %d bytes, limit is %d", len(*req.Notes), incidentNotesMaxLen))
		return incidentPatchRequest{}, false
	}
	if req.Pinned != nil {
		// store.ValidatePinned is EXPORTED precisely so this pre-check and the
		// database layer's own check are one rule, not two that can drift.
		if err := store.ValidatePinned(*req.Pinned); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid incident", publicValidationDetail(err))
			return incidentPatchRequest{}, false
		}
	}
	return req, true
}

// handleIncidentsDelete removes one incident. Deleting one that is not there
// is 404, not success -- the caller asked about a SPECIFIC investigation.
func (s *Server) handleIncidentsDelete(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	id, ok := incidentIDFrom(w, r)
	if !ok {
		return
	}
	if err := s.incidents.DeleteIncident(r.Context(), id); err != nil {
		writeIncidentStoreError(w, id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
