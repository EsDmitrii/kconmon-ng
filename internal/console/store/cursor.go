package store

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// cursorSep joins the two fields of an encoded cursor. It cannot appear inside
// either field: RFC3339Nano never emits "|" and the id is formatted as a plain
// decimal integer.
const cursorSep = "|"

// EncodeCursor builds the opaque keyset cursor ListEvents hands back as
// EventPage.NextCursor: base64.RawURLEncoding of
// "<eventTime RFC3339Nano>|<id>". Encoding the same (eventTime, id) pair
// always yields the same string, which is what makes the cursor safe to
// compare or cache.
func EncodeCursor(eventTime time.Time, id int64) string {
	raw := eventTime.Format(time.RFC3339Nano) + cursorSep + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor is EncodeCursor's inverse. cursor == "" is the well-defined
// "no cursor" case — the first page of a listing — and reports ok == false
// with a nil error, never an error.
//
// Every other malformed input (bad base64, a missing separator, an
// unparseable timestamp, a non-numeric id) returns a non-nil error and
// ok == false. This function must never panic and must never return ok ==
// true with a zero-value time/id for a non-empty cursor: either of those
// would let a corrupt or tampered cursor silently restart pagination from
// the top, which for a caller polling a growing table loops forever instead
// of failing loudly.
func DecodeCursor(cursor string) (eventTime time.Time, id int64, ok bool, err error) {
	if cursor == "" {
		return time.Time{}, 0, false, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("store: decode cursor: %w", err)
	}

	// SplitN(2) so a stray separator inside a malformed id segment still
	// yields exactly two parts to validate, rather than silently truncating.
	parts := strings.SplitN(string(decoded), cursorSep, 2)
	if len(parts) != 2 {
		return time.Time{}, 0, false, fmt.Errorf("store: decode cursor: missing %q separator", cursorSep)
	}

	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("store: decode cursor: parse time: %w", err)
	}

	parsedID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("store: decode cursor: parse id: %w", err)
	}

	return ts, parsedID, true, nil
}

// EncodeRunCursor builds the opaque keyset cursor ListRuns hands back as
// RunPage.NextCursor: base64.RawURLEncoding of "<createdAt RFC3339Nano>|<id>",
// the same shape EncodeCursor uses except id is check_runs' own UUID primary
// key (a canonical UUID string) rather than a bigint.
func EncodeRunCursor(createdAt time.Time, id string) string {
	raw := createdAt.Format(time.RFC3339Nano) + cursorSep + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeRunCursor is EncodeRunCursor's inverse, with the same contract as
// DecodeCursor: cursor == "" is the well-defined "no cursor" case (ok ==
// false, err == nil); every other malformed input (bad base64, a missing
// separator, an unparseable timestamp, a non-UUID id) returns a non-nil
// error and ok == false. Must never panic and must never return ok == true
// with a zero-value time/id for a non-empty cursor -- same reasoning as
// DecodeCursor's doc comment.
func DecodeRunCursor(cursor string) (createdAt time.Time, id string, ok bool, err error) {
	if cursor == "" {
		return time.Time{}, "", false, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode run cursor: %w", err)
	}

	parts := strings.SplitN(string(decoded), cursorSep, 2)
	if len(parts) != 2 {
		return time.Time{}, "", false, fmt.Errorf("store: decode run cursor: missing %q separator", cursorSep)
	}

	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode run cursor: parse time: %w", err)
	}

	if _, err := uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode run cursor: parse id: %w", err)
	}

	return ts, parts[1], true, nil
}
