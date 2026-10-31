package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Blocklist holds the sources taken out of a channel from the page, kept in STATE_DIR. It is per
// channel: an id that carries the wrong channel under one name may be right under another.
type Blocklist struct {
	mu   sync.Mutex
	path string
	set  map[string][]string // slug -> blocked ids
}

func LoadBlocklist(dir string) *Blocklist {
	b := &Blocklist{path: filepath.Join(dir, "blocked.json"), set: map[string][]string{}}
	if raw, err := os.ReadFile(b.path); err == nil {
		json.Unmarshal(raw, &b.set)
	}
	return b
}

func (b *Blocklist) Has(slug, id string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Contains(b.set[slug], id)
}

func (b *Blocklist) Mark(slug, id string, blocked bool) error {
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
