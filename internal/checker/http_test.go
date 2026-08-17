package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestHTTPCheckerSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer srv.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv.URL, Method: "GET", ExpectStatus: 200},
	})

	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
	if result.Type != model.CheckHTTP {
		t.Errorf("expected type HTTP, got %s", result.Type)
	}
}

func TestHTTPCheckerWithBodyPattern(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv.URL, Method: "GET", BodyPattern: regexp.MustCompile(`"status":"ok"`)},
	})

	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
}

func TestHTTPCheckerBodyPatternMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer srv.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv.URL, Method: "GET", BodyPattern: regexp.MustCompile(`"status":"ok"`)},
	})

	result := c.Check(context.Background(), Target{})

	if result.Success {
		t.Error("expected failure for body pattern mismatch")
	}

	details, ok := result.Details.([]model.HTTPDetails)
	if !ok || len(details) == 0 {
		t.Fatal("expected HTTPDetails")
	}
	if !details[0].BodyMismatch {
		t.Error("expected BodyMismatch=true for mismatched body pattern")
	}
	if details[0].StatusCode != http.StatusOK {
		t.Errorf("expected real HTTP status 200, got %d", details[0].StatusCode)
	}
}

func TestHTTPCheckerBodyPatternMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv.URL, Method: "GET", BodyPattern: regexp.MustCompile(`"status":"ok"`)},
	})

	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success for matching body pattern, got error: %s", result.Error)
	}

	details, ok := result.Details.([]model.HTTPDetails)
	if !ok || len(details) == 0 {
		t.Fatal("expected HTTPDetails")
	}
	if details[0].BodyMismatch {
		t.Error("expected BodyMismatch=false for matching body pattern")
	}
}

func TestHTTPCheckerServerDown(t *testing.T) {
	c := NewHTTPChecker(1*time.Second, []HTTPCheckTarget{
		{URL: "http://127.0.0.1:1", Method: "GET"},
	})

	result := c.Check(context.Background(), Target{})

	if result.Success {
		t.Error("expected failure for unreachable server")
	}
}

func TestHTTPCheckerMultipleTargets(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv1.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv2.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv1.URL, Method: "GET"},
		{URL: srv2.URL, Method: "HEAD"},
	})

	result := c.Check(context.Background(), Target{})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}

	details, ok := result.Details.([]model.HTTPDetails)
	if !ok {
		t.Fatal("expected []HTTPDetails")
	}
	if len(details) != 2 {
		t.Errorf("expected 2 details, got %d", len(details))
	}
}

func TestHTTPCheckerPhasedTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{
		{URL: srv.URL},
	})

	result := c.Check(context.Background(), Target{})

	details, ok := result.Details.([]model.HTTPDetails)
	if !ok || len(details) == 0 {
		t.Fatal("expected HTTPDetails")
	}

	d := details[0]
	if d.TotalTime <= 0 {
		t.Error("expected positive total time")
	}
	if d.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", d.StatusCode)
	}
}

/* ── the status is part of the answer ────────────────────────────────────── */

/*
 * expectStatus was parsed from config, validated by the chart's schema, copied into the checker by
 * agent.New — and never read. A target configured to expect 200 reported SUCCESS on a 301, a 404 or
 * a 503: the probe said the endpoint was healthy because something answered at all.
 */
func TestHTTPCheckerAppliesExpectStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serve  int
		expect int
		wantOK bool
	}{
		{"expected status matches", http.StatusOK, http.StatusOK, true},
		{"expected status does not match", http.StatusMovedPermanently, http.StatusOK, false},
		{"a 503 where 200 was expected", http.StatusServiceUnavailable, http.StatusOK, false},
		{"no expectation: a 2xx passes", http.StatusNoContent, 0, true},
		{"no expectation: a 4xx fails", http.StatusNotFound, 0, false},
		{"an expectation MAY be a 4xx", http.StatusNotFound, http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.serve)
			}))
			defer srv.Close()

			c := NewHTTPChecker(time.Second, []HTTPCheckTarget{{URL: srv.URL, Method: http.MethodGet, ExpectStatus: tc.expect}})
			result := c.Check(context.Background(), Target{})

			if result.Success != tc.wantOK {
				t.Errorf("serve %d, expect %d: Success = %v (%s), want %v",
					tc.serve, tc.expect, result.Success, result.Error, tc.wantOK)
			}
		})
	}
}

/*
 * An https target with a certificate the client cannot verify must FAIL.
 *
 * One shared client set InsecureSkipVerify for every target, with no knob and nothing outside a code
 * comment saying so: an expired certificate, a certificate for another hostname, or an interceptor's
 * CA all handshaked, answered 200, and counted as a success. The check could not fail on the very
 * condition an operator adds an https check to notice.
 */
func TestHTTPCheckerVerifiesCertificatesByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := HTTPCheckTarget{URL: srv.URL, ExpectStatus: http.StatusOK}
	res := NewHTTPChecker(5*time.Second, []HTTPCheckTarget{target}).
		Check(context.Background(), Target{})
	if res.Success {
		t.Errorf("an untrusted certificate was reported healthy: %+v", res)
	}

	// And the per-target opt-out still works, for the endpoint that really does front its own CA.
	target.InsecureSkipVerify = true
	res = NewHTTPChecker(5*time.Second, []HTTPCheckTarget{target}).
		Check(context.Background(), Target{})
	if !res.Success {
		t.Errorf("insecureSkipVerify did not opt the target out: %+v", res)
	}
}
