//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

/*
The gateway / external-agent / HTTP SD leg of .github/workflows/e2e.yaml.

The workflow reinstalls the chart with e2e/testdata/gateway-values.yaml, runs a simulated external
agent (e2e/testdata/external-agent-pod.yaml: hostNetwork, no pod identity, dialing the gateway
NodePort with the CA and token) and a fixture Prometheus (e2e/testdata/prometheus.yaml) that reads
the controller's SD endpoint, then runs `go test -run TestExternal`. Every test here is keyed on an
env var that step sets; unset means skip, the consoleBaseURL convention, so the file is inert in the
main run, where none of that exists.
*/

// Label keys as the agent writes them (internal/model/agent.go); string literals rather than an
// import so the e2e stays a black-box client of the HTTP API.
const (
	labelExternal    = "kconmon-ng.io/external"
	labelHostNetwork = "kconmon-ng.io/host-network"
)

// externalNodeIP is the InternalIP of the node the external-agent Pod is pinned to; the workflow
// resolves it with kubectl. It is the ONE address every assertion below expects to see.
func externalNodeIP(t *testing.T) string {
	t.Helper()
	ip := os.Getenv("KCONMON_EXTERNAL_NODE_IP")
	if ip == "" {
		t.Skip("KCONMON_EXTERNAL_NODE_IP not set")
	}
	if net.ParseIP(ip) == nil {
		t.Fatalf("KCONMON_EXTERNAL_NODE_IP %q is not an IP literal", ip)
	}
	return ip
}

// externalNodeName is agent.nodeName from the external agent's config; the default matches
// e2e/testdata/external-agent-pod.yaml.
func externalNodeName() string {
	if name := os.Getenv("KCONMON_EXTERNAL_NODE_NAME"); name != "" {
		return name
	}
	return "edge-host-01"
}

// externalMetricsPort is the metricsPort the external agent's config sets (9091 in the fixture),
// i.e. the port the SD endpoint must publish for it.
func externalMetricsPort(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("KCONMON_EXTERNAL_METRICS_PORT")
	if raw == "" {
		return 9091
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("KCONMON_EXTERNAL_METRICS_PORT %q: %v", raw, err)
	}
	return port
}

// sdBaseURL is a port-forward to the controller's METRICS listener (config.metricsPort), the
// listener the chart's ScrapeConfig and the scrape NetworkPolicy point Prometheus at. The API
// listener (KCONMON_CONTROLLER_URL) serves the same route and is compared against it.
func sdBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KCONMON_SD_URL")
	if url == "" {
		t.Skip("KCONMON_SD_URL not set")
	}
	return strings.TrimSuffix(url, "/")
}

// prometheusBaseURL is a port-forward to the fixture Prometheus.
func prometheusBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KCONMON_PROMETHEUS_URL")
	if url == "" {
		t.Skip("KCONMON_PROMETHEUS_URL not set")
	}
	return strings.TrimSuffix(url, "/")
}

// externalJob is the fixture Prometheus' job for the SD-discovered targets.
func externalJob() string {
	if job := os.Getenv("KCONMON_EXTERNAL_JOB"); job != "" {
		return job
	}
	return "kconmon-ng-agent-external"
}

// kubectlKconmonBinary is the plugin the workflow builds from ./cmd/kubectl-kconmon. It opens its
// own port-forward through the kubeconfig, so the test process needs a kube context and nothing
// else.
func kubectlKconmonBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("KCONMON_KUBECTL_KCONMON")
	if bin == "" {
		t.Skip("KCONMON_KUBECTL_KCONMON not set")
	}
	return bin
}

// releaseNamespace is where the chart is installed; the workflow installs into default.
func releaseNamespace() string {
	if ns := os.Getenv("KCONMON_NAMESPACE"); ns != "" {
		return ns
	}
	return "default"
}

// topologyAgent is one entry of GET /api/v1/topology's agents array, the fields this file reads.
// The three ports are what 2.4.0 added to the registration.
type topologyAgent struct {
	ID          string            `json:"id"`
	NodeName    string            `json:"nodeName"`
	PodName     string            `json:"podName"`
	PodIP       string            `json:"podIP"`
	Zone        string            `json:"zone"`
	Labels      map[string]string `json:"labels"`
	HTTPPort    int               `json:"httpPort"`
	UDPPort     int               `json:"udpPort"`
	MetricsPort int               `json:"metricsPort"`
}

func (a *topologyAgent) external() bool {
	return a.Labels[labelExternal] == "true"
}

// listTopologyAgents reads the controller's agents; it reports rather than fails so poll bodies can
// retry through a controller rollout.
func listTopologyAgents(t *testing.T, base string) ([]topologyAgent, bool) {
	t.Helper()
	status, _, data, err := request(t, http.MethodGet, base+"/api/v1/topology", nil)
	if err != nil || status != http.StatusOK {
		return nil, false
	}
	var topo struct {
		Agents []topologyAgent `json:"agents"`
	}
	if err := json.Unmarshal(data, &topo); err != nil {
		return nil, false
	}
	return topo.Agents, true
}

// awaitExternalAgent polls the topology until the external agent named by the fixture is
// registered. The budget covers the agent's reconnect backoff after the gateway upgrade rolled the
// controller, plus the controller's own lease acquisition.
func awaitExternalAgent(t *testing.T, base string) (topologyAgent, []topologyAgent) {
	t.Helper()
	name := externalNodeName()
	var found topologyAgent
	var all []topologyAgent
	pollUntil(t, 120*time.Second, 2*time.Second,
		"external agent "+name+" to appear in GET /api/v1/topology with "+labelExternal+"=true",
		func() bool {
			agents, ok := listTopologyAgents(t, base)
			if !ok {
				return false
			}
			for i := range agents {
				if agents[i].NodeName == name && agents[i].external() {
					found = agents[i]
					all = agents
					return true
				}
			}
			return false
		})
	return found, all
}

// TestExternalAgentRegistered pins how a bare-host agent shows up in the topology: marked external,
// advertising the node's own address (there is no pod IP to fall back to), in the zone its config
// asserted, and reporting the three ports peers and Prometheus will dial.
func TestExternalAgentRegistered(t *testing.T) {
	nodeIP := externalNodeIP(t)
	base := getBaseURL()
	agent, all := awaitExternalAgent(t, base)

	if agent.PodIP != nodeIP {
		t.Errorf("external agent %s advertises %q, want the node InternalIP %q: with no KCONMON_NG_POD_IP "+
			"the address must come from the outbound interface towards the gateway", agent.ID, agent.PodIP, nodeIP)
	}
	if agent.Zone != "external" {
		t.Errorf("external agent zone = %q, want %q (agent.zone in the fixture config)", agent.Zone, "external")
	}
	// The ID is <nodeName>-<podName>, and the pod name of an agent without KCONMON_NG_POD_NAME is
	// its hostname; under hostNetwork that is the kind node's name, never "edge-host-01" twice.
	if !strings.HasPrefix(agent.ID, externalNodeName()+"-") {
		t.Errorf("external agent id = %q, want the <nodeName>-<hostname> shape", agent.ID)
	}
	if agent.HTTPPort <= 0 || agent.UDPPort <= 0 || agent.MetricsPort <= 0 {
		t.Errorf("external agent must report all three ports, got http=%d udp=%d metrics=%d",
			agent.HTTPPort, agent.UDPPort, agent.MetricsPort)
	}
	if want := externalMetricsPort(t); agent.MetricsPort != want {
		t.Errorf("external agent metricsPort = %d, want %d (the fixture config)", agent.MetricsPort, want)
	}
	// hostNetwork is this Pod's plumbing, not the agent's identity: only the chart's
	// agent.hostNetwork flag sets that label, and a bare host has no such flag.
	if v, ok := agent.Labels[labelHostNetwork]; ok {
		t.Errorf("external agent carries %s=%q; a bare-host agent must not be labelled host-network", labelHostNetwork, v)
	}

	// The in-cluster fleet is untouched by the gateway: no other agent is external, and none of
	// them advertises the external host's address.
	for i := range all {
		a := &all[i]
		if a.ID == agent.ID {
			continue
		}
		if a.external() {
			t.Errorf("in-cluster agent %s is labelled external", a.ID)
		}
		if a.PodIP == nodeIP {
			t.Errorf("in-cluster agent %s advertises the external host's address %s", a.ID, nodeIP)
		}
	}
	t.Logf("external agent %s: address %s zone %s ports http=%d udp=%d metrics=%d, %d agents total",
		agent.ID, agent.PodIP, agent.Zone, agent.HTTPPort, agent.UDPPort, agent.MetricsPort, len(all))
}

// sdTargetGroup is one element of the Prometheus http_sd body.
type sdTargetGroup struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// getSD reads the SD body from one listener and decodes it.
func getSD(t *testing.T, base string) (http.Header, []sdTargetGroup, []byte) {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodGet, base+"/api/v1/prometheus/sd", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET %s/api/v1/prometheus/sd 200, got %d: %s", base, status, data)
	}
	var groups []sdTargetGroup
	decodeJSON(t, "prometheus sd body", data, &groups)
	if groups == nil {
		t.Fatalf("expected a JSON array (the literal [] when empty), got null: %s", data)
	}
	return header, groups, data
}

// TestExternalPrometheusSD pins the http_sd contract: the metrics listener answers one target
// group per external agent, <nodeIP>:<metricsPort>, under exactly the four fixed labels, with the
// headers Prometheus relies on; and the API listener serves the identical body.
func TestExternalPrometheusSD(t *testing.T) {
	nodeIP := externalNodeIP(t)
	sdBase := sdBaseURL(t)
	apiBase := getBaseURL()
	agent, _ := awaitExternalAgent(t, apiBase)

	header, groups, data := getSD(t, sdBase)
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	// no-store, because a cached body would outlive an eviction and keep a gone host as a target.
	if cc := header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if len(groups) != 1 {
		t.Fatalf("expected exactly one target group (one external agent), got %d: %s", len(groups), data)
	}
	group := groups[0]
	wantTarget := net.JoinHostPort(nodeIP, strconv.Itoa(externalMetricsPort(t)))
	if !reflect.DeepEqual(group.Targets, []string{wantTarget}) {
		t.Errorf("targets = %v, want [%s]", group.Targets, wantTarget)
	}
	// The label set is FIXED (the agent's own labels map is never copied through: a host could
	// otherwise inject arbitrary target labels into Prometheus), so equality, not containment.
	wantLabels := map[string]string{
		"node":     externalNodeName(),
		"zone":     "external",
		"external": "true",
		"agent_id": agent.ID,
	}
	if !reflect.DeepEqual(group.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", group.Labels, wantLabels)
	}

	// The same route on the API port (httpPort), mounted for curl and uniformity; same body.
	_, apiGroups, apiData := getSD(t, apiBase)
	if !reflect.DeepEqual(apiGroups, groups) {
		t.Errorf("SD body differs between the metrics listener and the API listener:\n%s\n%s", data, apiData)
	}
	t.Logf("SD body: %s", strings.TrimSpace(string(data)))
}

// promVector is the instant-query envelope, the fields this file reads.
type promVector struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// promQuery runs one instant query against the fixture Prometheus; it reports rather than fails so
// a poll body can retry through Prometheus' own start-up.
func promQuery(t *testing.T, base, query string) (promVector, bool) {
	t.Helper()
	u := base + "/api/v1/query?" + url.Values{"query": {query}}.Encode()
	status, _, data, err := request(t, http.MethodGet, u, nil)
	if err != nil || status != http.StatusOK {
		return promVector{}, false
	}
	var vec promVector
	if err := json.Unmarshal(data, &vec); err != nil || vec.Status != "success" {
		return promVector{}, false
	}
	return vec, true
}

// TestExternalPrometheusScrape closes the loop the SD endpoint opens: the fixture Prometheus reads
// the target list on its own refresh, scrapes <nodeIP>:<metricsPort> through the host network, and
// reports up == 1 under the SD labels. Budget: an SD refresh (10s) plus a scrape (5s) plus slack
// for a controller rollout that emptied the registry mid-way.
func TestExternalPrometheusScrape(t *testing.T) {
	nodeIP := externalNodeIP(t)
	promBase := prometheusBaseURL(t)
	job := externalJob()
	target := net.JoinHostPort(nodeIP, strconv.Itoa(externalMetricsPort(t)))

	query := fmt.Sprintf(`up{job=%q}`, job)
	var last promVector
	pollUntil(t, 150*time.Second, 3*time.Second,
		fmt.Sprintf("Prometheus %s to report instance %s up", query, target),
		func() bool {
			vec, ok := promQuery(t, promBase, query)
			if !ok {
				return false
			}
			last = vec
			for _, r := range vec.Data.Result {
				if r.Metric["instance"] != target {
					continue
				}
				v, isString := r.Value[1].(string)
				return isString && v == "1"
			}
			return false
		})

	// Exactly the SD-discovered host: the job has one target, and it carries the SD labels.
	if len(last.Data.Result) != 1 {
		t.Errorf("expected exactly one %s series, got %d: %+v", query, len(last.Data.Result), last.Data.Result)
	}
	for _, r := range last.Data.Result {
		if r.Metric["instance"] != target {
			continue
		}
		for k, want := range map[string]string{"node": externalNodeName(), "zone": "external", "external": "true"} {
			if got := r.Metric[k]; got != want {
				t.Errorf("up{instance=%q} label %s = %q, want %q", target, k, got, want)
			}
		}
		if !strings.HasPrefix(r.Metric["agent_id"], externalNodeName()+"-") {
			t.Errorf("up{instance=%q} agent_id = %q, want the <nodeName>-<hostname> shape", target, r.Metric["agent_id"])
		}
		t.Logf("scraped: %v", r.Metric)
	}
}

// matrixCell is one source→destination pair of GET /api/v1/matrix.
type matrixCell struct {
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	FailRatio   *float64 `json:"failRatio"`
}

// TestExternalConsoleMatrix asserts the external host is a full participant of the mesh as the
// console draws it: a row (its own probes towards the cluster nodes, from its own scraped series)
// and a column (the cluster nodes' probes towards it, from theirs), and both directions reach.
// "Reach" is failRatio below 1 rather than exactly 0: the ratio is a rate over a five-minute window
// that starts before the agent was up, so the first samples can carry failed probes. Both need the
// fixture Prometheus to have at least two samples of each rate()'d counter, hence the budget.
func TestExternalConsoleMatrix(t *testing.T) {
	externalNodeIP(t) // the leg's gate; the matrix is keyed on names, not addresses
	base := consoleBaseURL(t)
	name := externalNodeName()

	// reaches is a cell whose pair has at least one successful probe in the window.
	reaches := func(c matrixCell) bool { return c.FailRatio != nil && *c.FailRatio < 1 }
	allReach := func(cells []matrixCell) bool {
		for _, c := range cells {
			if !reaches(c) {
				return false
			}
		}
		return len(cells) > 0
	}

	var rows, cols []matrixCell
	var nodes []string
	pollUntil(t, 240*time.Second, 5*time.Second,
		"GET /api/v1/matrix?protocol=tcp to hold a reaching row and column for "+name,
		func() bool {
			status, _, data, err := request(t, http.MethodGet, base+"/api/v1/matrix?protocol=tcp", nil)
			if err != nil {
				return false
			}
			if status == http.StatusServiceUnavailable {
				t.Fatalf("GET /api/v1/matrix answered 503: this console has no Prometheus; "+
					"e2e/testdata/gateway-values.yaml must set console.prometheus.url: %s", data)
			}
			if status != http.StatusOK {
				return false
			}
			var m struct {
				Nodes []string     `json:"nodes"`
				Cells []matrixCell `json:"cells"`
			}
			if err := json.Unmarshal(data, &m); err != nil {
				return false
			}
			rows, cols = rows[:0], cols[:0]
			for _, c := range m.Cells {
				switch {
				case c.Source == name && c.Destination != name:
					rows = append(rows, c)
				case c.Destination == name && c.Source != name:
					cols = append(cols, c)
				}
			}
			nodes = m.Nodes
			return allReach(rows) && allReach(cols)
		})

	found := false
	for _, n := range nodes {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("matrix nodes %v do not list %s", nodes, name)
	}
	for _, c := range append(rows, cols...) {
		t.Logf("cell %s -> %s failRatio %v", c.Source, c.Destination, *c.FailRatio)
	}
	t.Logf("matrix: %d row cells from %s, %d column cells to it, nodes %v", len(rows), name, len(cols), nodes)
}

// runKubectlKconmon runs the plugin with the release namespace and returns its stdout; the plugin
// opens and tears down its own port-forward per invocation.
func runKubectlKconmon(t *testing.T, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	full := append([]string{"-n", releaseNamespace()}, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", bin, strings.Join(full, " "), err, out)
	}
	return string(out)
}

// tableRow finds the row of a tabwriter table whose column `col` (whitespace-split) equals want.
func tableRow(table string, col int, want string) ([]string, bool) {
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) > col && fields[col] == want {
			return fields, true
		}
	}
	return nil, false
}

// TestExternalKubectlKconmon pins the CLI's view: `agents` marks the host EXTERNAL yes (and the
// in-cluster ones "-"), `topology` lists it as a row of its own below the nodes.
func TestExternalKubectlKconmon(t *testing.T) {
	nodeIP := externalNodeIP(t)
	bin := kubectlKconmonBinary(t)
	name := externalNodeName()

	// ID  NODE  POD IP  ZONE  EXTERNAL  LAST SEEN -- fields: id, node, ip, zone, external, ...
	agents := runKubectlKconmon(t, bin, "agents")
	header := strings.Fields(strings.SplitN(agents, "\n", 2)[0])
	if !strings.Contains(strings.Join(header, " "), "EXTERNAL") {
		t.Fatalf("`agents` header has no EXTERNAL column:\n%s", agents)
	}
	row, ok := tableRow(agents, 1, name)
	if !ok {
		t.Fatalf("`agents` prints no row for node %s:\n%s", name, agents)
	}
	if len(row) < 5 {
		t.Fatalf("`agents` row for %s is too short: %v", name, row)
	}
	if row[2] != nodeIP || row[3] != "external" || row[4] != "yes" {
		t.Errorf("`agents` row for %s = %v, want POD IP %s, ZONE external, EXTERNAL yes", name, row, nodeIP)
	}
	if !strings.HasPrefix(row[0], name+"-") {
		t.Errorf("`agents` row ID = %q, want the <nodeName>-<hostname> shape", row[0])
	}
	// "-" rather than "no" for the fleet, so the eye lands on the exception.
	dashes := 0
	for _, line := range strings.Split(agents, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 5 && f[1] != name && f[4] == "-" {
			dashes++
		}
	}
	if dashes == 0 {
		t.Errorf("`agents` shows no in-cluster row with EXTERNAL \"-\":\n%s", agents)
	}
	t.Logf("agents row: %v", row)

	// NODE  ZONE  READY  AGENT  AGENT IP -- an external host is appended after the Node objects with
	// READY "-", since nothing reports readiness for a host outside the cluster.
	topology := runKubectlKconmon(t, bin, "topology")
	row, ok = tableRow(topology, 0, name)
	if !ok {
		t.Fatalf("`topology` prints no row for %s:\n%s", name, topology)
	}
	if len(row) < 5 {
		t.Fatalf("`topology` row for %s is too short: %v", name, row)
	}
	if row[1] != "external" || row[2] != "-" || !strings.HasPrefix(row[3], name+"-") || row[4] != nodeIP {
		t.Errorf("`topology` row for %s = %v, want ZONE external, READY -, AGENT %s-<hostname>, AGENT IP %s",
			name, row, name, nodeIP)
	}
	t.Logf("topology row: %v", row)
}
