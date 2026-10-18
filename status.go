package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// What bifrost already knows, served without opening anything: the list it read, what it told
// Jellyfin, what is playing through it and how the last opens went. A status page or another
// service reads this instead of probing channels, which this Pi cannot afford.

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
	Seconds int       `json:"seconds"`
}

type streamStatus struct {
	Channel       string    `json:"channel"`
	Slug          string    `json:"slug,omitempty"`
	Source        string    `json:"source,omitempty"`
	Viewers       int       `json:"viewers"`
	Since         time.Time `json:"since"`
	StartedAfterS int       `json:"started_after_s"`
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

	out := []streamStatus{}
	for _, s := range streams {
		s.mu.Lock()
		if !s.done && s.ok {
			out = append(out, streamStatus{Channel: s.label, Slug: s.slug, Source: s.source,
				Viewers: len(s.viewers), Since: s.playingSince, StartedAfterS: s.startedAfterS})
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
}

// serveChannels is the catalog for players that go to the engine directly (farnsworth): each
// channel with its sources, the one that last produced video first.
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
		out = append(out, channelInfo{Slug: slug, Name: g.Name, Group: extractAttr(g.extinf, "group-title"),
			Logo: extractAttr(g.extinf, "tvg-logo"), TvgID: extractAttr(g.extinf, "tvg-id"), IDs: h.ordered(slug, g.IDs),
			LastOpen: h.status.lastOpen(slug)})
		if st, ok := playing[slug]; ok {
			out[len(out)-1].Playing = &st
		}
	}
	writeJSON(w, map[string]any{"channels": out})
}

// ordered puts the source that last produced video first, as planFor does.
func (h *hub) ordered(slug string, ids []string) []string {
	h.mu.Lock()
	good := h.lastGood[slug]
	h.mu.Unlock()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == good {
			out = append([]string{id}, out...)
		} else {
			out = append(out, id)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

// jellyfinPlayback is how Jellyfin is delivering a live channel: copying it or re-encoding it, and
// why. A re-encode on a Pi 4 cannot keep up with 1080p, so this is the first thing to look at when a
// channel plays for a few seconds and stalls.
type jellyfinPlayback struct {
	Channel   string   `json:"channel"`
	Client    string   `json:"client"`
	Device    string   `json:"device"`
	Video     string   `json:"video"` // copy or transcode
	Audio     string   `json:"audio"`
	Reasons   []string `json:"reasons,omitempty"`
	Framerate float64  `json:"framerate,omitempty"`
	Progress  float64  `json:"completion,omitempty"`
}

// jellyfinSessions asks Jellyfin, once per status request, which live channels it is playing.
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
		Client         string
		DeviceName     string
		NowPlayingItem *struct {
			Name string
			Type string
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
			Video: "copy", Audio: "copy"}
		if t := s.TranscodingInfo; t != nil {
			if !t.IsVideoDirect {
				p.Video = "transcode"
			}
			if !t.IsAudioDirect {
				p.Audio = "transcode"
			}
			p.Reasons, p.Framerate, p.Progress = t.TranscodeReasons, t.Framerate, t.CompletionPercentage
		}
		out = append(out, p)
	}
	return out, nil
}
