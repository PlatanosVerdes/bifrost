package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/access"
	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/stream"
)

func TestPageAndAPIHeaders(t *testing.T) {
	a := testAPI(stream.Config{Engine: "http://127.0.0.1:1", MaxWait: time.Second, Linger: time.Millisecond})
	mux := Routes(a, access.New(access.Config{}))
	want := map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/app.js"}, {"GET", "/vendor/lit-all.min.js"}, {"GET", "/favicon.svg"}, {"GET", "/apple-touch-icon.png"},
		{"GET", "/api/status"}, {"GET", "/api/channels"}, {"GET", "/api/access"}, {"POST", "/api/access/members"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s %s: %s is %q, want %q", c.method, c.path, k, got, v)
			}
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/vendor/lit-all.min.js", nil))
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("a script is served as %q, which nosniff would refuse to run", ct)
	}
}

func TestPageIsServed(t *testing.T) {
	for path, want := range map[string]string{"/": "bifrost-app", "/app.js": "api/channels", "/vendor/lit-all.min.js": "LitElement", "/favicon.svg": "<svg", "/apple-touch-icon.png": "PNG"} {
		rec := httptest.NewRecorder()
		servePage(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s: %d, missing %q", path, rec.Code, want)
		}
	}
	if isPage("/channel/channel-a-1080p") || isPage("/ace/getstream") || !isPage("/") || !isPage("/favicon.svg") {
		t.Error("isPage routes streams to the page or the page to the proxy")
	}
}

// testAPI builds a hub from cfg, with an empty catalog and guide unless given, and the page's API over it.
func testAPI(cfg stream.Config) *API {
	if cfg.Catalog == nil {
		cfg.Catalog = &catalog.Catalog{}
	}
	if cfg.Schedule == nil {
		cfg.Schedule = &catalog.Schedule{}
	}
	if cfg.Policy == (stream.Policy{}) {
		cfg.Policy = stream.DefaultPolicy
	}
	return &API{Hub: stream.NewHub(cfg), Catalog: cfg.Catalog, List: &catalog.Board{}}
}
