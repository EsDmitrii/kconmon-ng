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

// EncodeCursor builds the opaque keyset cursor ListEvents hands back as EventPage.NextCursor.
func EncodeCursor(eventTime time.Time, id int64) string {
	raw := eventTime.Format(time.RFC3339Nano) + cursorSep + strconv.FormatInt(id, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor is EncodeCursor's inverse. cursor == "" is the well-defined "no cursor" case — the
// first page of a listing.
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

// EncodeUUIDCursor builds the opaque keyset cursor every (created_at, UUID) listing hands back as
// its NextCursor.
func EncodeUUIDCursor(createdAt time.Time, id string) string {
	raw := createdAt.Format(time.RFC3339Nano) + cursorSep + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeUUIDCursor is EncodeUUIDCursor's inverse, with the same contract as DecodeCursor; must
// never panic and must never return ok == true with a zero-value time/id for a non-empty cursor.
func DecodeUUIDCursor(cursor string) (createdAt time.Time, id string, ok bool, err error) {
	if cursor == "" {
		return time.Time{}, "", false, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode cursor: %w", err)
	}

	parts := strings.SplitN(string(decoded), cursorSep, 2)
	if len(parts) != 2 {
		return time.Time{}, "", false, fmt.Errorf("store: decode cursor: missing %q separator", cursorSep)
	}

	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode cursor: parse time: %w", err)
	}

	if _, err := uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: decode cursor: parse id: %w", err)
	}

	return ts, parts[1], true, nil
}

// EncodeRunCursor builds the opaque keyset cursor ListRuns hands back as RunPage.NextCursor.
func EncodeRunCursor(createdAt time.Time, id string) string {
	return EncodeUUIDCursor(createdAt, id)
}

// DecodeRunCursor is EncodeRunCursor's inverse -- DecodeUUIDCursor under a
// run-specific name, same contract.
func DecodeRunCursor(cursor string) (createdAt time.Time, id string, ok bool, err error) {
	return DecodeUUIDCursor(cursor)
}

/*
EncodePairCursor builds the keyset cursor GET /api/v1/mtr/destinations hands back.

The sort key is the PAIR ITSELF, not the last_seen the pane displays, and that is deliberate. A
keyset cursor over a mutable sort key drops rows -- ListPathSnapshots' own comment spells this out --
and last_seen is bumped by every repeat trace: a pair sitting just below the cursor gets re-traced,
its last_seen jumps above the cursor, and the next page's predicate excludes it from a page it was
never on. (source_node, destination) never changes, so paging on it is complete by construction, and
the caller sorts the assembled set for display.

The two fields are length-prefixed rather than separator-joined: a node name or a destination may
contain any byte the store accepts, "|" included, and a separator that can appear inside a field is
a cursor that decodes to the wrong pair.
*/
func EncodePairCursor(sourceNode, destination string) string {
	raw := strconv.Itoa(len(sourceNode)) + cursorSep + sourceNode + destination
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodePairCursor is EncodePairCursor's inverse, with the same contract as DecodeCursor: an empty
// cursor is the well-defined first page, and a malformed one is an error rather than a silent reset.
func DecodePairCursor(cursor string) (sourceNode, destination string, ok bool, err error) {
	if cursor == "" {
		return "", "", false, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", false, fmt.Errorf("store: decode cursor: %w", err)
	}
	raw := string(decoded)
	sep := strings.Index(raw, cursorSep)
	if sep < 0 {
		return "", "", false, fmt.Errorf("store: decode cursor: missing %q separator", cursorSep)
	}
	n, err := strconv.Atoi(raw[:sep])
	if err != nil || n < 0 || sep+1+n > len(raw) {
		return "", "", false, fmt.Errorf("store: decode cursor: bad source length")
	}
	body := raw[sep+1:]
	return body[:n], body[n:], true, nil
}
