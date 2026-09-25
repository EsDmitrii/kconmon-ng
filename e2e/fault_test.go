//go:build e2e

package e2e

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fault injection runs iptables inside a kind node container. kind names each node container after the
// node, so the node name from the pod spec is the docker target.

type agentPod struct{ name, ip, node string }

func kubectlOut(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func podOf(t *testing.T, name string) agentPod {
	t.Helper()
	out := kubectlOut(t, "get", "pod", name, "-o", "jsonpath={.status.podIP} {.spec.nodeName}")
	f := strings.Fields(out)
	if len(f) != 2 {
		t.Fatalf("pod %s: unexpected jsonpath output %q", name, out)
	}
	return agentPod{name: name, ip: f[0], node: f[1]}
}

// agentPeers returns the agent whose metrics KCONMON_AGENT_URL forwards, and one peer on another node.
func agentPeers(t *testing.T) (self, peer agentPod) {
	t.Helper()
	name := os.Getenv("KCONMON_AGENT_POD")
	if name == "" {
		t.Skip("KCONMON_AGENT_POD not set")
	}
	self = podOf(t, name)
	names := strings.Fields(kubectlOut(t, "get", "pods", "-l", "app.kubernetes.io/component=agent",
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}"))
	for _, n := range names {
		if p := podOf(t, n); p.node != self.node {
			return self, p
		}
	}
	t.Fatalf("no agent on a node other than %s; the kind config must have at least two nodes running agents", self.node)
	return agentPod{}, agentPod{}
}

func nodeExec(t *testing.T, node string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", node}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %s: %v\n%s", node, strings.Join(args, " "), err, out)
	}
}

// dropRule inserts an iptables FORWARD rule on the source's node and removes it on cleanup, whatever
// the test's outcome: a leaked DROP would break every later e2e test in the job.
func dropRule(t *testing.T, node string, match ...string) {
	t.Helper()
	rule := append([]string{"FORWARD"}, append(match, "-j", "DROP")...)
	nodeExec(t, node, append([]string{"iptables", "-I"}, rule...)...)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", append([]string{"exec", node, "iptables", "-D"}, rule...)...).Run()
	})
}

func scrapeAgent(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, agentBaseURL(t)+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape agent: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// metricValue finds the one sample of name whose label set contains every pair in want; it is a
// line scan of the text exposition format, enough for the counters and gauges asserted here.
func metricValue(exposition, name string, want map[string]string) (float64, bool) {
	for line := range strings.SplitSeq(exposition, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		end := strings.LastIndex(line, "}")
		if end < 0 {
			continue
		}
		labels := line[len(name)+1 : end]
		ok := true
		for k, v := range want {
			if !strings.Contains(labels, k+`="`+v+`"`) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(line[end+1:]), 64)
		if err != nil {
			continue
		}
		return v, true
	}
	return 0, false
}

// waitMetric polls the agent until cond holds for the sample, or fails with the last value seen.
func waitMetric(t *testing.T, budget time.Duration, name string, labels map[string]string, what string, cond func(float64) bool) float64 {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last float64
	var seen bool
	for time.Now().Before(deadline) {
		if v, ok := metricValue(scrapeAgent(t), name, labels); ok {
			last, seen = v, true
			if cond(v) {
				return v
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s: %s%v never satisfied the condition within %v (last %v, seen %v)", what, name, labels, budget, last, seen)
	return 0
}

// TestFaultPairFailureTriggersMTRAndRecovers is the headline feature end to end: a pair loses its
// path, the probes fail, a reactive MTR runs for that pair, the path comes back, the probes recover.
func TestFaultPairFailureTriggersMTRAndRecovers(t *testing.T) {
	self, peer := agentPeers(t)
	pair := map[string]string{"source_node": self.node, "destination_node": peer.node}
	fail := map[string]string{"source_node": self.node, "destination_node": peer.node, "result": "fail"}
	success := map[string]string{"source_node": self.node, "destination_node": peer.node, "result": "success"}

	failsBefore, _ := metricValue(scrapeAgent(t), "kconmon_ng_tcp_results_total", fail)
	tracesBefore, _ := metricValue(scrapeAgent(t), "kconmon_ng_mtr_triggered_total", pair)

	func() {
		dropRule(t, self.node, "-s", self.ip, "-d", peer.ip)
		waitMetric(t, 60*time.Second, "kconmon_ng_tcp_results_total", fail, "tcp failures during the cut",
			func(v float64) bool { return v > failsBefore })
		waitMetric(t, 120*time.Second, "kconmon_ng_mtr_triggered_total", pair, "reactive MTR for the cut pair",
			func(v float64) bool { return v > tracesBefore })
	}()
	// dropRule's cleanup runs at the end of the test; remove the rule now to see the recovery.
	nodeExec(t, self.node, "iptables", "-D", "FORWARD", "-s", self.ip, "-d", peer.ip, "-j", "DROP")

	okBefore, _ := metricValue(scrapeAgent(t), "kconmon_ng_tcp_results_total", success)
	waitMetric(t, 60*time.Second, "kconmon_ng_tcp_results_total", success, "tcp success after the cut is lifted",
		func(v float64) bool { return v > okBefore })
}

// TestFaultMTUBlackHole drops only the datagrams longer than 1400 bytes on one pair: TCP and small
// packets keep working, and the pmtu probe must name the black hole and the size that still crosses.
func TestFaultMTUBlackHole(t *testing.T) {
	self, peer := agentPeers(t)
	pmtuFail := map[string]string{"source_node": self.node, "destination_node": peer.node, "result": "fail"}
	pair := map[string]string{"source_node": self.node, "destination_node": peer.node}
	tcpFail := map[string]string{"source_node": self.node, "destination_node": peer.node, "result": "fail"}

	before, _ := metricValue(scrapeAgent(t), "kconmon_ng_pmtu_results_total", pmtuFail)
	tcpFailsBefore, _ := metricValue(scrapeAgent(t), "kconmon_ng_tcp_results_total", tcpFail)

	dropRule(t, self.node, "-s", self.ip, "-d", peer.ip, "-p", "udp", "-m", "length", "--length", "1401:65535")

	waitMetric(t, 90*time.Second, "kconmon_ng_pmtu_results_total", pmtuFail, "pmtu black-hole verdicts",
		func(v float64) bool { return v > before })
	mtu := waitMetric(t, 30*time.Second, "kconmon_ng_pmtu_bytes", pair, "path MTU under the length rule",
		func(v float64) bool { return v >= 1380 && v <= 1400 })
	t.Logf("path MTU %s -> %s under the rule: %v bytes", self.node, peer.node, mtu)

	// The other planes must not be blamed: TCP keeps succeeding on the same pair.
	if v, _ := metricValue(scrapeAgent(t), "kconmon_ng_tcp_results_total", tcpFail); v > tcpFailsBefore {
		t.Errorf("tcp failures rose from %v to %v under a rule that only drops large UDP datagrams", tcpFailsBefore, v)
	}

	// The operator's on-demand view: the CLI reports the verdict and exits 2 on a failed check.
	bin := kubectlKconmonBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "check", self.node, peer.node, "--type", "pmtu")
	out, err := cmd.CombinedOutput()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); err == nil || !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("kubectl kconmon check --type pmtu: err=%v, want exit 2\n%s", err, out)
	}
	if !strings.Contains(string(out), "verdict=blackhole") {
		t.Errorf("CLI output lacks verdict=blackhole:\n%s", out)
	}
}
