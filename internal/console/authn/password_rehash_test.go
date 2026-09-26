package authn_test

import (
	"strings"
	"testing"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

// A hash written with other argon2 parameters, like every pre-2.5.0 account's, is flagged for an
// upgrade; one HashPassword wrote today is not, and neither is a string that is no hash at all.
func TestNeedsRehashFlagsOnlyHashesAtOtherParameters(t *testing.T) {
	current, err := authn.HashPassword("current parameters")
	if err != nil {
		t.Fatal(err)
	}
	if authn.NeedsRehash(current) {
		t.Error("a hash at the current parameters is flagged for a rehash")
	}
	if !authn.NeedsRehash(legacyPHC("pre-2.5.0 account")) {
		t.Error("a 64 MiB, t=3, p=2 hash is not flagged for a rehash")
	}
	if authn.NeedsRehash("not-a-phc") {
		t.Error("a malformed hash is flagged for a rehash")
	}
}

// The upgrade keeps the salt, so the password stamp every open session carries still matches: the
// password did not change, and nobody is signed out by it.
func TestRehashPasswordUpgradesTheParametersAndKeepsTheStamp(t *testing.T) {
	legacy := legacyPHC("pre-2.5.0 account")
	upgraded, err := authn.RehashPassword(legacy, "pre-2.5.0 account")
	if err != nil {
		t.Fatalf("RehashPassword: %v", err)
	}
	if authn.NeedsRehash(upgraded) {
		t.Errorf("the upgraded hash still needs a rehash: %s", upgraded)
	}
	if !strings.Contains(upgraded, "$m=19456,t=2,p=1$") {
		t.Errorf("upgraded hash %s is not at the current parameters", upgraded)
	}
	if ok, verr := authn.VerifyPassword(upgraded, "pre-2.5.0 account"); verr != nil || !ok {
		t.Fatalf("the upgraded hash does not verify the password: ok=%v err=%v", ok, verr)
	}
	if got, want := authn.PasswordStamp(upgraded), authn.PasswordStamp(legacy); got != want {
		t.Errorf("stamp changed from %q to %q: every open session would end", want, got)
	}
	if _, err := authn.RehashPassword("not-a-phc", "x"); err == nil {
		t.Error("RehashPassword accepted a malformed hash")
	}
}
