package ui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFaviconIsServed(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/favicon.svg", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /favicon.svg = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct[:9] != "image/svg" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
}

/*
 * The tracked placeholder must be BYTE-FOR-BYTE the file the build ships.
 *
 * dist/favicon.svg exists in git so `go test` can serve it without a node build; web/public is where
 * it is authored, and Vite copies it over on every build. Two copies of one file drift, and the
 * drift would be invisible: the Go test would keep passing against a stale icon while the console
 * served a different one.
 */
func TestFaviconPlaceholderMatchesTheSource(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("dist", "favicon.svg"))
	if err != nil {
		t.Fatalf("read the tracked placeholder: %v", err)
	}
	authored, err := os.ReadFile(filepath.Join("..", "..", "..", "web", "public", "favicon.svg"))
	if err != nil {
		t.Fatalf("read the authored source: %v", err)
	}
	if !bytes.Equal(shipped, authored) {
		t.Error("internal/console/ui/dist/favicon.svg and web/public/favicon.svg have drifted; " +
			"copy the authored one over the placeholder")
	}
}
