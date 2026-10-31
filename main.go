// bifrost reads an AceStream channel list published on IPFS, writes Jellyfin's playlist and guide, and
// serves the proxy Jellyfin plays channels from. See README.md.
package main

import (
	"log"
	"math/rand"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"
)

var buildVersion = "local"

func main() {
	cfg := loadConfig()
	cat := &catalog{}

	h := newHub(cfg.aceserveBaseURL, getduration("MAX_WAIT", 45*time.Second), getduration("LINGER", 10*time.Second))
	h.table = cat
	h.jellyfinURL, h.jellyfinKey = cfg.jellyfinURL, cfg.jellyfinAPIKey
	h.name = getenv("BIFROST_NAME", hostname())
	h.sourceWait = getduration("SOURCE_WAIT", 25*time.Second)
	h.channelWait = getduration("CHANNEL_MAX_WAIT", 90*time.Second)
	h.policy.behind = getduration("SWITCH_BEHIND", h.policy.behind)
	h.policy.probeFor = getduration("PROBE_FOR", h.policy.probeFor)
	h.policy.cooldown = getduration("PROBE_COOLDOWN", h.policy.cooldown)
	h.policy.minHeadroom = getfloat("SWITCH_MIN_HEADROOM", h.policy.minHeadroom)
	h.policy.margin = getfloat("SWITCH_MARGIN", h.policy.margin)
	h.policy.starvingBelow = getfloat("STARVING_BELOW", h.policy.starvingBelow)
	h.ffmpeg = findFFmpeg(getenv("FFMPEG", "ffmpeg"))
	h.ladder = getenv("RESOLUTION_SWITCH", "on") != "off"
	h.thumbs = newThumbs(filepath.Join(cfg.stateDir, "thumbs"), h.ffmpeg)
	cat.blocked = loadBlocklist(cfg.stateDir)
	factsFile := filepath.Join(cfg.stateDir, "sources.json")
	h.loadFacts(factsFile)
	go h.keepFacts(factsFile, 30*time.Second)
	if h.ffmpeg != "" && getenv("SLATE", "on") != "off" {
		sl, err := newSlate(offairTS)
		if err != nil {
			log.Fatalf("the embedded slate: %v", err)
		}
		h.slate = sl
		if h.loading, err = newSlate(loadingTS); err != nil {
			log.Fatalf("the embedded loading slate: %v", err)
		}
	}

	if after := getduration("ORPHAN_AFTER", 15*time.Minute); after > 0 && h.jellyfinURL != "" {
		go h.closeOrphans(time.Minute, after)
	}

	// The guide left by the last run, so the page knows what airs before the first pass cuts a new one.
	_ = h.schedule.load(filepath.Join(filepath.Dir(cfg.outputFile), "guide.xml"))

	if cfg.pushgatewayURL != "" {
		go h.pushSwarm(cfg.pushgatewayURL)
	}

	l := &lister{cfg: cfg, catalog: cat, hub: h}
	go func() {
		for {
			l.run()
			sleep := sleepMin + rand.Intn(sleepRange)
			log.Printf("Sleeping %dm%ds...", sleep/60, sleep%60)
			time.Sleep(time.Duration(sleep) * time.Second)
		}
	}()

	log.Printf("bifrost %s: %s -> %s, playlist %s", buildVersion, cfg.listenAddr, cfg.aceserveBaseURL, cfg.outputFile)
	// No WriteTimeout: a stream answer lasts as long as the broadcast.
	srv := &http.Server{Addr: cfg.listenAddr, Handler: routes(h, newAccess(cfg)), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

func findFFmpeg(name string) string {
	if name == "off" {
		return ""
	}
	path, err := exec.LookPath(name)
	if err != nil {
		log.Printf("[proxy] no ffmpeg (%v): sources are relayed raw, and a switch may freeze some players", err)
		return ""
	}
	return path
}

func routes(h *hub, acc *access) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if h.table.count() == 0 {
			http.Error(w, "no channel list yet", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/status", guarded(h.serveStatus))
	mux.HandleFunc("/api/channels", guarded(h.serveChannels))
	mux.HandleFunc("POST /api/channels/{slug}/source", guarded(h.servePick))
	mux.HandleFunc("POST /api/channels/{slug}/blocked/{id}", guarded(h.serveBlock))
	mux.HandleFunc("DELETE /api/channels/{slug}/blocked/{id}", guarded(h.serveBlock))
	mux.HandleFunc("GET /api/thumbs/{id}", guarded(h.serveThumb))
	mux.HandleFunc("/api/access", guarded(acc.serve))
	mux.HandleFunc("/api/access/members", guarded(acc.serveMembers))
	mux.HandleFunc("/api/access/links", guarded(acc.serveLinks))
	mux.HandleFunc("/join/{token}", guarded(acc.serveJoin))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isPage(r.URL.Path) {
			guarded(servePage)(w, r)
			return
		}
		h.serve(w, r)
	})
	return mux
}
