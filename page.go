package main

import (
	_ "embed"
	"net/http"
	"os"
)

// The status page: one static file that reads /api/channels and /api/status. Nothing on it opens a
// channel.
//
//go:embed web/index.html
var page []byte

func servePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(page)
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "bifrost"
}
