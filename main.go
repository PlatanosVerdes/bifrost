// bifrost bridges an AceStream channel list published on IPFS to Jellyfin.
//
// One process with two halves sharing the channel catalog in memory: the list loop, which reads the
// list, writes Jellyfin's playlist and cut-down guide and tells Jellyfin when either really changed;
// and the proxy, which Jellyfin plays from, picking a live source per channel and sharing one
// engine connection among everyone watching it. See README.md for why each piece is the way it is.
package main

import (
	"log"
	"math/rand"
	"net/http"
	"time"
)

// buildVersion is set at build time to the release tag.
var buildVersion = "local"

func main() {
	cfg := loadConfig()
	cat := &catalog{}

	h := newHub(cfg.aceserveBaseURL, getduration("MAX_WAIT", 45*time.Second), getduration("LINGER", 10*time.Second))
	h.table = cat
	h.jellyfinURL, h.jellyfinKey = cfg.jellyfinURL, cfg.jellyfinAPIKey
	h.sourceWait = getduration("SOURCE_WAIT", 25*time.Second)
	h.channelWait = getduration("CHANNEL_MAX_WAIT", 90*time.Second)

	l := &lister{cfg: cfg, catalog: cat, hub: h}
	go func() {
		for {
			l.run()
			sleep := sleepMin + rand.Intn(sleepRange)
			log.Printf("Sleeping %dm%ds...", sleep/60, sleep%60)
			time.Sleep(time.Duration(sleep) * time.Second)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if cat.count() == 0 {
			http.Error(w, "no channel list yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/status", h.serveStatus)
	mux.HandleFunc("/api/channels", h.serveChannels)
	mux.HandleFunc("/", h.serve)

	log.Printf("bifrost %s: %s -> %s, playlist %s", buildVersion, cfg.listenAddr, cfg.aceserveBaseURL, cfg.outputFile)
	log.Fatal(http.ListenAndServe(cfg.listenAddr, mux))
}
