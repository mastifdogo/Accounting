package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The SvelteKit static build is copied into backend/web by `make frontend`
// and embedded, so the server stays a single binary. Without a build, the
// directory only holds .gitkeep and a placeholder page is served.
//
//go:embed all:web
var webEmbed embed.FS

// spaFallback is the page adapter-static writes for routes that are not
// prerendered (configured as `fallback` in frontend/vite.config.ts).
const spaFallback = "200.html"

// staticHandler serves the embedded frontend. Unknown paths get the SPA
// fallback page so client-side routing handles them.
func staticHandler() http.Handler {
	web, err := fs.Sub(webEmbed, "web")
	if err != nil {
		panic(err) // embed path is a compile-time constant
	}
	if _, err := fs.Stat(web, spaFallback); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<!doctype html><title>Ledger</title><p>Frontend not built. Run <code>make build</code>. The API is at <a href=\"/api/v1/health\">/api/v1</a>.</p>"))
		})
	}

	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if p != "" {
			if info, err := fs.Stat(web, p); err == nil && !info.IsDir() {
				if strings.HasPrefix(p, "_app/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// Client-side route: serve the SPA shell.
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, web, spaFallback)
	})
}
