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
		// Counted locally as well as in the metric: flushAudit reports the
		// total in the pod's own logs at shutdown, for the case where nobody
		// scrapes this replica again before it goes away.
		s.auditDropped.Add(1)
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
	// destinationKind joined in M4: a closed three-value enum that tells an
	// auditor whether a run probed the mesh or something outside it. The
	// external address and target id stay excluded for the same reason
	// sources/destinations are -- reconstructible from the run itself, and an
	// address is exactly the kind of value an audit row must not echo.
	"POST /api/v1/runs": {"type", "plane", "destinationKind"},
	// POST /api/v1/runs/{id}/cancel has NO entry, and that is the conscious
	// decision, not an omission: it carries no request body at all, so there
	// are no body keys to allow-list, and the run it names is already in the
	// row's resource column (auditResource reads {id}). It is still audited
	// like every other mutation -- authorize routes every POST through
	// auditMutation -- with the empty {} detail this map's default-deny rule
	// gives a body-less route. Same shape as every DELETE here.
	// Targets (M4 Task 3): "name" and "kind" only, NEVER "address". This is
	// the conscious decision the default-deny allow-list forces per mutating
	// route -- an audit log is read by more people, and retained longer, than
	// a target list is, and the address is the one field that names internal
	// infrastructure (a management URL, an internal IP). Name and kind are
	// enough to say WHICH target an operator changed; the address itself is
	// always readable from GET /api/v1/targets/{id} by whoever holds
	// targets:read. DELETE has no body and therefore no entry at all -- the
	// id it names is already recorded in the row's resource column
	// (auditResource).
	"POST /api/v1/targets":     {"name", "kind"},
	"PUT /api/v1/targets/{id}": {"name", "kind"},
	// Check definitions (M4 Task 4): the same conscious choice as targets,
	// one field further. "name", "checkType" and "sourceSelection" say WHICH
	// definition changed and how far it fans out; "enabled" is listed because
	// it is the single most consequential bit on the row -- it is what turns
	// a draft into fleet-wide probe traffic, and "who enabled this" is
	// precisely the question an audit log is read to answer.
	// "destinationAddress" is NEVER listed, for the identical reason a
	// target's address is not: it names internal infrastructure, and it stays
	// readable from GET /api/v1/checks/{id} by whoever holds checks:read.
	// "params" is not listed either -- it is an open JSON object this package
	// never inspects, so allow-listing it would be allow-listing whatever a
	// future check type decides to put in it, sight unseen.
	"POST /api/v1/checks":     {"name", "checkType", "sourceSelection", "enabled"},
	"PUT /api/v1/checks/{id}": {"name", "checkType", "sourceSelection", "enabled"},
	// POST /api/v1/checks/projection has NO entry: it persists nothing, so
	// there is no state change to attribute, and its body is a draft that may
	// never be saved. It is still audited as a mutating method (it is a POST)
	// -- with an empty {} detail, which is the honest record of "someone
	// asked what this would project".
	//
	// Schedules: "definitionId" and "kind" name what was scheduled and how,
	// "enabled" is again the bit that decides whether anything actually runs.
	// "intervalNs" and "runAt" are left off -- the cadence itself is
	// reconstructible from GET /api/v1/schedules/{id}, and an audit row is
	// not the place to keep a second copy of it. DELETE carries no body and
	// therefore no entry, its id being in the resource column already.
	"POST /api/v1/schedules":     {"definitionId", "kind", "enabled"},
	"PUT /api/v1/schedules/{id}": {"definitionId", "kind", "enabled"},
	// Annotations (M5 Task 4): "scope" and NOTHING else. "text" is free-form
	// operator prose -- the one field on this route that can contain
	// literally anything a human types, including the credential or hostname
	// they were mid-incident about -- and an audit log is read by more people
	// and retained longer than the note itself is. The note stays readable
	// from GET /api/v1/annotations by whoever holds annotations:read.
	// "startAt"/"endAt" are left off as noise: the row's own timestamp says
	// when the mark was pinned, and what it points at is reconstructible from
	// the annotation. DELETE has no body and therefore no entry -- the id it
	// names is already in the row's resource column (auditResource).
	"POST /api/v1/annotations": {"scope"},
	// Incidents (M6 Task 4): "title", "scope" and "status" — what was opened,
	// about what, and where it stands. "notes" is annotations' "text" one
	// milestone later, and the ban is the same one for the same reason: it is
	// free-form incident prose, the field most likely to hold the credential
	// or hostname the operator was mid-incident about, and it stays readable
	// from GET /api/v1/incidents/{id} by whoever holds incidents:read.
	// "pinned" is banned too, and for a sharper reason than noise: it is an
	// open JSON array whose "note" on each ref is more free-form prose,
	// so allow-listing it would allow-list exactly what banning "notes" set
	// out to keep out. "fromAt"/"toAt" are left off as noise — the row's own
	// timestamp says when the incident was opened.
	"POST /api/v1/incidents": {"title", "scope", "status"},
	// PATCH allow-lists "status" ALONE. It is the one bit of an incident's
	// evolution an audit log is read to answer for ("who resolved this, and
	// when"); notes and pinned are the same banned free-form fields as above.
	// Note that "status" is present here even though POST never accepts one:
	// the key names a body field, and only PATCH has a body that carries it.
	"PATCH /api/v1/incidents/{id}": {"status"},
	// Maintenance (M6 Task 4): the SCOPE alone, on the annotations precedent.
	// "reason" is free text — the field an operator explains a change window
	// in, which is exactly where a change ticket id, a vendor name or an
	// internal hostname ends up — and it stays readable from GET
	// /api/v1/maintenance by whoever holds maintenance:read. "startAt"/"endAt"
	// are left off as noise, reconstructible from the window itself.
	"POST /api/v1/maintenance": {"scope"},
	// Webhooks (M6 Task 4): "name" and "events" — WHICH endpoint changed and
	// what it now fires on. Two bans, both absolute:
	//
	//   - "secret" NEVER. It is the HMAC key every delivery is signed with.
	//     An audit log is read by more people and retained far longer than an
	//     endpoint row is, and a signing key that reaches it is a signing key
	//     that has to be rotated. M6's global constraint states this at the
	//     milestone level: webhook secrets appear in no audit row, no API
	//     read, no log, no metric label.
	//   - "url" NEVER, on the targets "address" precedent one step stronger:
	//     it names infrastructure OUTSIDE this cluster (a Slack hook, a
	//     PagerDuty ingest), and such URLs routinely embed a token in the path
	//     itself — allow-listing the field would be allow-listing the
	//     credential it may contain. It stays readable from GET
	//     /api/v1/webhooks by whoever holds webhooks:manage, which is admin.
	//
	// POST /api/v1/webhooks/{id}/test has NO entry, the conscious decision the
	// default-deny rule forces: it carries no body, and the endpoint it names
	// is already in the row's resource column (auditResource reads {id}). It
	// is still audited like every other mutation, with the empty {} detail.
	"POST /api/v1/webhooks":     {"name", "events"},
	"PUT /api/v1/webhooks/{id}": {"name", "events"},
	// Alert rules (M7 Task 4): the rule NAME and nothing else. Everything else
	// on this body is banned, and unusually the ban covers fields that look
	// harmless:
	//
	//   - "params" is where a raw rule's PromQL EXPRESSION lives, and an
	//     expression is a pile of label matchers -- {instance="10.4.2.9:9100"},
	//     {url="https://payments.internal/health"} -- which is the targets
	//     "address" ban applied to a field that can hold a dozen of them.
	//   - "labels" and "annotations" are operator-typed maps whose VALUES carry
	//     runbook URLs, team handles and hostnames, on the same line. Their key
	//     names would be safe; their values are what an audit row would echo,
	//     and this allow-list works on keys, so the only correct answer is to
	//     leave both out entirely.
	//   - "kind" and "severity" are closed enums and would in fact be safe, and
	//     are still left off: the row's resource column plus the name answer
	//     "which rule changed", and GET /api/v1/alert-rules/{id} answers
	//     everything else for whoever holds alerts:read.
	//
	// The rendered expression is never in a body at all -- the SERVER renders
	// it -- so it cannot reach a row through this channel by construction.
	//
	// POST /api/v1/alert-rules/preview and POST /api/v1/alert-rules/{id}/sync
	// have NO entry, and preview's absence is the consequential one: its body
	// is a DRAFT that routinely carries exactly the raw expression banned
	// above, and it is a POST, so it IS audited -- with the empty {} detail
	// this map's default-deny rule gives a route it does not name. DELETE
	// carries no body and its id is already in the resource column.
	"POST /api/v1/alert-rules":     {"name"},
	"PUT /api/v1/alert-rules/{id}": {"name"},
	// Import (M7 Decision 4): the FOREIGN OBJECT's name, which is the whole
	// body. The names of the rules it adopted stay off the row for the exact
	// reason the create/update entries ban everything but "name" -- an adopted
	// rule's name follows somebody else's naming convention and routinely
	// carries a customer, a cluster or a hostname in it, and there can be
	// dozens of them from one call.
	//
	// The COUNTS are not recorded either, and that is the one place this route
	// diverges from POST /api/v1/import's auditResultAllowlist. The reason is
	// that the import bundle's counts answer a question nothing else can
	// ("what did that opaque blob change"), while an adoption's counts are
	// derivable: the object is named on the row, its rules are readable from
	// the cluster, and the rows it produced are readable from GET
	// /api/v1/alert-rules by whoever holds alerts:read. A second audit channel
	// that adds no un-derivable fact is a second channel to keep honest.
	"POST /api/v1/alert-rules/import": {"name"},
	// Configuration import (M7 Task 6): "dryRun" and nothing else. It is
	// listed HERE, off the request body, rather than only in
	// auditResultAllowlist below, because a rejected import -- an unsupported
	// bundle version, a body with no bundle -- never reaches the handler and
	// would otherwise be recorded as an unattributed {} mutation. Which of
	// the two an audit reader is looking at is the single most important bit
	// on the row.
	//
	// "bundle" is NOT listed and never will be: it is the whole declarative
	// configuration of a console, every probe address and every webhook URL
	// included, and echoing it into a table read by more people and retained
	// longer than the configuration itself would be the largest single leak
	// this allow-list exists to prevent. What the import DID lands on the row
	// as counts, from auditResultAllowlist.
	"POST /api/v1/import": {"dryRun"},
}

// auditResultAllowlist is auditDetailAllowlist's counterpart for detail a
// handler computes rather than receives: "METHOD route-pattern" -> the
// top-level keys that handler may add to its audit row.
//
// It exists because a request body cannot answer the question an import's
// audit row is read to answer. "Who imported a bundle" is on the row already;
// "and what did it change" is only knowable AFTER the merge ran, and the
// counts are the honest, bounded summary of it.
//
// The posture is identical to the request-body list's, in every respect that
// matters:
//
//   - DEFAULT-DENY. A route with no entry here gets no mailbox installed at
//     all (auditMutation), so setAuditResult is a no-op for it and a future
//     handler cannot smuggle detail onto its row by omission.
//   - KEY-NAME allow-listing, not value inspection. Every key listed below
//     names a COUNT -- created/updated/skipped/errors/warnings per collection
//     -- and counts are the one class of value that cannot carry a name, an
//     address, an expression or a secret.
//
// Pinned, together with the request-body list, by
// TestAuditDetailAllowlistIsPinned.
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

// auditResultHolder is the mailbox itself: the closed key set this route may
// record, and whatever the handler put there. No mutex -- the write
// (setAuditResult, from the handler) happens-before the read (auditMutation,
// after next.ServeHTTP returns) on the request's own goroutine.
type auditResultHolder struct {
	allowed []string
	fields  map[string]json.RawMessage
}

// setAuditResult records handler-computed detail for this request's audit
// row, keeping only the keys auditResultAllowlist names for the route. A
// complete no-op for a route with no entry, for a request that is not being
// audited (database.mode=disabled), and for a key that is not allow-listed --
// so a handler can call it unconditionally.
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

// mergeAuditResult folds a holder's recorded fields into the detail extracted
// from the request body. Result keys WIN on a collision: they are computed
// from what actually happened, while the body key is only what was asked for
// (POST /api/v1/import's "dryRun" is deliberately in both lists for exactly
// that reason -- the response echoes the flag the handler acted on).
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
