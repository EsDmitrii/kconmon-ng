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
	} {
		status, _, data := mustRequest(t, tc.method, base+tc.path, tc.body)
		if status != http.StatusServiceUnavailable {
			t.Errorf("expected %s %s 503 with console.database.mode=disabled, got %d: %s",
				tc.method, tc.path, status, data)
		}
	}
}
