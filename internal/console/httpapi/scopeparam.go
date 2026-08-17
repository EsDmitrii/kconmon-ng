package httpapi

import (
	"net/http"
	"strings"
	"unicode"
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
// input at all. An empty value carries nothing and always passes.
func rejectControlChars(w http.ResponseWriter, field, v string) bool {
	if strings.IndexFunc(v, unicode.IsControl) < 0 {
		return false
	}
	writeProblem(w, http.StatusBadRequest, "invalid "+field,
		field+" must not contain control characters")
	return true
}
