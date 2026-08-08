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

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// WebhookService is the subset of *store.DB httpapi needs for
// /api/v1/webhooks: the read seam and the write seam together, in
// TargetService's shape.
type WebhookService interface {
	store.WebhookReader
	store.WebhookStore
}

var _ WebhookService = (*store.DB)(nil)

// SecretSealer turns a plaintext endpoint secret into the ciphertext
// store.WebhookInput.SecretEnc carries. It is the reason THIS package never
// encrypts anything: the AES-GCM key is config (`console.webhooks.encryptionKey`,
// M6 Decision 4) and lives with the code that also has to DECRYPT at delivery
// time -- the dispatcher (M6 Task 5). Splitting the cipher across two packages
// would give one key two implementations to disagree about.
//
// Narrow on purpose: Seal is the only direction the HTTP layer is allowed. A
// handler that could unseal could serve a secret back, and the whole point of
// the write-only field is that nothing can.
type SecretSealer interface {
	Seal(plain []byte) ([]byte, error)
}

// TestDispatcher enqueues one signed ping to an already-stored endpoint,
// addressed by ID ALONE. It never takes a secret, plaintext or sealed: the
// dispatcher reads the row it is going to deliver to, so the ciphertext never
// travels through this package, and the /test route cannot be turned into an
// oracle that confirms a secret a caller supplied.
//
// Enqueue, not deliver: the error is about accepting the work, not about what
// the endpoint answered. A 202 here means "queued"; the outcome lands on the
// endpoint row (lastStatus/lastAttempt/failures), which is what GET returns.
type TestDispatcher interface {
	DispatchTest(ctx context.Context, id string) error
}

// webhooksUnavailableDetail is served whenever s.webhooks is nil: endpoints
// are persisted CONFIGURATION and get no in-memory fallback, targetsUnavailableDetail's
// rule.
const webhooksUnavailableDetail = "webhook endpoints are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/webhooks"

// webhookKeyUnavailableDetail is served for the two operations that need the
// cipher -- creating an endpoint (its secret must be sealed) and testing one
// (its secret must be unsealed to sign the ping). M6 Decision 4 makes the key
// OPTIONAL at boot: a console that never configures a webhook must not fail to
// start over a key it will never use, so the honest place to report the
// missing key is the first request that actually needs it, naming it.
const webhookKeyUnavailableDetail = "webhook secrets are encrypted at rest and this console has no encryption " +
	"key configured: set console.webhooks.encryptionKey (32 bytes, base64, from a Secret) to create or " +
	"test webhook endpoints"

// webhookValidationPrefix is the prefix store.WebhookInput.Validate builds
// every one of its errors with.
const webhookValidationPrefix = "store: webhook: "

// webhooksUnavailable answers 503 and reports true when no WebhookService is
// wired.
func (s *Server) webhooksUnavailable(w http.ResponseWriter) bool {
	if s.webhooks == nil {
		writeProblem(w, http.StatusServiceUnavailable, "webhooks not available", webhooksUnavailableDetail)
		return true
	}
	return false
}

// webhookResponse is one endpoint on the wire. There is NO secret field, and
// that is the type-level half of the guarantee: HasSecret is the only thing a
// reader ever learns about it, and it is always true for a stored row (the
// store refuses an empty one), so it is a contract statement rather than a
// query -- "this endpoint signs its deliveries".
//
// The URL is returned, unlike the secret: it is what the operator typed and
// what the UI has to show to be usable at all. It stays out of the AUDIT log
// for the reason a target's address does (audit.go) -- a log read by more
// people, retained longer, must not carry a value that names internal
// infrastructure.
type webhookResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	HasSecret bool     `json:"hasSecret"`
	// The last delivery outcome, kept on the endpoint row rather than in a
	// delivery-log table (M6 Decision 5). LastAttempt is omitted when nothing
	// has ever been delivered.
	LastStatus  string     `json:"lastStatus"`
	LastAttempt *time.Time `json:"lastAttempt,omitempty"`
	Failures    int32      `json:"failures"`
	CreatedAt   time.Time  `json:"createdAt"`
}

func webhookResponseFrom(h *store.Webhook) webhookResponse {
	events := h.Events
	if events == nil {
		// Defensive: the column is NOT NULL and validation refuses an empty
		// list, but a nil here would marshal as JSON null and the frontend
		// iterates events.
		events = []string{}
	}
	return webhookResponse{
		ID: h.ID, Name: h.Name, URL: h.URL, Events: events, Enabled: h.Enabled,
		HasSecret:   len(h.SecretEnc) > 0,
		LastStatus:  h.LastStatus,
		LastAttempt: h.LastAttempt,
		Failures:    h.Failures,
		CreatedAt:   h.CreatedAt,
	}
}

// webhooksListResponse is GET /api/v1/webhooks's body. UNPAGED, and therefore
// carrying no nextCursor: store.ListWebhooks is unpaged for
// ListMTRDestinations' reason -- the row count is endpoints an operator typed,
// not a function of time.
type webhooksListResponse struct {
	Webhooks []webhookResponse `json:"webhooks"`
}

// webhookRequest is POST /api/v1/webhooks's and PUT /api/v1/webhooks/{id}'s
// body. Both are FULL replaces, matching store.WebhookInput's contract -- with
// exactly ONE field that is not: Secret.
//
// Secret is WRITE-ONLY and three-state, which is why it is a pointer:
//   - absent (nil): on create, 422 (every delivery is signed, so there is no
//     such thing as an endpoint without a secret); on update, KEEP the stored
//     one. This is what makes editing a URL or an event filter possible
//     without the operator having to re-type a secret they may not have.
//   - present and empty (""): 422 on BOTH. "" is what a form submits for a
//     field the operator left blank, and reading that as "keep" would be a
//     guess; reading it as "clear" would leave an endpoint that cannot sign.
//     Refusing it is the only answer that is not a guess.
//   - present and non-empty: sealed and stored, replacing whatever was there.
//
// Enabled is a pointer for a smaller reason: an omitted enabled means TRUE
// (an endpoint you just declared is one you want), not Go's false zero value.
type webhookRequest struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Secret  *string  `json:"secret"`
	Enabled *bool    `json:"enabled"`
}

// webhookIDFrom resolves the {id} path parameter, answering 404 and reporting
// false for anything that is not a canonical UUID -- targetIDFrom's reasoning.
func webhookIDFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusNotFound, "webhook not found", "no webhook with that id")
		return "", false
	}
	return id, true
}

// writeWebhookStoreError maps a WebhookService error to a response.
// ErrAlreadyExists is 422 rather than 409, for writeTargetStoreError's reason:
// a duplicate name is a rejected FIELD VALUE in an otherwise well-formed body,
// and the caller's fix is to change the name and resend.
func writeWebhookStoreError(w http.ResponseWriter, name, id string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "webhook not found", "no webhook with that id")
	case errors.Is(err, store.ErrAlreadyExists):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook",
			"webhook: name "+strconv.Quote(name)+" is already taken; webhook names are unique")
	case strings.HasPrefix(err.Error(), webhookValidationPrefix):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook", publicValidationDetail(err))
	default:
		slog.Error("httpapi: webhook store call failed", "webhook", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "webhooks unavailable", "failed to reach the webhooks store")
	}
}

// decodeWebhookRequest reads a create/update body. A body that is not JSON at
// all is a 400; a well-formed body whose VALUES break a rule is a 422 --
// decodeTargetRequest's distinction. The secret's own three-state rule is
// applied by the callers, which are the only ones that know whether there is
// an existing ciphertext to keep.
func decodeWebhookRequest(w http.ResponseWriter, r *http.Request) (webhookRequest, bool) {
	var req webhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			`a webhook body must be JSON with "name", "url" (http/https), "events" `+
				`(a subset of incident.created, incident.resolved, incident.reopened), `+
				`an optional "enabled", and a write-only "secret"`)
		return webhookRequest{}, false
	}
	if req.Secret != nil && *req.Secret == "" {
		// The one rule that cannot be delegated to store, because store never
		// sees a plaintext secret at all.
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook",
			`webhook: "secret" must not be empty -- omit the field entirely to keep the stored secret `+
				`(on update), or send a non-empty one to replace it`)
		return webhookRequest{}, false
	}
	return req, true
}

// sealWebhookSecret encrypts plain through the configured SecretSealer. A nil
// sealer is 503 naming the key, NOT 500: the deployment is missing a
// configuration value, which is an operator action, and Decision 4 made the
// key optional precisely so this is the first place it can possibly be
// noticed.
func (s *Server) sealWebhookSecret(w http.ResponseWriter, plain string) ([]byte, bool) {
	if s.webhookSealer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "webhook encryption key not configured",
			webhookKeyUnavailableDetail)
		return nil, false
	}
	sealed, err := s.webhookSealer.Seal([]byte(plain))
	if err != nil {
		// The error can only be about the KEY (wrong length, unusable cipher),
		// never about the plaintext -- and it must not reach the wire, because
		// a cipher error message is a hint about the key.
		slog.Error("seal webhook secret failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "webhooks unavailable", "failed to encrypt the webhook secret")
		return nil, false
	}
	return sealed, true
}

// enabledOrDefault reads an omitted "enabled" as true: an endpoint an operator
// just declared is one they want firing.
func enabledOrDefault(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// handleWebhooksList serves every configured endpoint, newest first, with NO
// secret anywhere in the body (webhookResponse has no field for one).
func (s *Server) handleWebhooksList(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	hooks, err := s.webhooks.ListWebhooks(r.Context())
	if err != nil {
		slog.Error("list webhooks failed", "error", err)
		writeProblem(w, http.StatusBadGateway, "webhooks unavailable", "failed to query webhooks")
		return
	}
	out := make([]webhookResponse, 0, len(hooks))
	for i := range hooks {
		out = append(out, webhookResponseFrom(&hooks[i]))
	}
	writeJSON(w, webhooksListResponse{Webhooks: out})
}

// handleWebhooksCreate declares one endpoint: 201 with a Location header.
// The secret is REQUIRED here -- every delivery is signed, so an endpoint
// without one could never deliver -- and is sealed before it reaches store,
// which is typed to accept ciphertext only.
func (s *Server) handleWebhooksCreate(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	req, ok := decodeWebhookRequest(w, r)
	if !ok {
		return
	}
	if req.Secret == nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook",
			`webhook: "secret" is required when creating an endpoint: every delivery is signed with it`)
		return
	}
	sealed, ok := s.sealWebhookSecret(w, *req.Secret)
	if !ok {
		return
	}

	in := store.WebhookInput{
		Name: req.Name, URL: req.URL, Events: req.Events,
		SecretEnc: sealed, Enabled: enabledOrDefault(req.Enabled),
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook", publicValidationDetail(err))
		return
	}

	hook, err := s.webhooks.CreateWebhook(r.Context(), in)
	if err != nil {
		writeWebhookStoreError(w, in.Name, "", err)
		return
	}
	w.Header().Set("Location", "/api/v1/webhooks/"+hook.ID)
	writeJSONStatus(w, http.StatusCreated, webhookResponseFrom(&hook))
}

// handleWebhooksGet serves one endpoint -- again with no secret in the body,
// only hasSecret and the last delivery outcome.
func (s *Server) handleWebhooksGet(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	id, ok := webhookIDFrom(w, r)
	if !ok {
		return
	}
	hook, err := s.webhooks.GetWebhook(r.Context(), id)
	if err != nil {
		writeWebhookStoreError(w, "", id, err)
		return
	}
	writeJSON(w, webhookResponseFrom(&hook))
}

// handleWebhooksUpdate replaces one endpoint in full -- EXCEPT the secret,
// whose absence means "keep the stored one" (see webhookRequest).
//
// Note what that implies and what it does not: keeping a secret needs no
// encryption key, so an update that does not carry one succeeds on a console
// with no console.webhooks.encryptionKey configured. Only an update that
// actually REPLACES the secret needs the cipher, and only that one answers 503
// naming the key. The alternative -- gating the whole route on a key it may
// not need -- would make an operator unable to disable a misfiring endpoint
// during exactly the incident that made them want to.
func (s *Server) handleWebhooksUpdate(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	id, ok := webhookIDFrom(w, r)
	if !ok {
		return
	}
	req, ok := decodeWebhookRequest(w, r)
	if !ok {
		return
	}

	var sealed []byte
	if req.Secret != nil {
		if sealed, ok = s.sealWebhookSecret(w, *req.Secret); !ok {
			return
		}
	} else {
		// Read the stored ciphertext back so the full-replace UPDATE writes it
		// unchanged. This also turns an unknown id into a 404 before anything
		// is written, which is the same ordering handleIncidentsUpdate uses.
		current, err := s.webhooks.GetWebhook(r.Context(), id)
		if err != nil {
			writeWebhookStoreError(w, "", id, err)
			return
		}
		sealed = current.SecretEnc
	}

	in := store.WebhookInput{
		Name: req.Name, URL: req.URL, Events: req.Events,
		SecretEnc: sealed, Enabled: enabledOrDefault(req.Enabled),
	}
	if err := in.Validate(); err != nil {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid webhook", publicValidationDetail(err))
		return
	}

	hook, err := s.webhooks.UpdateWebhook(r.Context(), id, in)
	if err != nil {
		writeWebhookStoreError(w, in.Name, id, err)
		return
	}
	writeJSON(w, webhookResponseFrom(&hook))
}

// handleWebhooksDelete removes one endpoint. Deleting one that is not there is
// 404, not success.
func (s *Server) handleWebhooksDelete(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	id, ok := webhookIDFrom(w, r)
	if !ok {
		return
	}
	if err := s.webhooks.DeleteWebhook(r.Context(), id); err != nil {
		writeWebhookStoreError(w, "", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWebhooksTest enqueues one signed ping so an operator can find out
// whether an endpoint works BEFORE an incident depends on it.
//
// 202, not 200: delivery is asynchronous with a retry ladder (M6 Decision 5),
// so the only honest thing this can report is that the work was accepted. The
// OUTCOME arrives on the endpoint row and is read back from GET
// /api/v1/webhooks/{id} -- lastStatus/lastAttempt/failures.
//
// A nil dispatcher answers 503 with the SAME detail a missing key gets, and on
// purpose: the dispatcher is exactly the component the key configures (M6 Task
// 5 wires them together), so naming a second knob here would send the operator
// looking for something that does not exist.
func (s *Server) handleWebhooksTest(w http.ResponseWriter, r *http.Request) {
	if s.webhooksUnavailable(w) {
		return
	}
	id, ok := webhookIDFrom(w, r)
	if !ok {
		return
	}
	if s.webhookTester == nil {
		writeProblem(w, http.StatusServiceUnavailable, "webhook delivery not configured", webhookKeyUnavailableDetail)
		return
	}
	// Existence is checked HERE rather than left to the dispatcher: enqueuing
	// a ping to an id that names nothing would answer 202 for work that can
	// never happen, and the operator would wait for an outcome row that never
	// appears.
	if _, err := s.webhooks.GetWebhook(r.Context(), id); err != nil {
		writeWebhookStoreError(w, "", id, err)
		return
	}
	if err := s.webhookTester.DispatchTest(r.Context(), id); err != nil {
		slog.Error("enqueue webhook test failed", "webhook", id, "error", err) //nolint:gosec // G706: structured slog fields, not string-built log injection
		writeProblem(w, http.StatusBadGateway, "webhooks unavailable", "failed to enqueue the test delivery")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
