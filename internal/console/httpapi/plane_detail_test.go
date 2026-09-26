package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The refusal's title already says the plane is invalid, so the detail is the rule alone.
func TestPlaneRefusalDetailIsTheRule(t *testing.T) {
	const rule = `plane must be "pod": agents probe the pod network only`

	runs := newRunsTestServer(t, newFakeRunner(), "operator")
	w := doRequest(t, runs, http.MethodPost, "/api/v1/runs", strings.NewReader(
		`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"host","timeoutNs":1000000000}`), mutateWithCSRF)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/v1/runs with plane host = %d, want 400: %s", w.Code, w.Body)
	}
	if got := problemDetail(t, w.Body.Bytes()); got != rule {
		t.Errorf("POST /api/v1/runs detail = %q, want %q", got, rule)
	}

	defs := newOperatorChecksServer(t, newFakeChecksStore(), nil)
	body := strings.Replace(validDefinitionBody, `"plane":"pod"`, `"plane":"host"`, 1)
	w = doRequest(t, defs, http.MethodPost, "/api/v1/checks", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /api/v1/checks with plane host = %d, want 422: %s", w.Code, w.Body)
	}
	if got, want := problemDetail(t, w.Body.Bytes()), "definition: "+rule; got != want {
		t.Errorf("POST /api/v1/checks detail = %q, want %q", got, want)
	}
}
