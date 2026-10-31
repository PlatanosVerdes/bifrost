// bifrost reads an AceStream channel list published on IPFS, writes Jellyfin's playlist and guide, and
// serves the proxy Jellyfin plays channels from. See README.md.
package main

import (
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/access"
	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/env"
	"github.com/PlatanosVerdes/bifrost/internal/slate"
	"github.com/PlatanosVerdes/bifrost/internal/stream"
	"github.com/PlatanosVerdes/bifrost/internal/web"
)

var buildVersion = "local"

func main() {
	cfg := loadConfig()
	cat := &catalog.Catalog{}
	sched := &catalog.Schedule{}
	board := &catalog.Board{}

	policy := stream.DefaultPolicy
	policy.Behind = env.Duration("SWITCH_BEHIND", policy.Behind)
	policy.ProbeFor = env.Duration("PROBE_FOR", policy.ProbeFor)
	policy.Cooldown = env.Duration("PROBE_COOLDOWN", policy.Cooldown)
	policy.MinHeadroom = env.Float("SWITCH_MIN_HEADROOM", policy.MinHeadroom)
	policy.Margin = env.Float("SWITCH_MARGIN", policy.Margin)
	policy.StarvingBelow = env.Float("STARVING_BELOW", policy.StarvingBelow)
	ffmpeg := findFFmpeg(env.Get("FFMPEG", "ffmpeg"))
	var offAir, loading *slate.Slate
	if ffmpeg != "" && env.Get("SLATE", "on") != "off" {
		var err error
		if offAir, err = slate.New(slate.OffAir); err != nil {
			log.Fatalf("the embedded slate: %v", err)
		}
		if loading, err = slate.New(slate.Loading); err != nil {
			log.Fatalf("the embedded loading slate: %v", err)
		}
	}
	h := stream.NewHub(stream.Config{
		Engine:      cfg.aceserveBaseURL,
		MaxWait:     env.Duration("MAX_WAIT", 45*time.Second),
		Linger:      env.Duration("LINGER", 10*time.Second),
		SourceWait:  env.Duration("SOURCE_WAIT", 25*time.Second),
		ChannelWait: env.Duration("CHANNEL_MAX_WAIT", 90*time.Second),
		JellyfinURL: cfg.list.JellyfinURL,
		JellyfinKey: cfg.list.JellyfinAPIKey,
		Policy:      policy,
		FFmpeg:      ffmpeg,
		Ladder:      env.Get("RESOLUTION_SWITCH", "on") != "off",
		SurveyMax:   stream.MachineSize(env.Get("MACHINE_SIZE", "small")).Surveys,
		ThumbsDir:   filepath.Join(cfg.list.StateDir, "thumbs"),
		Slate:       offAir,
		Loading:     loading,
		Catalog:     cat,
		Schedule:    sched,
	})
	cat.Blocked = catalog.LoadBlocklist(cfg.list.StateDir)
	factsFile := filepath.Join(cfg.list.StateDir, "sources.json")
	h.LoadFacts(factsFile)
	go h.KeepFacts(factsFile, 30*time.Second)

	if after := env.Duration("ORPHAN_AFTER", 15*time.Minute); after > 0 && cfg.list.JellyfinURL != "" {
		go h.CloseOrphans(time.Minute, after)
	}

	// The guide left by the last run, so the page knows what airs before the first pass cuts a new one.
	_ = sched.Load(filepath.Join(filepath.Dir(cfg.list.OutputFile), "guide.xml"))

	if cfg.list.PushgatewayURL != "" {
		go h.PushSwarm(cfg.list.PushgatewayURL)
	}

	l := &catalog.Lister{Config: cfg.list, Catalog: cat, Schedule: sched, Status: board, Failed: h.Failed, Watching: h.Active}
	go func() {
		for {
			l.Run()
			sleep := catalog.SleepMin + rand.Intn(catalog.SleepRange)
			log.Printf("Sleeping %dm%ds...", sleep/60, sleep%60)
			time.Sleep(time.Duration(sleep) * time.Second)
		}
	}()

	log.Printf("bifrost %s: %s -> %s, playlist %s", buildVersion, cfg.listenAddr, cfg.aceserveBaseURL, cfg.list.OutputFile)
	// No WriteTimeout: a stream answer lasts as long as the broadcast.
	a := &web.API{Name: env.Get("BIFROST_NAME", hostname()), Version: buildVersion, Hub: h, Catalog: cat, List: board}
	srv := &http.Server{Addr: cfg.listenAddr, Handler: web.Routes(a, access.New(cfg.access)), ReadHeaderTimeout: 10 * time.Second}
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

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "bifrost"
}

type settings struct {
	listenAddr      string
	aceserveBaseURL string
	list            catalog.Config
	access          access.Config
}

func loadConfig() settings {
	// A path is a file of lists, reread on every pass so an edit needs no restart.
	raw, file := env.Must("SOURCE_URLS"), ""
	var lists [][]string
	var err error
	if strings.HasPrefix(raw, "/") {
		file = raw
		lists, err = catalog.ReadSourceFile(file)
	} else {
		lists, err = catalog.ParseSourceLists(raw)
	}
	if err != nil {
		log.Fatalf("SOURCE_URLS: %v", err)
	}
	stateDir := env.Get("STATE_DIR", "/state")
	return settings{
		listenAddr:      env.Get("LISTEN_ADDR", ":6879"),
		aceserveBaseURL: env.Get("ACESERVE_BASE_URL", "http://aceserve:6878"),
		list: catalog.Config{
			OutputFile:     env.Get("OUTPUT_FILE", "/data/channels_ace.m3u"),
			StateDir:       stateDir,
			ChannelBaseURL: env.Get("CHANNEL_BASE_URL", "http://bifrost:6879/channel/"),
			GuideSourceURL: os.Getenv("GUIDE_SOURCE_URL"),
			SourceLists:    lists,
			SourceFile:     file,
			KeepDropped:    env.Duration("KEEP_DROPPED", catalog.KeepDroppedFor),
			PushgatewayURL: os.Getenv("PUSHGATEWAY_URL"),
			JellyfinURL:    env.Must("JELLYFIN_URL"),
			JellyfinAPIKey: env.Must("JELLYFIN_API_KEY"),
		},
		access: access.Config{
			StateDir:   stateDir,
			Admins:     os.Getenv("BIFROST_ADMINS"),
			TeamDomain: os.Getenv("ACCESS_TEAM_DOMAIN"),
			AUD:        os.Getenv("ACCESS_AUD"),
			JoinAUD:    os.Getenv("ACCESS_JOIN_AUD"),
			AccountID:  os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
			APIToken:   os.Getenv("CLOUDFLARE_API_TOKEN"),
		},
	}
}
