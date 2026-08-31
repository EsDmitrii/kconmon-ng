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
	"github.com/google/uuid"
)

// RunService is the subset of *checks.Runner that httpapi needs: start a new run; a local
// interface, same shape as EventLister/Auditor/RoleAdmin.
type RunService interface {
	Start(ctx context.Context, spec checks.Spec, initiator authz.Subject) (string, error) //nolint:gocritic // Subject is a value type by design
	// StartZonePair is the investigation preset: both zones expanded against live topology, chunked
	// into runs of at most 400 pairs. A non-empty slice alongside an error means a partial start —
	// see checks.Runner.StartZonePair.
	StartZonePair(ctx context.Context, spec checks.ZonePairSpec, initiator authz.Subject) ([]checks.ZonePairRun, error) //nolint:gocritic // Subject is a value type by design
	Get(ctx context.Context, runID string) (checks.Run, error)
	GetResults(ctx context.Context, runID string) (results []store.RunResult, truncated bool, err error)
	List(ctx context.Context, f checks.ListFilter) (checks.RunPage, error)
	// Cancel stops a run in flight. See checks.Runner.Cancel for the two
	// deliberate non-errors this endpoint inherits: cancelling an already
	// terminal run, and cancelling one another replica started, both succeed.
	Cancel(ctx context.Context, runID string) error
}

var _ RunService = (*checks.Runner)(nil)

// Limit bounds for GET /api/v1/runs, mirroring GET /api/v1/events's
// eventsMinLimit/eventsMaxLimit/eventsDefaultLimit convention (events.go).
const (
	runsMinLimit     = 1
	runsMaxLimit     = 500
	runsDefaultLimit = 100
)

// runsUnavailableDetail is served whenever s.runner is nil.
const runsUnavailableDetail = "set controller.url in the console config (Helm: console.controller.url) to enable on-demand diagnostics runs"

// runCreateRequest is POST /api/v1/runs's body.
type runCreateRequest struct {
	Sources      []string `json:"sources"`
	Destinations []string `json:"destinations"`
	Type         string   `json:"type"`
	Plane        string   `json:"plane"`
	TimeoutNs    int64    `json:"timeoutNs"`

	// DestinationKind is "node", "target" or "adhoc" -- the diagnostics form's destination selector;
	// both external kinds travel to the controller as destinationKind=external.
	DestinationKind     string `json:"destinationKind,omitempty"`
	DestinationTargetID string `json:"destinationTargetId,omitempty"`
	DestinationAddress  string `json:"destinationAddress,omitempty"`

	// DurationNs turns the run into an INTERVAL run; absent or 0 is an instant run -- one probe per
	// pair, the only thing that existed.
	DurationNs int64 `json:"durationNs,omitempty"`

	// SampleIntervalNs is the cadence the caller wants between one pair's probes. Absent or 0 keeps
	// the derived behaviour byte-identical: duration/500, floored at 5s, stretched to one round when
	// the check type is slower than that.
	//
	// Bounded to [1s, durationNs] and refused with 422 outside it. A cadence the fan-out cannot keep
	// is NOT out of range -- it is planned around and reported back in sampleIntervalAdjusted.
	SampleIntervalNs int64 `json:"sampleIntervalNs,omitempty"`
}

// runCreateResponse is POST /api/v1/runs's 202 body; WSTopic is the server-chosen topic name --
// ws.RunTopic's own canonical format.
type runCreateResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	PairTotal int32  `json:"pairTotal"`
	WSTopic   string `json:"wsTopic"`
	// The cadence the server actually planned. A slow check type stretches it past the duration's
	// base cadence, so the caller cannot derive these from the duration alone.
	PlannedSampleIntervalNs int64 `json:"plannedSampleIntervalNs,omitempty"`
	PlannedSamplesPerPair   int   `json:"plannedSamplesPerPair,omitempty"`
	// RequestedSampleIntervalNs echoes sampleIntervalNs back, and SampleIntervalAdjusted names why
	// the plan is not it: "cap" (the 500-samples-per-pair ceiling bound it) or "round" (one round
	// over this fan-out cannot finish that fast). Both empty when nothing was asked or nothing
	// moved. They are separate fields rather than one "the cadence" because the whole defect this
	// closes was one quantity reported as three different numbers.
	RequestedSampleIntervalNs int64  `json:"requestedSampleIntervalNs,omitempty"`
	SampleIntervalAdjusted    string `json:"sampleIntervalAdjusted,omitempty"`
}

// writeJSONStatus is writeJSON (server.go) with an explicit status code; headers (Content-Type, and
// the caller's own Location) must be set before this is called.
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// runsRateLimitDetail is the 429 body's detail for POST /api/v1/runs; it names the config key an
// operator would change.
const runsRateLimitDetail = "too many diagnostics runs started; retry shortly " +
	"(limit: console.rateLimit.runsPerMinute per subject per minute)"

// handleRunsCreate plans and starts a new diagnostics run.
func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}

	// Rate limit BEFORE anything expensive: a run fans out to up to 400 agent pairs.
	subject, _ := SubjectFrom(r.Context())
	if !s.rateLimitAllow(r.Context(), rateLimitRuns, s.cfg.RateLimit.RunsPerMinute, runsRateLimitKey(subject)) {
		writeRateLimited(w, runsRateLimitDetail)
		return
	}

	var req runCreateRequest
	// Strict: a misspelled field (durationNS, destinationaddress) is a clean 400
	// naming it, not a silently dropped value that starts a DIFFERENT run.
	if !decodeMutationBody(w, r, &req,
		`body must be JSON with "sources", "destinations", "type", "plane", and "timeoutNs" (nanoseconds)`) {
		return
	}

	/* Node names go into the run's JSONB spec column, and a NUL there is refused by PostgreSQL
	   (22P05) — which writeRunStartError's default arm reported as "the run could not be started",
	   a sentence about the fleet in answer to a malformed body. */
	for _, n := range append(append([]string{}, req.Sources...), req.Destinations...) {
		if rejectControlChars(w, "sources/destinations", n) {
			return
		}
	}

	spec := checks.Spec{
		Sources:                 req.Sources,
		Destinations:            req.Destinations,
		Type:                    req.Type,
		Plane:                   req.Plane,
		Timeout:                 time.Duration(req.TimeoutNs),
		Duration:                time.Duration(req.DurationNs),
		RequestedSampleInterval: time.Duration(req.SampleIntervalNs),
	}
	if !s.resolveRunDestination(w, r, &req, &spec) {
		return
	}

	id, err := s.runner.Start(r.Context(), spec, subject)
	if err != nil {
		s.writeRunStartError(w, err)
		return
	}

	// Start already persisted the run (status "pending") before returning.
	run, err := s.runner.Get(r.Context(), id)
	if err != nil {
		slog.Error("httpapi: read back run after start failed", "run", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusInternalServerError, "run created but could not be read back", "")
		return
	}

	// The planned cadence is read back off the persisted spec, so the response cannot describe a
	// different plan than the one the run is executing.
	planned := plannedCadence(run.Spec)

	w.Header().Set("Location", "/api/v1/runs/"+run.ID)
	writeJSONStatus(w, http.StatusAccepted, runCreateResponse{
		ID: run.ID, Status: run.Status, PairTotal: run.PairTotal, WSTopic: ws.RunTopic(run.ID),
		PlannedSampleIntervalNs:   planned.PlannedSampleIntervalNs,
		PlannedSamplesPerPair:     planned.PlannedSamplesPerPair,
		RequestedSampleIntervalNs: planned.RequestedSampleInterval.Nanoseconds(),
		SampleIntervalAdjusted:    planned.SampleIntervalAdjusted,
	})
}

// plannedCadence reads the derived cadence back out of a run's spec snapshot; a run created before
// the fields existed simply reports zeroes, which `omitempty` drops.
func plannedCadence(spec json.RawMessage) checks.Spec {
	var out checks.Spec
	if len(spec) == 0 {
		return out
	}
	_ = json.Unmarshal(spec, &out)
	return out
}

// resolveRunDestination turns runCreateRequest's destination fields into spec's.
func (s *Server) resolveRunDestination(w http.ResponseWriter, r *http.Request, req *runCreateRequest, spec *checks.Spec) bool {
	switch req.DestinationKind {
	case "", "node":
		// The contract, byte-identical: Destinations are node names; the external-only fields are refused
		// rather than ignored.
		if req.DestinationTargetID != "" || req.DestinationAddress != "" {
			writeProblem(w, http.StatusBadRequest, "invalid destination",
				"destinationTargetId and destinationAddress require destinationKind target or adhoc")
			return false
		}
		return true
	case "target", "adhoc":
	default:
		writeProblem(w, http.StatusBadRequest, "invalid destination",
			"destinationKind must be node, target or adhoc")
		return false
	}

	// Both external kinds: node-name Destinations must be empty -- one run
	// probes either the mesh or one external destination, never a mix.
	if len(req.Destinations) != 0 {
		writeProblem(w, http.StatusBadRequest, "invalid destination",
			"destinations must be empty when destinationKind is target or adhoc")
		return false
	}

	if req.DestinationKind == "target" {
		if s.targets == nil {
			writeProblem(w, http.StatusServiceUnavailable, "targets not available", targetsUnavailableDetail)
			return false
		}
		if _, err := uuid.Parse(req.DestinationTargetID); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid destination",
				"destinationTargetId must be a UUID")
			return false
		}
		target, err := s.targets.GetTarget(r.Context(), req.DestinationTargetID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeProblem(w, http.StatusUnprocessableEntity, "unknown target",
				"destinationTargetId does not name a saved target")
			return false
		case err != nil:
			slog.Error("httpapi: resolve run target failed", "error", err)
			writeProblem(w, http.StatusBadGateway, "targets unavailable",
				"failed to read the target the run would probe")
			return false
		}
		spec.Destinations = nil
		spec.TypedDestinations = []checks.Destination{{
			Kind:    checks.DestKindTarget,
			Name:    target.Name,
			Address: target.Address,
		}}
		return true
	}

	// adhoc -- the first switch admits only target|adhoc past it.
	if req.DestinationAddress == "" {
		writeProblem(w, http.StatusBadRequest, "invalid destination",
			"destinationAddress is required when destinationKind is adhoc")
		return false
	}
	// The same rule a SAVED definition's destination_address is held to: a well-formed field carrying
	// a value the agent could never dial is 422, the class ErrTooManyPairs already sits in.
	if err := store.ValidateAdhocAddress(req.DestinationAddress); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid destination address", err.Error())
		return false
	}
	spec.Destinations = nil
	// Name deliberately repeats the address: for an ad-hoc probe the address
	// IS the operator's name for it, matching the controller's own destName
	// fallback -- and it is the label the run's results carry.
	spec.TypedDestinations = []checks.Destination{{
		Kind:    checks.DestKindAdhoc,
		Name:    req.DestinationAddress,
		Address: req.DestinationAddress,
	}}
	return true
}

// writeRunStartError maps a checks.Runner.Start error to a response;
// ErrTooManyPairs/ErrNoPairs/ErrNoNodes are all a well-formed spec refused for what it would
// produce.
func (s *Server) writeRunStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, checks.ErrTooManyPairs):
		writeProblem(w, http.StatusUnprocessableEntity, "too many pairs", err.Error())
	case errors.Is(err, checks.ErrNoPairs):
		writeProblem(w, http.StatusUnprocessableEntity, "no pairs to check", err.Error())
	case errors.Is(err, checks.ErrNoNodes):
		writeProblem(w, http.StatusUnprocessableEntity, "no nodes available", err.Error())
	case errors.Is(err, checks.ErrDurationOutOfRange):
		// A well-formed field carrying a value policy refuses -- the same class as ErrTooManyPairs, and
		// 422 for the same reason.
		writeProblem(w, http.StatusUnprocessableEntity, "run duration out of range", err.Error())
	case errors.Is(err, checks.ErrSampleIntervalOutOfRange):
		// Same class, same code. Note what is NOT here: a cadence this fan-out cannot keep is not an
		// error at all -- it is planned around and reported in sampleIntervalAdjusted.
		writeProblem(w, http.StatusUnprocessableEntity, "sample interval out of range", err.Error())
	case errors.Is(err, checks.ErrUnknownType):
		writeProblem(w, http.StatusBadRequest, "invalid type", err.Error())
	case errors.Is(err, checks.ErrInvalidDestination):
		writeProblem(w, http.StatusBadRequest, "invalid destination", err.Error())
	case errors.Is(err, controllerclient.ErrUnavailable):
		writeProblem(w, http.StatusBadGateway, "controller unavailable", "no controller leader answered after retries")
	default:
		/* A FIXED sentence, and the detail goes to the log.
		   err here is whatever failed inside Start, and that is not always the controller: a store
		   write that PostgreSQL refuses lands in this arm too, so the client was handed a database
		   error message under the title "controller error" — wrong about the component and leaking
		   the internals of one. handleTopology already answers this shape; this now matches it. */
		slog.Error("httpapi: start run failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "run not started",
			"the run could not be started; see the console logs for the reason")
	}
}

// runIDFrom resolves the {id} path parameter for POST /api/v1/runs/{id}/cancel.
func runIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	parsed, err := uuid.Parse(id)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "run not found", "")
		return "", false
	}
	/* CANONICALISED, not just validated. uuid.Parse accepts uppercase, the hyphenless 32-hex form and
	   urn:uuid:; the raw string was then handed on, and Runner.Cancel looks a run up in a sync.Map
	   keyed by the canonical spelling. The lookup missed, Cancel fell through to "not in flight in
	   this process — leaving it to the reaper", and the handler answered 204: the operator was told
	   the run was cancelled while it went on fanning out to up to 400 pairs. One spelling downstream
	   is what makes the map, the store and the WebSocket topic agree. */
	return parsed.String(), true
}

// handleRunsCancel stops a run in flight; both are 204, not 409 and not 202. An operator who clicks
// cancel on a run that just finished has not done anything wrong.
func (s *Server) handleRunsCancel(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}
	id, ok := runIDFrom(w, r)
	if !ok {
		return
	}

	if err := s.runner.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "run not found", "")
			return
		}
		/* The run is real and still going, on a replica this one cannot talk to. 502 rather than the
		   204 this used to answer: "cancelled" is a claim about the fleet, and it was false. */
		if errors.Is(err, checks.ErrCancelUnreachable) {
			writeProblem(w, http.StatusBadGateway, "cancel not delivered",
				"this run is owned by another console replica and this console has no cross-replica bus to "+
					"forward the cancel on (set redis.existingSecret, or run a single replica); the run is still going")
			return
		}
		slog.Error("cancel run failed", "run", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "run history unavailable", "failed to query run history")
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// toRunSummary projects a run onto the wire. PairOK/PairFailed count PAIRS, never samples: on an
// interval run a pair is OK when its LATEST sample succeeded (checks.pairOutcomes), the same rule
// the detail page applies -- so "7/9 ok" in the list and on the detail page is the same 7/9.
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
	/* type and status land in Postgres text columns, and the driver is fatal on a NUL there -- the
	   error branch below would report the caller's malformed input as a 502. Same guard the scope
	   params already use. */
	for _, f := range []string{"type", "status"} {
		if rejectControlChars(w, f, q.Get(f)) {
			return
		}
	}

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
	// SampleSeq is which probe of this pair the row records; not `omitempty`: the zero value is a
	// meaningful sample number.
	SampleSeq int32 `json:"sampleSeq"`
}

// runDetailResponse is GET /api/v1/runs/{id}'s body: the run plus its per-pair results.
type runDetailResponse struct {
	runSummary
	Spec    json.RawMessage     `json:"spec"`
	Results []runResultResponse `json:"results"`
	// ResultsTruncated says the run holds MORE results than this response carries — the newest
	// store.RunResultsCap of them. An interval run is bounded at 400 pairs x 500 samples, and this
	// body is re-read every five seconds while the run is alive; unbounded, one long run could hand a
	// replica a multi-hundred-megabyte marshal on repeat. What the flag buys is that the page can say
	// its aggregates cover a tail, rather than presenting the tail as the whole run.
	ResultsTruncated bool `json:"resultsTruncated,omitempty"`
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

	results, truncated, err := s.runner.GetResults(r.Context(), id)
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
			Result: res.Result, RecordedAt: res.RecordedAt, SampleSeq: res.SampleSeq,
		})
	}
	writeJSON(w, runDetailResponse{
		runSummary: toRunSummary(&run), Spec: run.Spec, Results: out, ResultsTruncated: truncated,
	})
}
