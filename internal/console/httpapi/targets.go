package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// TargetService is the subset of *store.DB httpapi needs for CRUD /api/v1/targets; a local
// interface in the shape of RunService (runs.go) rather than *store.DB itself.
type TargetService interface {
	store.TargetReader
	store.TargetStore
}

var _ TargetService = (*store.DB)(nil)

// targetsUnavailableDetail is served whenever s.targets is nil; unlike Runner -- which falls back
// to checks.NewMemoryStore so on-demand runs still work with the database off.
const targetsUnavailableDetail = "targets are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/targets"

// Limit bounds for GET /api/v1/targets, mirroring runsMinLimit/runsMaxLimit/runsDefaultLimit
// (runs.go).
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

// decodeTargetRequest reads and validates a create/update body; a body that is not JSON at all is a
// 400 (malformed request).
func decodeTargetRequest(w http.ResponseWriter, r *http.Request) (store.TargetInput, bool) {
	var req targetRequest
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", unknownFieldDetail(err,
			`body must be JSON with "name", "kind" ("host" or "url"), "address", and an optional "labels" object`))
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

// targetIDFrom resolves the {id} path parameter; without this guard a malformed id travels all the
// way to pgx.
func targetIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "target not found", "no target with that id")
		return "", false
	}
	return id, true
}

// writeTargetStoreError maps a TargetService error to a response; ErrAlreadyExists is 422, NOT 409:
// a duplicate name is a rejected FIELD VALUE in an otherwise well-formed body.
func writeTargetStoreError(w http.ResponseWriter, name, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "target not found", "no target with that id")
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid target",
			"target: name "+strconv.Quote(name)+" is already taken; target names are unique")
	case errors.Is(err, store.ErrInUse):
		// Only handleTargetsDelete can produce this, and it answers ErrInUse itself with the
		// names; this branch is the fallback for a caller that could not enumerate them.
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

	/* A NUL here is fatal to pgx and used to surface as 502 "targets unavailable"; and a kind that
	   is not one of the two the schema knows can only ever match nothing, so it is a 400 rather than
	   an empty page that reads as "you have no targets". */
	if rejectControlChars(w, "kind", q.Get("kind")) {
		return
	}
	if kind := q.Get("kind"); kind != "" && kind != "host" && kind != "url" {
		writeProblem(w, http.StatusBadRequest, "invalid kind", `kind must be "host" or "url"`)
		return
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
	// Refused here rather than discovered as a timeout on every later probe.
	if s.refuseUnreachableTarget(w, r, in.Address) {
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
	// An edit can move a reachable target out of the allowlist just as a create can.
	if s.refuseUnreachableTarget(w, r, in.Address) {
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
		if errors.Is(err, store.ErrInUse) {
			s.writeTargetInUse(r.Context(), w, id)
			return
		}
		writeTargetStoreError(w, "", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// inUseDefinitionsListed caps how many referencing definitions the 409 spells out; past this the
// message names the rest by count rather than turning into a wall of text.
const inUseDefinitionsListed = 10

// writeTargetInUse answers the delete refusal in the terms the operator works in -- the target's
// NAME and the names of the definitions still pointing at it -- falling back to the id-only
// wording whenever a lookup cannot be made.
func (s *Server) writeTargetInUse(ctx context.Context, w http.ResponseWriter, id string) {
	label := strconv.Quote(id)
	if t, err := s.targets.GetTarget(ctx, id); err == nil && t.Name != "" {
		label = strconv.Quote(t.Name)
	}

	names, more := s.referencingDefinitionNames(ctx, id)
	if len(names) == 0 {
		writeProblem(w, http.StatusConflict, "target in use",
			"one or more check definitions still reference target "+label+
				"; delete or re-point those definitions first")
		return
	}

	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	listed := strings.Join(quoted, ", ")
	if more > 0 {
		listed += " and " + strconv.Itoa(more) + " more"
	}
	writeProblem(w, http.StatusConflict, "target in use",
		"target "+label+" is still referenced by check "+pluralDefinitions(len(names)+more)+" "+listed+
			"; delete or re-point them first")
}

// referencingDefinitionNames lists the definitions pointing at target id, capped at
// inUseDefinitionsListed; more counts the ones past the cap. An absent or failing
// DefinitionService yields no names at all, which the caller reads as "cannot enumerate".
func (s *Server) referencingDefinitionNames(ctx context.Context, id string) (names []string, more int) {
	if s.definitions == nil {
		return nil, 0
	}
	page, err := s.definitions.ListDefinitions(ctx, store.DefinitionFilter{
		TargetID: id,
		// One past the cap, so "and N more" can be honest about there being more at all.
		Limit: inUseDefinitionsListed + 1,
	})
	if err != nil {
		slog.Error("httpapi: list definitions for in-use target failed", "target", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return nil, 0
	}
	for i := range page.Definitions {
		names = append(names, page.Definitions[i].Name)
	}
	sort.Strings(names)
	if len(names) > inUseDefinitionsListed {
		more = len(names) - inUseDefinitionsListed
		names = names[:inUseDefinitionsListed]
	}
	return names, more
}

func pluralDefinitions(n int) string {
	if n == 1 {
		return "definition"
	}
	return "definitions"
}
