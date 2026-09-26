package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// plane goes into the run's JSONB spec and its plane column like the node names do, and PostgreSQL
// refuses a NUL there: a malformed body must be a 400, never Start's 502 "run not started".
func TestRunsCreateRefusesControlCharsInPlane(t *testing.T) {
	for name, plane := range map[string]string{"NUL": `pod\u0000`, "newline": `pod\n`} {
		t.Run(name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			body := `{"sources":["n1"],"destinations":["n3"],"type":"tcp","plane":"` + plane + `"}`
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "plane") {
				t.Errorf("body does not name the plane field: %s", w.Body.String())
			}
			if n := len(runner.started); n != 0 {
				t.Errorf("Start called %d times, want 0", n)
			}
		})
	}
}

// Every string a run persists is stored in its spec, and the plane in its own column too; each
// GET /api/v1/runs re-reads and re-encodes them for every viewer. A field longer than anything
// legitimate is a 400 before the runner sees it.
func TestRunsCreateBoundsEveryFreeStringItPersists(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"plane host", map[string]any{"sources": []string{"n1"}, "destinations": []string{"n3"}, "plane": "host"}, "plane"},
		{"plane of 8 MiB", map[string]any{"sources": []string{"n1"}, "destinations": []string{"n3"}, "plane": long(8 << 20)}, "plane"},
		{"plane in capitals", map[string]any{"sources": []string{"n1"}, "destinations": []string{"n3"}, "plane": "Pod"}, "plane"},
		{"source over 253 bytes", map[string]any{"sources": []string{long(254)}, "destinations": []string{"n3"}, "plane": "pod"}, "sources/destinations"},
		{"destination over 253 bytes", map[string]any{"sources": []string{"n1"}, "destinations": []string{long(254)}, "plane": "pod"}, "sources/destinations"},
		{"ad-hoc address over 2048 bytes", map[string]any{
			"sources": []string{"n1"}, "plane": "pod", "destinationKind": "adhoc",
			"destinationAddress": "https://example.com/" + long(2048),
		}, "destination"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			c.body["type"] = "tcp"
			raw, err := json.Marshal(c.body)
			if err != nil {
				t.Fatal(err)
			}
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", bytes.NewReader(raw), mutateWithCSRF)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%.300s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"invalid `+c.field+`"`) {
				t.Errorf("body does not name %s: %.300s", c.field, w.Body.String())
			}
			if w.Body.Len() > 1024 {
				t.Errorf("the refusal echoes the input back: %d bytes", w.Body.Len())
			}
			if n := len(runner.started); n != 0 {
				t.Errorf("Start called %d times, want 0", n)
			}
		})
	}
}

// The limits sit above anything real: a 253-byte node name and a 2048-byte URL still start a run.
func TestRunsCreateAcceptsStringsAtTheirBound(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"node name": {"sources": []string{strings.Repeat("a", 253)}, "destinations": []string{"n3"}, "plane": "pod"},
		"ad-hoc address": {
			"sources": []string{"n1"}, "plane": "pod", "destinationKind": "adhoc",
			"destinationAddress": "https://example.com/" + strings.Repeat("a", 2048-len("https://example.com/")),
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			body["type"] = "tcp"
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", bytes.NewReader(raw), mutateWithCSRF)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202; body=%.300s", w.Code, w.Body.String())
			}
		})
	}
}
