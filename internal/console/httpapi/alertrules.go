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

// RuleSyncer is the reconciler seam: ask for a reconcile now; ONE interface rather than a separate
// kicker and lister.
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

// alertingDisabledDetail is served whenever s.ruleSync is nil, and it is a 409 rather than a 503.
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

// alertQueryTimeout bounds the ONE Prometheus round trip the preview and the firing list each make;
// deliberately short and independent of the promql client's own guard.
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

// alertingDisabled answers 409 and reports true when no RuleSyncer is wired; ordered AFTER
// alertRulesUnavailable at every call site.
func (s *Server) alertingDisabled(w http.ResponseWriter) bool {
	if s.ruleSync == nil {
		writeProblem(w, http.StatusConflict, "prometheus rule sync is disabled", alertingDisabledDetail)
		return true
	}
	return false
}

// renderer builds the expression renderer from the SAME metric prefix this console publishes under;
// it is constructed per call rather than held on the Server.
func (s *Server) renderer() alerting.Renderer {
	return alerting.NewRenderer(s.cfg.MetricsPrefix)
}

// alertRuleResponse is one rule on the wire: the builder fields an operator typed.
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

// alertRulesListResponse is GET /api/v1/alert-rules's body.
type alertRulesListResponse struct {
	Rules []alertRuleResponse `json:"rules"`
}

// alertRuleRequest is POST /api/v1/alert-rules's, PUT /{id}'s and /preview's body: the BUILDER
// fields, and only those.
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

// foreignRuleResponse is ONE PrometheusRule in the namespace that this console does not own; an
// import reads the groups server-side.
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

// alertRuleImportRequest is POST /api/v1/alert-rules/import's body: the NAME of a foreign
// PrometheusRule object.
type alertRuleImportRequest struct {
	Name string `json:"name"`
}

// alertRuleImportSkip is one rule entry that did NOT become a row; name is the entry's own alert
// (or record) name as the foreign object spells.
type alertRuleImportSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// alertRuleImportNote is one rule that WAS imported and about which the console had to make a
// choice; collapsing them would make an operator re-check rules that imported perfectly.
type alertRuleImportNote struct {
	Name string `json:"name"`
	Note string `json:"note"`
}

// alertRuleImportResponse is the import's report, and the report IS the result; all three slices
// are non-nil on the wire for orEmptyJSONObject's reason.
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

// alertRulePreviewResponse is POST /api/v1/alert-rules/preview's body; only the first failure is a
// status code -- see the handler.
type alertRulePreviewResponse struct {
	Expr   string `json:"expr"`
	Series int    `json:"series"`
	// Error is set exactly when the QUERY half did not run or did not answer.
	// It is never set for a render failure, which is a 422 with no body of this
	// shape at all.
	Error string `json:"error,omitempty"`
	// Rejected narrows Error to the one cause a CLIENT can act on: Prometheus
	// parsed the expression and refused it. "Could not be reached" and "not
	// configured" are the console's problems, not the expression's, and leave
	// this false -- so a UI can block a save on a PROVEN-bad expression without
	// also blocking one it merely failed to check.
	Rejected bool `json:"rejected,omitempty"`
}

// firingAlert is one entry of GET /api/v1/alerts; labels keeps them too: it is the upstream set.
type firingAlert struct {
	Name        string            `json:"name"`
	State       string            `json:"state"`
	Severity    string            `json:"severity"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	ActiveAt    *time.Time        `json:"activeAt,omitempty"`
	// Value is Prometheus's sample value as a STRING, verbatim ("7e+00"); not parsed to a float: the
	// upstream field is a string precisely because it carries NaN and ±Inf.
	Value string `json:"value"`
	// RuleID is the alert_rules row this alert came from.
	RuleID string `json:"ruleId,omitempty"`
}

// alertsResponse is GET /api/v1/alerts's body.
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

// writeAlertRuleStoreError maps an AlertRuleService error to a response; ErrAlreadyExists is 422
// rather than 409, writeTargetStoreError's reason.
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

// alertRuleInputFrom turns a decoded request into the store's write payload; a row whose
// rendered_expr came from a different version of its own params is exactly what
// store.AlertRuleInput's doc comment refuses to allow.
func (s *Server) alertRuleInputFrom(w http.ResponseWriter, req *alertRuleRequest) (store.AlertRuleInput, bool) {
	in := store.AlertRuleInput{
		Name: req.Name, Kind: req.Kind, Params: req.Params,
		Severity: req.Severity, ForNs: req.ForNs,
		Labels: req.Labels, Annotations: req.Annotations,
		Enabled: enabledOrDefault(req.Enabled),
	}
	// Store validation FIRST, so a bad severity or a bad name is reported as itself rather than as
	// whatever the renderer happens to say about a body it was never going to accept.
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

// renderAlertRule renders one request body into PromQL, answering 422 and reporting false on
// failure.
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

// kickSync nudges the reconciler after a successful write; a complete no-op when alerting is off --
// the CRUD routes deliberately do NOT gate on the reconciler being present (that is what makes
// rules editable on a console with no cluster access at all).
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

// handleAlertRulesUpdate replaces one rule's builder fields IN FULL and re-renders the expression
// from them; PUT, not PATCH, and that is a deliberate non-exception.
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

// handleAlertRulesDelete removes one rule; the row is what goes away here.
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

// handleAlertRulesSync asks the reconciler for a pass now. 202, not 200: Kick is a non-blocking
// coalescing nudge.
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

// handleAlertRulesForeign lists the PrometheusRules in the console's namespace that it does not
// own; READ-ONLY in the strongest sense: this console never mutates a foreign object under any
// circumstance.
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

// foreignRuleNotFoundDetail is the ONE 404 this route serves; ListForeign is the only lookup the
// import performs and it excludes anything carrying the managed-by label.
const foreignRuleNotFoundDetail = "no foreign PrometheusRule with that name in the console's namespace; " +
	"list them with GET /api/v1/alert-rules/foreign (an object this console already owns is not foreign)"

// recordingRuleSkipReason names the one shape of rule entry the builder has no model for at all.
const recordingRuleSkipReason = "recording rule -- the console builder has no recording model, only alerting rules"

// handleAlertRulesImport ADOPTS a foreign PrometheusRule; the foreign object is never mutated and
// never deleted.
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

	// Asking the store per entry would be N queries to answer a question one query answers, and would
	// still not be atomic.
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

// adoptForeignRule walks spec.groups[].rules[] and adopts what it can; a malformed group or entry
// is SKIPPED, never fatal -- countGroups' posture in promrules.
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

// adoptRuleEntry turns ONE spec.groups[].rules[] entry into a row, or records why it did not.
//
//nolint:gocognit,gocyclo // one linear gate chain: every branch is one adoption rule stated once
func (s *Server) adoptRuleEntry(
	ctx context.Context, renderer alerting.Renderer,
	entry map[string]any, taken map[string]bool, report *alertRuleImportResponse,
) {
	if record, ok := entry["record"].(string); ok && strings.TrimSpace(record) != "" {
		report.skip(record, recordingRuleSkipReason)
		return
	}
	// The alert name is used AS IS or not at all; SanitizeAlertName exists and is deliberately NOT
	// called here.
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
	// Both reserved names come out. severity is LIFTED into its own column.
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

// liftSeverity reads labels.severity, falling back to warning and SAYING SO; a note rather than a
// skip, and a note in BOTH fallback cases (absent label, and a value outside the closed set).
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

// foreignForNS reads a rule entry's `for`, in nanoseconds; an unreadable one is an ERROR (and
// therefore a skip at the call site) rather than a fallback to 0.
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

// foreignStringMap reads labels/annotations off a rule entry; a non-string value NAMES ITS KEY --
// decodeJSONStringMap's rule.
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

// mustMarshalStringMap encodes a map[string]string for a JSONB column; it cannot fail -- every key
// and value is a string, which json always encodes.
func mustMarshalStringMap(m map[string]string) json.RawMessage {
	raw, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// handleAlertRulesPreview renders a DRAFT rule and reports what its expression currently matches;
// the render SUCCEEDED, which is the half the builder form is actually asking about.
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
		text, rejected := promQueryErrorText(err)
		writeJSON(w, alertRulePreviewResponse{Expr: expr, Error: text, Rejected: rejected})
		return
	}
	series, err := countPromSeries(raw)
	if err != nil {
		writeJSON(w, alertRulePreviewResponse{Expr: expr, Error: err.Error()})
		return
	}
	writeJSON(w, alertRulePreviewResponse{Expr: expr, Series: series})
}

// promQueryErrorText turns a promql client error into the one-line `error` a preview body carries;
// prometheus's own message is forwarded for an UpstreamError. The second return reports whether
// PROMETHEUS ITSELF refused the expression -- the only case where the expression is proven bad
// rather than merely unchecked.
func promQueryErrorText(err error) (text string, rejected bool) {
	var ue *promql.UpstreamError
	if errors.As(err, &ue) {
		// 4xx is Prometheus judging the EXPRESSION (a parse error, bad_data);
		// 5xx is Prometheus having a bad day, which says nothing about the
		// expression and must not be reported as a rejection.
		judged := ue.Status >= http.StatusBadRequest && ue.Status < http.StatusInternalServerError
		var envelope struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(ue.Body, &envelope) == nil && envelope.Error != "" {
			if !judged {
				return truncateUpstream("prometheus could not evaluate the expression: " + envelope.Error), false
			}
			return truncateUpstream("prometheus rejected the expression: " + envelope.Error), true
		}
		if !judged {
			return "prometheus could not be reached to evaluate the expression", false
		}
		return "prometheus rejected the expression", true
	}
	switch {
	case errors.Is(err, promql.ErrResponseTooLarge):
		// The expression PARSED; it just matches too much. Nothing to block on.
		return "the expression matched more data than the console will read back; narrow it", false
	case errors.Is(err, promql.ErrBadRequest):
		return "the console could not build a valid instant query from this expression", false
	default:
		slog.Warn("alert rule preview query failed", "error", err)
		return "prometheus could not be reached to evaluate the expression", false
	}
}

func truncateUpstream(s string) string {
	if len(s) <= alertsUpstreamMaxLen {
		return s
	}
	return s[:alertsUpstreamMaxLen] + "..."
}

// countPromSeries counts the vector entries in an instant-query envelope; a non-success envelope or
// an unexpected shape is an error rather than a zero.
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

// handleAlerts serves the alerts Prometheus is currently firing or pending; the Overview card must
// be able to render "nothing is firing" and "nobody is watching" without one of them being an
// error.
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
