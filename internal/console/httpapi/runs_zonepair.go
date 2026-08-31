package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

/*
 * POST /api/v1/runs/zone-pair — the investigation preset (roadmap M10-3).
 *
 * A zone-pair alert names two zones; under a sparse topology the per-pair series behind them no
 * longer exist, so "which pairs, exactly?" has to be answered by on-demand runs. This endpoint is
 * that one click: the console expands both zones to their agent node lists and starts ordinary
 * runs, chunked by checks.StartZonePair so no run exceeds the 400-pair bound and no source agent
 * is shared between concurrent chunks.
 *
 * The rate limiter charges ONE token per preset call, not one per chunk: the preset's own run cap
 * (checks.PresetMaxRuns) already bounds the burst, and charging per chunk would make the same
 * click cost 1 or 8 tokens depending on fleet size the operator cannot see.
 */

// zonePairRunsRequest is POST /api/v1/runs/zone-pair's body. There is no plane field: "pod" is the
// only plane that exists, and no duration — the preset is an instant snapshot by design.
type zonePairRunsRequest struct {
	SourceZone      string `json:"sourceZone"`
	DestinationZone string `json:"destinationZone"`
	Type            string `json:"type"`
	TimeoutNs       int64  `json:"timeoutNs,omitempty"`
}

// zonePairRunResponse is one started chunk in the 202 body.
type zonePairRunResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	PairTotal int32  `json:"pairTotal"`
	WSTopic   string `json:"wsTopic"`
}

// zonePairRunsResponse is POST /api/v1/runs/zone-pair's 202 body. No Location header upstream: a
// preset starts SEVERAL runs, and each row here carries its own permalink id.
type zonePairRunsResponse struct {
	SourceZone      string                `json:"sourceZone"`
	DestinationZone string                `json:"destinationZone"`
	PairTotal       int32                 `json:"pairTotal"`
	Runs            []zonePairRunResponse `json:"runs"`
}

// handleRunsZonePairCreate expands a zone pair into chunked diagnostics runs.
func (s *Server) handleRunsZonePairCreate(w http.ResponseWriter, r *http.Request) {
	if s.runner == nil {
		writeProblem(w, http.StatusServiceUnavailable, "runs not available", runsUnavailableDetail)
		return
	}

	// The same gate, the same bucket and the same position as handleRunsCreate: before anything
	// expensive.
	subject, _ := SubjectFrom(r.Context())
	if !s.rateLimitAllow(r.Context(), rateLimitRuns, s.cfg.RateLimit.RunsPerMinute, runsRateLimitKey(subject)) {
		writeRateLimited(w, runsRateLimitDetail)
		return
	}

	var req zonePairRunsRequest
	if !decodeMutationBody(w, r, &req,
		`body must be JSON with "sourceZone", "destinationZone" and "type" (optional "timeoutNs", nanoseconds)`) {
		return
	}
	for _, zone := range []string{req.SourceZone, req.DestinationZone} {
		if rejectControlChars(w, "sourceZone/destinationZone", zone) {
			return
		}
	}
	if req.SourceZone == "" || req.DestinationZone == "" {
		writeProblem(w, http.StatusBadRequest, "invalid zone pair",
			"sourceZone and destinationZone are both required")
		return
	}

	started, err := s.runner.StartZonePair(r.Context(), checks.ZonePairSpec{
		SourceZone:      req.SourceZone,
		DestinationZone: req.DestinationZone,
		Type:            req.Type,
		Plane:           "pod",
		Timeout:         time.Duration(req.TimeoutNs),
	}, subject)
	if err != nil {
		s.writeZonePairStartError(w, started, err)
		return
	}

	resp := zonePairRunsResponse{
		SourceZone:      req.SourceZone,
		DestinationZone: req.DestinationZone,
		Runs:            make([]zonePairRunResponse, 0, len(started)),
	}
	for _, run := range started {
		resp.PairTotal += int32(run.PairTotal) //nolint:gosec // per-run PairTotal <= 400, at most PresetMaxRuns runs
		resp.Runs = append(resp.Runs, zonePairRunResponse{
			ID: run.ID, Status: "pending", PairTotal: int32(run.PairTotal), //nolint:gosec // PairTotal <= 400 by checks.Plan
			WSTopic: ws.RunTopic(run.ID),
		})
	}
	writeJSONStatus(w, http.StatusAccepted, resp)
}

// writeZonePairStartError maps a StartZonePair failure onto a response. The partial-start branch
// comes FIRST: runs already started are facts about the fleet, and an error body that hid them
// would leave an operator unaware of probe traffic they own.
func (s *Server) writeZonePairStartError(w http.ResponseWriter, started []checks.ZonePairRun, err error) {
	if len(started) > 0 {
		ids := make([]string, len(started))
		for i := range started {
			ids[i] = started[i].ID
		}
		slog.Error("httpapi: zone-pair preset partially started", "started", len(started), "error", err)
		writeProblem(w, http.StatusBadGateway, "zone-pair runs partially started",
			fmt.Sprintf("%d of the planned runs started before the failure and are still running "+
				"(run ids: %s); the rest were not started — see the console logs for the reason",
				len(started), strings.Join(ids, ", ")))
		return
	}
	switch {
	case errors.Is(err, checks.ErrUnknownZone):
		// A well-formed name the topology cannot resolve — the ErrNoNodes class, and 422 like it.
		writeProblem(w, http.StatusUnprocessableEntity, "unknown zone", err.Error())
	case errors.Is(err, checks.ErrZonePairTooLarge):
		writeProblem(w, http.StatusUnprocessableEntity, "zone pair too large", err.Error())
	default:
		s.writeRunStartError(w, err)
	}
}
