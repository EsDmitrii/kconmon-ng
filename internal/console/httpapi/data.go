package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
	"github.com/EsDmitrii/kconmon-ng/internal/console/matrix"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
)

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
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
		writeProblem(w, http.StatusBadGateway, "controller error", err.Error())
		return
	}
	writeJSON(w, topo)
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
		writeProblem(w, http.StatusBadGateway, "matrix computation failed", err.Error())
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

func (s *Server) handlePromQLQuery(w http.ResponseWriter, r *http.Request) {
	if s.prom == nil {
		writeProblem(w, http.StatusServiceUnavailable, "prometheus not configured",
			"set prometheus.url in the console config (Helm: console.prometheus.url)")
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
		writeProblem(w, http.StatusBadGateway, "prometheus error", err.Error())
	}
}
