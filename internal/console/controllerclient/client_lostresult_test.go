package controllerclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These cover the second-order half of the lost-MTR bug: when the controller accepts a dispatch and
// then drops the connection without answering (its response write deadline expired), the Console's
// run used to record the raw transport error -- `controller diagnose: Post "http://.../api/v1/
// diagnostics?timeout=110": EOF` -- which says nothing about what happened or where. The error must
// name the layer that cut the call and admit that a completed check may have been thrown away.

// hijackAndDrop answers nothing: it takes the connection over and closes it, which is exactly what a
// Go http.Server does to a handler that outlives its write deadline.
func hijackAndDrop(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = r.Body.Read(make([]byte, 1))
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDiagnoseConnectionDroppedBeforeAnswerIsNotABareEOF(t *testing.T) {
	var calls atomic.Int32
	srv := hijackAndDrop(t, &calls)
	c := New(srv.URL, 5*time.Second)

	_, err := c.Diagnose(context.Background(), DiagnoseRequest{
		Source: "node-a", Destination: "google-dns", Type: "mtr",
		DestinationKind: DestinationKindExternal, DestinationAddress: "8.8.8.8",
	}, 110*time.Second)

	if err == nil {
		t.Fatal("expected an error when the controller drops the connection")
	}
	if !errors.Is(err, ErrResultLost) {
		t.Fatalf("expected ErrResultLost, got %v", err)
	}

	msg := err.Error()
	for _, want := range []string{
		"closed the connection",  // what actually happened
		"1m50s",                  // the dispatch timeout that was negotiated
		"controller HTTP server", // the layer that cut it
		"lost",                   // the honest consequence
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message does not mention %q: %s", want, msg)
		}
	}
	// A trace that already ran must not be silently re-run: this failure is not retried.
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly one dispatch attempt, got %d", got)
	}
}

// TestDiagnoseClientDeadlineIsNamedAsSuch keeps the two failures distinguishable: the Console giving
// up on its own deadline is a different sentence from the controller dropping the answer.
func TestDiagnoseClientDeadlineIsNamedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := New(srv.URL, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.Diagnose(ctx, DiagnoseRequest{Source: "node-a", Destination: "node-b", Type: "mtr"}, 110*time.Second)
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("expected ErrCheckTimeout, got %v", err)
	}
	if errors.Is(err, ErrResultLost) {
		t.Fatalf("a console-side deadline must not be reported as a dropped controller answer: %v", err)
	}
	if !strings.Contains(err.Error(), "console") {
		t.Errorf("error must name the layer that gave up: %s", err.Error())
	}
}

// TestDiagnoseSuccessUnaffected guards the classification from swallowing the happy path.
func TestDiagnoseSuccessUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, 5*time.Second)

	raw, err := c.Diagnose(context.Background(), DiagnoseRequest{Source: "node-a", Destination: "node-b", Type: "mtr"}, 90*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(raw) != `{"success":true}` {
		t.Fatalf("unexpected body: %s", raw)
	}
}
