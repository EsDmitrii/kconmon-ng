//go:build integration

package store_test

// TestWebhook* require a real PostgreSQL.
// Run: docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=test -e POSTGRES_DB=kconmon postgres:17-alpine
// Then: KCONMON_TEST_DATABASE_DSN='postgres://postgres:test@127.0.0.1:5432/kconmon?sslmode=disable' \
//       go test -tags=integration ./internal/console/store/... -v

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/EsDmitrii/kconmon-ng/internal/console/store"
)

// newWebhooksDB opens a *store.DB with migrations applied, dropping and
// re-creating the schema first -- same convention as newAnnotationsDB.
func newWebhooksDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := testDSN(t)
	dropSchema(t, dsn)
	t.Cleanup(func() { dropSchema(t, dsn) })

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := store.Open(ctx, dsn, 5, connectTimeout, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// ciphertext is a stand-in for what the dispatcher's AES-GCM actually hands
// the store: arbitrary bytes including NULs and invalid UTF-8. The store must
// round-trip them byte-for-byte without ever looking inside.
func ciphertext(tag byte) []byte {
	return []byte{0x00, 0xff, 0x80, tag, 0x00, 0x7f, 0xfe}
}

func webhookInput(name string) store.WebhookInput {
	return store.WebhookInput{
		Name:      name,
		URL:       "https://hooks.example.test/services/" + name,
		Events:    []string{store.WebhookEventIncidentCreated, store.WebhookEventIncidentResolved},
		SecretEnc: ciphertext(0x01),
		Enabled:   true,
	}
}

// TestWebhookLifecycle is the whole CRUD: create -> get -> list -> update ->
// delete, with the delete asserted through an independent read.
func TestWebhookLifecycle(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()

	created, err := db.CreateWebhook(ctx, webhookInput("ops-slack"))
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateWebhook: ID is empty, want a minted UUID")
	}
	if created.Name != "ops-slack" || created.URL != "https://hooks.example.test/services/ops-slack" {
		t.Errorf("CreateWebhook: got %+v, want the input's fields back", created)
	}
	if len(created.Events) != 2 ||
		created.Events[0] != store.WebhookEventIncidentCreated ||
		created.Events[1] != store.WebhookEventIncidentResolved {
		t.Errorf("CreateWebhook: Events = %v, want the two in order", created.Events)
	}
	if !bytes.Equal(created.SecretEnc, ciphertext(0x01)) {
		t.Errorf("CreateWebhook: SecretEnc = %v, want the exact bytes back", created.SecretEnc)
	}
	if !created.Enabled {
		t.Error("CreateWebhook: Enabled = false, want true")
	}
	if created.LastStatus != "" || created.LastAttempt != nil || created.Failures != 0 {
		t.Errorf("CreateWebhook: delivery outcome = (%q, %v, %d), want the untried zero values",
			created.LastStatus, created.LastAttempt, created.Failures)
	}
	if created.CreatedAt.IsZero() {
		t.Error("CreateWebhook: CreatedAt is zero, want the column default")
	}

	got, err := db.GetWebhook(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("GetWebhook = %+v, want %+v", got, created)
	}

	hooks, err := db.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(hooks) != 1 || hooks[0].ID != created.ID {
		t.Fatalf("ListWebhooks = %+v, want the one endpoint", hooks)
	}

	in := webhookInput("ops-slack")
	in.URL = "https://hooks.example.test/v2/ops-slack"
	in.Events = []string{store.WebhookEventIncidentReopened}
	in.SecretEnc = ciphertext(0x02)
	in.Enabled = false
	updated, err := db.UpdateWebhook(ctx, created.ID, in)
	if err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}
	if updated.URL != in.URL || updated.Enabled {
		t.Errorf("UpdateWebhook: got %+v, want the new url and enabled=false", updated)
	}
	if len(updated.Events) != 1 || updated.Events[0] != store.WebhookEventIncidentReopened {
		t.Errorf("UpdateWebhook: Events = %v, want the replaced single event", updated.Events)
	}
	if !bytes.Equal(updated.SecretEnc, ciphertext(0x02)) {
		t.Errorf("UpdateWebhook: SecretEnc = %v, want the rotated bytes", updated.SecretEnc)
	}

	if err := db.DeleteWebhook(ctx, created.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if _, err := db.GetWebhook(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWebhook after the delete = %v, want ErrNotFound", err)
	}
	if err := db.DeleteWebhook(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second DeleteWebhook = %v, want ErrNotFound", err)
	}
}

// TestWebhookSecretRoundTripsByteForByte is the layering claim against real
// BYTEA: AES-GCM ciphertext contains NULs and invalid UTF-8, and a store that
// mangled either -- by round-tripping through a string, say -- would make
// every delivery signature fail with no visible cause.
func TestWebhookSecretRoundTripsByteForByte(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()

	secret := make([]byte, 256)
	for i := range secret {
		secret[i] = byte(i)
	}
	in := webhookInput("byte-fidelity")
	in.SecretEnc = secret

	created, err := db.CreateWebhook(ctx, in)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	got, err := db.GetWebhook(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if !bytes.Equal(got.SecretEnc, secret) {
		t.Fatalf("SecretEnc round trip lost bytes: got %d bytes, want the 256 written", len(got.SecretEnc))
	}
}

// TestCreateWebhookDuplicateNameIsAlreadyExists pins the UNIQUE(name)
// constraint's Go answer: the edge needs a 409, not a 500.
func TestCreateWebhookDuplicateNameIsAlreadyExists(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()

	if _, err := db.CreateWebhook(ctx, webhookInput("ops-slack")); err != nil {
		t.Fatalf("first CreateWebhook: %v", err)
	}
	if _, err := db.CreateWebhook(ctx, webhookInput("ops-slack")); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("second CreateWebhook(same name) = %v, want ErrAlreadyExists", err)
	}

	hooks, err := db.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(hooks) != 1 {
		t.Errorf("the rejected create left %d endpoints, want 1", len(hooks))
	}
}

// TestUpdateWebhookDeliveryLeavesConfigurationAlone is the split the two write
// paths exist to keep: the dispatcher writes outcomes, an operator writes
// configuration, and neither overwrites the other's half. In particular an
// operator fixing a URL typo must not reset the endpoint's failure history.
func TestUpdateWebhookDeliveryLeavesConfigurationAlone(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()

	created, err := db.CreateWebhook(ctx, webhookInput("ops-slack"))
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	attempt := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.UpdateWebhookDelivery(ctx, created.ID, "502 Bad Gateway", attempt, 3); err != nil {
		t.Fatalf("UpdateWebhookDelivery: %v", err)
	}

	got, err := db.GetWebhook(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.LastStatus != "502 Bad Gateway" || got.Failures != 3 {
		t.Errorf("delivery outcome = (%q, %d), want (\"502 Bad Gateway\", 3)", got.LastStatus, got.Failures)
	}
	if got.LastAttempt == nil || !got.LastAttempt.Equal(attempt) {
		t.Errorf("LastAttempt = %v, want %v", got.LastAttempt, attempt)
	}
	// The dispatcher's write did not touch configuration.
	if got.Name != created.Name || got.URL != created.URL || !bytes.Equal(got.SecretEnc, created.SecretEnc) {
		t.Errorf("UpdateWebhookDelivery changed configuration: %+v vs %+v", got, created)
	}

	// And an operator's edit does not reset the failure history.
	in := webhookInput("ops-slack")
	in.URL = "https://hooks.example.test/fixed"
	updated, err := db.UpdateWebhook(ctx, created.ID, in)
	if err != nil {
		t.Fatalf("UpdateWebhook: %v", err)
	}
	if updated.Failures != 3 || updated.LastStatus != "502 Bad Gateway" {
		t.Errorf("UpdateWebhook reset the delivery history: failures=%d status=%q, want 3 / \"502 Bad Gateway\"",
			updated.Failures, updated.LastStatus)
	}
	if updated.LastAttempt == nil || !updated.LastAttempt.Equal(attempt) {
		t.Errorf("UpdateWebhook cleared LastAttempt: %v, want %v", updated.LastAttempt, attempt)
	}

	// A success then zeroes the streak -- failures is SET, not incremented.
	ok := attempt.Add(time.Minute)
	if err := db.UpdateWebhookDelivery(ctx, created.ID, "200 OK", ok, 0); err != nil {
		t.Fatalf("UpdateWebhookDelivery(ok): %v", err)
	}
	got, err = db.GetWebhook(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetWebhook: %v", err)
	}
	if got.Failures != 0 || got.LastStatus != "200 OK" {
		t.Errorf("after a success: failures=%d status=%q, want 0 / \"200 OK\"", got.Failures, got.LastStatus)
	}
}

// TestWebhookInvalidInputNeverReachesTheDatabase asserts validation runs
// before the INSERT: a rejected input leaves no row.
func TestWebhookInvalidInputNeverReachesTheDatabase(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()

	bad := webhookInput("ops-slack")
	bad.URL = "file:///etc/passwd"
	if _, err := db.CreateWebhook(ctx, bad); err == nil {
		t.Fatal("CreateWebhook(file:// url) succeeded, want a validation error")
	}

	// alert.acknowledged, not alert.fired: M7 widened the closed set to the
	// two real alert events, so the probe for "outside the set" moved with it
	// (the unit twin in webhooks_test.go made the same M7 move).
	worse := webhookInput("ops-slack")
	worse.Events = []string{"alert.acknowledged"}
	if _, err := db.CreateWebhook(ctx, worse); err == nil {
		t.Fatal("CreateWebhook(alert.acknowledged) succeeded, want the closed event set to reject it")
	}

	hooks, err := db.ListWebhooks(ctx)
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if len(hooks) != 0 {
		t.Errorf("rejected creates left %d endpoints behind", len(hooks))
	}
}

// TestListWebhooksEmptyIsAnEmptySlice pins the nil-vs-empty distinction the
// API layer serializes: an unconfigured console must render [], not null.
func TestListWebhooksEmptyIsAnEmptySlice(t *testing.T) {
	db := newWebhooksDB(t)

	hooks, err := db.ListWebhooks(context.Background())
	if err != nil {
		t.Fatalf("ListWebhooks: %v", err)
	}
	if hooks == nil {
		t.Fatal("ListWebhooks returned nil on an empty table, want an empty slice")
	}
	if len(hooks) != 0 {
		t.Errorf("ListWebhooks returned %d endpoints on an empty table", len(hooks))
	}
}

// TestGetWebhookUnknownIDIsNotFound is the seam's miss contract.
func TestGetWebhookUnknownIDIsNotFound(t *testing.T) {
	db := newWebhooksDB(t)
	ctx := context.Background()
	id := uuid.NewString()

	if _, err := db.GetWebhook(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetWebhook(unknown) = %v, want ErrNotFound", err)
	}
	if err := db.DeleteWebhook(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteWebhook(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := db.UpdateWebhook(ctx, id, webhookInput("ops-slack")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateWebhook(unknown) = %v, want ErrNotFound", err)
	}
	if err := db.UpdateWebhookDelivery(ctx, id, "200", time.Now(), 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateWebhookDelivery(unknown) = %v, want ErrNotFound", err)
	}
}
