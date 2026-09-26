package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

func seedTargetDefinition(t *testing.T, checks *fakeChecksStore, in store.DefinitionInput) { //nolint:gocritic // hugeParam: test helper
	t.Helper()
	checks.mu.Lock()
	checks.targets[in.DestinationTargetID] = true
	checks.mu.Unlock()
	if _, err := checks.CreateDefinition(context.Background(), in); err != nil {
		t.Fatalf("seed definition %q: %v", in.Name, err)
	}
}

func httpDefinitionOn(name, targetID string, enabled bool) store.DefinitionInput {
	return store.DefinitionInput{
		Name: name, SourceSelection: "all", DestinationKind: "target", DestinationTargetID: targetID,
		CheckType: "http", Plane: "pod", Enabled: enabled,
	}
}

/*
The write-time guard covers the target side too: a PUT that turns a url target into a host one
would leave every http definition pointing at it unrunnable (the reconciler skips it, the console
still lists it enabled), so it is refused with the definitions named, and the target stays as it
was. Edits that keep the definitions runnable, and definitions that could not run before the edit
either, do not block it.
*/
func TestTargetsUpdateRefusesAnEditThatBreaksAReferencingDefinition(t *testing.T) {
	fake := newFakeTargetService()
	checks := newFakeChecksStore()
	s := newTargetsTestServer(t, "operator", fake, Deps{Definitions: checks})

	w := doRequest(t, s, http.MethodPost, "/api/v1/targets",
		strings.NewReader(`{"name":"portal","kind":"url","address":"https://svc.example.com/health"}`), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", w.Code, w.Body)
	}
	var created targetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seedTargetDefinition(t, checks, httpDefinitionOn("portal-http", created.ID, true))
	// Disabled rows are held to the same rule the definition routes apply: re-enabling one must not
	// meet a 422 the target edit caused.
	seedTargetDefinition(t, checks, httpDefinitionOn("portal-http-paused", created.ID, false))

	w = doRequest(t, s, http.MethodPut, "/api/v1/targets/"+created.ID,
		strings.NewReader(`{"name":"portal","kind":"host","address":"svc.example.com:443"}`), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("url -> host with an http definition on it = %d, want 422: %s", w.Code, w.Body)
	}
	detail := problemDetail(t, w.Body.Bytes())
	for _, want := range []string{`"portal-http"`, `"portal-http-paused"`, "http target address"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not name %s", detail, want)
		}
	}
	if got, _ := fake.GetTarget(context.Background(), created.ID); got.Kind != "url" {
		t.Errorf("the target was changed anyway: %+v", got)
	}

	for _, body := range []string{
		`{"name":"portal","kind":"url","address":"https://svc.example.com/health","labels":{"team":"edge"}}`,
		`{"name":"portal","kind":"url","address":"https://svc.example.com/ready"}`,
		`{"name":"portal-renamed","kind":"url","address":"https://svc.example.com/ready"}`,
	} {
		if w := doRequest(t, s, http.MethodPut, "/api/v1/targets/"+created.ID, strings.NewReader(body), mutateWithCSRF); w.Code != http.StatusOK {
			t.Errorf("PUT %s = %d, want 200: %s", body, w.Code, w.Body)
		}
	}
}

// A definition that could not run before the edit is not the edit's doing, and must not pin the
// target: rows written before the definition guard existed would otherwise block every change.
func TestTargetsUpdateIgnoresDefinitionsThatAlreadyCouldNotRun(t *testing.T) {
	fake := newFakeTargetService()
	checks := newFakeChecksStore()
	s := newTargetsTestServer(t, "operator", fake, Deps{Definitions: checks})

	target, err := fake.CreateTarget(context.Background(), store.TargetInput{Name: "edge-lb", Kind: "host", Address: "10.0.0.7:443"})
	if err != nil {
		t.Fatal(err)
	}
	seedTargetDefinition(t, checks, httpDefinitionOn("legacy-http", target.ID, true))

	w := doRequest(t, s, http.MethodPut, "/api/v1/targets/"+target.ID,
		strings.NewReader(`{"name":"edge-lb","kind":"host","address":"10.0.0.8:443"}`), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("host -> host with an already unrunnable definition = %d, want 200: %s", w.Code, w.Body)
	}
}

// The import's update branch applies the same rule, in dry run and for real; a definition the same
// bundle replaces is judged as the bundle writes it, so a consistent bundle still applies.
func TestImportTargetUpdateRefusesAnEditThatBreaksAReferencingDefinition(t *testing.T) {
	fx := newExportServer(t, "admin")
	target, err := fx.targets.CreateTarget(context.Background(), store.TargetInput{
		Name: "portal", Kind: "url", Address: "https://svc.example.com/health",
	})
	if err != nil {
		t.Fatal(err)
	}
	seedTargetDefinition(t, fx.checks, httpDefinitionOn("portal-http", target.ID, true))

	const bundleTargetID = "11111111-1111-4111-8111-111111111111"
	hostTarget := targetResponse{ID: bundleTargetID, Name: "portal", Kind: "host", Address: "svc.example.com:443"}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: &exportBundle{
			Version: exportBundleVersion, Targets: []targetResponse{hostTarget},
		}})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.Targets.Updated != 0 || len(res.Targets.Errors) != 1 ||
			!strings.Contains(res.Targets.Errors[0].Reason, `"portal-http"`) {
			t.Errorf("dryRun=%v: targets = %+v, want the update refused naming portal-http", dryRun, res.Targets)
		}
	}
	if got, _ := fx.targets.GetTarget(context.Background(), target.ID); got.Kind != "url" {
		t.Errorf("the target was changed anyway: %+v", got)
	}

	// The same bundle, now also re-typing the definition to tcp: both apply.
	tcp := definitionResponse{
		ID: "22222222-2222-4222-8222-222222222222", Name: "portal-http", SourceSelection: "all",
		DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "tcp", Plane: "pod",
		Params: json.RawMessage(`{}`), Enabled: true,
	}
	for _, dryRun := range []bool{true, false} {
		code, res := doImport(t, &fx, importRequest{DryRun: dryRun, Bundle: &exportBundle{
			Version: exportBundleVersion, Targets: []targetResponse{hostTarget}, CheckDefinitions: []definitionResponse{tcp},
		}})
		if code != http.StatusOK {
			t.Fatalf("dryRun=%v: import = %d, want 200", dryRun, code)
		}
		if res.Targets.Updated != 1 || res.CheckDefinitions.Updated != 1 ||
			len(res.Targets.Errors) != 0 || len(res.CheckDefinitions.Errors) != 0 {
			t.Errorf("dryRun=%v: targets %+v, definitions %+v, want both updated", dryRun, res.Targets, res.CheckDefinitions)
		}
	}
	if got, _ := fx.targets.GetTarget(context.Background(), target.ID); got.Kind != "host" {
		t.Errorf("the consistent bundle did not apply: %+v", got)
	}
}

// POST /api/v1/checks refuses a definition no agent could run; the import is the other way to write
// one, and it refuses the same rows, judged against the targets the same bundle writes.
func TestImportRefusesADefinitionNoAgentCanRun(t *testing.T) {
	const bundleTargetID = "33333333-3333-4333-8333-333333333333"
	bundle := func() *exportBundle {
		return &exportBundle{
			Version: exportBundleVersion,
			Targets: []targetResponse{{ID: bundleTargetID, Name: "api", Kind: "host", Address: "api.example.com:443"}},
			CheckDefinitions: []definitionResponse{
				{
					ID: "44444444-4444-4444-8444-444444444444", Name: "api-http", SourceSelection: "all",
					DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "http", Plane: "pod",
					Params: json.RawMessage(`{}`), Enabled: true,
				},
				{
					ID: "55555555-5555-4555-8555-555555555555", Name: "resolve", SourceSelection: "all",
					DestinationKind: "adhoc", DestinationAddress: "10.0.0.10", CheckType: "dns", Plane: "pod",
					Params: json.RawMessage(`{}`), Enabled: true,
				},
				{
					ID: "66666666-6666-4666-8666-666666666666", Name: "api-tcp", SourceSelection: "all",
					DestinationKind: "target", DestinationTargetID: bundleTargetID, CheckType: "tcp", Plane: "pod",
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
			t.Fatalf("dryRun=%v: definitions = %+v, want api-tcp created and the other two refused", dryRun, res.CheckDefinitions)
		}
		for _, e := range res.CheckDefinitions.Errors {
			if e.Name == "api-tcp" || !strings.Contains(e.Reason, "no agent could run this check") {
				t.Errorf("dryRun=%v: error %+v, want only api-http and resolve refused as unrunnable", dryRun, e)
			}
		}
		if !dryRun {
			fx.checks.mu.Lock()
			_, httpWritten := fx.checks.defNames["api-http"]
			fx.checks.mu.Unlock()
			if httpWritten {
				t.Error("the unrunnable definition was written")
			}
		}
	}
}
