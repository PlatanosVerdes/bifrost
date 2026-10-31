package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// blocklist holds the sources taken out of a channel from the page, kept in STATE_DIR. It is per
// channel: an id that carries the wrong channel under one name may be right under another.
type blocklist struct {
	mu   sync.Mutex
	path string
	set  map[string][]string // slug -> blocked ids
}

func loadBlocklist(dir string) *blocklist {
	b := &blocklist{path: filepath.Join(dir, "blocked.json"), set: map[string][]string{}}
	if raw, err := os.ReadFile(b.path); err == nil {
		json.Unmarshal(raw, &b.set)
	}
	return b
}

func (b *blocklist) has(slug, id string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Contains(b.set[slug], id)
}

func (b *blocklist) mark(slug, id string, blocked bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := b.set[slug]
	switch i := slices.Index(ids, id); {
	case blocked && i < 0:
		b.set[slug] = append(ids, id)
	case !blocked && i >= 0:
		if b.set[slug] = slices.Delete(ids, i, i+1); len(b.set[slug]) == 0 {
			delete(b.set, slug)
		}
	default:
		return nil
	}
	raw, _ := json.Marshal(b.set)
	tmp := b.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.path)
}

// serveBlock takes a source out of a channel (POST) or puts it back (DELETE).
func (h *hub) serveBlock(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return
	}
	slug, id := r.PathValue("slug"), r.PathValue("id")
	g := h.table.group(slug)
	if g == nil || !slices.Contains(g.IDs, id) {
		writeError(w, http.StatusNotFound, "That is not one of this channel's sources")
		return
	}
	block := r.Method == http.MethodPost
	if block {
		if _, left := h.table.lookup(slug); len(left) == 1 && left[0] == id {
			writeError(w, http.StatusConflict, "This is the channel's last source")
			return
		}
		h.mu.Lock()
		s := h.streams["channel:"+slug]
		h.mu.Unlock()
		if s != nil {
			s.mu.Lock()
			playing := s.source == id
			s.mu.Unlock()
			if playing {
				writeError(w, http.StatusConflict, "The channel is playing this source: pick another one first")
				return
			}
		}
	}
	if err := h.table.blocked.mark(slug, id, block); err != nil {
		writeError(w, http.StatusInternalServerError, "The blocklist could not be saved")
		return
	}
	writeJSON(w, map[string]any{"id": id, "blocked": block})
}
