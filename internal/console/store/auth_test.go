package store

import (
	"context"
	"encoding/json"
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

// A malformed id is refused before the pool, like UpdateUserPassword's, and never reads as a lost
// race (false, nil), which the login's hash upgrade would take silently.
func TestRehashUserPasswordRejectsMalformedIDBeforeTouchingThePool(t *testing.T) {
	swapped, err := (&DB{}).RehashUserPassword(context.Background(), "not-a-uuid", "old", "new")
	if err == nil || swapped {
		t.Fatalf("RehashUserPassword(malformed id) = %v, %v; want false and a parse error", swapped, err)
	}
	if !strings.Contains(err.Error(), "rehash user password") {
		t.Errorf("RehashUserPassword(malformed id) = %v, want the error prefixed with the operation", err)
	}
}

// storableAuditDetail keeps a clean detail byte for byte and turns what JSONB refuses into U+FFFD;
// the real-PostgreSQL side is TestAuditInsertStoresARowWhateverTheFieldsCarry.
func TestStorableAuditDetail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{``, `{}`},
		{`{"name":"ok","n":12345678901234567890}`, `{"name":"ok","n":12345678901234567890}`},
		{`{"name":"ci-\ud800-token","n":12345678901234567890}`, `{"n":12345678901234567890,"name":"ci-` + "�" + `-token"}`},
		{`{"a":["x\u0000y",{"\udc00":1}]}`, `{"a":["x` + "�" + `y",{"` + "�" + `":1}]}`},
		{"{\"a\":\"\xff\"}", `{"a":"` + "�" + `"}`},
		{`{"a":`, `{"unstorable":true}`},
		{`{} {}`, `{"unstorable":true}`},
		{`{"username":1e200000}`, `{"username":"1e200000"}`},
		{`{"username":1e-20000}`, `{"username":"1e-20000"}`},
		{`{"name":"ok","n":[1,1e131072]}`, `{"n":[1,"1e131072"],"name":"ok"}`},
	} {
		if got := string(storableAuditDetail(json.RawMessage(tc.in))); got != tc.want {
			t.Errorf("storableAuditDetail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := storableText("a\x00b\xffc"); got != "a�b�c" {
		t.Errorf("storableText = %q", got)
	}
}
