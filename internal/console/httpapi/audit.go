package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// Auditor is the subset of store.AuditStore the audit middleware (write)
// and GET /api/v1/audit (read) need. store.AuditStore's
// DeleteAuditEntriesBefore is deliberately excluded -- that is the
// retention Pruner's job, wired directly against *store.DB in cmd/console,
// never something httpapi calls.
type Auditor interface {
	InsertAuditEntry(ctx context.Context, subjectKind, subjectID, action, resource, outcome, remoteAddr string, detail json.RawMessage) (store.AuditEntry, error)
	ListAuditEntries(ctx context.Context, f store.AuditFilter) (store.AuditPage, error)
}

// Audit outcomes (audit_log.outcome, migration 00002: "allowed | denied |
// error").
const (
	auditOutcomeAllowed = "allowed"
	auditOutcomeDenied  = "denied"
	auditOutcomeError   = "error"
)

// auditBufferSize is the audit write queue's fixed capacity -- SMALL and
// bounded on purpose (SECURITY.md §10.2 via task-17-brief.md: "a small
// buffered channel"). A full buffer means the audit writer (InsertAuditEntry,
// or the database underneath it) cannot keep up; recordAudit's non-blocking
// send then DROPS the entry and increments metrics.AuditDropped rather than
// blocking the request that triggered it. This is documented lossiness, not
// an oversight: a best-effort audit trail must never become a backpressure
// mechanism the rest of the console pays request latency for.
const auditBufferSize = 64

// auditWriteTimeout bounds one InsertAuditEntry call the drain goroutine
// makes, so a single stuck database connection cannot wedge every
// subsequent audit write forever. It only ever affects the audit log's own
// lag -- recordAudit has already returned, and the request it described has
// already been answered, by the time this runs.
const auditWriteTimeout = 5 * time.Second

// emptyDetail is the audit row's default "nothing allow-listed" detail.
var emptyDetail = json.RawMessage(`{}`)

// auditJob is one row queued for the drain goroutine to write.
type auditJob struct {
	subjectKind string
	subjectID   string
	action      string
	resource    string
	outcome     string
	remoteAddr  string
	detail      json.RawMessage
}

// runAuditDrain is the one goroutine draining s.auditCh, started by
// NewServer exactly when Deps.Audit is non-nil and left running for the
// life of the process -- s.auditCh is never closed, the same lifecycle
// convention as ws.Hub.Run and the other realtime components cmd/console
// spawns, none of which have an explicit Stop either. Writes are strictly
// serialized, one at a time: that is what makes "drained by ONE goroutine"
// true, and why a single slow or stuck write only delays the writes queued
// behind it -- it never lets two overlapping writes race, and it never
// blocks a live request (recordAudit's send is non-blocking; this goroutine
// runs entirely off the request path).
func (s *Server) runAuditDrain() {
	for job := range s.auditCh {
		ctx, cancel := context.WithTimeout(context.Background(), auditWriteTimeout)
		_, err := s.audit.InsertAuditEntry(ctx, job.subjectKind, job.subjectID, job.action, job.resource, job.outcome, job.remoteAddr, job.detail)
		cancel()
		if err != nil {
			slog.Warn("httpapi: write audit entry failed", "action", job.action, "outcome", job.outcome, "error", err)
		}
	}
}

// recordAudit enqueues one best-effort audit row. A complete no-op when
// s.audit is nil (database.mode=disabled -- the brief's "the middleware is
// a no-op"), and otherwise a non-blocking, drop-and-count send: this
// function must never add latency to, or fail, the request it is called
// from.
func (s *Server) recordAudit(r *http.Request, subject authz.Subject, outcome string, detail json.RawMessage) { //nolint:gocritic // Subject is a value type by design
	if s.audit == nil {
		return
	}
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	job := auditJob{
		subjectKind: string(subject.Kind),
		subjectID:   subject.ID,
		action:      r.Method + " " + pattern,
		resource:    auditResource(r),
		outcome:     outcome,
		remoteAddr:  r.RemoteAddr,
		detail:      detail,
	}
	select {
	case s.auditCh <- job:
	default:
		s.metrics.AuditDropped.WithLabelValues().Inc()
		slog.Warn("httpapi: audit buffer full, dropping entry", "action", job.action, "outcome", job.outcome)
	}
}

// auditResource extracts the one path parameter (name or id) this
// package's mutating routes carry, when they carry one at all: DELETE
// /api/v1/rbac/roles/{name}, DELETE /api/v1/rbac/bindings/{id}, DELETE
// /api/v1/tokens/{id}. A route with neither (POST /api/v1/rbac/roles, POST
// /api/v1/rbac/bindings, POST /api/v1/tokens, and the pre-existing
// login/logout/promql routes) gets "" -- a legal value for
// audit_log.resource (NOT NULL, no non-empty constraint).
func auditResource(r *http.Request) string {
	if name := chi.URLParam(r, "name"); name != "" {
		return name
	}
	return chi.URLParam(r, "id")
}

// auditDetailAllowlist maps "METHOD route-pattern" to the top-level JSON
// body keys permitted into an audit row's detail column. A mutating route
// with NO entry here -- or whose body fails to decode as a JSON object --
// gets an empty {} detail: omission is enforced as "allow nothing", never
// "allow everything", so a future mutating route added without an entry
// fails safe and can never leak its raw body into the audit log. This is
// also why "password" never appears for POST /api/v1/auth/login and
// "query" never appears for the PromQL routes (which have no entry at all,
// by design -- task-17-brief.md: detail "must never contain a request body
// verbatim, a password, a token, or a PromQL string").
// POST /api/v1/runs allow-lists "type" and "plane" only -- enough to tell
// what kind of run an audit row describes -- and deliberately excludes
// "sources"/"destinations": those are node-name arrays, not secrets, but a
// full-mesh spec can carry up to 400 entries between them, and there is no
// value in bloating every run's audit row with a list already reconstructible
// from the run itself (GET /api/v1/runs/{id}).
var auditDetailAllowlist = map[string][]string{
	"POST /api/v1/auth/login":    {"username"},
	"POST /api/v1/rbac/roles":    {"name", "permissions"},
	"POST /api/v1/rbac/bindings": {"roleName", "subjectKind", "subjectId"},
	"POST /api/v1/tokens":        {"name", "expiresAt"},
	"POST /api/v1/runs":          {"type", "plane"},
}

// auditDetailFor extracts action's allow-listed subset of body's top-level
// JSON keys. Values are copied through unexamined -- only the KEY NAME is
// allow-listed -- which is safe because every allow-listed key names a
// field this codebase already treats as non-sensitive (a role name, a
// permission list, a subject id, a token's display name); nothing that
// could hold a password, a raw token, or a PromQL query string is ever
// listed.
func auditDetailFor(action string, body []byte) json.RawMessage {
	allowed, ok := auditDetailAllowlist[action]
	if !ok || len(allowed) == 0 || len(body) == 0 {
		return emptyDetail
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return emptyDetail
	}
	out := make(map[string]json.RawMessage, len(allowed))
	for _, key := range allowed {
		if v, present := fields[key]; present {
			out[key] = v
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return emptyDetail
	}
	return encoded
}

// captureAuditDetail buffers r's body and restores it (io.NopCloser over the
// bytes already read) BEFORE returning, so the handler that runs next sees
// it byte-for-byte unchanged -- then extracts the allow-listed detail for
// r's route from the buffered copy. Only ever called for a mutating request
// once s.audit is known non-nil (auditMutation's caller, authorize); the
// extra body read is real per-request cost, but it is paid only in that
// case, never when database.mode=disabled.
func (s *Server) captureAuditDetail(r *http.Request) json.RawMessage {
	// Routes with no allow-list entry always audit {} — skip the body read
	// entirely rather than buffering (e.g. a PromQL query body) for a
	// guaranteed-empty result.
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	key := r.Method + " " + pattern
	if _, ok := auditDetailAllowlist[key]; !ok {
		return emptyDetail
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil || len(raw) == 0 {
		return emptyDetail
	}
	return auditDetailFor(key, raw)
}

// auditMutation wraps next for a mutating request that already passed
// authorize's permission and CSRF checks: it captures the allow-listed
// request detail BEFORE calling next, then the handler's own final status
// code AFTER, and records one best-effort async audit row -- outcome
// "allowed" for a handler status < 400, "error" otherwise. Only called when
// s.audit is non-nil (authorize's own gate): database.mode=disabled must
// add zero overhead on this path.
func (s *Server) auditMutation(w http.ResponseWriter, r *http.Request, subject authz.Subject, next http.Handler) { //nolint:gocritic // Subject is a value type by design
	detail := s.captureAuditDetail(r)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	outcome := auditOutcomeAllowed
	if rec.status >= http.StatusBadRequest {
		outcome = auditOutcomeError
	}
	s.recordAudit(r, subject, outcome, detail)
}

// Limit bounds for GET /api/v1/audit, mirroring GET /api/v1/events'
// eventsMinLimit/eventsMaxLimit/eventsDefaultLimit convention
// (events.go) -- a hand-kept-in-sync copy, same reasoning: store keeps its
// own clampLimit private.
const (
	auditMinLimit     = 1
	auditMaxLimit     = 500
	auditDefaultLimit = 100
)

func parseAuditLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

func clampAuditLimit(n int) int {
	switch {
	case n == 0:
		return auditDefaultLimit
	case n < auditMinLimit:
		return auditMinLimit
	case n > auditMaxLimit:
		return auditMaxLimit
	default:
		return n
	}
}

// auditEntryResponse is one row of GET /api/v1/audit's body.
type auditEntryResponse struct {
	ID          int64           `json:"id"`
	At          time.Time       `json:"at"`
	SubjectKind string          `json:"subjectKind"`
	SubjectID   string          `json:"subjectId"`
	Action      string          `json:"action"`
	Resource    string          `json:"resource"`
	Outcome     string          `json:"outcome"`
	RemoteAddr  string          `json:"remoteAddr"`
	Detail      json.RawMessage `json:"detail"`
}

// auditResponse is GET /api/v1/audit's body -- same keyset-cursor shape as
// eventsResponse (events.go).
type auditResponse struct {
	Entries    []auditEntryResponse `json:"entries"`
	NextCursor string               `json:"nextCursor"`
}

// handleAudit serves one page of the audit log, newest first, behind an
// opaque keyset cursor -- exactly like GET /api/v1/events. A nil s.audit
// (database.mode=disabled) answers 503, the same convention every other
// database-backed endpoint in this package uses.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		writeProblem(w, http.StatusServiceUnavailable, "audit log not available",
			"set console.database.mode in the console config (Helm: console.database.mode) to enable GET /api/v1/audit")
		return
	}

	q := r.URL.Query()

	cursor := q.Get("cursor")
	if cursor != "" {
		if _, _, _, err := store.DecodeCursor(cursor); err != nil {
			writeProblem(w, http.StatusBadRequest, "invalid cursor", "cursor is malformed or does not match this server")
			return
		}
	}

	filter := store.AuditFilter{
		SubjectKind: q.Get("subjectKind"),
		SubjectID:   q.Get("subjectId"),
		Cursor:      cursor,
		Limit:       clampAuditLimit(parseAuditLimit(q.Get("limit"))),
	}

	page, err := s.audit.ListAuditEntries(r.Context(), filter)
	if err != nil {
		slog.Error("list audit entries failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "audit log unavailable", "failed to query audit log")
		return
	}

	out := make([]auditEntryResponse, 0, len(page.Entries))
	for i := range page.Entries {
		e := &page.Entries[i]
		out = append(out, auditEntryResponse{
			ID: e.ID, At: e.At, SubjectKind: e.SubjectKind, SubjectID: e.SubjectID,
			Action: e.Action, Resource: e.Resource, Outcome: e.Outcome,
			RemoteAddr: e.RemoteAddr, Detail: e.Detail,
		})
	}
	writeJSON(w, auditResponse{Entries: out, NextCursor: page.NextCursor})
}
