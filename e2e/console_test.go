//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// consoleBaseURL returns KCONMON_CONSOLE_URL, or skips the test when unset.
// Unlike getBaseURL (the controller helper above), there is deliberately no
// localhost default: the console and controller are separate Services on
// separate ports, and guessing a port here would silently point console
// assertions at whatever happens to be listening instead of skipping
// cleanly on a workflow run that never wired the console up.
func consoleBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KCONMON_CONSOLE_URL")
	if url == "" {
		t.Skip("KCONMON_CONSOLE_URL not set")
	}
	return strings.TrimSuffix(url, "/")
}

// agentBaseURL returns KCONMON_AGENT_URL -- ONE agent Pod's own HTTP port,
// port-forwarded by the workflow -- or skips. Same no-default rule as
// consoleBaseURL, and for a sharper reason: the agent's /metrics is the only
// place a continuous external check can be observed at all (the console never
// sees external results, they are Prometheus series on the agent that ran
// them), so a wrong guess here would turn "the feature is broken" into "the
// series is missing", which reads identically.
func agentBaseURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("KCONMON_AGENT_URL")
	if url == "" {
		t.Skip("KCONMON_AGENT_URL not set")
	}
	return strings.TrimSuffix(url, "/")
}

// externalTargetAddr is the destination the continuous-external-check test
// probes. It defaults to the controller's own ClusterIP Service in the
// release namespace the workflow installs into, and that choice is the whole
// point: the harness allowlist (e2e/testdata/values.yaml) covers the kind pod
// and Service networks ONLY, so this test proves the external-check pipeline
// end to end WITHOUT the cluster ever egressing to the internet and without a
// public-address carve-out that would weaken the very allowlist
// TestConsoleExternalCheckDenied exists to verify.
func externalTargetAddr() string {
	if addr := os.Getenv("KCONMON_EXTERNAL_TARGET_ADDR"); addr != "" {
		return addr
	}
	return "kconmon-ng-controller.default.svc.cluster.local:8080"
}

// deniedTargetAddr is the destination the security test points at: an
// RFC 5737 TEST-NET-3 literal. It is outside the harness allowedCidrs, it is
// an IP literal so no DNS lookup can make the outcome depend on a resolver,
// and it is reserved for documentation so it is not routable anywhere even if
// the allowlist were removed. A denial here is therefore a statement about
// the allowlist, never about the network.
func deniedTargetAddr() string {
	if addr := os.Getenv("KCONMON_DENIED_TARGET_ADDR"); addr != "" {
		return addr
	}
	return "203.0.113.10:80"
}

// e2eClient is the single HTTP client this file uses. A per-request timeout
// matters more here than anywhere else in the repo: every assertion below
// runs inside a poll loop with its own budget, and a request that hangs
// forever would burn the whole budget on one iteration and report a timeout
// against the wrong thing.
var e2eClient = &http.Client{Timeout: 30 * time.Second}

// requestTimeout bounds one request's context. It matches e2eClient.Timeout on
// purpose -- the context is what actually cancels an in-flight body read when
// `go test -timeout` fires, which the client timeout alone does not guarantee
// for a response already being streamed.
const requestTimeout = 30 * time.Second

// request issues one HTTP request and returns its status, headers and the
// FULL body, with the response already closed.
//
// Every HTTP call in this file goes through it, which is what keeps the file
// free of the two mistakes a hand-rolled call inside a polling loop makes
// most often: a response body that is never closed (one leaked connection per
// iteration, over a 60s budget) and a request carrying no context, which
// cannot be cancelled at all.
//
// A transport error is RETURNED, never fatal: most callers here are poll
// bodies, for which "the console has not come back yet" is an ordinary
// intermediate state, not a failure. Callers that cannot recover use
// mustRequest.
func request(t *testing.T, method, url string, body any) (int, http.Header, []byte, error) {
	t.Helper()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s body: %v", method, url, err)
		}
		payload = bytes.NewReader(encoded)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		t.Fatalf("build %s %s request: %v", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e2eClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Logf("close %s %s response body: %v", method, url, cerr)
		}
	}()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	return resp.StatusCode, resp.Header, data, nil
}

// mustRequest is request for a call whose transport failure means the test
// cannot continue at all.
func mustRequest(t *testing.T, method, url string, body any) (int, http.Header, []byte) {
	t.Helper()
	status, header, data, err := request(t, method, url, body)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, url, err)
	}
	return status, header, data
}

// decodeJSON decodes a response body, naming what failed and echoing the body
// -- a decode error on a Problem Details response is otherwise reported as
// "cannot unmarshal string into field", which says nothing about the 422 the
// server actually sent.
func decodeJSON(t *testing.T, what string, data []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", what, err, data)
	}
}

// uniqueName mints a name unique per test run, so every test below is
// re-runnable against a cluster that still holds rows from an earlier run and
// order-independent with respect to the others. The charset is the one
// store.validateName enforces (alphanumerics, '-', '_', '.'), and the whole
// string stays inside its 63-byte limit for any prefix used here.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// deleteResource DELETEs one row and tolerates a 404: a test that already
// deleted the row explicitly (TestConsoleTargetsCRUD does, that IS its
// assertion) must not then fail in cleanup.
func deleteResource(t *testing.T, url string) {
	t.Helper()
	status, _, data, err := request(t, http.MethodDelete, url, nil)
	switch {
	case err != nil:
		t.Logf("cleanup DELETE %s failed: %v", url, err)
	case status != http.StatusNoContent && status != http.StatusNotFound:
		t.Errorf("cleanup DELETE %s: expected 204 or 404, got %d: %s", url, status, data)
	}
}

// createTarget POSTs one target, asserts the created-and-point-at-it contract
// (201 + Location) and registers its deletion.
func createTarget(t *testing.T, base string, body map[string]any) string {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/targets", body)
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/targets 201, got %d: %s", status, data)
	}

	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, "create-target response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty target id in the create response")
	}
	if want := "/api/v1/targets/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}

	t.Cleanup(func() { deleteResource(t, base+"/api/v1/targets/"+created.ID) })
	return created.ID
}

// createDefinition POSTs one check definition and registers its deletion. Its
// cleanup runs BEFORE any target's, because t.Cleanup is LIFO and every
// caller here creates the target first: a target with a definition still
// pointing at it is a 409, by design.
func createDefinition(t *testing.T, base string, body map[string]any) string {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/checks", body)
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/checks 201, got %d: %s", status, data)
	}

	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, "create-definition response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty definition id in the create response")
	}
	if want := "/api/v1/checks/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}

	t.Cleanup(func() { deleteResource(t, base+"/api/v1/checks/"+created.ID) })
	return created.ID
}

// createSchedule POSTs one schedule and returns the stored row -- not an echo
// of the request: an interval below the store's 10s floor is clamped up, and
// a caller that asserts on what it sent would be asserting on a value the
// server rejected.
func createSchedule(t *testing.T, base string, body map[string]any) scheduleRow {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/schedules", body)
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/schedules 201, got %d: %s", status, data)
	}

	var created scheduleRow
	decodeJSON(t, "create-schedule response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty schedule id in the create response")
	}
	if want := "/api/v1/schedules/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}

	t.Cleanup(func() { deleteResource(t, base+"/api/v1/schedules/"+created.ID) })
	return created
}

// scheduleRow is the subset of the Schedule schema these tests assert on.
type scheduleRow struct {
	ID           string     `json:"id"`
	DefinitionID string     `json:"definitionId"`
	Kind         string     `json:"kind"`
	IntervalNs   int64      `json:"intervalNs"`
	Enabled      bool       `json:"enabled"`
	NextFireAt   *time.Time `json:"nextFireAt"`
}

// metricSamples returns every sample LINE of family `name` in a Prometheus
// text exposition body whose label set carries all of `labels`.
//
// Substring matching on `key="value"` is deliberate rather than a full
// exposition parse: the label values these tests match on are run-unique
// names (uniqueName), so a false positive would need another series to carry
// the exact same nanosecond-stamped string.
func metricSamples(text, name string, labels map[string]string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		matched := true
		for key, value := range labels {
			if !strings.Contains(line, key+`="`+value+`"`) {
				matched = false
				break
			}
		}
		if matched {
			out = append(out, line)
		}
	}
	return out
}

// sampleValue parses the numeric value off one exposition line.
func sampleValue(t *testing.T, line string) float64 {
	t.Helper()
	brace := strings.LastIndex(line, "}")
	if brace < 0 {
		t.Fatalf("malformed exposition line (no closing brace): %q", line)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(line[brace+1:]), 64)
	if err != nil {
		t.Fatalf("parse value off exposition line %q: %v", line, err)
	}
	return value
}

// scrapeAgentMetrics fetches one agent's /metrics, returning "" (and logging)
// on anything that is not a 200 -- poll bodies treat that as "not yet".
func scrapeAgentMetrics(t *testing.T, agentBase string) string {
	t.Helper()
	status, _, data, err := request(t, http.MethodGet, agentBase+"/metrics", nil)
	if err != nil {
		t.Logf("agent metrics request failed (will retry): %v", err)
		return ""
	}
	if status != http.StatusOK {
		t.Logf("agent metrics returned %d (will retry)", status)
		return ""
	}
	return string(data)
}

func TestConsoleHealthz(t *testing.T) {
	base := consoleBaseURL(t)
	status, _, _ := mustRequest(t, http.MethodGet, base+"/healthz", nil)
	if status != http.StatusOK {
		t.Errorf("expected /healthz 200, got %d", status)
	}
}

func TestConsoleReadyz(t *testing.T) {
	base := consoleBaseURL(t)
	status, _, _ := mustRequest(t, http.MethodGet, base+"/readyz", nil)
	if status != http.StatusOK {
		t.Errorf("expected /readyz 200, got %d", status)
	}
}

func TestConsoleMetrics(t *testing.T) {
	base := consoleBaseURL(t)
	status, _, data := mustRequest(t, http.MethodGet, base+"/metrics", nil)
	if status != http.StatusOK {
		t.Errorf("expected /metrics 200, got %d", status)
	}

	if !strings.Contains(string(data), "kconmon_ng_console_") {
		t.Errorf("expected /metrics to contain kconmon_ng_console_ series")
	}

	// Proof the store was actually exercised, not merely compiled in: this
	// series is only emitted once the console issues a query against
	// PostgreSQL, which console-values.yaml's database.mode=external makes
	// happen (e.g. GET /api/v1/config's own database.configured check, or
	// the events/runs polling elsewhere in this file). It rides the same
	// ingester/store pipeline as TestConsoleEvents, so it is polled rather
	// than asserted on a single GET: this is observability-of-arrival for
	// that pipeline, not a race assertion on unrelated behavior.
	pollUntil(t, 60*time.Second, 2*time.Second,
		"kconmon_ng_console_store_queries_total to appear in /metrics (store not exercised)", func() bool {
			pollStatus, _, body, err := request(t, http.MethodGet, base+"/metrics", nil)
			if err != nil {
				t.Logf("metrics request failed (will retry): %v", err)
				return false
			}
			if pollStatus != http.StatusOK {
				t.Logf("metrics request returned %d (will retry)", pollStatus)
				return false
			}
			return strings.Contains(string(body), "kconmon_ng_console_store_queries_total")
		})
}

func TestConsoleVersion(t *testing.T) {
	base := consoleBaseURL(t)
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/version", nil)
	if status != http.StatusOK {
		t.Fatalf("expected /api/v1/version 200, got %d", status)
	}

	var body struct {
		Capabilities []string `json:"capabilities"`
	}
	decodeJSON(t, "version response", data, &body)
	// capabilities is asserted to be a present array only -- NOT that it
	// contains "events". Whether the realtime pipeline is healthy at the
	// moment this request lands depends on leader/reconnect timing; asserting
	// its presence would make this test flake on exactly the kind of blip
	// that is not a real bug.
	if body.Capabilities == nil {
		t.Errorf("expected capabilities to be a JSON array (possibly empty), got null")
	}
}

func TestConsoleConfig(t *testing.T) {
	base := consoleBaseURL(t)
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/config", nil)
	if status != http.StatusOK {
		t.Fatalf("expected /api/v1/config 200, got %d", status)
	}

	var body struct {
		Database struct {
			Configured bool `json:"configured"`
		} `json:"database"`
	}
	decodeJSON(t, "config response", data, &body)
	if !body.Database.Configured {
		t.Errorf("expected database.configured=true (console-values.yaml sets console.database.mode=external)")
	}
}

// TestConsoleEvents is the single most valuable assertion in this file:
// agents registering with the controller produce topology_changed domain
// events, which -- with controller.events.enabled=true and
// database.mode=external wired -- the console's ingester consumes and
// persists. A non-empty history here is end-to-end proof that
// ingester -> store -> API works in a real cluster, not just in a unit test.
func TestConsoleEvents(t *testing.T) {
	base := consoleBaseURL(t)

	var last struct {
		Events []json.RawMessage `json:"events"`
	}

	pollUntil(t, 60*time.Second, 2*time.Second,
		"at least one persisted event (topology_changed from agent registration)", func() bool {
			status, _, data, err := request(t, http.MethodGet, base+"/api/v1/events", nil)
			if err != nil {
				t.Logf("events request failed (will retry): %v", err)
				return false
			}
			if status != http.StatusOK {
				t.Logf("events request returned %d (will retry)", status)
				return false
			}
			if err := json.Unmarshal(data, &last); err != nil {
				t.Logf("decode events response failed (will retry): %v", err)
				return false
			}
			return len(last.Events) > 0
		})
}

// TestConsoleRuns exercises POST /api/v1/runs end to end against the real
// agents in the cluster: an empty sources/destinations pair fans out to
// every node in the current topology (checks.Plan's documented fallback), so
// on a 3-node kind cluster this dispatches several TCP pairs through the
// real controller/agent path, not a stub.
func TestConsoleRuns(t *testing.T) {
	base := consoleBaseURL(t)

	reqBody := map[string]any{
		"sources":      []string{},
		"destinations": []string{},
		"type":         "tcp",
		"plane":        "pod",
	}

	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/runs", reqBody)
	if status != http.StatusAccepted {
		t.Fatalf("expected POST /api/v1/runs 202, got %d: %s", status, data)
	}
	if header.Get("Location") == "" {
		t.Errorf("expected a Location header on the 202 response")
	}

	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, "create-run response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty run id in the create-run response")
	}

	var final struct {
		Status    string `json:"status"`
		PairTotal int32  `json:"pairTotal"`
	}
	pollUntil(t, 60*time.Second, 2*time.Second,
		fmt.Sprintf("run %s to reach a terminal status", created.ID), func() bool {
			pollStatus, _, body, err := request(t, http.MethodGet, base+"/api/v1/runs/"+created.ID, nil)
			if err != nil {
				t.Logf("get run request failed (will retry): %v", err)
				return false
			}
			if pollStatus != http.StatusOK {
				t.Logf("get run returned %d (will retry)", pollStatus)
				return false
			}
			if err := json.Unmarshal(body, &final); err != nil {
				t.Logf("decode run response failed (will retry): %v", err)
				return false
			}
			// finalStatus's closed set (internal/console/checks/runner.go):
			// "succeeded", "failed", or "partial". "pending"/"running" are
			// not terminal.
			switch final.Status {
			case "succeeded", "failed", "partial":
				return true
			default:
				return false
			}
		})

	if final.PairTotal <= 0 {
		t.Errorf("expected pairTotal > 0, got %d", final.PairTotal)
	}

	listStatus, _, listData := mustRequest(t, http.MethodGet, base+"/api/v1/runs?limit=500", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/runs 200, got %d", listStatus)
	}

	var list struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	decodeJSON(t, "runs list", listData, &list)

	found := false
	for _, r := range list.Runs {
		if r.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected run %s to appear in GET /api/v1/runs", created.ID)
	}
}

// TestConsoleTargetsCRUD walks the whole /api/v1/targets lifecycle against the
// real console and the real PostgreSQL fixture, ending on the one status code
// that cannot be reached from a single table: the 409 a DELETE gets while a
// check definition still references the row (ON DELETE RESTRICT in migration
// 00004, surfaced as store.ErrInUse). That path crosses targets ->
// check_definitions -> the handler's error mapping, which is exactly the kind
// of wiring a unit test with a fake store cannot prove.
func TestConsoleTargetsCRUD(t *testing.T) {
	base := consoleBaseURL(t)

	name := uniqueName("e2e-tgt")
	id := createTarget(t, base, map[string]any{
		"name":    name,
		"kind":    "host",
		"address": "e2e-crud.invalid:80",
		"labels":  map[string]string{"origin": "e2e"},
	})

	// GET the stored row.
	getStatus, _, getData := mustRequest(t, http.MethodGet, base+"/api/v1/targets/"+id, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/targets/%s 200, got %d: %s", id, getStatus, getData)
	}
	var stored struct {
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Kind      string            `json:"kind"`
		Address   string            `json:"address"`
		Labels    map[string]string `json:"labels"`
		CreatedAt time.Time         `json:"createdAt"`
		UpdatedAt time.Time         `json:"updatedAt"`
	}
	decodeJSON(t, "get-target response", getData, &stored)
	if stored.ID != id || stored.Name != name || stored.Kind != "host" {
		t.Errorf("expected the stored row to echo id/name/kind, got %+v", stored)
	}
	if stored.Address != "e2e-crud.invalid:80" {
		t.Errorf("expected address %q, got %q", "e2e-crud.invalid:80", stored.Address)
	}
	if stored.Labels["origin"] != "e2e" {
		t.Errorf("expected labels to round-trip through jsonb, got %v", stored.Labels)
	}

	// LIST and find it. limit=500 is the store's own ceiling: a cluster that
	// has been through several e2e runs can hold many rows, and a default
	// page would make this assertion depend on how many.
	listStatus, _, listData := mustRequest(t, http.MethodGet, base+"/api/v1/targets?limit=500", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/targets 200, got %d: %s", listStatus, listData)
	}
	var page struct {
		Targets []struct {
			ID string `json:"id"`
		} `json:"targets"`
	}
	decodeJSON(t, "targets list", listData, &page)
	listed := false
	for _, tgt := range page.Targets {
		if tgt.ID == id {
			listed = true
			break
		}
	}
	if !listed {
		t.Errorf("expected target %s to appear in GET /api/v1/targets", id)
	}

	// UPDATE is a FULL replace: labels omitted here must come back as {},
	// never "left as-is", which is the one thing a PUT contract can get
	// silently wrong.
	putStatus, _, putData := mustRequest(t, http.MethodPut, base+"/api/v1/targets/"+id, map[string]any{
		"name":    name,
		"kind":    "host",
		"address": "e2e-crud-updated.invalid:443",
	})
	if putStatus != http.StatusOK {
		t.Fatalf("expected PUT /api/v1/targets/%s 200, got %d: %s", id, putStatus, putData)
	}
	var updated struct {
		Address   string            `json:"address"`
		Labels    map[string]string `json:"labels"`
		UpdatedAt time.Time         `json:"updatedAt"`
	}
	decodeJSON(t, "update-target response", putData, &updated)
	if updated.Address != "e2e-crud-updated.invalid:443" {
		t.Errorf("expected the update to replace address, got %q", updated.Address)
	}
	if len(updated.Labels) != 0 {
		t.Errorf("expected an omitted labels field to replace with {}, got %v", updated.Labels)
	}
	if updated.UpdatedAt.Before(stored.CreatedAt) {
		t.Errorf("expected updatedAt (%s) at or after createdAt (%s)", updated.UpdatedAt, stored.CreatedAt)
	}

	// A duplicate name is 422, not 409 (docs/console-api.yaml: it is a
	// rejected field value in an otherwise well-formed body).
	dupStatus, _, dupData := mustRequest(t, http.MethodPost, base+"/api/v1/targets", map[string]any{
		"name":    name,
		"kind":    "host",
		"address": "e2e-crud-dup.invalid:80",
	})
	if dupStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected a duplicate target name to be 422, got %d: %s", dupStatus, dupData)
	}
	if dupStatus == http.StatusCreated {
		// Belt and braces: if the server ever DID accept it, the row must not
		// outlive this test.
		var dup struct {
			ID string `json:"id"`
		}
		decodeJSON(t, "duplicate-target response", dupData, &dup)
		deleteResource(t, base+"/api/v1/targets/"+dup.ID)
	}

	// THE 409. The definition is created disabled on purpose: this assertion
	// is about referential integrity, and an enabled definition would also
	// enter the reconciler's view of the fleet for as long as the test runs.
	defID := createDefinition(t, base, map[string]any{
		"name":                uniqueName("e2e-tgtdef"),
		"sourceSelection":     "one-per-zone",
		"destinationKind":     "target",
		"destinationTargetId": id,
		"checkType":           "tcp",
		"plane":               "pod",
		"enabled":             false,
	})

	conflictStatus, _, conflictData := mustRequest(t, http.MethodDelete, base+"/api/v1/targets/"+id, nil)
	if conflictStatus != http.StatusConflict {
		t.Fatalf("expected DELETE of an in-use target to be 409, got %d: %s", conflictStatus, conflictData)
	}

	// Drop the reference, and the same DELETE now succeeds.
	defDeleteStatus, _, defDeleteData := mustRequest(t, http.MethodDelete, base+"/api/v1/checks/"+defID, nil)
	if defDeleteStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/checks/%s 204, got %d: %s", defID, defDeleteStatus, defDeleteData)
	}

	delStatus, _, delData := mustRequest(t, http.MethodDelete, base+"/api/v1/targets/"+id, nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/targets/%s 204, got %d: %s", id, delStatus, delData)
	}

	goneStatus, _, _ := mustRequest(t, http.MethodGet, base+"/api/v1/targets/"+id, nil)
	if goneStatus != http.StatusNotFound {
		t.Errorf("expected GET of a deleted target to be 404, got %d", goneStatus)
	}

	// An id that is not a canonical UUID is a 404, never a 400 or a 502.
	malformedStatus, _, _ := mustRequest(t, http.MethodGet, base+"/api/v1/targets/not-a-uuid", nil)
	if malformedStatus != http.StatusNotFound {
		t.Errorf("expected GET of a malformed target id to be 404, got %d", malformedStatus)
	}
}

// TestConsoleSchedule proves the console's schedule loop actually runs in a
// real cluster: an interval schedule is seeded due immediately
// (httpapi.seedNextFireAt), the advisory-locked tick picks it up and fires it
// through the SAME diagnostics runner POST /api/v1/runs uses, and the run row
// it produces carries initiatorKind "scheduler" with the SCHEDULE's id --
// which is the only evidence that distinguishes a scheduled run from the
// on-demand one TestConsoleRuns creates.
//
// The second half is the half that catches the real regression: disabling a
// schedule must actually stop it. check_schedules_due_idx is a partial index
// on `enabled`, so a disabled row simply stops being handed out -- but that
// is a claim about an index predicate, and an e2e is where it gets tested
// against the loop that reads it.
func TestConsoleSchedule(t *testing.T) {
	base := consoleBaseURL(t)

	defID := createDefinition(t, base, map[string]any{
		"name":            uniqueName("e2e-schdef"),
		"sourceSelection": "one-per-zone",
		// "node" keeps the fired run on the agents' own peer mesh -- the same
		// shape TestConsoleRuns dispatches -- so this test measures the
		// SCHEDULER, not the external-check path the two tests below own.
		"destinationKind": "node",
		"checkType":       "tcp",
		"plane":           "pod",
		"enabled":         true,
	})

	// 10s is the store's floor for kind "interval"; anything shorter is
	// clamped up to it, so this is the fastest cadence the API can express
	// and the tightest budget these assertions can be given.
	const interval = 10 * time.Second
	sched := createSchedule(t, base, map[string]any{
		"definitionId": defID,
		"kind":         "interval",
		"intervalNs":   interval.Nanoseconds(),
		"enabled":      true,
	})
	if sched.Kind != "interval" {
		t.Fatalf("expected the stored schedule kind to be interval, got %q", sched.Kind)
	}
	if sched.IntervalNs < interval.Nanoseconds() {
		t.Errorf("expected intervalNs >= %d (the 10s floor), got %d", interval.Nanoseconds(), sched.IntervalNs)
	}
	if sched.NextFireAt == nil {
		t.Errorf("expected an interval schedule to be seeded into the due index (nextFireAt non-null)")
	}

	// The schedule must be findable by its definition.
	listStatus, _, listData := mustRequest(t, http.MethodGet, base+"/api/v1/schedules?definitionId="+defID, nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/schedules?definitionId= 200, got %d: %s", listStatus, listData)
	}
	var schedPage struct {
		Schedules []scheduleRow `json:"schedules"`
	}
	decodeJSON(t, "schedules list", listData, &schedPage)
	listed := false
	for i := range schedPage.Schedules {
		if schedPage.Schedules[i].ID == sched.ID {
			listed = true
			break
		}
	}
	if !listed {
		t.Errorf("expected schedule %s to appear in GET /api/v1/schedules?definitionId=%s", sched.ID, defID)
	}

	// Budget: the tick cadence is console.scheduler.tickInterval (5s in the
	// e2e values) and the run itself is a real fan-out through the
	// controller, so 120s is several whole cycles rather than a tight race.
	pollUntil(t, 120*time.Second, 3*time.Second,
		fmt.Sprintf("a run initiated by scheduler schedule %s", sched.ID), func() bool {
			return scheduledRunCount(t, base, sched.ID) > 0
		})

	// Disable it, then let anything already in flight land before taking the
	// baseline: a fire could have been dispatched in the same instant the PUT
	// was served, and counting that as "a run after disabling" would be a
	// race, not a regression.
	putStatus, _, putData := mustRequest(t, http.MethodPut, base+"/api/v1/schedules/"+sched.ID, map[string]any{
		"definitionId": defID,
		"kind":         "interval",
		"intervalNs":   sched.IntervalNs,
		"enabled":      false,
	})
	if putStatus != http.StatusOK {
		t.Fatalf("expected PUT /api/v1/schedules/%s 200, got %d: %s", sched.ID, putStatus, putData)
	}
	var disabled scheduleRow
	decodeJSON(t, "update-schedule response", putData, &disabled)
	if disabled.Enabled {
		t.Fatalf("expected the stored schedule to be disabled after the PUT")
	}

	time.Sleep(interval + 5*time.Second)
	baseline := scheduledRunCount(t, base, sched.ID)

	// Two and a half intervals of quiet. A disabled schedule that still fires
	// would produce at least two more runs in that window.
	time.Sleep(interval*2 + interval/2)
	after := scheduledRunCount(t, base, sched.ID)
	if after != baseline {
		t.Errorf("expected no further runs after disabling schedule %s, count went %d -> %d",
			sched.ID, baseline, after)
	}
}

// scheduledRunCount counts the runs on the newest page that were initiated by
// scheduleID. initiatorKind is asserted alongside the id deliberately: the id
// alone would also match a hypothetical user id, and "scheduler" is the exact
// string internal/console/scheduler writes.
func scheduledRunCount(t *testing.T, base, scheduleID string) int {
	t.Helper()
	status, _, data, err := request(t, http.MethodGet, base+"/api/v1/runs?limit=500", nil)
	if err != nil {
		t.Logf("list runs failed (treated as 0): %v", err)
		return 0
	}
	if status != http.StatusOK {
		t.Logf("list runs returned %d (treated as 0)", status)
		return 0
	}

	var page struct {
		Runs []struct {
			ID            string `json:"id"`
			InitiatorKind string `json:"initiatorKind"`
			InitiatorID   string `json:"initiatorId"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(data, &page); err != nil {
		t.Logf("decode runs list failed (treated as 0): %v", err)
		return 0
	}

	count := 0
	for _, run := range page.Runs {
		if run.InitiatorKind == "scheduler" && run.InitiatorID == scheduleID {
			count++
		}
	}
	return count
}

// TestConsoleExternalCheckAssignment is the continuous external-check path end
// to end, across four processes: the console stores a target + an enabled
// definition + a kind="continuous" schedule, the reconciler resolves that
// against the live topology and PUTs a per-agent assignment to the
// controller, the controller streams it to every agent over
// WatchExternalChecks, and the agent probes it on its own checker loop and
// exports kconmon_ng_external_results_total. None of that is observable from
// the console at all -- the result never travels back -- which is why the
// assertion is made against one agent Pod's own /metrics.
//
// The destination is an IN-CLUSTER Service (externalTargetAddr), so this test
// needs no internet egress and no public-address carve-out in the harness
// allowlist. "External" here means "not a peer agent", which a ClusterIP
// Service is.
func TestConsoleExternalCheckAssignment(t *testing.T) {
	base := consoleBaseURL(t)
	agentBase := agentBaseURL(t)

	// The target's NAME is what becomes the Prometheus `target` label value
	// for a destinationKind="target" definition (checks.Reconciler's
	// resolveTarget), so it is what the assertion below matches on.
	targetName := uniqueName("e2e-ext")
	targetID := createTarget(t, base, map[string]any{
		"name":    targetName,
		"kind":    "host",
		"address": externalTargetAddr(),
	})

	defID := createDefinition(t, base, map[string]any{
		"name": uniqueName("e2e-extdef"),
		// "all" and not "one-per-zone": the workflow port-forwards ONE agent
		// Pod, and only "all" guarantees THAT agent is among the ones the
		// assignment reaches.
		"sourceSelection":     "all",
		"destinationKind":     "target",
		"destinationTargetId": targetID,
		"checkType":           "tcp",
		"plane":               "pod",
		"enabled":             true,
	})

	// kind "continuous" carries no cadence at all -- the store rejects an
	// intervalNs on it, and the scheduler never fires it. It is the flag the
	// reconciler filters on, nothing more.
	sched := createSchedule(t, base, map[string]any{
		"definitionId": defID,
		"kind":         "continuous",
		"enabled":      true,
	})
	if sched.NextFireAt != nil {
		t.Errorf("expected a continuous schedule to stay out of the due index (nextFireAt null), got %s",
			sched.NextFireAt)
	}

	// Budget: one reconcile tick (5s in the e2e values) + the controller
	// push + one agent external tick (5s) is the happy path, so 180s is a
	// wide margin that still fails in reasonable time when the pipeline is
	// genuinely broken.
	wanted := map[string]string{"target": targetName, "target_kind": "host", "result": "success"}
	pollUntil(t, 180*time.Second, 5*time.Second,
		fmt.Sprintf("a successful kconmon_ng_external_results_total sample for target %q on %s",
			targetName, agentBase), func() bool {
			text := scrapeAgentMetrics(t, agentBase)
			if text == "" {
				return false
			}
			if denied := metricSamples(text, "kconmon_ng_external_denied_total",
				map[string]string{"target": targetName}); len(denied) > 0 {
				// Loud, and immediately: an in-cluster destination landing in
				// denied_total means the harness allowlist does not cover the
				// kind Service network, and no amount of further polling will
				// change that.
				t.Errorf("in-cluster target %q was REFUSED by the agent allowlist: %v", targetName, denied)
				return true
			}
			return len(metricSamples(text, "kconmon_ng_external_results_total", wanted)) > 0
		})
}

// TestConsoleExternalCheckDenied is THE security e2e for external checks.
//
// The agent's allowlist is enforced in-process, after DNS resolution, and it
// is the only thing standing between an operator-writable target row and the
// agents probing an arbitrary address from inside the cluster. Every unit
// test of it runs against an in-memory allowlist; this one runs against the
// allowlist an operator actually configures, reached through the whole
// console -> reconciler -> controller -> agent chain, and asserts BOTH halves
// of a refusal:
//
//   - kconmon_ng_external_denied_total{reason="cidr"} increments, so the
//     refusal is observable and alertable rather than silent;
//   - kconmon_ng_external_results_total carries NO sample for the target, so
//     a denied probe is neither a success nor a failure. Counting it as a
//     failure would report an allowlist misconfiguration as an outage at a
//     destination the agent never touched.
func TestConsoleExternalCheckDenied(t *testing.T) {
	base := consoleBaseURL(t)
	agentBase := agentBaseURL(t)

	targetName := uniqueName("e2e-deny")
	targetID := createTarget(t, base, map[string]any{
		"name":    targetName,
		"kind":    "host",
		"address": deniedTargetAddr(),
	})

	defID := createDefinition(t, base, map[string]any{
		"name":                uniqueName("e2e-denydef"),
		"sourceSelection":     "all",
		"destinationKind":     "target",
		"destinationTargetId": targetID,
		"checkType":           "tcp",
		"plane":               "pod",
		"enabled":             true,
	})

	createSchedule(t, base, map[string]any{
		"definitionId": defID,
		"kind":         "continuous",
		"enabled":      true,
	})

	denied := map[string]string{"target": targetName, "target_kind": "host", "reason": "cidr"}
	var lastScrape string
	pollUntil(t, 180*time.Second, 5*time.Second,
		fmt.Sprintf("a kconmon_ng_external_denied_total{reason=\"cidr\"} sample for target %q on %s",
			targetName, agentBase), func() bool {
			text := scrapeAgentMetrics(t, agentBase)
			if text == "" {
				return false
			}
			lastScrape = text
			return len(metricSamples(text, "kconmon_ng_external_denied_total", denied)) > 0
		})

	for _, line := range metricSamples(lastScrape, "kconmon_ng_external_denied_total", denied) {
		if value := sampleValue(t, line); value <= 0 {
			t.Errorf("expected a positive denial count, got %v on %q", value, line)
		}
	}

	// The probe never reached the network, so there must be no result series
	// for it at all -- not a failed one.
	if results := metricSamples(lastScrape, "kconmon_ng_external_results_total",
		map[string]string{"target": targetName}); len(results) > 0 {
		t.Errorf("expected NO kconmon_ng_external_results_total sample for a denied target, got %v", results)
	}
}

// ---------------------------------------------------------------------------
// M5: MTR path history, operator annotations, historical topology.
// ---------------------------------------------------------------------------

// mtrPairTimeout is the per-pair budget the mtr runs below ask for, set
// EXPLICITLY rather than left to the server: an omitted timeoutNs clamps to
// checks.minPerPairTimeout (1s), while one trace is up to
// config.checkers.mtr.maxHops probes at a 1s per-hop read deadline
// (internal/agent/agent.go hardcodes that one) plus a reverse-DNS lookup per
// answered hop. A 1s budget would cancel the trace mid-flight, the checker
// would return an error carrying no hops at all, and that outcome is
// indistinguishable from the kind limitation this test skips on -- so the
// suite would report an environment story about a timeout it set itself.
const mtrPairTimeout = 45 * time.Second

// mtrProjectionBudget bounds the wait for a finished trace to show up in path
// history. The projection runs in the SAME process that just completed the run
// (checks.Runner.projectMTRSnapshot, on the result-ingest path), so this is
// one database write behind a run that has already reached a terminal status;
// the budget is generous only so a loaded kind node cannot turn a slow write
// into a false "kind cannot traceroute" skip.
const mtrProjectionBudget = 60 * time.Second

// pathHop is the subset of a stored hop these tests assert on. The IP is the
// only field that takes part in the dedupe hash, and it is also what decides
// the skip branch below: internal/checker/mtr.go writes "*" for a TTL that
// never answered, and internal/console/checks/mtrproject.go drops those hops
// before hashing -- a trace whose every hop is silent projects to nothing.
type pathHop struct {
	Number int    `json:"number"`
	IP     string `json:"ip"`
}

// pathSnapshot is the subset of the PathSnapshot schema these tests assert on.
type pathSnapshot struct {
	ID          string    `json:"id"`
	SourceNode  string    `json:"sourceNode"`
	Destination string    `json:"destination"`
	PathHash    string    `json:"pathHash"`
	HopCount    int       `json:"hopCount"`
	Hops        []pathHop `json:"hops"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	TraceCount  int64     `json:"traceCount"`
}

// mtrRunResult is one pair's row out of GET /api/v1/runs/{id}. Result is the
// check payload stored verbatim, and it is decoded here with exactly the two
// fields internal/console/checks/mtrproject.go reads -- so what this test
// calls "an all-silent trace" is the same predicate the projector applies.
type mtrRunResult struct {
	SourceNode      string `json:"sourceNode"`
	DestinationNode string `json:"destinationNode"`
	Success         bool   `json:"success"`
	Error           string `json:"error"`
	Result          struct {
		Type    string `json:"type"`
		Details struct {
			Target string    `json:"target"`
			Hops   []pathHop `json:"hops"`
		} `json:"details"`
	} `json:"result"`
}

type mtrRunDetail struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Results []mtrRunResult `json:"results"`
}

// topologyNodePair returns two distinct node names that currently carry a
// registered agent, or skips when the cluster cannot express a pair at all.
//
// The names come from the AGENTS list, not the nodes list: a node with no
// agent is a node the controller cannot dispatch a trace to, and asking it to
// would produce a failed pair with no payload -- the one outcome that reads
// exactly like the kind limitation this test is careful to name honestly.
// Sorted so re-runs keep tracing the SAME pair, which is what lets path
// history accumulate on one row instead of scattering across pairs.
func topologyNodePair(t *testing.T, base string) (source, destination string) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/topology", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/topology 200, got %d: %s", status, data)
	}

	var topo struct {
		Agents []struct {
			NodeName string `json:"nodeName"`
		} `json:"agents"`
	}
	decodeJSON(t, "topology response", data, &topo)

	seen := make(map[string]struct{}, len(topo.Agents))
	names := make([]string, 0, len(topo.Agents))
	for _, agent := range topo.Agents {
		if agent.NodeName == "" {
			continue
		}
		if _, dup := seen[agent.NodeName]; dup {
			continue
		}
		seen[agent.NodeName] = struct{}{}
		names = append(names, agent.NodeName)
	}
	if len(names) < 2 {
		t.Skipf("need two nodes with a registered agent to trace between, topology has %d: %v", len(names), names)
	}
	slices.Sort(names)
	return names[0], names[1]
}

// listPathSnapshots reads one pair's whole route history (limit=500 is the
// store's own ceiling and a pair has orders of magnitude fewer DISTINCT routes
// than that). It reports failure instead of failing: every caller is either a
// poll body or a branch that has its own verdict to reach.
func listPathSnapshots(t *testing.T, base, source, destination string) ([]pathSnapshot, bool) {
	t.Helper()
	query := url.Values{}
	query.Set("source", source)
	query.Set("destination", destination)
	query.Set("limit", "500")

	status, _, data, err := request(t, http.MethodGet, base+"/api/v1/mtr/snapshots?"+query.Encode(), nil)
	if err != nil {
		t.Logf("list path snapshots failed (will retry): %v", err)
		return nil, false
	}
	if status != http.StatusOK {
		t.Logf("list path snapshots returned %d (will retry): %s", status, data)
		return nil, false
	}

	var page struct {
		Snapshots []pathSnapshot `json:"snapshots"`
	}
	if decodeErr := json.Unmarshal(data, &page); decodeErr != nil {
		t.Logf("decode path snapshots failed (will retry): %v", decodeErr)
		return nil, false
	}
	return page.Snapshots, true
}

// traceTotal sums a pair's traces across its distinct routes. It is the one
// counter that cannot be inherited from an earlier e2e run against the same
// cluster: the node pair is not run-unique, so "a snapshot exists" would pass
// without this test tracing anything, while "the total grew" cannot.
func traceTotal(snaps []pathSnapshot) int64 {
	var total int64
	for i := range snaps {
		total += snaps[i].TraceCount
	}
	return total
}

// pathHashes indexes a pair's routes by their content hash.
func pathHashes(snaps []pathSnapshot) map[string]pathSnapshot {
	out := make(map[string]pathSnapshot, len(snaps))
	for i := range snaps {
		out[snaps[i].PathHash] = snaps[i]
	}
	return out
}

// awaitTraceTotal polls a pair's history until its total traceCount exceeds
// above, and REPORTS the outcome rather than failing on timeout: "no trace was
// projected" is a state TestConsoleMTRHistory has to inspect the run to
// interpret, so pollUntil -- which ends the test right there -- is the wrong
// tool for this one wait.
func awaitTraceTotal(t *testing.T, base, source, destination string, above int64) ([]pathSnapshot, bool) {
	t.Helper()
	deadline := time.Now().Add(mtrProjectionBudget)
	for {
		snaps, ok := listPathSnapshots(t, base, source, destination)
		if ok && traceTotal(snaps) > above {
			return snaps, true
		}
		if time.Now().After(deadline) {
			return snaps, false
		}
		time.Sleep(3 * time.Second)
	}
}

// getRunDetail fetches one run with its per-pair results, reporting failure
// for a poll body to retry on.
func getRunDetail(t *testing.T, base, runID string) (mtrRunDetail, bool) {
	t.Helper()
	var detail mtrRunDetail
	status, _, data, err := request(t, http.MethodGet, base+"/api/v1/runs/"+runID, nil)
	if err != nil {
		t.Logf("get run %s failed (will retry): %v", runID, err)
		return detail, false
	}
	if status != http.StatusOK {
		t.Logf("get run %s returned %d (will retry)", runID, status)
		return detail, false
	}
	if decodeErr := json.Unmarshal(data, &detail); decodeErr != nil {
		t.Logf("decode run %s failed (will retry): %v", runID, decodeErr)
		return detail, false
	}
	return detail, true
}

// runMTR dispatches ONE mtr pair through the real controller and agents and
// waits for the run to reach a terminal status, returning its id.
//
// Success is deliberately NOT asserted. Whether the trace carried usable hops
// is precisely what the caller then has to decide on, and a pair that failed
// is evidence to report, never a reason to stop before reporting it.
func runMTR(t *testing.T, base, source, destination string) string {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodPost, base+"/api/v1/runs", map[string]any{
		"sources":      []string{source},
		"destinations": []string{destination},
		"type":         "mtr",
		"plane":        "pod",
		"timeoutNs":    mtrPairTimeout.Nanoseconds(),
	})
	if status != http.StatusAccepted {
		t.Fatalf("expected POST /api/v1/runs (mtr) 202, got %d: %s", status, data)
	}

	var created struct {
		ID        string `json:"id"`
		PairTotal int32  `json:"pairTotal"`
	}
	decodeJSON(t, "create-mtr-run response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty run id in the create-run response")
	}
	// One source, one destination, one pair -- which is what makes "exactly
	// one trace was added" an assertion rather than a guess further down.
	if created.PairTotal != 1 {
		t.Fatalf("expected an mtr run over one source and one destination to plan 1 pair, got %d", created.PairTotal)
	}

	// 120s is a hair under three times the per-pair budget this run asked for,
	// which is wide enough for dispatch and ingest on a loaded kind node and
	// tight enough that two of these runs cannot eat the workflow's own job
	// timeout when the pipeline is genuinely stuck.
	pollUntil(t, 120*time.Second, 3*time.Second,
		fmt.Sprintf("mtr run %s to reach a terminal status", created.ID), func() bool {
			detail, ok := getRunDetail(t, base, created.ID)
			if !ok {
				return false
			}
			switch detail.Status {
			case "succeeded", "failed", "partial", "cancelled":
				return true
			default:
				return false
			}
		})
	return created.ID
}

// silentHopIP is internal/console/checks/mtrproject.go's silentHop predicate,
// spelled once more here so the skip branch below tests the SAME thing the
// projector rejects on rather than an approximation of it.
func silentHopIP(ip string) bool {
	trimmed := strings.TrimSpace(ip)
	return trimmed == "" || trimmed == "*"
}

// allSilentTrace reports whether the run carried at least one trace and every
// hop of every trace was silent -- the exact input for which
// ProjectMTRSnapshot returns false and writes no snapshot.
func allSilentTrace(detail *mtrRunDetail) bool {
	traced := false
	for i := range detail.Results {
		hops := detail.Results[i].Result.Details.Hops
		if len(hops) == 0 {
			continue
		}
		traced = true
		for _, hop := range hops {
			if !silentHopIP(hop.IP) {
				return false
			}
		}
	}
	return traced
}

// mtrRunEvidence renders what a run's pairs actually reported, hop addresses
// included. It is what turns the skip below into a statement with proof
// attached instead of a shrug.
func mtrRunEvidence(detail *mtrRunDetail) string {
	parts := make([]string, 0, len(detail.Results)+1)
	parts = append(parts, fmt.Sprintf("run %s status=%s results=%d", detail.ID, detail.Status, len(detail.Results)))
	for i := range detail.Results {
		result := &detail.Results[i]
		hops := make([]string, 0, len(result.Result.Details.Hops))
		for _, hop := range result.Result.Details.Hops {
			hops = append(hops, fmt.Sprintf("%d:%s", hop.Number, hop.IP))
		}
		parts = append(parts, fmt.Sprintf("%s->%s success=%t type=%q error=%q hops=[%s]",
			result.SourceNode, result.DestinationNode, result.Success,
			result.Result.Type, result.Error, strings.Join(hops, " ")))
	}
	return strings.Join(parts, "; ")
}

// endUnprojected ends the test after a dispatched trace produced no path
// history, deciding between a skip and a failure on the RUN's own results.
//
// The distinction is the whole point of this helper. traceroute from inside a
// kind container can legitimately come back with every TTL unanswered, and the
// projector rejects an all-silent trace on purpose (hashing "*" would make a
// stable route look new every time an intermediate router rate-limited ICMP) --
// so that outcome is an environment limitation, not a defect, and it is
// skipped with the hop list quoted as evidence. Anything else -- a pair that
// never produced a payload, a checker error, a trace with usable hops that
// still did not project -- is a genuine break in the console's ingest path and
// fails, with the same evidence attached.
//
// It never returns: both branches end the test on the calling goroutine.
func endUnprojected(t *testing.T, base, runID, source, destination string) {
	t.Helper()
	detail, ok := getRunDetail(t, base, runID)
	if !ok {
		t.Fatalf("no path-history row appeared for %s -> %s after run %s, and the run itself could not be read",
			source, destination, runID)
	}
	if allSilentTrace(&detail) {
		t.Skipf("traceroute inside this kind cluster answered no TTL for %s -> %s: every hop is silent, "+
			"and internal/console/checks/mtrproject.go rejects an all-silent trace by design (no snapshot, "+
			"not an error). Environment limitation, not a console defect -- evidence: %s",
			source, destination, mtrRunEvidence(&detail))
	}
	t.Fatalf("no path-history row appeared for %s -> %s within %s, and the run's trace was NOT all-silent, "+
		"so the projector should have written one: %s",
		source, destination, mtrProjectionBudget, mtrRunEvidence(&detail))
}

// assertSnapshotInvariants checks what every stored route must satisfy
// regardless of which hops answered: it belongs to the pair that was traced,
// it carries a content hash, hopCount agrees with the hop list it is derived
// from, no silent hop survived normalization, and -- the dedupe invariant, the
// one that holds whether or not the route changed -- no two rows share a hash.
func assertSnapshotInvariants(t *testing.T, snaps []pathSnapshot, source, destination string) {
	t.Helper()
	if len(snaps) == 0 {
		t.Fatalf("expected at least one stored route for %s -> %s", source, destination)
	}

	seen := make(map[string]string, len(snaps))
	for i := range snaps {
		snap := &snaps[i]
		if snap.SourceNode != source || snap.Destination != destination {
			t.Errorf("expected snapshot %s to carry the traced pair %s -> %s, got %s -> %s",
				snap.ID, source, destination, snap.SourceNode, snap.Destination)
		}
		if snap.PathHash == "" {
			t.Errorf("expected snapshot %s to carry a path hash", snap.ID)
		}
		if snap.HopCount != len(snap.Hops) {
			t.Errorf("expected snapshot %s hopCount %d to match its %d stored hops",
				snap.ID, snap.HopCount, len(snap.Hops))
		}
		if len(snap.Hops) == 0 {
			t.Errorf("expected snapshot %s to carry hops (the store refuses a hopless route)", snap.ID)
		}
		for _, hop := range snap.Hops {
			if silentHopIP(hop.IP) {
				t.Errorf("expected normalizeHops to drop silent hops, snapshot %s kept hop %d %q",
					snap.ID, hop.Number, hop.IP)
			}
		}
		if snap.TraceCount < 1 {
			t.Errorf("expected snapshot %s to have been produced by at least one trace, got %d",
				snap.ID, snap.TraceCount)
		}
		if snap.LastSeen.Before(snap.FirstSeen) {
			t.Errorf("expected snapshot %s lastSeen (%s) at or after firstSeen (%s)",
				snap.ID, snap.LastSeen, snap.FirstSeen)
		}
		if other, dup := seen[snap.PathHash]; dup {
			t.Errorf("path hash %s is stored twice (%s and %s): the same route must dedupe onto one row",
				snap.PathHash, other, snap.ID)
		}
		seen[snap.PathHash] = snap.ID
	}
}

// mtrDestination is the subset of the MTRDestination schema asserted on.
type mtrDestination struct {
	SourceNode    string    `json:"sourceNode"`
	Destination   string    `json:"destination"`
	SnapshotCount int64     `json:"snapshotCount"`
	TraceCount    int64     `json:"traceCount"`
	FirstSeen     time.Time `json:"firstSeen"`
	LastSeen      time.Time `json:"lastSeen"`
}

// findMTRDestination looks one pair up in the unpaged destinations listing.
func findMTRDestination(t *testing.T, base, source, destination string) (mtrDestination, bool) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/mtr/destinations", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/mtr/destinations 200, got %d: %s", status, data)
	}

	var list struct {
		Destinations []mtrDestination `json:"destinations"`
	}
	decodeJSON(t, "mtr destinations list", data, &list)
	for i := range list.Destinations {
		if list.Destinations[i].SourceNode == source && list.Destinations[i].Destination == destination {
			return list.Destinations[i], true
		}
	}
	return mtrDestination{}, false
}

// TestConsoleMTRHistory is MTR path history end to end: a real trace between
// two real agents, projected onto mtr_path_snapshots by the console's own
// result-ingest path, then traced AGAIN so the dedupe that makes this table
// useful is observable rather than merely unit-tested.
//
// Two things make the assertions here honest against a cluster that has
// already run this suite. The pair is not run-unique -- there are only so many
// nodes -- so every claim is made about GROWTH from a baseline taken first,
// never about a row simply existing. And the second run's effect is bounded
// exactly: one pair, one trace, so the pair's total traceCount must move by
// exactly one while its DISTINCT-route count either holds (the route repeated,
// which is the dedupe) or grows by at most one (an intermediate router went
// silent, so normalizeHops produced a genuinely different hop list).
func TestConsoleMTRHistory(t *testing.T) {
	base := consoleBaseURL(t)
	source, destination := topologyNodePair(t, base)

	baseline, ok := listPathSnapshots(t, base, source, destination)
	if !ok {
		t.Fatalf("GET /api/v1/mtr/snapshots did not answer for %s -> %s", source, destination)
	}
	baselineTraces := traceTotal(baseline)

	firstRun := runMTR(t, base, source, destination)
	afterFirst, projected := awaitTraceTotal(t, base, source, destination, baselineTraces)
	if !projected {
		endUnprojected(t, base, firstRun, source, destination)
		return // endUnprojected never returns; this keeps the flow readable.
	}
	assertSnapshotInvariants(t, afterFirst, source, destination)

	firstTraces := traceTotal(afterFirst)
	firstHashes := pathHashes(afterFirst)

	// THE SAME PAIR AGAIN. Nothing else in this suite traces MTR -- the
	// scheduler's definitions are tcp, and the agents' own reactive traces
	// never reach this table (the projector runs on the console's run-ingest
	// path only) -- so this adds exactly one trace.
	secondRun := runMTR(t, base, source, destination)
	afterSecond, projectedAgain := awaitTraceTotal(t, base, source, destination, firstTraces)
	if !projectedAgain {
		endUnprojected(t, base, secondRun, source, destination)
		return // as above.
	}
	assertSnapshotInvariants(t, afterSecond, source, destination)

	if got, want := traceTotal(afterSecond), firstTraces+1; got != want {
		t.Errorf("expected one mtr run over one pair to record exactly one trace, traceCount went %d -> %d (want %d)",
			firstTraces, got, want)
	}

	secondHashes := pathHashes(afterSecond)
	// A stored route is append-only: a repeat trace updates its row, and
	// nothing ever removes or rewrites one.
	for hash := range firstHashes {
		if _, still := secondHashes[hash]; !still {
			t.Errorf("route %s disappeared from %s -> %s history after a second trace", hash, source, destination)
		}
	}

	if len(secondHashes) == len(firstHashes) {
		// THE DEDUPE OBSERVATION: the second trace took a route already on
		// record, so the pair's distinct-route count did not move while its
		// trace count did -- one row absorbed the repeat.
		grew := 0
		for hash, snap := range secondHashes {
			prev, known := firstHashes[hash]
			if !known {
				continue
			}
			switch {
			case snap.TraceCount > prev.TraceCount:
				grew++
				if snap.LastSeen.Before(prev.LastSeen) {
					t.Errorf("expected lastSeen to advance on repeated route %s, got %s (was %s)",
						hash, snap.LastSeen, prev.LastSeen)
				}
				if !snap.FirstSeen.Equal(prev.FirstSeen) {
					t.Errorf("expected firstSeen to stay put on repeated route %s, got %s (was %s)",
						hash, snap.FirstSeen, prev.FirstSeen)
				}
			case snap.TraceCount < prev.TraceCount:
				t.Errorf("traceCount went BACKWARDS on route %s: %d -> %d", hash, prev.TraceCount, snap.TraceCount)
			}
		}
		if grew != 1 {
			t.Errorf("expected exactly one route's traceCount to grow on a repeat trace, %d did", grew)
		}
	} else {
		// The two traces genuinely took different routes, which inside kind
		// means a router answered one probe and stayed silent on the next:
		// normalizeHops DROPS a silent hop, so the hop-IP list -- and with it
		// the hash -- is a different route by construction. That is the
		// projector working as designed, so it is reported, not failed; the
		// dedupe invariant that holds either way (one row per hash) is
		// asserted in assertSnapshotInvariants above.
		t.Logf("route changed between the two traces for %s -> %s (%d -> %d distinct routes): "+
			"an intermediate hop answered one probe and not the other",
			source, destination, len(firstHashes), len(secondHashes))
		if len(secondHashes) != len(firstHashes)+1 {
			t.Errorf("one trace can add at most one route, distinct count went %d -> %d",
				len(firstHashes), len(secondHashes))
		}
	}

	// The pair must be in the destinations listing, with the two counters the
	// listing exists to keep apart agreeing with the page above: snapshotCount
	// is DISTINCT ROUTES, traceCount is how many traces produced them.
	pair, found := findMTRDestination(t, base, source, destination)
	if !found {
		t.Fatalf("expected GET /api/v1/mtr/destinations to carry the traced pair %s -> %s", source, destination)
	}
	if pair.SnapshotCount != int64(len(afterSecond)) {
		t.Errorf("expected destinations snapshotCount %d to match the %d routes on the snapshots page",
			pair.SnapshotCount, len(afterSecond))
	}
	if want := traceTotal(afterSecond); pair.TraceCount != want {
		t.Errorf("expected destinations traceCount %d to match the summed trace count %d", pair.TraceCount, want)
	}
	if pair.LastSeen.Before(pair.FirstSeen) {
		t.Errorf("expected pair lastSeen (%s) at or after firstSeen (%s)", pair.LastSeen, pair.FirstSeen)
	}

	// Both filters are REQUIRED: dropping one is 422, never an unfiltered
	// listing of every trace the fleet ever took.
	partialStatus, _, partialData := mustRequest(t, http.MethodGet,
		base+"/api/v1/mtr/snapshots?source="+url.QueryEscape(source), nil)
	if partialStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected GET /api/v1/mtr/snapshots without a destination to be 422, got %d: %s",
			partialStatus, partialData)
	}
}

// annotation is the subset of the Annotation schema these tests assert on.
type annotation struct {
	ID        string     `json:"id"`
	StartAt   time.Time  `json:"startAt"`
	EndAt     *time.Time `json:"endAt"`
	Scope     string     `json:"scope"`
	Text      string     `json:"text"`
	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
}

// listAnnotations reads one page with the given query. The query is built by
// the caller because ?scope= carries THREE states and only url.Values can
// express the middle one: a key that is present and empty.
func listAnnotations(t *testing.T, base string, query url.Values) []annotation {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/annotations?"+query.Encode(), nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/annotations?%s 200, got %d: %s", query.Encode(), status, data)
	}
	var page struct {
		Annotations []annotation `json:"annotations"`
	}
	decodeJSON(t, "annotations list", data, &page)
	return page.Annotations
}

// containsAnnotation reports whether a page carries the given id.
func containsAnnotation(page []annotation, id string) bool {
	for i := range page {
		if page[i].ID == id {
			return true
		}
	}
	return false
}

// TestConsoleAnnotations walks one operator note's whole life against the real
// console and PostgreSQL, and spends most of its assertions on the ONE part of
// this API that cannot be guessed from its shape: ?scope= has three states,
// and the middle one -- present but empty -- means "the global marks only",
// because "" is a real scope value here rather than a missing filter. A server
// that treated an empty parameter as absent would still pass every other
// assertion in this file while quietly returning every scope to a chart that
// asked for global marks.
//
// The viewer half of the RBAC contract (annotations:read in both roles,
// annotations:write only from operator up) is NOT exercised here: the e2e
// console runs auth.mode=anonymous, whose authenticator returns the single
// configured subject for every request and ignores credentials entirely
// (internal/console/authn/authn.go), so this harness has no way to present a
// second role at all. internal/console/authz covers it in unit tests.
func TestConsoleAnnotations(t *testing.T) {
	base := consoleBaseURL(t)

	scope := uniqueName("e2e-ann")
	// Whole seconds: the API speaks RFC3339 on the query string, so a
	// sub-second startAt could not be echoed back into a window bound exactly.
	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(30 * time.Second)

	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/annotations", map[string]any{
		"startAt": start.Format(time.RFC3339),
		"endAt":   end.Format(time.RFC3339),
		"scope":   scope,
		"text":    "e2e annotation for " + scope,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/annotations 201, got %d: %s", status, data)
	}

	var created annotation
	decodeJSON(t, "create-annotation response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty annotation id in the create response")
	}
	t.Cleanup(func() { deleteResource(t, base+"/api/v1/annotations/"+created.ID) })

	if want := "/api/v1/annotations/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}
	if created.Scope != scope {
		t.Errorf("expected the stored scope to echo %q, got %q", scope, created.Scope)
	}
	if !created.StartAt.Equal(start) {
		t.Errorf("expected startAt to round-trip as %s, got %s", start, created.StartAt)
	}
	if created.EndAt == nil || !created.EndAt.Equal(end) {
		t.Errorf("expected endAt to round-trip as %s, got %v", end, created.EndAt)
	}
	// createdBy is the SERVER's view of the subject and was never in the body
	// this test sent; an empty one would mean attribution was lost.
	if created.CreatedBy == "" {
		t.Errorf("expected createdBy to be filled in by the server, got %q", created.CreatedBy)
	}

	window := url.Values{}
	window.Set("from", start.Add(-2*time.Minute).Format(time.RFC3339))
	window.Set("to", start.Add(2*time.Minute).Format(time.RFC3339))
	window.Set("limit", "500")

	// 1. scope present and exact: the mark comes back, and nothing else does.
	exact := url.Values{}
	for key, values := range window {
		exact[key] = values
	}
	exact.Set("scope", scope)
	page := listAnnotations(t, base, exact)
	if !containsAnnotation(page, created.ID) {
		t.Errorf("expected annotation %s in a window listing filtered by its own scope %q", created.ID, scope)
	}
	for i := range page {
		if page[i].Scope != scope {
			t.Errorf("expected ?scope=%s to return that scope only, got %q on %s", scope, page[i].Scope, page[i].ID)
		}
	}

	// 2. scope PRESENT AND EMPTY: the global marks only, so a scoped mark must
	// not appear -- the assertion this whole test exists for.
	globalOnly := url.Values{}
	for key, values := range window {
		globalOnly[key] = values
	}
	globalOnly.Set("scope", "")
	page = listAnnotations(t, base, globalOnly)
	if containsAnnotation(page, created.ID) {
		t.Errorf("expected ?scope= (present and empty) to return GLOBAL annotations only, but %s (scope %q) came back",
			created.ID, scope)
	}
	for i := range page {
		if page[i].Scope != "" {
			t.Errorf("expected ?scope= to return only global marks, got scope %q on %s", page[i].Scope, page[i].ID)
		}
	}

	// 3. scope ABSENT: every scope, so the mark is back.
	if page = listAnnotations(t, base, window); !containsAnnotation(page, created.ID) {
		t.Errorf("expected an unfiltered-scope listing to carry annotation %s", created.ID)
	}

	// A window the mark does not overlap must not return it -- the from/to
	// bounds are a filter, not decoration.
	elsewhere := url.Values{}
	elsewhere.Set("scope", scope)
	elsewhere.Set("from", start.Add(time.Hour).Format(time.RFC3339))
	elsewhere.Set("to", start.Add(2*time.Hour).Format(time.RFC3339))
	elsewhere.Set("limit", "500")
	if page = listAnnotations(t, base, elsewhere); containsAnnotation(page, created.ID) {
		t.Errorf("expected annotation %s to be outside a window an hour after it ended", created.ID)
	}

	// Delete, then delete again: a mark that is not there is 404, never a
	// second success. Idempotent-delete would make a stale UI think it had
	// removed something it never had.
	delStatus, _, delData := mustRequest(t, http.MethodDelete, base+"/api/v1/annotations/"+created.ID, nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/annotations/%s 204, got %d: %s", created.ID, delStatus, delData)
	}
	goneStatus, _, goneData := mustRequest(t, http.MethodDelete, base+"/api/v1/annotations/"+created.ID, nil)
	if goneStatus != http.StatusNotFound {
		t.Errorf("expected a second DELETE of annotation %s to be 404, got %d: %s", created.ID, goneStatus, goneData)
	}

	if page = listAnnotations(t, base, exact); containsAnnotation(page, created.ID) {
		t.Errorf("expected annotation %s to be gone from its own scope's listing after the delete", created.ID)
	}
}

// historicalTopologyBound returns an instant that is in the past, at or after
// the retention floor, and at or after at least one topology_changed event --
// plus how many such events the page carried.
//
// Picking "two minutes ago" outright would be a coin flip on a fresh cluster:
// ?at= answers 422 for any instant older than the OLDEST retained event, and a
// console whose history started ninety seconds ago has no honest answer for
// two minutes ago. That 422 is correct behavior, so a test asserting 200 there
// would really be asserting on how long the workflow had been running.
//
// The floor is derived from a topology_changed row rather than from any event,
// for two separate reasons: such a row is retained by definition, so an
// instant at or after it can never be pre-retention; and the fold counts only
// this type, so the same instant guarantees eventsFolded >= 1 -- a floor taken
// from some other event type could satisfy the 422 check and still fold
// nothing. It is rounded UP to the next whole second because ?at= is carried
// as RFC 3339 with second precision, and rounding a sub-second timestamp DOWN
// would land just before the floor it was derived from.
func historicalTopologyBound(t *testing.T, base string) (at time.Time, retained int) {
	t.Helper()
	var page struct {
		Events []struct {
			Timestamp time.Time `json:"timestamp"`
		} `json:"events"`
	}

	pollUntil(t, 60*time.Second, 2*time.Second,
		"at least one persisted topology_changed event to reconstruct a topology from", func() bool {
			status, _, data, err := request(t, http.MethodGet,
				base+"/api/v1/events?type=topology_changed&limit=500", nil)
			if err != nil {
				t.Logf("events request failed (will retry): %v", err)
				return false
			}
			if status != http.StatusOK {
				t.Logf("events request returned %d (will retry)", status)
				return false
			}
			if decodeErr := json.Unmarshal(data, &page); decodeErr != nil {
				t.Logf("decode events response failed (will retry): %v", decodeErr)
				return false
			}
			return len(page.Events) > 0
		})

	// The listing is newest-first, so the last entry is the oldest one this
	// page proves is still retained.
	oldest := page.Events[len(page.Events)-1].Timestamp
	floor := oldest.UTC().Truncate(time.Second).Add(time.Second)
	at = time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	if at.Before(floor) {
		at = floor
	}
	// Rounding up can put the floor as much as a second ahead of the event it
	// came from, so on a console whose history started this very second `at`
	// could be in the FUTURE -- a 400. Wait it out instead of racing it.
	if wait := time.Until(at); wait > 0 {
		time.Sleep(wait + time.Second)
	}
	return at, len(page.Events)
}

// TestConsoleTopologyAt covers GET /api/v1/topology?at= against real persisted
// history: the fold's answer, and the three refusals around it.
//
// It asserts the FIELDS, never node presence, and that is the honest choice
// rather than a weak one. A topology_changed event records {reason, nodeName,
// agentId}, and the controller shipped with this release publishes the reason
// WITHOUT the other two -- so history written by it folds to an EMPTY nodes
// array with every event counted in unfoldableEvents. Asserting a node would
// therefore fail against a correct server; the counters are what carry the
// information, which is exactly why they are in the response body.
func TestConsoleTopologyAt(t *testing.T) {
	base := consoleBaseURL(t)
	at, retained := historicalTopologyBound(t, base)

	status, _, data := mustRequest(t, http.MethodGet,
		base+"/api/v1/topology?at="+url.QueryEscape(at.Format(time.RFC3339)), nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/topology?at=%s 200 (%d topology_changed events retained), got %d: %s",
			at.Format(time.RFC3339), retained, status, data)
	}

	var folded struct {
		Nodes            []json.RawMessage `json:"nodes"`
		Agents           []json.RawMessage `json:"agents"`
		Timestamp        time.Time         `json:"timestamp"`
		Historical       bool              `json:"historical"`
		AsOf             time.Time         `json:"asOf"`
		EventsFolded     int               `json:"eventsFolded"`
		UnfoldableEvents int               `json:"unfoldableEvents"`
		Truncated        bool              `json:"truncated"`
	}
	decodeJSON(t, "historical topology response", data, &folded)

	if !folded.Historical {
		t.Errorf("expected historical=true on an ?at= response")
	}
	if !folded.AsOf.Equal(at) {
		t.Errorf("expected asOf to echo the requested instant %s, got %s", at, folded.AsOf)
	}
	if folded.Timestamp.IsZero() {
		t.Errorf("expected a timestamp (the last change at or before asOf, or asOf itself), got the zero time")
	}
	if folded.Timestamp.After(folded.AsOf) {
		t.Errorf("expected timestamp (%s) at or before asOf (%s): a fold cannot see past what it was asked about",
			folded.Timestamp, folded.AsOf)
	}
	// at is at or after a retained topology_changed event (see
	// historicalTopologyBound), so that row is inside the fold's range and at
	// least one event was consumed -- which is what makes unfoldableEvents
	// below a statement about the events' CONTENT rather than their absence.
	if folded.EventsFolded < 1 {
		t.Errorf("expected at least one event to be folded for an instant at or after a retained topology_changed event, got %d",
			folded.EventsFolded)
	}
	if folded.UnfoldableEvents < 0 || folded.UnfoldableEvents > folded.EventsFolded {
		t.Errorf("expected unfoldableEvents (%d) within [0, eventsFolded=%d]",
			folded.UnfoldableEvents, folded.EventsFolded)
	}
	if folded.Truncated {
		t.Errorf("expected truncated=false: this cluster cannot have written the fold's 100k-row guard")
	}
	// nodes/agents must be ARRAYS, possibly empty -- never null. See the doc
	// comment: an empty fold beside a non-zero unfoldableEvents is the
	// expected shape here, and it is logged rather than asserted on.
	if folded.Nodes == nil || folded.Agents == nil {
		t.Errorf("expected nodes and agents to be JSON arrays (possibly empty), got nodes=%v agents=%v",
			folded.Nodes, folded.Agents)
	}
	t.Logf("fold at %s: %d nodes, %d agents, eventsFolded=%d unfoldableEvents=%d",
		at.Format(time.RFC3339), len(folded.Nodes), len(folded.Agents),
		folded.EventsFolded, folded.UnfoldableEvents)

	// The live route is the same body WITHOUT the marker, which is how a
	// client tells a reconstruction from a passthrough without inspecting the
	// URL it asked for. Decoded into a map on purpose: a typed struct cannot
	// tell an absent field from a false one.
	liveStatus, _, liveData := mustRequest(t, http.MethodGet, base+"/api/v1/topology", nil)
	if liveStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/topology 200, got %d: %s", liveStatus, liveData)
	}
	var live map[string]json.RawMessage
	decodeJSON(t, "live topology response", liveData, &live)
	if _, present := live["historical"]; present {
		t.Errorf("expected the live topology body to carry NO historical field, got %s", live["historical"])
	}

	for _, tc := range []struct {
		name string
		at   string
		want int
	}{
		{
			// Before any retained event: the rows that would have built that
			// node set are pruned (or were never written), and an empty 200
			// would claim the cluster was empty then.
			name: "pre-retention",
			at:   "2000-01-01T00:00:00Z",
			want: http.StatusUnprocessableEntity,
		},
		{
			// Not a timestamp at all: a malformed parameter, not a rejected
			// value -- 400, not 422.
			name: "garbage",
			at:   "not-a-timestamp",
			want: http.StatusBadRequest,
		},
		{
			name: "future",
			at:   time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			want: http.StatusBadRequest,
		},
	} {
		caseStatus, _, caseData := mustRequest(t, http.MethodGet,
			base+"/api/v1/topology?at="+url.QueryEscape(tc.at), nil)
		if caseStatus != tc.want {
			t.Errorf("expected GET /api/v1/topology?at=%s (%s) to be %d, got %d: %s",
				tc.at, tc.name, tc.want, caseStatus, caseData)
		}
	}
}

// incident is the subset of the Incident schema these tests assert on.
// ResolvedAt is a POINTER and is deliberately not tagged omitempty here: the
// whole resolve/reopen assertion below is "is this field null or not", and a
// value type could not tell an absent resolvedAt from the zero time.
type incident struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Scope      string          `json:"scope"`
	FromAt     time.Time       `json:"fromAt"`
	Status     string          `json:"status"`
	Notes      string          `json:"notes"`
	Pinned     json.RawMessage `json:"pinned"`
	CreatedBy  string          `json:"createdBy"`
	ResolvedAt *time.Time      `json:"resolvedAt"`
}

// pinnedRef is one entry of an incident's pinned list.
type pinnedRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Note string `json:"note"`
}

// listIncidents reads one page with the given query, built by the caller so a
// test can express ?status= (open|resolved|absent) without this helper
// growing a parameter per filter.
func listIncidents(t *testing.T, base string, query url.Values) []incident {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/incidents?"+query.Encode(), nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/incidents?%s 200, got %d: %s", query.Encode(), status, data)
	}
	var page struct {
		Incidents []incident `json:"incidents"`
	}
	decodeJSON(t, "incidents list", data, &page)
	return page.Incidents
}

// containsIncident reports whether a page carries the given id.
func containsIncident(page []incident, id string) bool {
	for i := range page {
		if page[i].ID == id {
			return true
		}
	}
	return false
}

// patchIncident PATCHes one incident and returns the row the SERVER answers
// with -- never an echo of the request: a patch that names one field still
// returns the whole row, and every assertion below is about what the other
// fields did while that one changed.
func patchIncident(t *testing.T, base, id, what string, body map[string]any) incident {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodPatch, base+"/api/v1/incidents/"+id, body)
	if status != http.StatusOK {
		t.Fatalf("expected PATCH /api/v1/incidents/%s (%s) 200, got %d: %s", id, what, status, data)
	}
	var patched incident
	decodeJSON(t, "patch-incident response ("+what+")", data, &patched)
	return patched
}

// decodePinned reads an incident's pinned column as the typed list. The column
// is json.RawMessage on the wire precisely because the console stores it
// opaquely, so this is where a test states the shape it expects.
func decodePinned(t *testing.T, raw json.RawMessage) []pinnedRef {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("expected pinned to be a JSON array, got an absent field")
	}
	var refs []pinnedRef
	decodeJSON(t, "incident pinned", raw, &refs)
	if refs == nil {
		t.Fatalf("expected pinned to be a JSON array (possibly empty), got null: %s", raw)
	}
	return refs
}

// TestConsoleIncidentLifecycle walks one saved investigation from open to
// resolved to reopened against the real console and PostgreSQL, and spends its
// assertions on the two things the route's shape does not give away.
//
// First, resolvedAt is STATUS' WITNESS rather than a field of its own: the
// store's invariant is that it is non-nil exactly when status is "resolved"
// (store/incidents.go validateIncidentStatus), so a resolve must fill it, a
// reopen must clear it, and a patch that touches neither must leave both
// alone. A server that let the two drift would still answer 200 to every
// request here.
//
// Second, pinned refs are validated for SHAPE, not for EXISTENCE. Verified in
// store.ValidatePinned before this test was written: it checks the kind
// against a closed vocabulary (event, audit, annotation, snapshot, run, k8s),
// a non-empty bounded id and a bounded note, and never looks the id up. So the
// ids below are deliberately synthetic -- pinning a real event id would make
// this test depend on the events pipeline having produced one, which is a
// different test's job (TestConsoleEvents), and would prove nothing extra.
func TestConsoleIncidentLifecycle(t *testing.T) {
	base := consoleBaseURL(t)

	title := uniqueName("e2e-incident")
	// A scope value, not a node that must exist: scope is a filter key matched
	// exactly and is never resolved against the topology.
	const scope = "e2e-node"
	// Whole seconds: fromAt round-trips through RFC3339 on the way in.
	from := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)

	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/incidents", map[string]any{
		"title":  title,
		"scope":  scope,
		"fromAt": from.Format(time.RFC3339),
		"notes":  "opened by " + title,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/incidents 201, got %d: %s", status, data)
	}

	var created incident
	decodeJSON(t, "create-incident response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty incident id in the create response")
	}
	t.Cleanup(func() { deleteResource(t, base+"/api/v1/incidents/"+created.ID) })

	if want := "/api/v1/incidents/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}
	// An incident is ALWAYS created open with no resolvedAt, whatever the body
	// asked for -- there is no status field on the create request at all.
	if created.Status != "open" {
		t.Errorf("expected a new incident to be open, got %q", created.Status)
	}
	if created.ResolvedAt != nil {
		t.Errorf("expected a new incident to have no resolvedAt, got %s", created.ResolvedAt)
	}
	if created.Scope != scope {
		t.Errorf("expected the stored scope to echo %q, got %q", scope, created.Scope)
	}
	if !created.FromAt.Equal(from) {
		t.Errorf("expected fromAt to round-trip as %s, got %s", from, created.FromAt)
	}
	// createdBy is the SERVER's view of the subject and was never in the body.
	if created.CreatedBy == "" {
		t.Errorf("expected createdBy to be filled in by the server, got %q", created.CreatedBy)
	}
	// An omitted pinned is stored as the EMPTY LIST, never null: the UI
	// iterates it without a nil check.
	if refs := decodePinned(t, created.Pinned); len(refs) != 0 {
		t.Errorf("expected a new incident to carry an empty pinned list, got %d entries: %s",
			len(refs), created.Pinned)
	}

	getStatus, _, getData := mustRequest(t, http.MethodGet, base+"/api/v1/incidents/"+created.ID, nil)
	if getStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/incidents/%s 200, got %d: %s", created.ID, getStatus, getData)
	}
	var fetched incident
	decodeJSON(t, "get-incident response", getData, &fetched)
	if fetched.ID != created.ID || fetched.Title != title || fetched.Status != "open" {
		t.Errorf("expected GET to return the row just created (id %s, title %q, open), got %+v",
			created.ID, title, fetched)
	}

	openQuery := url.Values{}
	openQuery.Set("status", "open")
	openQuery.Set("limit", "500")
	resolvedQuery := url.Values{}
	resolvedQuery.Set("status", "resolved")
	resolvedQuery.Set("limit", "500")

	if !containsIncident(listIncidents(t, base, openQuery), created.ID) {
		t.Errorf("expected a just-opened incident %s in ?status=open", created.ID)
	}

	resolved := patchIncident(t, base, created.ID, "resolve", map[string]any{"status": "resolved"})
	if resolved.Status != "resolved" {
		t.Errorf("expected status resolved after the resolve patch, got %q", resolved.Status)
	}
	if resolved.ResolvedAt == nil {
		t.Fatalf("expected resolvedAt to be set by the resolve patch, got null")
	}
	resolvedAt := *resolved.ResolvedAt

	if !containsIncident(listIncidents(t, base, resolvedQuery), created.ID) {
		t.Errorf("expected the resolved incident %s in ?status=resolved", created.ID)
	}
	if containsIncident(listIncidents(t, base, openQuery), created.ID) {
		t.Errorf("expected the resolved incident %s to be gone from ?status=open", created.ID)
	}

	// Notes-only: the status side of the row must be untouched. This is the
	// assertion that catches a patch handler which rebuilds the row from its
	// request instead of updating the named field.
	const notes = "notes-only patch, status must not move"
	noted := patchIncident(t, base, created.ID, "notes only", map[string]any{"notes": notes})
	if noted.Notes != notes {
		t.Errorf("expected the notes patch to store %q, got %q", notes, noted.Notes)
	}
	if noted.Status != "resolved" {
		t.Errorf("expected a notes-only patch to leave the status resolved, got %q", noted.Status)
	}
	if noted.ResolvedAt == nil || !noted.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("expected a notes-only patch to leave resolvedAt at %s, got %v", resolvedAt, noted.ResolvedAt)
	}

	reopened := patchIncident(t, base, created.ID, "reopen", map[string]any{"status": "open"})
	if reopened.Status != "open" {
		t.Errorf("expected status open after the reopen patch, got %q", reopened.Status)
	}
	if reopened.ResolvedAt != nil {
		t.Errorf("expected the reopen patch to clear resolvedAt, got %s", reopened.ResolvedAt)
	}
	if reopened.Notes != notes {
		t.Errorf("expected the reopen patch to leave the notes alone, got %q", reopened.Notes)
	}

	// Pinned is a WHOLESALE replacement, not a merge: two entries in, two
	// entries out, and the one-entry patch below replaces both rather than
	// removing one. Both kinds are in ValidatePinned's vocabulary and both ids
	// are synthetic -- see the doc comment.
	twoPins := []map[string]any{
		{"kind": "event", "id": "424242", "note": "the event this investigation started from"},
		{"kind": "run", "id": "00000000-0000-0000-0000-000000000001"},
	}
	withPins := patchIncident(t, base, created.ID, "pin two", map[string]any{"pinned": twoPins})
	pins := decodePinned(t, withPins.Pinned)
	if len(pins) != 2 {
		t.Fatalf("expected two pinned refs after the wholesale patch, got %d: %s", len(pins), withPins.Pinned)
	}
	if pins[0].Kind != "event" || pins[0].ID != "424242" || pins[0].Note == "" {
		t.Errorf("expected the first pin to round-trip as {event,424242,<note>}, got %+v", pins[0])
	}
	if pins[1].Kind != "run" || pins[1].ID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("expected the second pin to round-trip as the run ref that was sent, got %+v", pins[1])
	}
	if withPins.Status != "open" || withPins.Notes != notes {
		t.Errorf("expected a pinned-only patch to leave status and notes alone, got status %q notes %q",
			withPins.Status, withPins.Notes)
	}

	onePin := []map[string]any{{"kind": "k8s", "id": "77", "note": "replaces both, does not merge"}}
	replaced := patchIncident(t, base, created.ID, "pin one", map[string]any{"pinned": onePin})
	if pins = decodePinned(t, replaced.Pinned); len(pins) != 1 || pins[0].Kind != "k8s" {
		t.Errorf("expected the second pinned patch to REPLACE the list with its single k8s ref, got %s",
			replaced.Pinned)
	}

	// Delete, then delete again: an investigation that is not there is 404,
	// never a second success -- annotations' rule, and for the same reason.
	delStatus, _, delData := mustRequest(t, http.MethodDelete, base+"/api/v1/incidents/"+created.ID, nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/incidents/%s 204, got %d: %s", created.ID, delStatus, delData)
	}
	goneStatus, _, goneData := mustRequest(t, http.MethodDelete, base+"/api/v1/incidents/"+created.ID, nil)
	if goneStatus != http.StatusNotFound {
		t.Errorf("expected a second DELETE of incident %s to be 404, got %d: %s", created.ID, goneStatus, goneData)
	}
}

// maintenanceWindow is the subset of the MaintenanceWindow schema this test
// asserts on. Both ends are VALUES, not pointers: unlike an annotation, a
// window with no end is not a window at all (the table's own CHECK).
type maintenanceWindow struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	StartAt   time.Time `json:"startAt"`
	EndAt     time.Time `json:"endAt"`
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"createdBy"`
}

// listMaintenance reads one page bounded by from/to.
func listMaintenance(t *testing.T, base string, query url.Values) []maintenanceWindow {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/maintenance?"+query.Encode(), nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/maintenance?%s 200, got %d: %s", query.Encode(), status, data)
	}
	var page struct {
		Windows []maintenanceWindow `json:"windows"`
	}
	decodeJSON(t, "maintenance list", data, &page)
	return page.Windows
}

// containsWindow reports whether a page carries the given id.
func containsWindow(page []maintenanceWindow, id string) bool {
	for i := range page {
		if page[i].ID == id {
			return true
		}
	}
	return false
}

// TestConsoleMaintenanceWindows covers the declare/see/refuse/withdraw loop of
// a maintenance window, and puts most of its weight on the ONE thing that
// separates ?from/?to here from every other listing in this file: they bound a
// window the row must OVERLAP, not one that must CONTAIN it. A half-hour
// window that started five minutes ago overlaps a two-minute probe around
// "now" while containing neither of its bounds, so a server that had
// implemented containment would answer an empty page to exactly the question
// an operator asks most ("is anything in maintenance right now?").
//
// The scope is global (""), which is a real value here rather than a missing
// filter -- the same three-state rule TestConsoleAnnotations exercises for
// ?scope=, restated on the write side.
func TestConsoleMaintenanceWindows(t *testing.T) {
	base := consoleBaseURL(t)

	reason := uniqueName("e2e-maintenance")
	start := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	end := start.Add(35 * time.Minute)

	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/maintenance", map[string]any{
		"scope":   "",
		"startAt": start.Format(time.RFC3339),
		"endAt":   end.Format(time.RFC3339),
		"reason":  reason,
	})
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/maintenance 201, got %d: %s", status, data)
	}

	var created maintenanceWindow
	decodeJSON(t, "create-maintenance response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty maintenance window id in the create response")
	}
	t.Cleanup(func() { deleteResource(t, base+"/api/v1/maintenance/"+created.ID) })

	if want := "/api/v1/maintenance/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}
	if created.Scope != "" {
		t.Errorf("expected a global window to store the empty scope, got %q", created.Scope)
	}
	if !created.StartAt.Equal(start) || !created.EndAt.Equal(end) {
		t.Errorf("expected the window to round-trip as [%s, %s], got [%s, %s]",
			start, end, created.StartAt, created.EndAt)
	}
	if created.Reason != reason {
		t.Errorf("expected the stored reason to echo %q, got %q", reason, created.Reason)
	}
	if created.CreatedBy == "" {
		t.Errorf("expected createdBy to be filled in by the server, got %q", created.CreatedBy)
	}

	// A probe strictly INSIDE the window: it contains neither bound, so only
	// an overlap test can match it.
	now := time.Now().UTC()
	overlap := url.Values{}
	overlap.Set("from", now.Add(-time.Minute).Format(time.RFC3339))
	overlap.Set("to", now.Add(time.Minute).Format(time.RFC3339))
	overlap.Set("limit", "500")
	if !containsWindow(listMaintenance(t, base, overlap), created.ID) {
		t.Errorf("expected window %s (a half-hour window around now) to overlap a two-minute probe at now",
			created.ID)
	}

	// A window an hour past the end: no overlap, so the row must not appear.
	// Without this the assertion above would pass against a server that
	// ignored from/to entirely.
	disjoint := url.Values{}
	disjoint.Set("from", end.Add(time.Hour).Format(time.RFC3339))
	disjoint.Set("to", end.Add(2*time.Hour).Format(time.RFC3339))
	disjoint.Set("limit", "500")
	if containsWindow(listMaintenance(t, base, disjoint), created.ID) {
		t.Errorf("expected window %s to be outside a probe an hour after it ends", created.ID)
	}

	// end == start and end < start are both refusals, and both are 422 rather
	// than 400: the body is well-formed JSON whose VALUES break a rule.
	for _, tc := range []struct {
		name    string
		startAt time.Time
		endAt   time.Time
	}{
		{"end equal to start", start, start},
		{"end before start", start, start.Add(-time.Minute)},
	} {
		badStatus, _, badData := mustRequest(t, http.MethodPost, base+"/api/v1/maintenance", map[string]any{
			"startAt": tc.startAt.Format(time.RFC3339),
			"endAt":   tc.endAt.Format(time.RFC3339),
			"reason":  reason + "-invalid",
		})
		if badStatus != http.StatusUnprocessableEntity {
			t.Errorf("expected POST /api/v1/maintenance with %s to be 422, got %d: %s",
				tc.name, badStatus, badData)
		}
	}

	delStatus, _, delData := mustRequest(t, http.MethodDelete, base+"/api/v1/maintenance/"+created.ID, nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/maintenance/%s 204, got %d: %s", created.ID, delStatus, delData)
	}
	goneStatus, _, goneData := mustRequest(t, http.MethodDelete, base+"/api/v1/maintenance/"+created.ID, nil)
	if goneStatus != http.StatusNotFound {
		t.Errorf("expected a second DELETE of window %s to be 404, got %d: %s", created.ID, goneStatus, goneData)
	}
	if containsWindow(listMaintenance(t, base, overlap), created.ID) {
		t.Errorf("expected window %s to be gone from an overlapping listing after the delete", created.ID)
	}
}

// unroutableWebhookURL is where every endpoint this file declares points: an
// RFC 5737 TEST-NET-3 literal on the discard port. Same reasoning as
// deniedTargetAddr -- an IP literal so no resolver can influence the outcome,
// and reserved for documentation so it is unroutable from a CI runner
// regardless. There is no in-cluster receiver to point at, and inventing one
// would test a fixture; this tests the dispatcher.
const unroutableWebhookURL = "http://203.0.113.9:9/hook"

// e2eWebhookSecret is the plaintext HMAC signing secret the endpoints below
// are created with. It is also the needle for the "no secret is ever served
// back" scan, which is why it is a distinctive literal rather than a uuid.
const e2eWebhookSecret = "e2e-webhook-signing-secret-do-not-echo"

// webhookOutcomeBudget bounds the wait for a /test ping's outcome to land on
// the endpoint row. The ceiling is one attempt: the dispatcher's per-attempt
// timeout is 10s (webhooks/dispatcher.go attemptTimeout) plus a 5s store
// write, and /test runs a SINGLE-attempt ladder. 90s is that with room for a
// loaded runner, not a guess.
const webhookOutcomeBudget = 90 * time.Second

// webhookRow is one endpoint on the wire. There is no secret field because the
// server has none -- hasSecret is all a reader ever learns.
type webhookRow struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	Events      []string   `json:"events"`
	Enabled     bool       `json:"enabled"`
	HasSecret   bool       `json:"hasSecret"`
	LastStatus  string     `json:"lastStatus"`
	LastAttempt *time.Time `json:"lastAttempt"`
	Failures    int32      `json:"failures"`
}

// createWebhook POSTs one endpoint, asserts the created-and-point-at-it
// contract and registers its deletion. It returns the RAW body beside the
// decoded row: the secret assertions are a string scan over exactly the bytes
// a client receives, which no typed struct could make -- webhookRow has no
// field for a secret, which is the point and also why it could never notice
// one.
//
// A 503 here is the HARNESS, not the server: creating an endpoint is one of
// the two routes that need the cipher, so an unconfigured encryption key takes
// this whole test down with a message naming the value to set.
func createWebhook(t *testing.T, base string, body map[string]any) (webhookRow, []byte) {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/webhooks", body)
	if status == http.StatusServiceUnavailable {
		t.Fatalf("POST /api/v1/webhooks answered 503: this console has no webhook encryption key. "+
			"e2e/testdata/console-values.yaml must set console.webhooks.encryptionKeySecret and the "+
			"workflow must create that Secret before the install. Body: %s", data)
	}
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/webhooks 201, got %d: %s", status, data)
	}

	var created webhookRow
	decodeJSON(t, "create-webhook response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty webhook id in the create response")
	}
	if want := "/api/v1/webhooks/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}

	t.Cleanup(func() { deleteResource(t, base+"/api/v1/webhooks/"+created.ID) })
	return created, data
}

// assertNoSecretServed is the whole write-only-secret contract in one place:
// the body must not carry the plaintext that was sent, and must not carry a
// "secret" key at all -- a null or empty one would still be a field the UI
// could bind an input to.
func assertNoSecretServed(t *testing.T, what string, body []byte) {
	t.Helper()
	if strings.Contains(string(body), e2eWebhookSecret) {
		t.Errorf("%s served the plaintext webhook secret back: %s", what, body)
	}
	if strings.Contains(string(body), `"secret"`) {
		t.Errorf(`%s carries a "secret" key; the webhook secret is write-only: %s`, what, body)
	}
}

// webhookDeliveryCount reads one result-label sample off the console's own
// delivery counter, answering 0 for a label a Prometheus CounterVec has not
// created a child for yet: an absent series and a zero one mean the same thing
// here, and only the DELTA is ever asserted on.
func webhookDeliveryCount(t *testing.T, base, result string) float64 {
	t.Helper()
	status, _, data, err := request(t, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Logf("console metrics request failed (counted as 0): %v", err)
		return 0
	}
	if status != http.StatusOK {
		t.Logf("console metrics returned %d (counted as 0)", status)
		return 0
	}
	samples := metricSamples(string(data), "kconmon_ng_console_webhook_deliveries_total",
		map[string]string{"result": result})
	if len(samples) == 0 {
		return 0
	}
	return sampleValue(t, samples[0])
}

// TestConsoleWebhookDelivery proves the outbound dispatcher fires, signs,
// attempts and records HONESTLY -- with no receiver anywhere in the cluster.
// That constraint is the whole design of this test, and the split below is
// forced by what internal/console/webhooks/dispatcher.go actually does:
//
//   - deliver() records EXACTLY ONE outcome per delivery, at the END of the
//     ladder, never per attempt. A failing first attempt writes nothing.
//   - Notify (the incident lifecycle path) runs retryLadder = {0, 30s, 5m}
//     with +/-20% jitter, so its row lands four to six and a half MINUTES
//     after the incident. Waiting that out in e2e is not an option.
//   - DispatchTest (POST /{id}/test) runs singleAttempt = {0}: one POST, then
//     record. Against an unroutable address that is one attemptTimeout (10s)
//     and the row is there.
//
// So the OUTCOME assertions ride /test, which is the only path whose terminal
// state is observable inside a test budget. The LIFECYCLE assertion cannot be
// the same row, and it is not faked either: creating an incident is asserted
// to (a) return its 201 well inside one attempt timeout, which is the
// non-blocking contract -- a dispatcher that delivered inline would stall the
// 201 for 10s against this dead endpoint -- and (b) advance
// webhook_deliveries_total{result="filtered"}, which Notify increments
// SYNCHRONOUSLY for an endpoint that does not subscribe to the event. That
// counter moving is direct proof the incident create reached the dispatcher
// and ran its event filter, with no HTTP and no ladder in the way.
//
// The endpoint that DOES subscribe to incident.created is declared anyway, so
// a real ladder is enqueued against a real unreachable address. Its terminal
// row is deliberately not waited for; the observed lastStatus is logged.
func TestConsoleWebhookDelivery(t *testing.T) {
	base := consoleBaseURL(t)

	// probeHook subscribes to incident.resolved ONLY. Two things follow, both
	// wanted: /test still pings it (the ping ignores the event filter by
	// design, so an operator can test an endpoint they just narrowed), and the
	// incident CREATE below cannot enqueue a five-and-a-half-minute ladder
	// against it that would overwrite the row mid-assertion.
	probeHook, probeBody := createWebhook(t, base, map[string]any{
		"name":   uniqueName("e2e-hook-probe"),
		"url":    unroutableWebhookURL,
		"events": []string{"incident.resolved"},
		"secret": e2eWebhookSecret,
	})
	assertNoSecretServed(t, "the create-webhook response", probeBody)
	if !probeHook.HasSecret {
		t.Errorf("expected hasSecret=true on an endpoint created with a secret, got false")
	}
	if !probeHook.Enabled {
		t.Errorf("expected an omitted enabled to default to true, got false")
	}
	if probeHook.LastStatus != "" || probeHook.LastAttempt != nil || probeHook.Failures != 0 {
		t.Errorf("expected a brand-new endpoint to carry no delivery history, got lastStatus=%q lastAttempt=%v failures=%d",
			probeHook.LastStatus, probeHook.LastAttempt, probeHook.Failures)
	}

	// liveHook is the lifecycle subscriber: incident.created, so the incident
	// below enqueues a REAL ladder against a REAL unreachable address.
	liveHook, liveBody := createWebhook(t, base, map[string]any{
		"name":   uniqueName("e2e-hook-live"),
		"url":    unroutableWebhookURL,
		"events": []string{"incident.created"},
		"secret": e2eWebhookSecret,
	})
	assertNoSecretServed(t, "the create-webhook response", liveBody)

	// --- The outcome, via the single-attempt /test ladder. ---
	testStatus, _, testData := mustRequest(t, http.MethodPost,
		base+"/api/v1/webhooks/"+probeHook.ID+"/test", nil)
	if testStatus != http.StatusAccepted {
		t.Fatalf("expected POST /api/v1/webhooks/%s/test 202 (enqueued, not delivered), got %d: %s",
			probeHook.ID, testStatus, testData)
	}

	var outcome webhookRow
	pollUntil(t, webhookOutcomeBudget, 2*time.Second,
		"the /test ping's failed outcome to land on the endpoint row", func() bool {
			pollStatus, _, pollData, err := request(t, http.MethodGet,
				base+"/api/v1/webhooks/"+probeHook.ID, nil)
			if err != nil {
				t.Logf("webhook request failed (will retry): %v", err)
				return false
			}
			if pollStatus != http.StatusOK {
				t.Logf("webhook request returned %d (will retry)", pollStatus)
				return false
			}
			var row webhookRow
			if decodeErr := json.Unmarshal(pollData, &row); decodeErr != nil {
				t.Logf("decode webhook response failed (will retry): %v", decodeErr)
				return false
			}
			outcome = row
			return strings.HasPrefix(row.LastStatus, "failed: ") && row.Failures >= 1
		})

	t.Logf("/test against %s recorded lastStatus=%q failures=%d",
		unroutableWebhookURL, outcome.LastStatus, outcome.Failures)
	if outcome.LastAttempt == nil {
		t.Errorf("expected lastAttempt to be stamped alongside the failed outcome, got null")
	}
	// last_status is a CLASS, never an echo of what the far end said or of
	// where it was: a column the UI renders must not be an input an endpoint
	// the console does not control can write to.
	if strings.Contains(outcome.LastStatus, "203.0.113.9") {
		t.Errorf("expected lastStatus to be a failure CLASS carrying no destination, got %q", outcome.LastStatus)
	}
	if outcome.Failures != 1 {
		t.Errorf("expected exactly one consecutive failure after one single-attempt ping, got %d", outcome.Failures)
	}

	// --- The lifecycle path. ---
	filteredBefore := webhookDeliveryCount(t, base, "filtered")

	incidentBody := map[string]any{
		"title":  uniqueName("e2e-webhook-incident"),
		"fromAt": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
	}
	began := time.Now()
	incStatus, _, incData := mustRequest(t, http.MethodPost, base+"/api/v1/incidents", incidentBody)
	elapsed := time.Since(began)
	if incStatus != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/incidents 201, got %d: %s", incStatus, incData)
	}
	var fired incident
	decodeJSON(t, "create-incident response", incData, &fired)
	t.Cleanup(func() { deleteResource(t, base+"/api/v1/incidents/"+fired.ID) })

	// The non-blocking contract, measured against the dispatcher's own
	// per-attempt timeout: an endpoint that never answers must cost the
	// caller nothing. Half of attemptTimeout is a generous ceiling for one
	// unpaged SELECT plus an insert.
	if elapsed >= 5*time.Second {
		t.Errorf("expected POST /api/v1/incidents to return without waiting on a dead endpoint, took %s "+
			"(the dispatcher's per-attempt timeout is 10s -- this looks like an inline delivery)", elapsed)
	}

	pollUntil(t, 30*time.Second, time.Second,
		"webhook_deliveries_total{result=\"filtered\"} to advance (incident.created reaching the dispatcher's filter)",
		func() bool {
			return webhookDeliveryCount(t, base, "filtered") >= filteredBefore+1
		})

	// The ladder against liveHook is running RIGHT NOW and will not record for
	// another four to six minutes (see the doc comment). Reported, never
	// asserted: waiting it out would add five and a half minutes to every e2e
	// run to learn what /test already proved above.
	live, liveRowBody := getWebhookRow(t, base, liveHook.ID)
	t.Logf("incident.created subscriber %s mid-ladder: lastStatus=%q failures=%d "+
		"(the {0s,30s,5m} ladder records ONE terminal outcome, so this is expected to be empty here)",
		live.ID, live.LastStatus, live.Failures)

	// --- The secret, on every read path. ---
	assertNoSecretServed(t, "GET /api/v1/webhooks/{id}", liveRowBody)
	if !live.HasSecret {
		t.Errorf("expected hasSecret to stay true on a read-back endpoint, got false")
	}

	listStatus, _, listData := mustRequest(t, http.MethodGet, base+"/api/v1/webhooks", nil)
	if listStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/webhooks 200, got %d: %s", listStatus, listData)
	}
	assertNoSecretServed(t, "GET /api/v1/webhooks", listData)
	var page struct {
		Webhooks []webhookRow `json:"webhooks"`
	}
	decodeJSON(t, "webhooks list", listData, &page)
	var seen int
	for i := range page.Webhooks {
		row := &page.Webhooks[i]
		if row.ID != probeHook.ID && row.ID != liveHook.ID {
			continue
		}
		seen++
		if !row.HasSecret {
			t.Errorf("expected hasSecret=true for endpoint %s in the listing, got false", row.ID)
		}
		if row.URL != unroutableWebhookURL {
			t.Errorf("expected endpoint %s to carry the url it was created with, got %q", row.ID, row.URL)
		}
	}
	if seen != 2 {
		t.Errorf("expected both e2e endpoints in GET /api/v1/webhooks, found %d", seen)
	}
}

// getWebhookRow reads one endpoint, returning the decoded row AND the raw body
// the secret scan needs.
func getWebhookRow(t *testing.T, base, id string) (webhookRow, []byte) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/webhooks/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/webhooks/%s 200, got %d: %s", id, status, data)
	}
	var row webhookRow
	decodeJSON(t, "webhook response", data, &row)
	return row, data
}

// k8sEventRow is one captured cluster event on the wire.
type k8sEventRow struct {
	ID        string    `json:"id"`
	EventTime time.Time `json:"eventTime"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Reason    string    `json:"reason"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
}

// k8sCaptureBudget is how long the reader gets to put a first row in the
// database. Generous because it covers a cold start (the console Pod's watch
// opening, its initial list, and the batched insert behind it), not because
// the events themselves are in doubt.
const k8sCaptureBudget = 120 * time.Second

// TestConsoleK8sEventsCapture is the live test of the whole M6 Kubernetes
// context path: console.kubernetesContext.enabled renders a console-only
// ServiceAccount, ClusterRole and ClusterRoleBinding (charts' rbac.yaml), the
// console watches the apiserver with that identity, and the rows land in
// PostgreSQL where GET /api/v1/k8s-events serves them. A grant that is too
// narrow shows up here -- and ONLY here -- as an endpoint that answers 200
// with an empty page forever, which is why this is a hard assertion rather
// than a skip.
//
// It is a hard assertion for a second reason too: a helm install churns Pods.
// The release namespace holds the controller Deployment, the agent DaemonSet
// (which the workflow deliberately RESTARTS mid-run), the console itself, the
// bundled Valkey and the Postgres fixture, so Scheduled/Pulled/Created/Started
// events for that namespace are not a maybe. The apiserver keeps events for an
// hour by default and the reader lists on start, so even the ones emitted
// before the console came up are in scope.
//
// The risk worth naming: this was written against the code and the chart, not
// against a live kind cluster (no kind locally -- the M4/M5 precedent). If it
// ever fails on an empty page, the two candidates are the RBAC grant and the
// namespace the reader was pointed at, in that order.
func TestConsoleK8sEventsCapture(t *testing.T) {
	base := consoleBaseURL(t)

	var page struct {
		Events []k8sEventRow `json:"events"`
	}
	pollUntil(t, k8sCaptureBudget, 3*time.Second,
		"at least one captured Kubernetes event (console.kubernetesContext.enabled + the console's own RBAC)",
		func() bool {
			status, _, data, err := request(t, http.MethodGet, base+"/api/v1/k8s-events?limit=50", nil)
			if err != nil {
				t.Logf("k8s-events request failed (will retry): %v", err)
				return false
			}
			if status != http.StatusOK {
				t.Logf("k8s-events request returned %d (will retry): %s", status, data)
				return false
			}
			if decodeErr := json.Unmarshal(data, &page); decodeErr != nil {
				t.Logf("decode k8s-events response failed (will retry): %v", decodeErr)
				return false
			}
			return len(page.Events) > 0
		})

	kinds := map[string]int{}
	for i := range page.Events {
		row := &page.Events[i]
		kinds[row.Kind]++
		// The kind vocabulary is CLOSED on the write side (store/k8sevents.go),
		// so anything else here means a row was stored that the read filters
		// can never match.
		if row.Kind != "Pod" && row.Kind != "Node" {
			t.Errorf("expected every captured event to be Pod or Node, got %q on %s", row.Kind, row.ID)
		}
		if row.Name == "" {
			t.Errorf("expected a non-empty object name on captured event %s (%s)", row.ID, row.Kind)
		}
		if row.Reason == "" {
			t.Errorf("expected a non-empty reason on captured event %s (%s/%s)", row.ID, row.Kind, row.Name)
		}
		if row.Type != "Normal" && row.Type != "Warning" {
			t.Errorf("expected type Normal or Warning on captured event %s, got %q", row.ID, row.Type)
		}
		if row.EventTime.IsZero() {
			t.Errorf("expected an event time on captured event %s, got the zero time", row.ID)
		}
		// Node events are cluster-scoped and carry no namespace; a Pod event
		// without one would mean the namespace column was dropped on the way
		// in, which is exactly what the capture is scoped BY.
		if row.Kind == "Pod" && row.Namespace == "" {
			t.Errorf("expected a namespace on captured Pod event %s (%s)", row.ID, row.Name)
		}
	}
	t.Logf("captured %d Kubernetes events in the newest page, by kind: %v", len(page.Events), kinds)

	// ?kind=Pod is asserted separately: it exercises the filter AND states the
	// stronger claim, that POD events -- the ones scoped to the release
	// namespace, and therefore the ones the RBAC and the namespace wiring both
	// have to be right for -- were captured, not just cluster-scoped Node ones.
	podStatus, _, podData := mustRequest(t, http.MethodGet, base+"/api/v1/k8s-events?kind=Pod&limit=50", nil)
	if podStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/k8s-events?kind=Pod 200, got %d: %s", podStatus, podData)
	}
	var pods struct {
		Events []k8sEventRow `json:"events"`
	}
	decodeJSON(t, "k8s-events ?kind=Pod list", podData, &pods)
	if len(pods.Events) == 0 {
		t.Errorf("expected at least one captured Pod event: a helm install churns Pods in the release "+
			"namespace throughout this run. Captured kinds on the unfiltered page: %v", kinds)
	}
	for i := range pods.Events {
		if pods.Events[i].Kind != "Pod" {
			t.Errorf("expected ?kind=Pod to return Pod events only, got %q on %s",
				pods.Events[i].Kind, pods.Events[i].ID)
		}
	}
}

// ---------------------------------------------------------------------------
// M7: alert rules, PrometheusRule sync, export/import, the WebSocket gate
// ---------------------------------------------------------------------------

// alertRuleRow is one alert rule on the wire. The three JSONB columns are
// RawMessage rather than typed maps for the reason the API serves them as
// objects: the tests below assert on their SHAPE (never null, `{}` when
// cleared), which a decoded map would silently normalise away.
type alertRuleRow struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Params       json.RawMessage `json:"params"`
	Severity     string          `json:"severity"`
	ForNs        int64           `json:"forNs"`
	Labels       json.RawMessage `json:"labels"`
	Annotations  json.RawMessage `json:"annotations"`
	Enabled      bool            `json:"enabled"`
	RenderedExpr string          `json:"renderedExpr"`
	SyncStatus   string          `json:"syncStatus"`
	SyncMessage  string          `json:"syncMessage"`
	LastSyncedAt *time.Time      `json:"lastSyncedAt"`
}

// foreignRuleRow is one PrometheusRule in the namespace that the console does
// NOT own, as GET /api/v1/alert-rules/foreign projects it. There is
// deliberately no raw object on the wire (httpapi.foreignRuleResponse's doc
// comment), so these four fields are everything a client -- or this test --
// can learn about somebody else's rule.
type foreignRuleRow struct {
	Name      string `json:"name"`
	Groups    int    `json:"groups"`
	Rules     int    `json:"rules"`
	ManagedBy string `json:"managedBy"`
}

// e2eRawExpr is the expression every rule these tests declare renders to.
// kind "raw" is verbatim (alerting.renderRaw: "there is no Prometheus parser
// in this module, so the console must not pretend to normalise an expression
// it cannot read"), which is what lets renderedExpr be asserted for EQUALITY
// below rather than merely for non-emptiness.
//
// `vector(1) > 0` is chosen because it is well-formed PromQL that depends on
// no scrape target, no series and no Prometheus at all -- and nothing in this
// cluster ever evaluates it: the Prometheus Operator is not installed (M7
// Decision 14), only its CRD is.
const e2eRawExpr = "vector(1) > 0"

// consoleBundleName is the ONE PrometheusRule object the console owns. It is
// the chart default (console.alerting.bundleName), left unset in
// e2e/testdata/console-values.yaml on purpose, and it is asserted by NAME
// below -- as the object that must NOT appear in the foreign list, because it
// is ours.
const consoleBundleName = "kconmon-ng-console-rules"

// foreignFixtureName, foreignFixtureAlert and foreignFixtureRecord name the
// harness fixture .github/workflows/e2e.yaml applies from
// e2e/testdata/foreign-prometheusrule.yaml. Changing either file means
// changing these.
const (
	foreignFixtureName      = "e2e-foreign-rules"
	foreignFixtureManagedBy = "e2e-fixture"
	foreignFixtureAlert     = "E2eForeignTargetDown"
	foreignFixtureRecord    = "e2e_foreign_recording"
)

// alertSyncBudget bounds the wait for a reconcile to land its outcome on a
// rule. The KICK is what this budget is written against -- every CRUD write
// and every POST /{id}/sync nudges the reconciler immediately
// (httpapi.kickSync), so the normal path here is seconds. The jittered 60s
// loop (console.alerting.syncInterval) is the backstop for a kick that
// coalesced away, and 90s clears one full jittered interval plus an apply.
const alertSyncBudget = 90 * time.Second

// alertRuleBody builds a create/replace body. Both writes are FULL REPLACES
// -- there is no PATCH on this resource by design (httpapi.alertRuleRequest:
// "an alert rule is a definition one person edits in a form") -- so this
// helper takes every field rather than merging, which is what makes the
// omitted-field-clears-it assertion below meaningful.
func alertRuleBody(name, severity string, forNs int64, enabled bool) map[string]any {
	return map[string]any{
		"name":     name,
		"kind":     "raw",
		"params":   map[string]any{"expr": e2eRawExpr},
		"severity": severity,
		"forNs":    forNs,
		"enabled":  enabled,
	}
}

// createAlertRule POSTs one rule, asserts the created-and-point-at-it
// contract and registers its deletion.
//
// A 503 here is the HARNESS, not the server -- createWebhook's posture: alert
// rules are persisted configuration with no in-memory fallback, so an
// unconfigured database takes the whole alerting surface down with a message
// naming the value to set rather than with fifteen confusing assertion
// failures.
func createAlertRule(t *testing.T, base string, body map[string]any) alertRuleRow {
	t.Helper()
	status, header, data := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules", body)
	if status == http.StatusServiceUnavailable {
		t.Fatalf("POST /api/v1/alert-rules answered 503: this console has no database, so there is nowhere "+
			"for an alert rule to live. e2e/testdata/console-values.yaml must set console.database.mode. Body: %s", data)
	}
	if status != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/alert-rules 201, got %d: %s", status, data)
	}

	var created alertRuleRow
	decodeJSON(t, "create-alert-rule response", data, &created)
	if created.ID == "" {
		t.Fatalf("expected a non-empty alert rule id in the create response")
	}
	if want := "/api/v1/alert-rules/" + created.ID; header.Get("Location") != want {
		t.Errorf("expected Location %q on the 201 response, got %q", want, header.Get("Location"))
	}

	t.Cleanup(func() { deleteResource(t, base+"/api/v1/alert-rules/"+created.ID) })
	return created
}

// getAlertRule reads one rule, failing the test on anything but a 200.
func getAlertRule(t *testing.T, base, id string) alertRuleRow {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/alert-rules/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/alert-rules/%s 200, got %d: %s", id, status, data)
	}
	var row alertRuleRow
	decodeJSON(t, "get-alert-rule response", data, &row)
	return row
}

// pollAlertRule reads one rule from inside a poll body: a transport error or a
// non-200 is "not yet", never a failure, so a console restarting mid-budget
// does not end the test on the wrong assertion.
func pollAlertRule(t *testing.T, base, id string) (alertRuleRow, bool) {
	t.Helper()
	status, _, data, err := request(t, http.MethodGet, base+"/api/v1/alert-rules/"+id, nil)
	if err != nil {
		t.Logf("alert rule request failed (will retry): %v", err)
		return alertRuleRow{}, false
	}
	if status != http.StatusOK {
		t.Logf("alert rule request returned %d (will retry): %s", status, data)
		return alertRuleRow{}, false
	}
	var row alertRuleRow
	if decodeErr := json.Unmarshal(data, &row); decodeErr != nil {
		t.Logf("decode alert rule response failed (will retry): %v", decodeErr)
		return alertRuleRow{}, false
	}
	return row, true
}

// listAlertRules reads the whole (unpaged, by design) rule list.
func listAlertRules(t *testing.T, base string) []alertRuleRow {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/alert-rules", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/alert-rules 200, got %d: %s", status, data)
	}
	var page struct {
		Rules []alertRuleRow `json:"rules"`
	}
	decodeJSON(t, "alert rules list", data, &page)
	return page.Rules
}

// findAlertRuleByName looks one rule up the way an operator does. The match is
// CASE-INSENSITIVE because the store's uniqueness is (migration 00007's
// lower(name) index) -- a lookup that missed "EdgeLoss" when asked for
// "edgeloss" would report "not created" for a row that is very much there.
func findAlertRuleByName(t *testing.T, base, name string) (alertRuleRow, bool) {
	t.Helper()
	rules := listAlertRules(t, base)
	for i := range rules {
		if strings.EqualFold(rules[i].Name, name) {
			return rules[i], true
		}
	}
	return alertRuleRow{}, false
}

// dropAlertRuleByName deletes a rule if it is there, and says nothing if it is
// not. It exists for RE-RUNNABILITY and nothing else: adoption refuses a name
// already taken, so a run that died between an import and its cleanup would
// otherwise turn every later run's "created" assertion into a "skipped: name
// already taken" -- a harness scar reported as a product failure.
func dropAlertRuleByName(t *testing.T, base, name string) {
	t.Helper()
	if row, found := findAlertRuleByName(t, base, name); found {
		t.Logf("removing a leftover alert rule %q (%s) from an earlier run", row.Name, row.ID)
		deleteResource(t, base+"/api/v1/alert-rules/"+row.ID)
	}
}

// listForeignRules reads the PrometheusRules in the console's namespace that
// it does not own, returning the STATUS beside the list: this route answers
// 409 (not 503) on a console with alerting off, and that distinction is itself
// an assertion in TestConsoleDegradedMode.
func listForeignRules(t *testing.T, base string) (int, []foreignRuleRow, []byte) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/alert-rules/foreign", nil)
	if status != http.StatusOK {
		return status, nil, data
	}
	var page struct {
		Foreign []foreignRuleRow `json:"foreign"`
	}
	decodeJSON(t, "foreign rules list", data, &page)
	return status, page.Foreign, data
}

// mustListForeignRules is listForeignRules for a caller that cannot continue
// without the cluster read. A 409 is called out as the harness problem it is:
// the flag is set in e2e/testdata/console-values.yaml, and the console drops
// the reconciler when it cannot build one.
func mustListForeignRules(t *testing.T, base string) []foreignRuleRow {
	t.Helper()
	status, foreign, data := listForeignRules(t, base)
	switch status {
	case http.StatusOK:
		return foreign
	case http.StatusConflict:
		t.Fatalf("GET /api/v1/alert-rules/foreign answered 409: this console is not running the PrometheusRule "+
			"reconciler. e2e/testdata/console-values.yaml sets console.alerting.enabled=true, so the console "+
			"either has no database or could not reach the apiserver -- check its logs. Body: %s", data)
	default:
		t.Fatalf("expected GET /api/v1/alert-rules/foreign 200, got %d: %s", status, data)
	}
	return nil
}

// findForeignRule returns the named object out of a foreign listing.
func findForeignRule(foreign []foreignRuleRow, name string) (foreignRuleRow, bool) {
	for i := range foreign {
		if foreign[i].Name == name {
			return foreign[i], true
		}
	}
	return foreignRuleRow{}, false
}

// TestConsoleAlertRulesCRUD walks one rule through the whole surface against
// the real console and PostgreSQL, and spends its assertions on the three
// things the route shape does not give away.
//
// First, renderedExpr is the SERVER's, computed at write time and never the
// caller's. httpapi.alertRuleInputFrom renders BEFORE it stores, precisely so
// a row can never hold an expression rendered from a different version of its
// own params; kind "raw" makes that observable, because its render is verbatim
// and the expected value is therefore knowable here.
//
// Second, syncStatus is RESET BY THE WRITE. store's update query flips the row
// back to "unsynced" on every change ("an edited rule is by definition not the
// rule that was applied"), and the assertion has to be made on the PUT's own
// RESPONSE rather than on a later GET -- the create/update kick means a
// reconcile may well have moved it to synced by the time a second request
// lands, which is correct behaviour and would make a GET-based assertion a
// race.
//
// Third, a duplicate name is 422 and not 409. On this ONE resource those two
// codes mean different things (httpapi.alertingDisabledDetail), so the
// case-flipped create below is checking the code as much as the constraint.
func TestConsoleAlertRulesCRUD(t *testing.T) {
	base := consoleBaseURL(t)

	name := uniqueName("e2e-alertrule")
	const forNs = int64(60 * time.Second)
	created := createAlertRule(t, base, alertRuleBody(name, "warning", forNs, true))

	if created.Name != name {
		t.Errorf("expected the stored name to echo %q, got %q", name, created.Name)
	}
	if created.Kind != "raw" {
		t.Errorf("expected kind raw, got %q", created.Kind)
	}
	if created.Severity != "warning" {
		t.Errorf("expected severity warning, got %q", created.Severity)
	}
	if created.ForNs != forNs {
		t.Errorf("expected forNs %d to round-trip, got %d", forNs, created.ForNs)
	}
	if !created.Enabled {
		t.Errorf("expected the rule to be created enabled, got false")
	}
	if created.RenderedExpr != e2eRawExpr {
		t.Errorf("expected kind raw to render its params.expr VERBATIM as %q, got %q", e2eRawExpr, created.RenderedExpr)
	}
	// A brand-new rule has never been applied to anything, whatever the kick
	// is doing in the background: the row is written unsynced and the create
	// response is built from that row.
	if created.SyncStatus != "unsynced" {
		t.Errorf("expected a new alert rule to be unsynced, got %q (message %q)",
			created.SyncStatus, created.SyncMessage)
	}
	// The three JSONB columns are {} and never null on the wire: the UI
	// indexes all three by key (httpapi.orEmptyJSONObject).
	for _, tc := range []struct {
		field string
		raw   json.RawMessage
	}{
		{"params", created.Params},
		{"labels", created.Labels},
		{"annotations", created.Annotations},
	} {
		if len(tc.raw) == 0 || string(tc.raw) == "null" {
			t.Errorf("expected %s to be a JSON object on the wire, got %q", tc.field, tc.raw)
		}
	}

	fetched := getAlertRule(t, base, created.ID)
	if fetched.ID != created.ID || fetched.Name != name || fetched.RenderedExpr != e2eRawExpr {
		t.Errorf("expected GET to return the row just created (id %s, name %q), got %+v", created.ID, name, fetched)
	}

	found := false
	for _, row := range listAlertRules(t, base) {
		if row.ID == created.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected alert rule %s in GET /api/v1/alert-rules", created.ID)
	}

	// A FULL replace: severity and forNs change, and the labels/annotations
	// that were never set stay empty rather than becoming null.
	const replacedForNs = int64(5 * time.Minute)
	putStatus, _, putData := mustRequest(t, http.MethodPut, base+"/api/v1/alert-rules/"+created.ID,
		alertRuleBody(name, "critical", replacedForNs, true))
	if putStatus != http.StatusOK {
		t.Fatalf("expected PUT /api/v1/alert-rules/%s 200, got %d: %s", created.ID, putStatus, putData)
	}
	var replaced alertRuleRow
	decodeJSON(t, "replace-alert-rule response", putData, &replaced)
	if replaced.Severity != "critical" {
		t.Errorf("expected severity critical after the replace, got %q", replaced.Severity)
	}
	if replaced.ForNs != replacedForNs {
		t.Errorf("expected forNs %d after the replace, got %d", replacedForNs, replaced.ForNs)
	}
	if replaced.SyncStatus != "unsynced" {
		t.Errorf("expected the replace to reset syncStatus to unsynced, got %q (message %q)",
			replaced.SyncStatus, replaced.SyncMessage)
	}
	if replaced.RenderedExpr != e2eRawExpr {
		t.Errorf("expected the replace to re-render the expression as %q, got %q", e2eRawExpr, replaced.RenderedExpr)
	}

	// Case-flipped: a DIFFERENT string, the SAME row as far as the store's
	// lower(name) index is concerned.
	dupStatus, _, dupData := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules",
		alertRuleBody(strings.ToUpper(name), "info", 0, true))
	if dupStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected a case-flipped duplicate name to be 422 (a rejected field value, not a 409 -- "+
			"409 on this resource means alerting is disabled), got %d: %s", dupStatus, dupData)
		if dupStatus == http.StatusCreated {
			var dup alertRuleRow
			decodeJSON(t, "duplicate create response", dupData, &dup)
			t.Cleanup(func() { deleteResource(t, base+"/api/v1/alert-rules/"+dup.ID) })
		}
	}

	// Delete, then delete again: a rule that is not there is 404, never a
	// second success -- the incidents rule, and for the same reason.
	delStatus, _, delData := mustRequest(t, http.MethodDelete, base+"/api/v1/alert-rules/"+created.ID, nil)
	if delStatus != http.StatusNoContent {
		t.Fatalf("expected DELETE /api/v1/alert-rules/%s 204, got %d: %s", created.ID, delStatus, delData)
	}
	goneStatus, _, goneData := mustRequest(t, http.MethodDelete, base+"/api/v1/alert-rules/"+created.ID, nil)
	if goneStatus != http.StatusNotFound {
		t.Errorf("expected a second DELETE of alert rule %s to be 404, got %d: %s", created.ID, goneStatus, goneData)
	}
}

// awaitAlertRuleSynced drives one rule to syncStatus=synced, RE-KICKING the
// reconciler on every iteration that finds it anywhere else.
//
// The re-kick is not impatience, it is the model. promrules.Reconcile compares
// the live object against the desired one BEFORE applying it, so the first
// pass after any rule change legitimately reports `drift` (the live bundle is
// the previous one) even though that same pass then applies the correction --
// the package doc says so in as many words: "drift is past tense here, the
// correction already happened". `synced` is therefore the state of the SECOND
// pass, and the honest way to wait for a converged bundle is to ask for
// another pass rather than to accept drift as success.
func awaitAlertRuleSynced(t *testing.T, base, id, what string) alertRuleRow {
	t.Helper()
	var row alertRuleRow
	pollUntil(t, alertSyncBudget, 3*time.Second, what, func() bool {
		got, ok := pollAlertRule(t, base, id)
		if !ok {
			return false
		}
		row = got
		if row.SyncStatus == "synced" {
			return true
		}
		if row.SyncStatus == "error" {
			// Reported every iteration on purpose: the cause classes
			// (crd-missing, forbidden, other) are the first token of the
			// message, and a job that times out here should say WHICH of the
			// two fixable causes it hit in its own log rather than only in a
			// final one-line failure.
			t.Logf("alert rule %s is in sync error: %s", id, row.SyncMessage)
		}
		mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/"+id+"/sync", nil)
		return false
	})
	return row
}

// TestConsoleAlertRuleSyncAgainstRealCRD is the live test of the whole M7
// alerting path: console.alerting.enabled renders a console-only Role and
// RoleBinding for prometheusrules (charts' rbac.yaml -- M7 Decision 3, a Role
// and NOT a ClusterRole), the console server-side-applies ONE PrometheusRule
// object into its own namespace with that identity, and the outcome comes back
// on each rule's syncStatus. The workflow applies the CRD before the install
// (e2e/testdata/prometheusrule-crd.yaml); the Prometheus Operator itself is
// deliberately absent, because SSA needs the resource to be served, not the
// controller behind it.
//
// WHY THE ASSERTIONS ARE API READS AND NOT `kubectl get prometheusrule`.
// Nothing in this package shells out -- every kubectl call in the harness
// lives in the workflow, and the K8s-events e2e (M6) set the precedent of
// asserting a cluster fact through the console route that reads it. Keeping
// that line has a concrete payoff here: the suite stays runnable against any
// console reachable at KCONMON_CONSOLE_URL, including a port-forward from a
// laptop with no cluster credentials at all, instead of silently degrading to
// "kubectl not found" on the one assertion that matters most. What is given up
// is a direct read of the object's own bytes; what replaces it is two API
// facts that cannot both be true unless the object is there and is ours:
//
//   - syncStatus reaching `synced` (or `drift`, its one-pass-behind sibling)
//     happens ONLY after promrules.Client.Apply returned without error. A
//     missing CRD, a missing Role or a wrong namespace each produce
//     syncStatus=error with a named cause instead.
//   - the bundle is ABSENT from GET /api/v1/alert-rules/foreign, which is a
//     LIVE list of the namespace's PrometheusRules filtered on exactly one
//     thing -- the app.kubernetes.io/managed-by label. An object that existed
//     without our label would be in that list under its own name; an object
//     with our label is excluded, which is the fact being asserted. The
//     fixture object IS in the same list on the same read, so an empty answer
//     cannot be mistaken for a passing one.
func TestConsoleAlertRuleSyncAgainstRealCRD(t *testing.T) {
	base := consoleBaseURL(t)

	// TWO rules, and the second one is not padding. A disabled rule is dropped
	// from the bundle entirely (alerting.RenderBundle) and the reconciler only
	// ever walks ENABLED rows, so nothing is written onto a rule after it is
	// switched off -- its own status can therefore say nothing about whether
	// the cluster caught up. The keeper is what makes that observable: its
	// lastSyncedAt advancing is proof that an apply happened AFTER the
	// disable, and the bundle that apply carried was rendered without the
	// disabled rule by construction.
	keeperName := uniqueName("e2e-alertsync-keeper")
	keeper := createAlertRule(t, base, alertRuleBody(keeperName, "info", 0, true))
	subjectName := uniqueName("e2e-alertsync")
	subject := createAlertRule(t, base, alertRuleBody(subjectName, "warning", int64(time.Minute), true))

	syncStatus, _, syncData := mustRequest(t, http.MethodPost,
		base+"/api/v1/alert-rules/"+subject.ID+"/sync", nil)
	if syncStatus == http.StatusConflict {
		t.Fatalf("POST /api/v1/alert-rules/%s/sync answered 409: this console is not running the "+
			"PrometheusRule reconciler, so nothing can be applied. e2e/testdata/console-values.yaml sets "+
			"console.alerting.enabled=true -- check the console logs. Body: %s", subject.ID, syncData)
	}
	if syncStatus != http.StatusAccepted {
		t.Fatalf("expected POST /api/v1/alert-rules/%s/sync 202 (a kick is enqueued, never awaited), got %d: %s",
			subject.ID, syncStatus, syncData)
	}
	var kick struct {
		Status string `json:"status"`
	}
	decodeJSON(t, "sync-kick response", syncData, &kick)
	if kick.Status != "kicked" {
		t.Errorf("expected the sync response to report status kicked, got %q", kick.Status)
	}
	// A sync kick for an id that names nothing would promise an outcome that
	// never arrives, so existence is checked even though the reconcile is
	// whole-bundle.
	missingStatus, _, missingData := mustRequest(t, http.MethodPost,
		base+"/api/v1/alert-rules/00000000-0000-0000-0000-000000000000/sync", nil)
	if missingStatus != http.StatusNotFound {
		t.Errorf("expected a sync kick for an unknown rule id to be 404, got %d: %s", missingStatus, missingData)
	}

	synced := awaitAlertRuleSynced(t, base, subject.ID,
		"alert rule "+subjectName+" to reach syncStatus=synced against the real PrometheusRule CRD")
	if synced.LastSyncedAt == nil {
		t.Errorf("expected lastSyncedAt to be stamped alongside syncStatus=synced, got null")
	}
	if synced.SyncMessage != "" {
		t.Errorf("expected a synced rule to carry no sync message, got %q", synced.SyncMessage)
	}
	t.Logf("alert rule %s synced at %v", subjectName, synced.LastSyncedAt)

	syncedKeeper := awaitAlertRuleSynced(t, base, keeper.ID,
		"alert rule "+keeperName+" to reach syncStatus=synced")
	if syncedKeeper.LastSyncedAt == nil {
		t.Fatalf("expected the keeper rule to carry a lastSyncedAt once synced, got null")
	}
	baseline := *syncedKeeper.LastSyncedAt

	// The object is in the cluster AND it is ours -- see the doc comment for
	// why these two reads are the assertion.
	foreign := mustListForeignRules(t, base)
	if _, isForeign := findForeignRule(foreign, consoleBundleName); isForeign {
		t.Errorf("expected the console's own bundle %q to be EXCLUDED from the foreign list: it carries "+
			"app.kubernetes.io/managed-by=kconmon-ng-console, and an object listed as foreign is one that "+
			"does not. Foreign listing: %+v", consoleBundleName, foreign)
	}
	if _, haveFixture := findForeignRule(foreign, foreignFixtureName); !haveFixture {
		t.Errorf("expected the harness fixture %q in the foreign list -- without it, the assertion above "+
			"cannot tell 'our bundle is correctly labelled' from 'this list is empty because the cluster read "+
			"returned nothing'. Applied by .github/workflows/e2e.yaml from "+
			"e2e/testdata/foreign-prometheusrule.yaml. Foreign listing: %+v", foreignFixtureName, foreign)
	}

	// --- Disable, and prove the cluster caught up. ---
	disableStatus, _, disableData := mustRequest(t, http.MethodPut, base+"/api/v1/alert-rules/"+subject.ID,
		alertRuleBody(subjectName, "warning", int64(time.Minute), false))
	if disableStatus != http.StatusOK {
		t.Fatalf("expected PUT /api/v1/alert-rules/%s (disable) 200, got %d: %s",
			subject.ID, disableStatus, disableData)
	}
	var disabled alertRuleRow
	decodeJSON(t, "disable-alert-rule response", disableData, &disabled)
	if disabled.Enabled {
		t.Fatalf("expected the rule to be disabled by the replace, got enabled")
	}
	if disabled.SyncStatus != "unsynced" {
		t.Errorf("expected the disabling replace to reset syncStatus to unsynced, got %q", disabled.SyncStatus)
	}

	mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/"+subject.ID+"/sync", nil)

	var afterDisable alertRuleRow
	pollUntil(t, alertSyncBudget, 3*time.Second,
		"a PrometheusRule apply to land AFTER "+subjectName+" was disabled (the keeper's lastSyncedAt moving "+
			"past "+baseline.Format(time.RFC3339Nano)+")", func() bool {
			got, ok := pollAlertRule(t, base, keeper.ID)
			if !ok {
				return false
			}
			afterDisable = got
			if got.LastSyncedAt != nil && got.LastSyncedAt.After(baseline) {
				return true
			}
			mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/"+keeper.ID+"/sync", nil)
			return false
		})
	// An empty bundle is legal and a shrunken one is ordinary: what the apply
	// carried was rendered from the ENABLED rules only, so the disabled rule
	// is not in the object that just landed.
	t.Logf("keeper %s re-applied at %v with status %q (message %q) after %s was disabled",
		keeperName, afterDisable.LastSyncedAt, afterDisable.SyncStatus, afterDisable.SyncMessage, subjectName)

	// The disabled rule is now OUTSIDE the reconciler's world: it walks
	// enabled rows only, so nothing will ever write a status onto this one
	// again. unsynced is the honest state for a rule that is not applied
	// anywhere, and a console that reported it as synced would be claiming the
	// cluster is evaluating a rule its owner switched off.
	stillOff := getAlertRule(t, base, subject.ID)
	if stillOff.SyncStatus != "unsynced" {
		t.Errorf("expected a DISABLED rule to stay unsynced (the reconciler walks enabled rules only, "+
			"so it is in no bundle and nothing updates its status), got %q (message %q)",
			stillOff.SyncStatus, stillOff.SyncMessage)
	}
}

// TestConsoleAlertRuleForeignImport is M7 Decision 4 end to end: a
// PrometheusRule this console does not own is LISTED read-only, adoption is an
// explicit import that COPIES its rules into builder rows, and the foreign
// object is never touched.
//
// The last clause is the one worth a test rather than a comment. After an
// adoption the same alerts are defined twice in the cluster -- once by the
// object its owner still controls, once by the console's own bundle -- and
// deleting theirs is THEIR decision. A console that "helpfully" cleaned up
// after itself would be silently editing somebody else's alerting, so the
// fixture is re-read after the import and its projection compared field by
// field.
//
// The fixture is harness state, not test state: .github/workflows/e2e.yaml
// applies it from e2e/testdata/foreign-prometheusrule.yaml and nothing here
// creates or deletes it. What this test does clean up is the console ROW the
// import mints, which is what keeps the suite re-runnable -- and it drops a
// leftover of the same name up front, so a run that died mid-import does not
// poison every run after it.
func TestConsoleAlertRuleForeignImport(t *testing.T) {
	base := consoleBaseURL(t)

	before := mustListForeignRules(t, base)
	fixture, found := findForeignRule(before, foreignFixtureName)
	if !found {
		t.Fatalf("expected the foreign PrometheusRule fixture %q in GET /api/v1/alert-rules/foreign. It is "+
			"applied by .github/workflows/e2e.yaml from e2e/testdata/foreign-prometheusrule.yaml, into the "+
			"release namespace, before the test run. Foreign listing: %+v", foreignFixtureName, before)
	}
	if fixture.ManagedBy != foreignFixtureManagedBy {
		t.Errorf("expected the fixture's managedBy to be served verbatim as %q, got %q "+
			"(the API surfaces this because 'managed by another tool' and 'managed by nobody' are different "+
			"facts for an operator deciding whether to adopt)", foreignFixtureManagedBy, fixture.ManagedBy)
	}
	if fixture.Groups != 1 || fixture.Rules != 2 {
		t.Errorf("expected the fixture to project as 1 group / 2 rule entries (one alert, one record), got %d/%d",
			fixture.Groups, fixture.Rules)
	}

	dropAlertRuleByName(t, base, foreignFixtureAlert)

	importStatus, _, importData := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/import",
		map[string]any{"name": foreignFixtureName})
	if importStatus != http.StatusOK {
		t.Fatalf("expected POST /api/v1/alert-rules/import 200, got %d: %s", importStatus, importData)
	}
	var report struct {
		Created []string `json:"created"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Notes []struct {
			Name string `json:"name"`
			Note string `json:"note"`
		} `json:"notes"`
	}
	decodeJSON(t, "import report", importData, &report)
	// All three lists are non-nil on the wire: the UI renders all three after
	// pressing Import, and a null would be a runtime error in exactly the
	// place somebody is looking.
	if report.Created == nil || report.Skipped == nil || report.Notes == nil {
		t.Errorf("expected created/skipped/notes to be arrays (possibly empty) and never null: %s", importData)
	}
	if !slices.Contains(report.Created, foreignFixtureAlert) {
		t.Fatalf("expected the import to create %q, got created=%v skipped=%+v", foreignFixtureAlert,
			report.Created, report.Skipped)
	}
	t.Cleanup(func() { dropAlertRuleByName(t, base, foreignFixtureAlert) })

	// The record entry is not a failure and not a rendering problem: a
	// recording rule produces a time series, an alert rule produces an alert,
	// and alert_rules has no column that could mean the first.
	skippedRecord := false
	for _, s := range report.Skipped {
		if s.Name == foreignFixtureRecord {
			skippedRecord = true
			if !strings.Contains(s.Reason, "recording rule") {
				t.Errorf("expected the recording rule's skip reason to say so, got %q", s.Reason)
			}
		}
	}
	if !skippedRecord {
		t.Errorf("expected the fixture's recording rule %q in the import's skipped list, got %+v",
			foreignFixtureRecord, report.Skipped)
	}

	adopted, ok := findAlertRuleByName(t, base, foreignFixtureAlert)
	if !ok {
		t.Fatalf("expected an alert rule named %q after the import reported creating it", foreignFixtureAlert)
	}
	// Adoption copies into the ONE kind the builder has no model for: raw,
	// carrying the foreign expression verbatim.
	if adopted.Kind != "raw" {
		t.Errorf("expected an adopted rule to land as kind raw, got %q", adopted.Kind)
	}
	if adopted.RenderedExpr != e2eRawExpr {
		t.Errorf("expected the adopted rule to carry the fixture's expression %q, got %q",
			e2eRawExpr, adopted.RenderedExpr)
	}
	// severity is LIFTED out of the label set into its own column, and the
	// label is dropped: one fact editable in two places is a fact that can
	// disagree with itself.
	if adopted.Severity != "critical" {
		t.Errorf("expected labels.severity=critical to be lifted into the severity column, got %q", adopted.Severity)
	}
	var adoptedLabels map[string]string
	decodeJSON(t, "adopted rule labels", adopted.Labels, &adoptedLabels)
	if _, present := adoptedLabels["severity"]; present {
		t.Errorf("expected the reserved severity label to be dropped from the adopted label set, got %v", adoptedLabels)
	}
	if adoptedLabels["origin"] != foreignFixtureManagedBy {
		t.Errorf("expected the fixture's own labels to survive adoption (origin=%q), got %v",
			foreignFixtureManagedBy, adoptedLabels)
	}
	// "2m" in the object, nanoseconds in the column. Asserted because a
	// misparsed `for` is the single most consequential thing this importer
	// could get wrong: 0 means "fire the instant the expression holds".
	if want := int64(2 * time.Minute); adopted.ForNs != want {
		t.Errorf("expected the fixture's `for: 2m` to import as %dns, got %d", want, adopted.ForNs)
	}
	if !adopted.Enabled {
		t.Errorf("expected an adopted rule to arrive enabled, got false")
	}

	// The foreign object is UNCHANGED and still foreign. Re-read on the same
	// route the import read it through, so a mutation of any kind -- an added
	// label, a rewritten group, a deletion -- shows up as a changed projection
	// or a missing entry.
	after := mustListForeignRules(t, base)
	stillThere, present := findForeignRule(after, foreignFixtureName)
	if !present {
		t.Fatalf("expected the foreign object %q to still exist after being adopted: the console COPIES a "+
			"foreign rule and never owns, edits or deletes it. Foreign listing: %+v", foreignFixtureName, after)
	}
	if stillThere != fixture {
		t.Errorf("expected the foreign object to be untouched by the import, got %+v before and %+v after",
			fixture, stillThere)
	}

	// Adopting twice does not duplicate: the name is taken, by the row the
	// first import made.
	againStatus, _, againData := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/import",
		map[string]any{"name": foreignFixtureName})
	if againStatus != http.StatusOK {
		t.Fatalf("expected a second POST /api/v1/alert-rules/import 200, got %d: %s", againStatus, againData)
	}
	var second struct {
		Created []string `json:"created"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	decodeJSON(t, "second import report", againData, &second)
	if len(second.Created) != 0 {
		t.Errorf("expected a repeated import to create nothing, got %v", second.Created)
	}
	takenReported := false
	for _, s := range second.Skipped {
		if s.Name == foreignFixtureAlert && strings.Contains(s.Reason, "already taken") {
			takenReported = true
		}
	}
	if !takenReported {
		t.Errorf("expected the repeated import to skip %q naming the taken name, got %+v",
			foreignFixtureAlert, second.Skipped)
	}

	// An object this console already owns is not foreign, and neither is one
	// that does not exist: ONE 404 covers both, deliberately (the import's own
	// lookup is the foreign list, which excludes our bundle).
	for _, name := range []string{uniqueName("e2e-no-such-object"), consoleBundleName} {
		status, _, data := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/import",
			map[string]any{"name": name})
		if status != http.StatusNotFound {
			t.Errorf("expected importing %q to be 404, got %d: %s", name, status, data)
		}
	}
	// A blank name is a rejected field VALUE, not a 404: answering "no foreign
	// rule is called nothing" would send the caller to look at their cluster
	// for a bug that is in their request.
	blankStatus, _, blankData := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/import",
		map[string]any{"name": "   "})
	if blankStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected an import with a blank name to be 422, got %d: %s", blankStatus, blankData)
	}
}

// TestConsoleAlertsWithoutPrometheus pins the honest-empty branch of GET
// /api/v1/alerts against the harness as it really is.
//
// e2e/testdata/console-values.yaml sets no console.prometheus.url, and the
// chart default is the empty string, so this console genuinely has no
// Prometheus. That is not a gap in the harness -- it is the case the route's
// shape exists for. The firing list answers 200 with an empty array and
// promConfigured:false rather than 503, because the Overview card that
// consumes it has to be able to render "nothing is firing" and "nobody is
// watching" without treating one of them as an error. GET /api/v1/matrix
// answers 503 for the same missing dependency, and the difference is the
// point: the matrix IS the Prometheus data, while the firing set is a list
// that is legitimately empty.
//
// If a future harness DOES point the console at a Prometheus, this test says
// so instead of failing: the assertions below fork on promConfigured, and the
// configured branch checks the only invariant that survives -- the list is an
// array, never null.
func TestConsoleAlertsWithoutPrometheus(t *testing.T) {
	base := consoleBaseURL(t)

	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/alerts", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/alerts 200 (an unconfigured Prometheus is an honest empty list, "+
			"never a 503 -- that divergence from /api/v1/matrix is deliberate), got %d: %s", status, data)
	}
	var page struct {
		Alerts         *[]firingAlertRow `json:"alerts"`
		PromConfigured bool              `json:"promConfigured"`
	}
	decodeJSON(t, "alerts response", data, &page)
	if page.Alerts == nil {
		t.Fatalf("expected alerts to be an array (possibly empty) and never null: %s", data)
	}
	if page.PromConfigured {
		t.Logf("this console HAS a Prometheus configured (%d alerts): the honest-empty branch is not "+
			"exercised by this harness", len(*page.Alerts))
	} else if len(*page.Alerts) != 0 {
		t.Errorf("expected an empty firing list when promConfigured is false, got %d entries: %s",
			len(*page.Alerts), data)
	}

	// managedOnly is a real filter on this route and must parse as a boolean.
	okStatus, _, okData := mustRequest(t, http.MethodGet, base+"/api/v1/alerts?managedOnly=true", nil)
	if okStatus != http.StatusOK {
		t.Errorf("expected GET /api/v1/alerts?managedOnly=true 200, got %d: %s", okStatus, okData)
	}
	// Refused rather than defaulted to false: an unparseable filter silently
	// read as "no filter" returns MORE than was asked for, which is the wrong
	// direction to guess in.
	badStatus, _, badData := mustRequest(t, http.MethodGet, base+"/api/v1/alerts?managedOnly=perhaps", nil)
	if badStatus != http.StatusBadRequest {
		t.Errorf("expected an unparseable managedOnly to be 400, got %d: %s", badStatus, badData)
	}
}

// firingAlertRow is one entry of GET /api/v1/alerts. Only the fields this file
// asserts on; value is a STRING upstream because it carries NaN and +/-Inf.
type firingAlertRow struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Severity string `json:"severity"`
	RuleID   string `json:"ruleId"`
}

// exportBundleMap is a bundle as this file handles it: the raw JSON object,
// decoded into a map rather than into six mirrored Go structs.
//
// Deliberate. The round trip below has to POST BACK BYTE-FOR-BYTE what the
// export served (that is the whole idempotence claim), and re-encoding through
// hand-written structs would quietly drop any field this file forgot to
// mirror -- turning "the console round-trips its own configuration" into "the
// console round-trips the subset e2e knows about". A map carries every key,
// including the ones a future milestone adds.
type exportBundleMap map[string]any

// bundleCollection returns one collection of a bundle as a []any, failing the
// test when it is absent or not an array -- the bundle's shape claim, checked
// once, where every caller benefits.
func bundleCollection(t *testing.T, bundle exportBundleMap, key string) []any {
	t.Helper()
	raw, present := bundle[key]
	if !present {
		t.Fatalf("expected the export bundle to carry a %q collection, got keys %v", key, bundleKeys(bundle))
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected bundle collection %q to be an array, got %T", key, raw)
	}
	return items
}

func bundleKeys(bundle exportBundleMap) []string {
	keys := make([]string, 0, len(bundle))
	for key := range bundle {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// bundleItemNamed finds the item whose "name" is name, so a test can mutate
// exactly its own row in a bundle that also carries every other test's.
func bundleItemNamed(t *testing.T, items []any, name string) map[string]any {
	t.Helper()
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if item["name"] == name {
			return item
		}
	}
	t.Fatalf("expected an item named %q in the bundle collection", name)
	return nil
}

// importCollection is one collection's outcome in an import response.
type importCollection struct {
	Created int `json:"created"`
	Updated int `json:"updated"`
	Skipped int `json:"skipped"`
	Errors  []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"errors"`
	Warnings []struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	} `json:"warnings"`
}

// importReport is POST /api/v1/import's body. Identical in shape for a dry run
// and an apply -- that is the entire point of the dry run, and the assertions
// below compare the two directly.
type importReport struct {
	DryRun             bool             `json:"dryRun"`
	Targets            importCollection `json:"targets"`
	CheckDefinitions   importCollection `json:"checkDefinitions"`
	CheckSchedules     importCollection `json:"checkSchedules"`
	AlertRules         importCollection `json:"alertRules"`
	Webhooks           importCollection `json:"webhooks"`
	MaintenanceWindows importCollection `json:"maintenanceWindows"`
}

// collections returns the six results beside their bundle key names, so an
// assertion that holds for every collection is written once.
func (r *importReport) collections() []struct {
	name string
	res  *importCollection
} {
	return []struct {
		name string
		res  *importCollection
	}{
		{"targets", &r.Targets},
		{"checkDefinitions", &r.CheckDefinitions},
		{"checkSchedules", &r.CheckSchedules},
		{"alertRules", &r.AlertRules},
		{"webhooks", &r.Webhooks},
		{"maintenanceWindows", &r.MaintenanceWindows},
	}
}

// postImport runs one import and returns its report.
func postImport(t *testing.T, base string, dryRun bool, bundle exportBundleMap) importReport {
	t.Helper()
	what := "apply"
	if dryRun {
		what = "dry run"
	}
	status, _, data := mustRequest(t, http.MethodPost, base+"/api/v1/import",
		map[string]any{"dryRun": dryRun, "bundle": bundle})
	if status != http.StatusOK {
		t.Fatalf("expected POST /api/v1/import (%s) 200, got %d: %s", what, status, data)
	}
	var report importReport
	decodeJSON(t, "import report ("+what+")", data, &report)
	if report.DryRun != dryRun {
		t.Errorf("expected the import response to echo dryRun=%v, got %v", dryRun, report.DryRun)
	}
	return report
}

// TestConsoleExportImportRoundTrip proves M7 Decision 9's central claim: the
// bundle a console exports is a bundle it can import, the dry run predicts
// exactly what the apply does, and a re-import of an unmodified bundle changes
// nothing.
//
// That last property is what makes this more than a serialisation test. Every
// collection is merged by NATURAL KEY, not by id -- no store call anywhere
// accepts a caller-chosen primary key -- so a bundle round-tripped into its own
// source console must resolve every item to the row it came from and update or
// skip it. A single spurious create here would mean a restore duplicates the
// configuration it was meant to restore.
//
// The secret scan is the second claim and it is made on RAW BYTES, not on a
// decoded struct: a bundle carries webhooks as name/url/events/enabled plus a
// hasSecret BOOLEAN, and the sealed bytes never leave the store through this
// package. A typed assertion could not notice a secret field, because the type
// has none -- which is the point and also why it could never catch a
// regression that added one.
func TestConsoleExportImportRoundTrip(t *testing.T) {
	base := consoleBaseURL(t)

	// The two rows the IMPORT mints have no create call to hang a cleanup off,
	// so their names are minted here and their cleanups registered FIRST --
	// which, t.Cleanup being LIFO, is what makes them run LAST.
	//
	// That ordering is load-bearing rather than tidy. The renamed target below
	// becomes the destination the seeded definition points at (the importer
	// remaps the bundle's target id onto the row it actually created), and a
	// target with a definition still pointing at it is a 409 by design. A
	// cleanup registered at the point of mutation would run BEFORE the
	// definition's and fail on that 409 -- in cleanup, where the failure would
	// name the wrong thing entirely.
	renamedTarget := uniqueName("e2e-import-target")
	t.Cleanup(func() { dropTargetByName(t, base, renamedTarget) })
	newRuleName := uniqueName("e2e-import-rule")
	t.Cleanup(func() { dropAlertRuleByName(t, base, newRuleName) })

	// --- Seed one item in each of the six collections. ---
	targetName := uniqueName("e2e-export-target")
	targetID := createTarget(t, base, map[string]any{
		"name":    targetName,
		"kind":    "host",
		"address": "e2e-export.invalid:80",
		"labels":  map[string]string{"origin": "e2e-export"},
	})
	definitionName := uniqueName("e2e-export-def")
	definitionID := createDefinition(t, base, map[string]any{
		"name":                definitionName,
		"sourceSelection":     "one-per-zone",
		"destinationKind":     "target",
		"destinationTargetId": targetID,
		"checkType":           "tcp",
		"plane":               "pod",
		"enabled":             false,
	})
	createSchedule(t, base, map[string]any{
		"definitionId": definitionID,
		"kind":         "interval",
		"intervalNs":   int64(10 * time.Minute),
		"enabled":      false,
	})
	alertRuleName := uniqueName("e2e-export-rule")
	createAlertRule(t, base, alertRuleBody(alertRuleName, "warning", int64(time.Minute), true))
	webhookName := uniqueName("e2e-export-hook")
	createWebhook(t, base, map[string]any{
		"name":   webhookName,
		"url":    unroutableWebhookURL,
		"events": []string{"incident.resolved"},
		"secret": e2eWebhookSecret,
	})
	// The window must END IN THE FUTURE: an export carries only windows that
	// have not ended (a closed window is history, and carrying every window
	// ever declared would make the bundle grow with time).
	windowStart := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	windowEnd := windowStart.Add(2 * time.Hour)
	windowScope := uniqueName("e2e-export-scope")
	windowStatus, _, windowData := mustRequest(t, http.MethodPost, base+"/api/v1/maintenance", map[string]any{
		"scope":   windowScope,
		"startAt": windowStart.Format(time.RFC3339),
		"endAt":   windowEnd.Format(time.RFC3339),
		"reason":  "e2e export/import round trip",
	})
	if windowStatus != http.StatusCreated {
		t.Fatalf("expected POST /api/v1/maintenance 201, got %d: %s", windowStatus, windowData)
	}
	var window maintenanceWindow
	decodeJSON(t, "create-maintenance response", windowData, &window)
	t.Cleanup(func() { deleteResource(t, base+"/api/v1/maintenance/"+window.ID) })

	// --- Export. ---
	exportStatus, _, exportData := mustRequest(t, http.MethodGet, base+"/api/v1/export", nil)
	if exportStatus == http.StatusForbidden {
		t.Fatalf("GET /api/v1/export answered 403: export/import is admin-only (settings:write), and "+
			"auth.mode=anonymous stamps ONE role on every request. e2e/testdata/console-values.yaml must set "+
			"console.auth.anonymous.role=admin. Body: %s", exportData)
	}
	if exportStatus != http.StatusOK {
		t.Fatalf("expected GET /api/v1/export 200, got %d: %s", exportStatus, exportData)
	}
	// The whole claim, on the bytes a client actually receives.
	assertNoSecretServed(t, "GET /api/v1/export", exportData)

	var bundle exportBundleMap
	decodeJSON(t, "export bundle", exportData, &bundle)
	if version, _ := bundle["version"].(float64); int(version) != 1 {
		t.Errorf("expected the bundle to declare version 1 (checked for EQUALITY on import, never >=), got %v",
			bundle["version"])
	}
	if _, present := bundle["exportedAt"]; !present {
		t.Errorf("expected the bundle to carry exportedAt, got keys %v", bundleKeys(bundle))
	}
	// All six collections, each carrying the item seeded above -- so the
	// assertions below are about a bundle that is actually populated.
	for _, tc := range []struct {
		key  string
		name string
	}{
		{"targets", targetName},
		{"checkDefinitions", definitionName},
		{"alertRules", alertRuleName},
		{"webhooks", webhookName},
	} {
		bundleItemNamed(t, bundleCollection(t, bundle, tc.key), tc.name)
	}
	if len(bundleCollection(t, bundle, "checkSchedules")) == 0 {
		t.Errorf("expected the bundle's checkSchedules collection to be populated")
	}
	if len(bundleCollection(t, bundle, "maintenanceWindows")) == 0 {
		t.Errorf("expected the bundle's maintenanceWindows collection to carry the still-open window")
	}
	// hasSecret is informational and TRUE for a stored row (the store refuses
	// an empty secret); it is never a licence to create one on import.
	exportedHook := bundleItemNamed(t, bundleCollection(t, bundle, "webhooks"), webhookName)
	if hasSecret, _ := exportedHook["hasSecret"].(bool); !hasSecret {
		t.Errorf("expected the exported webhook to carry hasSecret=true, got %v", exportedHook["hasSecret"])
	}
	if _, present := exportedHook["secret"]; present {
		t.Errorf("expected no secret key on an exported webhook at all, got %v", exportedHook)
	}

	// --- Idempotence: the same bundle, into the console it came from. ---
	dry := postImport(t, base, true, bundle)
	for _, c := range dry.collections() {
		if c.res.Created != 0 {
			t.Errorf("expected a dry run of an UNMODIFIED bundle to create nothing in %s (every item merges by "+
				"natural key onto the row it came from), got created=%d", c.name, c.res.Created)
		}
		if len(c.res.Errors) != 0 {
			t.Errorf("expected no errors in %s on a round-tripped bundle, got %+v", c.name, c.res.Errors)
		}
	}
	if dry.Targets.Updated == 0 || dry.CheckDefinitions.Updated == 0 || dry.Webhooks.Updated == 0 {
		t.Errorf("expected targets/checkDefinitions/webhooks to be UPDATED by a re-import of their own bundle, "+
			"got %d/%d/%d", dry.Targets.Updated, dry.CheckDefinitions.Updated, dry.Webhooks.Updated)
	}
	// Windows are SKIPPED and never updated: the store has no update for one
	// by design (a window is two timestamps and a reason, so delete-and-
	// recreate is the whole of the correction path).
	if dry.MaintenanceWindows.Skipped == 0 || dry.MaintenanceWindows.Updated != 0 {
		t.Errorf("expected an identical maintenance window to be SKIPPED and never updated, got "+
			"skipped=%d updated=%d", dry.MaintenanceWindows.Skipped, dry.MaintenanceWindows.Updated)
	}

	applied := postImport(t, base, false, bundle)
	for _, c := range applied.collections() {
		if c.res.Created != 0 {
			t.Errorf("expected APPLYING an unmodified bundle to create nothing in %s, got created=%d",
				c.name, c.res.Created)
		}
		if len(c.res.Errors) != 0 {
			t.Errorf("expected no errors in %s on an applied round-trip, got %+v", c.name, c.res.Errors)
		}
	}

	// --- Mutate: what the dry run predicts is what the apply does. ---
	bundleItemNamed(t, bundleCollection(t, bundle, "targets"), targetName)["name"] = renamedTarget

	bundle["alertRules"] = append(bundleCollection(t, bundle, "alertRules"), map[string]any{
		"id":           "00000000-0000-0000-0000-000000000000",
		"name":         newRuleName,
		"kind":         "raw",
		"params":       map[string]any{"expr": e2eRawExpr},
		"severity":     "critical",
		"forNs":        int64(30 * time.Second),
		"labels":       map[string]any{},
		"annotations":  map[string]any{},
		"enabled":      true,
		"renderedExpr": e2eRawExpr,
	})

	// A webhook the destination does not have. It is the ONE asymmetry in the
	// whole import and it is the store's rule, not the importer's: every
	// delivery is signed, a bundle never carries a secret, and the only
	// alternatives to skipping would be fabricating a signing key or weakening
	// that rule.
	freshHookName := uniqueName("e2e-import-hook")
	bundle["webhooks"] = append(bundleCollection(t, bundle, "webhooks"), map[string]any{
		"id":        "00000000-0000-0000-0000-000000000000",
		"name":      freshHookName,
		"url":       unroutableWebhookURL,
		"events":    []any{"incident.created"},
		"enabled":   true,
		"hasSecret": true,
	})

	mutatedDry := postImport(t, base, true, bundle)
	if mutatedDry.Targets.Created != 1 {
		t.Errorf("expected the renamed target to be predicted as ONE create, got created=%d updated=%d errors=%+v",
			mutatedDry.Targets.Created, mutatedDry.Targets.Updated, mutatedDry.Targets.Errors)
	}
	if mutatedDry.AlertRules.Created != 1 {
		t.Errorf("expected the added alert rule to be predicted as ONE create, got created=%d errors=%+v",
			mutatedDry.AlertRules.Created, mutatedDry.AlertRules.Errors)
	}
	if mutatedDry.Webhooks.Skipped != 1 {
		t.Errorf("expected the secret-less webhook to be predicted as ONE skip, got skipped=%d created=%d",
			mutatedDry.Webhooks.Skipped, mutatedDry.Webhooks.Created)
	}
	assertWebhookSkipWarning(t, &mutatedDry.Webhooks, freshHookName, "dry run")
	// A dry run WRITES NOTHING. Checked rather than assumed, because it is the
	// property everything above rests on.
	if _, exists := findAlertRuleByName(t, base, newRuleName); exists {
		t.Errorf("expected a dry run to persist nothing, but alert rule %q exists", newRuleName)
	}

	mutatedApply := postImport(t, base, false, bundle)
	if mutatedApply.Targets.Created != mutatedDry.Targets.Created ||
		mutatedApply.AlertRules.Created != mutatedDry.AlertRules.Created ||
		mutatedApply.Webhooks.Skipped != mutatedDry.Webhooks.Skipped {
		t.Errorf("expected the apply to match what the dry run predicted; dry: targets.created=%d "+
			"alertRules.created=%d webhooks.skipped=%d, apply: %d/%d/%d",
			mutatedDry.Targets.Created, mutatedDry.AlertRules.Created, mutatedDry.Webhooks.Skipped,
			mutatedApply.Targets.Created, mutatedApply.AlertRules.Created, mutatedApply.Webhooks.Skipped)
	}
	assertWebhookSkipWarning(t, &mutatedApply.Webhooks, freshHookName, "apply")

	// --- Verify the apply through the ordinary read routes. ---
	importedRule, ok := findAlertRuleByName(t, base, newRuleName)
	if !ok {
		t.Fatalf("expected the imported alert rule %q to exist after the apply", newRuleName)
	}
	if importedRule.Severity != "critical" || importedRule.RenderedExpr != e2eRawExpr {
		t.Errorf("expected the imported rule to arrive as declared (critical, %q), got severity %q expr %q",
			e2eRawExpr, importedRule.Severity, importedRule.RenderedExpr)
	}
	if _, found := findTargetByName(t, base, renamedTarget); !found {
		t.Errorf("expected the renamed target %q to exist after the apply", renamedTarget)
	}
	// The endpoint that arrived without a secret was SKIPPED, so it is not
	// there -- and must not be, because a webhook the console cannot sign is
	// an endpoint it must not have created.
	if _, found := findWebhookByName(t, base, freshHookName); found {
		t.Errorf("expected the secret-less webhook %q NOT to be created by the import", freshHookName)
	}

	// A bundle version this build does not read is refused whole, never
	// partially applied: silently importing the subset a future console
	// described would be a partial restore presented as a complete one.
	bundle["version"] = 999
	badStatus, _, badData := mustRequest(t, http.MethodPost, base+"/api/v1/import",
		map[string]any{"bundle": bundle})
	if badStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected an unsupported bundle version to be 422, got %d: %s", badStatus, badData)
	}
	missingStatus, _, missingData := mustRequest(t, http.MethodPost, base+"/api/v1/import",
		map[string]any{"dryRun": true})
	if missingStatus != http.StatusUnprocessableEntity {
		t.Errorf("expected an import with no bundle to be 422, got %d: %s", missingStatus, missingData)
	}
}

// assertWebhookSkipWarning checks the one WARNING this API has: an endpoint the
// import handled correctly and completely, whose outcome still needs a human.
// A warning is not an error -- nothing failed, and the operator's next step is
// to create the endpoint with a secret and re-import.
func assertWebhookSkipWarning(t *testing.T, res *importCollection, name, what string) {
	t.Helper()
	for _, w := range res.Warnings {
		if w.Name == name {
			if !strings.Contains(w.Reason, "without secret") {
				t.Errorf("expected the %s webhook warning for %q to name the missing secret, got %q",
					what, name, w.Reason)
			}
			return
		}
	}
	t.Errorf("expected a %s WARNING (not an error) for the secret-less webhook %q, got warnings=%+v errors=%+v",
		what, name, res.Warnings, res.Errors)
}

// findTargetByName looks one target up by its natural key.
func findTargetByName(t *testing.T, base, name string) (string, bool) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/targets?limit=500", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/targets 200, got %d: %s", status, data)
	}
	var page struct {
		Targets []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"targets"`
	}
	decodeJSON(t, "targets list", data, &page)
	for i := range page.Targets {
		if page.Targets[i].Name == name {
			return page.Targets[i].ID, true
		}
	}
	return "", false
}

// dropTargetByName removes a target the IMPORT created, which no createTarget
// cleanup can know the id of.
func dropTargetByName(t *testing.T, base, name string) {
	t.Helper()
	if id, found := findTargetByName(t, base, name); found {
		deleteResource(t, base+"/api/v1/targets/"+id)
	}
}

// findWebhookByName reports whether an endpoint with that name exists.
func findWebhookByName(t *testing.T, base, name string) (string, bool) {
	t.Helper()
	status, _, data := mustRequest(t, http.MethodGet, base+"/api/v1/webhooks", nil)
	if status != http.StatusOK {
		t.Fatalf("expected GET /api/v1/webhooks 200, got %d: %s", status, data)
	}
	var page struct {
		Webhooks []webhookRow `json:"webhooks"`
	}
	decodeJSON(t, "webhooks list", data, &page)
	for i := range page.Webhooks {
		if page.Webhooks[i].Name == name {
			return page.Webhooks[i].ID, true
		}
	}
	return "", false
}

// wsEnvelope is every server->client frame, mirrored here rather than imported
// from internal/console/ws: these tests assert on the WIRE, and sharing the
// server's own type would make a rename invisible to exactly the test whose
// job is to notice it.
type wsEnvelope struct {
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	Seq   uint64          `json:"seq"`
	Data  json.RawMessage `json:"data"`
}

// wsErrorDetail reads the `error` string out of an error frame's payload.
func wsErrorDetail(t *testing.T, env *wsEnvelope) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		t.Errorf("decode ws error frame payload %q: %v", env.Data, err)
		return ""
	}
	return payload.Error
}

// wsSubscribe writes one subscribe frame.
func wsSubscribe(t *testing.T, conn *websocket.Conn, topic string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"action": "subscribe", "topic": topic}); err != nil {
		t.Fatalf("subscribe to %q failed: %v", topic, err)
	}
}

// wsAwait reads frames until match returns true or budget elapses, returning
// every frame it saw. A read deadline bounds each read so a socket that simply
// goes quiet ends the wait instead of the package timeout.
func wsAwait(t *testing.T, conn *websocket.Conn, budget time.Duration, match func(*wsEnvelope) bool) ([]wsEnvelope, bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var seen []wsEnvelope
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return seen, false
		}
		if err := conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			t.Fatalf("set ws read deadline: %v", err)
		}
		var env wsEnvelope
		if err := conn.ReadJSON(&env); err != nil {
			t.Logf("ws read ended after %d frame(s): %v", len(seen), err)
			return seen, false
		}
		seen = append(seen, env)
		if match(&env) {
			return seen, true
		}
	}
}

// TestConsoleWSSubscribeGate covers the /ws surface M7 touched: the upgrade
// row became `anyOf{events:read, runs:read}` (M3 follow-up #10, Decision 13)
// and each SUBSCRIBE gained a second, per-connection decision
// (httpapi.wsTopicAuthorizer).
//
// WHAT THIS TEST DOES NOT DO, AND WHY. The interesting half of that change --
// a runs:read-ONLY subject that is admitted to the socket and then refused the
// `live` topic by name -- is NOT REACHABLE in this harness, and no amount of
// test code can make it so. e2e/testdata/console-values.yaml runs
// auth.mode=anonymous, and internal/console/authn.NewAnonymous returns the SAME
// fixed Subject for every request "regardless of what the request carries": a
// Bearer token, a session cookie and a spoofed header are all ignored by
// construction (its own test asserts exactly that). So the M3 RBAC surface can
// happily mint a custom role {runs:read}, a binding and a PAT -- and the socket
// would still see the one configured role. There is no in-harness path from a
// created token to an authenticated connection.
//
// The both-permissions-absent 403 is unreachable for a second, independent
// reason: EVERY builtin role (viewer, operator, alert-editor, admin) holds
// events:read, so no value of console.auth.anonymous.role produces a subject
// the /ws row refuses. Covering it would need a whole third console rollout on
// auth.mode=token plus a bootstrap credential -- a harness of its own, for one
// status code that internal/console/httpapi's unit tests already pin against
// the live router.
//
// What IS asserted here is the part that only a real socket can prove, and it
// is not nothing: the route still admits an ordinary role after the anyOf
// rewrite, a refused subscribe is answered with an error FRAME rather than a
// closed connection, the refusal names the topic it refused, and the socket
// keeps working afterwards -- which is the property that makes a multiplexed
// socket usable at all, and the one a rewrite of the subscribe path is most
// likely to break.
func TestConsoleWSSubscribeGate(t *testing.T) {
	base := consoleBaseURL(t)

	wsURL := strings.Replace(strings.Replace(base, "https://", "wss://", 1), "http://", "ws://", 1) + "/ws"
	dialer := &websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, nil) //nolint:bodyclose // closed immediately below
	if resp != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Logf("close ws handshake response body: %v", cerr)
		}
	}
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("expected the /ws upgrade to be admitted (the route takes events:read OR runs:read, and "+
			"every builtin role holds the first), dial %s failed with status %d: %v", wsURL, status, err)
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			t.Logf("close ws connection: %v", cerr)
		}
	}()

	// A topic ADR-003 names but nothing implements: subscribing is an error
	// rather than a topic that silently never delivers.
	wsSubscribe(t, conn, "mtr")
	frames, got := wsAwait(t, conn, 15*time.Second, func(env *wsEnvelope) bool {
		return env.Type == "error" && env.Topic == "mtr"
	})
	if !got {
		t.Fatalf("expected an error frame for the unimplemented topic %q, saw %d frame(s): %+v", "mtr", len(frames), frames)
	}
	if detail := wsErrorDetail(t, &frames[len(frames)-1]); !strings.Contains(detail, "unknown topic") {
		t.Errorf("expected the refusal to say the topic is unknown and list the subscribable ones, got %q", detail)
	}

	// A run topic for a run that is not streaming here. EXISTENCE is checked
	// before permission, deliberately (ws.Hub.handleClientMessage says why):
	// answering "forbidden" for a run that simply is not open on this replica
	// would send a browser to fix its RBAC instead of falling back to polling.
	const absentRunTopic = "run:00000000-0000-0000-0000-000000000000"
	wsSubscribe(t, conn, absentRunTopic)
	runFrames, gotRun := wsAwait(t, conn, 15*time.Second, func(env *wsEnvelope) bool {
		return env.Type == "error" && env.Topic == absentRunTopic
	})
	if !gotRun {
		t.Fatalf("expected an error frame for a run topic that is not open, saw %d frame(s): %+v",
			len(runFrames), runFrames)
	}
	if detail := wsErrorDetail(t, &runFrames[len(runFrames)-1]); !strings.Contains(detail, "unknown topic") {
		t.Errorf("expected an unopened run topic to be refused as UNKNOWN rather than as forbidden "+
			"(existence is checked first, on purpose), got %q", detail)
	}

	// The socket survives both refusals. This is the assertion the two above
	// exist to set up: a refused subscribe must not cost the connection its
	// other topics, which is the whole premise of multiplexing N topics onto
	// one socket per browser tab.
	wsSubscribe(t, conn, "topology")
	topoFrames, gotTopo := wsAwait(t, conn, 60*time.Second, func(env *wsEnvelope) bool {
		return env.Topic == "topology" && env.Type != "error"
	})
	if !gotTopo {
		t.Fatalf("expected a topology frame after two refused subscribes -- the socket must stay usable -- "+
			"saw %d frame(s): %+v", len(topoFrames), topoFrames)
	}
	for i := range topoFrames {
		if topoFrames[i].Topic == "topology" && topoFrames[i].Type == "error" {
			t.Errorf("expected topology to be admitted for this subject, got an error frame: %s",
				wsErrorDetail(t, &topoFrames[i]))
		}
	}
}

// TestConsoleDegradedMode runs against a SEPARATE console rollout with
// console.database.mode=disabled (.github/workflows/e2e.yaml's "Reinstall
// console with database disabled" + "Run degraded-mode E2E tests" steps
// redeploy and re-forward before invoking just this test by name) -- it
// reuses KCONMON_CONSOLE_URL, now pointed at that rollout's fresh
// port-forward. This is the Phase A guarantee (M3 plan Decision) verified in
// a real cluster: with no database wired, the M1/M2 surface (healthz,
// topology) stays fully up and only the database-backed endpoints degrade
// to 503 -- never a 500, never a hang.
//
// M4 widened what "database-backed" covers: targets, check definitions and
// schedules are CONFIGURATION and get NO in-memory fallback (Plan Decision
// 13), unlike runs, which fall back to checks.NewMemoryStore(). So all three
// CRUD surfaces must answer 503 here -- and the M3 surface's behavior must be
// unchanged by their arrival, which is why the original assertions below are
// kept verbatim rather than folded into the new loop.
//
// M5 widened it once more, and its addition is the sharpest one in this test:
// GET /api/v1/topology?at= must be 503 while GET /api/v1/topology stays 200.
// One route, two answers, decided purely by whether a parameter is present --
// history needs a database and the live passthrough does not, so a rollout
// with no database must degrade the parameter without taking the route down.
//
// M6 adds incidents, maintenance windows, webhook endpoints and captured
// Kubernetes events. The last one is the interesting case: this rollout keeps
// console-values.yaml's console.kubernetesContext.enabled, so the console
// starts with the capture CONFIGURED and no database to write to -- it warns
// and skips the reader rather than failing to start, and the endpoint answers
// 503 rather than an empty 200.
func TestConsoleDegradedMode(t *testing.T) {
	base := consoleBaseURL(t)

	healthStatus, _, _ := mustRequest(t, http.MethodGet, base+"/healthz", nil)
	if healthStatus != http.StatusOK {
		t.Errorf("expected /healthz 200 in degraded mode, got %d", healthStatus)
	}

	topoStatus, _, _ := mustRequest(t, http.MethodGet, base+"/api/v1/topology", nil)
	if topoStatus != http.StatusOK {
		t.Errorf("expected /api/v1/topology 200 in degraded mode, got %d", topoStatus)
	}

	eventsStatus, _, _ := mustRequest(t, http.MethodGet, base+"/api/v1/events", nil)
	if eventsStatus != http.StatusServiceUnavailable {
		t.Errorf("expected /api/v1/events 503 with console.database.mode=disabled, got %d", eventsStatus)
	}

	// The three M4 CRUD surfaces, read and write side. A write is asserted
	// too because the 503 guard sits before the body is even decoded: a
	// regression that moved it after decoding would still answer 503 to a GET
	// while answering 400 to a malformed POST, which is a different contract.
	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/v1/targets", nil},
		{http.MethodPost, "/api/v1/targets", map[string]any{
			"name": uniqueName("e2e-degraded"), "kind": "host", "address": "e2e-degraded.invalid:80",
		}},
		{http.MethodGet, "/api/v1/checks", nil},
		{http.MethodPost, "/api/v1/checks", map[string]any{
			"name": uniqueName("e2e-degraded"), "sourceSelection": "one-per-zone",
			"destinationKind": "node", "checkType": "tcp", "plane": "pod",
		}},
		{http.MethodGet, "/api/v1/schedules", nil},
		{http.MethodPost, "/api/v1/schedules", map[string]any{
			"definitionId": "00000000-0000-0000-0000-000000000000", "kind": "continuous",
		}},
		// M5. Path history and operator notes have no in-memory fallback
		// either, for a reason the M4 surfaces share: a trace history or an
		// incident note that vanishes on pod restart is worse than one that
		// was never accepted.
		{http.MethodGet, "/api/v1/mtr/destinations", nil},
		// Both filters present and valid, so a 422 here would mean the
		// unavailability guard had slipped BEHIND the parameter validation --
		// and a request with them missing would then get 422 while a complete
		// one got 503, which is two different contracts on one route.
		{http.MethodGet, "/api/v1/mtr/snapshots?source=a&destination=b", nil},
		{http.MethodGet, "/api/v1/annotations", nil},
		{http.MethodPost, "/api/v1/annotations", map[string]any{
			"startAt": time.Now().UTC().Format(time.RFC3339),
			"scope":   uniqueName("e2e-degraded"),
			"text":    "e2e degraded-mode annotation",
		}},
		// The parameter, not the route: the plain GET above already asserted
		// 200 on the same path in the same rollout.
		{http.MethodGet, "/api/v1/topology?at=" +
			url.QueryEscape(time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339)), nil},
		// M6. Four more surfaces with no in-memory fallback, and the reasons
		// differ enough to be worth naming:
		//
		//   - incidents and maintenance windows are operator RECORDS, the
		//     annotations rule verbatim: one that vanishes on a pod restart is
		//     worse than one that was never accepted.
		//   - webhook endpoints are persisted CONFIGURATION (the targets /
		//     checks / schedules rule), and their signing secrets live in the
		//     same rows.
		//   - k8s-events is the sharpest of the four, because this rollout
		//     ALSO has console.kubernetesContext.enabled: the capture is
		//     configured and the endpoint is still 503, since captured events
		//     live in PostgreSQL and there is nowhere for the reader to have
		//     written them. Turning the capture on must not make this route
		//     answer an empty 200, which would report "nothing happened in
		//     your cluster" for "this console has no database".
		{http.MethodGet, "/api/v1/incidents", nil},
		{http.MethodPost, "/api/v1/incidents", map[string]any{
			"title":  uniqueName("e2e-degraded"),
			"fromAt": time.Now().UTC().Format(time.RFC3339),
		}},
		{http.MethodGet, "/api/v1/maintenance", nil},
		{http.MethodGet, "/api/v1/webhooks", nil},
		{http.MethodGet, "/api/v1/k8s-events", nil},
		// M7. Alert rules are persisted CONFIGURATION with no in-memory
		// fallback -- the targets/checks/schedules rule verbatim -- and the
		// three routes below are the three shapes of that gate:
		//
		//   - the CRUD surface, read and write side.
		//   - the SYNC kick, which is 503 and not 409 even though this
		//     rollout also has no reconciler: the two gates are ordered
		//     store-first on purpose, because a console with no database has
		//     no rules to sync in the first place, so naming the feature flag
		//     here would be the less actionable of two true statements.
		//   - the IMPORT, for the same ordering: there is nowhere to adopt TO
		//     before there is nothing to adopt FROM.
		{http.MethodGet, "/api/v1/alert-rules", nil},
		{http.MethodPost, "/api/v1/alert-rules", map[string]any{
			"name": uniqueName("e2e-degraded"), "kind": "raw",
			"params": map[string]any{"expr": e2eRawExpr}, "severity": "warning", "forNs": 0,
		}},
		{http.MethodPost, "/api/v1/alert-rules/00000000-0000-0000-0000-000000000000/sync", nil},
		{http.MethodPost, "/api/v1/alert-rules/import", map[string]any{"name": "anything"}},
		// Export/import reads EVERY persisted config table and is
		// all-or-nothing about it: a bundle with targets but no alert rules
		// because that one seam happened to be nil would be a restore point
		// with a hole in it, which is worse than no restore point.
		{http.MethodGet, "/api/v1/export", nil},
		{http.MethodPost, "/api/v1/import", map[string]any{
			"dryRun": true,
			"bundle": map[string]any{"version": 1},
		}},
	} {
		status, _, data := mustRequest(t, tc.method, base+tc.path, tc.body)
		if status != http.StatusServiceUnavailable {
			t.Errorf("expected %s %s 503 with console.database.mode=disabled, got %d: %s",
				tc.method, tc.path, status, data)
		}
	}

	// --- The three M7 routes that must NOT be 503, and each for its own
	// reason. This rollout keeps console.alerting.enabled=true from
	// console-values.yaml, so it is the only place these can be observed. ---

	// 1. The firing list needs Prometheus, not a database, and this console
	// has neither. It answers an honest empty 200: the Overview card must be
	// able to render "nothing is firing" and "nobody is watching" without
	// treating either as an error, and promConfigured is how it tells them
	// apart. A 503 here would make a console with no database report a
	// Prometheus outage.
	alertsStatus, _, alertsData := mustRequest(t, http.MethodGet, base+"/api/v1/alerts", nil)
	if alertsStatus != http.StatusOK {
		t.Errorf("expected /api/v1/alerts 200 in degraded mode (it needs no database at all), got %d: %s",
			alertsStatus, alertsData)
	} else {
		var page struct {
			Alerts         *[]firingAlertRow `json:"alerts"`
			PromConfigured bool              `json:"promConfigured"`
		}
		decodeJSON(t, "degraded alerts response", alertsData, &page)
		if page.PromConfigured {
			t.Errorf("expected promConfigured=false: e2e/testdata/console-values.yaml sets no "+
				"console.prometheus.url, so this console has nothing to read alerts from: %s", alertsData)
		}
		if page.Alerts == nil || len(*page.Alerts) != 0 {
			t.Errorf("expected an empty (never null) alerts array with no Prometheus configured: %s", alertsData)
		}
	}

	// 2. The foreign list is the ONE place in this API where 409 and 503 come
	// apart, and this rollout is where that is provable. The route reads the
	// CLUSTER, not the database, so the database being off is not its problem
	// -- what is missing is the reconciler, which the console drops when there
	// is no store to keep rules in. 409 says "the request is fine and the
	// resource exists, but it conflicts with the state this console is
	// configured to be in", and it names the flag to set. 503 would send an
	// operator to look at their database for a reconciler nobody asked to
	// start.
	foreignStatus, _, foreignData := mustRequest(t, http.MethodGet, base+"/api/v1/alert-rules/foreign", nil)
	if foreignStatus != http.StatusConflict {
		t.Errorf("expected /api/v1/alert-rules/foreign 409 (not 503) on a console with no PrometheusRule "+
			"reconciler, got %d: %s", foreignStatus, foreignData)
	} else if !strings.Contains(string(foreignData), "console.alerting.enabled") {
		t.Errorf("expected the 409 detail to name console.alerting.enabled -- the value an operator would "+
			"set to fix it -- got %s", foreignData)
	}

	// 3. Preview is gated on NEITHER: it renders an expression from a request
	// body and asks Prometheus what that expression currently matches. With no
	// Prometheus the render half still succeeds, so the honest answer is a 200
	// carrying the expression plus an error string for the half that could not
	// run -- refusing the whole request would hide a correct expression behind
	// an unrelated outage.
	previewStatus, _, previewData := mustRequest(t, http.MethodPost, base+"/api/v1/alert-rules/preview",
		alertRuleBody(uniqueName("e2e-degraded-preview"), "info", 0, true))
	if previewStatus != http.StatusOK {
		t.Errorf("expected /api/v1/alert-rules/preview 200 in degraded mode (it persists nothing and needs "+
			"no store), got %d: %s", previewStatus, previewData)
	} else {
		var preview struct {
			Expr  string `json:"expr"`
			Error string `json:"error"`
		}
		decodeJSON(t, "degraded preview response", previewData, &preview)
		if preview.Expr != e2eRawExpr {
			t.Errorf("expected the preview to render kind raw verbatim as %q, got %q", e2eRawExpr, preview.Expr)
		}
		if preview.Error == "" {
			t.Errorf("expected the preview to report that it could not evaluate the expression with no "+
				"Prometheus configured, got an empty error: %s", previewData)
		}
	}
}
