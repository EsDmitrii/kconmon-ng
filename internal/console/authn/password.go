package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// paramsFieldRE strictly matches a PHC parameter field ("m=<digits>,t=<digits>,p=<digits>")
// with nothing before or after it -- see parsePHC's use of it for why an
// unanchored fmt.Sscanf could not do this job.
var paramsFieldRE = regexp.MustCompile(`^m=(\d+),t=(\d+),p=(\d+)$`)

// Argon2id parameters for new hashes: OWASP's first option (19 MiB, t=2, p=1). Hashes written by
// earlier releases (64 MiB, t=3, p=2) keep verifying, since VerifyPassword reads m, t and p from the
// PHC; see argonSlots for how the two share the console pod's 256Mi default limit.
const (
	argonMemoryKiB   = 19 * 1024 // 19 MiB
	argonIterations  = 2
	argonParallelism = 1
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
	return hashWithSalt(plain, salt), nil
}

// NeedsRehash reports whether phc was written with argon2 parameters other than the current ones. A
// wrong password on such an account costs a different time than the dummy hash an unknown username
// is checked against, so a successful login upgrades it (RehashPassword).
func NeedsRehash(phc string) bool {
	p, err := parsePHC(phc)
	if err != nil {
		return false
	}
	return p.memory != argonMemoryKiB || p.iterations != argonIterations ||
		p.parallelism != argonParallelism || len(p.hash) != argonKeyBytes
}

// RehashPassword re-derives phc's password at the current parameters, keeping its salt: the password
// is unchanged, so its PasswordStamp, and every session carrying it, stays valid. plain must already
// have been verified against phc.
func RehashPassword(phc, plain string) (string, error) {
	p, err := parsePHC(phc)
	if err != nil {
		return "", err
	}
	return hashWithSalt(plain, p.salt), nil
}

func hashWithSalt(plain string, salt []byte) string {
	release := takeArgonSlots(argonMemoryKiB)
	defer release()
	hash := argon2.IDKey([]byte(plain), salt, argonIterations, argonMemoryKiB, argonParallelism, argonKeyBytes)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}

// argonSlots bounds the argon2id memory in flight for HashPassword and VerifyPassword together: a
// login is unauthenticated and each derivation holds its whole m, and the GC roughly doubles the live
// peak. A current hash takes one slot (2 x 19 MiB at once); a heavier one, like a 64 MiB hash from
// an earlier release, takes all of them and runs alone, which keeps the 256Mi default limit safe.
var (
	argonSlots   = make(chan struct{}, argonConcurrency)
	argonSlotsMu sync.Mutex
)

const argonConcurrency = 2

// takeArgonSlots blocks until a derivation with the given m (KiB) may run and returns its release.
// Slots are taken under argonSlotsMu so two heavy derivations cannot each hold half of them.
func takeArgonSlots(memoryKiB uint32) (release func()) {
	heavy := memoryKiB > argonMemoryKiB
	n := 1
	if heavy {
		n = argonConcurrency
	}
	argonSlotsMu.Lock()
	for range n {
		argonSlots <- struct{}{}
	}
	argonSlotsMu.Unlock()
	// A heavy run holds every slot; collecting on both sides of it keeps the finished blocks of
	// other runs from sitting next to its own, which the GC's normal pacing would allow.
	if heavy {
		runtime.GC()
	}
	return func() {
		if heavy {
			runtime.GC()
		}
		for range n {
			<-argonSlots
		}
	}
}

// VerifyPassword reports whether plain hashes to phc; the argon2id parameters (m, t, p) and salt
// are read out of phc itself.
//
// It BLOCKS while the argon2 memory budget is taken; see argonSlots.
func VerifyPassword(phc, plain string) (ok bool, err error) {
	params, err := parsePHC(phc)
	if err != nil {
		return false, err
	}

	release := takeArgonSlots(params.memory)
	defer release()

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

// parsePHC splits a PHC string of the exact shape HashPassword produces; any other shape is an error.
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

// SessionStamp is what a local session carries and what every request compares: the password
// stamp, plus the user's session epoch once a disable has bumped it. Epoch 0 gives the bare
// password stamp, so sessions issued before the epoch existed stay valid.
func SessionStamp(phc string, sessionEpoch int64) string {
	if sessionEpoch == 0 {
		return PasswordStamp(phc)
	}
	return PasswordStamp(phc) + "." + strconv.FormatInt(sessionEpoch, 10)
}

// PasswordStamp tags a PHC hash with its salt. HashPassword draws a fresh salt on every call, so a
// new password always yields a new stamp; the salt is public by design and the derived key never
// enters the stamp, so a session record carries nothing about the password.
func PasswordStamp(phc string) string {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[4] == "" {
		// Never empty: an empty stamp is a session from before 2.5.0, which is never checked.
		return "-"
	}
	return parts[4]
}
