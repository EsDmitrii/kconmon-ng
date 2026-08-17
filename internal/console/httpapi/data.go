package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

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
	"set console.database.mode in the console config (Helm: console.database.mode) to enable GET /api/v1/topology?at="

// topologyRetentionDetail is ?at='s 422. It names the value an operator would
// change, because "we pruned it" is only actionable if you know what to turn up.
const topologyRetentionDetail = "no events are retained for that instant, so the topology cannot be " +
	"reconstructed there. Pick a later time, or raise console.database.retentionDays to keep more " +
	"history in future"

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
		out.Agents = append(out.Agents, controllerclient.Agent{
			ID: a.ID, NodeName: a.NodeName, PodIP: a.PodIP, Zone: a.Zone,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleMatrix(w http.ResponseWriter, r *http.Request) {
	if s.prom == nil {
		writeProblem(w, http.StatusServiceUnavailable, "prometheus not configured",
			"set prometheus.url in the console config (Helm: console.prometheus.url)")
		return
	}
	protocol := r.URL.Query().Get("protocol")
	if protocol == "" {
		protocol = "tcp"
	}
	plane := r.URL.Query().Get("plane")
	if plane == "" {
		plane = "pod"
	}
	if plane != "pod" {
		writeProblem(w, http.StatusBadRequest, "unsupported plane", "only plane=pod exists in M1")
		return
	}
	m, err := matrix.Compute(r.Context(), s.prom, s.cfg.MetricsPrefix, protocol)
	if err != nil {
		if errors.Is(err, matrix.ErrBadProtocol) {
			writeProblem(w, http.StatusBadRequest, "unsupported protocol", "protocol must be one of tcp|udp|icmp")
			return
		}
		// Same reason as handleTopology's: the error text names Prometheus' URL and its address.
		slog.Error("matrix computation failed", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "matrix computation failed", "prometheus did not answer")
		return
	}
	writeJSON(w, m)
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

func (s *Server) handlePromQLQuery(w http.ResponseWriter, r *http.Request) {
	if s.prom == nil {
		writeProblem(w, http.StatusServiceUnavailable, "prometheus not configured",
			"set prometheus.url in the console config (Helm: console.prometheus.url)")
		return
	}
	/* Rate limited BEFORE the query leaves for Prometheus: this route forwards ARBITRARY PromQL to
	   the cluster's monitoring stack, promql:query belongs to the viewer role, and the chart's demo
	   default makes every visitor a viewer. One wide range query is a great deal of upstream work,
	   and nothing bounded how many a caller could ask for. */
	subject, _ := SubjectFrom(r.Context())
	if !s.rateLimitAllow(r.Context(), rateLimitPromQL, s.cfg.RateLimit.PromQLPerMinute, promqlRateLimitKey(subject)) {
		writeRateLimited(w, promqlRateLimitDetail)
		return
	}
	var req promQLQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request", "body must be JSON with a non-empty \"query\"")
		return
	}
	ts := time.Time{}
	if req.Time != nil {
		ts = *req.Time
	}
	raw, err := s.prom.Query(r.Context(), req.Query, ts)
	s.writePromResult(w, raw, err)
}

func (s *Server) handlePromQLQueryRange(w http.ResponseWriter, r *http.Request) {
	if s.prom == nil {
		writeProblem(w, http.StatusServiceUnavailable, "prometheus not configured",
			"set prometheus.url in the console config (Helm: console.prometheus.url)")
		return
	}
	/* Rate limited BEFORE the query leaves for Prometheus: this route forwards ARBITRARY PromQL to
	   the cluster's monitoring stack, promql:query belongs to the viewer role, and the chart's demo
	   default makes every visitor a viewer. One wide range query is a great deal of upstream work,
	   and nothing bounded how many a caller could ask for. */
	subject, _ := SubjectFrom(r.Context())
	if !s.rateLimitAllow(r.Context(), rateLimitPromQL, s.cfg.RateLimit.PromQLPerMinute, promqlRateLimitKey(subject)) {
		writeRateLimited(w, promqlRateLimitDetail)
		return
	}
	var req promQLRangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Query == "" {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			"body must be JSON with \"query\", RFC3339 \"start\"/\"end\", and \"step\" in nanoseconds")
		return
	}
	raw, err := s.prom.QueryRange(r.Context(), req.Query, req.Start, req.End, time.Duration(req.Step))
	s.writePromResult(w, raw, err)
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
		var ue *promql.UpstreamError
		if errors.As(err, &ue) {
			// Forward Prometheus's own error envelope (e.g. PromQL parse errors)
			// with its status so the PromQL Console can show it verbatim.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.WriteHeader(ue.Status)
			_, _ = w.Write(ue.Body) //nolint:gosec // G705: Prometheus JSON error envelope, served as application/json with nosniff, never rendered as HTML
			return
		}
		// Prometheus' OWN envelope is forwarded above (that is the point of the branch); this branch
		// is a transport failure, whose text names the upstream URL and address.
		slog.Error("prometheus request failed", "error", err) //nolint:gosec // G706: structured slog fields
		writeProblem(w, http.StatusBadGateway, "prometheus error", "prometheus did not answer")
	}
}
