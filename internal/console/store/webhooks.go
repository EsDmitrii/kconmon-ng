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

// webhookNameRE is the webhook name charset: lowercase alphanumerics and hyphens; narrower than
// validateName's rule (targets.go) on purpose.
var webhookNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// The closed webhook event vocabulary.
const (
	WebhookEventIncidentCreated  = "incident.created"
	WebhookEventIncidentResolved = "incident.resolved"
	WebhookEventIncidentReopened = "incident.reopened"

	// The alert transitions are edges the console DETECTS by polling Prometheus' alert state, not
	// events it is told about.
	WebhookEventAlertFired    = "alert.fired"
	WebhookEventAlertResolved = "alert.resolved"
)

var webhookEvents = map[string]bool{
	WebhookEventIncidentCreated:  true,
	WebhookEventIncidentResolved: true,
	WebhookEventIncidentReopened: true,
	WebhookEventAlertFired:       true,
	WebhookEventAlertResolved:    true,
}

// webhookEventList is the vocabulary as an operator-facing sentence; it is a literal ordering
// rather than a range over webhookEvents because map iteration order is randomised.
const webhookEventList = "incident.created, incident.resolved, incident.reopened, " +
	"alert.fired, alert.resolved"

// Webhook is one configured outbound endpoint plus its last delivery outcome; the outcome lives on
// this row rather than in a delivery-log table.
type Webhook struct {
	ID   string
	Name string
	URL  string
	// Events is the non-empty subset of the closed vocabulary this endpoint
	// wants. An event outside it is filtered by the dispatcher, not delivered.
	Events []string
	// SecretEnc is OPAQUE CIPHERTEXT to this package; the store does not encrypt, does not decrypt and
	// never inspects.
	SecretEnc []byte
	Enabled   bool
	// LastStatus, LastAttempt and Failures are DELIVERY OUTCOMES, written only by
	// UpdateWebhookDelivery.
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
	// SecretEnc must be non-empty.
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
	// UpdateWebhookDelivery records one terminal delivery outcome.
	/* reset true zeroes the consecutive-failure counter (the delivery succeeded); false increments
	   whatever the ROW holds. The count is never passed in: the caller's copy is a snapshot taken
	   when the delivery was enqueued, and overlapping deliveries for one endpoint each wrote the
	   same stale base back — see the query's own comment. */
	UpdateWebhookDelivery(ctx context.Context, id, lastStatus string, lastAttempt time.Time, reset bool) error
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
	// See validateNoControlChars: a NUL here came back as 502 "webhooks unavailable".
	for _, f := range [][2]string{{"name", in.Name}, {"url", in.URL}} {
		if err := validateNoControlChars(f[0], f[1]); err != nil {
			return fmt.Errorf("store: webhook: %w", err)
		}
	}
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
			return fmt.Errorf("store: webhook: events[%d]: %q must be one of %s", i, ev, webhookEventList)
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

// validateWebhookURL applies the http(s) rule; checked as a prefix rather than with net/url because
// the point is the SCHEME.
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

func (db *DB) UpdateWebhookDelivery(ctx context.Context, id, lastStatus string, lastAttempt time.Time, reset bool) error {
	if len(lastStatus) > webhookLastStatusMaxLen {
		return fmt.Errorf("store: webhook: last status is %d bytes, limit is %d",
			len(lastStatus), webhookLastStatusMaxLen)
	}
	wid, err := parseUUID(id)
	if err != nil {
		return fmt.Errorf("store: update webhook delivery: %w: %w", ErrNotFound, err)
	}
	// A zero lastAttempt writes SQL NULL rather than year 1: the column is nullable precisely so
	// "never attempted" is expressible.
	var attempt pgtype.Timestamptz
	if !lastAttempt.IsZero() {
		attempt = pgtype.Timestamptz{Time: lastAttempt, Valid: true}
	}

	start := time.Now()
	rows, err := gen.New(db.pool).UpdateWebhookDelivery(ctx, gen.UpdateWebhookDeliveryParams{
		ID:          wid,
		LastStatus:  lastStatus,
		LastAttempt: attempt,
		Reset:       reset,
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
