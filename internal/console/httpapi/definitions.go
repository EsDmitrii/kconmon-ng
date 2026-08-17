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

// DefinitionService is the subset of *store.DB httpapi needs for CRUD /api/v1/checks; same shape
// and same reasoning as TargetService (targets.go).
type DefinitionService interface {
	store.DefinitionReader
	store.DefinitionStore
}

var _ DefinitionService = (*store.DB)(nil)

// TopologySource is the one thing the projection guard needs from the controller; a local interface
// rather than *controllerclient.Client itself.
type TopologySource interface {
	Topology(ctx context.Context) (*controllerclient.Topology, error)
}

var _ TopologySource = (*controllerclient.Client)(nil)

// definitionsUnavailableDetail is served whenever s.definitions is nil; check definitions are
// CONFIGURATION, exactly like targets, and get NO in-memory fallback.
const definitionsUnavailableDetail = "check definitions are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/checks"

// projectionUnavailableDetail is POST /api/v1/checks/projection's 503. The projected series count
// is a function of the CURRENT topology.
const projectionUnavailableDetail = "the projected series count is computed against the live topology: " +
	"set controller.url in the console config (Helm: console.controller.url) to enable /api/v1/checks/projection"

// maxProjectedSeries bounds the continuous external series ONE definition may project; the bound is
// PER DEFINITION, not fleet-wide, and that is load-bearing.
const maxProjectedSeries = 400

// protocolsPerDefinition is the "protocols" half; it is named rather than inlined because the wire
// shape reports it separately.
const protocolsPerDefinition = 1

// errTopologyUnavailable is projectDefinition's "no number can be computed" signal -- no
// TopologySource wired.
var errTopologyUnavailable = errors.New("httpapi: topology unavailable for projection")

// tooManySeriesMsg opens the projection guard's 422 detail; deliberately a plain string, not an
// error sentinel.
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

// definitionResponse is one check definition on the wire; params is passed through as raw JSON
// (always an object -- store writes {} for an absent one).
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

// definitionRequest is POST /api/v1/checks's; the first two are FULL replaces, matching
// store.DefinitionInput's own contract.
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

// decodeDefinitionRequest reads and validates a create/update/projection body; a body that is not
// JSON at all is a 400 (malformed request).
func decodeDefinitionRequest(w http.ResponseWriter, r *http.Request) (store.DefinitionInput, bool) {
	var req definitionRequest
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", unknownFieldDetail(err,
			`body must be JSON with "name", "sourceSelection" (all|per-zone|one-per-zone), `+
				`"destinationKind" (node|target|adhoc), "checkType" (tcp|udp|icmp|dns|http|mtr), "plane", `+
				`an optional "params" object and an optional "enabled" flag`))
		return store.DefinitionInput{}, false
	}
	/* A destinationTargetId that is not a UUID is a CLIENT error, and it used to reach the store as
	   one — where pgx refused the parse and the handler reported 502 "check definitions unavailable",
	   i.e. a typo in one field presented as an outage of the whole subsystem. Same pre-check runs.go
	   and the list route already make. */
	if req.DestinationTargetID != "" {
		if _, err := uuid.Parse(req.DestinationTargetID); err != nil {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid check definition",
				"destinationTargetId must be a UUID naming a saved target")
			return store.DefinitionInput{}, false
		}
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

// definitionIDFrom resolves the {id} path parameter.
func definitionIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "check definition not found", "no check definition with that id")
		return "", false
	}
	return id, true
}

// writeDefinitionStoreError maps a DefinitionService error to a response.
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

// writeDefinitionCreateError is writeDefinitionStoreError for the CREATE path.
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
	// Same pre-check the {id} routes apply, extended to the query filter: a typo'd targetId is the
	// CLIENT's error (400).
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

// parseOptionalBool maps a query parameter to store's *bool "nil means both" filter convention;
// anything that is not exactly "true" or "false" -- including the empty string.
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

// handleChecksCreate creates one check definition: 201 with a Location header naming the new row;
// the projection guard runs BEFORE the store call, and only for a definition arriving enabled.
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
	if s.refuseUnrunnableDefinition(w, r, &in) {
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
	if s.refuseUnrunnableDefinition(w, r, &in) {
		return
	}

	def, err := s.definitions.UpdateDefinition(r.Context(), id, in)
	if err != nil {
		/* TWO different things arrive here as ErrNotFound, and telling them apart matters.
		   The store folds "no such definition row" and "the destination target FK does not resolve"
		   into one sentinel, so a body naming a target that does not exist answered 404 "no check
		   definition with that id" — about a definition that is sitting right there, untouched. The
		   operator was told their check had disappeared when the real problem was one field of their
		   own body. The create path has always distinguished the two; this re-read is what lets the
		   update path do the same, and it only runs on the error branch. */
		if errors.Is(err, store.ErrNotFound) {
			if _, gerr := s.definitions.GetDefinition(r.Context(), id); gerr == nil {
				writeDefinitionCreateError(w, &in, err)
				return
			}
		}
		writeDefinitionStoreError(w, in.Name, id, err)
		return
	}
	writeJSON(w, definitionResponseFrom(&def))
}

// handleChecksDelete removes one check definition.
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

// projectionResponse is POST /api/v1/checks/projection's 200 body.
type projectionResponse struct {
	Agents    int  `json:"agents"`
	Protocols int  `json:"protocols"`
	Series    int  `json:"series"`
	Limit     int  `json:"limit"`
	OverLimit bool `json:"overLimit"`
}

// handleChecksProjection reports what a definition WOULD project, persisting nothing.
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

// projectDefinition resolves in's agent selection against the live topology and returns the
// projected series count.
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

// projectedAgents counts the agents a selection resolves to against topo; "all" and "per-zone" both
// resolve to EVERY agent.
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

// enforceProjection is create/update's half; it fails OPEN when the topology cannot be read.
func (s *Server) enforceProjection(w http.ResponseWriter, r *http.Request, in *store.DefinitionInput) bool {
	if !in.Enabled {
		return true
	}
	proj, err := s.projectDefinition(r.Context(), in)
	if err != nil {
		// fail open AND count it — a controller outage must not become a config-write outage, but a
		// bypassed guard must be alertable.
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

// projectionDetail spells the arithmetic out in full.
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
