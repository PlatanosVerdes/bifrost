package stream

import (
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/m3u"
)

const recentOpens = 20

type OpenRecord struct {
	Channel string    `json:"channel"`
	Slug    string    `json:"slug,omitempty"`
	Source  string    `json:"source,omitempty"`
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // playing, gave_up, abandoned
	Seconds float64   `json:"seconds"`
	Device  string    `json:"device,omitempty"`
}

type StreamStatus struct {
	Channel       string          `json:"channel"`
	Slug          string          `json:"slug,omitempty"`
	Source        string          `json:"source,omitempty"`
	Viewers       int             `json:"viewers"`
	External      int             `json:"external"`
	ThumbAt       time.Time       `json:"thumb_at,omitzero"`
	Since         time.Time       `json:"since"`
	StartedAfterS float64         `json:"started_after_s"`
	Now           *catalog.Airing `json:"now,omitempty"`
	Next          *catalog.Airing `json:"next,omitempty"`
	Swarm         *swarmStatus    `json:"swarm,omitempty"`
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

// statusBoard records each channel open for the page.
type statusBoard struct {
	mu     sync.Mutex
	recent []OpenRecord
	last   map[string]OpenRecord // slug -> that channel's latest open, kept beyond the recent 20
}

func (b *statusBoard) opened(r OpenRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append([]OpenRecord{r}, b.recent...)
	if len(b.recent) > recentOpens {
		b.recent = b.recent[:recentOpens]
	}
	if r.Slug != "" {
		if b.last == nil {
			b.last = map[string]OpenRecord{}
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
func (h *Hub) noteDevice(at time.Time, label string) {
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

func (b *statusBoard) lastOpen(slug string) *OpenRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.last[slug]
	if !ok {
		return nil
	}
	return &r
}

func (h *Hub) StreamsNow() []StreamStatus {
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

	out := []StreamStatus{}
	for _, s := range streams {
		s.mu.Lock()
		if !s.done && s.ok {
			out = append(out, StreamStatus{Channel: s.label, Slug: s.slug, Source: s.source, Waiting: s.source == "",
				Viewers: len(s.viewers), External: len(s.direct), ThumbAt: h.thumbs.at(s.source), Since: s.playingSince, StartedAfterS: s.startedAfterS, Swarm: s.swarm.status(),
				Probing: s.probing != "", ProbingSource: s.probing, Switches: s.switches, LastSwitch: s.lastSwitch,
				LastProbe: s.lastProbe, channelSwitches: switches[s.slug], remuxRestarts: restarts[s.slug]})
			out[len(out)-1].Now, out[len(out)-1].Next = h.onAir(s.slug)
		}
		s.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

func (h *Hub) onAir(slug string) (now, next *catalog.Airing) {
	g := h.table.Group(slug)
	if g == nil {
		return nil, nil
	}
	return h.schedule.At(m3u.Attr(g.Extinf, "tvg-id"), time.Now())
}

// recentlyFailed is how long a source that failed to open is tried after the others.
const recentlyFailed = 15 * time.Minute

// The source that last produced video goes first, one that looped lately after the rest and one that
// failed lately last, so a dead first source does not cost every open its whole wait.
func (h *Hub) ordered(slug string, ids []string) []string {
	h.mu.Lock()
	good := h.lastGood[slug]
	failed, looping := map[string]bool{}, h.loopingLately(ids)
	for _, id := range ids {
		if f := h.facts[id]; f != nil && f.Outcome == "failed" && time.Since(f.SeenAt) < recentlyFailed {
			failed[id] = true
		}
	}
	h.mu.Unlock()
	rank := func(id string) int {
		switch {
		case failed[id]:
			return 3
		case looping[id]:
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

func (b *statusBoard) recentOpens() []OpenRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]OpenRecord{}, b.recent...)
}

// PlayingSource is the content id a channel plays right now, empty when it plays none.
func (h *Hub) PlayingSource(slug string) string {
	h.mu.Lock()
	s := h.streams["channel:"+slug]
	h.mu.Unlock()
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source
}

func (h *Hub) RecentOpens() []OpenRecord {
	return h.status.recentOpens()
}

func (h *Hub) lastOpen(slug string) *OpenRecord {
	return h.status.lastOpen(slug)
}

func (h *Hub) thumbAt(id string) time.Time {
	return h.thumbs.at(id)
}

func (h *Hub) ThumbFile(id string) string {
	return h.thumbs.file(id)
}

// ChannelInfo is one channel as the page shows it.
type ChannelInfo struct {
	Slug     string          `json:"slug"`
	Name     string          `json:"name"`
	Group    string          `json:"group"`
	Logo     string          `json:"logo"`
	TvgID    string          `json:"tvg_id"`
	IDs      []string        `json:"ids"` // the order the proxy would try them in
	Playing  *StreamStatus   `json:"playing,omitempty"`
	LastOpen *OpenRecord     `json:"last_open,omitempty"`
	Now      *catalog.Airing `json:"now,omitempty"`
	Next     *catalog.Airing `json:"next,omitempty"`
	Sources  []SourceFacts   `json:"sources"` // the playlist's order, with what was last seen of each
	Blocked  []SourceFacts   `json:"blocked,omitempty"`
	Survey   *SurveyState    `json:"survey,omitempty"`
}

// Channels lists every channel in the playlist's order, with its sources the way an open would try them.
func (h *Hub) Channels() []ChannelInfo {
	order, groups := h.table.All()
	playing := map[string]StreamStatus{}
	for _, st := range h.StreamsNow() {
		if st.Slug != "" {
			playing[st.Slug] = st
		}
	}
	out := make([]ChannelInfo, 0, len(order))
	for _, slug := range order {
		g := groups[slug]
		if g == nil {
			continue
		}
		ids := h.table.Unblocked(slug, g.IDs)
		var blocked []string
		for _, id := range g.IDs {
			if !slices.Contains(ids, id) {
				blocked = append(blocked, id)
			}
		}
		out = append(out, ChannelInfo{Slug: slug, Name: g.Name, Group: m3u.Attr(g.Extinf, "group-title"),
			Logo: m3u.Attr(g.Extinf, "tvg-logo"), TvgID: m3u.Attr(g.Extinf, "tvg-id"), IDs: h.ordered(slug, ids),
			LastOpen: h.lastOpen(slug), Sources: h.sourcesOf(ids), Blocked: h.sourcesOf(blocked)})
		for _, list := range [][]SourceFacts{out[len(out)-1].Sources, out[len(out)-1].Blocked} {
			for i := range list {
				list[i].Kept, list[i].ThumbAt = g.Kept[list[i].ID], h.thumbAt(list[i].ID)
			}
		}
		out[len(out)-1].Survey = h.surveying(slug)
		out[len(out)-1].Now, out[len(out)-1].Next = h.schedule.At(m3u.Attr(g.Extinf, "tvg-id"), time.Now())
		if st, ok := playing[slug]; ok {
			out[len(out)-1].Playing = &st
		}
	}
	return out
}

// loopingLately is the ids that went back to play a stretch again in the last recentlyFailed; h.mu is held.
func (h *Hub) loopingLately(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if f := h.facts[id]; f != nil && f.Loops > 0 && time.Since(f.LoopedAt) < recentlyFailed {
			out[id] = true
		}
	}
	return out
}
