// Package webhooks is the Console's outbound notifier; a handler that could unseal could serve a
// secret back.
package webhooks

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// WebhookDeliveries{result} label values. Closed set, pinned in the metrics
// package's own test.
const (
	resultOK       = "ok"
	resultFailed   = "failed"
	resultFiltered = "filtered"
)

// SignatureHeader carries the HMAC over the RAW request body: "sha256=<hex>"; exported so a
// receiver written against this console -- and the tests.
const SignatureHeader = "X-Kconmon-Signature"

// TestEvent is the event name a /test ping carries; it is deliberately NOT in store's webhook
// vocabulary.
const TestEvent = "test"

// testIncidentTitle is what the synthetic incident in a /test ping is called.
// A test ping carries the SAME envelope as a real one so a receiver needs one
// parser, not two -- the only honest way to make "it works" mean anything.
const testIncidentTitle = "kconmon-ng webhook test"

// testIncidentAuthor fills createdBy in a test ping. A person's name would be
// a small lie: nobody opened this incident, because there is no incident.
const testIncidentAuthor = "kconmon-ng"

const (
	// attemptTimeout bounds ONE HTTP attempt. A receiver that accepts the
	// connection and then holds it must not pin a pool slot indefinitely.
	attemptTimeout = 10 * time.Second

	// storeWriteTimeout bounds the outcome write. Short on purpose: the
	// delivery already happened, and a slow database must not extend a
	// worker's hold on its pool slot much past the attempt it describes.
	storeWriteTimeout = 5 * time.Second

	// drainBudget is how long Close waits for in-flight deliveries.
	drainBudget = 5 * time.Second

	// maxConcurrent bounds the deliveries making an HTTP ATTEMPT at the same moment. A token is held
	// across one POST, never across the sleeps between rungs -- see deliver.
	maxConcurrent = 8

	/* maxPending bounds how many deliveries may be IN FLIGHT, ladder and all.
	   It exists because the two limits are different questions. maxConcurrent used to answer both:
	   a token was held for the whole ladder, so eight endpoints pointed at a black hole held all
	   eight tokens for the ladder's ~5.5 minutes while failing instantly, and the ninth endpoint --
	   healthy, subscribed, and the one that mattered -- was refused at enqueue and never retried,
	   while the API answered 201. Sleeping deliveries cost a goroutine and a slot here; they no
	   longer cost the pool. */
	maxPending = 1024

	// responseDrainLimit is how much of a response body is read before it is discarded; the body is
	// NEVER inspected, logged or stored.
	responseDrainLimit = 4 << 10

	// jitterFraction is the +/-20% spread applied to every non-zero rung of the ladder.
	jitterFraction = 0.2

	// nonceLen is AES-GCM's standard nonce size. Pinned rather than read from
	// the AEAD so the sealed layout (nonce || ciphertext) is stated once, in
	// the place a reader looks for it.
	nonceLen = 12
)

// retryLadder is the delay BEFORE each attempt: send immediately, retry at ~30s, retry again at
// ~5m.
var retryLadder = []time.Duration{0, 30 * time.Second, 5 * time.Minute}

// singleAttempt is the /test ping's ladder; one shot, on purpose: an operator clicking "test" is
// asking a question and waiting for the answer on the endpoint row.
var singleAttempt = []time.Duration{0}

// Store is everything the dispatcher needs from the persistence layer; it composes store's two
// narrow interfaces rather than taking *store.DB so a test can substitute a fake with no database.
type Store interface {
	store.WebhookReader
	store.WebhookStore
}

// Dispatcher delivers incident lifecycle notifications to configured
// endpoints. One per process, built by cmd/console when a database AND an
// encryption key are both present.
type Dispatcher struct {
	store Store
	m     *metrics.Metrics
	gcm   cipher.AEAD

	client *http.Client

	// sem bounds concurrent HTTP attempts (maxConcurrent); held around ONE attempt.
	sem chan struct{}
	// pending bounds admitted deliveries (maxPending); held for the whole ladder, sleeps included.
	pending chan struct{}
	// inflight covers every delivery goroutine, and is what Close's drain
	// budget waits on.
	inflight sync.WaitGroup

	// Two contexts, cancelled at two different moments.
	baseCtx     context.Context
	cancelBase  context.CancelFunc
	retryCtx    context.Context
	cancelRetry context.CancelFunc
	closeOnce   sync.Once

	// now and sleep are indirected for the tests that assert the retry ladder,
	// neither of which can be asserted against a real clock without waiting
	// five and a half minutes. Nothing outside this package can change them.
	now   func() time.Time
	sleep sleeper
}

// sleeper waits d, reporting true when the wait COMPLETED and false when it was cut short by
// shutdown or cancellation; the boolean is the whole point: a delivery that was interrupted
// mid-ladder must record a failure rather than silently retry into a process that is going away.
type sleeper func(ctx context.Context, d time.Duration) bool

// New builds a Dispatcher. key must be exactly 32 bytes -- the decoded form of
// console.webhooks.encryptionKey.
func New(key []byte, st Store, m *metrics.Metrics) (*Dispatcher, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("webhooks: encryption key is %d bytes, must be exactly %d", len(key), keyLen)
	}
	if st == nil {
		return nil, errors.New("webhooks: a store is required")
	}
	if m == nil {
		return nil, errors.New("webhooks: metrics are required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("webhooks: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webhooks: build gcm: %w", err)
	}

	base, cancelBase := context.WithCancel(context.Background())
	retry, cancelRetry := context.WithCancel(base)
	return &Dispatcher{
		store: st,
		m:     m,
		gcm:   gcm,
		// The client carries no Timeout of its own: every request is issued
		// with a per-attempt context deadline, and two independent clocks for
		// one budget is how a timeout ends up being neither of them.
		client:      &http.Client{},
		sem:         make(chan struct{}, maxConcurrent),
		pending:     make(chan struct{}, maxPending),
		baseCtx:     base,
		cancelBase:  cancelBase,
		retryCtx:    retry,
		cancelRetry: cancelRetry,
		now:         time.Now,
		sleep:       realSleep,
	}, nil
}

// keyLen is AES-256's key length, repeated from config so this package can be
// constructed and tested without importing config at all.
const keyLen = 32

// realSleep is the production sleeper.
func realSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Seal encrypts a plaintext endpoint secret into the ciphertext store.Webhook carries; it
// implements httpapi's SecretSealer, which is the ONLY direction that package is allowed.
func (d *Dispatcher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("webhooks: seal: read nonce: %w", err)
	}
	return d.gcm.Seal(nonce, nonce, plain, nil), nil
}

// A tampered or truncated blob, or one sealed under a different key, fails here with GCM's
// authentication error.
func (d *Dispatcher) Open(sealed []byte) ([]byte, error) {
	if len(sealed) < nonceLen {
		return nil, fmt.Errorf("webhooks: open: sealed value is %d bytes, shorter than the %d-byte nonce",
			len(sealed), nonceLen)
	}
	plain, err := d.gcm.Open(nil, sealed[:nonceLen], sealed[nonceLen:], nil)
	if err != nil {
		return nil, fmt.Errorf("webhooks: open: %w", err)
	}
	return plain, nil
}

// Payload is the wire body of every INCIDENT-family delivery; within a family the field set never
// changes.
type Payload struct {
	// Event is one of the store vocabulary values, or "test".
	Event string `json:"event"`
	// Incident is synthetic for a test ping -- same shape, so the receiver's
	// parser does not branch.
	Incident PayloadIncident `json:"incident"`
	// At is when the console decided to notify, NOT when this attempt was made.
	At time.Time `json:"at"`
}

// PayloadIncident is the incident subset a notification carries; notes and pinned findings are
// deliberately ABSENT.
type PayloadIncident struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Scope     string     `json:"scope"`
	Status    string     `json:"status"`
	FromAt    time.Time  `json:"fromAt"`
	ToAt      *time.Time `json:"toAt"`
	CreatedBy string     `json:"createdBy"`
}

func newPayload(event string, inc *store.Incident, at time.Time) Payload {
	return Payload{
		Event: event,
		Incident: PayloadIncident{
			ID: inc.ID, Title: inc.Title, Scope: inc.Scope, Status: inc.Status,
			FromAt: inc.FromAt, ToAt: inc.ToAt, CreatedBy: inc.CreatedBy,
		},
		At: at,
	}
}

// AlertPayload is the wire body of every ALERT-family delivery; the envelope key is `sentAt` rather
// than the incident family's `at` on purpose.
type AlertPayload struct {
	// Event is store.WebhookEventAlertFired or store.WebhookEventAlertResolved.
	Event string `json:"event"`
	// SentAt is when this delivery was built. Stable across the retry ladder
	// (the body is marshalled once), NOT stable across replicas.
	SentAt time.Time `json:"sentAt"`
	Alert  Alert     `json:"alert"`
}

// Alert is BOTH the notification seam's input and the payload's `alert` object -- one type; there
// is no alert row to project from: the console does not evaluate alerts.
type Alert struct {
	// RuleID is the alert_rules row this alert came from, off the kconmon_ng_rule_id label; never
	// empty on a delivery: the watcher only fires for MANAGED alerts (an unmanaged firing alert
	// belongs to whoever owns that rule, not to this console's endpoints).
	RuleID string `json:"ruleId"`
	// RuleName is the alert's name as PROMETHEUS knows it -- the sanitized alertname, not necessarily
	// the console row's name.
	RuleName string `json:"ruleName"`
	Severity string `json:"severity"`
	// Expr is the rendered PromQL the rule evaluates; it is "" when the row could not be resolved
	// (deleted between the poll and the lookup, or no rule source wired).
	Expr string `json:"expr"`
	// Labels is Prometheus' label set for this alert instance, verbatim,
	// including alertname/severity/kconmon_ng_rule_id. Never null on the wire.
	Labels map[string]string `json:"labels"`
	// Annotations is the alert's annotation set, verbatim. Never null.
	Annotations map[string]string `json:"annotations"`
	// FiredAt is Prometheus' activeAt for this alert instance -- when the
	// expression started matching, not when the console noticed. Stable across
	// replicas, which is what makes it part of the dedupe key.
	FiredAt time.Time `json:"firedAt"`
	// ResolvedAt is null on alert.fired and set on alert.resolved; that is the granularity, stated
	// rather than papered over.
	ResolvedAt *time.Time `json:"resolvedAt"`
}

func newAlertPayload(event string, a *Alert, sentAt time.Time) AlertPayload {
	out := AlertPayload{Event: event, SentAt: sentAt, Alert: *a}
	// A nil Go map marshals to `null`, which is a second shape for a receiver
	// that iterates the object. Normalised HERE rather than at every call site
	// so there is one place the guarantee lives.
	if out.Alert.Labels == nil {
		out.Alert.Labels = map[string]string{}
	}
	if out.Alert.Annotations == nil {
		out.Alert.Annotations = map[string]string{}
	}
	return out
}

// job is one delivery: everything a worker needs, captured at ENQUEUE time so
// nothing it does depends on a store read that could fail later.
type job struct {
	id  string
	url string
	// secretEnc is carried as CIPHERTEXT. It is opened inside each attempt and
	// the plaintext never outlives the signing call -- the one rule that makes
	// "the secret is never logged" enforceable rather than aspirational.
	secretEnc []byte
	// body is the marshalled Payload, byte-identical across retries because
	// the signature is over exactly these bytes.
	body     []byte
	event    string
	attempts []time.Duration
	// failures is the endpoint's consecutive-failure count as READ at enqueue time.
	failures int32
}

// Notify is the incident lifecycle seam httpapi calls after a successful create or status change
// (its IncidentNotifier interface).
func (d *Dispatcher) Notify(ctx context.Context, event string, inc store.Incident) { //nolint:gocritic // hugeParam: mirrors store.Incident's shape across the httpapi seam
	body, err := json.Marshal(newPayload(event, &inc, d.now().UTC()))
	if err != nil {
		// Unreachable in practice: every field is a string, a time or a
		// pointer to one. Reported rather than ignored, because reaching it
		// would mean the payload type grew something unmarshalable.
		slog.Error("webhooks: building the notification payload failed", "event", event, "error", err)
		return
	}
	d.fanOut(ctx, event, body)
}

// NotifyAlert is the ALERT transition seam, called by AlertWatcher on an edge it detected; the ONLY
// thing that differs between the two is the payload constructor.
func (d *Dispatcher) NotifyAlert(ctx context.Context, event string, a Alert) { //nolint:gocritic // hugeParam: Alert is the payload object itself, passed by value so the caller keeps no aliasing claim on the maps
	body, err := json.Marshal(newAlertPayload(event, &a, d.now().UTC()))
	if err != nil {
		slog.Error("webhooks: building the alert notification payload failed", "event", event, "error", err)
		return
	}
	d.fanOut(ctx, event, body)
}

// fanOut lists the endpoints, applies the enabled flag and the event filter; it takes bytes rather
// than a payload so the two families share it without either of them becoming an interface.
func (d *Dispatcher) fanOut(ctx context.Context, event string, body []byte) {
	hooks, err := d.store.ListWebhooks(ctx)
	if err != nil {
		slog.Error("webhooks: listing endpoints for a notification failed",
			"event", event, "error", err)
		return
	}

	for i := range hooks {
		h := &hooks[i]
		if !h.Enabled {
			// Not counted, deliberately: the endpoint was switched off on
			// purpose, and a filtered series climbing forever would report an
			// operator's own decision back to them as activity.
			continue
		}
		if !subscribes(h, event) {
			d.m.WebhookDeliveries.WithLabelValues(resultFiltered).Inc()
			continue
		}
		accepted := d.enqueue(job{
			id: h.ID, url: h.URL, secretEnc: h.SecretEnc,
			body: body, event: event, attempts: retryLadder, failures: h.Failures,
		})
		if !accepted {
			d.m.WebhookDeliveries.WithLabelValues(resultFailed).Inc()
			// The URL is absent by design (it is Debug-only, everywhere in
			// this package): a warning an operator greps is not the place to
			// put an address that names internal infrastructure.
			slog.Warn("webhooks: delivery pool saturated, dropping a notification",
				"webhook", h.ID, "event", event, "maxConcurrent", maxConcurrent) //nolint:gosec // G706: structured slog fields, not string-built log injection
		}
	}
}

// DispatchTest enqueues ONE signed ping to an already-stored endpoint, addressed by id alone; the
// error is about ACCEPTING the work, never about what the endpoint answered.
func (d *Dispatcher) DispatchTest(ctx context.Context, id string) error {
	hook, err := d.store.GetWebhook(ctx, id)
	if err != nil {
		return fmt.Errorf("webhooks: dispatch test: %w", err)
	}
	at := d.now().UTC()
	inc := store.Incident{
		Title:     testIncidentTitle,
		Status:    store.IncidentStatusOpen,
		FromAt:    at,
		CreatedBy: testIncidentAuthor,
	}
	body, err := json.Marshal(newPayload(TestEvent, &inc, at))
	if err != nil {
		return fmt.Errorf("webhooks: dispatch test: build payload: %w", err)
	}
	if !d.enqueue(job{
		id: hook.ID, url: hook.URL, secretEnc: hook.SecretEnc,
		body: body, event: TestEvent, attempts: singleAttempt, failures: hook.Failures,
	}) {
		// No metric here, unlike Notify's drop: the caller learns
		// SYNCHRONOUSLY (502, no 202), so counting a failed delivery would
		// report the same non-event twice.
		return errors.New("webhooks: dispatch test: the delivery pool is saturated, try again shortly")
	}
	return nil
}

// enqueue admits one delivery, reporting false when the pool is full. It never
// blocks -- that is the whole contract, and it is why the semaphore is polled
// with a default branch instead of a plain send.
func (d *Dispatcher) enqueue(j job) bool { //nolint:gocritic // hugeParam: job is passed by value so a worker owns its own copy
	select {
	case d.pending <- struct{}{}:
	default:
		return false
	}
	d.inflight.Add(1)
	go func() {
		defer func() {
			<-d.pending
			d.inflight.Done()
		}()
		d.deliver(j)
	}()
	return true
}

/*
 * withAttemptSlot runs fn holding one maxConcurrent token, WAITING for it rather than dropping.
 *
 * The wait is bounded by maxPending upstream: admission is refused before this queue can grow
 * without limit. Returns false when the dispatcher is shutting down, which is the same terminal
 * condition the sleep between rungs reports.
 */
func (d *Dispatcher) withAttemptSlot(fn func()) bool {
	select {
	case d.sem <- struct{}{}:
	case <-d.retryCtx.Done():
		return false
	}
	defer func() { <-d.sem }()
	fn()
	return true
}

// deliver runs one job's ladder to a terminal outcome and records it. Exactly
// ONE outcome is recorded per job, whatever happened in between -- the metric
// counts deliveries, not attempts.
func (d *Dispatcher) deliver(j job) { //nolint:gocritic // hugeParam: the worker owns this copy for the life of the delivery
	slog.Debug("webhooks: delivering", "webhook", j.id, "event", j.event, "url", j.url, //nolint:gosec // G706: structured slog fields; the URL is Debug-only by design
		"attempts", len(j.attempts))

	var lastStatus string
	for i, wait := range j.attempts {
		if wait > 0 && !d.sleep(d.retryCtx, jitter(wait)) {
			// Shutdown, mid-ladder. The remaining attempts are LOST (the
			// package comment's at-most-once-ish contract), and the honest
			// record is the last class this delivery actually produced.
			slog.Warn("webhooks: abandoning a retry, the dispatcher is shutting down",
				"webhook", j.id, "event", j.event, "attempt", i+1) //nolint:gosec // G706: structured slog fields, not string-built log injection
			break
		}
		/* The pool token is taken HERE, around the POST, and released before the next sleep. Holding
		   it across the ladder is what let a handful of dead endpoints starve every live one. */
		var (
			status   string
			ok       bool
			terminal bool
		)
		if !d.withAttemptSlot(func() { status, ok, terminal = d.attempt(&j) }) {
			slog.Warn("webhooks: abandoning a delivery, the dispatcher is shutting down",
				"webhook", j.id, "event", j.event, "attempt", i+1) //nolint:gosec // G706: structured slog fields, not string-built log injection
			break
		}
		if ok {
			// failures RESET on success: the column means CONSECUTIVE
			// failures, so one 2xx ends the streak whatever preceded it.
			// A delivery that went through resets the consecutive-failure count.
			d.record(j.id, resultOK, statusOK, true)
			return
		}
		lastStatus = status
		if terminal {
			// Nothing about this failure can change by trying again — an unreadable secret is the
			// case — so the remaining rungs are skipped rather than spent as no-ops.
			break
		}
	}
	/* The counter is incremented BY THE ROW, not from j.failures: that snapshot was taken when this
	   delivery was enqueued, and a delivery holds its job through the whole retry ladder while other
	   deliveries for the same endpoint come and go. Passing a computed value made three consecutive
	   failures read as one. */
	d.record(j.id, resultFailed, lastStatus, false)
}

// statusOK is what a delivered endpoint's last_status reads.
const statusOK = "ok"

// attempt performs ONE POST and classifies it; the returned string is the last_status class on
// failure -- a CLASS. The third value says the failure is TERMINAL: retrying cannot change it.
func (d *Dispatcher) attempt(j *job) (status string, ok, terminal bool) {
	secret, err := d.Open(j.secretEnc)
	if err != nil {
		// The row was sealed under a different key (or corrupted). Terminal: the ladder used to be
		// run out anyway, three no-op cycles holding a pool slot each.
		slog.Error("webhooks: endpoint secret could not be decrypted",
			"webhook", j.id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return "failed: secret unreadable (rotated encryption key?)", false, true
	}
	sig := sign(secret, j.body)
	// secret goes out of scope here; it is never logged, never stored, and
	// never leaves this function.

	ctx, cancel := context.WithTimeout(d.baseCtx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(j.body))
	if err != nil {
		// A URL that cannot be turned into a request will not become one on the next rung either.
		return "failed: invalid endpoint url", false, true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kconmon-ng-console")
	req.Header.Set(SignatureHeader, sig)

	resp, err := d.client.Do(req)
	if err != nil {
		// The error is CLASSIFIED, not echoed: a transport error carries the
		// URL, and last_status is served to the UI.
		if ctx.Err() != nil {
			return "failed: timeout after " + attemptTimeout.String(), false, false
		}
		slog.Warn("webhooks: delivery attempt failed", "webhook", j.id, "event", j.event, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return "failed: connection error", false, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseDrainLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return statusOK, true, false
	}
	return fmt.Sprintf("failed: HTTP %d", resp.StatusCode), false, false
}

// record writes the terminal outcome: one metric increment and one endpoint-row
// update. A failed write is warned and dropped -- the delivery already
// happened, and there is nothing useful to retry.
func (d *Dispatcher) record(id, result, lastStatus string, reset bool) {
	d.m.WebhookDeliveries.WithLabelValues(result).Inc()

	ctx, cancel := context.WithTimeout(d.baseCtx, storeWriteTimeout)
	defer cancel()
	if err := d.store.UpdateWebhookDelivery(ctx, id, lastStatus, d.now().UTC(), reset); err != nil {
		slog.Warn("webhooks: recording a delivery outcome failed",
			"webhook", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
	}
}

// Run blocks until ctx is cancelled and then drains, so cmd/console can spawn
// it through the same helper every other background component uses and have
// wg.Wait cover the drain.
func (d *Dispatcher) Run(ctx context.Context) {
	<-ctx.Done()
	d.Close()
}

// Close stops the dispatcher: retries are abandoned immediately; idempotent, so Run and an explicit
// Close in a test cannot double-close.
func (d *Dispatcher) Close() {
	d.closeOnce.Do(func() {
		d.cancelRetry()

		done := make(chan struct{})
		go func() {
			d.inflight.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(drainBudget):
			slog.Warn("webhooks: shutdown budget expired with deliveries still in flight",
				"budget", drainBudget)
		}
		d.cancelBase()
	})
}

// subscribes reports whether h asked for this event.
func subscribes(h *store.Webhook, event string) bool {
	for _, e := range h.Events {
		if e == event {
			return true
		}
	}
	return false
}

// sign builds the X-Kconmon-Signature value over the RAW body bytes -- the exact bytes on the wire.
func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// jitter spreads d by +/-jitterFraction. math/rand is correct here and
// crypto/rand would not be: this is a scheduling decision, not a secret, and
// nothing about the delivery's security depends on it being unpredictable.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	spread := 1 + jitterFraction*(2*mathrand.Float64()-1) //nolint:gosec // G404: retry spread, not a security decision
	return time.Duration(float64(d) * spread)
}
