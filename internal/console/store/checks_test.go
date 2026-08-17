package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The run readers must reject a malformed id BEFORE they touch pgx.
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
			_, _, err := db.GetRunResults(ctx, id)
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
