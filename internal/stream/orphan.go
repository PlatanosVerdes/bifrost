package stream

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
)

// Jellyfin can keep pulling a channel after losing its session (a stop that hits an older one): let it
// go after orphanAfter with no Jellyfin session playing it, and keep it on any doubt. A zap is no doubt:
// Jellyfin keeps the old channel open for minutes while a channel opened after it is the one it plays.
const zapAfter = time.Minute

func fromJellyfin(r *http.Request) bool {
	return strings.Contains(r.UserAgent(), "Jellyfin")
}

// client names who opened a stream in the log: Jellyfin, or the User-Agent of a direct player.
func client(r *http.Request) string {
	if fromJellyfin(r) {
		return "Jellyfin"
	}
	if ua := r.UserAgent(); ua != "" {
		return "direct: " + ua
	}
	return "direct"
}

type orphans struct {
	after  time.Duration
	unseen map[*stream]time.Time
}

// zappedFrom says whether a channel opened after s is the one Jellyfin plays now.
func zappedFrom(s *stream, streams []*stream, playing []JellyfinPlayback) bool {
	for _, t := range streams {
		if t != s && t.opened.After(s.opened) && watched(t.label, playing) {
			return true
		}
	}
	return false
}

// pass returns the streams to let go, given what Jellyfin plays right now. playing is nil when
// Jellyfin could not be asked, which closes nothing and forgets nothing.
func (o *orphans) pass(now time.Time, streams []*stream, playing []JellyfinPlayback) []*stream {
	if playing == nil {
		return nil
	}
	current := map[*stream]bool{}
	var release []*stream
	for _, s := range streams {
		current[s] = true
		if s.directViewers() > 0 || watched(s.label, playing) {
			delete(o.unseen, s)
			continue
		}
		first, ok := o.unseen[s]
		if !ok {
			o.unseen[s] = now
			continue
		}
		if now.Sub(first) >= o.after || now.Sub(first) >= zapAfter && zappedFrom(s, streams, playing) {
			release = append(release, s)
			delete(o.unseen, s)
		}
	}
	for s := range o.unseen {
		if !current[s] {
			delete(o.unseen, s)
		}
	}
	return release
}

func watched(label string, playing []JellyfinPlayback) bool {
	for _, p := range playing {
		if p.Channel != "" && strings.HasPrefix(label, p.Channel) {
			return true
		}
	}
	return false
}

func (h *Hub) open() []*stream {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*stream
	for _, s := range h.streams {
		s.mu.Lock()
		if !s.done {
			out = append(out, s)
		}
		s.mu.Unlock()
	}
	return out
}

func (h *Hub) CloseOrphans(every, after time.Duration) {
	o := &orphans{after: after, unseen: map[*stream]time.Time{}}
	for range time.Tick(every) {
		streams := h.open()
		if len(streams) == 0 {
			o.pass(time.Now(), nil, []JellyfinPlayback{})
			continue
		}
		playing, err := jellyfinSessions(h.jellyfinURL, h.jellyfinKey)
		if err != nil {
			playing = nil
		}
		for _, s := range o.pass(time.Now(), streams, playing) {
			log.Printf("[proxy] %s closed: no Jellyfin session plays it, left open by a zap or for %s", s.label, after)
			s.cancel()
		}
	}
}

// JellyfinPlayback is how Jellyfin delivers a live channel. A re-encode on a Pi 4 cannot keep up with
// 1080p, so a channel that plays a few seconds and stalls shows up here first.
type JellyfinPlayback struct {
	Channel   string   `json:"channel"`
	Client    string   `json:"client"`
	Device    string   `json:"device"`
	User      string   `json:"user,omitempty"`
	Video     string   `json:"video"` // copy or transcode
	Audio     string   `json:"audio"`
	Reasons   []string `json:"reasons,omitempty"`
	Framerate float64  `json:"framerate,omitempty"`
	// Speed is ffmpeg's frame rate over the channel's: below 1 it falls behind live and stalls.
	Speed    float64 `json:"speed,omitempty"`
	Progress float64 `json:"completion,omitempty"`
}

// who names the viewer for the page and the log: the Jellyfin user, then the device and client.
func (p JellyfinPlayback) who() string {
	parts := []string{}
	for _, part := range []string{p.User, p.Device, p.Client} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " · ")
}

func jellyfinSessions(baseURL, apiKey string) ([]JellyfinPlayback, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/Sessions?activeWithinSeconds=60", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Token", apiKey)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jellyfin /Sessions: HTTP %d", resp.StatusCode)
	}
	var sessions []struct {
		UserName       string
		Client         string
		DeviceName     string
		NowPlayingItem *struct {
			Name         string
			Type         string
			MediaStreams []struct {
				Type          string
				RealFrameRate float64
			}
		}
		TranscodingInfo *struct {
			IsVideoDirect        bool
			IsAudioDirect        bool
			TranscodeReasons     []string
			Framerate            float64
			CompletionPercentage float64
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, err
	}
	out := []JellyfinPlayback{}
	for _, s := range sessions {
		if s.NowPlayingItem == nil || s.NowPlayingItem.Type != "TvChannel" {
			continue
		}
		p := JellyfinPlayback{Channel: s.NowPlayingItem.Name, Client: s.Client, Device: s.DeviceName,
			User: s.UserName, Video: "copy", Audio: "copy"}
		if t := s.TranscodingInfo; t != nil {
			if !t.IsVideoDirect {
				p.Video = "transcode"
			}
			if !t.IsAudioDirect {
				p.Audio = "transcode"
			}
			p.Reasons, p.Framerate, p.Progress = t.TranscodeReasons, t.Framerate, t.CompletionPercentage
			for _, m := range s.NowPlayingItem.MediaStreams {
				if m.Type == "Video" && m.RealFrameRate > 0 && t.Framerate > 0 {
					p.Speed = math.Round(t.Framerate/m.RealFrameRate*100) / 100
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// JellyfinPlaying is what Jellyfin plays now; asked is false when no Jellyfin is configured.
func (h *Hub) JellyfinPlaying() (playing []JellyfinPlayback, asked bool, err error) {
	if h.jellyfinURL == "" {
		return nil, false, nil
	}
	playing, err = jellyfinSessions(h.jellyfinURL, h.jellyfinKey)
	return playing, true, err
}
