package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/EsDmitrii/kconmon-ng/internal/console/alerting"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promql"
	"github.com/EsDmitrii/kconmon-ng/internal/console/promrules"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// RuleSyncer is the reconciler seam: ask for a reconcile now, and list the
// PrometheusRules in the console's namespace that the console does NOT own
// (M7 Decision 4). Satisfied by *promrules.Reconciler.
//
// ONE interface rather than a separate kicker and lister, because the two are
// never separately available: both come off the same reconciler, and
// cmd/console builds it or does not. Two Deps fields could go out of sync and
// would give this package two nil checks to tell the same story with.
//
// Kick returns nothing and cannot fail on purpose -- it is a non-blocking,
// coalescing nudge (promrules.Reconciler.Kick), and a handler must never wait
// on a Kubernetes round trip to answer a database write.
type RuleSyncer interface {
	Kick()
	ListForeign(ctx context.Context) ([]promrules.ForeignRule, error)
}

var _ RuleSyncer = (*promrules.Reconciler)(nil)

// alertRulesUnavailableDetail is served whenever s.alertRules is nil: alert
// rules are persisted CONFIGURATION and get no in-memory fallback,
// targetsUnavailableDetail's rule.
const alertRulesUnavailableDetail = "alert rules are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/alert-rules"

// alertingDisabledDetail is served whenever s.ruleSync is nil, and it is a 409
// rather than a 503 -- the ONE place in this API where those two come apart,
// so the distinction is worth stating.
//
// 503 in this package always means "the dependency this route reads from is
// not configured": the database is off, the controller is unset, Prometheus is
// unset. That is not what is happening here. The database is fine, every rule
// is right where it was, and the list, the builder and the whole CRUD surface
// keep working. What is off is the SYNC LOOP -- an opt-in feature
// (console.alerting.enabled, default false) whose absence means the console is
// deliberately not talking to Kubernetes at all.
//
// 409 Conflict says exactly that: the request is well-formed and the resource
// exists, but it conflicts with the state this console is configured to be in.
// Answering 503 would send an operator looking at their database for a
// PrometheusRule reconciler that was never asked to start.
const alertingDisabledDetail = "prometheus rule sync is not running on this console: the alert rules " +
	"themselves are unaffected and stay readable and editable, but nothing is applying them to the " +
	"cluster -- set console.alerting.enabled=true (Helm: console.alerting.enabled) on a console running " +
	"in-cluster with the PrometheusRule CRD present"

// promUnconfiguredDetail is the sentence the PREVIEW route reports inside a
// 200 body rather than as a problem. See handleAlertRulesPreview.
const promUnconfiguredDetail = "prometheus is not configured on this console, so the expression could not " +
	"be evaluated: set prometheus.url in the console config (Helm: console.prometheus.url)"

// alertRuleValidationPrefix is the prefix store.AlertRuleInput.Validate builds
// every one of its errors with.
const alertRuleValidationPrefix = "store: alert rule: "

// alertQueryTimeout bounds the ONE Prometheus round trip the preview and the
// firing list each make. Deliberately short and independent of the promql
// client's own guard: both routes are interactive -- somebody is watching a
// form or an Overview card -- and a slow Prometheus must degrade to a partial
// answer quickly rather than hold a request open for the client's full query
// timeout.
const alertQueryTimeout = 5 * time.Second

// alertsUpstreamMaxLen bounds an upstream error string before it is placed in
// a preview body. Prometheus parse errors are short; a pathological one must
// not become the response.
const alertsUpstreamMaxLen = 512

// alertRulesUnavailable answers 503 and reports true when no AlertRuleService
// is wired.
func (s *Server) alertRulesUnavailable(w http.ResponseWriter) bool {
	if s.alertRules == nil {
		writeProblem(w, http.StatusServiceUnavailable, "alert rules not available", alertRulesUnavailableDetail)
		return true
	}
	return false
}

// alertingDisabled answers 409 and reports true when no RuleSyncer is wired.
// Ordered AFTER alertRulesUnavailable at every call site: a console with no
// database has no rules to sync in the first place, so naming the feature flag
// there would be the less actionable of two true statements.
func (s *Server) alertingDisabled(w http.ResponseWriter) bool {
	if s.ruleSync == nil {
		writeProblem(w, http.StatusConflict, "prometheus rule sync is disabled", alertingDisabledDetail)
		return true
	}
	return false
}

// renderer builds the expression renderer from the SAME metric prefix this
// console publishes under. It is constructed per call rather than held on the
// Server: alerting.Renderer is an immutable value over one string the config
// already carries, so a field would be a second copy of a value with no state
// (and cmd/console would have to remember to wire it).
//
// Consequence worth naming: a deployment that renamed its metric families
// renders rules over the families it actually publishes, never the package
// default -- the same reason cmd/console hands cfg.MetricsPrefix to the
// reconciler's renderer (M7 Task 3).
func (s *Server) renderer() alerting.Renderer {
	return alerting.NewRenderer(s.cfg.MetricsPrefix)
}

// alertRuleResponse is one rule on the wire: the builder fields an operator
// typed, the expression the SERVER rendered from them, and the reconciler's
// view of whether the cluster agrees.
//
// renderedExpr is returned even though it is derived, because it is the one
// field an operator cannot compute themselves and the one the drift view
// diffs against. syncStatus/syncMessage/lastSyncedAt are READ-ONLY here --
// there is no request field for any of them, by construction: they are the
// reconciler's outcomes (store.AlertRuleInput carries the builder half and
// only that half).
type alertRuleResponse struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Params       json.RawMessage `json:"params"`
	Severity     string          `json:"severity"`
	ForNs        int64           `json:"forNs"`
	Labels       json.RawMessage `json:"labels"`
	Annotations  json.RawMessage `json:"annotations"`
	Enabled      bool            `json:"enabled"`
	RenderedExpr string          `json:"renderedExpr"`
	SyncStatus   string          `json:"syncStatus"`
	SyncMessage  string          `json:"syncMessage"`
	LastSyncedAt *time.Time      `json:"lastSyncedAt,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

func alertRuleResponseFrom(r *store.AlertRule) alertRuleResponse {
	return alertRuleResponse{
		ID: r.ID, Name: r.Name, Kind: r.Kind, Params: orEmptyJSONObject(r.Params),
		Severity: r.Severity, ForNs: r.ForNs,
		Labels: orEmptyJSONObject(r.Labels), Annotations: orEmptyJSONObject(r.Annotations),
		Enabled: r.Enabled, RenderedExpr: r.RenderedExpr,
		SyncStatus: r.SyncStatus, SyncMessage: r.SyncMessage, LastSyncedAt: r.LastSyncedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// orEmptyJSONObject keeps the three JSONB columns as {} rather than null on
// the wire: the frontend indexes all three by key, and `null` would be a
// runtime error in the one place a labels editor opens.
func orEmptyJSONObject(raw json.RawMessage) json.RawMessage {
	if strings.TrimSpace(string(raw)) == "" {
		return json.RawMessage(`{}`)
	}
	return raw
}

// alertRulesListResponse is GET /api/v1/alert-rules's body. UNPAGED, and
// therefore carrying no nextCursor: store.ListAlertRules is unpaged for
// ListWebhooks' reason -- the row count is rules an operator configured, not a
// function of how long the system has been running.
type alertRulesListResponse struct {
	Rules []alertRuleResponse `json:"rules"`
}

// alertRuleRequest is POST /api/v1/alert-rules's, PUT /{id}'s and
// /preview's body: the BUILDER fields, and only those. Both writes are FULL
// REPLACES -- there is deliberately no PATCH here. The incidents PATCH is the
// one exception in this API and stays unique to incidents, for the reason
// handleIncidentsUpdate states: an incident evolves under collaboration, so a
// full replace would let the last writer discard a colleague's notes. An alert
// rule has no such property. It is a definition one person edits in a form,
// and a full replace is what makes "what you see is what is stored" true.
//
// Enabled is a pointer for webhookRequest's reason: an omitted enabled means
// TRUE (a rule you just declared is one you want evaluated), not Go's false
// zero value.
type alertRuleRequest struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Params      json.RawMessage `json:"params"`
	Severity    string          `json:"severity"`
	ForNs       int64           `json:"forNs"`
	Labels      json.RawMessage `json:"labels"`
	Annotations json.RawMessage `json:"annotations"`
	Enabled     *bool           `json:"enabled"`
}

// syncKickResponse is POST /api/v1/alert-rules/{id}/sync's body.
type syncKickResponse struct {
	Status string `json:"status"`
}

// foreignRuleResponse is ONE PrometheusRule in the namespace that this console
// does not own, PROJECTED down to four facts.
//
// The raw object is deliberately NOT served, even though promrules.ForeignRule
// carries it. It is somebody else's object: its annotations can hold anything
// their tooling put there, its expressions name their infrastructure, and
// nothing this console does with it needs more than "what is it called, how
// big is it, and who claims it". An import (M7 Decision 4) reads the groups
// server-side; the browser never needs them.
type foreignRuleResponse struct {
	Name   string `json:"name"`
	Groups int    `json:"groups"`
	Rules  int    `json:"rules"`
	// ManagedBy is app.kubernetes.io/managed-by, or "" when the object carries
	// no such label -- "managed by some other chart" and "managed by nobody"
	// are different facts for an operator deciding whether to import.
	ManagedBy string `json:"managedBy"`
}

type foreignRulesResponse struct {
	Foreign []foreignRuleResponse `json:"foreign"`
}

// alertRuleImportRequest is POST /api/v1/alert-rules/import's body: the NAME
// of a foreign PrometheusRule object, and nothing else.
//
// One field, because there is exactly one decision to make. There is no
// "which groups", no "rename to" and no dryRun: adoption reads an object the
// operator just saw on GET /foreign and reports, item by item, what it did --
// which is the dry run and the apply in one round trip, for an operation whose
// entire output is at most a few dozen rows.
type alertRuleImportRequest struct {
	Name string `json:"name"`
}

// alertRuleImportSkip is one rule entry that did NOT become a row, and why.
// Name is the entry's own alert (or record) name as the foreign object spells
// it -- including the spellings this store would refuse, because the operator
// has to find that line in their object.
type alertRuleImportSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// alertRuleImportNote is one rule that WAS imported and about which the
// console had to make a choice. The distinction from a skip is the whole
// reason there are two lists: a skip means "this is not in your console", a
// note means "this is in your console, and it is not byte-identical to what
// the object said". Collapsing them would make an operator re-check rules that
// imported perfectly.
type alertRuleImportNote struct {
	Name string `json:"name"`
	Note string `json:"note"`
}

// alertRuleImportResponse is the import's report, and the report IS the
// result: 0 created with a dozen skips is a 200, because "every rule in that
// object is a recording rule" is the useful answer and no status code can
// carry it.
//
// All three slices are non-nil on the wire for orEmptyJSONObject's reason: the
// UI renders all three, and a null would be a runtime error in the one place
// somebody looks after pressing Import.
type alertRuleImportResponse struct {
	Created []string              `json:"created"`
	Skipped []alertRuleImportSkip `json:"skipped"`
	Notes   []alertRuleImportNote `json:"notes"`
}

func newAlertRuleImportResponse() alertRuleImportResponse {
	return alertRuleImportResponse{
		Created: []string{},
		Skipped: []alertRuleImportSkip{},
		Notes:   []alertRuleImportNote{},
	}
}

func (r *alertRuleImportResponse) skip(name, reason string) {
	r.Skipped = append(r.Skipped, alertRuleImportSkip{Name: name, Reason: reason})
}

func (r *alertRuleImportResponse) note(name, note string) {
	r.Notes = append(r.Notes, alertRuleImportNote{Name: name, Note: note})
}

// alertRulePreviewResponse is POST /api/v1/alert-rules/preview's body.
//
// It has TWO halves that fail independently, and that is the whole design (M7
// Decision 2: there is no Prometheus parser dependency, so "is this expression
// valid" is answered by running it). The render either produced an expression
// or it did not; the instant query either produced a series count or it did
// not. Only the first failure is a status code -- see the handler.
type alertRulePreviewResponse struct {
	Expr   string `json:"expr"`
	Series int    `json:"series"`
	// Error is set exactly when the QUERY half did not run or did not answer.
	// It is never set for a render failure, which is a 422 with no body of this
	// shape at all.
	Error string `json:"error,omitempty"`
}

// firingAlert is one entry of GET /api/v1/alerts, mapped from Prometheus's own
// /api/v1/alerts shape (M7 Decision 6).
//
// name and severity are LIFTED off the label set rather than left in it,
// because every consumer needs them and digging them out of a map at three
// call sites is how a UI ends up with three spellings. Labels keeps them too:
// it is the upstream set, verbatim, and quietly deleting keys from it would
// make this a different alert than the one Prometheus is firing.
type firingAlert struct {
	Name        string            `json:"name"`
	State       string            `json:"state"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	ActiveAt    *time.Time        `json:"activeAt,omitempty"`
	// Value is Prometheus's sample value as a STRING, verbatim ("7e+00"). Not
	// parsed to a float: the upstream field is a string precisely because it
	// carries NaN and ±Inf, and re-encoding those through JSON would produce
	// either a lie or a marshal error.
	Value string `json:"value"`
	// RuleID is the alert_rules row this alert came from, lifted off the
	// kconmon_ng_rule_id label the renderer stamps on every managed rule. Empty
	// for an alert this console does not manage -- which is a fact worth
	// serving, not a reason to hide the alert.
	RuleID string `json:"ruleId,omitempty"`
}

// alertsResponse is GET /api/v1/alerts's body.
//
// promConfigured is IN THE BODY, not implied by a status code, and that is the
// point of this route's degraded shape: with no Prometheus wired the answer is
// 200 with an empty list and promConfigured:false, not 503. The consumer is the
// Overview card, and "nothing is firing" and "nobody is watching" are two
// different sentences it must be able to render without treating one of them as
// an error state.
//
// This DIVERGES from GET /api/v1/matrix, which answers 503 for the same missing
// dependency, and the difference is deliberate: the matrix IS the Prometheus
// data, so without it there is no answer at all, while the firing set is a
// LIST that is legitimately empty and whose emptiness has two causes this field
// tells apart. A Prometheus that is configured and FAILS is neither: that is a
// real upstream failure and stays a 502.
type alertsResponse struct {
	Alerts         []firingAlert `json:"alerts"`
	PromConfigured bool          `json:"promConfigured"`
}

// alertRuleIDFrom resolves the {id} path parameter, answering 404 and
// reporting false for anything that is not a canonical UUID -- targetIDFrom's
// reasoning.
func alertRuleIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "alert rule not found", "no alert rule with that id")
		return "", false
	}
	return id, true
}

// writeAlertRuleStoreError maps an AlertRuleService error to a response.
// ErrAlreadyExists is 422 rather than 409, writeTargetStoreError's reason: a
// duplicate name is a rejected FIELD VALUE in an otherwise well-formed body,
// and the caller's fix is to change the name and resend. (409 on this resource
// means something else entirely -- see alertingDisabledDetail.)
func writeAlertRuleStoreError(w http.ResponseWriter, name, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "alert rule not found", "no alert rule with that id")
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule",
			"alert rule: name "+strconv.Quote(name)+" is already taken; alert rule names are unique, "+
				"case-insensitively")
	case strings.HasPrefix(err.Error(), alertRuleValidationPrefix):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule", publicValidationDetail(err))
	default:
		slog.Error("httpapi: alert rule store call failed", "alertRule", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "alert rules unavailable", "failed to reach the alert rule store")
	}
}

// decodeAlertRuleRequest reads a create/update/preview body. A body that is not
// JSON at all is a 400; a well-formed body whose VALUES break a rule is a 422 --
// decodeTargetRequest's distinction.
func decodeAlertRuleRequest(w http.ResponseWriter, r *http.Request) (alertRuleRequest, bool) {
	var req alertRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`an alert rule body must be JSON with "name", "kind" (one of pair-loss, zone-latency, `+
				`dns-failures, http-ttfb, agent-missing, external-target-down, raw), "params" (an object, `+
				`closed per kind), "severity" (info|warning|critical), "forNs" (nanoseconds), optional `+
				`"labels"/"annotations" objects and an optional "enabled"`)
		return alertRuleRequest{}, false
	}
	return req, true
}

// alertRuleInputFrom turns a decoded request into the store's write payload,
// RENDERING the expression first.
//
// The order is the contract: render, then store. A row whose rendered_expr came
// from a different version of its own params is exactly what
// store.AlertRuleInput's doc comment refuses to allow, and the only way to keep
// that true is for the one HTTP write path to render on the way in. It also
// means an unrenderable rule is rejected AT WRITE TIME with a 422 naming the
// param, rather than becoming a stored row that first reports a problem a
// minute later as a per-rule sync error nobody is looking at.
//
// The render input is built from the SAME decoded body the store write uses,
// so the two can never disagree about what was asked for.
func (s *Server) alertRuleInputFrom(w http.ResponseWriter, req *alertRuleRequest) (store.AlertRuleInput, bool) {
	in := store.AlertRuleInput{
		Name: req.Name, Kind: req.Kind, Params: req.Params,
		Severity: req.Severity, ForNs: req.ForNs,
		Labels: req.Labels, Annotations: req.Annotations,
		Enabled: enabledOrDefault(req.Enabled),
	}
	// Store validation FIRST, so a bad severity or a bad name is reported as
	// itself rather than as whatever the renderer happens to say about a body
	// it was never going to accept. The kind check lives on both sides on
	// purpose: store's set is the column's CHECK constraint and the renderer's
	// is the set of templates that exist, and they differ (cert-expiry is in
	// the column and has no template) -- so a rule can pass one and fail the
	// other, which is precisely the case the next call catches.
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule", publicValidationDetail(err))
		return store.AlertRuleInput{}, false
	}

	expr, ok := s.renderAlertRule(w, req)
	if !ok {
		return store.AlertRuleInput{}, false
	}
	in.RenderedExpr = expr
	return in, true
}

// renderAlertRule renders one request body into PromQL, answering 422 and
// reporting false on failure. The renderer's errors already name the param
// that caused them ("params.thresholdPercent is required"), which is why they
// are surfaced verbatim: the caller typed that param a second ago.
func (s *Server) renderAlertRule(w http.ResponseWriter, req *alertRuleRequest) (string, bool) {
	params, err := decodeJSONObjectMap(req.Params)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule", "alert rule: "+err.Error())
		return "", false
	}
	labels, err := decodeJSONStringMap("labels", req.Labels)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule", "alert rule: "+err.Error())
		return "", false
	}
	annotations, err := decodeJSONStringMap("annotations", req.Annotations)
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule", "alert rule: "+err.Error())
		return "", false
	}
	expr, err := s.renderer().Render(alerting.Rule{
		Name: req.Name, Kind: req.Kind, Params: params,
		Severity: req.Severity, ForNS: req.ForNs,
		Labels: labels, Annotations: annotations,
	})
	if err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid alert rule",
			"alert rule: cannot render an expression from these fields: "+err.Error())
		return "", false
	}
	return expr, true
}

// decodeJSONObjectMap turns a JSONB-shaped payload into the renderer's map. An
// empty or null payload is an empty map, not an error: the store folds those
// into {} on the way in, and agent-missing genuinely takes no params.
func decodeJSONObjectMap(raw json.RawMessage) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errors.New("params must be a JSON object")
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// decodeJSONStringMap is decodeJSONObjectMap for the two payloads whose values
// must all be strings. A non-string value NAMES ITS KEY: the operator has to
// find it in a JSON object they typed.
func decodeJSONStringMap(field string, raw json.RawMessage) (map[string]string, error) {
	obj, err := decodeJSONObjectMap(raw)
	if err != nil {
		return nil, errors.New(field + " must be a JSON object")
	}
	if len(obj) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(obj))
	for key, value := range obj {
		s, ok := value.(string)
		if !ok {
			return nil, errors.New(field + "[" + strconv.Quote(key) + "] must be a string")
		}
		out[key] = s
	}
	return out, nil
}

// kickSync nudges the reconciler after a successful write, so an operator does
// not wait out the jittered 60s loop to see their rule applied. A complete
// no-op when alerting is off -- the CRUD routes deliberately do NOT gate on the
// reconciler being present (that is what makes rules editable on a console with
// no cluster access at all), so this must tolerate a nil seam rather than
// assume one.
func (s *Server) kickSync() {
	if s.ruleSync != nil {
		s.ruleSync.Kick()
	}
}

// handleAlertRulesList serves every configured rule, ordered by name.
func (s *Server) handleAlertRulesList(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	rules, err := s.alertRules.ListAlertRules(r.Context(), false)
	if err != nil {
		slog.Error("list alert rules failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "alert rules unavailable", "failed to query alert rules")
		return
	}
	out := make([]alertRuleResponse, 0, len(rules))
	for i := range rules {
		out = append(out, alertRuleResponseFrom(&rules[i]))
	}
	writeJSON(w, alertRulesListResponse{Rules: out})
}

// handleAlertRulesCreate declares one rule: 201 with a Location header. The
// expression is rendered BEFORE the row is written (alertRuleInputFrom), and
// the reconciler is kicked after.
func (s *Server) handleAlertRulesCreate(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	req, ok := decodeAlertRuleRequest(w, r)
	if !ok {
		return
	}
	in, ok := s.alertRuleInputFrom(w, &req)
	if !ok {
		return
	}
	rule, err := s.alertRules.CreateAlertRule(r.Context(), in)
	if err != nil {
		writeAlertRuleStoreError(w, in.Name, "", err)
		return
	}
	s.kickSync()
	w.Header().Set("Location", "/api/v1/alert-rules/"+rule.ID)
	writeJSONStatus(w, http.StatusCreated, alertRuleResponseFrom(&rule))
}

// handleAlertRulesGet serves one rule.
func (s *Server) handleAlertRulesGet(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	id, ok := alertRuleIDFrom(w, r)
	if !ok {
		return
	}
	rule, err := s.alertRules.GetAlertRule(r.Context(), id)
	if err != nil {
		writeAlertRuleStoreError(w, "", id, err)
		return
	}
	writeJSON(w, alertRuleResponseFrom(&rule))
}

// handleAlertRulesUpdate replaces one rule's builder fields IN FULL and
// re-renders the expression from them.
//
// PUT, not PATCH, and that is a deliberate non-exception: see
// alertRuleRequest's doc comment for why the incidents PATCH stays the only
// one in this API. Omitting a field here CLEARS it -- omitting "labels" is how
// you remove every label, not how you keep them -- which is the whole meaning
// of a full replace and the reason the form always sends the complete rule.
//
// The store resets the row to sync_status=unsynced on every update (its own
// query comment says why: an edited rule is by definition not the rule that was
// applied), so the response's syncStatus flips back the moment anything
// changes, before the kick below has had time to do anything.
func (s *Server) handleAlertRulesUpdate(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	id, ok := alertRuleIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeAlertRuleRequest(w, r)
	if !ok {
		return
	}
	in, ok := s.alertRuleInputFrom(w, &req)
	if !ok {
		return
	}
	rule, err := s.alertRules.UpdateAlertRule(r.Context(), id, in)
	if err != nil {
		writeAlertRuleStoreError(w, in.Name, id, err)
		return
	}
	s.kickSync()
	writeJSON(w, alertRuleResponseFrom(&rule))
}

// handleAlertRulesDelete removes one rule. Deleting one that is not there is
// 404, not success.
//
// The row is what goes away here; the CLUSTER catches up on the next reconcile,
// which re-renders the bundle from the enabled rules that remain and applies it
// (promrules renders ONE object holding all rules, so a deletion is a smaller
// bundle, never a delete call). The kick makes that "in a moment" rather than
// "within the jittered interval", and on a console with alerting off it is a
// no-op -- the rule is gone from the database either way.
func (s *Server) handleAlertRulesDelete(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	id, ok := alertRuleIDFrom(w, r)
	if !ok {
		return
	}
	if err := s.alertRules.DeleteAlertRule(r.Context(), id); err != nil {
		writeAlertRuleStoreError(w, "", id, err)
		return
	}
	s.kickSync()
	w.WriteHeader(http.StatusNoContent)
}

// handleAlertRulesSync asks the reconciler for a pass now.
//
// 202, not 200: Kick is a non-blocking coalescing nudge, so the only honest
// thing this can report is that the work was requested. The OUTCOME lands on
// the rules themselves and is read back from GET /api/v1/alert-rules --
// syncStatus/syncMessage/lastSyncedAt.
//
// The route is per-rule ({id}) but the reconcile is WHOLE-BUNDLE, because the
// bundle is one object (M7 Task 2: RenderBundle produces a single
// PrometheusRule holding every enabled rule). The id is not a filter; it is
// the rule the operator was looking at when they pressed the button, and it is
// what the audit row records. Existence is still checked, on
// handleWebhooksTest's precedent: answering 202 for an id that names nothing
// would promise an outcome that never arrives.
func (s *Server) handleAlertRulesSync(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	id, ok := alertRuleIDFrom(w, r)
	if !ok {
		return
	}
	if s.alertingDisabled(w) {
		return
	}
	if _, err := s.alertRules.GetAlertRule(r.Context(), id); err != nil {
		writeAlertRuleStoreError(w, "", id, err)
		return
	}
	s.ruleSync.Kick()
	writeJSONStatus(w, http.StatusAccepted, syncKickResponse{Status: "kicked"})
}

// handleAlertRulesForeign lists the PrometheusRules in the console's namespace
// that it does not own (M7 Decision 4). READ-ONLY in the strongest sense: this
// console never mutates a foreign object under any circumstance, and adoption
// is an explicit import that copies the groups into builder rows and creates a
// NEW object.
//
// It needs no database -- the answer comes from the cluster -- but it does need
// the reconciler, so a console with alerting off answers 409 naming the flag,
// exactly like the sync route.
func (s *Server) handleAlertRulesForeign(w http.ResponseWriter, r *http.Request) {
	if s.alertingDisabled(w) {
		return
	}
	found, err := s.ruleSync.ListForeign(r.Context())
	if err != nil {
		// Logged, never surfaced: a Kubernetes API error carries the request
		// URL, the ServiceAccount's identity and whatever the apiserver felt
		// like saying, none of which belongs in an HTTP response body.
		slog.Error("list foreign prometheus rules failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "foreign rules unavailable",
			"failed to list PrometheusRule objects in the console's namespace")
		return
	}
	out := make([]foreignRuleResponse, 0, len(found))
	for i := range found {
		out = append(out, foreignRuleResponse{
			Name: found[i].Name, Groups: found[i].Groups,
			Rules: found[i].Rules, ManagedBy: found[i].ManagedBy,
		})
	}
	writeJSON(w, foreignRulesResponse{Foreign: out})
}

// foreignRuleNotFoundDetail is the ONE 404 this route serves, and it covers
// two different situations on purpose: an object nobody has, and an object
// this console OWNS. ListForeign is the only lookup the import performs and it
// excludes anything carrying the managed-by label, so a bundle of ours is
// simply not in the set being searched. Telling the two apart would mean a
// second, unfiltered read of the namespace for no benefit -- and re-adopting
// our own bundle would produce a duplicate of every rule already in the
// database.
const foreignRuleNotFoundDetail = "no foreign PrometheusRule with that name in the console's namespace; " +
	"list them with GET /api/v1/alert-rules/foreign (an object this console already owns is not foreign)"

// recordingRuleSkipReason names the one shape of rule entry the builder has no
// model for at all. It is not a rendering failure or a validation failure: a
// recording rule produces a time series, an alert rule produces an alert, and
// alert_rules has no column that could mean the first.
const recordingRuleSkipReason = "recording rule -- the console builder has no recording model, only alerting rules"

// handleAlertRulesImport ADOPTS a foreign PrometheusRule (M7 Decision 4): it
// COPIES the object's alerting rules into console-managed builder rows.
//
// The foreign object is never mutated and never deleted, by this handler or by
// anything it calls. There is no code path from this route to a write against
// somebody else's object -- it reads the copy ListForeign already handed back
// and writes only to the alert_rules table. After a successful adoption the
// console's OWN bundle grows by the adopted rules and is applied on the next
// reconcile, at which point the same alerts are defined twice in the cluster:
// once by the object its owner still controls, once by ours. Deleting theirs
// is THEIR decision, and the console will not make it for them.
//
// Per-item and NON-TRANSACTIONAL, the import-bundle precedent (handleImport):
// each entry is created on its own, and one entry the store refuses does not
// roll back the ones before it. The response says exactly which is which.
//
// Both dependency gates apply, in the CRUD order: no store is 503 (there is
// nowhere to adopt TO), and no reconciler is 409 naming the feature flag
// (there is nothing to adopt FROM -- this route reads the cluster through the
// same seam GET /foreign does).
func (s *Server) handleAlertRulesImport(w http.ResponseWriter, r *http.Request) {
	if s.alertRulesUnavailable(w) {
		return
	}
	if s.alertingDisabled(w) {
		return
	}

	var req alertRuleImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`an import body must be JSON with "name", the name of a PrometheusRule object in the console's namespace`)
		return
	}
	// A blank name is a rejected field VALUE rather than a 404: answering "no
	// foreign rule is called nothing" would send the caller looking at their
	// cluster for a bug that is in their request.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid import",
			`"name" must name a PrometheusRule object in the console's namespace; `+
				"list them with GET /api/v1/alert-rules/foreign")
		return
	}

	found, err := s.ruleSync.ListForeign(r.Context())
	if err != nil {
		// Logged, never surfaced -- handleAlertRulesForeign's reasoning.
		slog.Error("list foreign prometheus rules failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "foreign rules unavailable",
			"failed to list PrometheusRule objects in the console's namespace")
		return
	}
	var source *promrules.ForeignRule
	for i := range found {
		if found[i].Name == name {
			source = &found[i]
			break
		}
	}
	if source == nil || source.Object == nil {
		writeProblem(w, http.StatusNotFound, "foreign rule not found", foreignRuleNotFoundDetail)
		return
	}

	// The name set is read ONCE, up front, and maintained in memory as rows are
	// created. Asking the store per entry would be N queries to answer a
	// question one query answers, and would still not be atomic: the uniqueness
	// that actually holds is the lower(name) index, and CreateAlertRule's
	// ErrAlreadyExists is the backstop this map only makes legible.
	existing, err := s.alertRules.ListAlertRules(r.Context(), false)
	if err != nil {
		slog.Error("list alert rules failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "alert rules unavailable", "failed to query alert rules")
		return
	}
	taken := make(map[string]bool, len(existing))
	for i := range existing {
		taken[strings.ToLower(existing[i].Name)] = true
	}

	report := s.adoptForeignRule(r.Context(), source.Object, taken)
	if len(report.Created) > 0 {
		s.kickSync()
	}
	writeJSON(w, report)
}

// adoptForeignRule walks spec.groups[].rules[] and adopts what it can.
//
// A malformed group or entry is SKIPPED, never fatal -- countGroups' posture in
// promrules, for the same reason: this is a read of somebody else's object, and
// refusing the whole import because one entry is the wrong shape would hide the
// rules that were perfectly adoptable. Group names are not carried: the console
// renders ONE group of its own (alerting.GroupName), so a foreign group name
// has nowhere to live and inventing a column for it would be a schema change
// serving no reader.
func (s *Server) adoptForeignRule(
	ctx context.Context, obj *unstructured.Unstructured, taken map[string]bool,
) alertRuleImportResponse {
	report := newAlertRuleImportResponse()
	groups, found, err := unstructured.NestedSlice(obj.Object, "spec", "groups")
	if !found || err != nil {
		return report
	}
	renderer := s.renderer()
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, ok := gm["rules"].([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			s.adoptRuleEntry(ctx, renderer, em, taken, &report)
		}
	}
	return report
}

// adoptRuleEntry turns ONE spec.groups[].rules[] entry into a row, or records
// why it did not.
//
//nolint:gocognit,gocyclo // one linear gate chain: every branch is one adoption rule stated once, and splitting it would scatter the skip vocabulary across helpers
func (s *Server) adoptRuleEntry(
	ctx context.Context, renderer alerting.Renderer,
	entry map[string]any, taken map[string]bool, report *alertRuleImportResponse,
) {
	if record, ok := entry["record"].(string); ok && strings.TrimSpace(record) != "" {
		report.skip(record, recordingRuleSkipReason)
		return
	}
	// The alert name is used AS IS or not at all. SanitizeAlertName exists and
	// is deliberately NOT called here: it is for names an operator typed into
	// the builder and can see being transformed, and applying it silently
	// during an adoption would store somebody's rule under a name they never
	// wrote and cannot search for.
	name, _ := entry["alert"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		report.skip("", "rule entry has neither an alert nor a record field")
		return
	}
	expr, _ := entry["expr"].(string)
	if strings.TrimSpace(expr) == "" {
		report.skip(name, "rule entry has no expr, or its expr is not a string")
		return
	}
	if taken[strings.ToLower(name)] {
		report.skip(name, alertRuleNameTakenReason)
		return
	}

	labels, err := foreignStringMap(entry, "labels")
	if err != nil {
		report.skip(name, err.Error())
		return
	}
	annotations, err := foreignStringMap(entry, "annotations")
	if err != nil {
		report.skip(name, err.Error())
		return
	}
	severity := liftSeverity(name, labels, report)
	// Both reserved names come out. severity is LIFTED into its own column, so
	// leaving it in the map would make one fact editable in two places and let
	// them disagree; the rule id is the renderer's, and a stale uuid from
	// somebody else's copy of our rule would make the firing list attribute
	// alerts to a row that never produced them.
	delete(labels, alerting.SeverityLabel)
	if _, present := labels[alerting.RuleIDLabel]; present {
		report.note(name, "the object carried the reserved "+alerting.RuleIDLabel+
			" label; it was dropped -- this console stamps its own rule id on every rule it manages")
		delete(labels, alerting.RuleIDLabel)
	}

	forNS, err := foreignForNS(entry)
	if err != nil {
		report.skip(name, err.Error())
		return
	}

	params, err := json.Marshal(map[string]string{"expr": expr})
	if err != nil {
		report.skip(name, "expr could not be encoded as a rule parameter")
		return
	}
	in := store.AlertRuleInput{
		Name: name, Kind: store.AlertRuleKindRaw, Params: params,
		Severity: severity, ForNs: forNS,
		Labels: mustMarshalStringMap(labels), Annotations: mustMarshalStringMap(annotations),
		Enabled: true,
	}
	// Store validation first, then the render -- alertRuleInputFrom's order, so
	// a rejected NAME is reported as a name and not as whatever the renderer
	// says about a rule it was never going to accept.
	if err = in.Validate(); err != nil {
		report.skip(name, publicValidationDetail(err))
		return
	}
	rendered, err := renderer.Render(alerting.Rule{
		Name: name, Kind: store.AlertRuleKindRaw, Params: map[string]any{"expr": expr},
		Severity: severity, ForNS: forNS, Labels: labels, Annotations: annotations,
	})
	if err != nil {
		report.skip(name, "cannot render an expression from this rule: "+err.Error())
		return
	}
	in.RenderedExpr = rendered

	row, err := s.alertRules.CreateAlertRule(ctx, in)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			report.skip(name, alertRuleNameTakenReason)
			return
		}
		// Any other store failure is per-item too, importAlertRules' precedent:
		// the import is non-transactional by design, and a report naming the
		// rules that did land beats a 502 that throws the whole outcome away.
		report.skip(name, publicValidationDetail(err))
		return
	}
	taken[strings.ToLower(row.Name)] = true
	report.Created = append(report.Created, row.Name)
}

// alertRuleNameTakenReason is one sentence for two collisions -- a rule already
// in the database, and a rule this same import created a moment ago -- because
// they are the same constraint: migration 00007's lower(name) index.
const alertRuleNameTakenReason = "name already taken: alert rule names are unique case-insensitively, " +
	"and adoption will not rename a rule to make room for it"

// liftSeverity reads labels.severity, falling back to warning and SAYING SO.
//
// A note rather than a skip, and a note in BOTH fallback cases (absent label,
// and a value outside the closed set): the rule imports either way, and what
// the operator needs to know is not "this failed" but "this one field is the
// console's guess, not your object's". warning is the fallback because it is
// the middle of the three -- guessing info would quietly demote a page, and
// guessing critical would invent one.
func liftSeverity(name string, labels map[string]string, report *alertRuleImportResponse) string {
	raw, present := labels[alerting.SeverityLabel]
	switch raw {
	case store.AlertSeverityInfo, store.AlertSeverityWarning, store.AlertSeverityCritical:
		return raw
	}
	if present {
		report.note(name, "labels.severity was "+strconv.Quote(raw)+
			", which is not one of info, warning, critical: imported as warning")
	} else {
		report.note(name, "the rule carries no severity label: imported as warning")
	}
	return store.AlertSeverityWarning
}

// foreignForNS reads a rule entry's `for`, in nanoseconds. Absent is 0 -- which
// is what Prometheus itself means by an absent `for`.
//
// An unreadable one is an ERROR (and therefore a skip at the call site) rather
// than a fallback to 0, and that is the single most consequential choice in
// this importer: 0 means "fire the instant the expression holds", so a
// misparsed 5m would turn a deliberately patient rule into a pager at 3am,
// silently, in an import nobody re-reads. A skipped rule is visible in the
// report; a wrongly-imported one is not.
func foreignForNS(entry map[string]any) (int64, error) {
	raw, present := entry["for"]
	if !present || raw == nil {
		return 0, nil
	}
	text, ok := raw.(string)
	if !ok {
		return 0, errors.New(`"for" must be a Prometheus duration string (e.g. "5m", "1h30m")`)
	}
	if strings.TrimSpace(text) == "" {
		return 0, nil
	}
	ns, err := alerting.ParsePromDuration(text)
	if err != nil {
		return 0, errors.New(`"for": ` + err.Error())
	}
	return ns, nil
}

// foreignStringMap reads labels/annotations off a rule entry. A non-string
// value NAMES ITS KEY -- decodeJSONStringMap's rule, applied to a map that came
// off the apiserver rather than out of a request body. The CRD declares both as
// map[string]string, so a value that is not one means the object is malformed
// and the honest outcome is to skip that rule rather than coerce.
func foreignStringMap(entry map[string]any, field string) (map[string]string, error) {
	raw, present := entry[field]
	if !present || raw == nil {
		return map[string]string{}, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(field + " must be an object of string values")
	}
	out := make(map[string]string, len(obj))
	for key, value := range obj {
		text, ok := value.(string)
		if !ok {
			return nil, errors.New(field + "[" + strconv.Quote(key) + "] must be a string")
		}
		out[key] = text
	}
	return out, nil
}

// mustMarshalStringMap encodes a map[string]string for a JSONB column. It
// cannot fail -- every key and value is a string, which json always encodes --
// and the signature says so rather than making four call sites handle an error
// that has no branch.
func mustMarshalStringMap(m map[string]string) json.RawMessage {
	raw, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// handleAlertRulesPreview renders a DRAFT rule and reports what its expression
// currently matches.
//
// This is M7 Decision 2 in one handler: there is no Prometheus parser
// dependency in this build, so "is this expression valid" is answered by
// RUNNING it. An instant query that errors is an invalid expression; one that
// returns N series is the preview.
//
// The two halves fail independently and are reported differently:
//
//   - RENDER fails -> 422. There is no expression, so there is nothing partial
//     to be honest about, and the error names the param the caller just typed.
//   - QUERY fails, or Prometheus is not configured at all -> 200, with the
//     rendered expression and an `error` string. The render SUCCEEDED, which is
//     the half the builder form is actually asking about; refusing the whole
//     request because the evaluation half could not run would hide a correct
//     expression behind an unrelated outage.
//
// It persists nothing and is gated on alerts:READ, checks/projection's shape
// one permission-class down: previewing is asking what a draft matches right
// now, which is a read of Prometheus. It is still a POST (the body is a rule)
// and is therefore still audited -- with an empty {} detail, since the
// allow-list names no keys for it (audit.go).
func (s *Server) handleAlertRulesPreview(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeAlertRuleRequest(w, r)
	if !ok {
		return
	}
	expr, ok := s.renderAlertRule(w, &req)
	if !ok {
		return
	}
	if s.prom == nil {
		writeJSON(w, alertRulePreviewResponse{Expr: expr, Error: promUnconfiguredDetail})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), alertQueryTimeout)
	defer cancel()
	raw, err := s.prom.Query(ctx, expr, time.Time{})
	if err != nil {
		writeJSON(w, alertRulePreviewResponse{Expr: expr, Error: promQueryErrorText(err)})
		return
	}
	series, err := countPromSeries(raw)
	if err != nil {
		writeJSON(w, alertRulePreviewResponse{Expr: expr, Error: err.Error()})
		return
	}
	writeJSON(w, alertRulePreviewResponse{Expr: expr, Series: series})
}

// promQueryErrorText turns a promql client error into the one-line `error` a
// preview body carries.
//
// Prometheus's own message is forwarded for an UpstreamError, and only for
// that: it is a PromQL parse or evaluation error about the expression the
// caller sent a second ago, which is the single most useful thing the preview
// can say. Every other failure -- a dial error, a timeout, a size cap -- is
// reported in this package's own words: those errors carry the configured
// Prometheus URL, which is infrastructure this response has no reason to name.
func promQueryErrorText(err error) string {
	var ue *promql.UpstreamError
	if errors.As(err, &ue) {
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(ue.Body, &envelope) == nil && envelope.Error != "" {
			return truncateUpstream("prometheus rejected the expression: " + envelope.Error)
		}
		return "prometheus rejected the expression"
	}
	switch {
	case errors.Is(err, promql.ErrResponseTooLarge):
		return "the expression matched more data than the console will read back; narrow it"
	case errors.Is(err, promql.ErrBadRequest):
		return "the console could not build a valid instant query from this expression"
	default:
		slog.Warn("alert rule preview query failed", "error", err)
		return "prometheus could not be reached to evaluate the expression"
	}
}

func truncateUpstream(s string) string {
	if len(s) <= alertsUpstreamMaxLen {
		return s
	}
	return s[:alertsUpstreamMaxLen] + "..."
}

// countPromSeries counts the vector entries in an instant-query envelope. A
// non-success envelope or an unexpected shape is an error rather than a zero:
// "0 series" is a meaningful preview answer ("this would never fire") and must
// never be produced by a parse failure.
func countPromSeries(raw json.RawMessage) (int, error) {
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, errors.New("prometheus returned a response the console could not read")
	}
	if envelope.Status != "success" {
		if envelope.Error != "" {
			return 0, errors.New(truncateUpstream("prometheus rejected the expression: " + envelope.Error))
		}
		return 0, errors.New("prometheus rejected the expression")
	}
	return len(envelope.Data.Result), nil
}

// handleAlerts serves the alerts Prometheus is currently firing or pending
// (M7 Decision 6), mapped onto this console's own vocabulary.
//
// The console does NOT evaluate anything: Prometheus evaluates, the console
// manages. This is a read of /api/v1/alerts, projected and optionally filtered.
//
// Three outcomes, and the difference between the first two is the reason this
// route does not simply mirror GET /api/v1/matrix's 503:
//
//   - Prometheus UNCONFIGURED -> 200, empty list, promConfigured:false. The
//     Overview card must be able to render "nothing is firing" and "nobody is
//     watching" without one of them being an error.
//   - Prometheus configured and FAILING -> 502. That is a real upstream
//     failure, and pretending it is an empty firing list would be the most
//     dangerous lie in this API.
//   - otherwise -> 200 with the mapped set.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	managedOnly := false
	if raw := r.URL.Query().Get("managedOnly"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			// Refused rather than defaulted to false: an unparseable filter
			// silently read as "no filter" returns MORE than was asked for,
			// which is the wrong direction to guess in.
			writeProblem(w, http.StatusBadRequest, "invalid managedOnly",
				"managedOnly must be a boolean (true/false)")
			return
		}
		managedOnly = parsed
	}

	if s.prom == nil {
		writeJSON(w, alertsResponse{Alerts: []firingAlert{}, PromConfigured: false})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), alertQueryTimeout)
	defer cancel()
	raw, err := s.prom.Alerts(ctx)
	if err != nil {
		// Logged, never surfaced: the error carries the configured Prometheus
		// URL, which is infrastructure a response body must not name.
		slog.Error("read prometheus alerts failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "alerts unavailable",
			"prometheus is configured but did not answer /api/v1/alerts")
		return
	}

	alerts, err := decodePromAlerts(raw, managedOnly)
	if err != nil {
		slog.Error("decode prometheus alerts failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "alerts unavailable",
			"prometheus answered /api/v1/alerts with a body the console could not read")
		return
	}
	writeJSON(w, alertsResponse{Alerts: alerts, PromConfigured: true})
}

// decodePromAlerts maps Prometheus's /api/v1/alerts envelope onto firingAlert.
// The returned slice is never nil -- the JSON must stay [] and never null,
// because the frontend indexes into it.
func decodePromAlerts(raw json.RawMessage, managedOnly bool) ([]firingAlert, error) {
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    *time.Time        `json:"activeAt"`
				Value       string            `json:"value"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Status != "success" {
		return nil, errors.New("prometheus reported status " + strconv.Quote(envelope.Status))
	}

	out := make([]firingAlert, 0, len(envelope.Data.Alerts))
	for i := range envelope.Data.Alerts {
		a := &envelope.Data.Alerts[i]
		ruleID := a.Labels[alerting.RuleIDLabel]
		if managedOnly && ruleID == "" {
			continue
		}
		labels := a.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		annotations := a.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		out = append(out, firingAlert{
			Name:        a.Labels["alertname"],
			State:       a.State,
			Severity:    a.Labels[alerting.SeverityLabel],
			Labels:      labels,
			Annotations: annotations,
			ActiveAt:    a.ActiveAt,
			Value:       a.Value,
			RuleID:      ruleID,
		})
	}
	return out, nil
}
