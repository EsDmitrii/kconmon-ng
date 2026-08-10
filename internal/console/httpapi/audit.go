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

// Auditor is the subset of store.AuditStore the audit middleware (write) and GET /api/v1/audit
// (read) need.
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

// auditBufferSize is the audit write queue's fixed capacity -- SMALL and bounded on purpose; a full
// buffer means the audit writer (InsertAuditEntry, or the database underneath it) cannot keep up.
const auditBufferSize = 64

// auditWriteTimeout bounds one InsertAuditEntry call the drain goroutine makes.
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

// runAuditDrain is the one goroutine draining s.auditCh.
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

// recordAudit enqueues one best-effort audit row; a complete no-op when s.audit is nil, and
// otherwise a non-blocking, drop-and-count send.
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
		// Counted locally as well as in the metric: flushAudit reports the
		// total in the pod's own logs at shutdown, for the case where nobody
		// scrapes this replica again before it goes away.
		s.auditDropped.Add(1)
		slog.Warn("httpapi: audit buffer full, dropping entry", "action", job.action, "outcome", job.outcome)
	}
}

// auditResource extracts the one path parameter (name or id) this package's mutating routes carry,
// when they carry one at all.
func auditResource(r *http.Request) string {
	if name := chi.URLParam(r, "name"); name != "" {
		return name
	}
	return chi.URLParam(r, "id")
}

// auditDetailAllowlist maps "METHOD route-pattern" to the top-level JSON body keys permitted into
// an audit row's detail column; a mutating route with NO entry here -- or whose body fails to
// decode as a JSON object.
var auditDetailAllowlist = map[string][]string{
	"POST /api/v1/auth/login":    {"username"},
	"POST /api/v1/rbac/roles":    {"name", "permissions"},
	"POST /api/v1/rbac/bindings": {"roleName", "subjectKind", "subjectId"},
	"POST /api/v1/tokens":        {"name", "expiresAt"},
	// destinationKind joined in : a closed three-value enum that tells an auditor whether a run probed
	// the mesh or something outside it; the external address and target id stay excluded for the same
	// reason sources/destinations.
	"POST /api/v1/runs": {"type", "plane", "destinationKind"},
	// Targets: "name" and "kind" only, NEVER "address".
	"POST /api/v1/targets":     {"name", "kind"},
	"PUT /api/v1/targets/{id}": {"name", "kind"},
	// "destinationAddress" is NEVER listed, for the identical reason a target's address is not: it
	// names internal infrastructure.
	"POST /api/v1/checks":     {"name", "checkType", "sourceSelection", "enabled"},
	"PUT /api/v1/checks/{id}": {"name", "checkType", "sourceSelection", "enabled"},
	// POST /api/v1/checks/projection has NO entry: it persists nothing, so there is no state change to
	// attribute.
	"POST /api/v1/schedules":     {"definitionId", "kind", "enabled"},
	"PUT /api/v1/schedules/{id}": {"definitionId", "kind", "enabled"},
	// Annotations: "scope" and NOTHING else.
	"POST /api/v1/annotations": {"scope"},
	// Incidents: "title", "scope" and "status" — what was opened, about what, and where it stands.
	"POST /api/v1/incidents": {"title", "scope", "status"},
	// PATCH allow-lists "status" ALONE; note that "status" is present here even though POST never
	// accepts.
	"PATCH /api/v1/incidents/{id}": {"status"},
	// Maintenance: the SCOPE alone, on the annotations precedent.
	"POST /api/v1/maintenance": {"scope"},
	// Two bans, both absolute: - "secret" NEVER.
	"POST /api/v1/webhooks":     {"name", "events"},
	"PUT /api/v1/webhooks/{id}": {"name", "events"},
	// Alert rules: the rule NAME and nothing else; their key names would be safe.
	"POST /api/v1/alert-rules":     {"name"},
	"PUT /api/v1/alert-rules/{id}": {"name"},
	// Import: the FOREIGN OBJECT's name, which is the whole body.
	"POST /api/v1/alert-rules/import": {"name"},
	// Configuration import: "dryRun" and nothing else; it is listed HERE, off the request body, rather
	// than only in auditResultAllowlist below.
	"POST /api/v1/import": {"dryRun"},
}

// auditResultAllowlist is auditDetailAllowlist's counterpart for detail a handler computes rather
// than receives.
var auditResultAllowlist = map[string][]string{
	"POST /api/v1/import": {
		"dryRun",
		"targets", "checkDefinitions", "checkSchedules",
		"alertRules", "webhooks", "maintenanceWindows",
	},
}

// auditResultKey is the context key auditMutation stores its one-slot result
// mailbox under. An unexported struct type, the same forgery-proof convention
// subjectContextKey uses.
type auditResultKey struct{}

// auditResultHolder is the mailbox itself: the closed key set this route may record, and whatever
// the handler put there.
type auditResultHolder struct {
	allowed []string
	fields  map[string]json.RawMessage
}

// setAuditResult records handler-computed detail for this request's audit row.
func setAuditResult(r *http.Request, fields map[string]any) {
	holder, _ := r.Context().Value(auditResultKey{}).(*auditResultHolder)
	if holder == nil {
		return
	}
	out := make(map[string]json.RawMessage, len(holder.allowed))
	for _, key := range holder.allowed {
		v, present := fields[key]
		if !present {
			continue
		}
		encoded, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[key] = encoded
	}
	holder.fields = out
}

// mergeAuditResult folds a holder's recorded fields into the detail extracted from the request
// body; result keys WIN on a collision: they are computed from what actually happened.
func mergeAuditResult(detail json.RawMessage, holder *auditResultHolder) json.RawMessage {
	if holder == nil || len(holder.fields) == 0 {
		return detail
	}
	merged := map[string]json.RawMessage{}
	if len(detail) > 0 {
		// A detail that does not decode is emptyDetail or a marshal this
		// package produced; either way starting from {} is correct.
		_ = json.Unmarshal(detail, &merged)
	}
	for key, value := range holder.fields {
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return detail
	}
	return encoded
}

// auditDetailFor extracts action's allow-listed subset of body's top-level JSON keys; values are
// copied through unexamined -- only the KEY NAME is allow-listed.
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

// captureAuditDetail buffers r's body and restores it (io.NopCloser over the bytes already read)
// BEFORE returning; only ever called for a mutating request once s.audit is known non-nil
// (auditMutation's caller, authorize).
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

// auditMutation wraps next for a mutating request that already passed authorize's permission and
// CSRF checks.
func (s *Server) auditMutation(w http.ResponseWriter, r *http.Request, subject authz.Subject, next http.Handler) { //nolint:gocritic // Subject is a value type by design
	detail := s.captureAuditDetail(r)

	// The result mailbox is installed ONLY for a route auditResultAllowlist
	// names, so every other mutating route pays nothing at all -- not a
	// context value, not an allocation.
	var holder *auditResultHolder
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	if allowed, ok := auditResultAllowlist[r.Method+" "+pattern]; ok {
		holder = &auditResultHolder{allowed: allowed}
		r = r.WithContext(context.WithValue(r.Context(), auditResultKey{}, holder))
	}

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	outcome := auditOutcomeAllowed
	if rec.status >= http.StatusBadRequest {
		outcome = auditOutcomeError
	}
	s.recordAudit(r, subject, outcome, mergeAuditResult(detail, holder))
}

// Limit bounds for GET /api/v1/audit, mirroring GET /api/v1/events'
// eventsMinLimit/eventsMaxLimit/eventsDefaultLimit convention (events.go).
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

// handleAudit serves one page of the audit log, newest first, behind an opaque keyset cursor.
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
