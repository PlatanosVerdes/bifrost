package catalog

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"time"
)

// Half the kept sources still played when all were tried; three days spans a weekend's events.
const KeepDroppedFor = 3 * 24 * time.Hour

// The list prunes channels and sources that still play, so what it drops is kept, in its old place in
// the playlist, until it fails to open or has been out of the list for keepDroppedFor.
type rememberedChannel struct {
	Slug   string   `json:"slug"`
	Name   string   `json:"name"`
	Extinf string   `json:"extinf"`
	IDs    []string `json:"ids"`
	// When each kept source left the list.
	Dropped map[string]time.Time `json:"dropped,omitempty"`
}

type memory struct {
	path     string
	keepFor  time.Duration // keepDroppedFor when zero
	loaded   bool
	channels []rememberedChannel // in playlist order
}

func (m *memory) window() time.Duration {
	if m.keepFor > 0 {
		return m.keepFor
	}
	return KeepDroppedFor
}

func (m *memory) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	if b, err := os.ReadFile(m.path); err == nil {
		json.Unmarshal(b, &m.channels)
	}
}

// keep adds what the list dropped to this pass's channels and remembers the result. failed says
// whether a source's last open failed; a dropped source that did, or that left the list too long
// ago, is forgotten.
func (m *memory) keep(order []string, groups map[string]*Group, failed func(string) bool, now time.Time) []string {
	m.load()
	listed := map[string]bool{}
	for _, slug := range order {
		listed[slug] = true
	}
	var out []string
	next, emitted := 0, map[string]bool{}
	droppedAt := map[string]time.Time{}
	emitListedUpTo := func(slug string) {
		for next < len(order) && !emitted[slug] {
			out = append(out, order[next])
			emitted[order[next]] = true
			next++
		}
	}
	for _, rc := range m.channels {
		g := groups[rc.Slug]
		if g == nil {
			g = &Group{Name: rc.Name, Extinf: rc.Extinf}
		}
		for _, id := range rc.IDs {
			if slices.Contains(g.IDs, id) {
				continue
			}
			since, ok := rc.Dropped[id]
			if !ok {
				since = now
			}
			if !failed(id) && now.Sub(since) < m.window() {
				droppedAt[id] = since
				g.IDs = append(g.IDs, id)
				if g.Kept == nil {
					g.Kept = map[string]bool{}
				}
				g.Kept[id] = true
			}
		}
		switch {
		case listed[rc.Slug]:
			emitListedUpTo(rc.Slug)
		case len(g.IDs) > 0:
			log.Printf("Keeping %s, which the list dropped, with %d sources", g.Name, len(g.IDs))
			groups[rc.Slug] = g
			out = append(out, rc.Slug)
		}
	}
	out = append(out, order[next:]...)

	m.channels = m.channels[:0]
	for _, slug := range out {
		g := groups[slug]
		rc := rememberedChannel{Slug: slug, Name: g.Name, Extinf: g.Extinf, IDs: g.IDs}
		for id := range g.Kept {
			if rc.Dropped == nil {
				rc.Dropped = map[string]time.Time{}
			}
			rc.Dropped[id] = droppedAt[id]
		}
		m.channels = append(m.channels, rc)
	}
	if b, err := json.Marshal(m.channels); err == nil {
		tmp := m.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err == nil {
			os.Rename(tmp, m.path)
		}
	}
	return out
}
