package main

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const recentOpens = 20

type listStatus struct {
	Channels   int       `json:"channels"`
	Sources    int       `json:"sources"`
	LastRead   time.Time `json:"last_read"`
	LastChange time.Time `json:"last_change"`
	Error      string    `json:"error,omitempty"`
}

type guideStatus struct {
	Channels int       `json:"channels"`
	Bytes    int64     `json:"bytes"`
	Updated  time.Time `json:"updated"`
}

type jellyfinStatus struct {
	LastRefresh time.Time  `json:"last_refresh"`
	LastCode    int        `json:"last_code"`
	RefreshDue  *time.Time `json:"refresh_due,omitempty"`
}

type openRecord struct {
	Channel string    `json:"channel"`
	Slug    string    `json:"slug,omitempty"`
	Source  string    `json:"source,omitempty"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // playing, gave_up, abandoned
	Seconds float64   `json:"seconds"`
	Device  string    `json:"device,omitempty"`
}

type streamStatus struct {
	Channel       string       `json:"channel"`
	Slug          string       `json:"slug,omitempty"`
	Source        string       `json:"source,omitempty"`
	Viewers       int          `json:"viewers"`
	Since         time.Time    `json:"since"`
	StartedAfterS float64      `json:"started_after_s"`
	Now           *airing      `json:"now,omitempty"`
	Next          *airing      `json:"next,omitempty"`
	Swarm         *swarmStatus `json:"swarm,omitempty"`
	// Probing is a second source being measured next to this one, to switch to if clearly better.
	Probing       bool          `json:"probing"`
	ProbingSource string        `json:"probing_source,omitempty"`
	Switches      int           `json:"switches"`
	Waiting       bool          `json:"waiting,omitempty"` // on the slate, no source has delivered yet
	LastSwitch    *switchRecord `json:"last_switch,omitempty"`
	LastProbe     *probeRecord  `json:"last_probe,omitempty"`

	channelSwitches int // since bifrost started, for the metric
	remuxRestarts   int // since bifrost started, for the metric
}

// statusBoard is written by both halves and read by the handlers.
type statusBoard struct {
	mu       sync.Mutex
	list     listStatus
	jellyfin jellyfinStatus
	guide    guideStatus
	recent   []openRecord
	last     map[string]openRecord // slug -> that channel's latest open, kept beyond the recent 20
}

func (b *statusBoard) listRead(channels, sources int, changed bool, err string) {
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

func (b *statusBoard) guideCut(channels int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.guide = guideStatus{Channels: channels, Bytes: bytes, Updated: time.Now()}
}

func (b *statusBoard) refreshed(code int, due time.Time) {
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

func (b *statusBoard) opened(r openRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append([]openRecord{r}, b.recent...)
	if len(b.recent) > recentOpens {
		b.recent = b.recent[:recentOpens]
	}
	if r.Slug != "" {
		if b.last == nil {
			b.last = map[string]openRecord{}
		}
		b.last[r.Slug] = r
	}
}

func (b *statusBoard) setDevice(at time.Time, device string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.recent {
		if b.recent[i].At.Equal(at) {
			b.recent[i].Device = device
		}
	}
	for slug, r := range b.last {
		if r.At.Equal(at) {
			r.Device = device
			b.last[slug] = r
		}
	}
}

// deviceChecks: Jellyfin probes the stream for a few seconds before its session names the channel.
var deviceChecks = []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second}

// A player that went to bifrost directly has no Jellyfin session and keeps no device.
func (h *hub) noteDevice(at time.Time, label string) {
	if h.jellyfinURL == "" {
		return
	}
	waited := time.Duration(0)
	for _, check := range deviceChecks {
		time.Sleep(check - waited)
		waited = check
		playing, err := jellyfinSessions(h.jellyfinURL, h.jellyfinKey)
		if err != nil {
			continue
		}
		for _, p := range playing {
			if strings.HasPrefix(label, p.Channel) {
				h.status.setDevice(at, p.who())
				log.Printf("[proxy] %s watched by %s", label, p.who())
				return
			}
		}
	}
}

func (b *statusBoard) lastOpen(slug string) *openRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.last[slug]
	if !ok {
		return nil
	}
	return &r
}

func (h *hub) streamsNow() []streamStatus {
	h.mu.Lock()
	streams := make([]*stream, 0, len(h.streams))
	for _, s := range h.streams {
		streams = append(streams, s)
	}
	h.mu.Unlock()

	h.mu.Lock()
	switches := maps.Clone(h.switches)
	restarts := maps.Clone(h.remuxRestarts)
	h.mu.Unlock()

	out := []streamStatus{}
	for _, s := range streams {
		s.mu.Lock()
		if !s.done && s.ok {
			out = append(out, streamStatus{Channel: s.label, Slug: s.slug, Source: s.source, Waiting: s.source == "",
				Viewers: len(s.viewers), Since: s.playingSince, StartedAfterS: s.startedAfterS, Swarm: s.swarm.status(),
				Probing: s.probing != "", ProbingSource: s.probing, Switches: s.switches, LastSwitch: s.lastSwitch,
				LastProbe: s.lastProbe, channelSwitches: switches[s.slug], remuxRestarts: restarts[s.slug]})
			out[len(out)-1].Now, out[len(out)-1].Next = h.onAir(s.slug)
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

func (h *hub) serveStatus(w http.ResponseWriter, r *http.Request) {
	h.status.mu.Lock()
	body := map[string]any{
		"version":  buildVersion,
		"name":     h.name,
		"guide":    h.status.guide,
		"list":     h.status.list,
		"jellyfin": h.status.jellyfin,
		"recent":   append([]openRecord{}, h.status.recent...),
	}
	h.status.mu.Unlock()
	body["streams"] = h.streamsNow()
	if h.jellyfinURL != "" {
		if playing, err := jellyfinSessions(h.jellyfinURL, h.jellyfinKey); err != nil {
			body["jellyfin_error"] = err.Error()
		} else {
			body["jellyfin_playing"] = playing
		}
	}
	writeJSON(w, body)
}

type channelInfo struct {
	Slug     string        `json:"slug"`
	Name     string        `json:"name"`
	Group    string        `json:"group"`
	Logo     string        `json:"logo"`
	TvgID    string        `json:"tvg_id"`
	IDs      []string      `json:"ids"` // the order the proxy would try them in
	Playing  *streamStatus `json:"playing,omitempty"`
	LastOpen *openRecord   `json:"last_open,omitempty"`
	Now      *airing       `json:"now,omitempty"`
	Next     *airing       `json:"next,omitempty"`
	Sources  []sourceFacts `json:"sources"` // the playlist's order, with what was last seen of each
	Blocked  []sourceFacts `json:"blocked,omitempty"`
}

// serveChannels is the catalog for players that go to the engine directly, the source that last produced
// video first.
func (h *hub) serveChannels(w http.ResponseWriter, r *http.Request) {
	h.table.mu.RLock()
	order := append([]string(nil), h.table.order...)
	groups := h.table.groups
	h.table.mu.RUnlock()

	playing := map[string]streamStatus{}
	for _, st := range h.streamsNow() {
		if st.Slug != "" {
			playing[st.Slug] = st
		}
	}
	out := make([]channelInfo, 0, len(order))
	for _, slug := range order {
		g := groups[slug]
		if g == nil {
			continue
		}
		ids := h.table.unblocked(slug, g.IDs)
		var blocked []string
		for _, id := range g.IDs {
			if !slices.Contains(ids, id) {
				blocked = append(blocked, id)
			}
		}
		out = append(out, channelInfo{Slug: slug, Name: g.Name, Group: extractAttr(g.extinf, "group-title"),
			Logo: extractAttr(g.extinf, "tvg-logo"), TvgID: extractAttr(g.extinf, "tvg-id"), IDs: h.ordered(slug, ids),
			LastOpen: h.status.lastOpen(slug), Sources: h.sourcesOf(ids), Blocked: h.sourcesOf(blocked)})
		for _, list := range [][]sourceFacts{out[len(out)-1].Sources, out[len(out)-1].Blocked} {
			for i := range list {
				list[i].Kept, list[i].ThumbAt = g.kept[list[i].ID], h.thumbs.at(list[i].ID)
			}
		}
		out[len(out)-1].Now, out[len(out)-1].Next = h.schedule.at(extractAttr(g.extinf, "tvg-id"), time.Now())
		if st, ok := playing[slug]; ok {
			out[len(out)-1].Playing = &st
		}
	}
	writeJSON(w, map[string]any{"channels": out})
}

func (h *hub) onAir(slug string) (now, next *airing) {
	g := h.table.group(slug)
	if g == nil {
		return nil, nil
	}
	return h.schedule.at(extractAttr(g.extinf, "tvg-id"), time.Now())
}

// recentlyFailed is how long a source that failed to open is tried after the others.
const recentlyFailed = 15 * time.Minute

// The source that last produced video goes first and one that failed lately last, so a dead first
// source does not cost every open its whole wait.
func (h *hub) ordered(slug string, ids []string) []string {
	h.mu.Lock()
	good := h.lastGood[slug]
	failed := map[string]bool{}
	for _, id := range ids {
		if f := h.facts[id]; f != nil && f.Outcome == "failed" && time.Since(f.SeenAt) < recentlyFailed {
			failed[id] = true
		}
	}
	h.mu.Unlock()
	rank := func(id string) int {
		switch {
		case failed[id]:
			return 2
		case id == good:
			return 0
		}
		return 1
	}
	out := slices.Clone(ids)
	slices.SortStableFunc(out, func(a, b string) int { return rank(a) - rank(b) })
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// jellyfinPlayback is how Jellyfin delivers a live channel. A re-encode on a Pi 4 cannot keep up with
// 1080p, so a channel that plays a few seconds and stalls shows up here first.
type jellyfinPlayback struct {
	Channel   string   `json:"channel"`
	Client    string   `json:"client"`
	Device    string   `json:"device"`
	User      string   `json:"user,omitempty"`
	Video     string   `json:"video"` // copy or transcode
	Audio     string   `json:"audio"`
	Reasons   []string `json:"reasons,omitempty"`
	Framerate float64  `json:"framerate,omitempty"`
	// Speed is ffmpeg's frame rate over the channel's: below 1 it falls behind live and stalls.
	Speed    float64 `json:"speed,omitempty"`
	Progress float64 `json:"completion,omitempty"`
}

// who names the viewer for the page and the log: the Jellyfin user, then the device and client.
func (p jellyfinPlayback) who() string {
	parts := []string{}
	for _, part := range []string{p.User, p.Device, p.Client} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " · ")
}

func jellyfinSessions(baseURL, apiKey string) ([]jellyfinPlayback, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/Sessions?activeWithinSeconds=60", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", apiKey)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jellyfin /Sessions: HTTP %d", resp.StatusCode)
	}
	var sessions []struct {
		UserName       string
		Client         string
		DeviceName     string
		NowPlayingItem *struct {
			Name         string
			Type         string
			MediaStreams []struct {
				Type          string
				RealFrameRate float64
			}
		}
		TranscodingInfo *struct {
			IsVideoDirect        bool
			IsAudioDirect        bool
			TranscodeReasons     []string
			Framerate            float64
			CompletionPercentage float64
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, err
	}
	out := []jellyfinPlayback{}
	for _, s := range sessions {
		if s.NowPlayingItem == nil || s.NowPlayingItem.Type != "TvChannel" {
			continue
		}
		p := jellyfinPlayback{Channel: s.NowPlayingItem.Name, Client: s.Client, Device: s.DeviceName,
			User: s.UserName, Video: "copy", Audio: "copy"}
		if t := s.TranscodingInfo; t != nil {
			if !t.IsVideoDirect {
				p.Video = "transcode"
			}
			if !t.IsAudioDirect {
				p.Audio = "transcode"
			}
			p.Reasons, p.Framerate, p.Progress = t.TranscodeReasons, t.Framerate, t.CompletionPercentage
			for _, m := range s.NowPlayingItem.MediaStreams {
				if m.Type == "Video" && m.RealFrameRate > 0 && t.Framerate > 0 {
					p.Speed = math.Round(t.Framerate/m.RealFrameRate*100) / 100
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}
