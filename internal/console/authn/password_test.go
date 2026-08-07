package authn_test

import (
	"encoding/base64"
	"fmt"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/EsDmitrii/kconmon-ng/internal/console/authn"
)

func TestHashPasswordVerifyPasswordRoundtrip(t *testing.T) {
	t.Parallel()

	phc, err := authn.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := authn.VerifyPassword(phc, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("expected the correct password to verify")
	}
}

func TestHashPasswordSaltsDifferently(t *testing.T) {
	t.Parallel()

	phc1, err := authn.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	phc2, err := authn.HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if phc1 == phc2 {
		t.Fatalf("hashing the same password twice must produce different PHC strings (random salt), got identical: %q", phc1)
	}
}

func TestVerifyPasswordWrongPasswordFails(t *testing.T) {
	t.Parallel()

	phc, err := authn.HashPassword("the-real-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := authn.VerifyPassword(phc, "not-the-real-password")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("expected a wrong password to fail verification")
	}
}

// TestVerifyPasswordMalformedHashNeverSucceeds proves every unparseable phc
// -- truncated, garbage, or empty -- returns a non-nil error and (critically)
// never (true, nil). A verifier that silently treated "I couldn't parse
// this" as "it matched" would be a full authentication bypass.
// validSaltB64 and validHashB64 are a well-formed 16-byte salt and 32-byte
// hash (base64 RawStdEncoding), reused across the pathological-parameter
// cases below so that the ONLY thing under test in each case is the
// parameter (t=0, p=0, version) or field (empty hash) being exercised --
// not an incidental salt/hash-length rejection.
var (
	validSaltB64 = base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	validHashB64 = base64.RawStdEncoding.EncodeToString(make([]byte, 32))
)

// TestVerifyPasswordMalformedHashNeverSucceeds proves every unparseable phc
// -- truncated, garbage, empty, or carrying a pathological/out-of-range
// parameter -- returns a non-nil error and (critically) never (true, nil)
// and never panics. A verifier that silently treated "I couldn't parse
// this" as "it matched" would be a full authentication bypass; a verifier
// that panicked on attacker-supplied input (e.g. a stored/forged hash with
// t=0, p=0, or a truncated hash field) would be a denial-of-service vector.
// t=0, p=0, and the empty-hash-field case are verified (against
// golang.org/x/crypto v0.54.0) to panic inside argon2.IDKey if parsePHC ever
// let them through unchecked.
func TestVerifyPasswordMalformedHashNeverSucceeds(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty string":        "",
		"plain garbage":       "not a hash at all",
		"missing fields":      "$argon2id$v=19$m=65536,t=3,p=2$onlysalt",
		"wrong algorithm tag": "$argon2i$v=19$m=65536,t=3,p=2$c29tZXNhbHQ$c29tZWhhc2g",
		"invalid base64 salt": "$argon2id$v=19$m=65536,t=3,p=2$not-base64!!!$c29tZWhhc2g",
		"invalid base64 hash": "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHQ$not-base64!!!",
		"garbage param field": "$argon2id$v=19$garbage$c29tZXNhbHQ$c29tZWhhc2g",
		// t=0: argon2.IDKey panics with "argon2: number of rounds too small".
		"zero iterations (t=0)": fmt.Sprintf("$argon2id$v=19$m=65536,t=0,p=2$%s$%s", validSaltB64, validHashB64),
		// p=0: argon2.IDKey panics with "argon2: parallelism degree too low".
		"zero parallelism (p=0)": fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=0$%s$%s", validSaltB64, validHashB64),
		// An empty hash field decodes to a zero-length hash, which
		// nil-pointer-derefs inside argon2.IDKey's blake2b key expansion.
		"empty hash field": fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=2$%s$", validSaltB64),
		// v=16 (argon2i's older revision) must not silently pass as if it
		// were v=19 (argon2.Version) -- previously parsed then discarded.
		"unsupported version (v=16)": fmt.Sprintf("$argon2id$v=16$m=65536,t=3,p=2$%s$%s", validSaltB64, validHashB64),
		"non-numeric version":        fmt.Sprintf("$argon2id$v=99nonsense$m=65536,t=3,p=2$%s$%s", validSaltB64, validHashB64),
		// fmt.Sscanf("m=%d,t=%d,p=%d") previously reported success even with
		// trailing garbage after p=<int>; must now be rejected outright.
		"trailing garbage in parameter field": fmt.Sprintf("$argon2id$v=19$m=8,t=1,p=1,JUNK$%s$%s", validSaltB64, validHashB64),
	}

	for name, phc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ok, err := authn.VerifyPassword(phc, "whatever")
			if err == nil {
				t.Errorf("phc %q: expected a non-nil error, got nil", phc)
			}
			if ok {
				t.Errorf("phc %q: VerifyPassword must never return (true, nil) for a malformed hash", phc)
			}
		})
	}
}

// TestVerifyPasswordHonorsParametersEncodedInHash proves the argon2id
// parameters (m, t, p) and salt used at verification time come from the PHC
// string itself, not from HashPassword's current hardcoded defaults -- so a
// hash minted with different (e.g. lighter, faster-to-test) parameters than
// today's defaults still verifies correctly.
func TestVerifyPasswordHonorsParametersEncodedInHash(t *testing.T) {
	t.Parallel()

	const (
		memory      = 8 * 1024 // 8 MiB -- deliberately far from HashPassword's 64 MiB default
		iterations  = 1
		parallelism = 1
		keyLen      = 32
	)
	salt := []byte("0123456789ABCDEF")
	hash := argon2.IDKey([]byte("hunter2"), salt, iterations, memory, parallelism, keyLen)
	phc := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)

	ok, err := authn.VerifyPassword(phc, "hunter2")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("expected verification to succeed using the parameters embedded in the hash, not HashPassword's defaults")
	}

	ok, err = authn.VerifyPassword(phc, "wrong-password")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("expected a wrong password to still fail even against a hash with non-default parameters")
	}
}

// The actual timing-budget assertion lives in password_timing_test.go: one
// test that always runs, including under -race (M-5 -- it previously split
// into a real assertion and a silently-disabled one across two build-tagged
// files). Only the numeric budget itself is still build-tag split, in
// password_timing_budget_norace_test.go / password_timing_budget_race_test.go,
// because the race detector's instrumentation genuinely does inflate
// argon2id's wall-clock cost by roughly 7-10x on this hardware (measured
// ~500ms vs ~50-70ms for the exact same m=65536,t=3,p=2 call).
