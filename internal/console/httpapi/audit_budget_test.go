package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// auditRequest is a request carrying the chi route pattern recordAudit reads.
func auditRequest(t *testing.T, method, pattern string, mutate func(*http.Request)) *http.Request {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.RoutePatterns = []string{pattern}
	req := httptest.NewRequestWithContext(context.WithValue(t.Context(), chi.RouteCtxKey, rctx), method, pattern, http.NoBody)
	if mutate != nil {
		mutate(req)
	}
	return req
}

// parkAuditDrain queues one row the drain takes into the blocked insert, so the buffer length the test
// reads afterwards is stable.
func parkAuditDrain(t *testing.T, s *Server) {
	t.Helper()
	s.auditCh <- auditJob{action: "POST /api/v1/annotations", outcome: auditOutcomeAllowed, detail: emptyDetail}
	deadline := time.Now().Add(2 * time.Second)
	for len(s.auditCh) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the audit drain never took the parked row")
		}
		time.Sleep(time.Millisecond)
	}
}

// A signed-in viewer's failed requests (a 4xx or 5xx after authorize passed) are as cheap to produce as
// a denial. They used to keep the whole buffer, so a PromQL flood ending in 429s dropped an operator's
// allowed row on the same replica.
func TestSignedInErrorFloodLeavesRoomForAllowedRows(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, []authz.Permission{authz.PermPromQLQuery}, Deps{})

	for range auditBufferSize * 3 {
		w := doRequest(t, s, http.MethodPost, "/api/v1/promql/query", strings.NewReader(`{"query":"up"}`), mutateWithCSRF)
		if w.Code < http.StatusBadRequest {
			t.Fatalf("POST /api/v1/promql/query = %d, the test needs a failed request", w.Code)
		}
	}
	if got := len(s.auditCh); got > auditBufferSize/2 {
		t.Fatalf("audit buffer holds %d of %d after one viewer's failed requests, want at most half", got, auditBufferSize)
	}

	before := len(s.auditCh)
	s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/annotations", nil),
		authz.Subject{Kind: authz.SubjectUser, ID: "op1"}, auditOutcomeAllowed, emptyDetail)
	if got := len(s.auditCh); got != before+1 {
		t.Fatalf("an operator's allowed row was dropped with the buffer at %d/%d", before, auditBufferSize)
	}
}

// One subject's failed rows cannot take every slot failed rows may use: another subject's failure
// still lands, whoever floods and however they authenticated.
func TestAuditFailedRowsHaveAPerSubjectBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flooder  authz.Subject
		other    authz.Subject
		setupReq func(addr string) func(*http.Request)
	}{
		{
			name:    "user",
			flooder: authz.Subject{Kind: authz.SubjectUser, ID: "viewer1"},
			other:   authz.Subject{Kind: authz.SubjectUser, ID: "viewer2"},
		},
		{
			name:    "token",
			flooder: authz.Subject{Kind: authz.SubjectToken, ID: "tok-1"},
			other:   authz.Subject{Kind: authz.SubjectUser, ID: "viewer2"},
		},
		{
			name:    "credential-less, per client address",
			flooder: authz.Subject{},
			other:   authz.Subject{},
			setupReq: func(addr string) func(*http.Request) {
				return func(r *http.Request) { r.RemoteAddr = addr + ":5555" }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := &fakeAuditStore{block: make(chan struct{})}
			defer close(audit.block)
			s := newAuditTestServer(t, audit, nil, Deps{})
			parkAuditDrain(t, s)

			var flood, other func(*http.Request)
			if tc.setupReq != nil {
				flood, other = tc.setupReq("198.51.100.7"), tc.setupReq("203.0.113.20")
			}
			for range auditBufferSize {
				s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/promql/query", flood), tc.flooder, auditOutcomeError, emptyDetail)
			}
			if got := len(s.auditCh); got > auditQueuedPerSubject {
				t.Fatalf("one subject holds %d queued failed rows, want at most %d", got, auditQueuedPerSubject)
			}
			before := len(s.auditCh)
			s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/promql/query", other), tc.other, auditOutcomeError, emptyDetail)
			if got := len(s.auditCh); got != before+1 {
				t.Fatalf("another subject's failed row was dropped with the buffer at %d/%d", before, auditBufferSize)
			}
		})
	}
}

// Once the drain takes a subject's rows, that subject's budget is free again.
func TestAuditSubjectBudgetIsReturnedByTheDrain(t *testing.T) {
	audit := &fakeAuditStore{}
	s := newAuditTestServer(t, audit, nil, Deps{})
	subject := authz.Subject{Kind: authz.SubjectUser, ID: "viewer1"}
	for i := range auditQueuedPerSubject * 4 {
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/promql/query", nil), subject, auditOutcomeError, emptyDetail)
		deadline := time.Now().Add(2 * time.Second)
		for len(audit.snapshot()) < i+1 {
			if time.Now().After(deadline) {
				t.Fatalf("row %d never reached the store: the subject's budget was not returned", i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A flood of rows that may never take the last quarter of the buffer, allowed or not, leaves that
// quarter to the rows sensitive routes write, and those land without waiting.
func TestAuditSensitiveRowsKeepAReserve(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)

	// Many operators writing annotations: allowed rows, from different subjects.
	for i := range auditBufferSize * 2 {
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/annotations", nil),
			authz.Subject{Kind: authz.SubjectUser, ID: "op" + strconv.Itoa(i)}, auditOutcomeAllowed, emptyDetail)
	}
	// Many viewers probing RBAC: denied rows on a sensitive route.
	for i := range auditBufferSize * 2 {
		s.recordAudit(auditRequest(t, http.MethodDelete, "/api/v1/rbac/bindings/{id}", nil),
			authz.Subject{Kind: authz.SubjectUser, ID: "viewer" + strconv.Itoa(i)}, auditOutcomeDenied, emptyDetail)
	}
	if got := len(s.auditCh); got >= auditBufferSize {
		t.Fatalf("the flood filled the whole buffer (%d/%d); nothing is left for a sensitive row", got, auditBufferSize)
	}

	start := time.Now()
	before := len(s.auditCh)
	s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/rbac/bindings", nil),
		authz.Subject{Kind: authz.SubjectUser, ID: "admin"}, auditOutcomeAllowed, emptyDetail)
	if got := len(s.auditCh); got != before+1 {
		t.Fatalf("the admin's RBAC row was dropped with the buffer at %d/%d", before, auditBufferSize)
	}
	if waited := time.Since(start); waited > auditSensitiveSendWait/2 {
		t.Errorf("the admin's RBAC row waited %s for room; the reserve should take it at once", waited)
	}
}

// Many subjects recording at once never push the buffer past the tier a row is allowed to use.
func TestAuditAdmissionHoldsItsLimitsUnderConcurrency(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)

	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			subject := authz.Subject{Kind: authz.SubjectUser, ID: "viewer" + strconv.Itoa(g)}
			for range auditBufferSize {
				s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/promql/query", nil), subject, auditOutcomeError, emptyDetail)
			}
		})
	}
	wg.Wait()
	if got := len(s.auditCh); got > auditBufferSize/2 {
		t.Fatalf("concurrent failed rows filled %d of %d slots, want at most half", got, auditBufferSize)
	}
}

// fromAddr is a request mutator that sets the client address.
func fromAddr(addr string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = addr + ":40000" }
}

/*
A failed row on a sensitive route is as cheap to produce as any other failed row: a credential-less
POST /api/v1/tokens answers 401 to anyone. Those rows shared the ceiling of the allowed rows on ordinary
routes, so three addresses sending 401s, with failed rows on ordinary routes besides, left an operator's
webhook change no room, and it went unrecorded.
*/
func TestAuditFailedRowsOnSensitiveRoutesLeaveRoomForAllowedRows(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)

	for i := range auditBufferSize * 2 {
		addr := fromAddr("203.0.113." + strconv.Itoa(i%8+1))
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/tokens", addr), authz.Subject{}, auditOutcomeDenied, emptyDetail)
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/annotations", addr), authz.Subject{}, auditOutcomeDenied, emptyDetail)
	}

	before := len(s.auditCh)
	s.recordAudit(auditRequest(t, http.MethodPut, "/api/v1/webhooks/{id}", nil),
		authz.Subject{Kind: authz.SubjectUser, ID: "op1"}, auditOutcomeAllowed, emptyDetail)
	if got := len(s.auditCh); got != before+1 {
		t.Fatalf("an operator's allowed webhook row was dropped with the buffer at %d/%d", before, auditBufferSize)
	}
}

// And the other way round: allowed rows on ordinary routes and failed rows anywhere else cannot take
// the share kept for failed rows on sensitive routes, which record who probed for authority.
func TestAuditFailedRowsOnSensitiveRoutesKeepAShare(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)

	for i := range auditBufferSize * 2 {
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/annotations", nil),
			authz.Subject{Kind: authz.SubjectUser, ID: "op" + strconv.Itoa(i)}, auditOutcomeAllowed, emptyDetail)
		s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/annotations", fromAddr("198.51.100."+strconv.Itoa(i%8+1))),
			authz.Subject{}, auditOutcomeDenied, emptyDetail)
	}

	before := len(s.auditCh)
	s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/rbac/bindings", nil),
		authz.Subject{Kind: authz.SubjectUser, ID: "viewer1"}, auditOutcomeDenied, emptyDetail)
	if got := len(s.auditCh); got != before+1 {
		t.Fatalf("a viewer's denied RBAC row was dropped with the buffer at %d/%d", before, auditBufferSize)
	}
}

// Concurrent failed rows on sensitive routes never grow past their share.
func TestAuditFailedSensitiveShareHoldsUnderConcurrency(t *testing.T) {
	audit := &fakeAuditStore{block: make(chan struct{})}
	defer close(audit.block)
	s := newAuditTestServer(t, audit, nil, Deps{})
	parkAuditDrain(t, s)

	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			subject := authz.Subject{Kind: authz.SubjectUser, ID: "viewer" + strconv.Itoa(g)}
			for range auditBufferSize {
				s.recordAudit(auditRequest(t, http.MethodPost, "/api/v1/rbac/bindings", nil), subject, auditOutcomeDenied, emptyDetail)
			}
		})
	}
	wg.Wait()
	if got := len(s.auditCh); got > auditBufferSize/8 {
		t.Fatalf("concurrent failed RBAC rows filled %d of %d slots, want at most an eighth", got, auditBufferSize)
	}
}
