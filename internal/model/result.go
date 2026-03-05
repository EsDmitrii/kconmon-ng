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
	URL         string        `json:"url"`
	Method      string        `json:"method"`
	StatusCode  int           `json:"statusCode"`
	DNSTime     time.Duration `json:"dnsTime"`
	ConnectTime time.Duration `json:"connectTime"`
	TLSTime     time.Duration `json:"tlsTime"`
	TTFBTime    time.Duration `json:"ttfbTime"`
	TotalTime   time.Duration `json:"totalTime"`
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
