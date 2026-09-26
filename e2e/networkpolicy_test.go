//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestNetworkPolicyBlocksUnnamedAgentEgress runs only in the NetworkPolicy leg, which sets
// KCONMON_NETWORKPOLICY_BLOCKED_ADDR to a Service whose ingress is open and that the agents' allowedCidrs
// admit, but that no agent egress rule names. The agent lets the probe out, so only the agent policy
// can make it fail; a success means the leg is running on a cluster that does not enforce it.
func TestNetworkPolicyBlocksUnnamedAgentEgress(t *testing.T) {
	addr := os.Getenv("KCONMON_NETWORKPOLICY_BLOCKED_ADDR")
	if addr == "" {
		t.Skip("KCONMON_NETWORKPOLICY_BLOCKED_ADDR not set; only the NetworkPolicy leg runs this test")
	}
	base := consoleBaseURL(t)
	agentBase := agentBaseURL(t)

	name := uniqueName("e2e-np-blocked")
	defID := createDefinition(t, base, map[string]any{
		"name":               name,
		"sourceSelection":    "all",
		"destinationKind":    "adhoc",
		"destinationAddress": addr,
		"checkType":          "tcp",
		"plane":              "pod",
		"enabled":            true,
	})
	createSchedule(t, base, map[string]any{
		"definitionId": defID,
		"kind":         "continuous",
		"enabled":      true,
	})

	target := map[string]string{"target": name}
	failed := map[string]string{"target": name, "result": "fail"}
	succeeded := map[string]string{"target": name, "result": "success"}
	pollUntil(t, 180*time.Second, 5*time.Second,
		fmt.Sprintf("a failed kconmon_ng_external_results_total sample for %s (%s) on %s", name, addr, agentBase),
		func() bool {
			text := scrapeAgentMetrics(t, agentBase)
			if text == "" {
				return false
			}
			if denied := metricSamples(text, "kconmon_ng_external_denied_total", target); len(denied) > 0 {
				t.Errorf("%s was refused by the agent allowlist, so the policy was never tested: %v", addr, denied)
				return true
			}
			if ok := metricSamples(text, "kconmon_ng_external_results_total", succeeded); len(ok) > 0 {
				t.Errorf("an agent reached %s, which no agent egress rule names: %v", addr, ok)
				return true
			}
			return len(metricSamples(text, "kconmon_ng_external_results_total", failed)) > 0
		})
}
