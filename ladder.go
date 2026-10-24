package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A channel's 720p and 1080p versions share a guide id: a starving channel steps down to a lower
// version's source and back up once its own measures clearly better, never to another channel.

var (
	resolutionSuffix = regexp.MustCompile(`(?i)\s+(\d{3,4}p|4k)$`)
	// Back up only on clear headroom, so a step does not bounce straight down again.
	stepUpAfter    = 5 * time.Minute
	stepUpHeadroom = 1.3
)

// height is a channel's resolution from its name, 0 when it carries none.
func height(name string) int {
	m := resolutionSuffix.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	if strings.EqualFold(m[1], "4k") {
		return 2160
	}
	var h int
	fmt.Sscanf(m[1], "%dp", &h)
	return h
}

// lower lists the sources of slug's lower versions, closest resolution first.
func (c *catalog) lower(slug string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	own := c.groups[slug]
	if own == nil {
		return nil
	}
	tvg, stem, top := extractAttr(own.extinf, "tvg-id"), resolutionSuffix.ReplaceAllString(own.Name, ""), height(own.Name)
	if tvg == "" || top == 0 {
		return nil
	}
	var siblings []*channelGroup
	for s, g := range c.groups {
		h := height(g.Name)
		if s != slug && h > 0 && h < top && extractAttr(g.extinf, "tvg-id") == tvg && resolutionSuffix.ReplaceAllString(g.Name, "") == stem {
			siblings = append(siblings, g)
		}
	}
	sort.Slice(siblings, func(i, j int) bool { return height(siblings[i].Name) > height(siblings[j].Name) })
	var ids []string
	for _, g := range siblings {
		ids = append(ids, g.IDs...)
	}
	return ids
}

func (c *catalog) heightOf(id string) int {
	return height(c.nameOf(id))
}

// announce tells the Jellyfin sessions playing a channel what changed, as an on-screen message.
func (h *hub) announce(channel, text string) {
	if h.jellyfinURL == "" {
		return
	}
	base := strings.TrimRight(h.jellyfinURL, "/")
	client := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, base+"/Sessions?activeWithinSeconds=60", nil)
	req.Header.Set("X-Emby-Token", h.jellyfinKey)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[proxy] %s could not tell Jellyfin: %v", channel, err)
		return
	}
	var sessions []struct {
		ID             string `json:"Id"`
		NowPlayingItem *struct{ Name string }
	}
	err = json.NewDecoder(resp.Body).Decode(&sessions)
	resp.Body.Close()
	if err != nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"Header": "bifrost", "Text": text, "TimeoutMs": 6000})
	for _, s := range sessions {
		if s.NowPlayingItem == nil || s.NowPlayingItem.Name != channel {
			continue
		}
		req, _ := http.NewRequest(http.MethodPost, base+"/Sessions/"+s.ID+"/Message", bytes.NewReader(body))
		req.Header.Set("X-Emby-Token", h.jellyfinKey)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
}
