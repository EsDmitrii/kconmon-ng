package agent

import (
	"regexp"
	"strings"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

var urlPassword = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/?#@\s:]*):[^/?#@\s]*@`)

// redactURLs masks the password of every URL inside free text, as checker.RedactURL does for one.
func redactURLs(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return urlPassword.ReplaceAllString(s, "${1}:xxxxx@")
}

// redactHTTPResult masks target passwords in an http result that leaves the process. The HTTP checker
// already masks at the source; this is the second layer, so a checker that forgets cannot leak a
// password to the controller. Details are copied, not edited: the checker may share the slice.
func redactHTTPResult(r *model.CheckResult) {
	if r.Type != model.CheckHTTP {
		return
	}
	r.Error = redactURLs(r.Error)
	if details, ok := r.Details.([]HTTPDetails); ok {
		out := make([]HTTPDetails, len(details))
		for i := range details {
			out[i] = details[i]
			out[i].URL = checker.RedactURL(details[i].URL)
		}
		r.Details = out
	}
}
