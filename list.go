package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// qualitySuffix strips resolution and source markers: "Channel+ C 1080p **" -> "Channel+ C".
var qualitySuffix = regexp.MustCompile(`\s+(?:(?:720p|1080p)\s+)?\*+$`)

const (
	guideMaxAge     = 24 * time.Hour
	refreshDebounce = 2 * time.Minute // a channel that drops out and back in costs one refresh, not two
	// Jellyfin rereads its copy of the XMLTV file, not the file, until the copy is an hour old
	// (XmlTvListingsProvider's _maxCacheAge), so a changed guide is only seen by a refresh after that.
	jellyfinGuideCopyAge = time.Hour
	sleepMin             = 60 // seconds
	sleepRange           = 241
)

type config struct {
	listenAddr      string
	outputFile      string // Jellyfin's M3U; guide.xml is written next to it
	stateDir        string // run counters and the cached full guide
	channelBaseURL  string // how Jellyfin reaches this proxy: <base><slug>
	aceserveBaseURL string
	guideSourceURL  string        // full XMLTV guides, comma-separated, cut down to our channels; empty skips it
	sourceLists     [][]string    // each list's routes, tried in order
	sourceFile      string        // where sourceLists is read from on every pass, when given as a file
	keepDropped     time.Duration // how long what the list dropped is kept; 0 trusts the list as it is
	pushgatewayURL  string
	jellyfinURL     string
	jellyfinAPIKey  string
	// Sharing (see access.go): off unless the team domain, app AUD tag, account and token are all set.
	accessTeamDomain    string
	accessAUD           string
	accessJoinAUD       string // the app that lets anyone sign in on /join; empty: no invite links
	cloudflareAccountID string
	cloudflareAPIToken  string
	admins              string // comma-separated emails that may change who has access
}

func loadConfig() config {
	// A path is a file of lists, reread on every pass so an edit needs no restart.
	raw, file := mustEnv("SOURCE_URLS"), ""
	var lists [][]string
	var err error
	if strings.HasPrefix(raw, "/") {
		file = raw
		lists, err = readSourceFile(file)
	} else {
		lists, err = parseSourceLists(raw)
	}
	if err != nil {
		log.Fatalf("SOURCE_URLS: %v", err)
	}
	return config{
		listenAddr:      getenv("LISTEN_ADDR", ":6879"),
		outputFile:      getenv("OUTPUT_FILE", "/data/channels_ace.m3u"),
		stateDir:        getenv("STATE_DIR", "/state"),
		channelBaseURL:  getenv("CHANNEL_BASE_URL", "http://bifrost:6879/channel/"),
		aceserveBaseURL: getenv("ACESERVE_BASE_URL", "http://aceserve:6878"),
		guideSourceURL:  os.Getenv("GUIDE_SOURCE_URL"),
		sourceLists:     lists,
		sourceFile:      file,
		keepDropped:     getduration("KEEP_DROPPED", keepDroppedFor),
		pushgatewayURL:  os.Getenv("PUSHGATEWAY_URL"),
		jellyfinURL:     mustEnv("JELLYFIN_URL"),
		jellyfinAPIKey:  mustEnv("JELLYFIN_API_KEY"),

		accessTeamDomain:    os.Getenv("ACCESS_TEAM_DOMAIN"),
		accessAUD:           os.Getenv("ACCESS_AUD"),
		accessJoinAUD:       os.Getenv("ACCESS_JOIN_AUD"),
		cloudflareAccountID: os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
		cloudflareAPIToken:  os.Getenv("CLOUDFLARE_API_TOKEN"),
		admins:              os.Getenv("BIFROST_ADMINS"),
	}
}

// parseSourceLists reads one list as comma-separated routes, or several as a JSON array of
// {"url", "backup": [...]}.
func parseSourceLists(raw string) ([][]string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") {
		var routes []string
		for _, u := range strings.Split(raw, ",") {
			if u = strings.TrimSpace(u); u != "" {
				routes = append(routes, u)
			}
		}
		if len(routes) == 0 {
			return nil, fmt.Errorf("no URL")
		}
		return [][]string{routes}, nil
	}
	var entries []struct {
		URL    string   `json:"url"`
		Backup []string `json:"backup"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	var lists [][]string
	for i, e := range entries {
		if e.URL == "" {
			return nil, fmt.Errorf("list %d has no url", i+1)
		}
		lists = append(lists, append([]string{e.URL}, e.Backup...))
	}
	if len(lists) == 0 {
		return nil, fmt.Errorf("no list")
	}
	return lists, nil
}

// readSourceFile skips lines starting with //, so a list can be left in the file switched off.
func readSourceFile(path string) ([][]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			kept = append(kept, line)
		}
	}
	return parseSourceLists(strings.Join(kept, "\n"))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}

type state struct {
	successChanges        int
	successNoChanges      int
	errors                int
	totalRuns             int
	jellyfinLastRefreshTS int64
}

type channel struct {
	name    string
	channel string // base name without quality suffix
	group   string
	hash    string
	extinf  string
	list    int // which of the configured lists it came from
}

func loadState(stateFile string) state {
	f, err := os.Open(stateFile)
	if err != nil {
		return state{}
	}
	defer f.Close()

	var s state
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var n int
		line := scanner.Text()
		var n64 int64
		if cnt, _ := fmt.Sscanf(line, "SUCCESS_CHANGES=%d", &n); cnt == 1 {
			s.successChanges = n
		} else if cnt, _ := fmt.Sscanf(line, "SUCCESS_NO_CHANGES=%d", &n); cnt == 1 {
			s.successNoChanges = n
		} else if cnt, _ := fmt.Sscanf(line, "ERRORS=%d", &n); cnt == 1 {
			s.errors = n
		} else if cnt, _ := fmt.Sscanf(line, "TOTAL_RUNS=%d", &n); cnt == 1 {
			s.totalRuns = n
		} else if cnt, _ := fmt.Sscanf(line, "JELLYFIN_LAST_REFRESH=%d", &n64); cnt == 1 {
			s.jellyfinLastRefreshTS = n64
		}
	}
	return s
}

func saveState(stateFile string, s state) {
	content := fmt.Sprintf(
		"SUCCESS_CHANGES=%d\nSUCCESS_NO_CHANGES=%d\nERRORS=%d\nTOTAL_RUNS=%d\nJELLYFIN_LAST_REFRESH=%d\n",
		s.successChanges, s.successNoChanges, s.errors, s.totalRuns, s.jellyfinLastRefreshTS,
	)
	if err := os.WriteFile(stateFile, []byte(content), 0644); err != nil {
		log.Printf("warning: failed to save state: %v", err)
	}
}

func downloadSource(rawURL string) ([]byte, int, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return body, resp.StatusCode, err
}

func parseAndDedup(combined []byte) ([]channel, string) {
	scanner := bufio.NewScanner(bytes.NewReader(combined))
	seen := make(map[string]bool)
	var channels []channel

	var urlTVG string
	var extinf string

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "#EXTM3U"):
			if urlTVG == "" {
				urlTVG = extractAttr(line, "url-tvg")
			}
		case strings.HasPrefix(line, "#EXTINF"):
			extinf = line
		case strings.HasPrefix(line, "acestream://"):
			hash := strings.TrimPrefix(line, "acestream://")
			if seen[hash] || extinf == "" {
				extinf = ""
				continue
			}
			seen[hash] = true
			name := channelName(extinf)
			channels = append(channels, channel{name: name, channel: strings.TrimSpace(qualitySuffix.ReplaceAllString(name, "")),
				group: extractAttr(extinf, "group-title"), hash: hash, extinf: extinf})
			extinf = ""
		}
	}
	return channels, m3uHeader(urlTVG)
}

func m3uHeader(urlTVG string) string {
	header := "#EXTM3U"
	if urlTVG != "" {
		header += fmt.Sprintf(` url-tvg="%s"`, urlTVG)
	}
	return header
}

func extractAttr(line, attr string) string {
	prefix := attr + `="`
	idx := strings.Index(line, prefix)
	if idx == -1 {
		return ""
	}
	rest := line[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}

func channelName(extinf string) string {
	if idx := strings.LastIndex(extinf, ","); idx != -1 {
		return strings.TrimSpace(extinf[idx+1:])
	}
	return extinf
}

// triggerJellyfinRefresh runs "Refresh Guide", the task that rereads the M3U tuner. Looked up by
// its Key: the id is an MD5 of the class name, and a hard-coded one ran "Refresh Channels" instead.
func triggerJellyfinRefresh(jellyfinURL, apiKey string) int {
	client := &http.Client{Timeout: 10 * time.Second}
	call := func(method, path string) (*http.Response, error) {
		req, err := http.NewRequest(method, jellyfinURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Emby-Token", apiKey)
		return client.Do(req)
	}

	resp, err := call("GET", "/ScheduledTasks")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode
	}
	var tasks []struct{ Id, Key string }
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return 0
	}
	for _, task := range tasks {
		if task.Key != "RefreshGuide" {
			continue
		}
		run, err := call("POST", "/ScheduledTasks/Running/"+task.Id)
		if err != nil {
			return 0
		}
		run.Body.Close()
		return run.StatusCode
	}
	log.Printf("Jellyfin has no RefreshGuide task")
	return http.StatusNotFound
}

func prometheusLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func hostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Host
}

// pushMetrics uses PUT so stale channel metrics are removed when channels disappear.
func pushMetrics(pushURL string, s state, channels []channel, jellyfinCode int,
	sourceHTTPCodes map[string]int) {
	if pushURL == "" {
		return
	}

	var buf bytes.Buffer
	w := func(format string, a ...any) { fmt.Fprintf(&buf, format, a...) }

	w("# HELP acestream_run_total Total executions\n# TYPE acestream_run_total counter\nacestream_run_total %d\n", s.totalRuns)
	w("# HELP acestream_updates_with_changes_total Updates with changes\n# TYPE acestream_updates_with_changes_total counter\nacestream_updates_with_changes_total %d\n", s.successChanges)
	w("# HELP acestream_updates_no_changes_total Executions without changes\n# TYPE acestream_updates_no_changes_total counter\nacestream_updates_no_changes_total %d\n", s.successNoChanges)
	w("# HELP acestream_update_errors_total Source downloads that failed (each run tries up to three routes to the list)\n# TYPE acestream_update_errors_total counter\nacestream_update_errors_total %d\n", s.errors)
	w("# HELP acestream_last_run_timestamp Last run timestamp\n# TYPE acestream_last_run_timestamp gauge\nacestream_last_run_timestamp %d\n", time.Now().Unix())
	w("# HELP acestream_unique_channels Current unique channel count\n# TYPE acestream_unique_channels gauge\nacestream_unique_channels %d\n", len(channels))
	w("# HELP acestream_jellyfin_refresh_http_code HTTP code from Jellyfin refresh API (0=not called)\n# TYPE acestream_jellyfin_refresh_http_code gauge\nacestream_jellyfin_refresh_http_code %d\n", jellyfinCode)
	w("# HELP acestream_jellyfin_last_refresh_timestamp Unix timestamp of last successful Jellyfin refresh\n# TYPE acestream_jellyfin_last_refresh_timestamp gauge\nacestream_jellyfin_last_refresh_timestamp %d\n", s.jellyfinLastRefreshTS)

	w("# HELP acestream_source_http_code HTTP response code per source URL (0=failed)\n# TYPE acestream_source_http_code gauge\n")
	for host, code := range sourceHTTPCodes {
		w("acestream_source_http_code{url=\"%s\"} %d\n", prometheusLabel(host), code)
	}

	req, err := http.NewRequest("PUT", pushURL+"/metrics/job/acestream_updater", &buf)
	if err != nil {
		log.Printf("warning: metrics request build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("warning: push metrics failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("warning: pushgateway returned %d", resp.StatusCode)
	}
}

type lister struct {
	cfg     config
	catalog *catalog
	hub     *hub

	memory     *memory     // what the list dropped, kept until it fails
	refreshDue time.Time   // when Jellyfin is due a refresh; zero when nothing is waiting
	guideDirty bool        // guide.xml changed since the last refresh
	guideAgain time.Time   // when a refresh is due for Jellyfin to see that change; zero when none
	guideIDs   []string    // the guide ids guide.xml was last cut down for
	guideIndex *guideIndex // the guides' channels by name, reread when a guide is downloaded again
}

func (l *lister) run() {
	cfg := l.cfg
	stateFile := filepath.Join(cfg.stateDir, "metrics.env")
	s := loadState(stateFile)
	s.totalRuns++
	log.Printf("Starting execution #%d", s.totalRuns)

	sourceHTTPCodes := make(map[string]int)
	var lists [][]byte
	if cfg.sourceFile != "" {
		if lists, err := readSourceFile(cfg.sourceFile); err != nil {
			log.Printf("warning: %s: %v, keeping the lists read before", cfg.sourceFile, err)
		} else {
			l.cfg.sourceLists, cfg.sourceLists = lists, lists
		}
	}
	for _, routes := range cfg.sourceLists {
		if data := l.readList(routes, &s, sourceHTTPCodes); data != nil {
			lists = append(lists, data)
		}
	}

	if len(lists) == 0 {
		log.Printf("error: all sources failed")
		l.board().listRead(0, 0, false, "all sources failed")
		saveState(stateFile, s)
		pushMetrics(cfg.pushgatewayURL, s, nil, 0, sourceHTTPCodes)
		return
	}

	channels, header := mergeLists(lists)
	log.Printf("Channels: %d unique after dedup", len(channels))

	order, groups := groupChannels(channels)
	if l.memory == nil {
		l.memory = &memory{path: filepath.Join(cfg.stateDir, "remembered.json"), keepFor: cfg.keepDropped}
	}
	if cfg.keepDropped > 0 {
		order = l.memory.keep(order, groups, l.failed, time.Now())
	}
	order = l.keepWatched(order, groups)

	// The guides come first: a channel its list gave no guide id may get one from them by name.
	guides := guideSources(cfg.guideSourceURL, cfg.stateDir)
	var guideFiles []string
	fresh := false
	for _, src := range guides {
		got, err := fetchGuide(src.url, src.file, guideMaxAge)
		if err != nil {
			log.Printf("warning: guide download failed, keeping the last one: %s: %v", src.url, err)
		}
		fresh = fresh || got
		guideFiles = append(guideFiles, src.file)
	}
	if len(guides) > 0 && (fresh || l.guideIndex == nil) {
		if idx, err := loadGuideIndex(guideFiles); err != nil {
			log.Printf("warning: reading the guides' channels: %v", err)
		} else {
			l.guideIndex = idx
		}
	}
	matchGuideIDs(order, groups, l.guideIndex)

	l.catalog.set(order, groups)
	newM3U := stableM3U(header, cfg.channelBaseURL, order, groups)

	changed := false
	if len(guides) > 0 {
		ids := tvgIDs(groups)
		if fresh || !sameIDs(ids, l.guideIDs) {
			guide := filepath.Join(filepath.Dir(cfg.outputFile), "guide.xml")
			var owner map[string]int
			if l.guideIndex != nil {
				owner = l.guideIndex.owner
			}
			if guideChanged, err := trimGuide(guideFiles, guide, ids, owner); err != nil {
				log.Printf("error trimming the guide: %v", err)
			} else {
				l.guideIDs = ids
				if err := l.hub.schedule.load(guide); err != nil {
					log.Printf("warning: reading the guide back: %v", err)
				}
				if st, err := os.Stat(guide); err == nil {
					l.board().guideCut(len(ids), st.Size())
				}
				if guideChanged {
					log.Printf("Guide: cut down to %d channels", len(ids))
					changed = true
					l.guideDirty = true
				}
			}
		}
	}

	jellyfinCode := 0
	existing, err := os.ReadFile(cfg.outputFile)
	if err != nil || string(existing) != newM3U {
		if err := os.WriteFile(cfg.outputFile, []byte(newM3U), 0644); err != nil {
			log.Printf("error writing output: %v", err)
		} else {
			log.Printf("Changes applied: %d channels from %d sources", len(order), len(channels))
			s.successChanges++
			changed = true
		}
	} else {
		log.Printf("No changes detected.")
		s.successNoChanges++
	}

	sources := 0
	for _, slug := range order {
		sources += len(groups[slug].IDs)
	}
	l.board().listRead(len(order), sources, changed, "")
	if changed {
		l.refreshDue = time.Now().Add(refreshDebounce)
		log.Printf("Jellyfin refresh due after %s without further changes", refreshDebounce)
	}
	if l.refreshNow(time.Now()) {
		jellyfinCode = triggerJellyfinRefresh(cfg.jellyfinURL, cfg.jellyfinAPIKey)
		log.Printf("Jellyfin refresh: %d", jellyfinCode)
		if jellyfinCode == 204 {
			s.jellyfinLastRefreshTS = time.Now().Unix()
			l.refreshedAt(time.Now())
		}
	}

	l.board().refreshed(jellyfinCode, l.refreshDue)
	saveState(stateFile, s)
	pushMetrics(cfg.pushgatewayURL, s, channels, jellyfinCode, sourceHTTPCodes)
	log.Printf("Done.")
}

// readList downloads one list by the first of its routes that answers: they lead to the same list,
// and a gateway with a stale record serves an older copy, so merging them would bring dropped
// channels back. When none answers, the last copy read stands in, so one list down drops nothing.
func (l *lister) readList(routes []string, s *state, codes map[string]int) []byte {
	sum := sha256.Sum256([]byte(routes[0]))
	cache := filepath.Join(l.cfg.stateDir, "lists", fmt.Sprintf("%x.m3u", sum[:6]))
	for _, route := range routes {
		log.Printf("Downloading: %s", route)
		data, code, err := downloadSource(route)
		codes[hostFromURL(route)] = code
		if err != nil || code != 200 {
			log.Printf("  warning: failed (HTTP %d): %v", code, err)
			s.errors++
			continue
		}
		log.Printf("  ok (%d)", code)
		if err := os.MkdirAll(filepath.Dir(cache), 0o755); err == nil {
			os.WriteFile(cache, data, 0o644)
		}
		return data
	}
	data, err := os.ReadFile(cache)
	if err != nil {
		return nil
	}
	log.Printf("  every route to %s failed, using its last copy", routes[0])
	return data
}

func (l *lister) failed(id string) bool {
	if l.hub == nil {
		return false
	}
	l.hub.mu.Lock()
	defer l.hub.mu.Unlock()
	f := l.hub.facts[id]
	return f != nil && f.Outcome == "failed"
}

// board is where the status page reads from; a lister without a hub (tests) writes to a throwaway.
func (l *lister) board() *statusBoard {
	if l.hub == nil {
		return &statusBoard{}
	}
	return l.hub.status
}

// A channel someone watches is put back when the list skipped it, or Jellyfin would delete it under the viewer.
func (l *lister) keepWatched(order []string, groups map[string]*channelGroup) []string {
	if l.hub == nil {
		return order
	}
	for slug := range l.hub.active() {
		if groups[slug] != nil {
			continue
		}
		if prev := l.catalog.group(slug); prev != nil {
			log.Printf("Keeping %s while it is being watched, though the list dropped it", prev.Name)
			groups[slug] = prev
			order = append(order, slug)
		}
	}
	return order
}

// refreshNow says whether Jellyfin is due a refresh, counting the one a changed guide needs once
// Jellyfin's copy of it has expired.
func (l *lister) refreshNow(now time.Time) bool {
	if !l.guideAgain.IsZero() && !now.Before(l.guideAgain) && l.refreshDue.IsZero() {
		log.Printf("Jellyfin refresh due: its copy of the changed guide has expired")
		l.refreshDue, l.guideAgain = now, time.Time{}
	}
	return !l.refreshDue.IsZero() && !now.Before(l.refreshDue)
}

func (l *lister) refreshedAt(now time.Time) {
	l.refreshDue = time.Time{}
	if l.guideDirty {
		l.guideDirty, l.guideAgain = false, now.Add(jellyfinGuideCopyAge+time.Minute)
	}
}
