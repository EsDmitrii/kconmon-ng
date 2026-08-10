package controller

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/metrics"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// externalSubscriberBuffer is the depth of an agent's assignment channel.
// Assignments are absolute, so a queued one is always superseded by the next;
// the buffer only has to absorb a burst of PUTs while a stream drains.
const externalSubscriberBuffer = 16

// validExternalCheckTypes is the set of check types a CONTINUOUS external
// check may use. udp and mtr are deliberately absent — see the
// ExternalCheckSpec comment in api/proto/kconmon.proto.
var validExternalCheckTypes = map[string]struct{}{
	"tcp":  {},
	"icmp": {},
	"dns":  {},
	"http": {},
}

// ExternalCheckManager owns the continuous external-check assignment of every agent and fans
// changes out to the agents' WatchExternalChecks streams; it is TaskManager's sibling and copies
// its concurrency discipline verbatim.
type ExternalCheckManager struct {
	mu sync.Mutex
	// assignments holds only the SPECS per agent, never a timestamp: the timestamp is stamped per
	// send.
	assignments map[string][]*pb.ExternalCheckSpec
	subscribers map[string]chan *pb.ExternalCheckAssignment
}

func NewExternalCheckManager() *ExternalCheckManager {
	return &ExternalCheckManager{
		assignments: make(map[string][]*pb.ExternalCheckSpec),
		subscribers: make(map[string]chan *pb.ExternalCheckAssignment),
	}
}

// Subscribe registers an agent's assignment channel and returns it alongside a cleanup func that
// removes the subscription; the cleanup func is idempotent and must be called when the
// WatchExternalChecks stream ends.
func (m *ExternalCheckManager) Subscribe(agentID string) (updates <-chan *pb.ExternalCheckAssignment, cleanup func()) {
	ch := make(chan *pb.ExternalCheckAssignment, externalSubscriberBuffer)

	m.mu.Lock()
	m.subscribers[agentID] = ch
	m.mu.Unlock()

	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			m.mu.Lock()
			if cur, ok := m.subscribers[agentID]; ok && cur == ch {
				delete(m.subscribers, agentID)
			}
			m.mu.Unlock()
		})
	}
	return ch, cleanup
}

// Assignment returns the agent's CURRENT complete assignment, freshly stamped; an agent with
// nothing assigned gets an empty (not nil) assignment.
func (m *ExternalCheckManager) Assignment(agentID string) *pb.ExternalCheckAssignment {
	m.mu.Lock()
	specs := m.assignments[agentID]
	m.mu.Unlock()
	return newAssignment(specs)
}

// Apply installs desired as the complete assignment state and pushes ONLY the agents whose
// assignment actually changed.
func (m *ExternalCheckManager) Apply(desired map[string][]*pb.ExternalCheckSpec) (changed int) {
	type push struct {
		agentID string
		specs   []*pb.ExternalCheckSpec
		ch      chan *pb.ExternalCheckAssignment
	}

	// Collect the sends under the mutex, perform them after releasing it.
	var pushes []push

	m.mu.Lock()
	for agentID, specs := range desired {
		if len(specs) == 0 {
			continue // handled by the removal sweep below
		}
		if sameSpecs(m.assignments[agentID], specs) {
			continue
		}
		m.assignments[agentID] = specs
		pushes = append(pushes, push{agentID: agentID, specs: specs, ch: m.subscribers[agentID]})
	}
	for agentID := range m.assignments {
		if len(desired[agentID]) > 0 {
			continue
		}
		delete(m.assignments, agentID)
		pushes = append(pushes, push{agentID: agentID, ch: m.subscribers[agentID]})
	}
	m.mu.Unlock()

	for i := range pushes {
		changed++
		if pushes[i].ch == nil {
			// No open stream: the agent picks the new state up on its initial
			// send when it (re)subscribes.
			continue
		}
		// Non-blocking: a subscriber whose stream died mid-push must never
		// stall the fan-out to the others. A dropped assignment is corrected by
		// the next change or by the Console's reconcile ticker.
		select {
		case pushes[i].ch <- newAssignment(pushes[i].specs):
		default:
			slog.Warn("dropping external-check assignment, subscriber channel full", "agent", pushes[i].agentID)
		}
	}

	return changed
}

// AssignedCount reports the number of agents with a NON-EMPTY assignment. It
// backs the controller_external_assignments gauge and is used by tests.
func (m *ExternalCheckManager) AssignedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.assignments)
}

// SubscriberCount reports the number of agents with an active
// WatchExternalChecks subscription. Intended for tests and diagnostics.
func (m *ExternalCheckManager) SubscriberCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subscribers)
}

// newAssignment builds a complete assignment message, stamped now. Specs are
// shared, never copied: they are only ever produced by decoding a PUT body and
// are treated as immutable once handed to the manager.
func newAssignment(specs []*pb.ExternalCheckSpec) *pb.ExternalCheckAssignment {
	if specs == nil {
		specs = []*pb.ExternalCheckSpec{}
	}
	return &pb.ExternalCheckAssignment{Specs: specs, Timestamp: timestamppb.Now()}
}

// sameSpecs reports whether two spec lists are identical. Order is significant
// — the HTTP body is an ordered list and reordering it is a legitimate change
// to push — so this is a positional proto.Equal, not a set comparison.
func sameSpecs(a, b []*pb.ExternalCheckSpec) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// externalChecksRequest is the PUT /api/v1/external-checks body; it carries the WHOLE desired
// state, never a delta, mirroring ExternalCheckAssignment.
type externalChecksRequest struct {
	Agents map[string][]externalCheckSpecJSON `json:"agents"`
}

type externalCheckSpecJSON struct {
	DefinitionID string             `json:"definitionId"`
	Target       externalTargetJSON `json:"target"`
	CheckType    string             `json:"checkType"`
	IntervalNs   int64              `json:"intervalNs"`
	TimeoutNs    int64              `json:"timeoutNs"`
	Params       json.RawMessage    `json:"params,omitempty"`
}

type externalTargetJSON struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Port    uint32 `json:"port"`
}

// externalChecksResponse is the 200 body: how many agents ended up with an
// assignment, how many were actually pushed (0 on a retried identical PUT),
// and the agent IDs the controller does not know about.
type externalChecksResponse struct {
	Agents  int      `json:"agents"`
	Changed int      `json:"changed"`
	Unknown []string `json:"unknown"`
}

// ExternalChecksHandler serves PUT /api/v1/external-checks: it validates the
// desired state, drops agent IDs the registry does not know, and hands the
// result to the ExternalCheckManager for change detection and fan-out.
type ExternalChecksHandler struct {
	registry       *Registry
	manager        *ExternalCheckManager
	metrics        *metrics.PrometheusMetrics
	leaderElection bool
	isLeader       func() bool
}

func NewExternalChecksHandler(
	registry *Registry,
	manager *ExternalCheckManager,
	m *metrics.PrometheusMetrics,
	leaderElection bool,
	isLeader func() bool,
) *ExternalChecksHandler {
	return &ExternalChecksHandler{
		registry:       registry,
		manager:        manager,
		metrics:        m,
		leaderElection: leaderElection,
		isLeader:       isLeader,
	}
}

func (h *ExternalChecksHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only the leader holds an authoritative view of registered agents and their streams; non-leaders
	// cannot fan out.
	if h.leaderElection && (h.isLeader == nil || !h.isLeader()) {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
		return
	}

	var req externalChecksRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	agents := h.registry.GetAll()
	known := make(map[string]struct{}, len(agents))
	for i := range agents {
		known[agents[i].ID] = struct{}{}
	}

	desired := make(map[string][]*pb.ExternalCheckSpec, len(req.Agents))
	unknown := []string{}

	for agentID, specs := range req.Agents {
		if _, ok := known[agentID]; !ok {
			// The Console's topology view can legitimately lag the registry, so
			// an unfamiliar agent ID is a warning and not a 400: rejecting the
			// whole desired state would block every other agent's assignment.
			slog.Warn("ignoring external checks for unknown agent", "agent", agentID, "specs", len(specs))
			unknown = append(unknown, agentID)
			continue
		}

		converted := make([]*pb.ExternalCheckSpec, 0, len(specs))
		for i := range specs {
			spec, err := specs[i].toProto()
			if err != nil {
				http.Error(w, "agent "+agentID+": "+err.Error(), http.StatusBadRequest)
				return
			}
			converted = append(converted, spec)
		}
		desired[agentID] = converted
	}

	changed := h.manager.Apply(desired)
	assigned := h.manager.AssignedCount()
	h.metrics.ControllerExternalAssignments.WithLabelValues().Set(float64(assigned))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(externalChecksResponse{
		Agents:  assigned,
		Changed: changed,
		Unknown: unknown,
	})
}

// toProto validates one spec and converts it to its proto form; only the check type is validated
// here: everything else is the agent's own business.
func (s *externalCheckSpecJSON) toProto() (*pb.ExternalCheckSpec, error) {
	if _, ok := validExternalCheckTypes[s.CheckType]; !ok {
		return nil, &externalValidationError{msg: "checkType must be one of tcp, icmp, dns, http"}
	}

	params, err := canonicalJSON(s.Params)
	if err != nil {
		return nil, &externalValidationError{msg: "params is not valid JSON"}
	}

	return &pb.ExternalCheckSpec{
		DefinitionId: s.DefinitionID,
		Target: &pb.ExternalTarget{
			Name:    s.Target.Name,
			Kind:    s.Target.Kind,
			Address: s.Target.Address,
			Port:    s.Target.Port,
		},
		CheckType:  s.CheckType,
		IntervalNs: s.IntervalNs,
		TimeoutNs:  s.TimeoutNs,
		ParamsJson: params,
	}, nil
}

// externalValidationError is a 400-worthy body problem.
type externalValidationError struct{ msg string }

func (e *externalValidationError) Error() string { return e.msg }

// canonicalJSON re-encodes raw through Go's map encoder, which sorts object keys; without it, two
// semantically identical params objects that differ only in key order would compare unequal and
// push a spurious assignment to every agent on every reconcile tick.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
