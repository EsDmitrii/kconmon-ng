package httpapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testBodyTimeout = 200 * time.Millisecond

/*
The body-phase deadline sits on the raw connection, and net/http's background read of a BODILESS
request shares that connection: at the deadline the background read timed out and cancelled the
request context, so a GET that ran longer than 30 s (a big export, a slow upstream) died with
"context canceled" although it never had a body to wait for.
*/
func TestBodilessRequestOutlivesTheBodyDeadline(t *testing.T) {
	ctxErr := make(chan error, 1)
	srv := httptest.NewServer(limitBodyFor(testBodyTimeout, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * testBodyTimeout):
		}
		ctxErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	})))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if err := <-ctxErr; err != nil {
		t.Errorf("a bodiless GET had its context cancelled by the body deadline: %v", err)
	}
}

// The bound itself stays: a client that announces a body and stops sending it is cut off.
func TestStalledBodyIsCutOffAtTheDeadline(t *testing.T) {
	readErr := make(chan error, 1)
	srv := httptest.NewServer(limitBodyFor(testBodyTimeout, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		readErr <- err
	})))
	defer srv.Close()

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\npartial"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Error("the stalled body read ended without an error")
		}
	case <-time.After(10 * testBodyTimeout):
		t.Fatal("a stalled body was never cut off")
	}
}

// A body that arrived in full leaves the rest of the request unbounded, as before.
func TestReadBodyRequestOutlivesTheBodyDeadline(t *testing.T) {
	ctxErr := make(chan error, 1)
	srv := httptest.NewServer(limitBodyFor(testBodyTimeout, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(3 * testBodyTimeout):
		}
		ctxErr <- r.Context().Err()
		w.WriteHeader(http.StatusNoContent)
	})))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_ = resp.Body.Close()
	if err := <-ctxErr; err != nil {
		t.Errorf("a POST that read its body had its context cancelled: %v", err)
	}
}
