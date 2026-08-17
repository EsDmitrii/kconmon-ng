package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// strictJSONDecoder returns a decoder that REJECTS unknown fields. A misspelled
// mutation field (durationNS for durationNs, destinationaddress for
// destinationAddress) is otherwise silently dropped by encoding/json and a
// DIFFERENT write runs than the caller asked for -- a strict decoder turns that
// typo into a clean 400. Every request schema this is applied to is marked
// additionalProperties:false in docs/console-api.yaml, so this aligns the
// implementation with the spec rather than tightening past it.
func strictJSONDecoder(body io.Reader) *json.Decoder {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	return dec
}

// unknownFieldDetail returns a 400 detail that NAMES the offending field when
// err is encoding/json's DisallowUnknownFields error, so the caller learns which
// field was wrong. For any other decode error it returns fallback -- the
// endpoint's own shape hint, kept byte-identical to the pre-strict message so a
// genuinely malformed body still reads exactly as before.
func unknownFieldDetail(err error, fallback string) string {
	const prefix = "json: unknown field "
	if msg := err.Error(); strings.HasPrefix(msg, prefix) {
		return "unknown field " + strings.TrimPrefix(msg, prefix) +
			" -- check the field name against the API schema"
	}
	return fallback
}

// decodeMutationBody is the one-call form for a handler that decodes straight
// into req and has no extra decode-time predicate: it strict-decodes, writes a
// 400 naming any unknown field on failure, and reports ok. Handlers that combine
// the decode with another check (|| req.Name == "") inline strictJSONDecoder +
// unknownFieldDetail instead so their own message survives.
func decodeMutationBody(w http.ResponseWriter, r *http.Request, req any, fallbackDetail string) bool {
	dec := strictJSONDecoder(r.Body)
	if err := dec.Decode(req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid request", unknownFieldDetail(err, fallbackDetail))
		return false
	}
	/* Nothing may FOLLOW the value.
	   Decode stops at the end of the first JSON value and the rest of the body was discarded in
	   silence, so `{...}{...}` — a duplicated body, a concatenation, a proxy that stapled two
	   requests together — was accepted as the first object alone. A body this handler cannot fully
	   account for is not a body it should act on. */
	if dec.More() {
		writeProblem(w, http.StatusBadRequest, "invalid request",
			"body carries more than one JSON value; send exactly one object")
		return false
	}
	return true
}
