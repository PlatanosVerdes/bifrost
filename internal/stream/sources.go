package stream

import (
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/httpx"
	"github.com/PlatanosVerdes/bifrost/internal/i18n"
)

type SourceFacts struct {
	ID       string    `json:"id"`
	Height   int       `json:"height,omitempty"`
	Audio    string    `json:"audio,omitempty"`
	Headroom float64   `json:"headroom,omitempty"`
	Peers    int       `json:"peers,omitempty"`
	SeenAt   time.Time `json:"seen_at,omitzero"`
	Outcome  string    `json:"outcome,omitempty"` // played, probed, failed
	Kept     bool      `json:"kept,omitempty"`    // dropped by the list, kept until it fails
	ThumbAt  time.Time `json:"thumb_at,omitzero"` // filled for the page, never stored
}

func (h *Hub) noteSource(id string, change func(*SourceFacts)) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.facts[id]
	if f == nil {
		f = &SourceFacts{ID: id}
		h.facts[id] = f
	}
	change(f)
	h.factsDirty = true
}

// Kept on disk so a restart does not forget a dead source; a week unseen is forgotten.
const factsKept = 7 * 24 * time.Hour

func (h *Hub) LoadFacts(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []SourceFacts
	if json.Unmarshal(b, &list) != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range list {
		if time.Since(f.SeenAt) < factsKept {
			h.facts[f.ID] = &f
		}
	}
}

func (h *Hub) saveFacts(path string) error {
	h.mu.Lock()
	if !h.factsDirty {
		h.mu.Unlock()
		return nil
	}
	list := make([]SourceFacts, 0, len(h.facts))
	for id, f := range h.facts {
		if time.Since(f.SeenAt) >= factsKept {
			delete(h.facts, id)
			continue
		}
		list = append(list, *f)
	}
	h.factsDirty = false
	h.mu.Unlock()
	b, _ := json.Marshal(list)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (h *Hub) KeepFacts(path string, every time.Duration) {
	for range time.Tick(every) {
		if err := h.saveFacts(path); err != nil {
			log.Printf("warning: saving source facts: %v", err)
		}
	}
}

// Failed says whether a source's last open failed, so the list's memory can let it go.
func (h *Hub) Failed(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.facts[id]
	return f != nil && f.Outcome == "failed"
}

func (h *Hub) noteOutcome(id, outcome string) {
	h.noteSource(id, func(f *SourceFacts) { f.Outcome, f.SeenAt = outcome, time.Now() })
}

// The playlist's order, so the page numbers a source the same whichever plays.
func (h *Hub) sourcesOf(ids []string) []SourceFacts {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]SourceFacts, len(ids))
	for i, id := range ids {
		out[i] = SourceFacts{ID: id}
		if f := h.facts[id]; f != nil {
			out[i] = *f
		}
	}
	return out
}

var (
	videoInfo = regexp.MustCompile(`Stream #0:\d+.*: Video: .*?, (\d{3,5})x(\d{3,5})[ ,]`)
	audioInfo = regexp.MustCompile(`Stream #0:\d+(?:\[\w+\])?(?:\((\w+)\))?: Audio: (\w+)[^,]*, \d+ Hz, ([^,]+)`)
)

// streamInfo reads one line of ffmpeg's listing: a video's height, or an audio track as "AAC stereo
// (spa)", its language when the source names one.
func streamInfo(line string) (height int, audio string) {
	if m := videoInfo.FindStringSubmatch(line); m != nil {
		fmt.Sscan(m[2], &height)
	}
	if m := audioInfo.FindStringSubmatch(line); m != nil {
		layout, _, _ := strings.Cut(strings.TrimSpace(m[3]), "(")
		audio = strings.ToUpper(m[2]) + " " + layout
		if m[1] != "" && m[1] != "und" {
			audio += " (" + m[1] + ")"
		}
	}
	return height, audio
}

// listing collects a source's height and every audio track from ffmpeg's stream listing, since a
// source may carry several languages and the remux passes them all on.
type listing struct {
	height int
	audio  []string
}

func (l *listing) read(line string) {
	h, a := streamInfo(line)
	if h > 0 && l.height == 0 {
		l.height = h
	}
	if a != "" {
		l.audio = append(l.audio, a)
	}
}

func (l *listing) audios() string {
	return strings.Join(l.audio, ", ")
}

func (h *Hub) ServePick(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, i18n.T(r, "Send the source as JSON"))
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		httpx.WriteError(w, http.StatusForbidden, i18n.T(r, "That request came from another site"))
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, i18n.T(r, "The source could not be read"))
		return
	}
	slug := r.PathValue("slug")
	_, ids := h.table.Lookup(slug)
	n := slices.Index(ids, body.ID) + 1
	if n == 0 {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, "That is not one of this channel's sources"))
		return
	}

	h.mu.Lock()
	s, busy := h.streams["channel:"+slug], h.probing != ""
	if s != nil && !busy {
		h.probing = slug
	}
	h.mu.Unlock()
	switch {
	case s == nil:
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "The channel is not playing"))
		return
	case busy:
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "Another switch is running, try again in a few seconds"))
		return
	}
	// Ends like a probe, so the automatic switch waits its cooldown before overriding the pick.
	defer func() {
		h.mu.Lock()
		h.probing = ""
		h.lastProbe[slug] = time.Now()
		h.mu.Unlock()
	}()
	s.mu.Lock()
	current := s.source
	s.mu.Unlock()
	if current == body.ID {
		httpx.WriteJSON(w, map[string]string{"source": body.ID})
		return
	}

	log.Printf("[proxy] %s source %d (%s) picked on the page", s.label, n, short(body.ID))
	failed := i18n.T(r, "Source %d did not start", n)
	start := time.Now()
	openBy := start.Add(min(h.sourceWait, probeOpenWait))
	label := s.label + " [pick " + short(body.ID) + "]"
	target := fmt.Sprintf("%s/ace/getstream?id=%s&pid=bifrost-probe-%d", h.base, body.ID, h.probes.Add(1))
	resp, session, ok := openEngine(s.ctx, h.client, target, s.headers, label, openBy)
	if !ok {
		h.noteOutcome(body.ID, "failed")
		httpx.WriteError(w, http.StatusBadGateway, failed)
		return
	}
	first, ok := awaitFirstBytes(s.ctx, resp.Body, label, start, openBy)
	if !ok {
		resp.Body.Close()
		session.stop()
		h.noteOutcome(body.ID, "failed")
		httpx.WriteError(w, http.StatusBadGateway, failed)
		return
	}
	hv := handover{up: feed(s.ctx, body.ID, resp, first, session), reason: "picked on the page", outcome: make(chan string, 1)}
	select {
	case s.handover <- hv:
	case <-s.ctx.Done():
		hv.up.cancel()
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "The channel stopped playing"))
		return
	case <-time.After(probeOpenWait):
		hv.up.cancel()
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "The channel is busy, try again in a few seconds"))
		return
	}
	if reason := <-hv.outcome; reason != "" {
		log.Printf("[proxy] %s kept %s: pick %s %s", s.label, short(current), short(body.ID), reason)
		h.noteOutcome(body.ID, "failed")
		httpx.WriteError(w, http.StatusBadGateway, failed)
		return
	}
	httpx.WriteJSON(w, map[string]string{"source": body.ID})
}
