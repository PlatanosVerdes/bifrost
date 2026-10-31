package catalog

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
	"sync"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/guide"
	"github.com/PlatanosVerdes/bifrost/internal/httpx"
	"github.com/PlatanosVerdes/bifrost/internal/m3u"
)

// qualitySuffix strips resolution and source markers: "Channel+ C 1080p **" -> "Channel+ C".
var qualitySuffix = regexp.MustCompile(`\s+(?:(?:720p|1080p)\s+)?\*+$`)

const (
	guideMaxAge     = 24 * time.Hour
	refreshDebounce = 2 * time.Minute // a channel that drops out and back in costs one refresh, not two
	// Jellyfin rereads its copy of the XMLTV file, not the file, until the copy is an hour old
	// (XmlTvListingsProvider's _maxCacheAge), so a changed guide is only seen by a refresh after that.
	jellyfinGuideCopyAge = time.Hour
	SleepMin             = 60 // seconds
	SleepRange           = 241
)

// Config is what the list loop needs from the environment.
type Config struct {
	OutputFile     string        // Jellyfin's M3U; guide.xml is written next to it
	StateDir       string        // run counters and the cached full guide
	ChannelBaseURL string        // how Jellyfin reaches this proxy: <base><slug>
	GuideSourceURL string        // full XMLTV guides, comma-separated, cut down to our channels; empty skips it
	SourceLists    [][]string    // each list's routes, tried in order
	SourceFile     string        // where sourceLists is read from on every pass, when given as a file
	KeepDropped    time.Duration // how long what the list dropped is kept; 0 trusts the list as it is
	PushgatewayURL string
	JellyfinURL    string
	JellyfinAPIKey string
}

// ParseSourceLists reads one list as comma-separated routes, or several as a JSON array of
// {"url", "backup": [...]}.
func ParseSourceLists(raw string) ([][]string, error) {
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

// ReadSourceFile skips lines starting with //, so a list can be left in the file switched off.
func ReadSourceFile(path string) ([][]string, error) {
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
	return ParseSourceLists(strings.Join(kept, "\n"))
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
				urlTVG = m3u.Attr(line, "url-tvg")
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
				group: m3u.Attr(extinf, "group-title"), hash: hash, extinf: extinf})
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
		w("acestream_source_http_code{url=\"%s\"} %d\n", httpx.PrometheusLabel(host), code)
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

type Lister struct {
	Config   Config
	Catalog  *Catalog
	Schedule *Schedule // what airs now, reread from each guide.xml cut
	Status   *Board    // nil in tests
	Failed   func(id string) bool
	Watching func() map[string]bool // slugs being played, kept even when the list drops them

	memory     *memory      // what the list dropped, kept until it fails
	refreshDue time.Time    // when Jellyfin is due a refresh; zero when nothing is waiting
	guideDirty bool         // guide.xml changed since the last refresh
	guideAgain time.Time    // when a refresh is due for Jellyfin to see that change; zero when none
	guideIDs   []string     // the guide ids guide.xml was last cut down for
	guideIndex *guide.Index // the guides' channels by name, reread when a guide is downloaded again
}

func (l *Lister) Run() {
	cfg := l.Config
	stateFile := filepath.Join(cfg.StateDir, "metrics.env")
	s := loadState(stateFile)
	s.totalRuns++
	log.Printf("Starting execution #%d", s.totalRuns)

	sourceHTTPCodes := make(map[string]int)
	var lists [][]byte
	if cfg.SourceFile != "" {
		if lists, err := ReadSourceFile(cfg.SourceFile); err != nil {
			log.Printf("warning: %s: %v, keeping the lists read before", cfg.SourceFile, err)
		} else {
			l.Config.SourceLists, cfg.SourceLists = lists, lists
		}
	}
	for _, routes := range cfg.SourceLists {
		if data := l.readList(routes, &s, sourceHTTPCodes); data != nil {
			lists = append(lists, data)
		}
	}

	if len(lists) == 0 {
		log.Printf("error: all sources failed")
		l.board().listRead(0, 0, false, "all sources failed")
		saveState(stateFile, s)
		pushMetrics(cfg.PushgatewayURL, s, nil, 0, sourceHTTPCodes)
		return
	}

	channels, header := mergeLists(lists)
	log.Printf("Channels: %d unique after dedup", len(channels))

	order, groups := groupChannels(channels)
	if l.memory == nil {
		l.memory = &memory{path: filepath.Join(cfg.StateDir, "remembered.json"), keepFor: cfg.KeepDropped}
	}
	if cfg.KeepDropped > 0 {
		order = l.memory.keep(order, groups, l.hasFailed, time.Now())
	}
	order = l.keepWatched(order, groups)

	// The guides come first: a channel its list gave no guide id may get one from them by name.
	guides := guide.Sources(cfg.GuideSourceURL, cfg.StateDir)
	var guideFiles []string
	fresh := false
	for _, src := range guides {
		got, err := guide.Fetch(src.URL, src.File, guideMaxAge)
		if err != nil {
			log.Printf("warning: guide download failed, keeping the last one: %s: %v", src.URL, err)
		}
		fresh = fresh || got
		guideFiles = append(guideFiles, src.File)
	}
	if len(guides) > 0 && (fresh || l.guideIndex == nil) {
		if idx, err := guide.LoadIndex(guideFiles); err != nil {
			log.Printf("warning: reading the guides' channels: %v", err)
		} else {
			l.guideIndex = idx
		}
	}
	matchGuideIDs(order, groups, l.guideIndex)

	l.Catalog.Set(order, groups)
	newM3U := stableM3U(header, cfg.ChannelBaseURL, order, groups)

	changed := false
	if len(guides) > 0 {
		ids := tvgIDs(groups)
		if fresh || !guide.SameIDs(ids, l.guideIDs) {
			guideFile := filepath.Join(filepath.Dir(cfg.OutputFile), "guide.xml")
			var owner map[string]int
			if l.guideIndex != nil {
				owner = l.guideIndex.Owner
			}
			if guideChanged, err := guide.Trim(guideFiles, guideFile, ids, owner); err != nil {
				log.Printf("error trimming the guide: %v", err)
			} else {
				l.guideIDs = ids
				if err := l.Schedule.Load(guideFile); err != nil {
					log.Printf("warning: reading the guide back: %v", err)
				}
				if st, err := os.Stat(guideFile); err == nil {
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
	existing, err := os.ReadFile(cfg.OutputFile)
	if err != nil || string(existing) != newM3U {
		if err := os.WriteFile(cfg.OutputFile, []byte(newM3U), 0644); err != nil {
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
		jellyfinCode = triggerJellyfinRefresh(cfg.JellyfinURL, cfg.JellyfinAPIKey)
		log.Printf("Jellyfin refresh: %d", jellyfinCode)
		if jellyfinCode == 204 {
			s.jellyfinLastRefreshTS = time.Now().Unix()
			l.refreshedAt(time.Now())
		}
	}

	l.board().refreshed(jellyfinCode, l.refreshDue)
	saveState(stateFile, s)
	pushMetrics(cfg.PushgatewayURL, s, channels, jellyfinCode, sourceHTTPCodes)
	log.Printf("Done.")
}

// readList downloads one list by the first of its routes that answers: they lead to the same list,
// and a gateway with a stale record serves an older copy, so merging them would bring dropped
// channels back. When none answers, the last copy read stands in, so one list down drops nothing.
func (l *Lister) readList(routes []string, s *state, codes map[string]int) []byte {
	sum := sha256.Sum256([]byte(routes[0]))
	cache := filepath.Join(l.Config.StateDir, "lists", fmt.Sprintf("%x.m3u", sum[:6]))
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

// board is where the status page reads from; a lister without one (tests) writes to a throwaway.
func (l *Lister) board() *Board {
	if l.Status == nil {
		return &Board{}
	}
	return l.Status
}

func (l *Lister) hasFailed(id string) bool {
	return l.Failed != nil && l.Failed(id)
}

// A channel someone watches is put back when the list skipped it, or Jellyfin would delete it under the viewer.
func (l *Lister) keepWatched(order []string, groups map[string]*Group) []string {
	if l.Watching == nil {
		return order
	}
	for slug := range l.Watching() {
		if groups[slug] != nil {
			continue
		}
		if prev := l.Catalog.Group(slug); prev != nil {
			log.Printf("Keeping %s while it is being watched, though the list dropped it", prev.Name)
			groups[slug] = prev
			order = append(order, slug)
		}
	}
	return order
}

// refreshNow says whether Jellyfin is due a refresh, counting the one a changed guide needs once
// Jellyfin's copy of it has expired.
func (l *Lister) refreshNow(now time.Time) bool {
	if !l.guideAgain.IsZero() && !now.Before(l.guideAgain) && l.refreshDue.IsZero() {
		log.Printf("Jellyfin refresh due: its copy of the changed guide has expired")
		l.refreshDue, l.guideAgain = now, time.Time{}
	}
	return !l.refreshDue.IsZero() && !now.Before(l.refreshDue)
}

func (l *Lister) refreshedAt(now time.Time) {
	l.refreshDue = time.Time{}
	if l.guideDirty {
		l.guideDirty, l.guideAgain = false, now.Add(jellyfinGuideCopyAge+time.Minute)
	}
}

type ListStatus struct {
	Channels   int       `json:"channels"`
	Sources    int       `json:"sources"`
	LastRead   time.Time `json:"last_read"`
	LastChange time.Time `json:"last_change"`
	Error      string    `json:"error,omitempty"`
}

type GuideStatus struct {
	Channels int       `json:"channels"`
	Bytes    int64     `json:"bytes"`
	Updated  time.Time `json:"updated"`
}

type JellyfinStatus struct {
	LastRefresh time.Time  `json:"last_refresh"`
	LastCode    int        `json:"last_code"`
	RefreshDue  *time.Time `json:"refresh_due,omitempty"`
}

// Board is what the list loop last did, for the status page.
type Board struct {
	mu       sync.Mutex
	list     ListStatus
	jellyfin JellyfinStatus
	guide    GuideStatus
}

func (b *Board) listRead(channels, sources int, changed bool, err string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.list.LastRead, b.list.Error = now, err
	if err == "" {
		b.list.Channels, b.list.Sources = channels, sources
	}
	if changed {
		b.list.LastChange = now
	}
}

func (b *Board) guideCut(channels int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.guide = GuideStatus{Channels: channels, Bytes: bytes, Updated: time.Now()}
}

func (b *Board) refreshed(code int, due time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if code != 0 {
		b.jellyfin.LastRefresh, b.jellyfin.LastCode = time.Now(), code
	}
	b.jellyfin.RefreshDue = nil
	if !due.IsZero() {
		d := due
		b.jellyfin.RefreshDue = &d
	}
}

func (b *Board) Snapshot() (ListStatus, JellyfinStatus, GuideStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.list, b.jellyfin, b.guide
}
