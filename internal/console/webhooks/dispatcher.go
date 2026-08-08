// Package webhooks is the Console's outbound notifier: it turns an incident
// lifecycle change (M6 Decision 5) or a Prometheus alert transition (M7
// Decision 7) into a signed HTTP POST to every endpoint an admin configured
// for that event.
//
// The two sources reach it from opposite directions, and the difference is
// worth stating up front. An incident event is something the console was TOLD
// about -- an HTTP handler calls Notify on the request that caused it. An
// alert transition is something the console DETECTED, by polling Prometheus'
// alert state on its own schedule: that is AlertWatcher, in this package, and
// it calls NotifyAlert. Everything downstream of those two calls is one path.
//
// It is also the ONE place in the Console that owns the webhook cipher. The
// per-endpoint HMAC signing secret is stored encrypted (M6 Decision 4,
// config-keyed AES-GCM), and both directions live here on purpose: httpapi
// SEALS a secret on its way in through the narrow SecretSealer seam, this
// package UNSEALS it at send time, and nothing else in the process can do
// either. A handler that could unseal could serve a secret back, which is
// exactly what the write-only field exists to prevent.
//
// Three postures are worth stating before the code, because each is a
// deliberate trade rather than an oversight:
//
//   - NON-BLOCKING. Notify enqueues and returns; an HTTP handler never waits
//     on a delivery, and never fails because one failed. An incident is
//     recorded in the database whether or not Slack heard about it, and the
//     opposite ordering -- a 502 on POST /api/v1/incidents because someone's
//     endpoint is down -- would make the console less useful during exactly
//     the outage it exists for.
//
//   - AT-MOST-ONCE-ISH. Deliveries live in this process. A rolling update
//     during the 5-minute retry window LOSES the remaining attempts, and
//     there is no durable queue to replay them from -- a delivery-log table
//     was deferred (Decision 5). The ledger for a miss is the endpoint row's
//     last_status/failures, which is what GET /api/v1/webhooks returns, plus
//     the webhook_deliveries_total{result="failed"} counter. That is the M6
//     contract, stated so an operator plans around it instead of discovering
//     it.
//
//   - ADMIN-TRUST SSRF POSTURE. Webhook URLs are not fetched on behalf of a
//     lesser role: declaring one requires webhooks:manage, which is admin
//     ONLY (M6 Decision 8), and the dispatcher fires on the console's own
//     schedule, never on a URL a caller supplied in the request it is
//     serving. So M6 ships NO destination allowlist -- deliberately. An admin
//     who can point a webhook at 169.254.169.254 can already read every
//     credential the console holds. If webhooks are ever delegated below
//     admin, an allowlist becomes mandatory in the same change.
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

// SignatureHeader carries the HMAC over the RAW request body:
// "sha256=<hex>". Exported so a receiver written against this console -- and
// the tests -- name the same header rather than two string literals that can
// drift.
const SignatureHeader = "X-Kconmon-Signature"

// TestEvent is the event name a /test ping carries. It is deliberately NOT in
// store's webhook vocabulary: that set is what an endpoint may SUBSCRIBE to,
// and a test ping is addressed to one endpoint by id, never matched against a
// filter.
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

	// drainBudget is how long Close waits for in-flight deliveries. It is a
	// budget for ATTEMPTS, not for the ladder: a delivery sleeping until its
	// 5-minute retry is abandoned immediately (see the at-most-once-ish note
	// in the package comment), while one mid-POST gets a real chance to
	// finish and record its outcome.
	drainBudget = 5 * time.Second

	// maxConcurrent is the worker pool bound. Deliveries are one goroutine
	// each, admitted by a semaphore of this size, and admission is
	// NON-BLOCKING: a full pool drops the delivery (counted + warned) rather
	// than making an HTTP handler wait behind someone else's dead endpoint.
	//
	// The cost is worth naming: because a delivery holds its slot across the
	// whole retry ladder, eight simultaneously-failing endpoints can saturate
	// the pool for the ~5.5 minutes the ladder runs, and notifications
	// arriving in that window are dropped. That is the deliberate shape of a
	// bounded, queue-less v1 -- the alternative, an unbounded goroutine per
	// delivery, turns one unreachable endpoint into an unbounded memory leak.
	maxConcurrent = 8

	// responseDrainLimit is how much of a response body is read before it is
	// discarded, so the connection can be reused. The body is NEVER inspected,
	// logged or stored: a receiver's error page is arbitrary remote content,
	// and last_status is a CLASS, not an echo.
	responseDrainLimit = 4 << 10

	// jitterFraction is the +/-20% spread applied to every non-zero rung of
	// the ladder. Without it, N endpoints notified by one incident retry in
	// lockstep, which is how a struggling receiver gets a synchronized second
	// wave at exactly the moment it is least able to take one.
	jitterFraction = 0.2

	// nonceLen is AES-GCM's standard nonce size. Pinned rather than read from
	// the AEAD so the sealed layout (nonce || ciphertext) is stated once, in
	// the place a reader looks for it.
	nonceLen = 12
)

// retryLadder is the delay BEFORE each attempt: send immediately, retry at
// ~30s, retry again at ~5m, then give up (M6 Decision 5). Three attempts over
// roughly five and a half minutes covers the failure this is actually for -- a
// receiver restarting or briefly rate-limiting -- without pretending to be a
// durable queue, which it is not.
var retryLadder = []time.Duration{0, 30 * time.Second, 5 * time.Minute}

// singleAttempt is the /test ping's ladder. One shot, on purpose: an operator
// clicking "test" is asking a question and waiting for the answer on the
// endpoint row, and answering it five and a half minutes later would be a
// worse answer than "no".
var singleAttempt = []time.Duration{0}

// Store is everything the dispatcher needs from the persistence layer: the
// read seam to find endpoints and their ciphertext, and exactly ONE write --
// UpdateWebhookDelivery. It composes store's two narrow interfaces rather than
// taking *store.DB so a test can substitute a fake with no database at all.
//
// Note what this package can therefore NOT do: create, update or delete an
// endpoint. Webhook CRUD is httpapi's, under an admin-only permission and an
// audit trail; a dispatcher that could rewrite the row it delivers to would be
// a second, unaudited config path.
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

	// sem is the worker-pool bound (maxConcurrent). A token is held for the
	// WHOLE delivery, ladder included.
	sem chan struct{}
	// inflight covers every delivery goroutine, and is what Close's drain
	// budget waits on.
	inflight sync.WaitGroup

	// Two contexts, cancelled at two different moments, which is what makes
	// "drain the attempts, abandon the retries" expressible at all:
	//
	//	baseCtx  governs HTTP attempts and outcome writes. Cancelled LAST, at
	//	         the end of Close, as the hard stop for anything still running
	//	         when the drain budget expired.
	//	retryCtx governs the waits BETWEEN attempts. Cancelled FIRST, so a
	//	         delivery parked until its 5-minute rung wakes immediately and
	//	         records the honest failed outcome instead of holding shutdown.
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

// sleeper waits d, reporting true when the wait COMPLETED and false when it
// was cut short by shutdown or cancellation. The boolean is the whole point:
// a delivery that was interrupted mid-ladder must record a failure rather than
// silently retry into a process that is going away.
type sleeper func(ctx context.Context, d time.Duration) bool

// New builds a Dispatcher. key must be exactly 32 bytes -- the decoded form of
// console.webhooks.encryptionKey, which config.WebhooksConfig.ResolveEncryptionKey
// already validated; the check is repeated here because this constructor is
// also reachable from a test that built the key by hand, and a short key is a
// quietly weaker cipher rather than a visible error.
//
// An error here is a composition failure, not a runtime condition: cmd/console
// treats it as fatal, the same way it treats an unreadable database.dsnFile.
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

// Seal encrypts a plaintext endpoint secret into the ciphertext store.Webhook
// carries: nonce || AES-GCM(plaintext). It implements httpapi's SecretSealer,
// which is the ONLY direction that package is allowed.
//
// The nonce is fresh random per call and prefixed rather than stored in a
// second column, so the ciphertext is one self-describing blob the store can
// treat as opaque bytes -- which is what lets store honestly say it cannot
// read what it holds.
func (d *Dispatcher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("webhooks: seal: read nonce: %w", err)
	}
	return d.gcm.Seal(nonce, nonce, plain, nil), nil
}

// Open is Seal's inverse. A tampered or truncated blob, or one sealed under a
// different key, fails here with GCM's authentication error -- which is the
// point of an AEAD: a rotated key produces an unusable endpoint that says so,
// not an endpoint that signs with garbage.
//
// Deliberately NOT part of any interface httpapi holds.
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

// Payload is the wire body of every INCIDENT-family delivery, test pings
// included (documented as WebhookPayload in docs/console-api.yaml).
//
// The invariant M6 wrote as "one shape" is, as of M7, one shape PER FAMILY,
// and the families are CLOSED. There are two: incident.* carries this type,
// alert.* carries AlertPayload, and a receiver dispatches on the `event` field
// -- read it first, then pick the parser. Within a family the field set never
// changes, so a receiver that dispatches correctly writes each parser once; a
// later milestone that widened either shape without a version marker would
// break every one of them, exactly as it would have in M6.
//
// Splitting rather than widening was the deliberate choice. A single union
// shape would have meant either an incident object full of empty strings on
// every alert delivery -- a synthetic incident that never existed -- or
// omitempty on half the keys, which is a shape that changes per delivery and
// is precisely what "closed and stable" exists to forbid. Two honest shapes
// cost a receiver one switch statement; one dishonest shape costs it a guess
// on every field.
//
// No omitempty anywhere, including on the nullable toAt: a key that sometimes
// vanishes is a second shape, and "stable" has to mean the JSON object has the
// same keys every time. AlertPayload repeats the rule for resolvedAt.
//
// This type's BYTES are frozen at their M6 layout. A delivery that an M6
// console would have produced and one this console produces are identical, and
// the tests assert that on the raw body rather than through the struct.
type Payload struct {
	// Event is one of the store vocabulary values, or "test".
	Event string `json:"event"`
	// Incident is synthetic for a test ping -- same shape, so the receiver's
	// parser does not branch.
	Incident PayloadIncident `json:"incident"`
	// At is when the console decided to notify, NOT when this attempt was
	// made: the body (and therefore the signature) is built once and every
	// retry resends the identical bytes, so a receiver can deduplicate on
	// (event, incident.id, at).
	At time.Time `json:"at"`
}

// PayloadIncident is the incident subset a notification carries. Notes and
// pinned findings are deliberately ABSENT: notes are free-form operator text
// that can run to 16 KiB and can name anything an investigation touched, and
// pushing them to a third-party chat endpoint on every status change would
// export the investigation itself, not the fact of it.
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

// AlertPayload is the wire body of every ALERT-family delivery (M7 Decision 7,
// documented as WebhookAlertPayload in docs/console-api.yaml). Its own closed,
// stable field set -- see Payload's comment for why the families are two
// shapes and not one.
//
// The envelope key is `sentAt` rather than the incident family's `at` on
// purpose: they do not mean the same thing. `at` is when the console DECIDED
// to notify about an incident and is the incident family's deduplication
// component; `sentAt` is only when this alert transition was observed and
// enqueued, and it is NOT a deduplication key -- every console replica polls
// independently (see AlertWatcher), so two replicas observing the same edge
// produce two deliveries with two different sentAt values. Deduplicate on
// (event, alert.ruleId, alert.labels, alert.firedAt), all four of which are
// stable across replicas and across the retry ladder.
type AlertPayload struct {
	// Event is store.WebhookEventAlertFired or store.WebhookEventAlertResolved.
	Event string `json:"event"`
	// SentAt is when this delivery was built. Stable across the retry ladder
	// (the body is marshalled once), NOT stable across replicas.
	SentAt time.Time `json:"sentAt"`
	Alert  Alert     `json:"alert"`
}

// Alert is BOTH the notification seam's input and the payload's `alert` object
// -- one type, not a pair like store.Incident/PayloadIncident. There is no
// alert row to project from: the console does not evaluate alerts, it observes
// Prometheus' state (M7 Decision 6), so the thing a caller hands the
// dispatcher and the thing a receiver reads are the same set of facts, and a
// second struct would only be an opportunity for the two to drift.
type Alert struct {
	// RuleID is the alert_rules row this alert came from, off the
	// kconmon_ng_rule_id label. Never empty on a delivery: the watcher only
	// fires for MANAGED alerts (an unmanaged firing alert belongs to whoever
	// owns that rule, not to this console's endpoints).
	RuleID string `json:"ruleId"`
	// RuleName is the alert's name as PROMETHEUS knows it -- the sanitized
	// alertname, not necessarily the console row's name -- when it had to be
	// read off the label set, and the row's own name when the rule was
	// resolvable. Either way it is the name an operator will search for.
	RuleName string `json:"ruleName"`
	Severity string `json:"severity"`
	// Expr is the rendered PromQL the rule evaluates. It is "" when the row
	// could not be resolved (deleted between the poll and the lookup, or no
	// rule source wired) -- an empty string, never a missing key, and never a
	// guess.
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
	// ResolvedAt is null on alert.fired and set on alert.resolved. The key is
	// PRESENT on both, no omitempty -- Payload's rule, repeated here for
	// Payload's reason.
	//
	// Its value is WHEN THE CONSOLE NOTICED, not when Prometheus stopped
	// firing: the console detects a resolution by an alert's ABSENCE from a
	// poll, and an absence has no timestamp of its own. The honest reading is
	// "resolved at some point in the alertPollInterval ending here". That is
	// the granularity, stated rather than papered over.
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
	// failures is the endpoint's consecutive-failure count as READ at enqueue
	// time. store.UpdateWebhookDelivery SETs rather than increments, so the
	// dispatcher computes the new value; using the enqueue-time snapshot means
	// two deliveries racing on one endpoint can each write failures+1 instead
	// of failures+2. That is an undercount of a number whose only job is to
	// say "this endpoint is unhealthy", which it still does.
	failures int32
}

// Notify is the incident lifecycle seam httpapi calls after a successful
// create or status change (its IncidentNotifier interface). It returns no
// error, and takes none of the caller's time beyond one unpaged SELECT over a
// table an admin typed by hand: everything after the filter is enqueued.
//
// ctx belongs to the REQUEST and is used only for that read. Deliveries run on
// the dispatcher's own context, so a client that disconnects the moment its
// 201 lands does not cancel the notification it just caused.
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

// NotifyAlert is the ALERT transition seam (M7 Decision 7), called by
// AlertWatcher on an edge it detected. Same contract as Notify in every
// respect that matters -- non-blocking, no error, one unpaged SELECT on the
// caller's context and nothing else -- and the same delivery path afterwards.
//
// The ONLY thing that differs between the two is the payload constructor. The
// signing, the ladder, the pool bound, the outcome write and the metric are
// literally the same code: a delivery path that forked per family is how one
// family quietly loses a guard the other keeps.
func (d *Dispatcher) NotifyAlert(ctx context.Context, event string, a Alert) { //nolint:gocritic // hugeParam: Alert is the payload object itself, passed by value so the caller keeps no aliasing claim on the maps
	body, err := json.Marshal(newAlertPayload(event, &a, d.now().UTC()))
	if err != nil {
		slog.Error("webhooks: building the alert notification payload failed", "event", event, "error", err)
		return
	}
	d.fanOut(ctx, event, body)
}

// fanOut lists the endpoints, applies the enabled flag and the event filter,
// and enqueues one delivery per subscriber of an ALREADY-MARSHALLED body.
//
// It takes bytes rather than a payload so the two families share it without
// either of them becoming an interface: the body is opaque from here down, and
// that is exactly the property that keeps the delivery path single.
//
// ctx belongs to the caller and is used only for the list read.
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

// DispatchTest enqueues ONE signed ping to an already-stored endpoint,
// addressed by id alone -- httpapi's TestDispatcher. The error is about
// ACCEPTING the work, never about what the endpoint answered; that outcome
// lands on the endpoint row and is read back from GET /api/v1/webhooks/{id}.
//
// Two deliberate differences from Notify: the ping ignores both the enabled
// flag and the event filter. An operator tests an endpoint precisely when they
// are about to enable it or have just narrowed it, and refusing to test what
// they asked about would answer a question they did not ask.
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
	case d.sem <- struct{}{}:
	default:
		return false
	}
	d.inflight.Add(1)
	go func() {
		defer func() {
			<-d.sem
			d.inflight.Done()
		}()
		d.deliver(j)
	}()
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
		status, ok := d.attempt(&j)
		if ok {
			// failures RESET on success: the column means CONSECUTIVE
			// failures, so one 2xx ends the streak whatever preceded it.
			d.record(j.id, resultOK, statusOK, 0)
			return
		}
		lastStatus = status
	}
	d.record(j.id, resultFailed, lastStatus, j.failures+1)
}

// statusOK is what a delivered endpoint's last_status reads.
const statusOK = "ok"

// attempt performs ONE POST and classifies it. The returned string is the
// last_status class on failure -- a CLASS, never an echo: a receiver's
// response body is arbitrary remote content, and putting it in a column the
// UI renders would make an endpoint an operator does not control an input to
// the console's own display.
func (d *Dispatcher) attempt(j *job) (string, bool) {
	secret, err := d.Open(j.secretEnc)
	if err != nil {
		// The row was sealed under a different key (or corrupted). Terminal
		// and un-retryable, but it still runs the ladder -- costing three
		// no-op cycles -- rather than growing a second exit path; the class
		// below is what tells the operator to re-enter the secret.
		slog.Error("webhooks: endpoint secret could not be decrypted",
			"webhook", j.id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return "failed: secret unreadable (rotated encryption key?)", false
	}
	sig := sign(secret, j.body)
	// secret goes out of scope here; it is never logged, never stored, and
	// never leaves this function.

	ctx, cancel := context.WithTimeout(d.baseCtx, attemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(j.body))
	if err != nil {
		return "failed: invalid endpoint url", false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "kconmon-ng-console")
	req.Header.Set(SignatureHeader, sig)

	resp, err := d.client.Do(req)
	if err != nil {
		// The error is CLASSIFIED, not echoed: a transport error carries the
		// URL, and last_status is served to the UI.
		if ctx.Err() != nil {
			return "failed: timeout after " + attemptTimeout.String(), false
		}
		slog.Warn("webhooks: delivery attempt failed", "webhook", j.id, "event", j.event, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		return "failed: connection error", false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseDrainLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return statusOK, true
	}
	return fmt.Sprintf("failed: HTTP %d", resp.StatusCode), false
}

// record writes the terminal outcome: one metric increment and one endpoint-row
// update. A failed write is warned and dropped -- the delivery already
// happened, and there is nothing useful to retry.
func (d *Dispatcher) record(id, result, lastStatus string, failures int32) {
	d.m.WebhookDeliveries.WithLabelValues(result).Inc()

	ctx, cancel := context.WithTimeout(d.baseCtx, storeWriteTimeout)
	defer cancel()
	if err := d.store.UpdateWebhookDelivery(ctx, id, lastStatus, d.now().UTC(), failures); err != nil {
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

// Close stops the dispatcher: retries are abandoned immediately, in-flight
// ATTEMPTS get drainBudget to finish and record, and anything still running
// after that is hard-cancelled. Idempotent, so Run and an explicit Close in a
// test cannot double-close.
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

// sign builds the X-Kconmon-Signature value over the RAW body bytes -- the
// exact bytes on the wire, not a re-marshalling of the payload, so a receiver
// that verifies before parsing (which is the only safe order) gets the same
// digest the console computed.
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
