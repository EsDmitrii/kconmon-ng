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
