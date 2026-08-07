package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
	"github.com/go-chi/chi/v5"
)

// RunService is the subset of *checks.Runner that httpapi needs: start a new
// run, read one run, read one run's results, and page through runs. A local
// interface, same shape as EventLister/Auditor/RoleAdmin -- so a test can
// substitute a fake without wiring a real controller, hub, bus, and store the
// way constructing a real *checks.Runner would require.
type RunService interface {
	Start(ctx context.Context, spec checks.Spec, initiator authz.Subject) (string, error) //nolint:gocritic // Subject is a value type by design
	Get(ctx context.Context, runID string) (checks.Run, error)
	GetResults(ctx context.Context, runID string) ([]store.RunResult, error)
	List(ctx context.Context, f checks.ListFilter) (checks.RunPage, error)
}

var _ RunService = (*checks.Runner)(nil)

// Limit bounds for GET /api/v1/runs, mirroring GET /api/v1/events's
// eventsMinLimit/eventsMaxLimit/eventsDefaultLimit convention (events.go) --
// store.RunFilter.Limit clamps identically on its own (checks.go's
// clampLimit, shared with EventFilter), but httpapi pre-clamps too, the same
// hand-kept-in-sync-copy reasoning events.go's own comment gives.
const (
	runsMinLimit     = 1
	runsMaxLimit     = 500
	runsDefaultLimit = 100
)

// runsUnavailableDetail is served whenever s.runner is nil: no controller
// was configured (cmd/console only ever constructs a Runner when one is), so
// there is no meaningful run path at all -- the same "nil dependency -> 503"
// convention every other optional Deps field in this package uses.
const runsUnavailableDetail = "set controller.url in the console config (Helm: console.controller.url) to enable on-demand diagnostics runs"

// runCreateRequest is POST /api/v1/runs's body (task-23-brief.md verbatim).
// TimeoutNs is nanoseconds in the JSON, the repo-wide duration convention
// (API.md:12) -- the one place the unit changes is inside
// controllerclient.Diagnose, which converts to the controller's
// ?timeout=<seconds>, exactly as promql's step already does (API.md:116-118).
type runCreateRequest struct {
	Sources      []string `json:"sources"`
	Destinations []string `json:"destinations"`
	Type         string   `json:"type"`
	Plane        string   `json:"plane"`
	TimeoutNs    int64    `json:"timeoutNs"`
}

// runCreateResponse is POST /api/v1/runs's 202 body (task-23-brief.md
// verbatim). WSTopic is the server-chosen topic name -- ws.RunTopic's own
// canonical format -- so the browser subscribes to a name it was told, never
// one it constructs itself by string concatenation.
type runCreateResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	PairTotal int32  `json:"pairTotal"`
	WSTopic   string `json:"wsTopic"`
}

// writeJSONStatus is writeJSON (server.go) with an explicit status code, for
// the one response in this package that is not a plain 200: POST
// /api/v1/runs's 202 Accepted. Headers (Content-Type, and the caller's own
// Location) must be set before this is called -- WriteHeader freezes them.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleRunsCreate plans and starts a new diagnostics run. s.runner nil
// (no controller.url configured -- there is no meaningful run path without
// one) answers 503. A malformed body answers 400. A well-formed spec Plan
// refuses on policy -- too many pairs, or every pair a self-pair/duplicate --
// answers 422, never 400: the request is valid JSON shaped exactly as
// documented, it is refused because of what it WOULD do, the same
// distinction promql's own guards draw (API.md:125-126). An unrecognized
// check type is a 400: that is a malformed field value, not a policy call.
func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}

	var req runCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`body must be JSON with "sources", "destinations", "type", "plane", and "timeoutNs" (nanoseconds)`)
		return
	}

	subject, _ := SubjectFrom(r.Context())
	spec := checks.Spec{
		Sources:      req.Sources,
		Destinations: req.Destinations,
		Type:         req.Type,
		Plane:        req.Plane,
		Timeout:      time.Duration(req.TimeoutNs),
	}

	id, err := s.runner.Start(r.Context(), spec, subject)
	if err != nil {
		s.writeRunStartError(w, err)
		return
	}

	// Start already persisted the run (status "pending") before returning --
	// Get reads that same row back rather than this handler hardcoding
	// "pending" and recomputing pairTotal itself, so the response always
	// reports the run's actual, just-created state.
	run, err := s.runner.Get(r.Context(), id)
	if err != nil {
		slog.Error("httpapi: read back run after start failed", "run", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusInternalServerError, "run created but could not be read back", "")
		return
	}

	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSONStatus(w, http.StatusAccepted, runCreateResponse{
		ID: run.ID, Status: run.Status, PairTotal: run.PairTotal, WSTopic: ws.RunTopic(run.ID),
	})
}

// writeRunStartError maps a checks.Runner.Start error to a response.
// ErrTooManyPairs/ErrNoPairs/ErrNoNodes are all a well-formed spec refused
// for what it would produce, not for how it is shaped -- 422, per this
// handler's own doc comment. ErrUnknownType is a malformed field value --
// 400. Everything else is either a wrapped controllerclient error from
// resolving the topology for node expansion (mirrors handleTopology's own
// mapping) or an unexpected store failure.
func (s *Server) writeRunStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checks.ErrTooManyPairs):
		writeProblem(w, http.StatusUnprocessableEntity, "too many pairs", err.Error())
	case errors.Is(err, checks.ErrNoPairs):
		writeProblem(w, http.StatusUnprocessableEntity, "no pairs to check", err.Error())
	case errors.Is(err, checks.ErrNoNodes):
		writeProblem(w, http.StatusUnprocessableEntity, "no nodes available", err.Error())
	case errors.Is(err, checks.ErrUnknownType):
		writeProblem(w, http.StatusBadRequest, "invalid type", err.Error())
	case errors.Is(err, controllerclient.ErrUnavailable):
		writeProblem(w, http.StatusBadGateway, "controller unavailable", "no controller leader answered after retries")
	default:
		slog.Error("httpapi: start run failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "controller error", err.Error())
	}
}

// runSummary is one row of GET /api/v1/runs's body, and the run half of GET
// /api/v1/runs/{id}'s body.
type runSummary struct {
	ID            string     `json:"id"`
	CreatedAt     time.Time  `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Status        string     `json:"status"`
	Type          string     `json:"type"`
	Plane         string     `json:"plane"`
	InitiatorKind string     `json:"initiatorKind"`
	InitiatorID   string     `json:"initiatorId"`
	PairTotal     int32      `json:"pairTotal"`
	PairOK        int32      `json:"pairOk"`
	PairFailed    int32      `json:"pairFailed"`
}

func toRunSummary(run *checks.Run) runSummary {
	return runSummary{
		ID: run.ID, CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		Status: run.Status, Type: run.CheckType, Plane: run.Plane,
		InitiatorKind: run.InitiatorKind, InitiatorID: run.InitiatorID,
		PairTotal: run.PairTotal, PairOK: run.PairOK, PairFailed: run.PairFailed,
	}
}

// runsListResponse is GET /api/v1/runs's body -- same keyset-cursor shape as
// eventsResponse/auditResponse.
type runsListResponse struct {
	Runs       []runSummary `json:"runs"`
	NextCursor string       `json:"nextCursor"`
}

// handleRunsList serves one page of runs, newest first, behind an opaque
// keyset cursor, filtered by ?type=&status=. s.runner nil answers 503, same
// convention as every other database/controller-backed endpoint here.
func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}

	q := r.URL.Query()

	cursor := q.Get("cursor")
	if cursor != "" {
		if _, _, _, err := store.DecodeRunCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	page, err := s.runner.List(r.Context(), checks.ListFilter{
		CheckType: q.Get("type"),
		Status:    q.Get("status"),
		Cursor:    cursor,
		Limit:     clampRunsLimit(parseRunsLimit(q.Get("limit"))),
	})
	if err != nil {
		slog.Error("list runs failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "run history unavailable", "failed to query run history")
		return
	}

	out := make([]runSummary, 0, len(page.Runs))
	for i := range page.Runs {
		out = append(out, toRunSummary(&page.Runs[i]))
	}
	writeJSON(w, runsListResponse{Runs: out, NextCursor: page.NextCursor})
}

// parseRunsLimit parses ?limit=, mirroring parseEventsLimit's contract:
// anything that fails to parse is treated as unset (0), never a 400 -- limit
// is documented to clamp.
func parseRunsLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// clampRunsLimit mirrors clampEventsLimit's contract: 0 defaults, everything
// else clamps into [runsMinLimit, runsMaxLimit].
func clampRunsLimit(n int) int {
	switch {
	case n == 0:
		return runsDefaultLimit
	case n < runsMinLimit:
		return runsMinLimit
	case n > runsMaxLimit:
		return runsMaxLimit
	default:
		return n
	}
}

// runResultResponse is one row of GET /api/v1/runs/{id}'s "results" array.
type runResultResponse struct {
	SourceNode      string          `json:"sourceNode"`
	DestinationNode string          `json:"destinationNode"`
	Success         bool            `json:"success"`
	DurationNs      int64           `json:"durationNs"`
	Error           string          `json:"error,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	RecordedAt      time.Time       `json:"recordedAt"`
}

// runDetailResponse is GET /api/v1/runs/{id}'s body: the run plus its
// per-pair results (task-23-brief.md: "run + its results").
type runDetailResponse struct {
	runSummary
	Spec    json.RawMessage     `json:"spec"`
	Results []runResultResponse `json:"results"`
}

// handleRunsGet serves one run and its results. s.runner nil answers 503;
// an unknown id answers 404.
func (s *Server) handleRunsGet(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}

	id := chi.URLParam(r, "id")
	run, err := s.runner.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found", "")
			return
		}
		slog.Error("get run failed", "run", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "run history unavailable", "failed to query run history")
		return
	}

	results, err := s.runner.GetResults(r.Context(), id)
	if err != nil {
		slog.Error("get run results failed", "run", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "run history unavailable", "failed to query run history")
		return
	}

	out := make([]runResultResponse, 0, len(results))
	for i := range results {
		res := &results[i]
		out = append(out, runResultResponse{
			SourceNode: res.SourceNode, DestinationNode: res.DestinationNode,
			Success: res.Success, DurationNs: res.DurationNs, Error: res.Error,
			Result: res.Result, RecordedAt: res.RecordedAt,
		})
	}
	writeJSON(w, runDetailResponse{runSummary: toRunSummary(&run), Spec: run.Spec, Results: out})
}
