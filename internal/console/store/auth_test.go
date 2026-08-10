package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGetTokenByIDRejectsMalformedIDBeforeTouchingThePool pins the two halves of GetTokenByID's
// boundary contract at once.
func TestGetTokenByIDRejectsMalformedIDBeforeTouchingThePool(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"not a uuid at all", "not-a-uuid"},
		{"decimal id", "42"},
		{"uuid missing a group", "0e2a6b3c-1f4d-4a7b-9c8e"},
		{"uuid with a non-hex digit", "0e2a6b3c-1f4d-4a7b-9c8e-zzzzzzzzzzzz"},
		{"owner literal the api_tokens column also allows", "system"},
	}
	db := &DB{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := db.GetTokenByID(context.Background(), c.id)
			if err == nil {
				t.Fatalf("GetTokenByID(%q): want error, got token %+v", c.id, got)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("GetTokenByID(%q) = %v, want a parse error, NOT ErrNotFound", c.id, err)
			}
			if !strings.Contains(err.Error(), "get token by id") {
				t.Errorf("GetTokenByID(%q) = %v, want the error prefixed with the operation", c.id, err)
			}
			if got != (Token{}) {
				t.Errorf("GetTokenByID(%q) returned %+v alongside its error, want the zero Token", c.id, got)
			}
		})
	}
}

// TestGetTokenByIDAcceptsCanonicalUUIDShape is the other side of the boundary: a well-formed id
// must get PAST the pre-check.
func TestGetTokenByIDAcceptsCanonicalUUIDShape(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("GetTokenByID with a canonical UUID did not reach the (nil) pool: the parse pre-check rejected a valid id")
		}
	}()
	//nolint:errcheck // the call is expected to panic on the nil pool; there is no error to check
	_, _ = (&DB{}).GetTokenByID(context.Background(), "0e2a6b3c-1f4d-4a7b-9c8e-3d5f7a9b1c2e")
}
