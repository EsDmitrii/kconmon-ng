package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// The alert-rule bounds.
const (
	// alertRuleNameMaxLen mirrors migration 00007's CHECK (length(name) BETWEEN 1 AND 63), which is
	// itself targets.name's bound.
	alertRuleNameMaxLen = nameMaxLen

	// alertRuleExprMaxLen bounds BOTH the stored rendered_expr and a raw rule's own params.expr.
	alertRuleExprMaxLen = 4096

	// alertRuleSyncMessageMaxLen bounds the reconciler's write-back message.
	alertRuleSyncMessageMaxLen = 1024
)

// The alert-rule template kinds; the Go check runs first so a caller gets a named error rather than
// a raw constraint violation.
const (
	AlertRuleKindPairLoss           = "pair-loss"
	AlertRuleKindZoneLatency        = "zone-latency"
	AlertRuleKindDNSFailures        = "dns-failures"
	AlertRuleKindHTTPTTFB           = "http-ttfb"
	AlertRuleKindCertExpiry         = "cert-expiry"
	AlertRuleKindAgentMissing       = "agent-missing"
	AlertRuleKindExternalTargetDown = "external-target-down"
	// AlertRuleKindRaw is the escape hatch and the import target: a hand-written PromQL expression
	// carried in params.expr.
	AlertRuleKindRaw = "raw"
)

var alertRuleKinds = map[string]bool{
	AlertRuleKindPairLoss:           true,
	AlertRuleKindZoneLatency:        true,
	AlertRuleKindDNSFailures:        true,
	AlertRuleKindHTTPTTFB:           true,
	AlertRuleKindCertExpiry:         true,
	AlertRuleKindAgentMissing:       true,
	AlertRuleKindExternalTargetDown: true,
	AlertRuleKindRaw:                true,
}

// The alert severities. Also CHECK-constrained: severity is the label
// Alertmanager routes on, and a fourth value would route nowhere.
const (
	AlertSeverityInfo     = "info"
	AlertSeverityWarning  = "warning"
	AlertSeverityCritical = "critical"
)

var alertSeverities = map[string]bool{
	AlertSeverityInfo:     true,
	AlertSeverityWarning:  true,
	AlertSeverityCritical: true,
}

// 'unsynced' is the column DEFAULT and the state every freshly created or freshly edited rule is
// in.
const (
	// AlertSyncStatusUnsynced means the row's bytes have not been applied
	// since they last changed. It is not an error state.
	AlertSyncStatusUnsynced = "unsynced"
	AlertSyncStatusSynced   = "synced"
	// AlertSyncStatusDrift means the live object no longer matches what was
	// rendered from this row -- somebody edited the CRD by hand.
	AlertSyncStatusDrift = "drift"
	// AlertSyncStatusError carries a SyncMessage: the CRD is absent, RBAC
	// refused, the API errored. The rule itself still lives in Postgres, which
	// is what makes a degraded sync survivable.
	AlertSyncStatusError = "error"
)

var alertSyncStatuses = map[string]bool{
	AlertSyncStatusUnsynced: true,
	AlertSyncStatusSynced:   true,
	AlertSyncStatusDrift:    true,
	AlertSyncStatusError:    true,
}

// AlertRule is one console-managed Prometheus alert rule: the builder fields
// an operator typed, the expression last rendered from them, and the
// reconciler's view of whether the cluster agrees.
type AlertRule struct {
	ID   string
	Name string
	// Kind names the template Params is interpreted against, or "raw" for a hand-written expression.
	Kind string
	// Params is the stored JSONB OBJECT; handed back raw for Incident.Pinned's reason: the API layer
	// re-serializes.
	Params   json.RawMessage
	Severity string
	// ForNs is NANOSECONDS, the repo-wide duration convention. The store does
	// NOT convert: the renderer formats it as Prometheus' own duration
	// spelling, and this layer has no business guessing at that.
	ForNs int64
	// Labels and Annotations are attached to the rendered alert. Both are
	// JSONB objects, verbatim, for Params' reason.
	Labels      json.RawMessage
	Annotations json.RawMessage
	Enabled     bool
	// RenderedExpr is what was last rendered from the builder fields above. It
	// lives on the row so the drift view can diff rendered-vs-live without
	// re-running the renderer.
	RenderedExpr string
	// SyncStatus, SyncMessage and LastSyncedAt are RECONCILER OUTCOMES, written only by
	// UpdateAlertRuleSyncStatus.
	SyncStatus   string
	SyncMessage  string
	LastSyncedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AlertRuleInput is the write payload for CreateAlertRule and UpdateAlertRule: the BUILDER half of
// the row, and only that half.
type AlertRuleInput struct {
	Name        string
	Kind        string
	Params      json.RawMessage // nil / empty / JSON null all store {}
	Severity    string
	ForNs       int64 // nanoseconds; never converted here
	Labels      json.RawMessage
	Annotations json.RawMessage
	Enabled     bool
	// RenderedExpr travels with the builder fields even though it is derived, because it changes
	// exactly when they do.
	RenderedExpr string
}

// The update surface is TWO NARROW UPDATES rather than one full replace.
type AlertRuleStore interface {
	CreateAlertRule(ctx context.Context, in AlertRuleInput) (AlertRule, error)
	// UpdateAlertRule replaces the builder fields and resets the rule to
	// 'unsynced': a changed rule is by definition not the rule that was
	// applied.
	UpdateAlertRule(ctx context.Context, id string, in AlertRuleInput) (AlertRule, error)
	// UpdateAlertRuleSyncStatus records one reconcile outcome and touches NOTHING else -- not the
	// builder fields, not updated_at.
	UpdateAlertRuleSyncStatus(ctx context.Context, id, status, message string, lastSyncedAt *time.Time) (AlertRule, error)
	// DeleteAlertRule returns ErrNotFound when id does not name a rule,
	// including when it is not a UUID at all.
	DeleteAlertRule(ctx context.Context, id string) error
}

var _ AlertRuleStore = (*DB)(nil)

// AlertRuleReader is the read seam: httpapi's /api/v1/alert-rules routes and
// the reconciler's own enabled-rules read.
type AlertRuleReader interface {
	GetAlertRule(ctx context.Context, id string) (AlertRule, error)
	// ListAlertRules returns every rule ordered by lower(name), or only the enabled ones when
	// enabledOnly is set.
	ListAlertRules(ctx context.Context, enabledOnly bool) ([]AlertRule, error)
}

var _ AlertRuleReader = (*DB)(nil)

// Validate reports whether in is a well-formed alert rule; it runs before the INSERT/UPDATE for
// TargetInput.Validate's reason: a caller gets a precise.
func (in *AlertRuleInput) Validate() error {
	if err := validateAlertRuleName(in.Name); err != nil {
		return fmt.Errorf("store: alert rule: %w", err)
	}
	if !alertRuleKinds[in.Kind] {
		return fmt.Errorf("store: alert rule: kind %q must be one of "+
			"pair-loss, zone-latency, dns-failures, http-ttfb, cert-expiry, "+
			"agent-missing, external-target-down, raw", in.Kind)
	}
	if !alertSeverities[in.Severity] {
		return fmt.Errorf("store: alert rule: severity %q must be one of info, warning, critical", in.Severity)
	}
	if in.ForNs < 0 {
		return fmt.Errorf("store: alert rule: for %dns must not be negative", in.ForNs)
	}
	if len(in.RenderedExpr) > alertRuleExprMaxLen {
		return fmt.Errorf("store: alert rule: rendered expression is %d bytes, limit is %d",
			len(in.RenderedExpr), alertRuleExprMaxLen)
	}
	for _, f := range []struct {
		field string
		raw   json.RawMessage
	}{
		{"params", in.Params},
		{"labels", in.Labels},
		{"annotations", in.Annotations},
	} {
		if err := validateJSONObject(f.field, f.raw); err != nil {
			return fmt.Errorf("store: alert rule: %w", err)
		}
	}
	if in.Kind == AlertRuleKindRaw {
		return validateRawAlertParams(in.Params)
	}
	return nil
}

// validateAlertRuleName applies validateName -- the shared rule targets and check_definitions
// carry.
func validateAlertRuleName(name string) error {
	return validateName(name)
}

// validateAlertSyncStatus applies the closed sync vocabulary and the message
// bound. Split out of the method so both the status write and any future
// caller assert one spelling of the rule.
func validateAlertSyncStatus(status, message string) error {
	if !alertSyncStatuses[status] {
		return fmt.Errorf("store: alert rule: sync status %q must be one of unsynced, synced, drift, error", status)
	}
	if len(message) > alertRuleSyncMessageMaxLen {
		return fmt.Errorf("store: alert rule: sync message is %d bytes, limit is %d",
			len(message), alertRuleSyncMessageMaxLen)
	}
	return nil
}

// validateRawAlertParams enforces the one thing this layer knows about a raw rule; everything else
// about params is the renderer's business, but a raw rule IS its expression.
func validateRawAlertParams(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return errors.New(`store: alert rule: kind "raw" requires params.expr, a non-empty PromQL expression`)
	}
	var params struct {
		Expr *string `json:"expr"`
	}
	if err := json.Unmarshal(trimmed, &params); err != nil {
		return fmt.Errorf(`store: alert rule: kind "raw" requires params.expr to be a string: %w`, err)
	}
	if params.Expr == nil || strings.TrimSpace(*params.Expr) == "" {
		return errors.New(`store: alert rule: kind "raw" requires params.expr, a non-empty PromQL expression`)
	}
	if len(*params.Expr) > alertRuleExprMaxLen {
		return fmt.Errorf("store: alert rule: params.expr is %d bytes, limit is %d",
			len(*params.Expr), alertRuleExprMaxLen)
	}
	return nil
}

// validateJSONObject is validateJSON (targets.go) plus the shape rule the three alert-rule JSONB
// columns need.
func validateJSONObject(field string, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, jsonNull) {
		return nil
	}
	// Same bound as validateJSON's, for the same reason: labels, annotations and params are read
	// back and re-marshalled by every listing, every export and every reconcile pass.
	if len(trimmed) > jsonFieldMaxBytes {
		return fmt.Errorf("%s is %d bytes, limit is %d", field, len(trimmed), jsonFieldMaxBytes)
	}
	if !json.Valid(trimmed) {
		return fmt.Errorf("%s must be valid JSON", field)
	}
	// Same reason as validateJSON's: a NUL is valid JSON and invalid jsonb.
	if err := validateNoJSONNUL(field, trimmed); err != nil {
		return err
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("%s must be a JSON object, not an array or a scalar", field)
	}
	return nil
}

func alertRuleFromRow(r *gen.AlertRule) AlertRule {
	return AlertRule{
		ID:           formatUUID(r.ID),
		Name:         r.Name,
		Kind:         r.Kind,
		Params:       r.Params,
		Severity:     r.Severity,
		ForNs:        r.ForNs,
		Labels:       r.Labels,
		Annotations:  r.Annotations,
		Enabled:      r.Enabled,
		RenderedExpr: r.RenderedExpr,
		SyncStatus:   r.SyncStatus,
		SyncMessage:  r.SyncMessage,
		LastSyncedAt: nullTime(r.LastSyncedAt),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

func (db *DB) CreateAlertRule(ctx context.Context, in AlertRuleInput) (AlertRule, error) { //nolint:gocritic // hugeParam: AlertRuleInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return AlertRule{}, err
	}
	rid, err := parseUUID(uuid.NewString())
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: create alert rule: %w", err)
	}

	start := time.Now()
	row, err := gen.New(db.pool).CreateAlertRule(ctx, gen.CreateAlertRuleParams{
		ID:           rid,
		Name:         in.Name,
		Kind:         in.Kind,
		Params:       orEmptyJSON(in.Params),
		Severity:     in.Severity,
		ForNs:        in.ForNs,
		Labels:       orEmptyJSON(in.Labels),
		Annotations:  orEmptyJSON(in.Annotations),
		Enabled:      in.Enabled,
		RenderedExpr: in.RenderedExpr,
	})
	db.observe(queryCreateAlertRule, start, queryResult(wrapUniqueViolation(err)))
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: create alert rule: %w", wrapUniqueViolation(err))
	}
	return alertRuleFromRow(&row), nil
}

// GetAlertRule applies GetRun's UUID pre-check: a malformed id is ErrNotFound.
func (db *DB) GetAlertRule(ctx context.Context, id string) (AlertRule, error) {
	rid, err := parseUUID(id)
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: get alert rule: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).GetAlertRule(ctx, rid)
	db.observe(queryGetAlertRule, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: get alert rule: %w", wrapNoRows(err))
	}
	return alertRuleFromRow(&row), nil
}

func (db *DB) ListAlertRules(ctx context.Context, enabledOnly bool) ([]AlertRule, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListAlertRules(ctx, enabledOnly)
	db.observe(queryListAlertRules, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list alert rules: %w", err)
	}
	rules := make([]AlertRule, len(rows))
	for i := range rows {
		rules[i] = alertRuleFromRow(&rows[i])
	}
	return rules, nil
}

// UpdateAlertRule replaces the builder half of the row. See the query's own
// comment for why it also resets sync_status/sync_message and why it does not
// clear last_synced_at.
func (db *DB) UpdateAlertRule(ctx context.Context, id string, in AlertRuleInput) (AlertRule, error) { //nolint:gocritic // hugeParam: AlertRuleInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return AlertRule{}, err
	}
	rid, err := parseUUID(id)
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: update alert rule: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).UpdateAlertRule(ctx, gen.UpdateAlertRuleParams{
		ID:           rid,
		Name:         in.Name,
		Kind:         in.Kind,
		Params:       orEmptyJSON(in.Params),
		Severity:     in.Severity,
		ForNs:        in.ForNs,
		Labels:       orEmptyJSON(in.Labels),
		Annotations:  orEmptyJSON(in.Annotations),
		Enabled:      in.Enabled,
		RenderedExpr: in.RenderedExpr,
	})
	db.observe(queryUpdateAlertRule, start, queryResult(wrapUniqueViolation(wrapNoRows(err))))
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: update alert rule: %w", wrapUniqueViolation(wrapNoRows(err)))
	}
	return alertRuleFromRow(&row), nil
}

// UpdateAlertRuleSyncStatus records one reconcile outcome. A nil lastSyncedAt
// writes SQL NULL rather than year 1, UpdateWebhookDelivery's reasoning: the
// column is nullable precisely so "never applied" is expressible.
func (db *DB) UpdateAlertRuleSyncStatus(
	ctx context.Context, id, status, message string, lastSyncedAt *time.Time,
) (AlertRule, error) {
	if err := validateAlertSyncStatus(status, message); err != nil {
		return AlertRule{}, err
	}
	rid, err := parseUUID(id)
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: update alert rule sync status: %w: %w", ErrNotFound, err)
	}

	var synced pgtype.Timestamptz
	if lastSyncedAt != nil && !lastSyncedAt.IsZero() {
		synced = pgtype.Timestamptz{Time: *lastSyncedAt, Valid: true}
	}

	start := time.Now()
	row, err := gen.New(db.pool).UpdateAlertRuleSyncStatus(ctx, gen.UpdateAlertRuleSyncStatusParams{
		ID:           rid,
		SyncStatus:   status,
		SyncMessage:  message,
		LastSyncedAt: synced,
	})
	db.observe(queryUpdateAlertRuleSyncStatus, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return AlertRule{}, fmt.Errorf("store: update alert rule sync status: %w", wrapNoRows(err))
	}
	return alertRuleFromRow(&row), nil
}

// DeleteAlertRule removes one rule. Same pre-check and same miss answer as
// DeleteWebhook: deleting a rule that is not there is ErrNotFound, not
// success -- the caller asked about a specific one.
func (db *DB) DeleteAlertRule(ctx context.Context, id string) error {
	rid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete alert rule: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteAlertRule(ctx, rid)
	db.observe(queryDeleteAlertRule, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete alert rule: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete alert rule: %w", ErrNotFound)
	}
	return nil
}
