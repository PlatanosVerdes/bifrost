package stream

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// A channel's 720p and 1080p versions share a guide id: a starving channel steps down to a lower
// version's source and back up once its own measures clearly better, never to another channel.

var (
	// Back up only on clear headroom, so a step does not bounce straight down again.
	stepUpAfter    = 5 * time.Minute
	stepUpHeadroom = 1.3
)

// announce tells the Jellyfin sessions playing a channel what changed, as an on-screen message.
func (h *Hub) announce(channel, text string) {
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
