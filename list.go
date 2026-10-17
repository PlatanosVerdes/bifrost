package main

import (
	"bufio"
	"bytes"
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

// qualitySuffix strips resolution and source markers from channel names.
// e.g. "M+ Liga de Campeones 1080p **" → "M+ Liga de Campeones"
var qualitySuffix = regexp.MustCompile(`\s+(?:(?:720p|1080p)\s+)?\*+$`)

const (
	guideMaxAge     = 24 * time.Hour
	refreshDebounce = 2 * time.Minute // a channel that drops out and back in costs one refresh, not two
	sleepMin        = 60              // seconds
	sleepRange      = 241             // rand(0..240) + sleepMin = 60..300s
)

type config struct {
	listenAddr      string
	outputFile      string // Jellyfin's M3U; guide.xml is written next to it
	stateDir        string // run counters and the cached full guide
	channelBaseURL  string // how Jellyfin reaches this proxy: <base><slug>
	aceserveBaseURL string
	guideSourceURL  string // full XMLTV guide, cut down to our channels; empty skips it
	sourceURLs      []string
	pushgatewayURL  string
	jellyfinURL     string
	jellyfinAPIKey  string
}

func loadConfig() config {
	var urls []string
	for _, u := range strings.Split(mustEnv("SOURCE_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return config{
		listenAddr:      getenv("LISTEN_ADDR", ":6879"),
		outputFile:      getenv("OUTPUT_FILE", "/data/channels_ace.m3u"),
		stateDir:        getenv("STATE_DIR", "/state"),
		channelBaseURL:  getenv("CHANNEL_BASE_URL", "http://bifrost:6879/channel/"),
		aceserveBaseURL: getenv("ACESERVE_BASE_URL", "http://aceserve:6878"),
		guideSourceURL:  os.Getenv("GUIDE_SOURCE_URL"),
		sourceURLs:      urls,
		pushgatewayURL:  os.Getenv("PUSHGATEWAY_URL"),
		jellyfinURL:     mustEnv("JELLYFIN_URL"),
		jellyfinAPIKey:  mustEnv("JELLYFIN_API_KEY"),
	}
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
	jellyfinLastRefreshTS int64 // unix timestamp of last successful Jellyfin refresh
}

type channel struct {
	name    string
	channel string // base name without quality suffix (for aggregation)
	group   string // group-title from EXTINF
	hash    string
	extinf  string
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

// parseAndDedup parses the combined raw M3U bytes, deduplicates by acestream hash,
// and rewrites acestream:// URLs to the aceserve HTTP URL.
// It preserves the url-tvg attribute from the first source header so Jellyfin
// can load the EPG guide automatically.
// aceserveURL is used for health probes; streamBaseURL is written into the M3U for Jellyfin.
func parseAndDedup(combined []byte) ([]channel, string) {
	scanner := bufio.NewScanner(bytes.NewReader(combined))
	seen := make(map[string]bool)
	var channels []channel

	var urlTVG string // extracted from the first #EXTM3U line in the sources
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

// extractAttr extracts the value of a quoted attribute from an M3U tag line.
// e.g. extractAttr(`#EXTM3U url-tvg="http://foo.xml"`, "url-tvg") → "http://foo.xml"
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

	var buf bytes.Buffer
	w := func(format string, a ...any) { fmt.Fprintf(&buf, format, a...) }

	w("# HELP acestream_run_total Total executions\n# TYPE acestream_run_total counter\nacestream_run_total %d\n", s.totalRuns)
	w("# HELP acestream_updates_with_changes Updates with changes\n# TYPE acestream_updates_with_changes counter\nacestream_updates_with_changes %d\n", s.successChanges)
	w("# HELP acestream_updates_no_changes Executions without changes\n# TYPE acestream_updates_no_changes counter\nacestream_updates_no_changes %d\n", s.successNoChanges)
	w("# HELP acestream_update_errors Total errors\n# TYPE acestream_update_errors counter\nacestream_update_errors %d\n", s.errors)
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

// lister is the list half: one pass reads the list, rebuilds the catalog, and rewrites Jellyfin's
// playlist and guide when they really changed.
type lister struct {
	cfg     config
	catalog *catalog
	hub     *hub

	refreshDue time.Time // when Jellyfin is due a refresh; zero when nothing is waiting
	guideIDs   []string  // the guide ids guide.xml was last cut down for
}

func (l *lister) run() {
	cfg := l.cfg
	stateFile := filepath.Join(cfg.stateDir, "metrics.env")
	s := loadState(stateFile)
	s.totalRuns++
	log.Printf("Starting execution #%d", s.totalRuns)

	var combined bytes.Buffer
	sourceHTTPCodes := make(map[string]int)

	// The sources are three routes to one IPNS key, and a gateway with a stale record resolves
	// it to an older CID, so merging them puts already dropped channels back. First answer wins.
	for _, sourceURL := range cfg.sourceURLs {
		log.Printf("Downloading: %s", sourceURL)
		data, code, err := downloadSource(sourceURL)
		sourceHTTPCodes[hostFromURL(sourceURL)] = code

		if err != nil || code != 200 {
			log.Printf("  warning: failed (HTTP %d): %v", code, err)
			s.errors++
			continue
		}
		combined.Write(data)
		// Sources do not always end in a newline, and without this the last URL of one
		// would be glued to the #EXTM3U of the next, escaping the dedup as a broken entry.
		combined.WriteByte('\n')
		log.Printf("  ok (%d)", code)
		break
	}

	if combined.Len() == 0 {
		log.Printf("error: all sources failed")
		saveState(stateFile, s)
		pushMetrics(cfg.pushgatewayURL, s, nil, 0, sourceHTTPCodes)
		return
	}

	channels, header := parseAndDedup(combined.Bytes())
	log.Printf("Channels: %d unique after dedup", len(channels))

	order, groups := groupChannels(channels)
	order = l.keepWatched(order, groups)
	l.catalog.set(order, groups)
	newM3U := stableM3U(header, cfg.channelBaseURL, order, groups)

	changed := false
	if cfg.guideSourceURL != "" {
		cache := filepath.Join(cfg.stateDir, "guide-full.xml")
		fresh, err := fetchGuide(cfg.guideSourceURL, cache, guideMaxAge)
		if err != nil {
			log.Printf("warning: guide download failed, keeping the last one: %v", err)
		}
		ids := tvgIDs(groups)
		if fresh || !sameIDs(ids, l.guideIDs) {
			guide := filepath.Join(filepath.Dir(cfg.outputFile), "guide.xml")
			if guideChanged, err := trimGuide(cache, guide, ids); err != nil {
				log.Printf("error trimming the guide: %v", err)
			} else {
				l.guideIDs = ids
				if guideChanged {
					log.Printf("Guide: cut down to %d channels", len(ids))
					changed = true
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

	if changed {
		l.refreshDue = time.Now().Add(refreshDebounce)
		log.Printf("Jellyfin refresh due after %s without further changes", refreshDebounce)
	}
	if !l.refreshDue.IsZero() && !time.Now().Before(l.refreshDue) {
		jellyfinCode = triggerJellyfinRefresh(cfg.jellyfinURL, cfg.jellyfinAPIKey)
		log.Printf("Jellyfin refresh: %d", jellyfinCode)
		if jellyfinCode == 204 {
			s.jellyfinLastRefreshTS = time.Now().Unix()
			l.refreshDue = time.Time{}
		}
	}

	saveState(stateFile, s)
	pushMetrics(cfg.pushgatewayURL, s, channels, jellyfinCode, sourceHTTPCodes)
	log.Printf("Done.")
}

// keepWatched puts back a channel someone is watching when the list skipped it this pass: dropping
// it would make Jellyfin delete the channel under the viewer. It leaves once nobody is on it.
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
