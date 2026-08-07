package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// TargetService is the subset of *store.DB httpapi needs for CRUD
// /api/v1/targets: the read seam and the write seam together. A local
// interface in the shape of RunService (runs.go) rather than *store.DB
// itself, so a test can substitute a fake with no database at all, and so
// this package's contract with store cannot widen every time *store.DB grows
// a method for some other caller.
type TargetService interface {
	store.TargetReader
	store.TargetStore
}

var _ TargetService = (*store.DB)(nil)

// targetsUnavailableDetail is served whenever s.targets is nil. Unlike
// Runner -- which falls back to checks.NewMemoryStore() so on-demand runs
// still work with the database off (Decision 15) -- targets are
// CONFIGURATION and get NO in-memory fallback (Decision 13): a probe
// definition that vanishes on pod restart is worse than one that was never
// accepted, so the only honest answer is 503 naming the value that turns
// persistence on.
const targetsUnavailableDetail = "targets are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/targets"

// Limit bounds for GET /api/v1/targets, mirroring
// runsMinLimit/runsMaxLimit/runsDefaultLimit (runs.go), themselves a
// hand-kept-in-sync copy of store's own clampLimit -- same reasoning, store
// keeps clampLimit private.
const (
	targetsMinLimit     = 1
	targetsMaxLimit     = 500
	targetsDefaultLimit = 100
)

// targetsUnavailable answers 503 and reports true when no TargetService is
// wired, the same shape tokensUnavailable (tokens.go) uses.
func (s *Server) targetsUnavailable(w http.ResponseWriter) bool {
	if s.targets == nil {
		writeProblem(w, http.StatusServiceUnavailable, "targets not available", targetsUnavailableDetail)
		return true
	}
	return false
}

// targetResponse is one target on the wire. Labels is passed through as raw
// JSON (always an object -- store writes {} for an absent one), never
// re-marshalled through a map, so key order stays whatever was stored.
type targetResponse struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Address   string          `json:"address"`
	Labels    json.RawMessage `json:"labels"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

func targetResponseFrom(t *store.Target) targetResponse {
	labels := t.Labels
	if len(labels) == 0 {
		// Defensive: *store.DB always stores {} (orEmptyJSON), but a nil here
		// would marshal as JSON null and the frontend reads labels as an
		// object.
		labels = json.RawMessage(`{}`)
	}
	return targetResponse{
		ID: t.ID, Name: t.Name, Kind: t.Kind, Address: t.Address, Labels: labels,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// targetsListResponse is GET /api/v1/targets's body -- the same keyset-cursor
// shape as eventsResponse/auditResponse/runsListResponse.
type targetsListResponse struct {
	Targets    []targetResponse `json:"targets"`
	NextCursor string           `json:"nextCursor"`
}

// targetRequest is POST /api/v1/targets's and PUT /api/v1/targets/{id}'s
// body. Both are FULL replaces, matching store.TargetInput's own contract:
// an omitted field means "empty", never "leave as-is".
type targetRequest struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Address string          `json:"address"`
	Labels  json.RawMessage `json:"labels,omitempty"`
}

// decodeTargetRequest reads and validates a create/update body. A body that
// is not JSON at all is a 400 (malformed request); a well-formed body whose
// VALUES break a target rule is a 422 -- the same distinction handleRunsCreate
// draws between an unparseable spec and one refused for what it would do.
//
// Validation is delegated to store.TargetInput.Validate rather than
// reimplemented here, deliberately: the name charset rule exists because
// targets.name becomes a Prometheus label value (migration 00004), and a
// second copy of that rule in httpapi is a second copy that can drift from
// the one the database layer actually enforces. The cost is that the detail
// string is store's, so the internal package prefix is trimmed before it
// reaches the wire.
func decodeTargetRequest(w http.ResponseWriter, r *http.Request) (store.TargetInput, bool) {
	var req targetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`body must be JSON with "name", "kind" ("host" or "url"), "address", and an optional "labels" object`)
		return store.TargetInput{}, false
	}
	in := store.TargetInput{Name: req.Name, Kind: req.Kind, Address: req.Address, Labels: req.Labels}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid target", publicValidationDetail(err))
		return store.TargetInput{}, false
	}
	return in, true
}

// publicValidationDetail strips store's own package prefix off a validation
// error so an API client is told "target: kind ... must be one of host, url"
// instead of a message that names an internal Go package.
func publicValidationDetail(err error) string {
	return strings.TrimPrefix(err.Error(), "store: ")
}

// targetIDFrom resolves the {id} path parameter, answering 404 and reporting
// false for anything that is not a canonical UUID.
//
// M3 follow-up #5: without this guard a malformed id travels all the way to
// pgx, which rejects it while encoding the query parameter, and the handler's
// catch-all maps that to 502 -- telling a client the gateway is broken when
// in fact it asked for something that cannot exist. An unparseable id and an
// unknown one are indistinguishable to a caller, so both are 404. The check
// lives here rather than in store because store's parse failure is a legitimate
// error for its own callers; only the HTTP layer knows it means "not found".
func targetIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "target not found", "no target with that id")
		return "", false
	}
	return id, true
}

// writeTargetStoreError maps a TargetService error to a response. The three
// sentinels are the whole contract (store/targets.go's TargetReader doc
// comment); anything else is an opaque backend failure and becomes 502, the
// same shape handleRunsGet uses for an unexpected store error.
//
// ErrAlreadyExists is 422, NOT 409: a duplicate name is a rejected FIELD
// VALUE in an otherwise well-formed body, indistinguishable from a bad kind
// as far as the client's fix goes (change the name and resend). 409 is
// reserved for ErrInUse, which is about the state of OTHER rows, not about
// anything in this request.
func writeTargetStoreError(w http.ResponseWriter, name, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "target not found", "no target with that id")
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid target",
			"target: name "+strconv.Quote(name)+" is already taken; target names are unique")
	case errors.Is(err, store.ErrInUse):
		// The referencing rows cannot be enumerated from here: TargetService
		// is TargetReader+TargetStore, and definitions live behind
		// DefinitionReader, which this handler deliberately does not take a
		// dependency on. So the detail names the resource kind and the query
		// that lists them (Task 12's GET /api/v1/checks?targetId=), which is
		// what an operator needs to act.
		writeProblem(w, http.StatusConflict, "target in use",
			"one or more check definitions still reference target "+strconv.Quote(id)+
				"; delete or re-point those definitions first (they are listable by target id)")
	default:
		slog.Error("httpapi: target store call failed", "target", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "targets unavailable", "failed to reach the targets store")
	}
}

// handleTargetsList serves one page of targets, newest first, behind an
// opaque keyset cursor, optionally filtered by ?kind=.
func (s *Server) handleTargetsList(w http.ResponseWriter, r *http.Request) {
	if s.targetsUnavailable(w) {
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

	page, err := s.targets.ListTargets(r.Context(), store.TargetFilter{
		Kind:   q.Get("kind"),
		Cursor: cursor,
		Limit:  clampTargetsLimit(parseTargetsLimit(q.Get("limit"))),
	})
	if err != nil {
		slog.Error("list targets failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "targets unavailable", "failed to query targets")
		return
	}

	out := make([]targetResponse, 0, len(page.Targets))
	for i := range page.Targets {
		out = append(out, targetResponseFrom(&page.Targets[i]))
	}
	writeJSON(w, targetsListResponse{Targets: out, NextCursor: page.NextCursor})
}

// parseTargetsLimit mirrors parseRunsLimit: an unparseable ?limit= is treated
// as unset, never a 400 -- limit is documented to clamp.
func parseTargetsLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// clampTargetsLimit mirrors clampRunsLimit.
func clampTargetsLimit(n int) int {
	switch {
	case n == 0:
		return targetsDefaultLimit
	case n < targetsMinLimit:
		return targetsMinLimit
	case n > targetsMaxLimit:
		return targetsMaxLimit
	default:
		return n
	}
}

// handleTargetsCreate creates one target: 201 with a Location header naming
// the new row, matching POST /api/v1/runs's create-and-point-at-it shape
// (which is 202 only because a run is asynchronous; a target is not).
func (s *Server) handleTargetsCreate(w http.ResponseWriter, r *http.Request) {
	if s.targetsUnavailable(w) {
		return
	}
	in, ok := decodeTargetRequest(w, r)
	if !ok {
		return
	}

	target, err := s.targets.CreateTarget(r.Context(), in)
	if err != nil {
		writeTargetStoreError(w, in.Name, "", err)
		return
	}

	w.Header().Set("Location", "/api/v1/targets/"+target.ID)
	writeJSONStatus(w, http.StatusCreated, targetResponseFrom(&target))
}

// handleTargetsGet serves one target. An unknown id and a malformed one are
// both 404 (targetIDFrom).
func (s *Server) handleTargetsGet(w http.ResponseWriter, r *http.Request) {
	if s.targetsUnavailable(w) {
		return
	}
	id, ok := targetIDFrom(w, r)
	if !ok {
		return
	}

	target, err := s.targets.GetTarget(r.Context(), id)
	if err != nil {
		writeTargetStoreError(w, "", id, err)
		return
	}
	writeJSON(w, targetResponseFrom(&target))
}

// handleTargetsUpdate replaces one target in full and answers 200 with the
// stored row -- so a client sees the server's own view (updatedAt, normalized
// labels), not an echo of what it sent.
func (s *Server) handleTargetsUpdate(w http.ResponseWriter, r *http.Request) {
	if s.targetsUnavailable(w) {
		return
	}
	id, ok := targetIDFrom(w, r)
	if !ok {
		return
	}
	in, ok := decodeTargetRequest(w, r)
	if !ok {
		return
	}

	target, err := s.targets.UpdateTarget(r.Context(), id, in)
	if err != nil {
		writeTargetStoreError(w, in.Name, id, err)
		return
	}
	writeJSON(w, targetResponseFrom(&target))
}

// handleTargetsDelete removes one target, or refuses with 409 while any
// check definition still points at it (ON DELETE RESTRICT, migration 00004).
func (s *Server) handleTargetsDelete(w http.ResponseWriter, r *http.Request) {
	if s.targetsUnavailable(w) {
		return
	}
	id, ok := targetIDFrom(w, r)
	if !ok {
		return
	}

	if err := s.targets.DeleteTarget(r.Context(), id); err != nil {
		writeTargetStoreError(w, "", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
