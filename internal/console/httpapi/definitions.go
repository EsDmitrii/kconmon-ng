package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// DefinitionService is the subset of *store.DB httpapi needs for CRUD
// /api/v1/checks: the read seam and the write seam together. Same shape and
// same reasoning as TargetService (targets.go) -- a local interface so a test
// can substitute a fake with no database at all, and so this package's
// contract with store cannot widen every time *store.DB grows a method for
// some other caller.
type DefinitionService interface {
	store.DefinitionReader
	store.DefinitionStore
}

var _ DefinitionService = (*store.DB)(nil)

// TopologySource is the one thing the projection guard needs from the
// controller: a live topology snapshot to resolve a definition's agent
// selection against. A local interface rather than *controllerclient.Client
// itself, for the same reason OIDCFlow (server.go) is one -- the projection
// tests supply a FIXED fake topology and must not need a controller, an HTTP
// server, or a leader election to do it.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

var _ TopologySource = (*controllerclient.Client)(nil)

// definitionsUnavailableDetail is served whenever s.definitions is nil. Check
// definitions are CONFIGURATION, exactly like targets, and get NO in-memory
// fallback (Decision 13): a probe definition that vanishes on pod restart is
// worse than one that was never accepted.
const definitionsUnavailableDetail = "check definitions are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/checks"

// projectionUnavailableDetail is POST /api/v1/checks/projection's 503. The
// projected series count is a function of the CURRENT topology, so without a
// controller there is no number to report -- and reporting a made-up one
// would be worse than reporting none, since the UI displays it as the
// enforcement threshold.
const projectionUnavailableDetail = "the projected series count is computed against the live topology: " +
	"set controller.url in the console config (Helm: console.controller.url) to enable /api/v1/checks/projection"

// maxProjectedSeries bounds the continuous external series ONE definition may
// project (Decision 12). 400 is not an arbitrary round number: it is
// checks.maxPairs, the bound the diagnostics runner already applies to one
// run's fan-out, and reusing it keeps the console's two cardinality guards
// telling an operator the same story instead of two different ones.
//
// The bound is PER DEFINITION, not fleet-wide, and that is load-bearing:
// POST /api/v1/checks/projection reports the number for the single definition
// in its body, the UI shows exactly that number, and create/update enforce
// exactly that number -- so the warning can never disagree with the
// enforcement, which is the whole point of the endpoint existing. A
// fleet-wide sum would be a different number from the one the form displays.
const maxProjectedSeries = 400

// protocolsPerDefinition is the "protocols" half of Decision 12's
// `agents x protocols`. A definition names exactly ONE check_type
// (store.DefinitionInput.CheckType, one of tcp|udp|icmp|dns|http|mtr), so it
// contributes one protocol's worth of series per assigned agent today. It is
// named rather than inlined because the wire shape reports it separately: the
// day a definition can probe several protocols from one spec, this becomes a
// function of the definition and nothing about projectionResponse changes.
const protocolsPerDefinition = 1

// errTopologyUnavailable is projectDefinition's "no number can be computed"
// signal -- no TopologySource wired, or the controller did not answer. It is
// deliberately distinct from a validation failure: the projection endpoint
// turns it into 503/502, while create/update fail OPEN on it (see
// enforceProjection).
var errTopologyUnavailable = errors.New("httpapi: topology unavailable for projection")

// tooManySeriesMsg opens the projection guard's 422 detail, echoing
// checks.ErrTooManyPairs's wording. Deliberately a plain string, not an
// error sentinel: it is only ever interpolated into projectionDetail — never
// returned, wrapped, or matched with errors.Is — and declaring it as an
// error would invite exactly that misreading.
const tooManySeriesMsg = "too many projected series"

// definitionsUnavailable answers 503 and reports true when no
// DefinitionService is wired, the same shape targetsUnavailable uses.
func (s *Server) definitionsUnavailable(w http.ResponseWriter) bool {
	if s.definitions == nil {
		writeProblem(w, http.StatusServiceUnavailable, "check definitions not available", definitionsUnavailableDetail)
		return true
	}
	return false
}

// definitionResponse is one check definition on the wire. Params is passed
// through as raw JSON (always an object -- store writes {} for an absent
// one), never re-marshalled through a map, so key order stays whatever was
// stored -- same contract targetResponse.Labels has.
type definitionResponse struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	SourceSelection     string          `json:"sourceSelection"`
	DestinationKind     string          `json:"destinationKind"`
	DestinationTargetID string          `json:"destinationTargetId"`
	DestinationAddress  string          `json:"destinationAddress"`
	CheckType           string          `json:"checkType"`
	Plane               string          `json:"plane"`
	Params              json.RawMessage `json:"params"`
	Enabled             bool            `json:"enabled"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
}

func definitionResponseFrom(d *store.Definition) definitionResponse {
	params := d.Params
	if len(params) == 0 {
		// Defensive, exactly as targetResponseFrom is for labels: a nil here
		// would marshal as JSON null and the frontend reads params as an
		// object.
		params = json.RawMessage(`{}`)
	}
	return definitionResponse{
		ID: d.ID, Name: d.Name, SourceSelection: d.SourceSelection,
		DestinationKind: d.DestinationKind, DestinationTargetID: d.DestinationTargetID,
		DestinationAddress: d.DestinationAddress, CheckType: d.CheckType, Plane: d.Plane,
		Params: params, Enabled: d.Enabled, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// definitionsListResponse is GET /api/v1/checks's body -- the same
// keyset-cursor shape as targetsListResponse.
type definitionsListResponse struct {
	Definitions []definitionResponse `json:"definitions"`
	NextCursor  string               `json:"nextCursor"`
}

// definitionRequest is POST /api/v1/checks's, PUT /api/v1/checks/{id}'s and
// POST /api/v1/checks/projection's body. The first two are FULL replaces,
// matching store.DefinitionInput's own contract: an omitted field means
// "empty", never "leave as-is". The projection endpoint reads the same shape
// so the UI can send the form it is about to submit, unchanged.
type definitionRequest struct {
	Name                string          `json:"name"`
	SourceSelection     string          `json:"sourceSelection"`
	DestinationKind     string          `json:"destinationKind"`
	DestinationTargetID string          `json:"destinationTargetId,omitempty"`
	DestinationAddress  string          `json:"destinationAddress,omitempty"`
	CheckType           string          `json:"checkType"`
	Plane               string          `json:"plane"`
	Params              json.RawMessage `json:"params,omitempty"`
	Enabled             bool            `json:"enabled"`
}

// decodeDefinitionRequest reads and validates a create/update/projection
// body. A body that is not JSON at all is a 400 (malformed request); a
// well-formed body whose VALUES break a definition rule is a 422 -- the same
// distinction decodeTargetRequest draws, for the same reason, and validation
// is delegated to store.DefinitionInput.Validate rather than reimplemented
// here so the two copies cannot drift.
func decodeDefinitionRequest(w http.ResponseWriter, r *http.Request) (store.DefinitionInput, bool) {
	var req definitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`body must be JSON with "name", "sourceSelection" (all|per-zone|one-per-zone), `+
				`"destinationKind" (node|target|adhoc), "checkType" (tcp|udp|icmp|dns|http|mtr), "plane", `+
				`an optional "params" object and an optional "enabled" flag`)
		return store.DefinitionInput{}, false
	}
	in := store.DefinitionInput{
		Name: req.Name, SourceSelection: req.SourceSelection,
		DestinationKind: req.DestinationKind, DestinationTargetID: req.DestinationTargetID,
		DestinationAddress: req.DestinationAddress, CheckType: req.CheckType, Plane: req.Plane,
		Params: req.Params, Enabled: req.Enabled,
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid check definition", publicValidationDetail(err))
		return store.DefinitionInput{}, false
	}
	return in, true
}

// definitionIDFrom resolves the {id} path parameter, answering 404 and
// reporting false for anything that is not a canonical UUID -- the same
// M3-follow-up-#5 guard targetIDFrom documents at length: without it a
// malformed id reaches pgx, which fails while ENCODING the parameter, and the
// catch-all would report 502 for something that simply cannot exist.
func definitionIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "check definition not found", "no check definition with that id")
		return "", false
	}
	return id, true
}

// writeDefinitionStoreError maps a DefinitionService error to a response, the
// same three-sentinel contract writeTargetStoreError handles.
//
// ErrNotFound is the one that carries extra weight here: store's
// DefinitionReader doc comment states it is ALSO returned when
// DestinationTargetID names no target (the FK violation), and that case is a
// rejected field value in the request body, not a missing definition. The
// caller therefore tells this function which of the two it is -- see
// writeDefinitionCreateError.
func writeDefinitionStoreError(w http.ResponseWriter, name, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "check definition not found", "no check definition with that id")
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid check definition",
			"definition: name "+strconv.Quote(name)+" is already taken; definition names are unique")
	case errors.Is(err, store.ErrInUse):
		writeProblem(w, http.StatusConflict, "check definition in use",
			"check definition "+strconv.Quote(id)+" is still referenced; remove what references it first")
	default:
		slog.Error("httpapi: definition store call failed", "definition", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "check definitions unavailable", "failed to reach the check definitions store")
	}
}

// writeDefinitionCreateError is writeDefinitionStoreError for the CREATE
// path, where ErrNotFound cannot mean "this definition does not exist" -- it
// has no id yet. It can only mean destinationTargetId names no target, which
// is a rejected FIELD VALUE and therefore 422, exactly as a duplicate name
// is. Answering 404 here would tell a client the endpoint it just POSTed to
// is missing.
func writeDefinitionCreateError(w http.ResponseWriter, in *store.DefinitionInput, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid check definition",
			"definition: destination target id "+strconv.Quote(in.DestinationTargetID)+" names no target")
		return
	}
	writeDefinitionStoreError(w, in.Name, "", err)
}

// handleChecksList serves one page of check definitions, newest first, behind
// an opaque keyset cursor, optionally filtered by ?targetId= and ?enabled=.
func (s *Server) handleChecksList(w http.ResponseWriter, r *http.Request) {
	if s.definitionsUnavailable(w) {
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
	// Same pre-check the {id} routes apply, extended to the query filter: a
	// typo'd targetId is the CLIENT's error (400), never "the store is down"
	// (502) — the exact class M3 follow-up #5 closed for path params.
	if tid := q.Get("targetId"); tid != "" {
		if _, err := uuid.Parse(tid); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid targetId", "targetId must be a UUID")
			return
		}
	}

	page, err := s.definitions.ListDefinitions(r.Context(), store.DefinitionFilter{
		TargetID: q.Get("targetId"),
		Enabled:  parseOptionalBool(q.Get("enabled")),
		Cursor:   cursor,
		// The [1,500]/100 bound is store's clampLimit, mirrored per listing;
		// the helpers live in targets.go only by birth order.
		Limit: clampTargetsLimit(parseTargetsLimit(q.Get("limit"))),
	})
	if err != nil {
		slog.Error("list check definitions failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "check definitions unavailable", "failed to query check definitions")
		return
	}

	out := make([]definitionResponse, 0, len(page.Definitions))
	for i := range page.Definitions {
		out = append(out, definitionResponseFrom(&page.Definitions[i]))
	}
	writeJSON(w, definitionsListResponse{Definitions: out, NextCursor: page.NextCursor})
}

// parseOptionalBool maps a query parameter to store's *bool "nil means both"
// filter convention. Anything that is not exactly "true" or "false" -- including
// the empty string -- is treated as UNSET rather than as a 400, the same
// leniency parseTargetsLimit applies to an unparseable ?limit=.
func parseOptionalBool(raw string) *bool {
	switch raw {
	case "true":
		v := true
		return &v
	case "false":
		v := false
		return &v
	default:
		return nil
	}
}

// handleChecksCreate creates one check definition: 201 with a Location header
// naming the new row, matching handleTargetsCreate.
//
// The projection guard runs BEFORE the store call, and only for a definition
// arriving enabled: an operator drafting something over the limit must be
// able to save it disabled (Decision 12), so the gated action is enabling,
// not saving.
func (s *Server) handleChecksCreate(w http.ResponseWriter, r *http.Request) {
	if s.definitionsUnavailable(w) {
		return
	}
	in, ok := decodeDefinitionRequest(w, r)
	if !ok {
		return
	}
	if !s.enforceProjection(w, r, &in) {
		return
	}

	def, err := s.definitions.CreateDefinition(r.Context(), in)
	if err != nil {
		writeDefinitionCreateError(w, &in, err)
		return
	}

	w.Header().Set("Location", "/api/v1/checks/"+def.ID)
	writeJSONStatus(w, http.StatusCreated, definitionResponseFrom(&def))
}

// handleChecksGet serves one check definition. An unknown id and a malformed
// one are both 404 (definitionIDFrom).
func (s *Server) handleChecksGet(w http.ResponseWriter, r *http.Request) {
	if s.definitionsUnavailable(w) {
		return
	}
	id, ok := definitionIDFrom(w, r)
	if !ok {
		return
	}

	def, err := s.definitions.GetDefinition(r.Context(), id)
	if err != nil {
		writeDefinitionStoreError(w, "", id, err)
		return
	}
	writeJSON(w, definitionResponseFrom(&def))
}

// handleChecksUpdate replaces one check definition in full and answers 200
// with the stored row, so a client sees the server's own view rather than an
// echo of what it sent. Same projection gate as create.
func (s *Server) handleChecksUpdate(w http.ResponseWriter, r *http.Request) {
	if s.definitionsUnavailable(w) {
		return
	}
	id, ok := definitionIDFrom(w, r)
	if !ok {
		return
	}
	in, ok := decodeDefinitionRequest(w, r)
	if !ok {
		return
	}
	if !s.enforceProjection(w, r, &in) {
		return
	}

	def, err := s.definitions.UpdateDefinition(r.Context(), id, in)
	if err != nil {
		writeDefinitionStoreError(w, in.Name, id, err)
		return
	}
	writeJSON(w, definitionResponseFrom(&def))
}

// handleChecksDelete removes one check definition. Its schedules go with it:
// check_schedules.definition_id is ON DELETE CASCADE (migration 00004), which
// store.DefinitionStore's own doc comment states, so there is no second call
// to make and no orphan to clean up here.
func (s *Server) handleChecksDelete(w http.ResponseWriter, r *http.Request) {
	if s.definitionsUnavailable(w) {
		return
	}
	id, ok := definitionIDFrom(w, r)
	if !ok {
		return
	}

	if err := s.definitions.DeleteDefinition(r.Context(), id); err != nil {
		writeDefinitionStoreError(w, "", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// projectionResponse is POST /api/v1/checks/projection's 200 body. Agents is
// the count the selection resolves to against the CURRENT topology, so the
// same definition can project differently as the cluster scales -- which is
// exactly the fact the operator needs to see before enabling it.
type projectionResponse struct {
	Agents    int  `json:"agents"`
	Protocols int  `json:"protocols"`
	Series    int  `json:"series"`
	Limit     int  `json:"limit"`
	OverLimit bool `json:"overLimit"`
}

// handleChecksProjection reports what a definition WOULD project, persisting
// nothing. It is the server-side arbiter of Decision 12 and the number the UI
// displays, so the warning can never disagree with the enforcement: the
// enabled-definition gate in create/update calls the very same function
// against the very same limit.
//
// It is deliberately gated on checks:write, not checks:read: the body is a
// draft definition, and a caller who cannot create one has nothing to ask
// this question about.
func (s *Server) handleChecksProjection(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeDefinitionRequest(w, r)
	if !ok {
		return
	}

	proj, err := s.projectDefinition(r.Context(), &in)
	switch {
	case errors.Is(err, errTopologyUnavailable):
		writeProblem(w, http.StatusServiceUnavailable, "topology not available", projectionUnavailableDetail)
		return
	case err != nil:
		slog.Error("httpapi: projection topology fetch failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "controller unavailable", "failed to read the topology the projection is computed against")
		return
	}
	writeJSON(w, proj)
}

// projectDefinition resolves in's agent selection against the live topology
// and returns the projected series count. No store access and no side
// effects: this is the same number whether it is asked for by the projection
// endpoint or by the create/update gate.
func (s *Server) projectDefinition(ctx context.Context, in *store.DefinitionInput) (projectionResponse, error) {
	if s.topology == nil {
		return projectionResponse{}, errTopologyUnavailable
	}
	topo, err := s.topology.Topology(ctx)
	if err != nil {
		return projectionResponse{}, fmt.Errorf("httpapi: projection: %w", err)
	}

	agents := projectedAgents(in.SourceSelection, topo)
	series := agents * protocolsPerDefinition
	return projectionResponse{
		Agents:    agents,
		Protocols: protocolsPerDefinition,
		Series:    series,
		Limit:     maxProjectedSeries,
		OverLimit: series > maxProjectedSeries,
	}, nil
}

// projectedAgents counts the agents a selection resolves to against topo.
//
// "all" and "per-zone" both resolve to EVERY agent -- per-zone groups the
// same agent set by zone rather than shrinking it (Plan Decision 11 and Task
// 5: "per-zone yields every agent grouped, one-per-zone yields exactly one
// per zone"), so grouping changes how the work is dispatched, not how many
// series come out of it. "one-per-zone" is the only mode that shrinks the
// count, and that is precisely why it is the default: it is bounded by ZONE
// count, and node count is the number that grows without an operator
// noticing.
//
// Zoneless agents are never dropped: under "all"/"per-zone" each counts
// individually, and under "one-per-zone" they collectively form ONE
// empty-string bucket (N zoneless agents -> one representative), the same
// rule the "" zone key gives any other zone. Dropping them would
// under-report the projection and make the guard lie in the unsafe
// direction. A nil topo yields 0.
func projectedAgents(selection string, topo *controllerclient.Topology) int {
	if topo == nil {
		return 0
	}
	if selection != "one-per-zone" {
		return len(topo.Agents)
	}
	zones := make(map[string]struct{}, len(topo.Agents))
	for i := range topo.Agents {
		zones[topo.Agents[i].Zone] = struct{}{}
	}
	return len(zones)
}

// enforceProjection is create/update's half of Decision 12. It answers 422
// and reports false when in arrives ENABLED and would project past
// maxProjectedSeries; it reports true (let the write through) in every other
// case, including a disabled definition -- which is never projected at all,
// because an operator drafting one must not be blocked.
//
// It fails OPEN when the topology cannot be read: a controller outage must
// not become a configuration-write outage, the same direction Decision 8
// chose for the rate limiter ("a Valkey outage must not become a login
// outage"). The consequence is bounded and visible: the number is a
// cardinality bound, not a security boundary, the WARN below names the
// definition that slipped through, and Task 17's reconciler recomputes the
// fleet-wide projection on every tick with a gauge to alert on.
func (s *Server) enforceProjection(w http.ResponseWriter, r *http.Request, in *store.DefinitionInput) bool {
	if !in.Enabled {
		return true
	}
	proj, err := s.projectDefinition(r.Context(), in)
	if err != nil {
		// Decision 8: fail open AND count it — a controller outage must not
		// become a config-write outage, but a bypassed guard must be
		// alertable, not just a log line.
		s.metrics.ProjectionGuardFailOpen.WithLabelValues().Inc()
		slog.Warn("httpapi: projection guard could not read the topology, allowing the write", //nolint:gosec // G706: structured slog fields, not string-built log injection
			"definition", in.Name, "error", err)
		return true
	}
	if !proj.OverLimit {
		return true
	}
	writeProblem(w, http.StatusUnprocessableEntity, "invalid check definition", projectionDetail(in.SourceSelection, proj))
	return false
}

// projectionDetail spells the arithmetic out in full, because the number on
// its own tells an operator nothing about which knob to turn: it names the
// product, both factors, the limit, and the one selection change that
// actually shrinks the left factor. The sentinel's own text leads, in the
// shape checks.ErrTooManyPairs's "computed %d pairs, limit %d" detail uses.
func projectionDetail(selection string, proj projectionResponse) string {
	detail := fmt.Sprintf("definition: %s: enabling this definition projects %d continuous external series "+
		"(%d agents x %d protocols), limit %d",
		tooManySeriesMsg, proj.Series, proj.Agents, proj.Protocols, proj.Limit)
	if selection != "one-per-zone" {
		return detail + `; set "sourceSelection":"one-per-zone" to bound it by zone count instead of node count, ` +
			`or save the definition with "enabled":false`
	}
	return detail + `; reduce the fleet's zone count or save the definition with "enabled":false`
}
