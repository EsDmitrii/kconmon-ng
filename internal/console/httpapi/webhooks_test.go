package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// The plaintext secret every webhook test posts, and the marker the fake
// sealer wraps it in. BOTH are searched for in raw response bodies and audit
// rows: the sealed form must never appear either, or a leak would merely have
// changed shape.
const (
	testWebhookSecret = "s3cr3t-signing-key"
	sealedPrefix      = "SEALED("
)

// fakeSealer is the SecretSealer double. The ciphertext is deliberately
// RECOGNISABLE -- "SEALED(<plain>)" -- so a leak test can assert that neither
// the plaintext nor the thing the handler actually stored reaches the wire.
type fakeSealer struct {
	err   error
	calls int
	mu    sync.Mutex
}

func (f *fakeSealer) Seal(plain []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return []byte(sealedPrefix + string(plain) + ")"), nil
}

func (f *fakeSealer) sealCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTestDispatcher is the TestDispatcher double: it records the ids it was
// asked to ping, which is the whole of the seam M6 Task 5 will implement.
type fakeTestDispatcher struct {
	mu   sync.Mutex
	ids  []string
	err  error
	seen map[string]bool
}

func newFakeTestDispatcher() *fakeTestDispatcher {
	return &fakeTestDispatcher{seen: map[string]bool{}}
}

func (f *fakeTestDispatcher) DispatchTest(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.ids = append(f.ids, id)
	f.seen[id] = true
	return nil
}

func (f *fakeTestDispatcher) dispatched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ids...)
}

// fakeWebhookStore is one double for WebhookService, mutex-guarded. Validation
// goes through the REAL store.WebhookInput.Validate.
type fakeWebhookStore struct {
	mu sync.Mutex

	hooks map[string]store.Webhook
	order []string

	createErr error
	listErr   error
	getErr    error
	updateErr error
	deleteErr error
}

func newFakeWebhookStore() *fakeWebhookStore {
	return &fakeWebhookStore{hooks: map[string]store.Webhook{}}
}

func (f *fakeWebhookStore) CreateWebhook(_ context.Context, in store.WebhookInput) (store.Webhook, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return store.Webhook{}, f.createErr
	}
	if err := in.Validate(); err != nil {
		return store.Webhook{}, err
	}
	for _, h := range f.hooks {
		if h.Name == in.Name {
			return store.Webhook{}, store.ErrAlreadyExists
		}
	}
	hook := store.Webhook{
		ID: uuid.NewString(), Name: in.Name, URL: in.URL, Events: in.Events,
		SecretEnc: in.SecretEnc, Enabled: in.Enabled, CreatedAt: time.Now().UTC(),
	}
	f.hooks[hook.ID] = hook
	f.order = append(f.order, hook.ID)
	return hook, nil
}

func (f *fakeWebhookStore) GetWebhook(_ context.Context, id string) (store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return store.Webhook{}, f.getErr
	}
	hook, ok := f.hooks[id]
	if !ok {
		return store.Webhook{}, store.ErrNotFound
	}
	return hook, nil
}

func (f *fakeWebhookStore) ListWebhooks(context.Context) ([]store.Webhook, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.Webhook, 0, len(f.order))
	for _, id := range f.order {
		if hook, ok := f.hooks[id]; ok {
			out = append(out, hook)
		}
	}
	return out, nil
}

func (f *fakeWebhookStore) UpdateWebhook(_ context.Context, id string, in store.WebhookInput) (store.Webhook, error) { //nolint:gocritic // hugeParam: test double mirrors the store signature
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return store.Webhook{}, f.updateErr
	}
	if err := in.Validate(); err != nil {
		return store.Webhook{}, err
	}
	hook, ok := f.hooks[id]
	if !ok {
		return store.Webhook{}, store.ErrNotFound
	}
	hook.Name, hook.URL, hook.Events = in.Name, in.URL, in.Events
	hook.SecretEnc, hook.Enabled = in.SecretEnc, in.Enabled
	f.hooks[id] = hook
	return hook, nil
}

func (f *fakeWebhookStore) UpdateWebhookDelivery(_ context.Context, id, lastStatus string, lastAttempt time.Time, failures int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	hook, ok := f.hooks[id]
	if !ok {
		return store.ErrNotFound
	}
	hook.LastStatus, hook.LastAttempt, hook.Failures = lastStatus, &lastAttempt, failures
	f.hooks[id] = hook
	return nil
}

func (f *fakeWebhookStore) DeleteWebhook(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.hooks[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.hooks, id)
	return nil
}

func (f *fakeWebhookStore) seed(name string) string {
	hook, err := f.CreateWebhook(context.Background(), store.WebhookInput{
		Name: name, URL: "https://hooks.example.test/" + name,
		Events:    []string{store.WebhookEventIncidentCreated},
		SecretEnc: []byte(sealedPrefix + testWebhookSecret + ")"), Enabled: true,
	})
	if err != nil {
		panic(err)
	}
	return hook.ID
}

func (f *fakeWebhookStore) get(t *testing.T, id string) store.Webhook {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	hook, ok := f.hooks[id]
	if !ok {
		t.Fatalf("webhook %q is not in the fake store", id)
	}
	return hook
}

const validWebhookBody = `{"name":"slack-oncall","url":"https://hooks.example.test/T0/B0/xoxb-token",` +
	`"events":["incident.created","incident.resolved"],"secret":"` + testWebhookSecret + `"}`

func webhookRoutes(id string) []struct{ method, path, body string } {
	return []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/webhooks", ""},
		{http.MethodPost, "/api/v1/webhooks", validWebhookBody},
		{http.MethodGet, "/api/v1/webhooks/" + id, ""},
		{http.MethodPut, "/api/v1/webhooks/" + id, validWebhookBody},
		{http.MethodDelete, "/api/v1/webhooks/" + id, ""},
		{http.MethodPost, "/api/v1/webhooks/" + id + "/test", ""},
	}
}

func TestWebhooksWithoutStoreReturn503(t *testing.T) {
	s := newM5TestServer(t, "admin", Deps{})
	for _, c := range webhookRoutes(uuid.NewString()) {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a WebhookService = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "console.database.mode") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.database.mode", c.method, c.path, w.Body)
		}
	}
}

// M6 Decision 8's sharpest line, against the REAL built-in roles: webhooks are
// ADMIN ONLY. Every other role -- including the operator that holds
// incidents:write and can open the very incidents these endpoints fire on --
// is refused on every route, read included.
func TestWebhooksAreAdminOnly(t *testing.T) {
	for _, role := range []string{"viewer", "alert-editor", "operator"} {
		st := newFakeWebhookStore()
		id := st.seed("seeded")
		s := newM5TestServer(t, role, Deps{Webhooks: st, WebhookSealer: &fakeSealer{}})
		for _, c := range webhookRoutes(id) {
			var mutate func(*http.Request)
			if isMutatingMethod(c.method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403 (webhooks:manage is admin-only): %s",
					role, c.method, c.path, w.Code, w.Body)
			}
		}
	}
}

func TestWebhooksAdminReachesEveryRoute(t *testing.T) {
	st := newFakeWebhookStore()
	sealer := &fakeSealer{}
	disp := newFakeTestDispatcher()
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: sealer, WebhookTestDispatcher: disp})

	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(validWebhookBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", w.Code, w.Body)
	}
	var got webhookResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "slack-oncall" || len(got.Events) != 2 {
		t.Errorf("body = %+v, want the created endpoint echoed back", got)
	}
	if !got.Enabled {
		t.Errorf("enabled = false, want an omitted enabled to default to true")
	}
	if want := "/api/v1/webhooks/" + got.ID; w.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", w.Header().Get("Location"), want)
	}
	if sealer.sealCalls() != 1 {
		t.Errorf("sealer calls = %d, want exactly 1 -- the handler must not encrypt itself", sealer.sealCalls())
	}
	if stored := st.get(t, got.ID); string(stored.SecretEnc) != sealedPrefix+testWebhookSecret+")" {
		t.Errorf("stored secret = %q, want the SEALED form, never the plaintext", stored.SecretEnc)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/webhooks", "", http.StatusOK},
		{http.MethodGet, "/api/v1/webhooks/" + got.ID, "", http.StatusOK},
		{http.MethodPost, "/api/v1/webhooks/" + got.ID + "/test", "", http.StatusAccepted},
		{http.MethodDelete, "/api/v1/webhooks/" + got.ID, "", http.StatusNoContent},
	} {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w = doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
	if ids := disp.dispatched(); len(ids) != 1 || ids[0] != got.ID {
		t.Errorf("dispatcher saw %v, want exactly the tested endpoint's id", ids)
	}
}

// The write-only guarantee, asserted on the RAW response bytes rather than a
// decoded struct: a field this package never declared could still be there if
// someone marshalled a store type by accident.
func TestWebhookSecretNeverAppearsInAnyResponse(t *testing.T) {
	st := newFakeWebhookStore()
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: &fakeSealer{}})

	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(validWebhookBody), mutateWithCSRF)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST = %d: %s", w.Code, w.Body)
	}
	var created webhookResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	bodies := map[string]string{"POST": w.Body.String()}
	for name, path := range map[string]string{
		"GET one":  "/api/v1/webhooks/" + created.ID,
		"GET list": "/api/v1/webhooks",
	} {
		got := doRequest(t, s, http.MethodGet, path, nil, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", name, got.Code, got.Body)
		}
		bodies[name] = got.Body.String()
	}

	for name, body := range bodies {
		for _, banned := range []string{testWebhookSecret, sealedPrefix, `"secret"`, "secretEnc"} {
			if strings.Contains(body, banned) {
				t.Errorf("%s body contains %q -- the secret is WRITE-ONLY at every layer: %s", name, banned, body)
			}
		}
		if !strings.Contains(body, `"hasSecret":true`) {
			t.Errorf("%s body = %s, want hasSecret:true instead of the secret", name, body)
		}
	}
}

// Secret lifecycle on update: ABSENT keeps the stored one, "" is 422, a new
// value replaces it.
func TestWebhookUpdateSecretLifecycle(t *testing.T) {
	st := newFakeWebhookStore()
	sealer := &fakeSealer{}
	id := st.seed("slack-oncall")
	original := st.get(t, id).SecretEnc
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: sealer})

	// 1. Absent secret -> the stored ciphertext survives a full-replace PUT.
	body := `{"name":"slack-oncall","url":"https://hooks.example.test/moved",` +
		`"events":["incident.resolved"],"enabled":false}`
	w := doRequest(t, s, http.MethodPut, "/api/v1/webhooks/"+id, strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT without a secret = %d, want 200: %s", w.Code, w.Body)
	}
	after := st.get(t, id)
	if string(after.SecretEnc) != string(original) {
		t.Errorf("secret = %q after a PUT that omitted it, want the stored one KEPT (%q)", after.SecretEnc, original)
	}
	if after.URL != "https://hooks.example.test/moved" || after.Enabled {
		t.Errorf("stored row = %+v, want the rest of the body fully replaced", after)
	}
	if sealer.sealCalls() != 0 {
		t.Errorf("sealer calls = %d, want 0 -- keeping a secret needs no cipher", sealer.sealCalls())
	}

	// 2. Empty secret -> 422, never a guess about whether it means keep or clear.
	empty := `{"name":"slack-oncall","url":"https://hooks.example.test/moved",` +
		`"events":["incident.resolved"],"secret":""}`
	w = doRequest(t, s, http.MethodPut, "/api/v1/webhooks/"+id, strings.NewReader(empty), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf(`PUT with "secret":"" = %d, want 422: %s`, w.Code, w.Body)
	}
	if got := st.get(t, id).SecretEnc; string(got) != string(original) {
		t.Errorf("secret = %q after a rejected PUT, want it untouched", got)
	}

	// 3. A new secret replaces it, sealed.
	rotated := `{"name":"slack-oncall","url":"https://hooks.example.test/moved",` +
		`"events":["incident.resolved"],"secret":"rotated-key"}`
	w = doRequest(t, s, http.MethodPut, "/api/v1/webhooks/"+id, strings.NewReader(rotated), mutateWithCSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT with a new secret = %d, want 200: %s", w.Code, w.Body)
	}
	if got := string(st.get(t, id).SecretEnc); got != sealedPrefix+"rotated-key)" {
		t.Errorf("secret = %q, want the new value SEALED", got)
	}
	if strings.Contains(w.Body.String(), "rotated-key") {
		t.Errorf("PUT response = %s, must never echo the new secret", w.Body)
	}
}

// Create REQUIRES a secret: every delivery is signed, so an endpoint without
// one could never deliver.
func TestWebhookCreateWithoutSecretIs422(t *testing.T) {
	st := newFakeWebhookStore()
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: &fakeSealer{}})
	body := `{"name":"slack-oncall","url":"https://hooks.example.test/x","events":["incident.created"]}`
	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(body), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST without a secret = %d, want 422: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "secret") {
		t.Errorf("detail = %s, want it to name the missing field", w.Body)
	}
}

// M6 Decision 4: the encryption key is OPTIONAL at boot, so the honest place
// to report it missing is the first request that needs the cipher -- and the
// 503 must NAME the key, not merely say "unavailable".
func TestWebhookCreateAndTestWithoutASealerReturn503NamingTheKey(t *testing.T) {
	st := newFakeWebhookStore()
	id := st.seed("seeded")
	s := newM5TestServer(t, "admin", Deps{Webhooks: st}) // no sealer, no dispatcher

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/webhooks", validWebhookBody},
		{http.MethodPost, "/api/v1/webhooks/" + id + "/test", ""},
	} {
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a sealer = %d, want 503: %s", c.method, c.path, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "console.webhooks.encryptionKey") {
			t.Errorf("%s %s 503 detail = %s, want it to name console.webhooks.encryptionKey",
				c.method, c.path, w.Body)
		}
	}
}

// The other half of Decision 4's trade: a console with NO key can still list,
// read, re-point, disable and delete an endpoint. Gating the whole resource on
// a key would leave an operator unable to silence a misfiring webhook during
// exactly the incident that made them want to.
func TestWebhookRoutesThatNeedNoCipherWorkWithoutASealer(t *testing.T) {
	st := newFakeWebhookStore()
	id := st.seed("slack-oncall")
	s := newM5TestServer(t, "admin", Deps{Webhooks: st})

	body := `{"name":"slack-oncall","url":"https://hooks.example.test/x",` +
		`"events":["incident.created"],"enabled":false}`
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/v1/webhooks", "", http.StatusOK},
		{http.MethodGet, "/api/v1/webhooks/" + id, "", http.StatusOK},
		{http.MethodPut, "/api/v1/webhooks/" + id, body, http.StatusOK},
		{http.MethodDelete, "/api/v1/webhooks/" + id, "", http.StatusNoContent},
	} {
		var mutate func(*http.Request)
		if isMutatingMethod(c.method) {
			mutate = mutateWithCSRF
		}
		w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
		if w.Code != c.want {
			t.Errorf("%s %s with no encryption key = %d, want %d: %s", c.method, c.path, w.Code, c.want, w.Body)
		}
	}
}

func TestWebhookCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"unparseable body", `not json`, http.StatusBadRequest},
		{
			"missing name",
			`{"url":"https://x.test","events":["incident.created"],"secret":"s"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"name with uppercase",
			`{"name":"Slack","url":"https://x.test","events":["incident.created"],"secret":"s"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"non-http url",
			`{"name":"n","url":"file:///etc/passwd","events":["incident.created"],"secret":"s"}`,
			http.StatusUnprocessableEntity,
		},
		{"empty events", `{"name":"n","url":"https://x.test","events":[],"secret":"s"}`, http.StatusUnprocessableEntity},
		{
			"unknown event",
			`{"name":"n","url":"https://x.test","events":["alert.fired"],"secret":"s"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"duplicate event",
			`{"name":"n","url":"https://x.test","events":["incident.created","incident.created"],"secret":"s"}`,
			http.StatusUnprocessableEntity,
		},
	}
	for _, c := range cases {
		st := newFakeWebhookStore()
		s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: &fakeSealer{}})
		w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(c.body), mutateWithCSRF)
		if w.Code != c.want {
			t.Errorf("%s: POST = %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		if w.Code >= http.StatusBadRequest && !strings.Contains(w.Body.String(), "webhook") {
			t.Errorf("%s: detail = %s, want it to name the resource", c.name, w.Body)
		}
	}
}

// A duplicate name is a rejected FIELD VALUE, so 422 -- writeTargetStoreError's
// precedent, not a 409.
func TestWebhookDuplicateNameIs422(t *testing.T) {
	st := newFakeWebhookStore()
	st.seed("slack-oncall")
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: &fakeSealer{}})
	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(validWebhookBody), mutateWithCSRF)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate name = %d, want 422: %s", w.Code, w.Body)
	}
}

func TestWebhookUnknownAndMalformedIDsAreBoth404(t *testing.T) {
	st := newFakeWebhookStore()
	s := newM5TestServer(t, "admin", Deps{
		Webhooks: st, WebhookSealer: &fakeSealer{}, WebhookTestDispatcher: newFakeTestDispatcher(),
	})
	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/webhooks/" + id, ""},
			{http.MethodPut, "/api/v1/webhooks/" + id, validWebhookBody},
			{http.MethodDelete, "/api/v1/webhooks/" + id, ""},
			{http.MethodPost, "/api/v1/webhooks/" + id + "/test", ""},
		} {
			var mutate func(*http.Request)
			if isMutatingMethod(c.method) {
				mutate = mutateWithCSRF
			}
			w := doRequest(t, s, c.method, c.path, strings.NewReader(c.body), mutate)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404: %s", c.method, c.path, w.Code, w.Body)
			}
		}
	}
}

// The /test route must not answer 202 for an endpoint that does not exist: the
// operator would wait forever for an outcome row that can never appear.
func TestWebhookTestOfAnUnknownEndpointNeverEnqueues(t *testing.T) {
	st := newFakeWebhookStore()
	disp := newFakeTestDispatcher()
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookTestDispatcher: disp})
	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks/"+uuid.NewString()+"/test", nil, mutateWithCSRF)
	if w.Code != http.StatusNotFound {
		t.Fatalf("test of an unknown endpoint = %d, want 404: %s", w.Code, w.Body)
	}
	if ids := disp.dispatched(); len(ids) != 0 {
		t.Errorf("dispatcher saw %v, want nothing enqueued", ids)
	}
}

func TestWebhookSealerFailureIs502AndNeverEchoesTheCipherError(t *testing.T) {
	st := newFakeWebhookStore()
	s := newM5TestServer(t, "admin", Deps{
		Webhooks: st, WebhookSealer: &fakeSealer{err: errors.New("key must be 32 bytes, got 7")},
	})
	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks", strings.NewReader(validWebhookBody), mutateWithCSRF)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "32 bytes") {
		t.Errorf("body = %s, must never echo the cipher error -- it is a hint about the key", w.Body)
	}
}

func TestWebhookStoreFailureReturns502(t *testing.T) {
	st := newFakeWebhookStore()
	st.listErr = errors.New("pool exhausted")
	s := newM5TestServer(t, "admin", Deps{Webhooks: st})
	w := doRequest(t, s, http.MethodGet, "/api/v1/webhooks", nil, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "pool exhausted") {
		t.Errorf("body = %s, must never echo the driver error", w.Body)
	}
}

// M6's global constraint at the audit layer: NEITHER the secret NOR the url
// may reach an audit row, on create or on update. A hook URL routinely embeds
// a token in its own path, which is why the ban is on the whole field.
func TestWebhookAuditDetailNeverCarriesSecretOrURL(t *testing.T) {
	for _, c := range []struct{ method, path, action string }{
		{http.MethodPost, "/api/v1/webhooks", "POST /api/v1/webhooks"},
		{http.MethodPut, "", "PUT /api/v1/webhooks/{id}"},
	} {
		fs := &fakeAuditStore{}
		st := newFakeWebhookStore()
		id := st.seed("existing")
		s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookSealer: &fakeSealer{}, Audit: fs})

		path := c.path
		if path == "" {
			path = "/api/v1/webhooks/" + id
		}
		body := `{"name":"slack-oncall","url":"https://hooks.example.test/T0/B0/xoxb-secret-in-the-path",` +
			`"events":["incident.created"],"secret":"` + testWebhookSecret + `"}`
		w := doRequest(t, s, c.method, path, strings.NewReader(body), mutateWithCSRF)
		if w.Code >= http.StatusBadRequest {
			t.Fatalf("%s %s status %d: %s", c.method, path, w.Code, w.Body)
		}

		entries := waitForOneAuditEntry(t, fs)
		detail := string(entries[0].Detail)
		for _, banned := range []string{
			"secret", testWebhookSecret, "url", "hooks.example.test", "xoxb", sealedPrefix,
		} {
			if strings.Contains(detail, banned) {
				t.Errorf("%s audit detail = %s, must never carry %q", c.action, detail, banned)
			}
		}
		if !strings.Contains(detail, `"name":"slack-oncall"`) || !strings.Contains(detail, "incident.created") {
			t.Errorf("%s audit detail = %s, want the allow-listed name and events", c.action, detail)
		}
		if entries[0].Action != c.action {
			t.Errorf("audit action = %q, want %q", entries[0].Action, c.action)
		}
	}
}

// The test ping carries no body, so its detail is the default-deny {} -- the
// endpoint it names is already in the row's resource column.
func TestWebhookTestIsAuditedWithEmptyDetail(t *testing.T) {
	fs := &fakeAuditStore{}
	st := newFakeWebhookStore()
	id := st.seed("slack-oncall")
	s := newM5TestServer(t, "admin", Deps{Webhooks: st, WebhookTestDispatcher: newFakeTestDispatcher(), Audit: fs})

	w := doRequest(t, s, http.MethodPost, "/api/v1/webhooks/"+id+"/test", nil, mutateWithCSRF)
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST test status %d: %s", w.Code, w.Body)
	}
	entries := waitForOneAuditEntry(t, fs)
	if got := string(entries[0].Detail); got != "{}" {
		t.Errorf("audit detail = %s, want {}", got)
	}
	if entries[0].Resource != id {
		t.Errorf("audit resource = %q, want the webhook id %q", entries[0].Resource, id)
	}
	if entries[0].Action != "POST /api/v1/webhooks/{id}/test" {
		t.Errorf("audit action = %q, want the route pattern", entries[0].Action)
	}
}
