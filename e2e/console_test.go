//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
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
	} {
		status, _, data := mustRequest(t, tc.method, base+tc.path, tc.body)
		if status != http.StatusServiceUnavailable {
			t.Errorf("expected %s %s 503 with console.database.mode=disabled, got %d: %s",
				tc.method, tc.path, status, data)
		}
	}
}
