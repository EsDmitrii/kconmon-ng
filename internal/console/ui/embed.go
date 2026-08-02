// Package ui embeds the built Console SPA and serves it with an SPA fallback.
// The Vite build (web/) writes into dist/; a placeholder dist/index.html is
// committed so the embed compiles without a node build.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler returns an http.Handler that serves embedded static assets and falls
// back to index.html for client-side routes. A missing path whose last segment
// contains a dot (i.e. looks like an asset) returns 404 instead of index.html.
func Handler() (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(dist, "index.html")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			serveIndex(w, index)
			return
		}
		if _, statErr := fs.Stat(dist, p); statErr == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Not found: asset-looking paths 404, client routes get index.html.
		if strings.Contains(lastSegment(p), ".") {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, index)
	}), nil
}

func serveIndex(w http.ResponseWriter, index []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
