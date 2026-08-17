package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	/* A DENIAL is the cheapest row to produce and the least valuable to keep, and that asymmetry was
	   a hole: a flood of rejected requests (no rate limit outside login and runs) kept this buffer
	   full, so an admin revoking a role binding at the same moment had their row silently dropped —
	   the one record of the most sensitive operation in the console, gone, behind a warning line
	   that carries neither the subject nor the resource.
	   So denials yield: they are dropped once the buffer is half full, leaving the other half for
	   rows that describe something that actually HAPPENED.

	   The first cut of this rule only made CREDENTIAL-LESS denials yield (`subject.Kind == ""`), and
	   that was two holes at once: any subject holding the least-privileged token in the system was
	   exempt and could still fill the buffer, and in anonymous mode Kind is "anonymous" rather than
	   "" — so on the chart's own demo default the rule never fired for anyone. The yield now keys on
	   what the row IS, not on who produced it, and a denial on one of the sensitive routes keeps the
	   full buffer: refused attempts at RBAC, tokens, import and export are exactly the ones an
	   investigation needs. */
	if outcome == auditOutcomeDenied && !auditSensitiveRoute(pattern) && len(s.auditCh) >= cap(s.auditCh)/2 {
		s.metrics.AuditDropped.WithLabelValues().Inc()
		s.auditDropped.Add(1)
		return
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

var auditSensitivePrefixes = []string{
	"/api/v1/rbac",
	"/api/v1/tokens",
	"/api/v1/users",
	"/api/v1/import",
	"/api/v1/export",
	"/api/v1/audit",
}

// auditedReads are the SAFE requests worth a row of their own. Auditing was keyed on the HTTP verb,
// so the single highest-value read in the API — the export, which is every probe address, every
// webhook URL, every alert expression and, for a caller holding rbac:manage, the whole role map —
// left no trace at all. A stolen token could dump it and the log an operator investigates with
// showed nothing.
// GET /api/v1/audit is deliberately NOT here: the console's own audit page polls it, and a row per
// page view would be an audit log mostly about being read. The export is a single deliberate act.
var auditedReads = map[string]bool{
	"GET /api/v1/export": true,
}

/*
auditResource extracts the one path parameter (name or id) this package's mutating routes carry,
when they carry one at all — SANITISED, because the value is the caller's and the row is not.

It used to be copied verbatim into audit_log.resource. PostgreSQL cannot store a NUL in a text
column, so the INSERT failed and the drain logged a warning and moved on: appending %00 to any id
deleted that request's audit row. The sharp case is the DENIED leg — middleware_auth records a
denial through this same function — so a refused probe of /api/v1/rbac/* or /api/v1/tokens/*, the
routes auditSensitiveRoute exists to guarantee are never dropped, became invisible to the caller's
own choosing. That is the exact property stripJSONNULs was written to defend for the body; the path
parameter had no equivalent.

Sanitised rather than rejected here: by the time this runs the request has been served, and a row
that names a mangled resource is worth immeasurably more than no row.
*/
func auditResource(r *http.Request) string {
	if name := chi.URLParam(r, "name"); name != "" {
		return sanitizeAuditText(name)
	}
	return sanitizeAuditText(chi.URLParam(r, "id"))
}

// sanitizeAuditText replaces every control character with U+FFFD, so a value the caller chose can
// never make a row unstorable. The substitution is visible on purpose: a reader of the audit log
// should be able to see that the input carried something it should not have.
func sanitizeAuditText(v string) string {
	if strings.IndexFunc(v, unicode.IsControl) < 0 {
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
	// The id of the binding that was created; the role and subject come from the request body.
	"POST /api/v1/rbac/bindings": {"bindingId"},
	// A DELETE has no body at all, so EVERYTHING an auditor needs about a revocation has to be
	// computed by the handler from the row it is about to destroy.
	"DELETE /api/v1/rbac/bindings/{id}": {"bindingId", "roleName", "subjectKind", "subjectId"},
	"POST /api/v1/import": {
		"dryRun",
		"targets", "checkDefinitions", "checkSchedules",
		"alertRules", "webhooks", "maintenanceWindows",
		/* The two access-control counters, and they are not optional: an import may MINT A CUSTOM ROLE
		   carrying rbac:manage, and without these keys that row read as six all-zero collections —
		   "this import changed nothing" — while the access map moved. Creating the same role through
		   POST /api/v1/rbac/roles has always been audited; this was the quieter path to it. Counts
		   only, like every other key here: no role name, no permission string, no subject. */
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
	/* Decoded in ORDER, matched the way encoding/json matches — not with a case-sensitive map index.
	   The handlers decode the same body into a struct, and encoding/json resolves a body key to a
	   struct field case-INSENSITIVELY, last occurrence winning. A map index does neither, so a body
	   carrying {"name":"benign", ..., "Name":"real"} let the caller write one name into the object
	   and a different one into the audit row: the sole record of a privileged mutation described an
	   action nobody performed. DisallowUnknownFields does not help — "Name" is not unknown, it is
	   the same field spelled differently, which is why even the strict routes were affected. */
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
		/* The row still says the detail was lost. Returning {} here made an unencodable value
		   indistinguishable from a route that records nothing, i.e. the caller got to choose
		   whether their own action was described. */
		return json.RawMessage(`{"unencodable":true}`)
	}
	if len(encoded) > auditDetailMaxBytes {
		// Belt and braces: several bounded values can still add up. The row survives, saying so.
		return json.RawMessage(`{"truncated":true}`)
	}
	return encoded
}

/*
 * Bounds on what one audit row may carry.
 *
 * The detail is lifted out of the REQUEST BODY before validation runs — that is the point, since a
 * refused mutation is worth recording — but nothing bounded it, so a 2 MB "name" that the handler
 * then rejected with "name is 2000000 bytes, limit is 63" was still written down in full. A handful
 * of such requests turned the DEFAULT page of GET /api/v1/audit into a multi-megabyte response, i.e.
 * a caller could make the audit log expensive to read by sending values it was never going to
 * accept. Nothing in the audit contract needs the whole value: it records what was attempted.
 */
const (
	// auditValueMaxBytes bounds ONE allow-listed value.
	auditValueMaxBytes = 512
	// auditDetailMaxBytes bounds the whole detail object.
	auditDetailMaxBytes = 4 << 10
)

/*
 * boundRawAuditValue caps a value on its RAW BYTES, before anything decodes it.
 *
 * The member bound above counts TOP-LEVEL members, so it does nothing about one member whose value
 * is enormous: `{"username":[0,0,0, ... ]}` at just under the 16 MiB request ceiling is a single
 * allow-listed member, and scrubJSONNULs then materialised it into a []any of ~5.5M elements --
 * roughly 560 MiB live for one request, on the PUBLIC login route, which OOM-killed a 256Mi console
 * pod at will. Capping the bytes first means the decode only ever runs on something small enough to
 * be worth recording, and a NUL can only hide inside a value that survived the cap.
 *
 * The shape recorded here matches boundAuditValue's own over-long marker, so a reader sees the same
 * sentence whichever bound fired.
 */
func boundRawAuditValue(v json.RawMessage) json.RawMessage {
	if len(v) <= auditRawValueMaxBytes {
		return v
	}
	return json.RawMessage(`"[truncated: ` + strconv.Itoa(len(v)) + ` bytes]"`)
}

// auditRawValueMaxBytes bounds ONE allow-listed value before it is decoded. Generous next to any
// real name/scope/title (auditValueMaxBytes then trims to 512 for the row itself) and far below the
// size at which decoding becomes a denial of service.
const auditRawValueMaxBytes = 64 << 10

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
		return json.RawMessage(`"[truncated: ` + strconv.Itoa(len(v)) + ` bytes]"`)
	}
	/* CLAMPED to the decoded length first. The guard above compares auditValueMaxBytes against
	   len(v), which is the ENCODED size, and this cut is then applied to s, the DECODED string —
	   two different lengths. A value made of escapes shrinks by up to 6x when decoded, so a body
	   carrying ~5 KB of \u0041 passed the size check and then sliced a ~830-character string at
	   index 4064: an out-of-range slice, i.e. a panic, on the audit path of an UNAUTHENTICATED
	   request (the login route is audited). The recoverer turns it into a 500, but the request is
	   still a remote panic anyone can fire at will. */
	cut := min(auditValueMaxBytes-32, len(s))
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	if cut == len(s) {
		// Nothing to cut after decoding: the value is only large in its encoded form.
		return v
	}
	encoded, err := json.Marshal(s[:cut] + "…[truncated]")
	if err != nil {
		return emptyDetail
	}
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
		/* KEPT only if some allow-listed key folds to it. The first cut of this appended EVERY
		   top-level member, duplicates included, and the audit middleware buffers the body before
		   authentication on public routes -- so one unauthenticated 16 MiB POST /api/v1/auth/login
		   whose body repeats "username" a million times built a million-element slice, ~13x the
		   body in live heap, and OOM-killed the console pod. The map this replaced collapsed those
		   duplicates to one entry; the slice has to earn its members back. */
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
 * scrubJSONNULs removes U+0000 from an allow-listed value by DECODING it, not by rewriting bytes.
 *
 * PostgreSQL rejects a NUL anywhere inside a jsonb value (22P05), and this detail is copied out of
 * the REQUEST BODY verbatim: appending such an escape to any allow-listed field ("name" on POST
 * /api/v1/webhooks, "title" on POST /api/v1/incidents, "scope" on POST /api/v1/maintenance) made the
 * whole audit row unwritable, so the attempt left no trace in GET /api/v1/audit at all. A caller
 * must not get to choose whether their own action is recorded.
 *
 * The first cut of this deleted the six-BYTE escape from the RAW JSON at any offset, which handed
 * that choice straight back: a name whose literal characters were a backslash followed by u0000 --
 * the caller escaping the backslash, so no NUL was ever present -- matched one byte late, and the
 * deletion left a dangling backslash. Invalid JSON, unencodable detail, empty row. Decoding first
 * makes the escape a character like any other, and only real NULs are dropped.
 */
func scrubJSONNULs(v json.RawMessage) json.RawMessage {
	/* UseNumber, not the default float64. encoding/json parses every JSON number into a float64, so a
	   value carrying a literal like 1e999 failed to decode -- and the error path returned the value
	   UNCHANGED, NUL escape and all. PostgreSQL then refused the jsonb (22P05) and the whole audit row
	   was dropped: the caller was handed back exactly the choice this function exists to take away.
	   json.Number never parses, so it neither overflows nor rewrites a large integer's digits. */
	dec := json.NewDecoder(bytes.NewReader(v))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		// Undecodable after all. The raw bytes may still carry a NUL escape, which PostgreSQL refuses
		// for the whole jsonb -- so the value is replaced rather than passed through. A detail that
		// says less is better than a row that does not exist.
		return json.RawMessage(`"[undecodable]"`)
	}
	scrubbed, changed := scrubNULValue(decoded)
	if !changed {
		return v
	}
	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		return json.RawMessage(`"[unencodable]"`)
	}
	return encoded
}

// scrubNULValue walks a decoded JSON value dropping U+0000 from every string, key or leaf.
func scrubNULValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if !strings.ContainsRune(t, 0) {
			return t, false
		}
		return strings.ReplaceAll(t, "\x00", ""), true
	case []any:
		changed := false
		for i, item := range t {
			scrubbed, c := scrubNULValue(item)
			t[i] = scrubbed
			changed = changed || c
		}
		return t, changed
	case map[string]any:
		changed := false
		out := make(map[string]any, len(t))
		for key, item := range t {
			scrubbed, c := scrubNULValue(item)
			cleanKey := key
			if strings.ContainsRune(key, 0) {
				cleanKey = strings.ReplaceAll(key, "\x00", "")
				c = true
			}
			out[cleanKey] = scrubbed
			changed = changed || c
		}
		return out, changed
	default:
		return v, false
	}
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

	/* A NUL in a text filter is fatal to pgx, and the handler mapped that to 502 "audit log
	   unavailable" — a 5xx for a client mistake, telling a human the store is down while it is
	   perfectly healthy. Every other filtered listing in this package already guards its text
	   params this way. */
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
		out = append(out, auditEntryResponse{
			ID: e.ID, At: e.At, SubjectKind: e.SubjectKind, SubjectID: e.SubjectID,
			Action: e.Action, Resource: e.Resource, Outcome: e.Outcome,
			RemoteAddr: e.RemoteAddr, Detail: e.Detail,
		})
	}
	writeJSON(w, auditResponse{Entries: out, NextCursor: page.NextCursor})
}
