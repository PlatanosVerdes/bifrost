package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"time"
)

// A survey opens a channel's other sources one after another, each for a few seconds, for the
// picture and facts the picker shows. Each open costs the engine a swarm join and a few MB, so a
// machine checks as many channels at once as MACHINE_SIZE allows, one source each.
const (
	surveyFor   = 10 * time.Second
	surveyFresh = 5 * time.Minute // a source checked this recently is skipped, so a double click opens nothing twice
)

type surveyState struct {
	Slug    string `json:"slug"`
	Current string `json:"current,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
}

func (h *hub) surveying(slug string) *surveyState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.surveys[slug] == nil {
		return nil
	}
	st := *h.surveys[slug]
	return &st
}

func (h *hub) serveSurvey(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return
	}
	if h.thumbs == nil {
		writeError(w, http.StatusServiceUnavailable, "Checking sources needs ffmpeg")
		return
	}
	slug := r.PathValue("slug")
	_, ids := h.table.lookup(slug)
	if len(ids) == 0 {
		writeError(w, http.StatusNotFound, "No such channel")
		return
	}

	h.mu.Lock()
	if busy := h.surveyBusy(slug); busy != "" {
		h.mu.Unlock()
		writeError(w, http.StatusConflict, busy)
		return
	}
	playing := ""
	if s := h.streams["channel:"+slug]; s != nil {
		s.mu.Lock()
		playing = s.source
		s.mu.Unlock()
	}
	var todo []string
	for _, id := range ids {
		f := h.facts[id]
		fresh := f != nil && time.Since(f.SeenAt) < surveyFresh && time.Since(h.thumbs.at(id)) < surveyFresh
		if id != playing && !fresh {
			todo = append(todo, id)
		}
	}
	if len(todo) > 0 {
		h.surveys[slug] = &surveyState{Slug: slug, Total: len(todo)}
		go h.runSurvey(slug, todo)
	}
	h.mu.Unlock()
	writeJSON(w, map[string]int{"checking": len(todo)})
}

// surveyBusy says why slug cannot be checked now, empty when it can; h.mu is held.
func (h *hub) surveyBusy(slug string) string {
	if h.surveys[slug] != nil {
		return "This channel's sources are being checked already"
	}
	if len(h.surveys) < h.surveyMax {
		return ""
	}
	if h.surveyMax == 1 {
		for other := range h.surveys {
			name, _ := h.table.lookup(other)
			return "This machine checks one channel at a time: " + name + " is being checked"
		}
	}
	return fmt.Sprintf("This machine checks %d channels at a time, try again when one ends", h.surveyMax)
}

func (h *hub) runSurvey(slug string, todo []string) {
	defer func() {
		h.mu.Lock()
		delete(h.surveys, slug)
		h.mu.Unlock()
	}()
	name, _ := h.table.lookup(slug)
	log.Printf("[survey] %s: checking %d sources", name, len(todo))
	for i, id := range todo {
		if _, ids := h.table.lookup(slug); !slices.Contains(ids, id) {
			continue
		}
		h.mu.Lock()
		h.surveys[slug].Current, h.surveys[slug].Done = id, i
		h.mu.Unlock()
		h.surveyOne(name, id)
	}
}

func (h *hub) surveyOne(name, id string) {
	label := name + " [survey " + short(id) + "]"
	h.waitForSwitches()
	start := time.Now()
	openBy := start.Add(min(h.sourceWait, probeOpenWait))
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(openBy)+surveyFor+5*time.Second)
	defer cancel()
	session, err := h.engineSession(ctx, id)
	if err != nil {
		log.Printf("[survey] %s: %v", label, err)
		h.noteOutcome(id, "failed")
		return
	}
	// The engine keeps a session some 15 s after its player leaves, and past its session limit it stops
	// the oldest, which is the one being watched: so every session is stopped the moment it is done.
	defer session.stop()
	resp, ok := open(ctx, h.client, session.Playback, nil, label, openBy)
	if !ok {
		h.noteOutcome(id, "failed")
		return
	}
	first, ok := awaitFirstBytes(ctx, resp.Body, label, start, openBy)
	if !ok {
		resp.Body.Close()
		h.noteOutcome(id, "failed")
		return
	}
	up := feed(ctx, id, resp, first)
	defer up.cancel()

	buf := append([]byte(nil), first...)
	end := time.NewTimer(surveyFor)
	defer end.Stop()
collect:
	for len(buf) < thumbBytes {
		select {
		case chunk, open := <-up.chunks:
			if !open {
				break collect
			}
			buf = append(buf, chunk...)
		case <-end.C:
			break collect
		}
	}
	peers := -1
	if up.stat != "" {
		if sample, err := fetchStat(ctx, &http.Client{Timeout: 3 * time.Second}, up.stat); err == nil {
			peers = sample.stat.Peers
		}
	}
	up.cancel()

	height, audio := h.thumbs.cutNow(id, buf)
	log.Printf("[survey] %s: %d KB, %dp, %s, %d peers", label, len(buf)>>10, height, audio, peers)
	h.noteSource(id, func(f *sourceFacts) {
		if height > 0 {
			f.Height, f.Audio = height, audio
		}
		if peers >= 0 {
			f.Peers = peers
		}
		f.SeenAt, f.Outcome = time.Now(), "probed"
	})
}

// engineSession is a session the engine opened for one content id, with the URL that stops it.
type engineSession struct {
	Playback string `json:"playback_url"`
	Command  string `json:"command_url"`
}

func (h *hub) engineSession(ctx context.Context, id string) (*engineSession, error) {
	target := fmt.Sprintf("%s/ace/getstream?id=%s&format=json&pid=bifrost-survey-%d", h.base, id, h.probes.Add(1))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: probeOpenWait}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var answer struct {
		Response *engineSession `json:"response"`
		Error    *string        `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, fmt.Errorf("engine answer: %v", err)
	}
	if answer.Response == nil || answer.Response.Playback == "" {
		reason := "no session"
		if answer.Error != nil {
			reason = *answer.Error
		}
		return nil, fmt.Errorf("engine: %s", reason)
	}
	return answer.Response, nil
}

func (e *engineSession) stop() {
	if e.Command == "" {
		return
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(e.Command + "?method=stop")
	if err != nil {
		log.Printf("[survey] stopping an engine session: %v", err)
		return
	}
	resp.Body.Close()
}

// waitForSwitches holds a survey while a pick or a probe opens a source, so the two never add up.
func (h *hub) waitForSwitches() {
	for {
		h.mu.Lock()
		busy := h.probing != ""
		h.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Second)
	}
}
