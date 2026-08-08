package controllerclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/controllerclient"
)

func topoJSON() string {
	return `{"nodes":[{"name":"node-1","zone":"us-east-1a","ready":true},
	{"name":"node-2","zone":"us-east-1b","ready":false}],
	"agents":[{"id":"node-1-agent-x","nodeName":"node-1","podIP":"10.0.0.1","zone":"us-east-1a"}],
	"timestamp":"2026-01-01T00:00:00Z"}`
}

func TestTopology(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/topology" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(topoJSON()))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	topo, err := c.Topology(context.Background())
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(topo.Nodes) != 2 || len(topo.Agents) != 1 {
		t.Fatalf("unexpected topology: %+v", topo)
	}
	if topo.Nodes[0].Name != "node-1" || !topo.Nodes[0].Ready || topo.Agents[0].PodIP != "10.0.0.1" {
		t.Errorf("field mapping wrong: %+v", topo)
	}
}

func TestTopologyRetriesNonLeader503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "not leader", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(topoJSON()))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.Topology(context.Background()); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestTopologyExhaustsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not leader", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.Topology(context.Background())
	if !errors.Is(err, controllerclient.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}

func TestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "1.4.0", "commit": "abc"})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil || v.Version != "1.4.0" || v.Commit != "abc" {
		t.Fatalf("Version: %+v, %v", v, err)
	}
}

func TestVersionCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "1.6.0", "commit": "abc", "capabilities": []string{"events"},
		})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !v.HasCapability("events") {
		t.Errorf("expected HasCapability(events) true, got capabilities=%v", v.Capabilities)
	}
	if v.HasCapability("nonexistent") {
		t.Error("expected HasCapability(nonexistent) false")
	}
}

func TestVersionNoCapabilitiesField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "1.5.0", "commit": "abc"})
	}))
	defer srv.Close()

	v, err := controllerclient.New(srv.URL, 5*time.Second).Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.HasCapability("events") {
		t.Error("expected HasCapability(events) false when the controller predates capability flags")
	}
}

func diagnoseReq() controllerclient.DiagnoseRequest {
	return controllerclient.DiagnoseRequest{Source: "node-1", Destination: "node-2", Type: "tcp", Plane: "pod"}
}

func TestDiagnosePostsBodyAndTimeoutQuery(t *testing.T) {
	var gotMethod, gotPath, gotTimeout, gotContentType string
	var gotBody controllerclient.DiagnoseRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotTimeout = r.URL.Query().Get("timeout")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"type":"tcp"}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	data, err := c.Diagnose(context.Background(), diagnoseReq(), 45*time.Second)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/diagnostics" {
		t.Errorf("request = %s %s, want POST /api/v1/diagnostics", gotMethod, gotPath)
	}
	if gotTimeout != "45" {
		t.Errorf("timeout query = %q, want %q", gotTimeout, "45")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody != diagnoseReq() {
		t.Errorf("posted body = %+v, want %+v", gotBody, diagnoseReq())
	}
	if string(data) != `{"success":true,"type":"tcp"}` {
		t.Errorf("returned data = %s, want the response body verbatim", data)
	}
}

func TestDiagnoseClampsTimeoutTo120s(t *testing.T) {
	var gotTimeout string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimeout = r.URL.Query().Get("timeout")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.Diagnose(context.Background(), diagnoseReq(), 600*time.Second); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if gotTimeout != "120" {
		t.Errorf("timeout query = %q, want 120 (clamped to the controller's own cap)", gotTimeout)
	}
}

// A sub-second timeout must be rounded UP to 1s, never encoded as the
// literal "timeout=0" -- int(timeout.Seconds()) truncates toward zero, and a
// server told "timeout=0" could read that as "no timeout" rather than "the
// smallest possible one" (task-22-brief.md minor a).
func TestDiagnoseSubSecondTimeoutRoundsUpTo1s(t *testing.T) {
	var gotTimeout string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTimeout = r.URL.Query().Get("timeout")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.Diagnose(context.Background(), diagnoseReq(), 200*time.Millisecond); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if gotTimeout != "1" {
		t.Errorf("timeout query = %q, want %q (rounded up, never truncated to 0)", gotTimeout, "1")
	}
}

// TestDiagnoseNotCappedByClientWideTimeout is I-4 (task-22-brief.md): a
// server that sleeps past New()'s own configured timeout (200ms here) but
// within the timeout given to Diagnose must still succeed -- proving
// Diagnose's own per-request bound (not the shared http.Client.Timeout New()
// configures for Topology/Version) is what actually governs the wait. Before
// the fix, hc.Timeout applied to every request through the single shared
// client regardless of context deadline, silently truncating every dispatch
// to whatever controller.timeout happened to be configured to (commonly
// 10s), no matter what timeout the caller asked Diagnose for or what the
// controller itself had been told to honour.
func TestDiagnoseNotCappedByClientWideTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"type":"tcp"}`))
	}))
	defer srv.Close()

	// New's own timeout (200ms) is far shorter than the server's 500ms sleep
	// -- if Diagnose were still bound by it, this would fail with a client
	// timeout error every time.
	c := controllerclient.New(srv.URL, 200*time.Millisecond)
	data, err := c.Diagnose(context.Background(), diagnoseReq(), 2*time.Second)
	if err != nil {
		t.Fatalf("Diagnose: %v, want success (the polling client's 200ms ceiling must not apply)", err)
	}
	if string(data) != `{"success":true,"type":"tcp"}` {
		t.Errorf("data = %s, want the response body verbatim", data)
	}
}

// TestDiagnoseStatusMapping exercises every plain-text http.Error status
// internal/controller/diagnostics.go can answer with (besides 503, which has
// its own retry test below) and asserts it maps to the matching sentinel.
func TestDiagnoseStatusMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"400 invalid request", http.StatusBadRequest, controllerclient.ErrBadRequest},
		{"404 no agent", http.StatusNotFound, controllerclient.ErrNoAgent},
		{"502 dispatch failed", http.StatusBadGateway, controllerclient.ErrDispatch},
		{"504 check timeout", http.StatusGatewayTimeout, controllerclient.ErrCheckTimeout},
		{"501 external unsupported", http.StatusNotImplemented, controllerclient.ErrExternalUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "induced failure", tc.status)
			}))
			defer srv.Close()

			c := controllerclient.New(srv.URL, 5*time.Second)
			_, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second)
			if !errors.Is(err, tc.want) {
				t.Fatalf("status %d: err = %v, want %v", tc.status, err, tc.want)
			}
		})
	}
}

func TestDiagnoseRetries503ThenErrUnavailable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "not leader", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second)
	if !errors.Is(err, controllerclient.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", calls.Load())
	}
}

// TestDiagnoseDoesNotRetry502Or504 is the most important assertion in this
// file (task-22-brief.md): a dispatch that reached an agent and then failed
// or timed out must not be silently re-run against a cluster the operator is
// diagnosing, unlike 503 above.
func TestDiagnoseDoesNotRetry502Or504(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "induced failure", status)
			}))
			defer srv.Close()

			c := controllerclient.New(srv.URL, 5*time.Second)
			if _, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second); err == nil {
				t.Fatal("expected an error")
			}
			if calls.Load() != 1 {
				t.Errorf("status %d: expected exactly 1 attempt (no retry), got %d", status, calls.Load())
			}
		})
	}
}

// A 501 means the SOURCE agent predates external checks
// (internal/controller/diagnostics.go's capability gate). Retrying cannot make
// an old agent grow the capability, so this must be single-shot exactly like
// 400/404/502/504 -- and it needs its own sentinel so a caller can tell "this
// cluster's agents are too old" apart from "the check failed".
func TestDiagnose501IsExternalUnsupportedAndDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "agent on node node-1 does not support external destinations", http.StatusNotImplemented)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second)
	if !errors.Is(err, controllerclient.ErrExternalUnsupported) {
		t.Fatalf("err = %v, want ErrExternalUnsupported", err)
	}
	if !strings.Contains(err.Error(), "node-1") {
		t.Errorf("err = %v, want the controller's own detail (naming the node) preserved", err)
	}
	if calls.Load() != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no retry)", calls.Load())
	}
	// A 501 is NOT a check timeout: the runner's own timeout/failed split must
	// not misclassify it.
	if errors.Is(err, controllerclient.ErrCheckTimeout) {
		t.Error("501 must not also satisfy ErrCheckTimeout")
	}
}

// DiagnoseRequest's two M4 fields are `omitempty`: a node dispatch puts
// exactly the pre-M4 four-field body on the wire, byte for byte, so a
// controller that predates them sees no change at all.
func TestDiagnoseNodeRequestBodyOmitsExternalFields(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	const want = `{"source":"node-1","destination":"node-2","type":"tcp","plane":"pod"}`
	if gotBody != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

// An external dispatch carries both new fields, and "destination" stays the
// metric-safe NAME rather than the address.
func TestDiagnoseExternalRequestBodyCarriesKindAndAddress(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	req := controllerclient.DiagnoseRequest{
		Source: "node-1", Destination: "api-prod", Type: "tcp", Plane: "pod",
		DestinationKind: "external", DestinationAddress: "api.example.com:443",
	}
	if _, err := c.Diagnose(context.Background(), req, 5*time.Second); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	const want = `{"source":"node-1","destination":"api-prod","type":"tcp","plane":"pod",` +
		`"destinationKind":"external","destinationAddress":"api.example.com:443"}`
	if gotBody != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestDiagnoseBodyCappedAtMaxBodyBytes(t *testing.T) {
	const maxBodyBytes = 4 << 20 // mirrors the unexported client.go constant
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	data, err := c.Diagnose(context.Background(), diagnoseReq(), 5*time.Second)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(data) != maxBodyBytes {
		t.Errorf("len(data) = %d, want %d (capped)", len(data), maxBodyBytes)
	}
}

// ---------------------------------------------------------------------------
// PutExternalChecks (M4 Task 17)
// ---------------------------------------------------------------------------

func externalSpecs() map[string][]controllerclient.ExternalCheckSpec {
	return map[string][]controllerclient.ExternalCheckSpec{
		"node-1-agent-x": {{
			DefinitionID: "def-1",
			Target: controllerclient.ExternalTarget{
				Name: "dns-root", Kind: "host", Address: "8.8.8.8", Port: 53,
			},
			CheckType:  "dns",
			IntervalNs: 30_000_000_000,
			TimeoutNs:  5_000_000_000,
			Params:     json.RawMessage(`{"query":"example.com"}`),
		}},
	}
}

// TestPutExternalChecksBody pins the wire shape internal/controller's
// externalChecksRequest decodes, field name for field name.
func TestPutExternalChecksBody(t *testing.T) {
	var gotBody, gotMethod, gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody, gotMethod, gotPath = string(raw), r.Method, r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"agents":1,"changed":1,"unknown":[]}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	res, err := c.PutExternalChecks(context.Background(), externalSpecs())
	if err != nil {
		t.Fatalf("PutExternalChecks: %v", err)
	}
	if res.Agents != 1 || res.Changed != 1 {
		t.Errorf("result = %+v, want agents=1 changed=1", res)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT", gotMethod)
	}
	if gotPath != "/api/v1/external-checks" {
		t.Errorf("path = %s, want /api/v1/external-checks", gotPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	const want = `{"agents":{"node-1-agent-x":[{"definitionId":"def-1",` +
		`"target":{"name":"dns-root","kind":"host","address":"8.8.8.8","port":53},` +
		`"checkType":"dns","intervalNs":30000000000,"timeoutNs":5000000000,` +
		`"params":{"query":"example.com"}}]}}`
	if gotBody != want {
		t.Errorf("body  = %s\nwant  = %s", gotBody, want)
	}
}

// TestPutExternalChecksNilIsEmptyObject: a nil desired state must serialize as
// {"agents":{}} and never as {"agents":null} — the controller reads the empty
// object as "no agent has any check" and sweeps, which is the honest meaning.
func TestPutExternalChecksNilIsEmptyObject(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"agents":0,"changed":0,"unknown":[]}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	if _, err := c.PutExternalChecks(context.Background(), nil); err != nil {
		t.Fatalf("PutExternalChecks: %v", err)
	}
	if gotBody != `{"agents":{}}` {
		t.Errorf("body = %s, want {\"agents\":{}}", gotBody)
	}
}

// TestPutExternalChecksRetries503 rides the same ladder Topology and Diagnose
// use: a non-leader replica answers 503, and retrying is safe because the PUT
// is absolute and idempotent.
func TestPutExternalChecksRetries503(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "not the leader", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"agents":1,"changed":0,"unknown":["ghost"]}`))
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	res, err := c.PutExternalChecks(context.Background(), externalSpecs())
	if err != nil {
		t.Fatalf("PutExternalChecks: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if len(res.Unknown) != 1 || res.Unknown[0] != "ghost" {
		t.Errorf("unknown = %v, want [ghost]", res.Unknown)
	}
}

func TestPutExternalChecksExhausted503IsErrUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not the leader", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.PutExternalChecks(context.Background(), externalSpecs())
	if !errors.Is(err, controllerclient.ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

// TestPutExternalChecks400IsErrBadRequest: an ineligible checkType is the
// realistic cause, it is not retryable, and — because the controller rejects
// the whole body — nothing was applied.
func TestPutExternalChecks400IsErrBadRequest(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "agent node-1: checkType must be one of tcp, icmp, dns, http", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.PutExternalChecks(context.Background(), externalSpecs())
	if !errors.Is(err, controllerclient.ErrBadRequest) {
		t.Fatalf("err = %v, want ErrBadRequest", err)
	}
	if !strings.Contains(err.Error(), "checkType must be one of") {
		t.Errorf("err = %v, want the controller's plain-text body carried through", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (a 400 is not retryable)", got)
	}
}

func TestPutExternalChecksUnexpectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := controllerclient.New(srv.URL, 5*time.Second)
	_, err := c.PutExternalChecks(context.Background(), externalSpecs())
	if err == nil || !strings.Contains(err.Error(), "unexpected status 500") {
		t.Errorf("err = %v, want an unexpected status 500 error", err)
	}
}
