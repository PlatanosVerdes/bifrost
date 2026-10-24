package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// Jellyfin can keep pulling a channel after losing its session (a stop that hits an older one): let it
// go after orphanAfter with no Jellyfin session playing it, and keep it on any doubt.

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

// pass returns the streams to let go, given what Jellyfin plays right now. playing is nil when
// Jellyfin could not be asked, which closes nothing and forgets nothing.
func (o *orphans) pass(now time.Time, streams []*stream, playing []jellyfinPlayback) []*stream {
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
		if now.Sub(first) >= o.after {
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

func watched(label string, playing []jellyfinPlayback) bool {
	for _, p := range playing {
		if p.Channel != "" && strings.HasPrefix(label, p.Channel) {
			return true
		}
	}
	return false
}

func (h *hub) open() []*stream {
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

func (h *hub) closeOrphans(every, after time.Duration) {
	o := &orphans{after: after, unseen: map[*stream]time.Time{}}
	for range time.Tick(every) {
		streams := h.open()
		if len(streams) == 0 {
			o.pass(time.Now(), nil, []jellyfinPlayback{})
			continue
		}
		playing, err := jellyfinSessions(h.jellyfinURL, h.jellyfinKey)
		if err != nil {
			playing = nil
		}
		for _, s := range o.pass(time.Now(), streams, playing) {
			log.Printf("[proxy] %s closed: no Jellyfin session has played it for %s", s.label, after)
			s.cancel()
		}
	}
}
