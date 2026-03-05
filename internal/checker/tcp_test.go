package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

func TestTCPCheckerSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)

	c := NewTCPChecker(5 * time.Second)
	result := c.Check(context.Background(), Target{
		PodIP: host,
		Port:  port,
	})

	if !result.Success {
		t.Errorf("expected success, got error: %s", result.Error)
	}
	if result.Type != model.CheckTCP {
		t.Errorf("expected type TCP, got %s", result.Type)
	}

	details, ok := result.Details.(*model.TCPDetails)
	if !ok {
		t.Fatal("expected TCPDetails")
	}
	if details.ConnectTime <= 0 {
		t.Error("expected positive connect time")
	}
	if details.TotalTime <= 0 {
		t.Error("expected positive total time")
	}
}

func TestTCPCheckerConnectionRefused(t *testing.T) {
	c := NewTCPChecker(1 * time.Second)
	result := c.Check(context.Background(), Target{
		PodIP: "127.0.0.1",
		Port:  1, // unlikely to be open
	})

	if result.Success {
		t.Error("expected failure for connection refused")
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestTCPCheckerTimeout(t *testing.T) {
	c := NewTCPChecker(100 * time.Millisecond)

	// 192.0.2.1 is TEST-NET-1, should timeout
	result := c.Check(context.Background(), Target{
		PodIP: "192.0.2.1",
		Port:  80,
	})

	if result.Success {
		t.Error("expected failure for timeout")
	}
}

func TestTCPCheckerNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	host, port := parseHostPort(t, srv.URL)

	c := NewTCPChecker(5 * time.Second)
	result := c.Check(context.Background(), Target{
		PodIP: host,
		Port:  port,
	})

	if result.Success {
		t.Error("expected failure for non-200 status")
	}
	if result.Error == "" {
		t.Error("expected error message for non-200")
	}
}

func parseHostPort(t *testing.T, rawURL string) (host string, port int) {
	t.Helper()
	// rawURL is like "http://127.0.0.1:12345"
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == ':' {
			host = rawURL[7:i] // skip "http://"
			for _, c := range rawURL[i+1:] {
				port = port*10 + int(c-'0')
			}
			break
		}
	}
	return host, port
}
