package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
	"github.com/EsDmitrii/kconmon-ng/internal/console/config"
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

// auditSensitiveSendWait is how long a row on an auditSensitiveRoute waits for room in a full
// buffer before it is dropped.
const auditSensitiveSendWait = 2 * time.Second

// auditQueuedPerSubject bounds the cheap rows (auditRowTier) one subject may have waiting in the
// buffer at once.
const auditQueuedPerSubject = auditBufferSize / 4

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
	// budget is the auditQueued key a cheap row counts against while it waits; "" for the others.
	budget string
	// failedSensitive marks a cheap row on a sensitive route, which waits in its own share.
	failedSensitive bool
}

// runAuditDrain is the one goroutine draining s.auditCh.
func (s *Server) runAuditDrain() {
	for job := range s.auditCh {
		auditQueued.release(s.auditCh, job.budget, job.failedSensitive)
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
	remoteAddr := r.RemoteAddr
	ip := clientIP(r, s.trustedProxies)
	if ip != remoteAddrHost(r.RemoteAddr) {
		// Only a trusted proxy's forwarding header gets here; the direct peer keeps its port.
		remoteAddr = ip
	}
	if subject.Kind == "" && outcome != auditOutcomeAllowed && !s.credentialLessRowAdmitted(r.Context(), ip) {
		s.countAuditDrop()
		return
	}
	job := auditJob{
		subjectKind: string(subject.Kind),
		// A header-mode identity is whatever bytes the proxy sent, and a text column refuses invalid UTF-8.
		subjectID:  sanitizeAuditText(subject.ID),
		action:     r.Method + " " + pattern,
		resource:   auditResource(r),
		outcome:    outcome,
		remoteAddr: remoteAddr,
		detail:     withSubjectDisplay(detail, subject.ID, subject.DisplayName),
	}
	cheap, sensitive := auditRowTier(outcome, subject.Kind, r.Method, pattern)
	if cheap {
		job.budget = auditBudgetKey(subject, ip)
		job.failedSensitive = sensitive
	}
	if auditQueued.admit(s.auditCh, job, auditQueueLimit(cheap, sensitive, cap(s.auditCh))) {
		return
	}
	if cheap {
		// Cheap rows are dropped by design once their share is used up; the counter still sees them.
		s.countAuditDrop()
		return
	}
	if sensitive {
		// A sensitive row waits a moment for the drain instead of being dropped outright.
		timer := time.NewTimer(auditSensitiveSendWait)
		defer timer.Stop()
		select {
		case s.auditCh <- job:
			return
		case <-timer.C:
		}
	}
	s.countAuditDrop()
	slog.Warn("httpapi: audit buffer full, dropping entry", "action", job.action, "outcome", job.outcome)
}

// countAuditDrop counts a dropped row in the metric and locally: flushAudit reports the local total in
// the pod's own logs at shutdown, for when nobody scrapes this replica again before it goes away.
func (s *Server) countAuditDrop() {
	s.metrics.AuditDropped.WithLabelValues().Inc()
	s.auditDropped.Add(1)
}

/*
auditRowTier sorts a row for the buffer, which is small and shared by every caller of this replica.

A row is CHEAP when anyone can produce it at will: a failed request (denied, or a 4xx/5xx after
authorize passed, such as a viewer's PromQL answered 429) whoever sent it, and any row of a caller with
no credentials or in anonymous mode. One subject has at most auditQueuedPerSubject cheap rows waiting.

A row is SENSITIVE on the routes that carry authority (auditSensitiveRoute) and for a sign-in or
password change that succeeded, the row a flood of failed ones would otherwise hide.

auditQueueLimit turns the two into a share of the buffer, so that no flood pushes out the rows that
describe a change, and no stream of changes pushes out the record of who probed for authority.
*/
func auditRowTier(outcome string, kind authz.SubjectKind, method, pattern string) (cheap, sensitive bool) {
	if outcome == auditOutcomeAllowed && auditCredentialRoutes[method+" "+pattern] {
		return false, true
	}
	cheap = outcome != auditOutcomeAllowed || kind == "" || kind == authz.SubjectAnonymous
	return cheap, auditSensitiveRoute(pattern)
}

/*
auditQueueLimit is how many rows may already be waiting for a row of this tier to join them.

Cheap rows on sensitive routes wait in a share of their own, an eighth of the buffer, and are counted
only against it: they take nothing from the other tiers, and the other tiers cannot take their share.
Every other row is measured against the rows waiting outside that share: cheap rows use at most half,
allowed rows on ordinary routes three quarters, and sensitive allowed rows the rest, for which they
also wait auditSensitiveSendWait before they are dropped.
*/
func auditQueueLimit(cheap, sensitive bool, capacity int) int {
	switch {
	case cheap && sensitive:
		return capacity / 8
	case cheap:
		return capacity / 2
	case !sensitive:
		return capacity - capacity/4
	default:
		return capacity
	}
}

// auditCredentialLessPerMinute is how many failed rows one client address without credentials may add
// to the audit log per minute. It is above the default per-address sign-in budget, so a sign-in the
// limiter admits is always recorded; past it the rows are counted as dropped.
const auditCredentialLessPerMinute = 120

// credentialLessRowAdmitted spends one of clientIP's auditCredentialLessPerMinute rows. Such a row costs
// its sender nothing, so its rate is bounded where it is written, not only where it waits; a KV error
// records the row.
func (s *Server) credentialLessRowAdmitted(ctx context.Context, clientIP string) bool {
	if s.kv == nil {
		return true
	}
	n, err := s.kv.IncrWithTTL(ctx, "audit:nocred:"+rateLimitAddr(clientIP), rateLimitWindow)
	return err != nil || n <= auditCredentialLessPerMinute
}

// auditBudgetKey names whose auditQueuedPerSubject budget a cheap row spends: the credential for a
// signed-in caller, the client address for everyone else.
func auditBudgetKey(subject authz.Subject, clientAddr string) string { //nolint:gocritic // Subject is a value type by design
	if subject.Kind == "" || subject.Kind == authz.SubjectAnonymous {
		return "addr:" + rateLimitAddr(clientAddr)
	}
	return string(subject.Kind) + ":" + subject.ID
}

// auditQueued counts the cheap rows each subject has waiting, and the failed rows on sensitive routes,
// per audit buffer. It is keyed by the buffer's channel so every Server keeps its own counts; an entry
// exists only while its rows wait.
var auditQueued = &auditQueueLedger{waiting: map[auditQueuedKey]int{}, failedSensitive: map[chan auditJob]int{}}

type auditQueuedKey struct {
	ch     chan auditJob
	budget string
}

type auditQueueLedger struct {
	mu              sync.Mutex
	waiting         map[auditQueuedKey]int
	failedSensitive map[chan auditJob]int
}

// admit queues job without blocking when fewer than limit rows of its reckoning are waiting (see
// auditQueueLimit) and job's budget, if it has one, is not spent. The check and the send share one
// lock, so concurrent callers cannot overshoot the limit; only the drain empties the channel meanwhile.
func (l *auditQueueLedger) admit(ch chan auditJob, job auditJob, limit int) bool { //nolint:gocritic // hugeParam: sent by value anyway
	l.mu.Lock()
	defer l.mu.Unlock()
	waiting := len(ch) - l.failedSensitive[ch]
	if job.failedSensitive {
		waiting = l.failedSensitive[ch]
	}
	if waiting >= limit {
		return false
	}
	key := auditQueuedKey{ch: ch, budget: job.budget}
	if job.budget != "" && l.waiting[key] >= auditQueuedPerSubject {
		return false
	}
	select {
	case ch <- job:
	default:
		return false
	}
	if job.budget != "" {
		l.waiting[key]++
	}
	if job.failedSensitive {
		l.failedSensitive[ch]++
	}
	return true
}

// release returns a row's place in the counts when the drain takes it off ch.
func (l *auditQueueLedger) release(ch chan auditJob, budget string, failedSensitive bool) {
	if budget == "" && !failedSensitive {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if failedSensitive {
		if l.failedSensitive[ch] <= 1 {
			delete(l.failedSensitive, ch)
		} else {
			l.failedSensitive[ch]--
		}
	}
	if budget == "" {
		return
	}
	key := auditQueuedKey{ch: ch, budget: budget}
	if l.waiting[key] <= 1 {
		delete(l.waiting, key)
		return
	}
	l.waiting[key]--
}

/*
 * auditSensitiveRoute names the routes whose rows are never dropped to make room, in either outcome:
 * they carry authority (who may do what, and with which credential) or they move the whole
 * configuration in or out of the console.
 *
 * Matched on the chi ROUTE PATTERN, so a path parameter cannot dodge it.
 */
func auditSensitiveRoute(pattern string) bool {
	for _, prefix := range auditSensitivePrefixes {
		if strings.HasPrefix(pattern, prefix) {
			return true
		}
	}
	return false
}

// auditCredentialRoutes are the public routes that verify a credential; their successful rows are kept
// like a sensitive route's, while their failures still yield.
var auditCredentialRoutes = map[string]bool{
	"POST /api/v1/auth/login":        true,
	"POST /api/v1/auth/password":     true,
	"GET " + config.OIDCCallbackPath: true,
}

var auditSensitivePrefixes = []string{
	"/api/v1/rbac",
	"/api/v1/tokens",
	"/api/v1/users",
	"/api/v1/import",
	"/api/v1/export",
	"/api/v1/audit",
}

// auditedReads are the safe requests worth a row of their own: the export hands out every probe
// address, webhook URL and alert expression, and for rbac:manage the whole role map. GET /api/v1/audit
// is not here: the audit page polls it, and a row per page view would drown the log in its own reads.
var auditedReads = map[string]bool{
	"GET /api/v1/export": true,
}

// auditResource extracts the one path parameter (name or id) this package's mutating routes carry,
// sanitised: PostgreSQL refuses a NUL in a text column, so a raw value would let the caller drop their
// own row, denied probes included. By the time this runs the request is served, so it is sanitised
// rather than refused.
func auditResource(r *http.Request) string {
	if name := chi.URLParam(r, "name"); name != "" {
		return sanitizeAuditText(name)
	}
	return sanitizeAuditText(chi.URLParam(r, "id"))
}

// sanitizeAuditText replaces every control character and every invalid UTF-8 byte with U+FFFD, so a
// value the caller chose can never make a row unstorable. The substitution is visible on purpose: a
// reader of the audit log should be able to see that the input carried something it should not have.
func sanitizeAuditText(v string) string {
	if !invalidParamText(v) {
		return v
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '\uFFFD'
		}
		return r
	}, v)
}

// auditDetailAllowlist maps "METHOD route-pattern" to the top-level JSON body keys permitted into
// an audit row's detail column; a route with no entry, or a body that is not a JSON object, records {}.
var auditDetailAllowlist = map[string][]string{
	"POST /api/v1/auth/login":    {"username"},
	"POST /api/v1/rbac/roles":    {"name", "permissions"},
	"POST /api/v1/rbac/bindings": {"roleName", "subjectKind", "subjectId"},
	"POST /api/v1/tokens":        {"name", "expiresAt"},
	// Users: who was created with which role, and whether someone was disabled. The password NEVER.
	"POST /api/v1/users":       {"username", "displayName", "role"},
	"PATCH /api/v1/users/{id}": {"disabled", "role"},
	// destinationKind is a closed enum saying whether the run probed the mesh; addresses and target ids
	// stay out, as sources and destinations do.
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
	// Incidents: what was opened and about what; PATCH records the status alone.
	"POST /api/v1/incidents":       {"title", "scope"},
	"PATCH /api/v1/incidents/{id}": {"status"},
	// Maintenance: the SCOPE alone, on the annotations precedent.
	"POST /api/v1/maintenance": {"scope"},
	// Webhooks: never "secret" or "url"; the url may carry a token of its own.
	"POST /api/v1/webhooks":     {"name", "events"},
	"PUT /api/v1/webhooks/{id}": {"name", "events"},
	// Alert rules: the name only.
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
	// The id of the binding that was created; the role and subject come from the request body.
	"POST /api/v1/rbac/bindings": {"bindingId"},
	// A DELETE has no body at all, so EVERYTHING an auditor needs about a revocation has to be
	// computed by the handler from the row it is about to destroy.
	"DELETE /api/v1/rbac/bindings/{id}": {"bindingId", "roleName", "subjectKind", "subjectId"},
	"POST /api/v1/import": {
		"dryRun",
		"targets", "checkDefinitions", "checkSchedules",
		"alertRules", "webhooks", "maintenanceWindows",
		// An import may mint a custom role, so the access-control counts are recorded too; counts only.
		"rbacRoles", "rbacBindings",
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
	maps.Copy(merged, holder.fields)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return detail
	}
	return encoded
}

/*
 * auditSubjectDisplayKey is the reserved detail member carrying the subject's human-readable name
 * (display name or email). The audit table stores subject kind/id only, and an OIDC id is a UUID
 * nobody recognises a week later — so the name the session already carries rides in the detail
 * column, and handleAudit lifts it back out into its own response field. It cannot collide with
 * caller input: auditDetailFor copies only allow-listed keys and no allow-list names this one.
 */
const auditSubjectDisplayKey = "subjectDisplay"

// withSubjectDisplay folds displayName into detail when it adds information over the id; sanitised
// and bounded like every other value a row carries, because IdP claims are caller-shaped data.
func withSubjectDisplay(detail json.RawMessage, id, displayName string) json.RawMessage {
	name := sanitizeAuditText(displayName)
	if name == "" || name == id {
		return detail
	}
	merged := map[string]json.RawMessage{}
	if len(detail) > 0 {
		// A detail that does not decode is emptyDetail or a marshal this package produced; starting
		// from {} is correct either way (mergeAuditResult's reasoning).
		_ = json.Unmarshal(detail, &merged)
	}
	encodedName, _ := json.Marshal(name) // a string always encodes
	merged[auditSubjectDisplayKey] = boundAuditValue(encodedName)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return detail
	}
	return encoded
}

// liftSubjectDisplay moves the reserved member out of a stored detail into its own value, so the
// response's detail keeps carrying only what the route's allow-list let through; a row written
// before the member existed passes through untouched.
func liftSubjectDisplay(detail json.RawMessage) (rest json.RawMessage, display string) {
	if !bytes.Contains(detail, []byte(auditSubjectDisplayKey)) {
		return detail, ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(detail, &m); err != nil {
		return detail, ""
	}
	raw, ok := m[auditSubjectDisplayKey]
	if !ok {
		return detail, ""
	}
	if err := json.Unmarshal(raw, &display); err != nil {
		return detail, ""
	}
	delete(m, auditSubjectDisplayKey)
	encoded, err := json.Marshal(m)
	if err != nil {
		return detail, display
	}
	return encoded, display
}

// auditDetailFor extracts action's allow-listed subset of body's top-level JSON keys; values are
// copied through unexamined -- only the KEY NAME is allow-listed.
func auditDetailFor(action string, body []byte) json.RawMessage {
	allowed, ok := auditDetailAllowlist[action]
	if !ok || len(allowed) == 0 || len(body) == 0 {
		return emptyDetail
	}
	// Matched in wire order and case-insensitively, last key winning, as encoding/json resolves the body
	// the handler acts on; otherwise {"name":"a","Name":"b"} would record a name the handler never used.
	fields, err := orderedJSONFields(body, allowed)
	if err != nil {
		return emptyDetail
	}
	out := make(map[string]json.RawMessage, len(allowed))
	for _, key := range allowed {
		for _, f := range fields {
			if strings.EqualFold(f.key, key) {
				// No break: the LAST fold-equal key is the one the handler's decoder kept.
				out[key] = boundAuditValue(scrubJSONNULs(boundRawAuditValue(f.value)))
			}
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		// Not {}: the row says the detail was lost, so the caller cannot choose to go undescribed.
		return json.RawMessage(`{"unencodable":true}`)
	}
	if len(encoded) > auditDetailMaxBytes {
		// Several bounded values can still add up; the row survives, saying so.
		return auditTruncatedDetail
	}
	return encoded
}

// Bounds on what one audit row may carry. The detail is taken from the body before validation, so a
// refused request is recorded too; unbounded, values it was never going to accept would make the log
// expensive to read.
const (
	// auditValueMaxBytes bounds ONE allow-listed value.
	auditValueMaxBytes = 512
	// auditDetailMaxBytes bounds the whole detail object.
	auditDetailMaxBytes = 4 << 10
	// auditRawValueMaxBytes bounds one allow-listed value before it is decoded: generous next to any
	// real name, scope or title, and far below the size at which decoding it becomes a denial of service.
	auditRawValueMaxBytes = 64 << 10
)

// boundRawAuditValue caps a value on its raw bytes, before anything decodes it, so a huge allow-listed
// member is never materialised.
func boundRawAuditValue(v json.RawMessage) json.RawMessage {
	if len(v) <= auditRawValueMaxBytes {
		return v
	}
	return truncatedMarker(len(v))
}

// truncatedMarker is the value recorded in place of one too large to keep, whichever bound fired.
func truncatedMarker(n int) json.RawMessage {
	return json.RawMessage(`"[truncated: ` + strconv.Itoa(n) + ` bytes]"`)
}

// boundAuditValue truncates an over-long value to auditValueMaxBytes, keeping it VALID JSON and
// saying that it was cut.
func boundAuditValue(v json.RawMessage) json.RawMessage {
	if len(v) <= auditValueMaxBytes {
		return v
	}
	// Re-encoded rather than sliced: cutting raw JSON mid-escape (or mid-rune) produces a value
	// PostgreSQL would refuse, which is the failure this whole path exists to avoid.
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		// Not a string (an object, an array): record the shape and the size, not the content.
		return truncatedMarker(len(v))
	}
	// Clamped to the decoded length: the guard above measured the encoded size, and escapes shrink by
	// up to 6x when decoded.
	cut := min(auditValueMaxBytes-32, len(s))
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	if cut == len(s) {
		// Nothing to cut after decoding: the value is only large in its encoded form.
		return v
	}
	encoded, _ := json.Marshal(s[:cut] + "…[truncated]") // a string always encodes
	return encoded
}

// jsonField is one top-level member of the request body, in the order it appeared on the wire.
type jsonField struct {
	key   string
	value json.RawMessage
}

var errNotJSONObject = errors.New("body is not a JSON object")

/*
 * orderedJSONFields decodes body's top-level object preserving KEY ORDER and DUPLICATES.
 *
 * A map[string]json.RawMessage loses both, and both are load-bearing: encoding/json keeps the LAST
 * occurrence of a fold-equal key, so the audit path has to walk the same sequence the handler's
 * decoder walked to record what the handler actually acted on.
 */
func orderedJSONFields(body []byte, allowed []string) ([]jsonField, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errNotJSONObject
	}
	fields := make([]jsonField, 0, len(allowed))
	members := 0
	for dec.More() {
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return nil, kerr
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errNotJSONObject
		}
		members++
		if members > auditBodyMaxMembers {
			return nil, errAuditBodyTooWide
		}
		// Kept only if some allow-listed key folds to it: a public body repeating one key a million times
		// must not become a million-element slice.
		if !foldContains(allowed, key) {
			var skip json.RawMessage
			if serr := dec.Decode(&skip); serr != nil {
				return nil, serr
			}
			continue
		}
		var value json.RawMessage
		if verr := dec.Decode(&value); verr != nil {
			return nil, verr
		}
		fields = append(fields, jsonField{key: key, value: value})
	}
	return fields, nil
}

// auditBodyMaxMembers bounds how many top-level members one audited body may carry. Every real body
// on an allow-listed route has a handful; a body with thousands is not a mutation this console was
// asked to describe, it is an attempt to make describing it expensive.
const auditBodyMaxMembers = 512

var errAuditBodyTooWide = errors.New("body carries more top-level members than an audited mutation can")

// foldContains reports whether keys holds one that is case-insensitively equal to key -- the same
// comparison encoding/json applies when it resolves a body key to a struct field.
func foldContains(keys []string, key string) bool {
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

/*
scrubJSONNULs removes U+0000 from an allow-listed value, which PostgreSQL refuses anywhere in a jsonb
(22P05) and so would let a caller drop their own row. It decodes rather than rewrites bytes, so an
escaped backslash followed by u0000 stays the literal text it is.
*/
func scrubJSONNULs(v json.RawMessage) json.RawMessage {
	// UseNumber: a float64 decode fails on a literal like 1e999, and json.Number neither overflows nor
	// rewrites a large integer's digits.
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		// Undecodable after all. The raw bytes may still carry a NUL escape, which PostgreSQL refuses
		// for the whole jsonb -- so the value is replaced rather than passed through. A detail that
		// says less is better than a row that does not exist.
		return json.RawMessage(`"[undecodable]"`)
	}
	/* Always encoded again, so the detail kept here, and logged if the write fails, is the value the
	   row stores: invalid UTF-8 (22021) and an unpaired surrogate escape (22P02), which PostgreSQL
	   refuses, were already turned into U+FFFD by the decode, as the handler stored them. */
	encoded, err := json.Marshal(scrubNULValue(decoded))
	if err != nil {
		return json.RawMessage(`"[unencodable]"`)
	}
	return encoded
}

// scrubNULValue walks a decoded JSON value dropping U+0000 from every string, key or leaf.
func scrubNULValue(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "\x00", "")
	case []any:
		for i, item := range t {
			t[i] = scrubNULValue(item)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for key, item := range t {
			out[strings.ReplaceAll(key, "\x00", "")] = scrubNULValue(item)
		}
		return out
	default:
		return v
	}
}

// auditTruncatedDetail is the detail of a row whose body was too large to describe.
var auditTruncatedDetail = json.RawMessage(`{"truncated":true}`)

// auditCaptureMaxBytes is the most of a body the audit reads to describe it. Far above any real body
// on an allow-listed route except an import, whose row the handler describes itself (setAuditResult).
const auditCaptureMaxBytes = 256 << 10

/*
captureAuditDetail reads at most auditCaptureMaxBytes of r's body and puts that prefix back in front
of the unread rest, so the handler still reads the whole body; called for an audited request once
s.audit is known non-nil (auditMutation).

A body over the bound is recorded as truncated, not described from its prefix: a later duplicate key
could name what the handler really acted on.
*/
func (s *Server) captureAuditDetail(r *http.Request) json.RawMessage {
	// Routes with no allow-list entry always audit {} — skip the body read
	// entirely rather than buffering (e.g. a PromQL query body) for a
	// guaranteed-empty result.
	pattern := chi.RouteContext(r.Context()).RoutePattern()
	key := r.Method + " " + pattern
	if _, ok := auditDetailAllowlist[key]; !ok {
		return emptyDetail
	}
	body := r.Body
	head, err := io.ReadAll(io.LimitReader(body, auditCaptureMaxBytes+1))
	r.Body = replayedBody{Reader: io.MultiReader(bytes.NewReader(head), body), Closer: body}
	var tooLarge *http.MaxBytesError
	switch {
	case len(head) > auditCaptureMaxBytes, errors.As(err, &tooLarge):
		return auditTruncatedDetail
	case err != nil || len(head) == 0:
		return emptyDetail
	}
	return auditDetailFor(key, head)
}

// replayedBody is a request body whose first bytes were already read and are served again.
type replayedBody struct {
	io.Reader
	io.Closer
}

// isAuditedRead reports whether this safe request is one of the privileged reads that gets its own
// audit row (auditedReads).
func (s *Server) isAuditedRead(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	return auditedReads[r.Method+" "+chi.RouteContext(r.Context()).RoutePattern()]
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
	// SubjectDisplay is the human-readable name recorded with the row (liftSubjectDisplay); absent
	// on rows written before it was captured.
	SubjectDisplay string `json:"subjectDisplay,omitempty"`
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
			databaseKnob+" to enable GET /api/v1/audit")
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

	// A NUL in a text filter is fatal to pgx, which would answer a client mistake with a 502.
	if rejectControlChars(w, "subjectKind", q.Get("subjectKind")) || rejectControlChars(w, "subjectId", q.Get("subjectId")) {
		return
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
		detail, display := liftSubjectDisplay(e.Detail)
		out = append(out, auditEntryResponse{
			ID: e.ID, At: e.At, SubjectKind: e.SubjectKind, SubjectID: e.SubjectID,
			Action: e.Action, Resource: e.Resource, Outcome: e.Outcome,
			RemoteAddr: e.RemoteAddr, Detail: detail, SubjectDisplay: display,
		})
	}
	writeJSON(w, auditResponse{Entries: out, NextCursor: page.NextCursor})
}
