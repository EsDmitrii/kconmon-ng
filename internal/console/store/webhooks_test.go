package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// validWebhookInput is the baseline every WebhookInput case below mutates one
// field of, so a failure names exactly the field under test.
func validWebhookInput() WebhookInput {
	return WebhookInput{
		Name:      "ops-slack",
		URL:       "https://hooks.example.test/services/abc",
		Events:    []string{WebhookEventIncidentCreated, WebhookEventIncidentResolved},
		SecretEnc: []byte{0x9e, 0x00, 0x01, 0xff, 0x7a},
		Enabled:   true,
	}
}

func TestWebhookInputValidateAcceptsWellFormed(t *testing.T) {
	cases := []struct {
		name string
		in   WebhookInput
	}{
		{"two events", validWebhookInput()},
		{"one event", func() WebhookInput {
			in := validWebhookInput()
			in.Events = []string{WebhookEventIncidentReopened}
			return in
		}()},
		{"all three events", func() WebhookInput {
			in := validWebhookInput()
			in.Events = []string{
				WebhookEventIncidentCreated,
				WebhookEventIncidentResolved,
				WebhookEventIncidentReopened,
			}
			return in
		}()},
		{"http url", func() WebhookInput { in := validWebhookInput(); in.URL = "http://receiver.svc:8080/h"; return in }()},
		{"disabled", func() WebhookInput { in := validWebhookInput(); in.Enabled = false; return in }()},
		{"digits and hyphens in the name", func() WebhookInput {
			in := validWebhookInput()
			in.Name = "team-9-alerts"
			return in
		}()},
		{"max name", func() WebhookInput {
			in := validWebhookInput()
			in.Name = strings.Repeat("n", webhookNameMaxLen)
			return in
		}()},
		{"max secret", func() WebhookInput {
			in := validWebhookInput()
			in.SecretEnc = make([]byte, webhookSecretMaxLen)
			return in
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if err := in.Validate(); err != nil {
				t.Errorf("Validate(%+v) = %v, want nil", tc.in, err)
			}
		})
	}
}

func TestWebhookInputValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*WebhookInput)
	}{
		{"empty name", func(in *WebhookInput) { in.Name = "" }},
		{"name over 64 bytes", func(in *WebhookInput) { in.Name = strings.Repeat("n", webhookNameMaxLen+1) }},
		{"uppercase name", func(in *WebhookInput) { in.Name = "Ops-Slack" }},
		{"underscore in the name", func(in *WebhookInput) { in.Name = "ops_slack" }},
		{"dot in the name", func(in *WebhookInput) { in.Name = "ops.slack" }},
		{"space in the name", func(in *WebhookInput) { in.Name = "ops slack" }},
		{"empty url", func(in *WebhookInput) { in.URL = "" }},
		{"scheme-less url", func(in *WebhookInput) { in.URL = "hooks.example.test/x" }},
		{"file url", func(in *WebhookInput) { in.URL = "file:///etc/passwd" }},
		{"gopher url", func(in *WebhookInput) { in.URL = "gopher://example.test/1" }},
		{"url over 2048 bytes", func(in *WebhookInput) {
			in.URL = "https://example.test/" + strings.Repeat("p", webhookURLMaxLen)
		}},
		{"nil events", func(in *WebhookInput) { in.Events = nil }},
		{"empty events", func(in *WebhookInput) { in.Events = []string{} }},
		{"unknown event", func(in *WebhookInput) { in.Events = []string{"alert.fired"} }},
		{"one unknown among known", func(in *WebhookInput) {
			in.Events = []string{WebhookEventIncidentCreated, "incident.escalated"}
		}},
		{"duplicate event", func(in *WebhookInput) {
			in.Events = []string{WebhookEventIncidentCreated, WebhookEventIncidentCreated}
		}},
		{"nil secret", func(in *WebhookInput) { in.SecretEnc = nil }},
		{"empty secret", func(in *WebhookInput) { in.SecretEnc = []byte{} }},
		{"secret over 4096 bytes", func(in *WebhookInput) {
			in.SecretEnc = make([]byte, webhookSecretMaxLen+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validWebhookInput()
			tc.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Errorf("Validate(%+v) = nil, want an error", in)
			}
		})
	}
}

// TestWebhookEventsAreTheClosedM6Set pins the VALUES, not just the count. M6
// fires on incident lifecycle only (Decision 5); alert-fired webhooks belong
// to M7. The literal strings here are deliberately NOT the constants -- a test
// that compares a constant to itself pins nothing -- so a typo in one of the
// exported names fails here rather than silently shipping a wire value no
// receiver filters on.
func TestWebhookEventsAreTheClosedM6Set(t *testing.T) {
	want := map[string]bool{
		"incident.created":  true,
		"incident.resolved": true,
		"incident.reopened": true,
	}
	if len(webhookEvents) != len(want) {
		t.Fatalf("webhookEvents has %d entries, want %d: %v", len(webhookEvents), len(want), webhookEvents)
	}
	for ev := range want {
		if !webhookEvents[ev] {
			t.Errorf("webhookEvents is missing %q", ev)
		}
	}
	for _, got := range []string{
		WebhookEventIncidentCreated,
		WebhookEventIncidentResolved,
		WebhookEventIncidentReopened,
	} {
		if !want[got] {
			t.Errorf("exported constant %q is not one of the three M6 events", got)
		}
	}
}

// TestWebhookSecretIsOpaqueToValidation is the layering claim migration 00006
// and WebhookInput.SecretEnc both make: the store persists bytes and does not
// know, or check, what they encrypt. Arbitrary binary -- NUL bytes, invalid
// UTF-8, a lone 0xff -- must round through Validate untouched, because
// AES-GCM ciphertext is exactly that and a store that "helpfully" rejected it
// would make the dispatcher's own crypto unstorable.
func TestWebhookSecretIsOpaqueToValidation(t *testing.T) {
	for _, secret := range [][]byte{
		{0x00},
		{0xff, 0xfe, 0xfd},
		{0x00, 0x00, 0x00, 0x00},
		[]byte("\x80\x81not-utf8"),
	} {
		in := validWebhookInput()
		in.SecretEnc = secret
		if err := in.Validate(); err != nil {
			t.Errorf("Validate rejected opaque ciphertext %v: %v", secret, err)
		}
	}
}

// TestWebhookMalformedIDIsNotFoundWithoutTouchingPgx mirrors the annotation
// readers' pre-check across every id-taking method. NIL pool, so a clean
// return proves no round trip was attempted.
func TestWebhookMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run(id, func(t *testing.T) {
			if _, err := db.GetWebhook(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetWebhook(%q) err = %v, want ErrNotFound", id, err)
			}
			if err := db.DeleteWebhook(ctx, id); !errors.Is(err, ErrNotFound) {
				t.Errorf("DeleteWebhook(%q) err = %v, want ErrNotFound", id, err)
			}
			if _, err := db.UpdateWebhook(ctx, id, validWebhookInput()); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateWebhook(%q) err = %v, want ErrNotFound", id, err)
			}
			if err := db.UpdateWebhookDelivery(ctx, id, "200", time.Now(), 0); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateWebhookDelivery(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// TestUpdateWebhookValidatesBeforeTheIDPreCheck asserts a bad payload is
// reported as a validation error rather than as a miss: an operator who typed
// a file:// URL must be told that, not told the endpoint does not exist. NIL
// pool.
func TestUpdateWebhookValidatesBeforeTheIDPreCheck(t *testing.T) {
	db := &DB{}
	in := validWebhookInput()
	in.URL = "file:///etc/passwd"

	_, err := db.UpdateWebhook(context.Background(), "not-a-uuid", in)
	if err == nil {
		t.Fatal("UpdateWebhook(bad url) = nil, want a validation error")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateWebhook reported %v, want the url error rather than a miss", err)
	}
}

// TestUpdateWebhookDeliveryBoundsItsInputs asserts the two delivery-outcome
// bounds are applied before the id pre-check, for the same reason. NIL pool.
func TestUpdateWebhookDeliveryBoundsItsInputs(t *testing.T) {
	db := &DB{}
	ctx := context.Background()
	id := "3f1d1a2f-6f8e-4a3a-9a0e-7f3f9d0f1c22"

	err := db.UpdateWebhookDelivery(ctx, id, strings.Repeat("s", webhookLastStatusMaxLen+1), time.Now(), 0)
	if err == nil {
		t.Error("UpdateWebhookDelivery(over-long status) = nil, want the bound to reject it")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateWebhookDelivery reported %v, want a length error rather than a miss", err)
	}

	err = db.UpdateWebhookDelivery(ctx, id, "200", time.Now(), -1)
	if err == nil {
		t.Error("UpdateWebhookDelivery(failures=-1) = nil, want a validation error")
	}
}

// TestCreateWebhookValidatesBeforeTouchingPgx asserts validation runs before
// the INSERT. NIL pool, so a clean return proves it.
func TestCreateWebhookValidatesBeforeTouchingPgx(t *testing.T) {
	db := &DB{}
	if _, err := db.CreateWebhook(context.Background(), WebhookInput{}); err == nil {
		t.Error("CreateWebhook(zero input) = nil error, want a validation error")
	}
}
