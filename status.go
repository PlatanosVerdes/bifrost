package main

import (
	"encoding/json"
	"net/http"
	"sort"
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

type jellyfinStatus struct {
	LastRefresh time.Time  `json:"last_refresh"`
	LastCode    int        `json:"last_code"`
	RefreshDue  *time.Time `json:"refresh_due,omitempty"`
}

type openRecord struct {
	Channel string    `json:"channel"`
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
	recent   []openRecord
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
		"list":     h.status.list,
		"jellyfin": h.status.jellyfin,
		"recent":   append([]openRecord{}, h.status.recent...),
	}
	h.status.mu.Unlock()
	body["streams"] = h.streamsNow()
	writeJSON(w, body)
}

type channelInfo struct {
	Slug  string   `json:"slug"`
	Name  string   `json:"name"`
	Group string   `json:"group"`
	Logo  string   `json:"logo"`
	TvgID string   `json:"tvg_id"`
	IDs   []string `json:"ids"` // the order the proxy would try them in
}

// serveChannels is the catalog for players that go to the engine directly (farnsworth): each
// channel with its sources, the one that last produced video first.
func (h *hub) serveChannels(w http.ResponseWriter, r *http.Request) {
	h.table.mu.RLock()
	order := append([]string(nil), h.table.order...)
	groups := h.table.groups
	h.table.mu.RUnlock()

	out := make([]channelInfo, 0, len(order))
	for _, slug := range order {
		g := groups[slug]
		if g == nil {
			continue
		}
		out = append(out, channelInfo{Slug: slug, Name: g.Name, Group: extractAttr(g.extinf, "group-title"),
			Logo: extractAttr(g.extinf, "tvg-logo"), TvgID: extractAttr(g.extinf, "tvg-id"), IDs: h.ordered(slug, g.IDs)})
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
