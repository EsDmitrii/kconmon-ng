package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStrictDecodeSharedFuncs pins the shared create/update decoders: a body
// carrying a field that is not in the schema is a 400 that NAMES the field, not
// a silently dropped value. These request schemas are additionalProperties:false
// in docs/console-api.yaml, so rejecting the unknown field aligns the code with
// the spec. Called directly -- no store or authz needed to prove the contract.
func TestStrictDecodeSharedFuncs(t *testing.T) {
	newReq := func(body string) *http.Request {
		return httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(body))
	}
	cases := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request) bool // returns the decoder's ok
	}{
		{"target", func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeTargetRequest(w, r); return ok }},
		{"alertRule", func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeAlertRuleRequest(w, r); return ok }},
		{"webhook", func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeWebhookRequest(w, r); return ok }},
		{"definition", func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeDefinitionRequest(w, r); return ok }},
		{"schedule", func(w http.ResponseWriter, r *http.Request) bool { _, ok := decodeScheduleRequest(w, r, ""); return ok }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			if ok := tc.call(w, newReq(`{"bogusField":1}`)); ok {
				t.Fatalf("%s: an unknown field must be rejected", tc.name)
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: status %d, want 400: %s", tc.name, w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), "bogusField") {
				t.Errorf("%s: 400 detail must name the field: %s", tc.name, w.Body)
			}
		})
	}
}

// TestStrictDecodeRoutesRejectUnknownField pins the inline-decode mutation
// handlers end-to-end through the router (CSRF and authz included). Every body
// carries all its REQUIRED fields plus one unknown field, so a 400 proves the
// strict decoder fired (a missing-field body could 400 for a different reason).
// runs is the primary case from the task: a misspelled durationNs must not
// silently start a DIFFERENT run.
func TestStrictDecodeRoutesRejectUnknownField(t *testing.T) {
	cases := []struct {
		name  string
		srv   *Server
		path  string
		body  string
		field string
	}{
		{
			// "durationNanos" does not case-match any known field (Go's JSON match
			// is case-insensitive, so "durationNS" would silently bind durationNs);
			// a field with no match at all is exactly what strict decode catches.
			"runs (unknown durationNanos)", newRunsTestServer(t, newFakeRunner(), "operator"), "/api/v1/runs",
			`{"sources":["n1"],"destinations":["n2"],"type":"tcp","plane":"pod","timeoutNs":1000000000,"durationNanos":60000000000}`,
			"durationNanos",
		},
		{
			"maintenance", newM5TestServer(t, "admin", Deps{Maintenance: newFakeMaintenanceStore()}), "/api/v1/maintenance",
			`{"startAt":"2026-01-01T00:00:00Z","endAt":"2026-01-02T00:00:00Z","reason":"x","bogusField":1}`, "bogusField",
		},
		{
			"tokens", newM5TestServer(t, "admin", Deps{Tokens: newFakeTokenStore()}), "/api/v1/tokens",
			`{"name":"t","bogusField":1}`, "bogusField",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(t, tc.srv, http.MethodPost, tc.path, strings.NewReader(tc.body), mutateWithCSRF)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", w.Code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.field) {
				t.Errorf("400 detail must name %q: %s", tc.field, w.Body)
			}
		})
	}
}
