package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/EsDmitrii/kconmon-ng/internal/console/cache"
	"github.com/EsDmitrii/kconmon-ng/internal/console/checks"
	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/ws"
)

/*
runs_interval_test.go covers POST /api/v1/runs's `sampleIntervalNs`: the field that made the run's
cadence something an operator can actually set, rather than a number three surfaces each guessed at
differently.
*/

// realRunner is the same construction TestRunsCreateDurationOutOfRangeReturns422NamingTheBound uses:
// the validation under test lives in checks.Runner.Start, and fakeRunner would accept anything.
func realRunner(t *testing.T) *checks.Runner {
	t.Helper()
	m := metrics.New("kconmon_ng_test", prometheus.NewRegistry())
	bus := cache.NewInProcessBus()
	return checks.NewRunner(nil, ws.NewHub(bus, m), bus, checks.NewMemoryStore(), m)
}

// sampleIntervalNs reaches the spec verbatim, and an absent one leaves the derived behaviour alone.
func TestRunsCreateCarriesSampleIntervalIntoTheSpec(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		wantInterval time.Duration
	}{
		{
			name:         "absent sampleIntervalNs asks for nothing",
			body:         `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000}`,
			wantInterval: 0,
		},
		{
			name:         "1s cadence",
			body:         `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"sampleIntervalNs":1000000000}`,
			wantInterval: time.Second,
		},
		{
			name:         "15s cadence",
			body:         `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"sampleIntervalNs":15000000000}`,
			wantInterval: 15 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			s := newRunsTestServer(t, runner, "operator")
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
			}
			if len(runner.started) != 1 {
				t.Fatalf("Start called %d times, want 1", len(runner.started))
			}
			if got := runner.started[0].RequestedSampleInterval; got != tc.wantInterval {
				t.Errorf("spec.RequestedSampleInterval = %s, want %s", got, tc.wantInterval)
			}
		})
	}
}

// The RANGE half of the contract: out of [1s, durationNs] is a 422 naming the bound, exactly as an
// out-of-range durationNs is.
func TestRunsCreateSampleIntervalOutOfRangeReturns422(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "under the 1s floor",
			body: `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"sampleIntervalNs":500000}`,
			want: "1s",
		},
		{
			name: "longer than the run itself",
			body: `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":60000000000,"sampleIntervalNs":900000000000}`,
			want: "1m0s",
		},
		{
			name: "a cadence on an instant run",
			body: `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","sampleIntervalNs":5000000000}`,
			want: "durationNs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRunsTestServer(t, realRunner(t), "operator")
			w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422: %s", w.Code, w.Body)
			}
			if body := w.Body.String(); !strings.Contains(body, tc.want) {
				t.Errorf("body = %s, want the bound %q named in the detail", body, tc.want)
			}
		})
	}
}

// A cadence the fan-out cannot keep is ACCEPTED, and the response reports the two numbers apart:
// what was asked for, what will run, and why. This is the below-floor semantics in one assertion.
func TestRunsCreateBelowFloorSampleIntervalIsAcceptedAndReported(t *testing.T) {
	s := newRunsTestServer(t, realRunner(t), "operator")
	// mtr, five sources onto one external address: one trace is budgeted at 90s, so a 1s cadence is
	// physically impossible and the planner stretches it instead of refusing.
	body := `{"sources":["n1","n2","n3","n4","n5"],"type":"mtr","plane":"pod","destinationKind":"adhoc",` +
		`"destinationAddress":"10.0.0.1","durationNs":900000000000,"sampleIntervalNs":1000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (a cadence that cannot be kept is planned around, never refused): %s", w.Code, w.Body)
	}

	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RequestedSampleIntervalNs != int64(time.Second) {
		t.Errorf("requestedSampleIntervalNs = %d, want the 1s that was asked for", resp.RequestedSampleIntervalNs)
	}
	if resp.SampleIntervalAdjusted != checks.IntervalStretched {
		t.Errorf("sampleIntervalAdjusted = %q, want %q", resp.SampleIntervalAdjusted, checks.IntervalStretched)
	}
	if resp.PlannedSampleIntervalNs <= int64(time.Second) {
		t.Errorf("plannedSampleIntervalNs = %d, want a stretched cadence above the request", resp.PlannedSampleIntervalNs)
	}
	if resp.PlannedSamplesPerPair < 1 {
		t.Errorf("plannedSamplesPerPair = %d, want at least one honest pass", resp.PlannedSamplesPerPair)
	}
}

// The 500-samples-per-pair ceiling binds a fast request over a long run, and SAYS SO.
func TestRunsCreateSampleIntervalCapIsReported(t *testing.T) {
	s := newRunsTestServer(t, realRunner(t), "operator")
	// 1s over 24h would be 86 400 samples for one pair; the cap widens it to 24h/500 = 172.8s.
	body := `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod",` +
		`"durationNs":86400000000000,"sampleIntervalNs":1000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}

	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SampleIntervalAdjusted != checks.IntervalCapped {
		t.Errorf("sampleIntervalAdjusted = %q, want %q", resp.SampleIntervalAdjusted, checks.IntervalCapped)
	}
	if want := int64(24 * time.Hour / checks.MaxSamplesPerPair); resp.PlannedSampleIntervalNs != want {
		t.Errorf("plannedSampleIntervalNs = %d, want %d", resp.PlannedSampleIntervalNs, want)
	}
	if resp.PlannedSamplesPerPair != checks.MaxSamplesPerPair {
		t.Errorf("plannedSamplesPerPair = %d, want the cap %d", resp.PlannedSamplesPerPair, checks.MaxSamplesPerPair)
	}
}

// A run that got exactly what it asked for reports NOTHING extra: `omitempty` drops both fields, so
// a caller cannot read an adjustment where none happened.
func TestRunsCreateHonouredSampleIntervalReportsNoAdjustment(t *testing.T) {
	s := newRunsTestServer(t, realRunner(t), "operator")
	body := `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod",` +
		`"durationNs":300000000000,"sampleIntervalNs":15000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, "sampleIntervalAdjusted") {
		t.Errorf("body = %s, want no sampleIntervalAdjusted key at all", body)
	}

	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PlannedSampleIntervalNs != int64(15*time.Second) {
		t.Errorf("plannedSampleIntervalNs = %d, want the 15s that was asked for", resp.PlannedSampleIntervalNs)
	}
	if resp.PlannedSamplesPerPair != 20 {
		t.Errorf("plannedSamplesPerPair = %d, want 20 (5m / 15s)", resp.PlannedSamplesPerPair)
	}
}

// The strict decoder still refuses a MISSPELLED cadence field rather than silently starting a run
// on a different one -- the guarantee the new field must not have loosened.
//
// Not covered, because encoding/json does not: a CASE variant ("sampleIntervalNS") matches the tag
// case-insensitively and is accepted. That is the decoder's own long-standing behaviour, true of
// durationNs before this field existed, and asserting otherwise here would pin a promise the
// package has never kept.
func TestRunsCreateUnknownSampleIntervalFieldStillReturns400(t *testing.T) {
	for _, body := range []string{
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"sampleInterval":1000000000}`,
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"intervalNs":1000000000}`,
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000,"sample_interval_ns":1000000000}`,
	} {
		runner := newFakeRunner()
		s := newRunsTestServer(t, runner, "operator")
		w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for %s: %s", w.Code, body, w.Body)
		}
		if len(runner.started) != 0 {
			t.Errorf("Start was called for a body with an unknown field: %s", body)
		}
	}
}

// The absent case is byte-identical to what it was: no request, no adjustment, and the plan is the
// derived cadence exactly.
func TestRunsCreateAbsentSampleIntervalIsUnchanged(t *testing.T) {
	s := newRunsTestServer(t, realRunner(t), "operator")
	body := `{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","durationNs":300000000000}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/runs", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body)
	}
	if got := w.Body.String(); strings.Contains(got, "requestedSampleIntervalNs") || strings.Contains(got, "sampleIntervalAdjusted") {
		t.Errorf("body = %s, want neither request-describing key present", got)
	}

	var resp runCreateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if want := int64(checks.MinSampleInterval); resp.PlannedSampleIntervalNs != want {
		t.Errorf("plannedSampleIntervalNs = %d, want the derived %d", resp.PlannedSampleIntervalNs, want)
	}
}
