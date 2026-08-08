package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The run readers must reject a malformed id BEFORE they touch pgx (follow-up
// #5's other half). Two things are asserted at once here, and the second is
// the reason this test constructs a *DB with a NIL pool: reaching
// gen.New(db.pool) with a nil pool panics, so a test that returns cleanly is
// itself proof that no database round trip was attempted. Without the
// pre-check, httpapi's GET /api/v1/runs/{id} answers 502 ("run history
// unavailable") for a typo in a URL, where 404 is the truthful answer -- an id
// that is not a UUID cannot name a row in a UUID-keyed table.
func TestGetRunMalformedIDIsNotFoundWithoutTouchingPgx(t *testing.T) {
	db := &DB{}
	ctx := context.Background()

	for _, id := range []string{"", "not-a-uuid", "../../etc/passwd", "1234", "%00"} {
		t.Run("GetRun/"+id, func(t *testing.T) {
			_, err := db.GetRun(ctx, id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetRun(%q) err = %v, want ErrNotFound", id, err)
			}
		})
		t.Run("GetRunResults/"+id, func(t *testing.T) {
			_, err := db.GetRunResults(ctx, id)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetRunResults(%q) err = %v, want ErrNotFound", id, err)
			}
		})
	}
}

// The pre-check must not swallow the malformed id itself: an operator reading
// the log needs to see what was rejected.
func TestGetRunMalformedIDErrorNamesTheID(t *testing.T) {
	db := &DB{}
	_, err := db.GetRun(context.Background(), "not-a-uuid")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); !strings.Contains(got, "not-a-uuid") {
		t.Errorf("err = %q, want it to name the rejected id", got)
	}
}
