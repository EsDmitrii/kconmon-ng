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

	"github.com/EsDmitrii/kconmon-ng/internal/console/events"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// IncidentService is the subset of *store.DB httpapi needs for /api/v1/incidents: the read seam and
// the write seam together.
type IncidentService interface {
	store.IncidentReader
	store.IncidentStore
}

var _ IncidentService = (*store.DB)(nil)

// IncidentNotifier is the outbound half of the incident lifecycle; delivery is asynchronous with a
// retry ladder.
type IncidentNotifier interface {
	Notify(ctx context.Context, event string, inc store.Incident)
}

// incidentsUnavailableDetail is served whenever s.incidents is nil.
const incidentsUnavailableDetail = "incidents are persisted investigations with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/incidents"

// incidentValidationPrefix is the prefix every store.IncidentInput.Validate.
const incidentValidationPrefix = "store: incident: "

// incidentNotesMaxLen mirrors store's own private bound (store/incidents.go); a hand-kept-in-sync
// copy, like eventsMaxLimit is of store's clampLimit.
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

// incidentResponse is one saved investigation on the wire; pinned is passed through as raw JSON,
// never re-marshalled through a slice.
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

// incidentRequest is POST /api/v1/incidents's body; a client that could post a resolved one would
// be recording an investigation that never happened here.
type incidentRequest struct {
	Title  string          `json:"title"`
	Scope  string          `json:"scope"`
	FromAt time.Time       `json:"fromAt"`
	ToAt   *time.Time      `json:"toAt"`
	Notes  string          `json:"notes"`
	Pinned json.RawMessage `json:"pinned"`
}

// incidentPatchRequest is PATCH /api/v1/incidents/{id}'s body: any SUBSET of the three fields an
// incident evolves through.
type incidentPatchRequest struct {
	Status *string          `json:"status"`
	Notes  *string          `json:"notes"`
	Pinned *json.RawMessage `json:"pinned"`
}

// incidentIDFrom resolves the {id} path parameter, answering 404 and reporting false for anything
// that is not a canonical UUID.
func incidentIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "incident not found", "no incident with that id")
		return "", false
	}
	return id, true
}

// isIncidentValidationError reports whether err came from store's incident validation rather than
// from the database.
func isIncidentValidationError(err error) bool {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrAlreadyExists) || errors.Is(err, store.ErrInUse) {
		return false
	}
	return strings.HasPrefix(err.Error(), incidentValidationPrefix)
}

// writeIncidentStoreError maps an IncidentService error to a response: ErrNotFound is 404.
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

// handleIncidentsList serves one page of incidents, newest-CREATED first, behind an opaque keyset
// cursor.
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
		// A control char here (a NUL above all) would 502 out of the text column;
		// it is client input, so it is a 400 before the query is built.
		if rejectControlChars(w, "scope", q.Get("scope")) {
			return
		}
		// Normalized so a scope filter matches whichever arrow form the window was written with.
		v := events.NormalizePairScope(q.Get("scope"))
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

// handleIncidentsCreate opens one investigation: 201 with a Location header naming the new row;
// validation is delegated to store.IncidentInput.Validate so the title bound.
func (s *Server) handleIncidentsCreate(w http.ResponseWriter, r *http.Request) {
	if s.incidentsUnavailable(w) {
		return
	}
	var req incidentRequest
	// Deliberately LENIENT (no DisallowUnknownFields): client-owned state
	// (createdBy, status) is IGNORED, not rejected -- the server sets the
	// authenticated subject and opens the incident itself
	// (TestIncidentsCreateRecordsTheSubjectAndIgnoresClientState).
	// STRICT, as the schema says: a misspelled optional field must be refused, not dropped.
	if !decodeMutationBody(w, r, &req,
		`an incident body must be JSON with "title" and "fromAt" (RFC3339), `+
			`plus optional "toAt", "scope", "notes" and "pinned"`) {
		return
	}

	subject, _ := SubjectFrom(r.Context())
	in := store.IncidentInput{
		Title: req.Title, Scope: events.NormalizePairScope(req.Scope), FromAt: req.FromAt, ToAt: req.ToAt,
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

	/* Notified AFTER the row exists and BEFORE the 201 is written: the database is the source of
	   truth.

	   WithoutCancel, because the notification is owed by the COMMIT, not by the client still being
	   there to hear about it. On the request context, a closed tab or a proxy read timeout made the
	   dispatcher's own endpoint listing fail with "context canceled" — the incident existed and
	   incident.created was never delivered, never retried, and nothing anywhere reconciles it. */
	s.notifyIncident(context.WithoutCancel(r.Context()), store.WebhookEventIncidentCreated, &inc)

	w.Header().Set("Location", "/api/v1/incidents/"+inc.ID)
	writeJSONStatus(w, http.StatusCreated, incidentResponseFrom(&inc))
}

// notifyIncident hands one lifecycle event to the dispatcher, if there is one; the nil check lives
// HERE rather than at each call site so "no key configured" cannot become a nil-pointer panic in a
// handler that is otherwise working perfectly.
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

// handleIncidentsUpdate is THE ONE PATCH in this API; one operator is typing notes while another
// pins a finding and a third resolves.
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

	// The current row is read first for two reasons: it turns an unknown id into a 404 before anything
	// is written.
	current, err := s.incidents.GetIncident(r.Context(), id)
	if err != nil {
		writeIncidentStoreError(w, id, err)
		return
	}

	// lifecycleEvent is set ONLY by a real transition.
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
		updated, moved, updateErr := s.incidents.UpdateIncidentStatus(r.Context(), id, *req.Status, resolvedAt)
		if updateErr != nil {
			writeIncidentStoreError(w, id, updateErr)
			return
		}
		current = updated
		if !moved {
			/* Somebody else made this transition between our read and our write. The row is already
			   where the caller wanted it — so this is a success — but the announcement belongs to
			   whoever performed the move: receivers have no idempotency key to fold two identical
			   resolutions by, and the payload's `at` differs per delivery. */
			lifecycleEvent = ""
		}
	}
	/* The lifecycle delivery is owed the moment the STATUS write commits, so a later field's failure
	   must not cancel it. These are three untransacted store calls: notes failing on a pool blip
	   after the status write left the row resolved, the handler returning 502, and the retry finding
	   *req.Status == current.Status — so lifecycleEvent stayed empty and the incident.resolved
	   webhook was never sent, on that request or on any request afterwards. The transition happened
	   and is durable; the notification follows it, not the rest of the patch.

	   Deferred rather than sent inline so it still carries the FINAL row when the remaining writes
	   do succeed, which is what a receiver wants to read. */
	notified := false
	notify := func() {
		if lifecycleEvent != "" && !notified {
			notified = true
			// WithoutCancel for the same reason as the create path above.
			s.notifyIncident(context.WithoutCancel(r.Context()), lifecycleEvent, &current)
		}
	}

	/* Assigned into a TEMPORARY and promoted only on success. `current, err = …` is a tuple
	   assignment: it overwrites current with the store's zero Incident before err is ever tested, so
	   the deferred notify below shipped an incident with no id, no title and an empty status — a
	   receiver was told that a nameless incident had been resolved. The lost notification this
	   deferral fixes is a smaller harm than a false one. */
	if req.Notes != nil {
		updated, uerr := s.incidents.UpdateIncidentNotes(r.Context(), id, *req.Notes)
		if uerr != nil {
			notify()
			writeIncidentStoreError(w, id, uerr)
			return
		}
		current = updated
	}
	if req.Pinned != nil {
		updated, uerr := s.incidents.UpdateIncidentPinned(r.Context(), id, *req.Pinned)
		if uerr != nil {
			notify()
			writeIncidentStoreError(w, id, uerr)
			return
		}
		current = updated
	}

	// Notified after EVERY named field has been applied, and with the FINAL row.
	notify()

	writeJSON(w, incidentResponseFrom(&current))
}

// decodeIncidentPatch reads and validates a PATCH body in full before the caller writes anything.
func decodeIncidentPatch(w http.ResponseWriter, r *http.Request) (incidentPatchRequest, bool) {
	var req incidentPatchRequest
	/* STRICT, and it matters most here: every field is optional, so a typo decodes to "no field
	   given" and the patch below answers 422 "nothing to change" for a request that named one. */
	if !decodeMutationBody(w, r, &req,
		`an incident patch body must be JSON with any subset of "status", "notes" and "pinned"`) {
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
