// Package ui embeds the built Console SPA and serves it with an SPA fallback.
// The Vite build (web/) writes into dist/; a placeholder dist/index.html is
// committed so the embed compiles without a node build.
package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

const (
	// Vite hashes every /assets/* filename, so changed content gets a new URL
	// and the old one is safe to cache forever.
	cacheImmutable = "public, max-age=31536000, immutable"
	// "no-cache" means revalidate before use, not "don't store"; with an ETag
	// an unchanged file costs one 304 instead of a re-download.
	cacheRevalidate = "no-cache"
)

// compressibleExt lists text-like extensions worth gzipping; binary formats
// (png, woff2) are already compressed and would only grow.
var compressibleExt = map[string]bool{
	".css": true, ".html": true, ".js": true, ".json": true, ".map": true,
	".mjs": true, ".svg": true, ".txt": true, ".webmanifest": true, ".xml": true,
}

type staticFile struct {
	etag  string
	ctype string
	gz    []byte // nil when gzip does not shrink the file
}

// Handler returns an http.Handler that serves embedded static assets and falls
// back to index.html for client-side routes. A missing path whose last segment
// contains a dot (i.e. looks like an asset) returns 404 instead of index.html.
func Handler() (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	return newHandler(dist)
}

// newHandler is split from Handler so tests can serve an fstest.MapFS: the
// real dist/assets is gitignored and absent on CI checkouts.
func newHandler(dist fs.FS) (http.Handler, error) {
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, err
	}
	indexETag := etagFor(index)
	// Gzip once at startup instead of precompressing in the build pipeline:
	// one code path for `go test`, `make build` and Dockerfile.console, and the
	// binary does not double-embed .gz siblings. ~3 MB compresses in <1s.
	files, err := preprocess(dist)
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		// FileServer would 301 /index.html to ./; serve it directly so both
		// spellings share the same no-cache+ETag contract.
		if p == "" || p == "index.html" {
			serveIndex(w, r, index, indexETag)
			return
		}
		if f, ok := files[p]; ok {
			serveStatic(w, r, f, p, fileServer)
			return
		}
		// Not found: asset-looking paths 404, client routes get index.html.
		if strings.Contains(lastSegment(p), ".") {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r, index, indexETag)
	}), nil
}

// preprocess reads every embedded file once, computing a strong ETag and a
// gzip variant for text types where compression actually helps.
func preprocess(dist fs.FS) (map[string]*staticFile, error) {
	files := map[string]*staticFile{}
	err := fs.WalkDir(dist, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		data, err := fs.ReadFile(dist, p)
		if err != nil {
			return err
		}
		f := &staticFile{etag: etagFor(data), ctype: contentTypeFor(p, data)}
		if compressibleExt[path.Ext(p)] {
			if gz := gzipBytes(data); len(gz) < len(data) {
				f.gz = gz
			}
		}
		files[p] = f
		return nil
	})
	return files, err
}

func serveStatic(w http.ResponseWriter, r *http.Request, f *staticFile, p string, fallback http.Handler) {
	hashed := strings.HasPrefix(p, "assets/")
	if hashed {
		w.Header().Set("Cache-Control", cacheImmutable)
	} else {
		w.Header().Set("Cache-Control", cacheRevalidate)
	}
	if f.gz != nil {
		w.Header().Set("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			serveGzip(w, r, f, hashed)
			return
		}
	}
	if !hashed {
		// net/http's serveContent honors a pre-set ETag header, so FileServer
		// answers If-None-Match with 304 on this path. Hashed assets need no
		// validator: immutable means clients never revalidate them.
		w.Header().Set("ETag", f.etag)
	}
	fallback.ServeHTTP(w, r)
}

func serveGzip(w http.ResponseWriter, r *http.Request, f *staticFile, hashed bool) {
	etag := gzipETag(f.etag)
	if !hashed {
		w.Header().Set("ETag", etag)
	}
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Type", f.ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(f.gz)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(f.gz)
}

func serveIndex(w http.ResponseWriter, r *http.Request, index []byte, etag string) {
	w.Header().Set("Cache-Control", cacheRevalidate)
	w.Header().Set("ETag", etag)
	if etagMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(index)
}

func etagFor(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// gzipETag derives a distinct validator for the gzip representation
// (nginx-style suffix) so caches cannot mix it up with the identity one.
func gzipETag(etag string) string {
	return strings.TrimSuffix(etag, `"`) + `-gzip"`
}

// etagMatch implements If-None-Match weak comparison per RFC 9110 §13.1.2.
func etagMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for candidate := range strings.SplitSeq(header, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == etag {
			return true
		}
	}
	return false
}

// acceptsGzip does minimal Accept-Encoding parsing: enough for real browsers
// and for an explicit gzip;q=0 refusal; exotic qvalue games fall back to identity.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		if v, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); ok {
			q, err := strconv.ParseFloat(v, 64)
			return err == nil && q > 0
		}
		return true
	}
	return false
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	// BestCompression: paid once at startup, saved on every response.
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(data); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

func contentTypeFor(p string, data []byte) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return http.DetectContentType(data)
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
