package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
)

func TestRunsZonePairCreateHappyPath(t *testing.T) {
	runner := newFakeRunner()
	runner.zonePairRuns = []checks.ZonePairRun{
		{ID: "run-a", PairTotal: 400},
		{ID: "run-b", PairTotal: 200},
	}
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp","timeoutNs":2000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}

	var resp zonePairRunsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SourceZone != "zone-a" || resp.DestinationZone != "zone-b" {
		t.Errorf("zones echoed as %q->%q", resp.SourceZone, resp.DestinationZone)
	}
	if resp.PairTotal != 600 {
		t.Errorf("pairTotal = %d, want 600", resp.PairTotal)
	}
	if len(resp.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(resp.Runs))
	}
	if resp.Runs[0].ID != "run-a" || resp.Runs[0].WSTopic != "run:run-a" || resp.Runs[0].Status != "pending" {
		t.Errorf("first run = %+v", resp.Runs[0])
	}

	if len(runner.zonePairSpecs) != 1 {
		t.Fatalf("StartZonePair called %d times, want 1", len(runner.zonePairSpecs))
	}
	spec := runner.zonePairSpecs[0]
	if spec.SourceZone != "zone-a" || spec.DestinationZone != "zone-b" ||
		spec.Type != "tcp" || spec.Plane != "pod" || spec.Timeout != 2*time.Second {
		t.Errorf("spec decoded wrong: %+v", spec)
	}
}

func TestRunsZonePairCreateRequiresRunsCreate(t *testing.T) {
	s := newRunsTestServer(t, newFakeRunner(), "viewer")
	body := `{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
}

func TestRunsZonePairCreateValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"missing source zone":      {`{"sourceZone":"","destinationZone":"zone-b","type":"tcp"}`, http.StatusBadRequest},
		"missing destination zone": {`{"sourceZone":"zone-a","destinationZone":"","type":"tcp"}`, http.StatusBadRequest},
		"control chars in zone":    {`{"sourceZone":"zone\u0000a","destinationZone":"zone-b","type":"tcp"}`, http.StatusBadRequest},
		// plane is deliberately NOT a preset field ("pod" is the only plane); the strict decoder
		// turns it into a clean 400 instead of silently dropping it.
		"unknown field": {`{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp","plane":"pod"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
			if len(runner.zonePairSpecs) != 0 {
				t.Errorf("StartZonePair reached despite the invalid body")
			}
		})
	}
}

func TestRunsZonePairCreateErrorMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"unknown zone":   {fmt.Errorf("checks: %w: no agent reports zone \"ghost\"", checks.ErrUnknownZone), http.StatusUnprocessableEntity},
		"too large":      {fmt.Errorf("checks: %w: narrow the scope", checks.ErrZonePairTooLarge), http.StatusUnprocessableEntity},
		"unknown type":   {fmt.Errorf("checks: plan: %w: %q", checks.ErrUnknownType, "bogus"), http.StatusBadRequest},
		"no pairs":       {fmt.Errorf("checks: plan: %w", checks.ErrNoPairs), http.StatusUnprocessableEntity},
		"opaque failure": {fmt.Errorf("checks: start: create run: boom"), http.StatusBadGateway},
	} {
		t.Run(name, func(t *testing.T) {
			runner := newFakeRunner()
			runner.zonePairErr = tc.err
			s := newRunsTestServer(t, runner, "operator")
			body := `{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp"}`
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(body), mutateWithCSRF)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// A mid-loop Start failure leaves real runs probing the fleet; the error body must say so and name
// them, not pretend nothing happened.
func TestRunsZonePairCreatePartialStartNamesTheStartedRuns(t *testing.T) {
	runner := newFakeRunner()
	runner.zonePairRuns = []checks.ZonePairRun{{ID: "run-started", PairTotal: 400}}
	runner.zonePairErr = fmt.Errorf("checks: zone-pair preset: started 1 of 3 runs, then: boom")
	s := newRunsTestServer(t, runner, "operator")

	body := `{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "run-started") {
		t.Errorf("502 body does not name the started run: %s", w.Body)
	}
}

func TestRunsZonePairCreateWithoutRunnerIs503(t *testing.T) {
	s := newRunsTestServer(t, nil, "operator")
	body := `{"sourceZone":"zone-a","destinationZone":"zone-b","type":"tcp"}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs/zone-pair", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body)
	}
}
