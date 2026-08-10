package model

import (
	"net"
	"time"
)

type CheckType string

const (
	CheckTCP  CheckType = "tcp"
	CheckUDP  CheckType = "udp"
	CheckICMP CheckType = "icmp"
	CheckDNS  CheckType = "dns"
	CheckHTTP CheckType = "http"
	CheckMTR  CheckType = "mtr"
	// CheckExternal is the result type of the CONTINUOUS external checker; it lives here rather than
	// in the checker package because an external result travels the same CheckResult path.
	CheckExternal CheckType = "external"
)

type CheckResult struct {
	Type        CheckType     `json:"type"`
	Success     bool          `json:"success"`
	Source      string        `json:"source"`
	Destination string        `json:"destination"`
	SourceZone  string        `json:"sourceZone"`
	DestZone    string        `json:"destZone"`
	Duration    time.Duration `json:"duration"`
	Error       string        `json:"error,omitempty"`
	Timestamp   time.Time     `json:"timestamp"`
	Details     any           `json:"details,omitempty"`
}

type TCPDetails struct {
	ConnectTime time.Duration `json:"connectTime"`
	TotalTime   time.Duration `json:"totalTime"`
}

type UDPDetails struct {
	PacketsSent int           `json:"packetsSent"`
	PacketsRecv int           `json:"packetsRecv"`
	LossRatio   float64       `json:"lossRatio"`
	MeanRTT     time.Duration `json:"meanRtt"`
	Variance    time.Duration `json:"variance"`
	Jitter      time.Duration `json:"jitter"`
}

type ICMPDetails struct {
	RTT       time.Duration `json:"rtt"`
	LossRatio float64       `json:"lossRatio"`
}

type DNSDetails struct {
	Host        string        `json:"host"`
	Resolver    string        `json:"resolver"`
	Duration    time.Duration `json:"duration"`
	ResolvedIPs []net.IP      `json:"resolvedIps,omitempty"`
}

type HTTPDetails struct {
	URL          string        `json:"url"`
	Method       string        `json:"method"`
	StatusCode   int           `json:"statusCode"`
	BodyMismatch bool          `json:"bodyMismatch,omitempty"`
	DNSTime      time.Duration `json:"dnsTime"`
	ConnectTime  time.Duration `json:"connectTime"`
	TLSTime      time.Duration `json:"tlsTime"`
	TTFBTime     time.Duration `json:"ttfbTime"`
	TotalTime    time.Duration `json:"totalTime"`
}

// ExternalDenyReason is why the allowlist refused a probe; it is a CLOSED set because it becomes a
// metric label value.
type ExternalDenyReason string

const (
	// ExternalDenyCIDR: the destination resolved to an address the allowlist
	// does not permit, including zone-scoped addresses.
	ExternalDenyCIDR ExternalDenyReason = "cidr"
	// ExternalDenyResolve: the destination could not be turned into an address
	// at all -- resolution failed, returned nothing, or the host was blank.
	ExternalDenyResolve ExternalDenyReason = "resolve"
	// ExternalDenyDisabled: this agent has no allowlist or resolver configured,
	// so external checks are off and every destination is refused.
	ExternalDenyDisabled ExternalDenyReason = "disabled"
)

// ExternalDetails is one target's outcome inside an external CheckResult; name is the ONLY field
// that may ever become a metric label value (see the ExternalTarget comment in
// api/proto/kconmon.proto).
type ExternalDetails struct {
	DefinitionID string    `json:"definitionId,omitempty"`
	Name         string    `json:"name"`
	CheckType    CheckType `json:"checkType"`
	Success      bool      `json:"success"`
	// Denied marks a probe that never happened because the allowlist refused
	// the destination. It is not a network failure and it is counted apart, on
	// external_denied_total rather than external_results_total.
	Denied bool `json:"denied,omitempty"`
	// DenyReason is set only when Denied. It is the closed-set label value of
	// external_denied_total.
	DenyReason  ExternalDenyReason `json:"denyReason,omitempty"`
	Duration    time.Duration      `json:"duration"`
	Error       string             `json:"error,omitempty"`
	StatusCode  int                `json:"statusCode,omitempty"`
	RTT         time.Duration      `json:"rtt,omitempty"`
	LossRatio   float64            `json:"lossRatio,omitempty"`
	ResolvedIPs int                `json:"resolvedIps,omitempty"`
}

type MTRHop struct {
	Number    int           `json:"number"`
	IP        string        `json:"ip"`
	Hostname  string        `json:"hostname,omitempty"`
	RTT       time.Duration `json:"rtt"`
	LossRatio float64       `json:"lossRatio"`
}

type MTRDetails struct {
	Target string   `json:"target"`
	Hops   []MTRHop `json:"hops"`
}
