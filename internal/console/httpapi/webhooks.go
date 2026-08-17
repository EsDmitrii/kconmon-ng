package httpapi

import (
	"context"
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

// SecretSealer turns a plaintext endpoint secret into the ciphertext store.WebhookInput.SecretEnc
// carries; it is the reason THIS package never encrypts anything.
type SecretSealer interface {
	Seal(plain []byte) ([]byte, error)
}

// TestDispatcher enqueues one signed ping to an already-stored endpoint, addressed by ID ALONE; it
// never takes a secret, plaintext or sealed.
type TestDispatcher interface {
	DispatchTest(ctx context.Context, id string) error
}

// webhooksUnavailableDetail is served whenever s.webhooks is nil: endpoints
// are persisted CONFIGURATION and get no in-memory fallback, targetsUnavailableDetail's
// rule.
const webhooksUnavailableDetail = "webhook endpoints are persisted configuration with no in-memory fallback: " +
	"set console.database.mode in the console config (Helm: console.database.mode) to enable /api/v1/webhooks"

// webhookKeyUnavailableDetail is served for the two operations that need the cipher.
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

// webhookResponse is one endpoint on the wire; there is NO secret field, and that is the type-level
// half of the guarantee.
type webhookResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	HasSecret bool     `json:"hasSecret"`
	// The last delivery outcome, kept on the endpoint row rather than in a delivery-log table.
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

// webhooksListResponse is GET /api/v1/webhooks's body.
type webhooksListResponse struct {
	Webhooks []webhookResponse `json:"webhooks"`
}

// webhookRequest is POST /api/v1/webhooks's and PUT /api/v1/webhooks/{id}'s body; secret is
// WRITE-ONLY and three-state, which is why it is a pointer: - absent (nil).
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

// writeWebhookStoreError maps a WebhookService error to a response; ErrAlreadyExists is 422 rather
// than 409, for writeTargetStoreError's reason.
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

// decodeWebhookRequest reads a create/update body; the secret's own three-state rule is applied by
// the callers.
func decodeWebhookRequest(w http.ResponseWriter, r *http.Request) (webhookRequest, bool) {
	var req webhookRequest
	if err := strictJSONDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", unknownFieldDetail(err,
			`a webhook body must be JSON with "name", "url" (http/https), "events" `+
				`(a subset of incident.created, incident.resolved, incident.reopened), `+
				`an optional "enabled", and a write-only "secret"`))
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

// sealWebhookSecret encrypts plain through the configured SecretSealer.
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

// handleWebhooksCreate declares one endpoint: 201 with a Location header; the secret is REQUIRED
// here -- every delivery is signed.
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

// handleWebhooksUpdate replaces one endpoint in full -- EXCEPT the secret; only an update that
// actually REPLACES the secret needs the cipher.
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

// handleWebhooksTest enqueues one signed ping so an operator can find out whether an endpoint works
// BEFORE an incident depends on it; a nil dispatcher answers 503 with the SAME detail a missing key
// gets.
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
	// Existence is checked HERE rather than left to the dispatcher.
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
