package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

const (
	defaultDiagnosticsTimeout = 60 * time.Second
	maxDiagnosticsTimeout     = 120 * time.Second
)

// validCheckTypes is the set of diagnostic check types the API accepts. The
// plane field is not validated here (host plane arrives with Epic A); it is
// forwarded verbatim.
var validCheckTypes = map[string]struct{}{
	string(model.CheckTCP):  {},
	string(model.CheckUDP):  {},
	string(model.CheckICMP): {},
	string(model.CheckDNS):  {},
	string(model.CheckHTTP): {},
	string(model.CheckMTR):  {},
}

// TaskDispatcher dispatches a diagnostic task to an agent and waits for the
// result. Implemented by *TaskManager; kept as an interface so the handler can
// be tested without a live gRPC stream.
type TaskDispatcher interface {
	Dispatch(ctx context.Context, agentID string, req *pb.TaskRequest) (*pb.TaskResult, error)
}

// diagnosticsRequest is the POST /api/v1/diagnostics body.
type diagnosticsRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Type        string `json:"type"`
	Plane       string `json:"plane"`
}

// DiagnosticsHandler serves POST /api/v1/diagnostics: it resolves the source
// and destination nodes to registered agents, dispatches an on-demand task to
// the source agent, and returns the resulting model.CheckResult verbatim.
type DiagnosticsHandler struct {
	registry       *Registry
	dispatcher     TaskDispatcher
	metrics        *metrics.PrometheusMetrics
	leaderElection bool
	isLeader       func() bool
}

func NewDiagnosticsHandler(
	registry *Registry,
	dispatcher TaskDispatcher,
	m *metrics.PrometheusMetrics,
	leaderElection bool,
	isLeader func() bool,
) *DiagnosticsHandler {
	return &DiagnosticsHandler{
		registry:       registry,
		dispatcher:     dispatcher,
		metrics:        m,
		leaderElection: leaderElection,
		isLeader:       isLeader,
	}
}

func (h *DiagnosticsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only the leader holds an authoritative view of registered agents and
	// their streams; non-leaders cannot dispatch.
	if h.leaderElection && (h.isLeader == nil || !h.isLeader()) {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	var req diagnosticsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.Source == "" || req.Destination == "" || req.Type == "" {
		http.Error(w, "source, destination and type are required", http.StatusBadRequest)
		return
	}
	if _, ok := validCheckTypes[req.Type]; !ok {
		http.Error(w, "invalid check type", http.StatusBadRequest)
		return
	}

	plane := req.Plane
	if plane == "" {
		plane = "pod"
	}

	source, ok := h.registry.GetByNodeName(req.Source)
	if !ok {
		h.count(req.Type, "not_found")
		http.Error(w, "no agent registered on source node", http.StatusNotFound)
		return
	}
	destination, ok := h.registry.GetByNodeName(req.Destination)
	if !ok {
		h.count(req.Type, "not_found")
		http.Error(w, "no agent registered on destination node", http.StatusNotFound)
		return
	}

	timeout := h.resolveTimeout(r)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	task := &pb.TaskRequest{
		CheckType: req.Type,
		Target:    agentInfoToProto(destination),
		Plane:     plane,
	}

	res, err := h.dispatcher.Dispatch(ctx, source.ID, task)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			h.count(req.Type, "timeout")
			http.Error(w, "diagnostics dispatch timed out", http.StatusGatewayTimeout)
		case errors.Is(err, ErrAgentNotSubscribed):
			// The source agent is registered but has no active task stream, so
			// there is no agent able to run the check.
			h.count(req.Type, "not_found")
			http.Error(w, "source agent has no active diagnostics stream", http.StatusNotFound)
		default:
			h.count(req.Type, "error")
			http.Error(w, "diagnostics dispatch failed", http.StatusBadGateway)
		}
		return
	}

	h.count(req.Type, "ok")
	w.Header().Set("Content-Type", "application/json")
	// details_json is the agent's serialized model.CheckResult; return it
	// verbatim so the CLI sees exactly what the agent produced.
	if _, err := w.Write(res.GetDetailsJson()); err != nil {
		return
	}
}

// resolveTimeout returns the dispatch timeout: the ?timeout= query value in
// seconds, defaulting to 60s and capped at 120s. Invalid values fall back to
// the default.
func (h *DiagnosticsHandler) resolveTimeout(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("timeout")
	if raw == "" {
		return defaultDiagnosticsTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return defaultDiagnosticsTimeout
	}
	timeout := time.Duration(secs) * time.Second
	if timeout > maxDiagnosticsTimeout {
		return maxDiagnosticsTimeout
	}
	return timeout
}

func (h *DiagnosticsHandler) count(checkType, result string) {
	h.metrics.ControllerDiagnostics.WithLabelValues(checkType, result).Inc()
}
