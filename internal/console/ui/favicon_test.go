package ui

import (
	"net/http"
	"net/http/httptest"
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
