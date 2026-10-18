package main

import (
	"embed"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

// The status page: index.html, its Lit components in app.js, and Lit itself under vendor/, pinned
// and served from here so the page needs nothing but this process. It only reads the API.
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

// isPage says whether a path belongs to the page rather than to a stream.
func isPage(path string) bool {
	return path == "/" || path == "/app.js" || path == "/favicon.svg" || strings.HasPrefix(path, "/vendor/")
}

func servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	webFiles.ServeHTTP(w, r)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "bifrost"
}
