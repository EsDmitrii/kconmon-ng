package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/EsDmitrii/kconmon-ng/internal/console/metrics"
	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// testKey is 32 bytes of nothing in particular. Fixed rather than random so a
// failing test is reproducible.
var testKey = bytes.Repeat([]byte{0x2a}, keyLen)

const testSecret = "s3cr3t-signing-key"

// delivery is one recorded outcome write.
type delivery struct {
	id         string
	lastStatus string
	failures   int32
}

// fakeStore is the Store double: the endpoints the dispatcher lists, plus every outcome it wrote;
// deliveries run on their own goroutines, so both halves are mutex-guarded and every write is
// announced on `updated`.
type fakeStore struct {
	mu    sync.Mutex
	hooks map[string]store.Webhook
	order []string

	listErr   error
	getErr    error
	updateErr error

	updates []delivery
	updated chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{hooks: map[string]store.Webhook{}, updated: make(chan struct{}, 64)}
}

// add stores one endpoint whose secret is sealed with sealer, which is how a
// real row gets there (httpapi seals on the way in).
func (f *fakeStore) add(t *testing.T, sealer *Dispatcher, url string, events []string, enabled bool, failures int32) string {
	t.Helper()
	sealed, err := sealer.Seal([]byte(testSecret))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := store.Webhook{
		ID: uuid.NewString(), Name: "hook", URL: url, Events: events,
		SecretEnc: sealed, Enabled: enabled, Failures: failures, CreatedAt: time.Now().UTC(),
	}
	f.hooks[h.ID] = h
	f.order = append(f.order, h.ID)
	return h.ID
}

func (f *fakeStore) ListWebhooks(context.Context) ([]store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Webhook, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.hooks[id])
	}
	return out, nil
}

func (f *fakeStore) GetWebhook(_ context.Context, id string) (store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.Webhook{}, f.getErr
	}
	h, ok := f.hooks[id]
	if !ok {
		return store.Webhook{}, store.ErrNotFound
	}
	return h, nil
}

func (f *fakeStore) UpdateWebhookDelivery(_ context.Context, id, lastStatus string, _ time.Time, failures int32) error {
	f.mu.Lock()
	if f.updateErr == nil {
		f.updates = append(f.updates, delivery{id: id, lastStatus: lastStatus, failures: failures})
	}
	err := f.updateErr
	f.mu.Unlock()
	select {
	case f.updated <- struct{}{}:
	default:
	}
	return err
}

// The rest of store.WebhookStore. The dispatcher must never call these, so
// they fail the process rather than returning a zero value that could let a
// wrong call pass unnoticed.
func (f *fakeStore) CreateWebhook(context.Context, store.WebhookInput) (store.Webhook, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	panic("the dispatcher must never create a webhook")
}

func (f *fakeStore) UpdateWebhook(context.Context, string, store.WebhookInput) (store.Webhook, error) { //nolint:gocritic // hugeParam: mirrors the store signature
	panic("the dispatcher must never update a webhook")
}

func (f *fakeStore) DeleteWebhook(context.Context, string) error {
	panic("the dispatcher must never delete a webhook")
}

func (f *fakeStore) outcomes() []delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]delivery(nil), f.updates...)
}

// waitOutcomes blocks until n terminal outcomes have been recorded. Every test
// that enqueues anything ends here; nothing in this file sleeps.
func (f *fakeStore) waitOutcomes(t *testing.T, n int) []delivery {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		if got := f.outcomes(); len(got) >= n {
			return got
		}
		select {
		case <-f.updated:
		case <-deadline:
			t.Fatalf("timed out waiting for %d delivery outcomes, saw %d", n, len(f.outcomes()))
		}
	}
}

// recordingSleeper replaces the real wait with a recorder, so the retry ladder
// is asserted in microseconds rather than in five and a half minutes.
type recordingSleeper struct {
	mu    sync.Mutex
	waits []time.Duration
	// block, when non-nil, is received from before each wait returns, which
	// lets a test hold a delivery inside its ladder.
	block chan struct{}
}

func (r *recordingSleeper) sleep(ctx context.Context, d time.Duration) bool {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	block := r.block
	r.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return false
		}
	}
	return ctx.Err() == nil
}

func (r *recordingSleeper) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

// newTestDispatcher builds a dispatcher with a fresh registry and a recording
// sleeper, and closes it when the test ends.
func newTestDispatcher(t *testing.T, st Store) (*Dispatcher, *metrics.Metrics, *recordingSleeper) {
	t.Helper()
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	d, err := New(testKey, st, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sl := &recordingSleeper{}
	d.sleep = sl.sleep
	t.Cleanup(d.Close)
	return d, m, sl
}

func deliveryCount(t *testing.T, m *metrics.Metrics, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.WebhookDeliveries.WithLabelValues(result))
}

// receiver is an httptest endpoint that records what it was sent and answers
// with a caller-supplied sequence of status codes.
type receiver struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []receivedRequest
	statuses []int
}

type receivedRequest struct {
	body      []byte
	signature string
	contentTy string
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	t.Helper()
	r := &receiver{statuses: statuses}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, receivedRequest{
			body:      body,
			signature: req.Header.Get(SignatureHeader),
			contentTy: req.Header.Get("Content-Type"),
		})
		status := http.StatusOK
		if len(r.statuses) > 0 {
			status = r.statuses[0]
			if len(r.statuses) > 1 {
				r.statuses = r.statuses[1:]
			}
		}
		r.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) received() []receivedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedRequest(nil), r.requests...)
}

func testIncident() store.Incident {
	to := time.Date(2026, 8, 8, 12, 30, 0, 0, time.UTC)
	return store.Incident{
		ID:        "3f1a0d1e-0000-4000-8000-000000000001",
		Title:     "loss spike between racks",
		Scope:     "node-a",
		FromAt:    time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
		ToAt:      &to,
		Status:    store.IncidentStatusOpen,
		Notes:     "the operator's private working notes -- MUST NOT be delivered",
		Pinned:    json.RawMessage(`[{"kind":"event","id":"e1"}]`),
		CreatedBy: "dmitrii",
		CreatedAt: time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC),
	}
}

// --- crypto -----------------------------------------------------------------

// The round trip is the whole reason both directions live in one package: a
// value httpapi sealed is a value the dispatcher can sign with, and nothing
// else in the process can do either half.
func TestSealOpenRoundTrip(t *testing.T) {
	d, _, _ := newTestDispatcher(t, newFakeStore())

	for _, plain := range []string{"", "s", testSecret, strings.Repeat("x", 1024)} {
		sealed, err := d.Seal([]byte(plain))
		if err != nil {
			t.Fatalf("Seal(%d bytes): %v", len(plain), err)
		}
		if bytes.Contains(sealed, []byte(plain)) && plain != "" {
			t.Errorf("sealed value contains the plaintext -- it is not encrypted")
		}
		opened, err := d.Open(sealed)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if string(opened) != plain {
			t.Errorf("round trip = %q, want %q", opened, plain)
		}
	}
}

// Two seals of one plaintext must differ: the nonce is fresh per call, and a
// deterministic ciphertext would let anyone with database read access tell
// which endpoints share a secret.
func TestSealIsNonDeterministic(t *testing.T) {
	d, _, _ := newTestDispatcher(t, newFakeStore())

	a, err := d.Seal([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Seal([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Error("two seals of the same plaintext are byte-identical -- the nonce is not fresh")
	}
}

// GCM is an AEAD, so a flipped bit anywhere -- nonce or ciphertext -- is an
// authentication failure, not a garbage plaintext that would go on to sign
// deliveries nobody can verify.
func TestOpenRejectsTamperedAndForeignCiphertext(t *testing.T) {
	d, _, _ := newTestDispatcher(t, newFakeStore())
	sealed, err := d.Seal([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func([]byte) []byte{
		"flipped ciphertext bit": func(b []byte) []byte { c := append([]byte(nil), b...); c[len(c)-1] ^= 1; return c },
		"flipped nonce bit":      func(b []byte) []byte { c := append([]byte(nil), b...); c[0] ^= 1; return c },
		"truncated":              func(b []byte) []byte { return b[:len(b)-1] },
		"shorter than the nonce": func(b []byte) []byte { return b[:nonceLen-1] },
		"empty":                  func([]byte) []byte { return nil },
	} {
		if _, openErr := d.Open(mutate(sealed)); openErr == nil {
			t.Errorf("%s: Open succeeded, want an authentication failure", name)
		}
	}

	// A different key is the rotation case, and it must fail loudly rather
	// than produce a secret that signs unverifiable deliveries.
	other, err := New(bytes.Repeat([]byte{0x2b}, keyLen), newFakeStore(),
		metrics.New("kconmon_ng", prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Open(sealed); err == nil {
		t.Error("Open under a different key succeeded, want an authentication failure")
	}
}

func TestNewRejectsAKeyThatIsNotThirtyTwoBytes(t *testing.T) {
	m := metrics.New("kconmon_ng", prometheus.NewRegistry())
	for _, n := range []int{0, 16, 24, 31, 33} {
		if _, err := New(bytes.Repeat([]byte{1}, n), newFakeStore(), m); err == nil {
			t.Errorf("New with a %d-byte key succeeded, want an error", n)
		}
	}
}

// --- delivery ---------------------------------------------------------------

// The signature is asserted the way a receiver actually checks it: recompute
// the HMAC over the RAW bytes that arrived, before parsing them.
func TestDeliverySignatureIsVerifiableOverTheRawBody(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	id := st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	got := st.waitOutcomes(t, 1)

	reqs := rec.received()
	if len(reqs) != 1 {
		t.Fatalf("receiver saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.contentTy != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentTy)
	}
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(req.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if req.signature != want {
		t.Errorf("%s = %q, want %q", SignatureHeader, req.signature, want)
	}
	if got[0] != (delivery{id: id, lastStatus: "ok", failures: 0}) {
		t.Errorf("outcome = %+v, want ok with failures reset", got[0])
	}
	if n := deliveryCount(t, m, resultOK); n != 1 {
		t.Errorf("WebhookDeliveries(ok) = %v, want 1", n)
	}
}

// The payload's field set is the contract a receiver writes a parser against,
// so it is pinned by KEY, not by struct: adding or renaming one is a breaking
// change and has to fail here first.
func TestPayloadFieldSetIsClosedAndStable(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentResolved}, true, 0)

	inc := testIncident()
	d.Notify(context.Background(), store.WebhookEventIncidentResolved, inc)
	st.waitOutcomes(t, 1)

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rec.received()[0].body, &envelope); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}
	assertKeys(t, "payload", envelope, "event", "incident", "at")

	var incident map[string]json.RawMessage
	if err := json.Unmarshal(envelope["incident"], &incident); err != nil {
		t.Fatalf("incident is not a JSON object: %v", err)
	}
	assertKeys(t, "incident", incident, "id", "title", "scope", "status", "fromAt", "toAt", "createdBy")

	var decoded Payload
	if err := json.Unmarshal(rec.received()[0].body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != store.WebhookEventIncidentResolved {
		t.Errorf("event = %q, want %q", decoded.Event, store.WebhookEventIncidentResolved)
	}
	if decoded.Incident.ID != inc.ID || decoded.Incident.Title != inc.Title ||
		decoded.Incident.Scope != inc.Scope || decoded.Incident.CreatedBy != inc.CreatedBy {
		t.Errorf("incident = %+v, want the notified one echoed", decoded.Incident)
	}
	if decoded.Incident.ToAt == nil || !decoded.Incident.ToAt.Equal(*inc.ToAt) {
		t.Errorf("toAt = %v, want %v", decoded.Incident.ToAt, inc.ToAt)
	}

	// The two fields that are deliberately NOT there. Asserted on the raw
	// bytes: a notes field re-added under any spelling would ship an
	// operator's private investigation text to a third-party chat endpoint.
	raw := string(rec.received()[0].body)
	if strings.Contains(raw, "working notes") || strings.Contains(raw, `"notes"`) {
		t.Errorf("payload carries incident notes: %s", raw)
	}
	if strings.Contains(raw, `"pinned"`) {
		t.Errorf("payload carries pinned findings: %s", raw)
	}
}

// --- alert family (M7 Decision 7) -------------------------------------------

func testAlert() Alert {
	return Alert{
		RuleID:      "9c5a1d20-0000-4000-8000-0000000000ab",
		RuleName:    "PairLossHigh",
		Severity:    "critical",
		Expr:        `kconmon_ng_pair_loss_ratio > 0.2`,
		Labels:      map[string]string{"alertname": "PairLossHigh", "severity": "critical", "zone": "a"},
		Annotations: map[string]string{"summary": "loss between racks"},
		FiredAt:     time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
	}
}

// The alert family is its OWN closed shape. Pinned by KEY, exactly the way the
// incident family is, because "stable per family" is only a contract if both
// halves are asserted the same way.
func TestAlertPayloadFieldSetIsClosedAndStable(t *testing.T) {
	for _, event := range []string{store.WebhookEventAlertFired, store.WebhookEventAlertResolved} {
		t.Run(event, func(t *testing.T) {
			rec := newReceiver(t, http.StatusOK)
			st := newFakeStore()
			d, _, _ := newTestDispatcher(t, st)
			st.add(t, d, rec.srv.URL, []string{event}, true, 0)

			a := testAlert()
			if event == store.WebhookEventAlertResolved {
				at := time.Date(2026, 8, 8, 12, 45, 0, 0, time.UTC)
				a.ResolvedAt = &at
			}
			d.NotifyAlert(context.Background(), event, a)
			st.waitOutcomes(t, 1)

			body := rec.received()[0].body
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(body, &envelope); err != nil {
				t.Fatalf("payload is not a JSON object: %v", err)
			}
			assertKeys(t, "alert payload", envelope, "event", "sentAt", "alert")

			var alert map[string]json.RawMessage
			if err := json.Unmarshal(envelope["alert"], &alert); err != nil {
				t.Fatalf("alert is not a JSON object: %v", err)
			}
			assertKeys(t, "alert", alert, "ruleId", "ruleName", "severity", "expr",
				"labels", "annotations", "firedAt", "resolvedAt")

			// The incident family's keys must be ABSENT: a receiver that
			// dispatches on `event` and then reaches for `.incident` on an
			// alert delivery has to get nothing, not a synthetic object.
			if _, ok := envelope["incident"]; ok {
				t.Errorf("alert payload carries an incident object: %s", body)
			}
			if _, ok := envelope["at"]; ok {
				t.Errorf("alert payload carries the incident family's `at`: %s", body)
			}

			var decoded AlertPayload
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Event != event {
				t.Errorf("event = %q, want %q", decoded.Event, event)
			}
			if decoded.Alert.RuleID != a.RuleID || decoded.Alert.RuleName != a.RuleName ||
				decoded.Alert.Severity != a.Severity || decoded.Alert.Expr != a.Expr {
				t.Errorf("alert = %+v, want the notified one echoed", decoded.Alert)
			}
			if !decoded.Alert.FiredAt.Equal(a.FiredAt) {
				t.Errorf("firedAt = %v, want %v", decoded.Alert.FiredAt, a.FiredAt)
			}
			if decoded.Alert.Labels["zone"] != "a" || decoded.Alert.Annotations["summary"] != "loss between racks" {
				t.Errorf("labels/annotations = %v / %v, want them echoed verbatim",
					decoded.Alert.Labels, decoded.Alert.Annotations)
			}

			// resolvedAt is the key the reasoning is repeated for: PRESENT on both events; asserted on the
			// RAW bytes, because a `null` and a missing key decode identically into a *time.Time and only
			// one of them is the contract.
			raw := string(body)
			switch event {
			case store.WebhookEventAlertFired:
				if !strings.Contains(raw, `"resolvedAt":null`) {
					t.Errorf("alert.fired must carry resolvedAt:null, got: %s", raw)
				}
				if decoded.Alert.ResolvedAt != nil {
					t.Errorf("resolvedAt = %v on a fired event, want nil", decoded.Alert.ResolvedAt)
				}
			case store.WebhookEventAlertResolved:
				if strings.Contains(raw, `"resolvedAt":null`) {
					t.Errorf("alert.resolved must carry a real resolvedAt, got: %s", raw)
				}
				if decoded.Alert.ResolvedAt == nil || !decoded.Alert.ResolvedAt.Equal(*a.ResolvedAt) {
					t.Errorf("resolvedAt = %v, want %v", decoded.Alert.ResolvedAt, a.ResolvedAt)
				}
			}
		})
	}
}

// labels and annotations are ALWAYS objects. A nil Go map marshals to `null`,
// which is a second shape for anyone who iterates the object without checking
// -- the same class of bug the closed field set exists to prevent.
func TestAlertPayloadLabelsAndAnnotationsAreAlwaysObjects(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventAlertFired}, true, 0)

	a := testAlert()
	a.Labels, a.Annotations = nil, nil
	d.NotifyAlert(context.Background(), store.WebhookEventAlertFired, a)
	st.waitOutcomes(t, 1)

	raw := string(rec.received()[0].body)
	if !strings.Contains(raw, `"labels":{}`) {
		t.Errorf("nil labels must marshal to {}, got: %s", raw)
	}
	if !strings.Contains(raw, `"annotations":{}`) {
		t.Errorf("nil annotations must marshal to {}, got: %s", raw)
	}
}

// The delivery PATH does not fork: an alert payload is signed, laddered and
// recorded by exactly the code an incident payload goes through. Asserted the
// way a receiver checks it -- recompute over the raw bytes that arrived.
func TestAlertDeliverySignatureIsVerifiableOverTheRawBody(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	id := st.add(t, d, rec.srv.URL, []string{store.WebhookEventAlertFired}, true, 0)

	d.NotifyAlert(context.Background(), store.WebhookEventAlertFired, testAlert())
	got := st.waitOutcomes(t, 1)

	req := rec.received()[0]
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(req.body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); req.signature != want {
		t.Errorf("%s = %q, want %q", SignatureHeader, req.signature, want)
	}
	if req.contentTy != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentTy)
	}
	if got[0] != (delivery{id: id, lastStatus: "ok", failures: 0}) {
		t.Errorf("outcome = %+v, want the same terminal outcome an incident delivery records", got[0])
	}
	if n := deliveryCount(t, m, resultOK); n != 1 {
		t.Errorf("WebhookDeliveries(ok) = %v, want 1 -- one pool, one counter, both families", n)
	}
}

// Subscription is per EVENT, not per family: an incident-only endpoint is
// filtered out of an alert fan-out and vice versa, through the one filter.
func TestFamiliesAreFilteredThroughTheOneSubscription(t *testing.T) {
	incOnly := newReceiver(t, http.StatusOK)
	alertOnly := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	st.add(t, d, incOnly.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)
	st.add(t, d, alertOnly.srv.URL, []string{store.WebhookEventAlertFired}, true, 0)

	d.NotifyAlert(context.Background(), store.WebhookEventAlertFired, testAlert())
	st.waitOutcomes(t, 1)

	if n := len(incOnly.received()); n != 0 {
		t.Errorf("the incident-only endpoint saw %d alert deliveries, want 0", n)
	}
	if n := len(alertOnly.received()); n != 1 {
		t.Fatalf("the alert endpoint saw %d deliveries, want 1", n)
	}
	if n := deliveryCount(t, m, resultFiltered); n != 1 {
		t.Errorf("WebhookDeliveries(filtered) = %v, want 1", n)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(alertOnly.received()[0].body, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["alert"]; !ok {
		t.Errorf("the alert endpoint was sent an incident-shaped body: %s", alertOnly.received()[0].body)
	}
}

// A store the dispatcher cannot read is survivable on the alert path too:
// Notify's contract, asserted for the family that was added later.
func TestNotifyAlertWithAnUnreadableStoreIsSilentlySurvivable(t *testing.T) {
	st := newFakeStore()
	st.listErr = errors.New("connection refused")
	d, m, _ := newTestDispatcher(t, st)

	d.NotifyAlert(context.Background(), store.WebhookEventAlertFired, testAlert())

	for _, result := range []string{resultOK, resultFailed, resultFiltered} {
		if n := deliveryCount(t, m, result); n != 0 {
			t.Errorf("WebhookDeliveries(%s) = %v, want 0", result, n)
		}
	}
}

func assertKeys(t *testing.T, what string, obj map[string]json.RawMessage, want ...string) {
	t.Helper()
	if len(obj) != len(want) {
		t.Errorf("%s has %d keys, want exactly %d (%v)", what, len(obj), len(want), want)
	}
	for _, k := range want {
		if _, ok := obj[k]; !ok {
			t.Errorf("%s is missing key %q -- the field set is closed and stable, no omitempty", what, k)
		}
	}
}

// The ladder, asserted through the injected sleeper: attempt one waits for
// nothing, and the two retries land inside 30s and 5m +/-20%.
func TestRetryLadderIsImmediateThenThirtySecondsThenFiveMinutes(t *testing.T) {
	rec := newReceiver(t, http.StatusInternalServerError)
	st := newFakeStore()
	d, m, sl := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	st.waitOutcomes(t, 1)

	if n := len(rec.received()); n != 3 {
		t.Fatalf("receiver saw %d attempts, want 3", n)
	}
	waits := sl.recorded()
	if len(waits) != 2 {
		t.Fatalf("sleeper saw %d waits, want 2 (the first attempt is immediate): %v", len(waits), waits)
	}
	for i, rung := range []time.Duration{30 * time.Second, 5 * time.Minute} {
		lo := time.Duration(float64(rung) * (1 - jitterFraction))
		hi := time.Duration(float64(rung) * (1 + jitterFraction))
		if waits[i] < lo || waits[i] > hi {
			t.Errorf("retry %d waited %v, want within +/-%.0f%% of %v (%v..%v)",
				i+1, waits[i], jitterFraction*100, rung, lo, hi)
		}
	}
	if n := deliveryCount(t, m, resultFailed); n != 1 {
		t.Errorf("WebhookDeliveries(failed) = %v, want exactly 1 -- one per DELIVERY, not per attempt", n)
	}
	if n := deliveryCount(t, m, resultOK); n != 0 {
		t.Errorf("WebhookDeliveries(ok) = %v, want 0", n)
	}
}

// Jitter must actually vary. A fixed ladder is how N endpoints notified by one
// incident produce a synchronized second wave at a receiver already struggling.
func TestJitterVariesAndStaysInsideItsBand(t *testing.T) {
	const rung = 30 * time.Second
	lo := time.Duration(float64(rung) * (1 - jitterFraction))
	hi := time.Duration(float64(rung) * (1 + jitterFraction))

	seen := map[time.Duration]bool{}
	for range 200 {
		got := jitter(rung)
		if got < lo || got > hi {
			t.Fatalf("jitter(%v) = %v, outside %v..%v", rung, got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 100 {
		t.Errorf("jitter produced %d distinct values over 200 calls -- the spread is not doing its job", len(seen))
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0 -- the first rung must stay immediate", got)
	}
}

// A retry that lands is a SUCCESS, and the endpoint's failure streak ends with
// it: last_status counts CONSECUTIVE failures, so one 2xx resets it whatever
// preceded it.
func TestSuccessOnTheSecondAttemptResetsFailures(t *testing.T) {
	rec := newReceiver(t, http.StatusBadGateway, http.StatusNoContent)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	id := st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 7)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	got := st.waitOutcomes(t, 1)

	if n := len(rec.received()); n != 2 {
		t.Fatalf("receiver saw %d attempts, want 2 (fail then succeed)", n)
	}
	if got[0] != (delivery{id: id, lastStatus: "ok", failures: 0}) {
		t.Errorf("outcome = %+v, want ok with failures reset from 7 to 0", got[0])
	}
	if n := deliveryCount(t, m, resultOK); n != 1 {
		t.Errorf("WebhookDeliveries(ok) = %v, want 1", n)
	}
	// Retries resend byte-identical bodies, which is what makes
	// (event, incident.id, at) a usable deduplication key on the receiver.
	reqs := rec.received()
	if !bytes.Equal(reqs[0].body, reqs[1].body) || reqs[0].signature != reqs[1].signature {
		t.Error("the retry sent a different body or signature -- a receiver cannot deduplicate on it")
	}
}

// The failure classes, end to end. last_status is a CLASS, never an echo: the
// receiver's body is arbitrary remote content and this column is rendered by
// the UI.
func TestFailureClassesAndFailureCountIncrement(t *testing.T) {
	t.Run("http status", func(t *testing.T) {
		rec := newReceiver(t, http.StatusTeapot)
		st := newFakeStore()
		d, _, _ := newTestDispatcher(t, st)
		st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 4)

		d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
		got := st.waitOutcomes(t, 1)
		if got[0].lastStatus != "failed: HTTP 418" {
			t.Errorf("lastStatus = %q, want %q", got[0].lastStatus, "failed: HTTP 418")
		}
		if got[0].failures != 5 {
			t.Errorf("failures = %d, want 5 (4 + this one)", got[0].failures)
		}
	})

	t.Run("connection error", func(t *testing.T) {
		// A closed listener: an address that parses and refuses.
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := dead.URL
		dead.Close()

		st := newFakeStore()
		d, _, _ := newTestDispatcher(t, st)
		st.add(t, d, url, []string{store.WebhookEventIncidentCreated}, true, 0)

		d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
		got := st.waitOutcomes(t, 1)
		if got[0].lastStatus != "failed: connection error" {
			t.Errorf("lastStatus = %q, want %q", got[0].lastStatus, "failed: connection error")
		}
		if got[0].failures != 1 {
			t.Errorf("failures = %d, want 1", got[0].failures)
		}
	})

	t.Run("secret sealed under another key", func(t *testing.T) {
		rec := newReceiver(t, http.StatusOK)
		st := newFakeStore()
		other, err := New(bytes.Repeat([]byte{0x2b}, keyLen), st,
			metrics.New("kconmon_ng", prometheus.NewRegistry()))
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		d, _, _ := newTestDispatcher(t, st)
		st.add(t, other, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

		d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
		got := st.waitOutcomes(t, 1)
		if !strings.HasPrefix(got[0].lastStatus, "failed: secret unreadable") {
			t.Errorf("lastStatus = %q, want the rotated-key class", got[0].lastStatus)
		}
		if n := len(rec.received()); n != 0 {
			t.Errorf("receiver saw %d requests, want 0 -- nothing may be sent unsigned", n)
		}
	})
}

// last_status must never carry the receiver's own words, however inviting.
func TestFailureStatusNeverEchoesTheResponseBody(t *testing.T) {
	const leak = "PANIC: connect to postgres://user:hunter2@db:5432 failed"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(leak))
	}))
	defer srv.Close()

	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)
	st.add(t, d, srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	got := st.waitOutcomes(t, 1)
	if strings.Contains(got[0].lastStatus, "hunter2") || strings.Contains(got[0].lastStatus, "PANIC") {
		t.Errorf("lastStatus = %q -- it echoed the receiver's body", got[0].lastStatus)
	}
	if len(got[0].lastStatus) > 255 {
		t.Errorf("lastStatus is %d bytes, over store's 255 bound", len(got[0].lastStatus))
	}
}

// --- filtering --------------------------------------------------------------

// An endpoint that did not subscribe is FILTERED: counted, never contacted.
// The counter is what makes ok/(ok+failed) meaningful -- without it, "no
// matching endpoints" and "the dispatcher is broken" look identical.
func TestEventMismatchIsFilteredAndNeverDelivered(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentResolved}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())

	if n := deliveryCount(t, m, resultFiltered); n != 1 {
		t.Errorf("WebhookDeliveries(filtered) = %v, want 1", n)
	}
	if n := len(rec.received()); n != 0 {
		t.Errorf("receiver saw %d requests, want 0", n)
	}
	if got := st.outcomes(); len(got) != 0 {
		t.Errorf("outcomes = %+v, want none -- a filtered endpoint's row is not touched", got)
	}
}

// A DISABLED endpoint is skipped SILENTLY -- not delivered, and not counted as
// filtered either. It was switched off deliberately, and a series climbing
// forever would report an operator's own decision back to them as activity.
func TestDisabledEndpointIsSkippedAndNotCounted(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, false, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())

	if n := len(rec.received()); n != 0 {
		t.Errorf("receiver saw %d requests, want 0", n)
	}
	for _, result := range []string{resultOK, resultFailed, resultFiltered} {
		if n := deliveryCount(t, m, result); n != 0 {
			t.Errorf("WebhookDeliveries(%s) = %v, want 0 for a disabled endpoint", result, n)
		}
	}
}

// One incident, three endpoints, one subscriber: the fan-out picks exactly the
// matching row.
func TestNotifyFansOutToEverySubscriberAndOnlyThose(t *testing.T) {
	hit := newReceiver(t, http.StatusOK)
	alsoHit := newReceiver(t, http.StatusOK)
	miss := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	st.add(t, d, hit.srv.URL, []string{store.WebhookEventIncidentCreated, store.WebhookEventIncidentResolved}, true, 0)
	st.add(t, d, alsoHit.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)
	st.add(t, d, miss.srv.URL, []string{store.WebhookEventIncidentReopened}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	st.waitOutcomes(t, 2)

	if n := len(hit.received()); n != 1 {
		t.Errorf("first subscriber saw %d requests, want 1", n)
	}
	if n := len(alsoHit.received()); n != 1 {
		t.Errorf("second subscriber saw %d requests, want 1", n)
	}
	if n := len(miss.received()); n != 0 {
		t.Errorf("non-subscriber saw %d requests, want 0", n)
	}
	if n := deliveryCount(t, m, resultOK); n != 2 {
		t.Errorf("WebhookDeliveries(ok) = %v, want 2", n)
	}
	if n := deliveryCount(t, m, resultFiltered); n != 1 {
		t.Errorf("WebhookDeliveries(filtered) = %v, want 1", n)
	}
}

// A store outage during Notify is logged and dropped -- never a panic, and
// never something the caller has to handle, because Notify has no error to
// return by design.
func TestNotifyWithAnUnreadableStoreIsSilentlySurvivable(t *testing.T) {
	st := newFakeStore()
	st.listErr = errors.New("connection refused")
	d, m, _ := newTestDispatcher(t, st)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())

	for _, result := range []string{resultOK, resultFailed, resultFiltered} {
		if n := deliveryCount(t, m, result); n != 0 {
			t.Errorf("WebhookDeliveries(%s) = %v, want 0", result, n)
		}
	}
}

// --- pool saturation --------------------------------------------------------

// A full pool DROPS rather than blocks: the caller is an HTTP handler, and
// making it wait behind someone else's dead endpoint is the one thing the
// non-blocking posture exists to prevent.
func TestSaturatedPoolDropsTheDeliveryAndCountsItFailed(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	// Occupy every worker slot, which is what an endpoint stuck inside its
	// five-minute ladder does to the pool.
	for range maxConcurrent {
		d.sem <- struct{}{}
	}

	done := make(chan struct{})
	go func() {
		d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a saturated pool -- it must never block its caller")
	}

	if n := deliveryCount(t, m, resultFailed); n != 1 {
		t.Errorf("WebhookDeliveries(failed) = %v, want 1 for the dropped delivery", n)
	}
	if n := len(rec.received()); n != 0 {
		t.Errorf("receiver saw %d requests, want 0", n)
	}
	for range maxConcurrent {
		<-d.sem
	}
}

// DispatchTest reports saturation SYNCHRONOUSLY (the handler answers 502
// instead of 202) and therefore does not also count a failed delivery -- the
// operator already learned about it.
func TestDispatchTestOnASaturatedPoolErrorsWithoutCountingAFailure(t *testing.T) {
	rec := newReceiver(t, http.StatusOK)
	st := newFakeStore()
	d, m, _ := newTestDispatcher(t, st)
	id := st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)
	for range maxConcurrent {
		d.sem <- struct{}{}
	}

	if err := d.DispatchTest(context.Background(), id); err == nil {
		t.Error("DispatchTest on a saturated pool returned nil, want an error the handler can 502 on")
	}
	if n := deliveryCount(t, m, resultFailed); n != 0 {
		t.Errorf("WebhookDeliveries(failed) = %v, want 0 -- the caller already learned synchronously", n)
	}
	for range maxConcurrent {
		<-d.sem
	}
}

// --- test ping --------------------------------------------------------------

// One shot, the same envelope, and it ignores both the enabled flag and the
// event filter -- an operator tests an endpoint precisely when they are about
// to enable it.
func TestDispatchTestIsASingleSignedShotAtADisabledEndpoint(t *testing.T) {
	rec := newReceiver(t, http.StatusInternalServerError)
	st := newFakeStore()
	d, m, sl := newTestDispatcher(t, st)
	id := st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentReopened}, false, 2)

	if err := d.DispatchTest(context.Background(), id); err != nil {
		t.Fatalf("DispatchTest: %v", err)
	}
	got := st.waitOutcomes(t, 1)

	reqs := rec.received()
	if len(reqs) != 1 {
		t.Fatalf("receiver saw %d attempts, want exactly 1 -- a test ping has no retry ladder", len(reqs))
	}
	if waits := sl.recorded(); len(waits) != 0 {
		t.Errorf("sleeper saw %v, want no waits at all", waits)
	}
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(reqs[0].body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); reqs[0].signature != want {
		t.Errorf("test ping signature = %q, want %q", reqs[0].signature, want)
	}

	var p Payload
	if err := json.Unmarshal(reqs[0].body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != TestEvent {
		t.Errorf("event = %q, want %q", p.Event, TestEvent)
	}
	if p.Incident.Title != testIncidentTitle || p.Incident.ID != "" {
		t.Errorf("incident = %+v, want the synthetic test one", p.Incident)
	}
	if got[0].lastStatus != "failed: HTTP 500" || got[0].failures != 3 {
		t.Errorf("outcome = %+v, want the failure recorded on the row (2 + this one)", got[0])
	}
	if n := deliveryCount(t, m, resultFailed); n != 1 {
		t.Errorf("WebhookDeliveries(failed) = %v, want 1", n)
	}
}

// An id that names nothing is an error, not a queued delivery for work that
// can never happen.
func TestDispatchTestOnAnUnknownEndpointErrors(t *testing.T) {
	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)

	err := d.DispatchTest(context.Background(), uuid.NewString())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DispatchTest(unknown) = %v, want a wrapped store.ErrNotFound", err)
	}
}

// --- secrecy ----------------------------------------------------------------

// The plaintext secret, its sealed form and the signing key must be absent from EVERY log line the
// dispatcher emits.
func TestSecretNeverReachesTheLogs(t *testing.T) {
	var buf lockedBuffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	fail := newReceiver(t, http.StatusInternalServerError)
	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)
	sealed := mustSeal(t, d)
	st.add(t, d, fail.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	// A second endpoint whose secret this dispatcher cannot open, so the
	// rotated-key error path -- the most tempting place to log the ciphertext
	// -- runs too.
	otherKeyed, err := New(bytes.Repeat([]byte{0x2b}, keyLen), st, metrics.New("kconmon_ng", prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	defer otherKeyed.Close()
	st.add(t, otherKeyed, fail.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	st.waitOutcomes(t, 2)
	d.Close()

	logs := buf.String()
	if logs == "" {
		t.Fatal("captured no log output at all -- the assertion below would be vacuous")
	}
	for _, forbidden := range []string{testSecret, string(sealed), hexOf(sealed)} {
		if forbidden != "" && strings.Contains(logs, forbidden) {
			t.Errorf("the logs carry the endpoint secret or its ciphertext:\n%s", logs)
		}
	}
	// The URL is Debug-only, so it IS here -- but only because the level was
	// turned all the way down for this test. The assertion below is what keeps
	// it out of an ordinary Info-level deployment's logs.
	if !strings.Contains(logs, fail.srv.URL) {
		t.Errorf("expected the endpoint URL at DEBUG level, got:\n%s", logs)
	}
}

func TestEndpointURLIsNotLoggedAboveDebug(t *testing.T) {
	var buf lockedBuffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(restore) })

	fail := newReceiver(t, http.StatusInternalServerError)
	st := newFakeStore()
	d, _, _ := newTestDispatcher(t, st)
	st.add(t, d, fail.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 0)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())
	st.waitOutcomes(t, 1)

	if strings.Contains(buf.String(), fail.srv.URL) {
		t.Errorf("an Info-level log line names the endpoint URL:\n%s", buf.String())
	}
}

func mustSeal(t *testing.T, d *Dispatcher) []byte {
	t.Helper()
	sealed, err := d.Seal([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// --- shutdown ---------------------------------------------------------------

// Close abandons a delivery parked on its retry rung and records the honest
// failure -- the at-most-once-ish contract, made visible on the endpoint row
// rather than left to the operator to infer from silence.
func TestCloseAbandonsPendingRetriesAndRecordsTheFailure(t *testing.T) {
	rec := newReceiver(t, http.StatusServiceUnavailable)
	st := newFakeStore()
	d, m, sl := newTestDispatcher(t, st)
	sl.block = make(chan struct{}) // hold the delivery inside its first retry wait
	st.add(t, d, rec.srv.URL, []string{store.WebhookEventIncidentCreated}, true, 1)

	d.Notify(context.Background(), store.WebhookEventIncidentCreated, testIncident())

	// Wait until the first attempt has been made and the worker is parked.
	deadline := time.After(5 * time.Second)
	for len(rec.received()) == 0 {
		select {
		case <-deadline:
			t.Fatal("the first attempt never happened")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	closed := make(chan struct{})
	go func() { d.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return -- the retry wait was not interrupted")
	}

	got := st.waitOutcomes(t, 1)
	if got[0].lastStatus != "failed: HTTP 503" || got[0].failures != 2 {
		t.Errorf("outcome = %+v, want the interrupted delivery recorded as failed", got[0])
	}
	if n := len(rec.received()); n != 1 {
		t.Errorf("receiver saw %d attempts, want 1 -- the remaining rungs are lost by design", n)
	}
	if n := deliveryCount(t, m, resultFailed); n != 1 {
		t.Errorf("WebhookDeliveries(failed) = %v, want 1", n)
	}
}

// Run is Close with a context in front of it, which is what lets cmd/console
// spawn it through the same helper as every other background component.
func TestRunReturnsOnContextCancellation(t *testing.T) {
	d, _, _ := newTestDispatcher(t, newFakeStore())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	// Idempotent: the deferred Close from newTestDispatcher must not panic.
	d.Close()
}
