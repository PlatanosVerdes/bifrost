// Package catalog turns the channel lists into channels, writes Jellyfin's playlist and guide, and keeps what airs.
package catalog

import "sync"

// The list loop replaces the catalog whole each pass, so a dropped id disappears at once.
type Catalog struct {
	mu     sync.RWMutex
	order  []string
	groups map[string]*Group
	names  map[string]string // content id -> channel name, for log lines

	Blocked *Blocklist // sources taken out from the page, left out of every read
}

func (c *Catalog) Set(order []string, groups map[string]*Group) {
	names := map[string]string{}
	for _, g := range groups {
		for _, id := range g.IDs {
			names[id] = g.Name
		}
	}
	c.mu.Lock()
	c.order, c.groups, c.names = order, groups, names
	c.mu.Unlock()
}

func (c *Catalog) Lookup(slug string) (string, []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g := c.groups[slug]
	if g == nil {
		return "", nil
	}
	return g.Name, c.Unblocked(slug, g.IDs)
}

func (c *Catalog) Unblocked(slug string, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !c.Blocked.Has(slug, id) {
			out = append(out, id)
		}
	}
	return out
}

func (c *Catalog) NameOf(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.names[id]
}

func (c *Catalog) Group(slug string) *Group {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.groups[slug]
}

func (c *Catalog) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.order)
}

func (c *Catalog) All() ([]string, map[string]*Group) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]string(nil), c.order...), c.groups
}
