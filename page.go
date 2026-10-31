package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

// Lit is vendored under web/vendor so the page needs nothing but this process.
//
//go:embed web
var webFS embed.FS

var webFiles = func() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}()

func isPage(path string) bool {
	return path == "/" || path == "/app.js" || path == "/favicon.svg" || strings.HasPrefix(path, "/vendor/")
}

func servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	webFiles.ServeHTTP(w, r)
}

// The PNG the Dockerfile renders from favicon.svg, since iOS ignores SVG icons on the home screen.
const touchIconPath = "/usr/local/share/bifrost/apple-touch-icon.png"

func serveTouchIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, touchIconPath)
}

func guarded(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next(w, r)
	}
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "bifrost"
}
