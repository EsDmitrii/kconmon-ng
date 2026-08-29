package ui

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// testDist mimics a Vite build: hashed /assets/* files, an unhashed favicon,
// and index.html. Real dist/assets is gitignored, so tests need their own FS.
func testDist() fs.FS {
	js := bytes.Repeat([]byte("export const kconmon = 'ng';\n"), 200)
	svg := bytes.Repeat([]byte("<svg><circle r=\"1\"/></svg>\n"), 100)
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<!doctype html><html><body><div id="root"></div></body></html>`)},
		"assets/index-abc123.js": {Data: js},
		"assets/logo-def456.png": {Data: []byte("\x89PNG\r\n\x1a\nnot-really-compressible")},
		"favicon.svg":            {Data: svg},
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	h, err := newHandler(testDist())
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return h
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, http.NoBody)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return out
}

func TestHashedAssetIsImmutable(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/assets/index-abc123.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none without Accept-Encoding", ce)
	}
	orig, _ := fs.ReadFile(testDist(), "assets/index-abc123.js")
	if !bytes.Equal(w.Body.Bytes(), orig) {
		t.Error("identity body differs from the embedded file")
	}
}

func TestHashedAssetGzipNegotiation(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/assets/index-abc123.js",
		map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", ce)
	}
	if v := w.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable on gzip variant too", cc)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want javascript", ct)
	}
	orig, _ := fs.ReadFile(testDist(), "assets/index-abc123.js")
	if got := gunzip(t, w.Body.Bytes()); !bytes.Equal(got, orig) {
		t.Error("gunzipped body differs from the embedded file")
	}
	if len(w.Body.Bytes()) >= len(orig) {
		t.Errorf("gzip variant (%d bytes) is not smaller than original (%d bytes)", w.Body.Len(), len(orig))
	}
}

func TestBinaryAssetIsNotGzipped(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/assets/logo-def456.png",
		map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want none for binary asset", ce)
	}
	orig, _ := fs.ReadFile(testDist(), "assets/logo-def456.png")
	if !bytes.Equal(w.Body.Bytes(), orig) {
		t.Error("binary body differs from the embedded file")
	}
}

func TestGzipQZeroIsRefused(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/assets/index-abc123.js",
		map[string]string{"Accept-Encoding": "gzip;q=0"})
	if ce := w.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q, want identity when gzip has q=0", ce)
	}
}

func TestIndexIsNoCacheWithETag(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("index response has no ETag")
	}
	w2 := do(t, h, http.MethodGet, "/", map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 carried a body of %d bytes", w2.Body.Len())
	}
}

func TestSPAFallbackRevalidatesWithIndexETag(t *testing.T) {
	h := newTestHandler(t)
	etag := do(t, h, http.MethodGet, "/", nil).Header().Get("ETag")
	w := do(t, h, http.MethodGet, "/investigate", map[string]string{"If-None-Match": etag})
	if w.Code != http.StatusNotModified {
		t.Fatalf("SPA fallback revalidation = %d, want 304", w.Code)
	}
}

func TestExplicitIndexPathServesIndex(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/index.html", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no FileServer 301)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="root"`) {
		t.Error("/index.html did not return the SPA index")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
}

func TestUnhashedFileRevalidates(t *testing.T) {
	h := newTestHandler(t)
	hdr := map[string]string{"Accept-Encoding": "gzip"}
	w := do(t, h, http.MethodGet, "/favicon.svg", hdr)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache for unhashed file", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("unhashed file has no ETag")
	}
	w2 := do(t, h, http.MethodGet, "/favicon.svg",
		map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Fatalf("favicon revalidation = %d, want 304", w2.Code)
	}
}

func TestUnhashedFileIdentityRevalidates(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodGet, "/favicon.svg", nil)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("identity favicon response has no ETag")
	}
	w2 := do(t, h, http.MethodGet, "/favicon.svg", map[string]string{"If-None-Match": etag})
	if w2.Code != http.StatusNotModified {
		t.Fatalf("identity favicon revalidation = %d, want 304", w2.Code)
	}
}

func TestHeadGzipAssetHasNoBody(t *testing.T) {
	h := newTestHandler(t)
	w := do(t, h, http.MethodHead, "/assets/index-abc123.js",
		map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ce := w.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", ce)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD carried a body of %d bytes", w.Body.Len())
	}
}

func TestMissingAssetStill404(t *testing.T) {
	h := newTestHandler(t)
	if w := do(t, h, http.MethodGet, "/assets/nope.js", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want 404", w.Code)
	}
}
