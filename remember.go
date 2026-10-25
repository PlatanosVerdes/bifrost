package main

import (
	"encoding/json"
	"log"
	"os"
	"slices"
)

// The list prunes channels and sources that still play, so what it drops is kept, in its old place in
// the playlist, until it fails to open.
type rememberedChannel struct {
	Slug   string   `json:"slug"`
	Name   string   `json:"name"`
	Extinf string   `json:"extinf"`
	IDs    []string `json:"ids"`
}

type memory struct {
	path     string
	loaded   bool
	channels []rememberedChannel // in playlist order
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
// whether a source's last open failed; a dropped source that did is forgotten.
func (m *memory) keep(order []string, groups map[string]*channelGroup, failed func(string) bool) []string {
	m.load()
	listed := map[string]bool{}
	for _, slug := range order {
		listed[slug] = true
	}
	var out []string
	next, emitted := 0, map[string]bool{}
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
			g = &channelGroup{Name: rc.Name, extinf: rc.Extinf}
		}
		for _, id := range rc.IDs {
			if !slices.Contains(g.IDs, id) && !failed(id) {
				g.IDs = append(g.IDs, id)
				if g.kept == nil {
					g.kept = map[string]bool{}
				}
				g.kept[id] = true
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
		m.channels = append(m.channels, rememberedChannel{Slug: slug, Name: g.Name, Extinf: g.extinf, IDs: g.IDs})
	}
	if b, err := json.Marshal(m.channels); err == nil {
		tmp := m.path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o644); err == nil {
			os.Rename(tmp, m.path)
		}
	}
	return out
}
