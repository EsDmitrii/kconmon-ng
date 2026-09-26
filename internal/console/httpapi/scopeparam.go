package httpapi

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rejectControlChars guards a scope-family query param (scope, scopeNode) that
// flows into a Postgres text column. A NUL byte (%00) in such a param reaches
// the driver, which refuses NUL in text, and the surrounding handler would map
// that failure to a 502 "unavailable" -- a server error for what is really the
// caller's malformed input. This moves the verdict to the input boundary: it
// answers 400 and reports true (the caller must return) when v carries a control
// character. NUL is the one Postgres is fatal on; every other control character
// is refused for the same reason a scope never legitimately carries one -- a
// scope is a node or pair name -- so the 502 can no longer be provoked from this
// input at all. An empty value carries nothing and always passes. Bytes that are
// not valid UTF-8 (%FF) are refused the same way: PostgreSQL rejects them in a
// text column (SQLSTATE 22021), and IndexFunc alone reads them as U+FFFD.
func rejectControlChars(w http.ResponseWriter, field, v string) bool {
	if !invalidParamText(v) {
		return false
	}
	writeProblem(w, http.StatusBadRequest, "invalid "+field,
		field+" must be valid UTF-8 without control characters")
	return true
}

// invalidParamText reports whether v carries a control character or bytes that are not valid
// UTF-8, the two things a Postgres text column or a name never legitimately holds.
func invalidParamText(v string) bool {
	return !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0
}
