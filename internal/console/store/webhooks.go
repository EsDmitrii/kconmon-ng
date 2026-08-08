package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store/gen"
)

// The webhook bounds (M6 Task 1). webhookSecretMaxLen bounds the CIPHERTEXT,
// not a plaintext secret: this layer never sees a plaintext one.
const (
	webhookNameMaxLen       = 64
	webhookURLMaxLen        = 2048
	webhookLastStatusMaxLen = 255
	webhookSecretMaxLen     = 4096
)

// webhookNameRE is the webhook name charset: lowercase alphanumerics and
// hyphens. Narrower than validateName's rule (targets.go) on purpose -- a
// webhook name is the operator-facing handle for an endpoint and appears in
// the audit log's allow-listed detail, so it stays in the one spelling that
// cannot be confused with another by case.
var webhookNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// The closed webhook event vocabulary. M6 fires on incident lifecycle ONLY
// (Decision 5): those are the events M6 itself introduces, and alert-fired
// webhooks belong to M7 alerting. The set is closed here rather than by a
// CHECK so M7 widens it in code, not in a migration.
const (
	WebhookEventIncidentCreated  = "incident.created"
	WebhookEventIncidentResolved = "incident.resolved"
	WebhookEventIncidentReopened = "incident.reopened"
)

var webhookEvents = map[string]bool{
	WebhookEventIncidentCreated:  true,
	WebhookEventIncidentResolved: true,
	WebhookEventIncidentReopened: true,
}

// Webhook is one configured outbound endpoint plus its last delivery outcome
// (M6 Decision 5). The outcome lives on this row rather than in a delivery-log
// table: one row per delivery is unbounded growth for marginal value, and the
// ledger is the console log.
type Webhook struct {
	ID   string
	Name string
	URL  string
	// Events is the non-empty subset of the closed vocabulary this endpoint
	// wants. An event outside it is filtered by the dispatcher, not delivered.
	Events []string
	// SecretEnc is OPAQUE CIPHERTEXT to this package. The store does not
	// encrypt, does not decrypt and never inspects it: the dispatcher owns the
	// crypto (config-keyed AES-GCM, M6 Decision 4) and hands the store bytes.
	// That is what keeps the secret out of audit rows, API responses, logs and
	// metric labels -- this layer has nothing to leak but a byte string it
	// cannot read.
	SecretEnc []byte
	Enabled   bool
	// LastStatus, LastAttempt and Failures are DELIVERY OUTCOMES, written only
	// by UpdateWebhookDelivery. UpdateWebhook deliberately leaves them alone:
	// an operator fixing a URL typo must not also reset the endpoint's failure
	// history.
	LastStatus  string
	LastAttempt *time.Time
	Failures    int32
	CreatedAt   time.Time
}

// WebhookInput is the write payload for CreateWebhook and UpdateWebhook: the
// CONFIGURED half of the row, and only that half.
type WebhookInput struct {
	Name string
	URL  string
	// Events must be non-empty: an endpoint subscribed to nothing is an
	// endpoint that is never called, which is what Enabled=false already says
	// more honestly.
	Events []string
	// SecretEnc must be non-empty. Every delivery is signed (Decision 5), so a
	// secret-less endpoint could not be delivered to at all. On an update that
	// is not rotating the secret, the caller passes the bytes it read back.
	SecretEnc []byte
	Enabled   bool
}

// WebhookStore is the write seam. httpapi owns the CRUD (admin-only, under
// webhooks:manage); the dispatcher owns UpdateWebhookDelivery and nothing
// else.
type WebhookStore interface {
	CreateWebhook(ctx context.Context, in WebhookInput) (Webhook, error)
	UpdateWebhook(ctx context.Context, id string, in WebhookInput) (Webhook, error)
	// DeleteWebhook returns ErrNotFound when id does not name an endpoint,
	// including when it is not a UUID at all.
	DeleteWebhook(ctx context.Context, id string) error
	// UpdateWebhookDelivery records one terminal delivery outcome. failures is
	// SET, not incremented: the dispatcher -- not this layer -- knows whether
	// the attempt ended a streak or extended one, and a set is idempotent
	// under the retry that an increment is not.
	UpdateWebhookDelivery(ctx context.Context, id, lastStatus string, lastAttempt time.Time, failures int32) error
}

var _ WebhookStore = (*DB)(nil)

// WebhookReader is the read seam: httpapi's list/get (secret masked there,
// never here -- masking is a presentation decision and this layer would only
// be guessing at it) and the dispatcher's own endpoint lookup.
type WebhookReader interface {
	GetWebhook(ctx context.Context, id string) (Webhook, error)
	// ListWebhooks returns every configured endpoint, newest first. Unpaged
	// for ListMTRDestinations' reason: the row count is endpoints an operator
	// typed, not a function of time.
	ListWebhooks(ctx context.Context) ([]Webhook, error)
}

var _ WebhookReader = (*DB)(nil)

// Validate reports whether in is a well-formed endpoint.
func (in *WebhookInput) Validate() error {
	if in.Name == "" {
		return errors.New("store: webhook: name must not be empty")
	}
	if len(in.Name) > webhookNameMaxLen {
		return fmt.Errorf("store: webhook: name is %d bytes, limit is %d", len(in.Name), webhookNameMaxLen)
	}
	if !webhookNameRE.MatchString(in.Name) {
		return fmt.Errorf("store: webhook: name %q must be lowercase alphanumerics and '-'", in.Name)
	}
	if err := validateWebhookURL(in.URL); err != nil {
		return err
	}
	if len(in.Events) == 0 {
		return errors.New("store: webhook: events must not be empty")
	}
	seen := make(map[string]bool, len(in.Events))
	for i, ev := range in.Events {
		if !webhookEvents[ev] {
			return fmt.Errorf("store: webhook: events[%d]: %q must be one of "+
				"incident.created, incident.resolved, incident.reopened", i, ev)
		}
		if seen[ev] {
			return fmt.Errorf("store: webhook: events[%d]: %q appears twice", i, ev)
		}
		seen[ev] = true
	}
	if len(in.SecretEnc) == 0 {
		return errors.New("store: webhook: secret must not be empty: every delivery is signed")
	}
	if len(in.SecretEnc) > webhookSecretMaxLen {
		return fmt.Errorf("store: webhook: secret is %d bytes, limit is %d",
			len(in.SecretEnc), webhookSecretMaxLen)
	}
	return nil
}

// validateWebhookURL applies the http(s) rule. Checked as a prefix rather than
// with net/url because the point is the SCHEME -- a file:// or a gopher://
// endpoint is the bug worth catching, and url.Parse accepts far stranger
// things than it rejects.
func validateWebhookURL(raw string) error {
	if raw == "" {
		return errors.New("store: webhook: url must not be empty")
	}
	if len(raw) > webhookURLMaxLen {
		return fmt.Errorf("store: webhook: url is %d bytes, limit is %d", len(raw), webhookURLMaxLen)
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("store: webhook: url %q must start with http:// or https://", raw)
	}
	return nil
}

func webhookFromRow(w *gen.Webhook) Webhook {
	return Webhook{
		ID:          formatUUID(w.ID),
		Name:        w.Name,
		URL:         w.Url,
		Events:      w.Events,
		SecretEnc:   w.SecretEnc,
		Enabled:     w.Enabled,
		LastStatus:  w.LastStatus,
		LastAttempt: nullTime(w.LastAttempt),
		Failures:    w.Failures,
		CreatedAt:   w.CreatedAt,
	}
}

func (db *DB) CreateWebhook(ctx context.Context, in WebhookInput) (Webhook, error) { //nolint:gocritic // hugeParam: WebhookInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Webhook{}, err
	}
	wid, err := parseUUID(uuid.NewString())
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}

	start := time.Now()
	row, err := gen.New(db.pool).CreateWebhook(ctx, gen.CreateWebhookParams{
		ID:        wid,
		Name:      in.Name,
		Url:       in.URL,
		Events:    in.Events,
		SecretEnc: in.SecretEnc,
		Enabled:   in.Enabled,
	})
	db.observe(queryCreateWebhook, start, queryResult(wrapUniqueViolation(err)))
	if err != nil {
		return Webhook{}, fmt.Errorf("store: create webhook: %w", wrapUniqueViolation(err))
	}
	return webhookFromRow(&row), nil
}

// GetWebhook applies GetRun's UUID pre-check: a malformed id is ErrNotFound.
func (db *DB) GetWebhook(ctx context.Context, id string) (Webhook, error) {
	wid, err := parseUUID(id)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: get webhook: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).GetWebhook(ctx, wid)
	db.observe(queryGetWebhook, start, queryResult(wrapNoRows(err)))
	if err != nil {
		return Webhook{}, fmt.Errorf("store: get webhook: %w", wrapNoRows(err))
	}
	return webhookFromRow(&row), nil
}

func (db *DB) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	start := time.Now()
	rows, err := gen.New(db.pool).ListWebhooks(ctx)
	db.observe(queryListWebhooks, start, queryResult(err))
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	hooks := make([]Webhook, len(rows))
	for i := range rows {
		hooks[i] = webhookFromRow(&rows[i])
	}
	return hooks, nil
}

func (db *DB) UpdateWebhook(ctx context.Context, id string, in WebhookInput) (Webhook, error) { //nolint:gocritic // hugeParam: WebhookInput mirrors the other write-payload structs in this package
	if err := in.Validate(); err != nil {
		return Webhook{}, err
	}
	wid, err := parseUUID(id)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: update webhook: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	row, err := gen.New(db.pool).UpdateWebhook(ctx, gen.UpdateWebhookParams{
		ID:        wid,
		Name:      in.Name,
		Url:       in.URL,
		Events:    in.Events,
		SecretEnc: in.SecretEnc,
		Enabled:   in.Enabled,
	})
	db.observe(queryUpdateWebhook, start, queryResult(wrapUniqueViolation(wrapNoRows(err))))
	if err != nil {
		return Webhook{}, fmt.Errorf("store: update webhook: %w", wrapUniqueViolation(wrapNoRows(err)))
	}
	return webhookFromRow(&row), nil
}

func (db *DB) UpdateWebhookDelivery(ctx context.Context, id, lastStatus string, lastAttempt time.Time, failures int32) error {
	if len(lastStatus) > webhookLastStatusMaxLen {
		return fmt.Errorf("store: webhook: last status is %d bytes, limit is %d",
			len(lastStatus), webhookLastStatusMaxLen)
	}
	if failures < 0 {
		return fmt.Errorf("store: webhook: failures %d must not be negative", failures)
	}
	wid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: update webhook delivery: %w: %w", ErrNotFound, err)
	}
	// A zero lastAttempt writes SQL NULL rather than year 1: the column is
	// nullable precisely so "never attempted" is expressible, and storing the
	// zero time would put every untried endpoint's last attempt in year 1
	// forever.
	var attempt pgtype.Timestamptz
	if !lastAttempt.IsZero() {
		attempt = pgtype.Timestamptz{Time: lastAttempt, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).UpdateWebhookDelivery(ctx, gen.UpdateWebhookDeliveryParams{
		ID:          wid,
		LastStatus:  lastStatus,
		LastAttempt: attempt,
		Failures:    failures,
	})
	db.observe(queryUpdateWebhookDelivery, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: update webhook delivery: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: update webhook delivery: %w", ErrNotFound)
	}
	return nil
}

func (db *DB) DeleteWebhook(ctx context.Context, id string) error {
	wid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w: %w", ErrNotFound, err)
	}
	start := time.Now()
	rows, err := gen.New(db.pool).DeleteWebhook(ctx, wid)
	db.observe(queryDeleteWebhook, start, queryResult(err))
	if err != nil {
		return fmt.Errorf("store: delete webhook: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("store: delete webhook: %w", ErrNotFound)
	}
	return nil
}
