package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

func createDefinition(t *testing.T, s *Server, name, destKind, checkType string) (int, string, string) {
	t.Helper()
	extra := `,"destinationAddress":"10.0.0.7:53"`
	switch {
	case destKind == "node":
		extra = ""
	case checkType == "http":
		extra = `,"destinationAddress":"https://svc.example.com/health"`
	case checkType == "dns":
		extra += `,"params":{"query":"example.com"}`
	}
	body := fmt.Sprintf(`{"name":%q,"sourceSelection":"one-per-zone","destinationKind":%q%s,"checkType":%q,"plane":"pod"}`,
		name, destKind, extra, checkType)
	w := doRequest(t, s, http.MethodPost, "/api/v1/checks", strings.NewReader(body), mutateWithCSRF)
	var def definitionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &def) //nolint:errcheck // a refusal has no id to decode
	return w.Code, def.ID, w.Body.String()
}

// docs/console/scheduled-checks.md promises that udp toward a target or ad-hoc address is refused
// at write time: the reconciler skips it as a continuous check and the agent refuses it in a run.
// mtr stays: it runs toward an external destination in one-off runs.
func TestChecksCreateRefusesUDPTowardAnExternalDestination(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)

	code, _, body := createDefinition(t, s, "edge-udp", "adhoc", "udp")
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "check definition cannot run") ||
		!strings.Contains(body, "check type udp probes kconmon nodes only") {
		t.Fatalf("udp adhoc = %d %s, want 422 check definition cannot run naming udp", code, body)
	}
	if code, _, body = createDefinition(t, s, "mesh-udp", "node", "udp"); code != http.StatusCreated {
		t.Errorf("udp node = %d %s, want 201", code, body)
	}
	if code, _, body = createDefinition(t, s, "edge-mtr", "adhoc", "mtr"); code != http.StatusCreated {
		t.Errorf("mtr adhoc = %d %s, want 201", code, body)
	}
}

// The import is the other way to write a definition; a 2.4 bundle carrying a udp definition toward
// an external destination imports everything else and reports that one item.
func TestImportReportsUDPTowardAnExternalDestinationPerItem(t *testing.T) {
	const bundleTargetID = "33333333-3333-4333-8333-333333333333"
	bundle := func() *exportBundle {
		return &exportBundle{
			Version: exportBundleVersion,
			Targets: []targetResponse{{ID: bundleTargetID, Name: "api", Kind: "host", Address: "api.example.com:443"}},
			CheckDefinitions: []definitionResponse{
				{
					ID: "44444444-4444-4444-8444-444444444444", Name: "api-udp", SourceSelection: "all",
					DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "udp", Plane: "pod",
					Params: json.RawMessage(`{}`), Enabled: true,
				},
				{
					ID: "55555555-5555-4555-8555-555555555555", Name: "edge-udp", SourceSelection: "all",
					DestinationKind: "adhoc", DestinationAddress: "10.0.0.10:9000", CheckType: "udp", Plane: "pod",
					Params: json.RawMessage(`{}`), Enabled: true,
				},
				{
					ID: "66666666-6666-4666-8666-666666666666", Name: "api-mtr", SourceSelection: "all",
					DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "mtr", Plane: "pod",
					Params: json.RawMessage(`{}`), Enabled: true,
				},
			},
		}
	}
	for _, dryRun := range []bool{true, false} {
		fx := newExportServer(t, "admin")
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: bundle()})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.CheckDefinitions.Created != 1 || len(res.CheckDefinitions.Errors) != 2 {
			t.Fatalf("dryRun=%v: definitions = %+v, want api-mtr created and both udp items refused", dryRun, res.CheckDefinitions)
		}
		for _, e := range res.CheckDefinitions.Errors {
			if e.Name == "api-mtr" || !strings.Contains(e.Reason, "udp probes kconmon nodes only") {
				t.Errorf("dryRun=%v: error %+v, want only the udp items refused, naming why", dryRun, e)
			}
		}
	}
}

func scheduleBody(defID, kind string) string {
	switch kind {
	case "once":
		return fmt.Sprintf(`{"definitionId":%q,"kind":"once","runAt":%q,"enabled":true}`,
			defID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case "interval":
		return intervalScheduleBody(defID, time.Minute, true)
	default:
		return fmt.Sprintf(`{"definitionId":%q,"kind":%q,"enabled":true}`, defID, kind)
	}
}

// A schedule must be able to run its definition: once and interval fire runs, which the agent takes
// toward an external destination for tcp, icmp and mtr only; continuous becomes an agent-side
// external check, which serves tcp, icmp, dns and http.
func TestSchedulesRefuseAKindThatCannotRunTheDefinition(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	defs := map[string]string{}
	for _, c := range []struct{ name, destKind, checkType string }{
		{"edge-tcp", "adhoc", "tcp"}, {"edge-mtr", "adhoc", "mtr"}, {"edge-dns", "adhoc", "dns"},
		{"edge-http", "adhoc", "http"}, {"mesh-dns", "node", "dns"}, {"mesh-mtr", "node", "mtr"},
	} {
		code, id, body := createDefinition(t, s, c.name, c.destKind, c.checkType)
		if code != http.StatusCreated {
			t.Fatalf("seed %s = %d %s", c.name, code, body)
		}
		defs[c.name] = id
	}

	for _, tc := range []struct {
		def, kind string
		want      int
	}{
		{"edge-tcp", "once", http.StatusCreated},
		{"edge-tcp", "interval", http.StatusCreated},
		{"edge-tcp", "continuous", http.StatusCreated},
		{"edge-mtr", "once", http.StatusCreated},
		{"edge-mtr", "interval", http.StatusCreated},
		{"edge-mtr", "continuous", http.StatusUnprocessableEntity},
		{"edge-dns", "once", http.StatusUnprocessableEntity},
		{"edge-dns", "interval", http.StatusUnprocessableEntity},
		{"edge-dns", "continuous", http.StatusCreated},
		{"edge-http", "interval", http.StatusUnprocessableEntity},
		{"edge-http", "continuous", http.StatusCreated},
		{"mesh-dns", "interval", http.StatusCreated},
		{"mesh-mtr", "once", http.StatusCreated},
	} {
		w := doRequest(t, s, http.MethodPost, "/api/v1/schedules", strings.NewReader(scheduleBody(defs[tc.def], tc.kind)), mutateWithCSRF)
		if w.Code != tc.want {
			t.Errorf("%s %s schedule = %d, want %d: %s", tc.def, tc.kind, w.Code, tc.want, w.Body)
			continue
		}
		if tc.want == http.StatusUnprocessableEntity {
			b := w.Body.String()
			if !strings.Contains(b, "invalid schedule") || !strings.Contains(b, "check type") {
				t.Errorf("%s %s refusal = %s, want invalid schedule naming the check type", tc.def, tc.kind, b)
			}
			// The web form places a detail by the field it names; these must not land on the interval box.
			if strings.Contains(strings.ToLower(b), "interval") {
				t.Errorf("%s %s refusal = %s, must not mention interval (the form would pin it to that field)", tc.def, tc.kind, b)
			}
		}
	}

	// PUT is held to the same rule.
	w := doRequest(t, s, http.MethodPost, "/api/v1/schedules", strings.NewReader(scheduleBody(defs["edge-mtr"], "interval")), mutateWithCSRF)
	var sched scheduleResponse
	if err := json.Unmarshal(w.Body.Bytes(), &sched); err != nil || sched.ID == "" {
		t.Fatalf("seed mtr interval schedule: %d %s", w.Code, w.Body)
	}
	w = doRequest(t, s, http.MethodPut, "/api/v1/schedules/"+sched.ID, strings.NewReader(scheduleBody(defs["edge-mtr"], "continuous")), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("PUT mtr interval -> continuous = %d, want 422: %s", w.Code, w.Body)
	}
}

// And from the definition side: an edit that leaves one of its own schedules unable to run it is
// refused; an edit its schedules can still run goes through.
func TestChecksUpdateRefusesAnEditItsSchedulesCannotRun(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	code, id, body := createDefinition(t, s, "edge-tcp", "adhoc", "tcp")
	if code != http.StatusCreated {
		t.Fatalf("seed = %d %s", code, body)
	}
	if w := doRequest(t, s, http.MethodPost, "/api/v1/schedules", strings.NewReader(scheduleBody(id, "interval")), mutateWithCSRF); w.Code != http.StatusCreated {
		t.Fatalf("seed schedule = %d %s", w.Code, w.Body)
	}

	put := func(checkType string) (int, string) {
		b := fmt.Sprintf(`{"name":"edge-tcp","sourceSelection":"one-per-zone","destinationKind":"adhoc",`+
			`"destinationAddress":"10.0.0.7:53","checkType":%q,"plane":"pod"}`, checkType)
		if checkType == "dns" {
			b = strings.Replace(b, `"plane":"pod"`, `"plane":"pod","params":{"query":"example.com"}`, 1)
		}
		w := doRequest(t, s, http.MethodPut, "/api/v1/checks/"+id, strings.NewReader(b), mutateWithCSRF)
		return w.Code, w.Body.String()
	}
	if code, body := put("dns"); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "its interval schedule could not run the edited check") {
		t.Fatalf("tcp -> dns with an interval schedule = %d %s, want 422 naming the schedule", code, body)
	}
	if got := st.defs[id].CheckType; got != "tcp" {
		t.Errorf("stored check type = %q after a refused edit, want tcp", got)
	}
	if code, body := put("mtr"); code != http.StatusOK {
		t.Errorf("tcp -> mtr with an interval schedule = %d %s, want 200", code, body)
	}
}

// A schedule or definition the guards refuse (a row saved before them, or through an import) can
// still be paused: an update to enabled:false runs nothing, so it is not judged. Enabling it again
// is, and so is a new one, disabled or not.
func TestARefusedScheduleOrDefinitionCanStillBePaused(t *testing.T) {
	st := newFakeChecksStore()
	s := newOperatorChecksServer(t, st, nil)
	ctx := context.Background()

	legacyUDP, err := st.CreateDefinition(ctx, store.DefinitionInput{
		Name: "edge-udp", SourceSelection: "all", DestinationKind: "adhoc", DestinationAddress: "10.0.0.7:9000",
		CheckType: "udp", Plane: "pod", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	code, dnsID, body := createDefinition(t, s, "edge-dns", "adhoc", "dns")
	if code != http.StatusCreated {
		t.Fatalf("seed edge-dns = %d %s", code, body)
	}
	legacyInterval, err := st.CreateSchedule(ctx, store.ScheduleInput{
		DefinitionID: dnsID, Kind: "interval", IntervalNs: int64(time.Minute), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	putDef := func(enabled bool) int {
		b := fmt.Sprintf(`{"name":"edge-udp","sourceSelection":"all","destinationKind":"adhoc",`+
			`"destinationAddress":"10.0.0.7:9000","checkType":"udp","plane":"pod","enabled":%t}`, enabled)
		return doRequest(t, s, http.MethodPut, "/api/v1/checks/"+legacyUDP.ID, strings.NewReader(b), mutateWithCSRF).Code
	}
	if got := putDef(false); got != http.StatusOK || st.defs[legacyUDP.ID].Enabled {
		t.Errorf("pausing a udp->adhoc definition = %d (enabled=%v), want 200 and paused", got, st.defs[legacyUDP.ID].Enabled)
	}
	if got := putDef(true); got != http.StatusUnprocessableEntity {
		t.Errorf("re-enabling a udp->adhoc definition = %d, want 422", got)
	}

	putSched := func(enabled bool) int {
		return doRequest(t, s, http.MethodPut, "/api/v1/schedules/"+legacyInterval.ID,
			strings.NewReader(intervalScheduleBody(dnsID, time.Minute, enabled)), mutateWithCSRF).Code
	}
	if got := putSched(false); got != http.StatusOK || st.scheds[legacyInterval.ID].Enabled {
		t.Errorf("pausing a dns interval schedule toward adhoc = %d (enabled=%v), want 200 and paused",
			got, st.scheds[legacyInterval.ID].Enabled)
	}
	if got := putSched(true); got != http.StatusUnprocessableEntity {
		t.Errorf("re-enabling a dns interval schedule toward adhoc = %d, want 422", got)
	}
	if w := doRequest(t, s, http.MethodPost, "/api/v1/schedules",
		strings.NewReader(intervalScheduleBody(dnsID, time.Minute, false)), mutateWithCSRF); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("creating that schedule disabled = %d %s, want 422", w.Code, w.Body)
	}
}
