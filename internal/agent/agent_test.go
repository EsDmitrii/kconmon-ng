package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

type mockDeregisterer struct {
	called    bool
	gotCtx    context.Context
	returnErr error
}

func (m *mockDeregisterer) Deregister(ctx context.Context) error {
	m.called = true
	m.gotCtx = ctx
	return m.returnErr
}

func TestGracefulDeregisterCallsClient(t *testing.T) {
	a := &Agent{}
	m := &mockDeregisterer{}

	a.gracefulDeregister(m)

	if !m.called {
		t.Fatal("expected Deregister to be called on shutdown")
	}
	if _, ok := m.gotCtx.Deadline(); !ok {
		t.Error("expected Deregister to be called with a bounded (timeout) context")
	}
}

func TestGracefulDeregisterDoesNotBlockOnError(t *testing.T) {
	a := &Agent{}
	m := &mockDeregisterer{returnErr: errors.New("controller unreachable")}

	done := make(chan struct{})
	go func() {
		a.gracefulDeregister(m)
		close(done)
	}()

	select {
	case <-done:
		// A failing Deregister must not block or panic the shutdown path.
	case <-time.After(3 * time.Second):
		t.Fatal("gracefulDeregister blocked on Deregister error")
	}

	if !m.called {
		t.Fatal("expected Deregister to be attempted even though it fails")
	}
}
