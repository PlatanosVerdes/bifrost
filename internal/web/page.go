// Package web serves the page and the JSON it reads.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/PlatanosVerdes/bifrost/internal/access"
)

// Lit is vendored under static/vendor so the page needs nothing but this process. iOS ignores SVG
// icons on the home screen, so it gets a PNG of favicon.svg, its background squared because iOS
// rounds the corners itself.
//
//go:generate sh -c "echo 'svg > rect:first-of-type { rx: 0; ry: 0 }' | rsvg-convert --width 180 --height 180 --stylesheet /dev/stdin static/favicon.svg -o static/apple-touch-icon.png"
//go:embed static
var webFS embed.FS

var static = func() fs.FS {
	sub, err := fs.Sub(webFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

var webFiles = http.FileServer(http.FS(static))

// isPage is any file the page is made of; every other path is the proxy's.
func isPage(path string) bool {
	if path == "/" {
		return true
	}
	info, err := fs.Stat(static, strings.TrimPrefix(path, "/"))
	return err == nil && !info.IsDir()
}

func servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	webFiles.ServeHTTP(w, r)
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

func Routes(a *API, acc *access.Access) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if a.Catalog.Count() == 0 {
			http.Error(w, "no channel list yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/status", guarded(a.serveStatus))
	mux.HandleFunc("/api/channels", guarded(a.serveChannels))
	mux.HandleFunc("POST /api/channels/{slug}/source", guarded(a.Hub.ServePick))
	mux.HandleFunc("POST /api/channels/{slug}/blocked/{id}", guarded(a.serveBlock))
	mux.HandleFunc("DELETE /api/channels/{slug}/blocked/{id}", guarded(a.serveBlock))
	mux.HandleFunc("GET /api/thumbs/{id}", guarded(a.serveThumb))
	mux.HandleFunc("POST /api/channels/{slug}/survey", guarded(a.Hub.ServeSurvey))
	mux.HandleFunc("/api/access", guarded(acc.Serve))
	mux.HandleFunc("/api/access/members", guarded(acc.ServeMembers))
	mux.HandleFunc("/api/access/links", guarded(acc.ServeLinks))
	mux.HandleFunc("/join/{token}", guarded(acc.ServeJoin))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isPage(r.URL.Path) {
			guarded(servePage)(w, r)
			return
		}
		a.Hub.Serve(w, r)
	})
	return mux
}
