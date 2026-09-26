package ui

import (
	"net/http"
	"strings"
	"testing"
)

/*
The console page carried no framing policy, so a hostile page could load it in a transparent iframe
and lay a decoy over a Run or Delete button; the click landed in the console with the viewer's
authority and passed the same-origin CSRF check. Every page the SPA can be loaded from refuses to
be framed, 304 revalidations included.
*/
func TestConsolePageRefusesToBeFramed(t *testing.T) {
	h := newTestHandler(t)
	etag := do(t, h, http.MethodGet, "/", nil).Header().Get("ETag")
	for _, tc := range []struct {
		name, target string
		hdr          map[string]string
	}{
		{"root", "/", nil},
		{"explicit index", "/index.html", nil},
		{"client route", "/investigate", nil},
		{"revalidated index", "/", map[string]string{"If-None-Match": etag}},
		{"asset", "/assets/index-abc123.js", nil},
	} {
		w := do(t, h, http.MethodGet, tc.target, tc.hdr)
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: Content-Security-Policy = %q, want frame-ancestors 'none'", tc.name, csp)
		}
		if xfo := w.Header().Get("X-Frame-Options"); xfo != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", tc.name, xfo)
		}
	}
}
