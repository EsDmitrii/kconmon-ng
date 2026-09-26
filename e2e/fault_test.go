//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// blackHoleAbove is the largest IP datagram TestFaultMTUBlackHole's length rule lets through. The
// bisection closes to one byte, so mtuSlack only absorbs a probe that fits but is lost anyway.
const (
	blackHoleAbove = 1400
	mtuSlack       = 20
)

// underBlackHole reports whether a path MTU is what the probe finds under the length rule.
func underBlackHole(mtu float64) bool {
	return mtu >= blackHoleAbove-mtuSlack && mtu <= blackHoleAbove
}

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
	names := strings.FieldsSeq(kubectlOut(t, "get", "pods", "-l", "app.kubernetes.io/component=agent",
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}"))
	for n := range names {
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

// dropRule inserts an iptables FORWARD rule on the source's node and returns lift, which deletes it.
// A cleanup deletes it too when the test ends first, and a failed delete there is an error: a leaked
// DROP would break every later e2e leg in the job.
func dropRule(t *testing.T, node string, match ...string) (lift func()) {
	t.Helper()
	rule := append([]string{"FORWARD"}, append(match, "-j", "DROP")...)
	nodeExec(t, node, append([]string{"iptables", "-I"}, rule...)...)
	lifted := false
	t.Cleanup(func() {
		if lifted {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", append([]string{"exec", node, "iptables", "-D"}, rule...)...).CombinedOutput()
		if err != nil {
			t.Errorf("remove %v on %s: %v\n%s", rule, node, err, out)
		}
	})
	return func() {
		t.Helper()
		nodeExec(t, node, append([]string{"iptables", "-D"}, rule...)...)
		lifted = true
	}
}

func scrapeAgent(t *testing.T) string {
	t.Helper()
	text := scrapeAgentMetrics(t, agentBaseURL(t))
	if text == "" {
		t.Fatal("scrape agent failed; see the log line above")
	}
	return text
}

// metricValue finds the one sample of name whose label set contains every pair in want; it is a
// line scan of the text exposition format, enough for the counters and gauges asserted here.
func metricValue(exposition, name string, want map[string]string) (float64, bool) {
	for _, line := range metricSamples(exposition, name, want) {
		end := strings.LastIndex(line, "}")
		if v, err := strconv.ParseFloat(strings.TrimSpace(line[end+1:]), 64); err == nil {
			return v, true
		}
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
		if v, ok := metricValue(scrapeAgentMetrics(t, agentBaseURL(t)), name, labels); ok {
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

// cellString renders a matrix cell for a log line or a failure message.
func cellString(c matrixCell) string {
	if c.Source == "" {
		return "no cell"
	}
	f := func(p *float64) string {
		if p == nil {
			return "null"
		}
		return strconv.FormatFloat(*p, 'g', 4, 64)
	}
	i := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return strconv.FormatInt(*p, 10)
	}
	return fmt.Sprintf("%s -> %s failRatio=%s mtuBytes=%s probeMtuBytes=%s",
		c.Source, c.Destination, f(c.FailRatio), i(c.MTUBytes), i(c.ProbeMTUBytes))
}

// awaitPMTUMatrix polls the console's pmtu matrix until self -> peer is a black hole the way the
// console paints one (failures next to a path MTU below the probe size) and peer -> self is healthy
// (no failures, full size), or fails with both cells as last seen.
func awaitPMTUMatrix(t *testing.T, console string, self, peer agentPod, budget time.Duration) (fwd, rev matrixCell) {
	t.Helper()
	blackholed := func(c matrixCell) bool {
		return c.FailRatio != nil && *c.FailRatio > 0 && c.MTUBytes != nil && c.ProbeMTUBytes != nil &&
			underBlackHole(float64(*c.MTUBytes)) && *c.MTUBytes < *c.ProbeMTUBytes
	}
	healthy := func(c matrixCell) bool {
		return c.FailRatio != nil && *c.FailRatio == 0 && c.MTUBytes != nil && c.ProbeMTUBytes != nil &&
			*c.MTUBytes == *c.ProbeMTUBytes
	}
	deadline := time.Now().Add(budget)
	for {
		status, _, data, err := request(t, http.MethodGet, console+"/api/v1/matrix?protocol=pmtu", nil)
		switch {
		case err != nil:
			t.Logf("pmtu matrix: %v (will retry)", err)
		case status == http.StatusServiceUnavailable:
			t.Fatalf("GET /api/v1/matrix answered 503: this console has no Prometheus; the workflow must "+
				"install e2e/testdata/prometheus-values.yaml before the fault leg: %s", data)
		case status != http.StatusOK:
			t.Logf("pmtu matrix: status %d (will retry): %s", status, data)
		default:
			var m struct {
				Cells []matrixCell `json:"cells"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("decode the pmtu matrix: %v\n%s", err, data)
			}
			fwd, rev = matrixCell{}, matrixCell{}
			for _, c := range m.Cells {
				switch {
				case c.Source == self.node && c.Destination == peer.node:
					fwd = c
				case c.Source == peer.node && c.Destination == self.node:
					rev = c
				}
			}
			if blackholed(fwd) && healthy(rev) {
				return fwd, rev
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("within %v the console's pmtu matrix never showed %s -> %s black-holed (failRatio > 0, "+
				"mtuBytes %d-%d below probeMtuBytes) and %s -> %s healthy (failRatio 0, mtuBytes = "+
				"probeMtuBytes); last seen: %s; %s",
				budget, self.node, peer.node, blackHoleAbove-mtuSlack, blackHoleAbove, peer.node, self.node,
				cellString(fwd), cellString(rev))
		}
		time.Sleep(5 * time.Second)
	}
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

	lift := dropRule(t, self.node, "-s", self.ip, "-d", peer.ip)
	liftReverse := dropRule(t, peer.node, "-s", peer.ip, "-d", self.ip)
	waitMetric(t, 60*time.Second, "kconmon_ng_tcp_results_total", fail, "tcp failures during the cut",
		func(v float64) bool { return v > failsBefore })
	waitMetric(t, 120*time.Second, "kconmon_ng_mtr_triggered_total", pair, "reactive MTR for the cut pair",
		func(v float64) bool { return v > tracesBefore })
	lift()
	liftReverse()

	okBefore, _ := metricValue(scrapeAgent(t), "kconmon_ng_tcp_results_total", success)
	waitMetric(t, 60*time.Second, "kconmon_ng_tcp_results_total", success, "tcp success after the cut is lifted",
		func(v float64) bool { return v > okBefore })
}

// TestFaultMTUBlackHole drops every packet longer than blackHoleAbove bytes on one pair: the small TCP, UDP and
// ICMP probes keep working, the pmtu probe must name the black hole and the size that still crosses,
// and once the rule is gone the probe must cross at full size again. The console matrix must show the
// same pair failing on pmtu while the reverse direction stays healthy.
func TestFaultMTUBlackHole(t *testing.T) {
	console := consoleBaseURL(t)
	self, peer := agentPeers(t)
	pair := map[string]string{"source_node": self.node, "destination_node": peer.node}
	result := func(r string) map[string]string {
		return map[string]string{"source_node": self.node, "destination_node": peer.node, "result": r}
	}
	// e2e/testdata/values.yaml enables all three for this control.
	planes := []string{"tcp", "udp", "icmp"}

	exposition := scrapeAgent(t)
	before, _ := metricValue(exposition, "kconmon_ng_pmtu_results_total", result("fail"))
	failsBefore := make(map[string]float64, len(planes))
	for _, p := range planes {
		failsBefore[p], _ = metricValue(exposition, "kconmon_ng_"+p+"_results_total", result("fail"))
	}

	lift := dropRule(t, self.node, "-s", self.ip, "-d", peer.ip,
		"-m", "length", "--length", fmt.Sprintf("%d:65535", blackHoleAbove+1))

	waitMetric(t, 90*time.Second, "kconmon_ng_pmtu_results_total", result("fail"), "pmtu black-hole verdicts",
		func(v float64) bool { return v > before })
	mtu := waitMetric(t, 30*time.Second, "kconmon_ng_pmtu_bytes", pair, "path MTU under the length rule", underBlackHole)
	t.Logf("path MTU %s -> %s under the rule: %v bytes", self.node, peer.node, mtu)

	// The other planes must not be blamed: each keeps succeeding on the same pair under the rule, and
	// none of them records a failure.
	exposition = scrapeAgent(t)
	for _, p := range planes {
		okUnderRule, _ := metricValue(exposition, "kconmon_ng_"+p+"_results_total", result("success"))
		waitMetric(t, 30*time.Second, "kconmon_ng_"+p+"_results_total", result("success"), p+" success under the length rule",
			func(v float64) bool { return v > okUnderRule })
	}
	exposition = scrapeAgent(t)
	for _, p := range planes {
		if v, _ := metricValue(exposition, "kconmon_ng_"+p+"_results_total", result("fail")); v > failsBefore[p] {
			t.Errorf("%s failures rose from %v to %v under a rule that only drops packets longer than %d bytes",
				p, failsBefore[p], v, blackHoleAbove)
		}
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

	// The console's view through the fixture Prometheus, still under the rule. The reverse direction
	// stays healthy because the echo reply is four bytes and the rule drops only longer packets.
	fwd, rev := awaitPMTUMatrix(t, console, self, peer, 120*time.Second)
	t.Logf("console pmtu matrix under the rule: %s; %s", cellString(fwd), cellString(rev))

	// Recovery: with the rule gone the probe crosses at the size it probes at, not the one it found.
	lift()
	exposition = scrapeAgent(t)
	probe, ok := metricValue(exposition, "kconmon_ng_pmtu_probe_bytes", pair)
	if !ok {
		t.Fatalf("kconmon_ng_pmtu_probe_bytes{source_node=%q,destination_node=%q} missing", self.node, peer.node)
	}
	okBefore, _ := metricValue(exposition, "kconmon_ng_pmtu_results_total", result("success"))
	waitMetric(t, 60*time.Second, "kconmon_ng_pmtu_results_total", result("success"), "pmtu success after the rule is lifted",
		func(v float64) bool { return v > okBefore })
	waitMetric(t, 30*time.Second, "kconmon_ng_pmtu_bytes", pair, "path MTU back at the probe size",
		func(v float64) bool { return v == probe })
}
