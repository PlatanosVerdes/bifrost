package main

import "sync"

// The list loop replaces the catalog whole each pass, so a dropped id disappears at once.
type catalog struct {
	mu     sync.RWMutex
	order  []string
	groups map[string]*channelGroup
	names  map[string]string // content id -> channel name, for log lines
}

func (c *catalog) set(order []string, groups map[string]*channelGroup) {
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

func (c *catalog) lookup(slug string) (string, []string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g := c.groups[slug]
	if g == nil {
		return "", nil
	}
	return g.Name, append([]string(nil), g.IDs...)
}

func (c *catalog) nameOf(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.names[id]
}

func (c *catalog) group(slug string) *channelGroup {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.groups[slug]
}

func (c *catalog) count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.order)
}
