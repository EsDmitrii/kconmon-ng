package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/matrix"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// TopologyHistory is the read seam GET /api/v1/topology?at= takes: the fold half of
// store.EventStore.
type TopologyHistory interface {
	TopologyAt(ctx context.Context, at time.Time) (store.TopologySnapshot, error)
}

// topologyHistoryUnavailableDetail is ?at='s 503, in annotationsUnavailableDetail's shape; it is
// deliberately NOT the live route's "controller not configured" message.
const topologyHistoryUnavailableDetail = "historical topology is reconstructed from persisted events: " +
	databaseKnob + " to enable GET /api/v1/topology?at="

// topologyRetentionDetail is ?at='s 422. It names the value an operator would
// change, because "we pruned it" is only actionable if you know what to turn up.
const topologyRetentionDetail = "no events are retained for that instant, so the topology cannot be " +
	"reconstructed there. Pick a later time, or raise database.retentionDays (console config and Helm) " +
	"to keep more history in future"

// historicalTopology is GET /api/v1/topology?at='s body.
type historicalTopology struct {
	Nodes            []controllerclient.Node  `json:"nodes"`
	Agents           []controllerclient.Agent `json:"agents"`
	Timestamp        time.Time                `json:"timestamp"`
	Historical       bool                     `json:"historical"`
	AsOf             time.Time                `json:"asOf"`
	EventsFolded     int                      `json:"eventsFolded"`
	UnfoldableEvents int                      `json:"unfoldableEvents"`
	Truncated        bool                     `json:"truncated"`
}

// handleTopology serves the LIVE controller snapshot, or -- with ?at= set; that is why the counter
// is in the response rather than only in a log.
func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	if at := r.URL.Query().Get("at"); at != "" {
		s.serveHistoricalTopology(w, r, at)
		return
	}
	if s.ctrl == nil {
		writeProblem(w, http.StatusServiceUnavailable, "controller not configured",
			"set controller.url in the console config (Helm: console.controller.url)")
		return
	}
	topo, err := s.ctrl.Topology(r.Context())
	if err != nil {
		if errors.Is(err, controllerclient.ErrUnavailable) {
			writeProblem(w, http.StatusBadGateway, "controller unavailable", "no controller leader answered after retries")
			return
		}
		/* LOGGED, not forwarded. err.Error() on a transport failure is a *url.Error carrying the
		   controller's in-cluster URL and its resolved pod IP, and under auth.mode=anonymous that
		   body reaches anyone who can reach the console. handleAlerts and serveHistoricalTopology in
		   this same file already answer with a fixed sentence. */
		slog.Error("controller topology failed", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "controller error", "the controller did not answer")
		return
	}
	// A controller that answered {"nodes":null} (Go marshals a nil slice as null)
	// must not reach the console as null: the frontend indexes into nodes/agents
	// the same way the historical path (serveHistoricalTopology) already guards
	// with make(). nil-slice accident, NOT a semantic null -- an empty topology is
	// [], never absent.
	if topo.Nodes == nil {
		topo.Nodes = []controllerclient.Node{}
	}
	if topo.Agents == nil {
		topo.Agents = []controllerclient.Agent{}
	}
	writeJSON(w, topo)
}

// serveHistoricalTopology answers GET /api/v1/topology?at=<RFC3339>. raw is the
// param verbatim, already known to be non-empty. See handleTopology's comment
// for the fold's contract and the status-code order.
func (s *Server) serveHistoricalTopology(w http.ResponseWriter, r *http.Request, raw string) {
	// The dependency gate comes first, exactly as every other store-backed handler in this package
	// does it.
	if s.topologyHistory == nil {
		writeProblem(w, http.StatusServiceUnavailable, "historical topology not available",
			topologyHistoryUnavailableDetail)
		return
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid at", "at must be an RFC3339 timestamp")
		return
	}
	if at.After(time.Now()) {
		// Refused rather than clamped to now: silently answering a different
		// question than the one asked is how a Time Machine starts lying.
		writeProblem(w, http.StatusBadRequest, "invalid at", "at is in the future; the topology there is not known yet")
		return
	}

	snap, err := s.topologyHistory.TopologyAt(r.Context(), at)
	if err != nil {
		// Logged, never surfaced: the driver error can carry a DSN or other
		// upstream detail that has no business in an HTTP response body.
		slog.Error("topology fold failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "topology history unavailable", "failed to query event history")
		return
	}
	// An empty table and an at below the floor are the same answer for the
	// same reason: the events that would have built that set are not there.
	if snap.OldestRetained.IsZero() || at.Before(snap.OldestRetained) {
		writeProblem(w, http.StatusUnprocessableEntity, "outside retained history", topologyRetentionDetail)
		return
	}

	out := historicalTopology{
		Nodes:            make([]controllerclient.Node, 0, len(snap.Nodes)),
		Agents:           make([]controllerclient.Agent, 0, len(snap.Agents)),
		Timestamp:        snap.LastChange,
		Historical:       true,
		AsOf:             at,
		EventsFolded:     snap.EventsFolded,
		UnfoldableEvents: snap.UnfoldableEvents,
		Truncated:        snap.Truncated,
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = at
	}
	for _, n := range snap.Nodes {
		out.Nodes = append(out.Nodes, controllerclient.Node{Name: n.Name, Zone: n.Zone, Ready: n.Ready})
	}
	for _, a := range snap.Agents {
		// Capabilities are deliberately absent: no event records them, and inventing an empty list
		// would read as "no planes" to a consumer that keys off the field's presence.
		out.Agents = append(out.Agents, controllerclient.Agent{
			ID: a.ID, NodeName: a.NodeName, PodIP: a.PodIP, Zone: a.Zone, Labels: a.Labels,
		})
	}
	writeJSON(w, out)
}

// prometheusUnavailableDetail is the 503 detail of every route that reads Prometheus.
const prometheusUnavailableDetail = "set prometheus.url in the console config (Helm: console.prometheus.url)"

// prometheusUnavailable answers 503 and reports true when no Prometheus client is wired.
func (s *Server) prometheusUnavailable(w http.ResponseWriter) bool {
	if s.prom == nil {
		writeProblem(w, http.StatusServiceUnavailable, "prometheus not configured", prometheusUnavailableDetail)
		return true
	}
	return false
}

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	if s.prometheusUnavailable(w) {
		return
	}
	protocol := r.URL.Query().Get("protocol")
	if protocol == "" {
		protocol = "tcp"
	}
	if err := checks.ValidatePlane(r.URL.Query().Get("plane")); err != nil {
		writeProblem(w, http.StatusBadRequest, "unsupported plane", err.Error())
		return
	}
	m, err := s.cachedMatrix(r.Context(), protocol)
	if err != nil {
		if errors.Is(err, matrix.ErrBadProtocol) {
			writeProblem(w, http.StatusBadRequest, "unsupported protocol", "protocol must be one of tcp|udp|icmp|pmtu")
			return
		}
		// Same reason as handleTopology's: the error text names Prometheus' URL and its address.
		slog.Error("matrix computation failed", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "matrix computation failed", "prometheus did not answer")
		return
	}
	writeJSON(w, m)
}

// matrixCacheTTL is how long GET /api/v1/matrix serves one computation; well under the SPA's
// 15s MATRIX_POLL_MS, so a poller never reads a matrix older than one cycle.
const matrixCacheTTL = 5 * time.Second

type matrixEntry struct {
	m  *matrix.Matrix
	at time.Time
}

// cachedMatrix is matrix.Compute behind a per-protocol singleflight and a matrixCacheTTL cache, so
// a client looping on GET /api/v1/matrix costs Prometheus at most one computation per protocol per
// TTL. Only successes are cached.
func (s *Server) cachedMatrix(ctx context.Context, protocol string) (*matrix.Matrix, error) {
	s.matrixMu.Lock()
	e, ok := s.matrixCache[protocol]
	s.matrixMu.Unlock()
	if ok && time.Since(e.at) < matrixCacheTTL {
		return e.m, nil
	}

	ch := s.matrixFlight.DoChan(protocol, func() (any, error) {
		// Detached from the first caller: its disconnect must not fail the others waiting on this
		// flight. promql.Client's QueryTimeout still bounds every query.
		m, err := matrix.Compute(context.WithoutCancel(ctx), s.prom, s.cfg.MetricsPrefix, protocol)
		if err != nil {
			return nil, err
		}
		m = s.restrictToAdvertisedPlane(context.WithoutCancel(ctx), m, protocol)
		s.matrixMu.Lock()
		if s.matrixCache == nil {
			s.matrixCache = make(map[string]matrixEntry)
		}
		s.matrixCache[protocol] = matrixEntry{m: m, at: time.Now()}
		s.matrixMu.Unlock()
		return m, nil
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*matrix.Matrix), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// matrixTopologyTimeout bounds the topology read the matrix is checked against; past it the matrix is
// served as Prometheus has it.
const matrixTopologyTimeout = 2 * time.Second

// restrictToAdvertisedPlane checks m against the planes the agents advertise (matrix.RestrictToPlane),
// failing open to m when the topology cannot be read.
func (s *Server) restrictToAdvertisedPlane(ctx context.Context, m *matrix.Matrix, protocol string) *matrix.Matrix {
	if s.topology == nil {
		return m
	}
	ctx, cancel := context.WithTimeout(ctx, matrixTopologyTimeout)
	defer cancel()
	topo, err := s.topology.Topology(ctx)
	if err != nil {
		slog.Debug("matrix: topology unavailable, serving it without the plane check", "error", err)
		return m
	}
	return matrix.RestrictToPlane(m, matrix.PlaneRunners(topo.Agents, protocol))
}

type promQLQueryRequest struct {
	Query string     `json:"query"`
	Time  *time.Time `json:"time,omitempty"`
}

type promQLRangeRequest struct {
	Query string    `json:"query"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Step  int64     `json:"step"` // nanoseconds, per API.md duration convention
}

// promqlRateLimitDetail is what a throttled PromQL caller is told; it names the knob, the way the
// runs limit does.
const promqlRateLimitDetail = "too many PromQL queries " +
	"(limit: console.rateLimit.promqlPerMinute per subject per minute)"

// promQLAllowed answers 429 and reports false when the caller is over its PromQL budget.
func (s *Server) promQLAllowed(w http.ResponseWriter, r *http.Request) bool {
	/* Rate limited BEFORE the query leaves for Prometheus: these routes forward ARBITRARY PromQL to
	   the cluster's monitoring stack, promql:query belongs to the viewer role, and the chart's demo
	   default makes every visitor a viewer. One wide range query is a great deal of upstream work,
	   and nothing bounded how many a caller could ask for. */
	subject, _ := SubjectFrom(r.Context())
	if !s.rateLimitAllow(r.Context(), rateLimitPromQL, s.cfg.RateLimit.PromQLPerMinute, promqlRateLimitKey(subject)) {
		writeRateLimited(w, promqlRateLimitDetail)
		return false
	}
	return true
}

func (s *Server) handlePromQLQuery(w http.ResponseWriter, r *http.Request) {
	if s.prometheusUnavailable(w) || !s.promQLAllowed(w, r) {
		return
	}
	var req promQLQueryRequest
	dec := strictJSONDecoder(r.Body)
	if err := dec.Decode(&req); err != nil || req.Query == "" {
		badBody(w, err, `body must be JSON with a non-empty "query"`)
		return
	}
	if refuseTrailingJSON(w, dec) {
		return
	}
	ts := time.Time{}
	if req.Time != nil {
		ts = *req.Time
	}
	raw, err := s.prom.Query(r.Context(), rewriteMetricsPrefix(req.Query, s.cfg.MetricsPrefix), ts)
	s.writePromResult(w, raw, err)
}

func (s *Server) handlePromQLQueryRange(w http.ResponseWriter, r *http.Request) {
	if s.prometheusUnavailable(w) || !s.promQLAllowed(w, r) {
		return
	}
	var req promQLRangeRequest
	dec := strictJSONDecoder(r.Body)
	if err := dec.Decode(&req); err != nil || req.Query == "" {
		badBody(w, err, `body must be JSON with "query", RFC3339 "start"/"end", and "step" in nanoseconds`)
		return
	}
	if refuseTrailingJSON(w, dec) {
		return
	}
	raw, err := s.prom.QueryRange(r.Context(), rewriteMetricsPrefix(req.Query, s.cfg.MetricsPrefix),
		req.Start, req.End, time.Duration(req.Step))
	s.writePromResult(w, raw, err)
}

// isPromAPIError reports whether ue is Prometheus's own API error: one of the statuses its HTTP API
// documents for a failed query, carrying the {"status":"error"} envelope.
func isPromAPIError(ue *promql.UpstreamError) bool {
	switch ue.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusInternalServerError, http.StatusServiceUnavailable:
	default:
		return false
	}
	var env struct {
		Status string `json:"status"`
	}
	return json.Unmarshal(ue.Body, &env) == nil && env.Status == "error"
}

func (s *Server) writePromResult(w http.ResponseWriter, raw json.RawMessage, err error) {
	switch {
	case err == nil:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(raw) //nolint:gosec // G705: Prometheus JSON envelope, served as application/json with nosniff, never rendered as HTML
	case errors.Is(err, promql.ErrBadRequest):
		writeProblem(w, http.StatusBadRequest, "invalid query parameters", err.Error())
	case errors.Is(err, promql.ErrRangeTooLarge):
		writeProblem(w, http.StatusUnprocessableEntity, "range exceeds maximum", err.Error())
	case errors.Is(err, promql.ErrResponseTooLarge):
		writeProblem(w, http.StatusUnprocessableEntity, "result too large", "narrow the query or shorten the range")
	default:
		ue, isUpstream := errors.AsType[*promql.UpstreamError](err)
		if isUpstream && isPromAPIError(ue) {
			// Forward Prometheus's own error envelope (e.g. PromQL parse errors)
			// with its status so the PromQL Console can show it verbatim.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(ue.Status)
			_, _ = w.Write(ue.Body) //nolint:gosec // G705: Prometheus JSON error envelope, served as application/json with nosniff, never rendered as HTML
			return
		}
		if isUpstream {
			// Anything else (an auth proxy's 401/403, an HTML error page) is a misconfigured upstream. Its
			// status must not become the console's own: the SPA reads a 401 as a lost session.
			slog.Error("prometheus answered with an unexpected status", "status", ue.Status) //nolint:gosec // G706: structured slog fields
			writeProblem(w, http.StatusBadGateway, "prometheus error",
				"prometheus answered HTTP "+strconv.Itoa(ue.Status))
			return
		}
		// Prometheus' OWN envelope is forwarded above (that is the point of the branch); this branch
		// is a transport failure, whose text names the upstream URL and address.
		slog.Error("prometheus request failed", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "prometheus error", "prometheus did not answer")
	}
}
