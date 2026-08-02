package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndexAtRoot(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("root status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="root"`) {
		t.Errorf("root body missing SPA mount point")
	}
}

func TestHandlerSPAFallbackForClientRoute(t *testing.T) {
	h, _ := Handler()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/investigate", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("SPA route status = %d, want 200 (index fallback)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="root"`) {
		t.Errorf("SPA fallback did not return index.html")
	}
}

func TestHandlerMissingAssetIs404(t *testing.T) {
	h, _ := Handler()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/assets/nope.js", http.NoBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", w.Code)
	}
}
