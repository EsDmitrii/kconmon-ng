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

// Argon2id parameters: RFC 9106's SECOND recommended option (64 MiB, t=3, p=2); a console pod's
// default resource limit is 256Mi (charts values.yaml console.resources).
const (
	argonMemoryKiB   = 64 * 1024 // 64 MiB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltBytes   = 16
	argonKeyBytes    = 32
)

// errMalformedHash is wrapped into every parse failure VerifyPassword can hit.
var errMalformedHash = errors.New("authn: malformed argon2id PHC string")

// HashPassword returns an argon2id PHC string; a fresh, cryptographically random salt is drawn on
// every call.
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

// VerifyPassword reports whether plain hashes to phc; the argon2id parameters (m, t, p) and salt
// are read out of phc itself.
func VerifyPassword(phc, plain string) (ok bool, err error) {
	params, err := parsePHC(phc)
	if err != nil {
		return false, err
	}

	//nolint:gosec // params.hash's length is attacker-independent (it comes from a stored hash, not user input); truncation to uint32 cannot overflow a real argon2 key length
	got := argon2.IDKey([]byte(plain), params.salt, params.iterations, params.memory, params.parallelism, uint32(len(params.hash)))

	return subtle.ConstantTimeCompare(params.hash, got) == 1, nil
}

// phcParams is parsePHC's result: the argon2id parameters and salt/hash bytes read out of a PHC
// string; grouped into a struct (rather than five separate named returns) purely to keep parsePHC's
// signature small.
type phcParams struct {
	memory, iterations uint32
	parallelism        uint8
	salt, hash         []byte
}

// minSaltBytes and minHashBytes are the smallest salt/hash lengths parsePHC accepts; both are well
// below anything HashPassword ever produces (argonSaltBytes=16, argonKeyBytes=32).
const (
	minSaltBytes = 8
	minHashBytes = 16
)

// parsePHC splits a PHC string of the exact shape HashPassword produces; VerifyPassword's own doc
// comment promises "never a panic" for any malformed phc.
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

	// fmt.Sscanf("m=%d,t=%d,p=%d") reports 3 successful conversions and a nil error even for
	// "m=8,t=1,p=1,JUNK".
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
