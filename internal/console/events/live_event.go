// Package events turns the controller's WatchEvents gRPC stream into the browser-facing "live"
// feed.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
)

// ErrUnknownPayload is returned by ToLiveEvent for a nil event, or one whose
// oneof payload this build does not know (a controller newer than this console).
// Callers log and drop the event; it never tears down the stream.
var ErrUnknownPayload = errors.New("event has no known payload")

// Severity values. Fixed set: the Live page filters on them and
// web/src/lib/types.ts mirrors them by hand.
const (
	SeverityInfo  = "info"
	SeverityWarn  = "warn"
	SeverityError = "error"
)

// Event type values, identical to the labels the controller's own eventType()
// helper uses for ControllerEventsPublished.
const (
	TypeTopologyChanged    = "topology_changed"
	TypeCheckObserved      = "check_observed"
	TypeMTRTriggered       = "mtr_triggered"
	TypeMTRCompleted       = "mtr_completed"
	TypeDiagnosticProgress = "diagnostic_progress"
)

// scopeCluster is the Scope for an event that is about neither one node nor one
// node pair.
const scopeCluster = "cluster"

// LiveEvent is the browser-facing JSON projection of pb.Event, mirrored by hand
// in web/src/lib/types.ts (repo convention: no codegen).
type LiveEvent struct {
	// ID is "<seq>-<unixNano>", built only from controller-assigned values so every console replica
	// derives the SAME id for the same pb.Event.
	ID        string          `json:"id"`
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	Severity  string          `json:"severity"`
	Scope     string          `json:"scope"`
	Timestamp time.Time       `json:"timestamp"`
	Summary   string          `json:"summary"`
	Details   json.RawMessage `json:"details"`
}

// Details payloads: one struct per event type, so the JSON keys are a compiled contract rather than
// a map literal.
type topologyChangedDetails struct {
	Reason   string `json:"reason"`
	NodeName string `json:"nodeName"`
	AgentID  string `json:"agentId"`
	Zone     string `json:"zone"`
}

type checkObservedDetails struct {
	TaskID     string `json:"taskId"`
	CheckType  string `json:"checkType"`
	Plane      string `json:"plane"`
	Success    bool   `json:"success"`
	DurationNs int64  `json:"durationNs"`
	Error      string `json:"error"`
}

type mtrTriggeredDetails struct {
	TaskID string `json:"taskId"`
}

type mtrHopDetails struct {
	Number    int32   `json:"number"`
	IP        string  `json:"ip"`
	Hostname  string  `json:"hostname"`
	RttNs     int64   `json:"rttNs"`
	LossRatio float64 `json:"lossRatio"`
}

type mtrCompletedDetails struct {
	TaskID  string          `json:"taskId"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Hops    []mtrHopDetails `json:"hops"`
}

type diagnosticProgressDetails struct {
	TaskID    string `json:"taskId"`
	CheckType string `json:"checkType"`
	State     string `json:"state"`
}

// ToLiveEvent projects a controller event onto the browser-facing shape; the error case callers
// must handle is ErrUnknownPayload: a nil event.
func ToLiveEvent(ev *pb.Event) (LiveEvent, error) {
	if ev == nil || ev.GetPayload() == nil {
		return LiveEvent{}, ErrUnknownPayload
	}

	// The timestamp comes from the controller and nowhere else; a nil Timestamp yields the Unix epoch,
	// which looks wrong but is IDENTICAL on every replica.
	ts := ev.GetTimestamp().AsTime().UTC().Truncate(time.Microsecond)

	out := LiveEvent{
		ID:        fmt.Sprintf("%d-%d", ev.GetSeq(), ts.UnixNano()),
		Seq:       ev.GetSeq(),
		Timestamp: ts,
	}

	var details any
	switch {
	case ev.GetTopologyChanged() != nil:
		p := ev.GetTopologyChanged()
		out.Type = TypeTopologyChanged
		out.Severity = SeverityInfo
		out.Scope = p.GetNodeName()
		if out.Scope == "" {
			out.Scope = scopeCluster
		}
		out.Summary = topologySummary(p)
		details = topologyChangedDetails{
			Reason:   p.GetReason(),
			NodeName: p.GetNodeName(),
			AgentID:  p.GetAgentId(),
			Zone:     p.GetZone(),
		}
	case ev.GetCheckObserved() != nil:
		p := ev.GetCheckObserved()
		out.Type = TypeCheckObserved
		out.Severity = successSeverity(p.GetSuccess())
		out.Scope = pairScope(p.GetSourceNode(), p.GetDestinationNode())
		out.Summary = fmt.Sprintf("%s check %s %s", p.GetCheckType(), out.Scope, outcomeWord(p.GetSuccess()))
		details = checkObservedDetails{
			TaskID:     p.GetTaskId(),
			CheckType:  p.GetCheckType(),
			Plane:      p.GetPlane(),
			Success:    p.GetSuccess(),
			DurationNs: p.GetDurationNs(),
			Error:      p.GetError(),
		}
	case ev.GetMtrTriggered() != nil:
		p := ev.GetMtrTriggered()
		out.Type = TypeMTRTriggered
		out.Severity = SeverityInfo
		out.Scope = pairScope(p.GetSourceNode(), p.GetDestinationNode())
		out.Summary = fmt.Sprintf("mtr triggered %s", out.Scope)
		details = mtrTriggeredDetails{TaskID: p.GetTaskId()}
	case ev.GetMtrCompleted() != nil:
		p := ev.GetMtrCompleted()
		out.Type = TypeMTRCompleted
		out.Severity = successSeverity(p.GetSuccess())
		out.Scope = pairScope(p.GetSourceNode(), p.GetDestinationNode())
		out.Summary = fmt.Sprintf("mtr %s %s with %d hops", out.Scope, outcomeWord(p.GetSuccess()), len(p.GetHops()))
		details = mtrCompletedDetails{
			TaskID:  p.GetTaskId(),
			Success: p.GetSuccess(),
			Error:   p.GetError(),
			Hops:    hopDetails(p.GetHops()),
		}
	case ev.GetDiagnosticProgress() != nil:
		p := ev.GetDiagnosticProgress()
		out.Type = TypeDiagnosticProgress
		out.Severity = stateSeverity(p.GetState())
		out.Scope = pairScope(p.GetSourceNode(), p.GetDestinationNode())
		out.Summary = fmt.Sprintf("%s diagnostic %s %s", p.GetCheckType(), out.Scope, p.GetState())
		details = diagnosticProgressDetails{
			TaskID:    p.GetTaskId(),
			CheckType: p.GetCheckType(),
			State:     p.GetState(),
		}
	default:
		return LiveEvent{}, fmt.Errorf("%w: %T", ErrUnknownPayload, ev.GetPayload())
	}

	data, err := json.Marshal(details)
	if err != nil {
		return LiveEvent{}, fmt.Errorf("marshal %s details: %w", out.Type, err)
	}
	out.Details = data
	return out, nil
}

// pairScope renders the "<src>→<dst>" scope every non-topology event uses.
func pairScope(src, dst string) string { return src + PairArrow + dst }

// PairArrow is U+2192, the ONE separator a pair scope is written with. Mirrored by
// web/src/lib/utils.ts PAIR_ARROW.
const PairArrow = "→"

// pairSeparator is every separator a keyboard CAN produce, plus the arrow itself, each with any
// surrounding whitespace collapsed into it. Whitespace ALONE is deliberately not a separator: a
// scope is not always a hostname, and reading a bare space as an arrow would cut a legitimate name
// in half. A hyphen not followed by ">" is left alone, which keeps "edge-gw-01" one name. Mirrored
// by web/src/lib/utils.ts TYPEABLE_SEPARATOR.
var pairSeparator = regexp.MustCompile(`\s*(?:→|-+>|=+>|>)\s*`)

// NormalizePairScope rewrites whatever an operator typed into the canonical pair scope, so a direct
// API consumer that sends "a->b" gets the same rows as one that sends "a→b". A single name comes
// back trimmed and otherwise untouched. Mirrors web/src/lib/utils.ts normalizePairInput.
func NormalizePairScope(raw string) string {
	return pairSeparator.ReplaceAllString(strings.TrimSpace(raw), PairArrow)
}

// topologySummary keeps the node name out of the sentence when the controller
// did not attribute the change to one node.
func topologySummary(p *pb.TopologyChanged) string {
	if p.GetNodeName() == "" {
		return fmt.Sprintf("topology changed: %s", p.GetReason())
	}
	return fmt.Sprintf("topology changed: %s (%s)", p.GetReason(), p.GetNodeName())
}

func successSeverity(success bool) string {
	if success {
		return SeverityInfo
	}
	return SeverityError
}

func outcomeWord(success bool) string {
	if success {
		return "succeeded"
	}
	return "failed"
}

// stateSeverity maps DiagnosticProgress.state. A dispatch is informational; a
// timeout or an error is a warning, because the terminal failure of a check is
// reported separately as check_observed/mtr_completed with severity error.
func stateSeverity(state string) string {
	if state == "dispatched" {
		return SeverityInfo
	}
	return SeverityWarn
}

// hopDetails always returns a non-nil slice so the JSON carries "hops":[]
// rather than "hops":null — the Live page maps over it directly.
func hopDetails(hops []*pb.MTRHop) []mtrHopDetails {
	out := make([]mtrHopDetails, 0, len(hops))
	for _, h := range hops {
		out = append(out, mtrHopDetails{
			Number:    h.GetNumber(),
			IP:        h.GetIp(),
			Hostname:  h.GetHostname(),
			RttNs:     h.GetRttNs(),
			LossRatio: h.GetLossRatio(),
		})
	}
	return out
}
