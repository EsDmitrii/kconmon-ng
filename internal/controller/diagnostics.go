package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
	"github.com/google/uuid"
)

const (
	defaultDiagnosticsTimeout = 60 * time.Second
	maxDiagnosticsTimeout     = 120 * time.Second
)

const (
	// destinationKindNode resolves Destination against the agent registry; it is the default and the
	// only kind that existed.
	destinationKindNode = "node"
	// destinationKindExternal skips the registry: DestinationAddress carries the
	// address to probe and Destination, if present, only names it.
	destinationKindExternal = "external"

	// capabilityExternalChecks is the AgentMeta.capabilities flag an agent build advertises when it
	// can probe a destination that is not a registered agent; a pre-M4 agent advertises nothing and
	// ignores TaskRequest.external_target silently.
	capabilityExternalChecks = "external-checks"

	// externalTargetKindHost is the ExternalTarget.kind this endpoint produces.
	// The HTTP body carries only an address, and an address without a scheme is
	// a host; "url" targets arrive with the operator-defined target objects.
	externalTargetKindHost = "host"
	// externalTargetDefaultPort is ExternalTarget.port's "use the check type's
	// own default" sentinel: the HTTP body has no port field yet.
	externalTargetDefaultPort = 0
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
	// DestinationKind is "node" or "external".
	DestinationKind    string `json:"destinationKind,omitempty"`
	DestinationAddress string `json:"destinationAddress,omitempty"`
}

// EventPublisher is the seam DiagnosticsHandler uses to emit domain events for
// each on-demand diagnostic dispatch. Satisfied by *GRPCServer.
type EventPublisher interface {
	PublishEvent(ev *pb.Event)
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
	events         EventPublisher
}

func NewDiagnosticsHandler(
	registry *Registry,
	dispatcher TaskDispatcher,
	m *metrics.PrometheusMetrics,
	leaderElection bool,
	isLeader func() bool,
	events EventPublisher,
) *DiagnosticsHandler {
	return &DiagnosticsHandler{
		registry:       registry,
		dispatcher:     dispatcher,
		metrics:        m,
		leaderElection: leaderElection,
		isLeader:       isLeader,
		events:         events,
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

	destKind := req.DestinationKind
	if destKind == "" {
		destKind = destinationKindNode
	}
	switch destKind {
	case destinationKindNode, destinationKindExternal:
	default:
		http.Error(w, "destinationKind must be node or external", http.StatusBadRequest)
		return
	}

	// A node destination is named; an external one is addressed. Either way
	// something must identify the far end, so both fields empty is a 400.
	if req.Source == "" || req.Type == "" || (destKind == destinationKindNode && req.Destination == "") {
		http.Error(w, "source, destination and type are required", http.StatusBadRequest)
		return
	}
	if destKind == destinationKindExternal && req.DestinationAddress == "" {
		http.Error(w, "destinationAddress is required when destinationKind is external", http.StatusBadRequest)
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

	// Stamp the task ID here rather than letting Dispatch mint it, so the
	// dispatch-start event published below carries the same ID as the terminal
	// event and a Console can correlate the two halves of one run.
	task := &pb.TaskRequest{
		TaskId:    uuid.NewString(),
		CheckType: req.Type,
		Plane:     plane,
	}

	// destName is what every published event reports as destination_node; the address never appears —
	// name is the only external field allowed to become an identifier downstream.
	destName := req.Destination

	if destKind == destinationKindExternal {
		// Gate on the SOURCE agent: it is the one that has to understand
		// external_target. Answering 501 here beats a mystifying timeout from an
		// old agent that ignored the field.
		if !slices.Contains(source.Capabilities, capabilityExternalChecks) {
			h.count(req.Type, "unsupported")
			http.Error(w,
				"agent on node "+source.NodeName+" does not support external destinations",
				http.StatusNotImplemented)
			return
		}
		if destName == "" {
			destName = req.DestinationAddress
		}
		// Exactly one of Target/ExternalTarget is ever populated: the agent
		// treats both-set as malformed.
		task.ExternalTarget = &pb.ExternalTarget{
			Name:    destName,
			Kind:    externalTargetKindHost,
			Address: req.DestinationAddress,
			Port:    externalTargetDefaultPort,
		}
	} else {
		destination, found := h.registry.GetByNodeName(req.Destination)
		if !found {
			h.count(req.Type, "not_found")
			http.Error(w, "no agent registered on destination node", http.StatusNotFound)
			return
		}
		task.Target = agentInfoToProto(destination)
	}

	timeout := h.resolveTimeout(r)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	h.publishDispatched(task.GetTaskId(), req.Type, req.Source, destName)

	res, err := h.dispatcher.Dispatch(ctx, source.ID, task)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			h.count(req.Type, "timeout")
			h.publishProgress(task.GetTaskId(), req.Type, req.Source, destName, "timeout")
			http.Error(w, "diagnostics dispatch timed out", http.StatusGatewayTimeout)
		case errors.Is(err, ErrAgentNotSubscribed):
			// The source agent is registered but has no active task stream, so
			// there is no agent able to run the check.
			h.count(req.Type, "not_found")
			h.publishProgress(task.GetTaskId(), req.Type, req.Source, destName, "error")
			http.Error(w, "source agent has no active diagnostics stream", http.StatusNotFound)
		default:
			h.count(req.Type, "error")
			h.publishProgress(task.GetTaskId(), req.Type, req.Source, destName, "error")
			http.Error(w, "diagnostics dispatch failed", http.StatusBadGateway)
		}
		return
	}

	h.count(req.Type, "ok")
	h.publishObserved(task.GetTaskId(), req.Type, plane, req.Source, destName, res.GetDetailsJson())
	w.Header().Set("Content-Type", "application/json")
	// nosniff pins the declared JSON type so no browser will ever interpret
	// this response as HTML, closing the theoretical XSS vector gosec's taint
	// analysis flags for echoing agent-produced bytes.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// details_json is the agent's serialized model.CheckResult; return it
	// verbatim so the CLI sees exactly what the agent produced.
	if _, err := w.Write(res.GetDetailsJson()); err != nil { //nolint:gosec // G705: JSON response with nosniff, never rendered as HTML
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

// publishDispatched announces that a diagnostic was handed to the source agent.
// mtr gets its own MTRTriggered event instead of a generic progress update.
func (h *DiagnosticsHandler) publishDispatched(taskID, checkType, source, destination string) {
	if h.events == nil {
		return
	}
	if checkType == string(model.CheckMTR) {
		h.events.PublishEvent(&pb.Event{Payload: &pb.Event_MtrTriggered{
			MtrTriggered: &pb.MTRTriggered{TaskId: taskID, SourceNode: source, DestinationNode: destination},
		}})
		return
	}
	h.events.PublishEvent(&pb.Event{Payload: &pb.Event_DiagnosticProgress{
		DiagnosticProgress: &pb.DiagnosticProgress{
			TaskId: taskID, CheckType: checkType, SourceNode: source, DestinationNode: destination, State: "dispatched",
		},
	}})
}

func (h *DiagnosticsHandler) publishProgress(taskID, checkType, source, destination, state string) {
	if h.events == nil {
		return
	}
	h.events.PublishEvent(&pb.Event{Payload: &pb.Event_DiagnosticProgress{
		DiagnosticProgress: &pb.DiagnosticProgress{
			TaskId: taskID, CheckType: checkType, SourceNode: source, DestinationNode: destination, State: state,
		},
	}})
}

// publishObserved decodes the agent's CheckResult and emits either a CheckObserved (non-mtr types)
// or an MTRCompleted (mtr, with hops).
func (h *DiagnosticsHandler) publishObserved(taskID, checkType, plane, source, destination string, detailsJSON []byte) {
	if h.events == nil {
		return
	}
	var result model.CheckResult
	if err := json.Unmarshal(detailsJSON, &result); err != nil {
		slog.Warn("failed to decode CheckResult for event publishing", "error", err, "taskId", taskID)
		return
	}

	if checkType == string(model.CheckMTR) {
		h.events.PublishEvent(&pb.Event{Payload: &pb.Event_MtrCompleted{
			MtrCompleted: &pb.MTRCompleted{
				TaskId: taskID, SourceNode: source, DestinationNode: destination,
				Success: result.Success, Error: result.Error, Hops: mtrHopsFromDetails(result.Details),
			},
		}})
		return
	}

	h.events.PublishEvent(&pb.Event{Payload: &pb.Event_CheckObserved{
		CheckObserved: &pb.CheckObserved{
			TaskId: taskID, CheckType: checkType, SourceNode: source, DestinationNode: destination, Plane: plane,
			Success: result.Success, DurationNs: result.Duration.Nanoseconds(), Error: result.Error,
		},
	}})
}

// mtrHopsFromDetails pulls the hop list out of a CheckResult.Details that was decoded into `any`.
func mtrHopsFromDetails(details any) []*pb.MTRHop {
	md, isMap := details.(map[string]any)
	if !isMap {
		return nil
	}
	raw, isSlice := md["hops"].([]any)
	if !isSlice {
		return nil
	}
	hops := make([]*pb.MTRHop, 0, len(raw))
	for _, item := range raw {
		hm, isHop := item.(map[string]any)
		if !isHop {
			continue
		}
		hops = append(hops, &pb.MTRHop{
			Number:    int32(asFloat(hm["number"])),
			Ip:        asString(hm["ip"]),
			Hostname:  asString(hm["hostname"]),
			RttNs:     int64(asFloat(hm["rtt"])),
			LossRatio: asFloat(hm["lossRatio"]),
		})
	}
	return hops
}

func asFloat(v any) float64 {
	f, _ := v.(float64) // json.Unmarshal into any always yields float64 for JSON numbers
	return f
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
