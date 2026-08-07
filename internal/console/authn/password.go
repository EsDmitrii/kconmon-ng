package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// paramsFieldRE strictly matches a PHC parameter field ("m=<digits>,t=<digits>,p=<digits>")
// with nothing before or after it -- see parsePHC's use of it for why an
// unanchored fmt.Sscanf could not do this job.
var paramsFieldRE = regexp.MustCompile(`^m=(\d+),t=(\d+),p=(\d+)$`)

// Argon2id parameters: RFC 9106's SECOND recommended option (64 MiB, t=3,
// p=2), not the first-choice 2 GiB/t=1 profile. A console pod's default
// resource limit is 256Mi (charts values.yaml console.resources) -- the 2
// GiB profile would OOM the container the moment a handful of logins land
// concurrently, so the memory-conservative option is the correct one here,
// not a weaker fallback.
const (
	argonMemoryKiB   = 64 * 1024 // 64 MiB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

// errMalformedHash is wrapped into every parse failure VerifyPassword can
// hit, so a caller can tell "this looked like a hash but wasn't" apart from
// an argon2 computation failure (which this package never actually produces,
// since argon2.IDKey cannot itself fail).
var errMalformedHash = errors.New("authn: malformed argon2id PHC string")

// HashPassword returns an argon2id PHC string:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
// <salt> and <hash> are base64 (standard alphabet, no padding) -- the same
// convention the PHC string format and every common Go argon2id library use.
// A fresh, cryptographically random salt is drawn on every call, so hashing
// the same password twice never produces the same PHC string.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: hash password: read salt: %w", err)
	}

	hash := argon2.IDKey([]byte(plain), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyBytes)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether plain hashes to phc. The argon2id
// parameters (m, t, p) and salt are read out of phc itself, not hardcoded --
// so a hash produced with different parameters than HashPassword's current
// defaults still verifies correctly; only the current password can ever get
// a *new* hash minted with today's parameters (via HashPassword). A
// malformed, truncated, empty, or otherwise unparseable phc always returns
// (false, non-nil error) -- never (true, nil), and never a panic.
func VerifyPassword(phc, plain string) (ok bool, err error) {
	params, err := parsePHC(phc)
	if err != nil {
		return false, err
	}

	//nolint:gosec // params.hash's length is attacker-independent (it comes from a stored hash, not user input); truncation to uint32 cannot overflow a real argon2 key length
	got := argon2.IDKey([]byte(plain), params.salt, params.iterations, params.memory, params.parallelism, uint32(len(params.hash)))

	return subtle.ConstantTimeCompare(params.hash, got) == 1, nil
}

// phcParams is parsePHC's result: the argon2id parameters and salt/hash
// bytes read out of a PHC string. Grouped into a struct (rather than five
// separate named returns) purely to keep parsePHC's signature small; nothing
// about phcParams is meant to be reused outside this file.
type phcParams struct {
	memory, iterations uint32
	parallelism        uint8
	salt, hash         []byte
}

// minSaltBytes and minHashBytes are the smallest salt/hash lengths parsePHC
// accepts. Both are well below anything HashPassword ever produces
// (argonSaltBytes=16, argonKeyBytes=32); they exist purely to keep a
// pathologically short field (in particular a zero-length hash) from ever
// reaching argon2.IDKey, which panics on a hash length short enough to
// underflow inside its internal blake2b expansion rather than returning an
// error -- see parsePHC's doc comment for the specific inputs verified to
// panic.
const (
	minSaltBytes = 8
	minHashBytes = 16
)

// parsePHC splits a PHC string of the exact shape HashPassword produces:
// "$argon2id$v=<int>$m=<int>,t=<int>,p=<int>$<salt>$<hash>". Every failure
// mode (wrong field count, wrong algorithm tag, unparseable parameter block,
// invalid base64 in either the salt or hash field) returns errMalformedHash;
// there is no partial-success return.
//
// argon2.IDKey (golang.org/x/crypto/argon2, verified against v0.54.0) does
// not return an error for a bad parameter -- it panics. Three shapes of PHC
// string that pass a naive parse still reach argon2.IDKey with a
// panic-inducing parameter if this function does not reject them first:
// t=0 ("argon2: number of rounds too small"), p=0 ("argon2: parallelism
// degree too low"), and a PHC string with an empty hash field (e.g.
// "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHQ$"), which decodes to a
// zero-length hash and then nil-pointer-derefs inside blake2b's internal key
// expansion. VerifyPassword's own doc comment promises "never a panic" for
// any malformed phc, so all three are rejected here, before argon2.IDKey is
// ever called, alongside a version field that does not match argon2.Version
// (accepted-then-discarded previously, which silently tolerated a hash
// produced by a different argon2 revision) and a parameter field with
// trailing garbage after p=<int> (fmt.Sscanf on "m=%d,t=%d,p=%d" never
// required the format to consume the whole input, so "m=8,t=1,p=1,JUNK"
// parsed undetected before paramsFieldRE's anchored match replaced it).
func parsePHC(phc string) (phcParams, error) {
	parts := strings.Split(phc, "$")
	// strings.Split("$a$b$c$d$e", "$") == ["", "a", "b", "c", "d", "e"]: the
	// string's leading '$' makes parts[0] an empty string.
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return phcParams{}, errMalformedHash
	}

	var version int
	if _, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil {
		return phcParams{}, fmt.Errorf("%w: version field %q", errMalformedHash, parts[2])
	}
	if version != argon2.Version {
		return phcParams{}, fmt.Errorf("%w: unsupported argon2 version %d (want %d)", errMalformedHash, version, argon2.Version)
	}

	// fmt.Sscanf("m=%d,t=%d,p=%d") reports 3 successful conversions and a nil
	// error even for "m=8,t=1,p=1,JUNK": Sscanf never required the format to
	// consume the entire input, and its %n verb (which would let us check
	// bytes-consumed) is rejected by the Scan family in this Go version --
	// "bad verb '%n' for integer" -- so it cannot be used to detect the
	// leftover ",JUNK" either. paramsFieldRE anchors both ends (^...$)
	// instead, which is the only way to reject trailing garbage outright.
	groups := paramsFieldRE.FindStringSubmatch(parts[3])
	if groups == nil {
		return phcParams{}, fmt.Errorf("%w: parameter field %q", errMalformedHash, parts[3])
	}
	var p phcParams
	memory, memErr := strconv.ParseUint(groups[1], 10, 32)
	iterations, iterErr := strconv.ParseUint(groups[2], 10, 32)
	parallelism, parErr := strconv.ParseUint(groups[3], 10, 8)
	if memErr != nil || iterErr != nil || parErr != nil {
		return phcParams{}, fmt.Errorf("%w: parameter field %q out of range", errMalformedHash, parts[3])
	}
	p.memory = uint32(memory)
	p.iterations = uint32(iterations)
	p.parallelism = uint8(parallelism)
	if p.iterations == 0 {
		return phcParams{}, fmt.Errorf("%w: iterations (t) must be at least 1, got %d", errMalformedHash, p.iterations)
	}
	if p.parallelism == 0 {
		return phcParams{}, fmt.Errorf("%w: parallelism (p) must be at least 1, got %d", errMalformedHash, p.parallelism)
	}

	salt, decErr := base64.RawStdEncoding.DecodeString(parts[4])
	if decErr != nil {
		return phcParams{}, fmt.Errorf("%w: salt is not valid base64: %w", errMalformedHash, decErr)
	}
	if len(salt) < minSaltBytes {
		return phcParams{}, fmt.Errorf("%w: salt too short (%d bytes, want at least %d)", errMalformedHash, len(salt), minSaltBytes)
	}
	p.salt = salt

	hash, decErr := base64.RawStdEncoding.DecodeString(parts[5])
	if decErr != nil {
		return phcParams{}, fmt.Errorf("%w: hash is not valid base64: %w", errMalformedHash, decErr)
	}
	if len(hash) < minHashBytes {
		return phcParams{}, fmt.Errorf("%w: hash too short (%d bytes, want at least %d)", errMalformedHash, len(hash), minHashBytes)
	}
	p.hash = hash

	return p, nil
}
