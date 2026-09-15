package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAgentInfoIsExternalReadsTheSharedLabel pins the one place the "external" verdict is computed:
// the kconmon-ng.io/external label set to "true" by identity.go, nothing else.
func TestAgentInfoIsExternalReadsTheSharedLabel(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "nil labels", labels: nil, want: false},
		{name: "empty labels", labels: map[string]string{}, want: false},
		{name: "external true", labels: map[string]string{LabelExternal: "true"}, want: true},
		{name: "external false", labels: map[string]string{LabelExternal: "false"}, want: false},
		{name: "unrelated label", labels: map[string]string{"role": "worker"}, want: false},
		{name: "host-network is not external", labels: map[string]string{LabelHostNetwork: "true"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := AgentInfo{ID: "a-1", Labels: tc.labels}
			if got := a.IsExternal(); got != tc.want {
				t.Fatalf("IsExternal() = %v, want %v for labels %v", got, tc.want, tc.labels)
			}
		})
	}
}

// TestSharedVocabularyValues pins the wire strings: identity.go writes LabelExternal, the chart may
// write LabelHostNetwork, and the agent emits plane capabilities under CapabilityPlanePrefix.
func TestSharedVocabularyValues(t *testing.T) {
	if LabelExternal != "kconmon-ng.io/external" {
		t.Fatalf("LabelExternal = %q", LabelExternal)
	}
	if LabelHostNetwork != "kconmon-ng.io/host-network" {
		t.Fatalf("LabelHostNetwork = %q", LabelHostNetwork)
	}
	if CapabilityPlanePrefix != "plane:" {
		t.Fatalf("CapabilityPlanePrefix = %q", CapabilityPlanePrefix)
	}
}

// TestAgentInfoPortsAreOmittedWhenUnknown keeps GET /api/v1/topology byte-identical for agents
// older than 2.4.0: zero ports (unknown) never appear in the JSON, reported ports do.
func TestAgentInfoPortsAreOmittedWhenUnknown(t *testing.T) {
	old, err := json.Marshal(AgentInfo{ID: "a-1", NodeName: "node-a"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"httpPort", "udpPort", "metricsPort"} {
		if strings.Contains(string(old), key) {
			t.Fatalf("zero %s leaked into JSON: %s", key, old)
		}
	}

	raw, err := json.Marshal(AgentInfo{ID: "a-2", HTTPPort: 18080, UDPPort: 19090, MetricsPort: 19091})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AgentInfo
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.HTTPPort != 18080 || back.UDPPort != 19090 || back.MetricsPort != 19091 {
		t.Fatalf("ports did not round-trip: %+v from %s", back, raw)
	}
	for _, key := range []string{`"httpPort":18080`, `"udpPort":19090`, `"metricsPort":19091`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("expected %s in %s", key, raw)
		}
	}
}
