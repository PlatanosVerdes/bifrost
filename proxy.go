package main

import (
	"context"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	retryInterval = 2 * time.Second
	logEvery      = 10 * time.Second // progress lines, so a 45s wait is 4 lines and not 22
	firstReadSize = 64 * 1024
	chunkSize     = 32 * 1024
	// Chunks a viewer may fall behind before it is dropped: 8 MB, about 9 s of a 7 Mbit/s channel.
	// The chunks are shared, so this bounds memory per channel, not per viewer.
	viewerBacklog = 256
)

// Aceserve answers 302 within half a second for every content id, whether or not a swarm exists,
// so its status code says nothing about whether the channel will play. The only honest signal is
// video actually arriving, which is what this waits for. Without it a dead channel leaves the
// player spinning forever with no error and nothing in the log.
//
// One upstream per channel, shared by every viewer. The engine serves a channel to one connection
// at a time, and Jellyfin opens two at once, one to play and one to probe the codecs: the probe got
// the 500, Jellyfin transcoded blind, and a Pi 4 re-encodes 1080p at half real time.
type hub struct {
	base    string
	maxWait time.Duration
	linger  time.Duration
	client  *http.Client

	// /channel/<slug>: one stable URL per channel, tried source by source. Below Jellyfin's 100 s.
	table       *catalog
	sourceWait  time.Duration
	channelWait time.Duration

	mu       sync.Mutex
	streams  map[string]*stream
	lastGood map[string]string // slug -> the content id that last produced video
	status   *statusBoard

	jellyfinURL, jellyfinKey string // only for /api/status to ask how Jellyfin is playing
	name                     string // the machine, shown on the page
	schedule                 *schedule
	swarmEvery               time.Duration

	policy        switchPolicy
	probing       string                          // the slug probing another source: one probe at a time, Pi-wide
	lastProbe     map[string]time.Time            // slug -> when its last probe ended
	probedAt      map[string]map[string]time.Time // slug -> content id -> when it was last probed
	switches      map[string]int                  // slug -> source switches since bifrost started
	remuxRestarts map[string]int
	facts         map[string]*sourceFacts // content id -> what was last seen of it
	probes        atomic.Int64

	ffmpeg string // remuxes every source when set; empty relays sources raw
}

// plan is what a request resolves to: the sources to try, in order, for one shared stream.
type plan struct {
	key       string
	label     string
	slug      string
	sources   []string // content ids
	budget    time.Duration
	perSource time.Duration
}

type stream struct {
	h     *hub
	id    string
	label string

	ready  chan struct{} // closed once the start has either produced video or failed
	ok     bool
	header http.Header

	mu            sync.Mutex
	viewers       map[chan []byte]struct{}
	direct        map[chan []byte]struct{} // viewers that are not Jellyfin, which the orphan check never closes
	cancel        context.CancelFunc
	idle          *time.Timer
	done          bool
	slug          string
	source        string
	playingSince  time.Time
	startedAfterS float64

	ctx        context.Context
	headers    http.Header
	handover   chan handover
	probing    string // the content id being probed
	lastProbe  *probeRecord
	lastSwitch *switchRecord
	switches   int

	relayed atomic.Int64
	swarm   *swarm
}

func newHub(base string, maxWait, linger time.Duration) *hub {
	return &hub{base: base, maxWait: maxWait, linger: linger, table: &catalog{},
		client: &http.Client{Transport: engineTransport()}, streams: map[string]*stream{}, lastGood: map[string]string{}, status: &statusBoard{},
		schedule: &schedule{}, swarmEvery: 5 * time.Second, policy: defaultPolicy, lastProbe: map[string]time.Time{},
		probedAt: map[string]map[string]time.Time{}, switches: map[string]int{}, remuxRestarts: map[string]int{},
		facts: map[string]*sourceFacts{}}
}

// The engine answers its redirects with no length and keeps the connection open, so a redirect on
// a reused connection waits for a body that never ends: each open gets a connection of its own.
func engineTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

func (h *hub) planFor(r *http.Request) (plan, bool) {
	if slug, ok := strings.CutPrefix(r.URL.Path, "/channel/"); ok {
		name, ids := h.table.lookup(slug)
		if len(ids) == 0 {
			return plan{label: slug}, false
		}
		return plan{key: "channel:" + slug, label: name, slug: slug, sources: h.ordered(slug, ids),
			budget: h.channelWait, perSource: h.sourceWait}, true
	}
	id := contentID(r.RequestURI)
	if id == "" {
		return plan{label: r.URL.Path}, false
	}
	return plan{key: id, label: h.label(id), sources: []string{id}, budget: h.maxWait, perSource: h.maxWait}, true
}

func (h *hub) label(id string) string {
	if name := h.table.nameOf(id); name != "" {
		return name + " (" + short(id) + ")"
	}
	return short(id)
}

// active lists the channels someone is watching right now, so the list loop does not take one away
// from Jellyfin in the middle of a match just because the list skipped it for a pass.
func (h *hub) active() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for key, s := range h.streams {
		if slug, ok := strings.CutPrefix(key, "channel:"); ok {
			s.mu.Lock()
			if !s.done {
				out[slug] = true
			}
			s.mu.Unlock()
		}
	}
	return out
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	p, ok := h.planFor(r)
	if !ok {
		log.Printf("[proxy] %s is not in the channel table", p.label)
		http.Error(w, "unknown channel", http.StatusNotFound)
		return
	}
	// Jellyfin asks HEAD before playing, then sends GET on the same connection: a HEAD that streamed
	// would hold that connection and leave the GET waiting until Jellyfin's 100s timeout.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		return
	}
	s, viewer, shared := h.join(p, r)
	if shared {
		log.Printf("[proxy] %s joined the running stream (%d watching, %s)", s.label, s.count(), client(r))
	} else {
		log.Printf("[proxy] %s requested (%s)", s.label, client(r))
	}
	defer s.leave(viewer)

	select {
	case <-s.ready:
	case <-r.Context().Done():
		return
	}
	if !s.ok {
		http.Error(w, "stream unavailable", http.StatusServiceUnavailable)
		return
	}

	for k, vals := range s.header {
		// A live stream has no length, and every viewer joins at its own point of it.
		if k == "Content-Length" || k == "Content-Range" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	played := time.Now()
	for {
		select {
		case chunk, open := <-viewer:
			if !open {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				log.Printf("[proxy] %s stream stopped after %s: %v", s.label, time.Since(played).Round(time.Second), err)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			log.Printf("[proxy] %s viewer left after %s", s.label, time.Since(played).Round(time.Second))
			return
		}
	}
}

// join returns the channel's stream, starting it if nobody is on it yet, plus a fresh viewer queue.
func (h *hub) join(p plan, r *http.Request) (*stream, chan []byte, bool) {
	viewer := make(chan []byte, viewerBacklog)
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.streams[p.key]; ok {
		s.mu.Lock()
		if !s.done {
			s.viewers[viewer] = struct{}{}
			s.noteDirect(viewer, r)
			if s.idle != nil {
				s.idle.Stop()
				s.idle = nil
			}
			s.mu.Unlock()
			return s, viewer, true
		}
		s.mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	headers := r.Header.Clone()
	headers.Del("Range") // live: there is nothing to seek to, and joiners never asked for it
	for _, k := range []string{"Cf-Access-Jwt-Assertion", "Cookie", "Authorization"} {
		headers.Del(k)
	}
	s := &stream{h: h, id: p.key, label: p.label, slug: p.slug, ready: make(chan struct{}),
		viewers: map[chan []byte]struct{}{viewer: {}}, cancel: cancel, ctx: ctx, headers: headers,
		handover: make(chan handover), swarm: &swarm{threshold: h.policy.starvingBelow}}
	s.noteDirect(viewer, r)
	h.streams[p.key] = s
	go s.run(ctx, p, headers)
	return s, viewer, false
}

// run tries the plan's sources one at a time, never two at once: the Pi plays one channel.
func (s *stream) run(ctx context.Context, p plan, headers http.Header) {
	defer s.finish()
	start := time.Now()
	deadline := start.Add(p.budget)

	var resp *http.Response
	var first []byte
	var chosen string
	for i, id := range p.sources {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		srcDeadline := time.Now().Add(p.perSource)
		if srcDeadline.After(deadline) {
			srcDeadline = deadline
		}
		label := s.label
		if len(p.sources) > 1 {
			label = s.label + " [" + short(id) + "]"
		}
		target := s.h.base + "/ace/getstream?id=" + id
		r, ok := open(ctx, s.h.client, target, headers, label, srcDeadline)
		if !ok {
			if ctx.Err() == nil {
				s.h.noteOutcome(id, "failed")
			}
			continue
		}
		b, ok := awaitFirstBytes(ctx, r.Body, label, time.Now(), srcDeadline)
		if !ok {
			r.Body.Close()
			if ctx.Err() == nil {
				s.h.noteOutcome(id, "failed")
			}
			if i < len(p.sources)-1 && ctx.Err() == nil {
				log.Printf("[proxy] %s no video, trying the next source", label)
			}
			continue
		}
		resp, first, chosen = r, b, id
		s.h.noteOutcome(id, "played")
		if p.slug != "" {
			s.h.mu.Lock()
			s.h.lastGood[p.slug] = id
			s.h.mu.Unlock()
		}
		break
	}
	if resp == nil {
		outcome := "abandoned"
		if ctx.Err() == nil {
			outcome = "gave_up"
			log.Printf("[proxy] %s gave up, no source produced video in %s", s.label, time.Since(start).Round(time.Second))
		}
		s.h.status.opened(openRecord{Channel: s.label, Slug: s.slug, At: start, Outcome: outcome, Seconds: tenths(time.Since(start))})
		close(s.ready)
		return
	}
	log.Printf("[proxy] %s playing after %.1fs", s.label, tenths(time.Since(start)))
	s.mu.Lock()
	s.source, s.playingSince, s.startedAfterS = chosen, time.Now(), tenths(time.Since(start))
	s.mu.Unlock()
	s.h.status.opened(openRecord{Channel: s.label, Slug: s.slug, Source: short(chosen), At: start, Outcome: "playing", Seconds: s.startedAfterS})
	go s.h.noteDevice(start, s.label)
	s.ok, s.header = true, resp.Header.Clone()
	close(s.ready)
	up := feed(ctx, chosen, resp, first)
	if up.stat != "" {
		go s.watchSwarm(up.ctx, up.stat, s.swarm)
	}
	s.relay(ctx, up, start)
}

// relay hands the source's chunks to the viewers, packet-aligned, until a probe hands over a better
// source: from then on the same viewers get that one, and the old session is closed.
func (s *stream) relay(ctx context.Context, up *upstream, start time.Time) {
	align := &tsAligner{}
	reb := newRebaser()
	mux := s.remux(ctx, up)
	var next *cutover
	stuck := time.NewTicker(time.Second)
	defer stuck.Stop()
	for {
		select {
		case chunk, open := <-mux.feed(up.chunks):
			if !open {
				if ctx.Err() == nil {
					log.Printf("[proxy] %s engine stream ended after %s: %v", s.label, time.Since(start).Round(time.Second), up.err)
				}
				up.cancel()
				mux.stop()
				next.abandon("the playing source ended")
				return
			}
			out := align.push(chunk)
			if len(out) == 0 {
				continue
			}
			s.relayed.Add(int64(len(out)))
			if mux == nil {
				s.broadcast(out)
			} else {
				mux.write(out)
			}
		case <-stuck.C:
			if mux.stuckFor() > remuxStuckAfter {
				log.Printf("[proxy] %s remux took nothing for %s, restarting it", s.label, remuxStuckAfter)
				s.remuxRestarted()
				mux.stop()
				mux, reb.restart = s.remux(ctx, up), []byte{}
			}
			if next != nil && next.mux.stuckFor() > remuxStuckAfter {
				next.abandon("its remux took nothing for " + remuxStuckAfter.String())
				next = nil
			}
		case out, open := <-mux.output():
			if !open {
				switch {
				case ctx.Err() != nil:
				case !mux.delivered:
					log.Printf("[proxy] %s plays raw, its remux delivered nothing", s.label)
					mux = nil
				default:
					log.Printf("[proxy] %s remux ended, restarting it", s.label)
					s.remuxRestarted()
					mux, reb.restart = s.remux(ctx, up), []byte{}
				}
				continue
			}
			mux.delivered = true
			if reb.restart == nil {
				s.broadcast(reb.push(out))
				continue
			}
			if reb.restart = append(reb.restart, out...); reb.ready(reb.restart) {
				s.broadcast(reb.splice(reb.restart))
				reb.restart = nil
			}
		case h := <-s.handover:
			if mux == nil {
				s.switchTo(up, h)
				up, align = h.up, &tsAligner{dropLead: true}
				continue
			}
			next.abandon("another source was handed over")
			next = s.startCutover(h)
		case chunk, open := <-next.input():
			if !open {
				next.abandon("the new source ended before its remux delivered")
				next = nil
				continue
			}
			if out := next.align.push(chunk); len(out) > 0 {
				next.h.drained += int64(len(out))
				next.mux.write(out)
			}
		case out, open := <-next.output():
			if !open {
				next.abandon("its remux ended before it delivered")
				next = nil
				continue
			}
			if next.held = append(next.held, out...); !reb.ready(next.held) {
				continue
			}
			s.switchTo(up, next.h)
			mux.stop()
			next.timer.Stop()
			up, align, mux = next.h.up, next.align, next.mux
			mux.delivered, reb.restart = true, nil
			s.broadcast(reb.splice(next.held))
			next = nil
		case <-next.expired():
			next.abandon("its remux delivered nothing in " + remuxStartWait.String())
			next = nil
		case <-ctx.Done():
			up.cancel()
			mux.stop()
			next.abandon("the stream closed")
			return
		}
	}
}

// nil relays the source raw, as before remuxing.
func (s *stream) remux(ctx context.Context, up *upstream) *remux {
	if s.h.ffmpeg == "" {
		return nil
	}
	r, err := startRemux(ctx, s.h.ffmpeg, s.label+" ["+short(up.id)+"]", func(height int, audio string) {
		s.h.noteSource(up.id, func(f *sourceFacts) { f.Height, f.Audio = height, audio })
	})
	if err != nil {
		log.Printf("[proxy] %s plays raw, ffmpeg did not start: %v", s.label, err)
		return nil
	}
	return r
}

func (s *stream) remuxRestarted() {
	s.h.mu.Lock()
	s.h.remuxRestarts[s.slug]++
	s.h.mu.Unlock()
}

func (r *remux) output() <-chan []byte {
	if r == nil {
		return nil
	}
	return r.out
}

// The playing source keeps playing until the handed-over one's remux delivers.
type cutover struct {
	h     handover
	mux   *remux
	align *tsAligner
	timer *time.Timer
	held  []byte // its output so far, played once it can be spliced
}

func (s *stream) startCutover(h handover) *cutover {
	c := &cutover{h: h, align: &tsAligner{dropLead: true}, timer: time.NewTimer(remuxStartWait)}
	if c.mux = s.remux(h.up.ctx, h.up); c.mux == nil {
		c.abandon("ffmpeg did not start")
		return nil
	}
	return c
}

func (c *cutover) input() <-chan []byte {
	if c == nil {
		return nil
	}
	return c.mux.feed(c.h.up.chunks)
}

func (c *cutover) output() <-chan []byte {
	if c == nil {
		return nil
	}
	return c.mux.out
}

func (c *cutover) expired() <-chan time.Time {
	if c == nil {
		return nil
	}
	return c.timer.C
}

func (c *cutover) abandon(reason string) {
	if c == nil {
		return
	}
	c.timer.Stop()
	c.mux.stop()
	c.h.up.cancel()
	c.h.outcome <- reason
}

// broadcast never blocks on a viewer: one that fell a whole backlog behind is dropped, so a stalled
// client cannot freeze the channel for everyone else.
func (s *stream) broadcast(chunk []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for v := range s.viewers {
		select {
		case v <- chunk:
		default:
			log.Printf("[proxy] %s dropped a viewer that fell %d chunks behind", s.label, viewerBacklog)
			delete(s.viewers, v)
			delete(s.direct, v)
			close(v)
		}
	}
}

// leave lets the upstream go once the last viewer has been gone for the linger period: Jellyfin
// closes and reopens within seconds when it switches from probing to playing.
func (s *stream) leave(viewer chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.viewers[viewer]; ok {
		delete(s.viewers, viewer)
	}
	delete(s.direct, viewer)
	if len(s.viewers) > 0 || s.done || s.idle != nil {
		return
	}
	s.idle = time.AfterFunc(s.h.linger, func() {
		s.mu.Lock()
		empty := len(s.viewers) == 0
		s.mu.Unlock()
		if empty {
			s.cancel()
		}
	})
}

func (s *stream) finish() {
	s.h.mu.Lock()
	if s.h.streams[s.id] == s {
		delete(s.h.streams, s.id)
	}
	s.h.mu.Unlock()

	s.mu.Lock()
	s.done = true
	for v := range s.viewers {
		close(v)
	}
	s.viewers = map[chan []byte]struct{}{}
	s.direct = map[chan []byte]struct{}{}
	if s.idle != nil {
		s.idle.Stop()
	}
	s.mu.Unlock()
	s.cancel()
}

// noteDirect remembers a viewer that is not Jellyfin. Called with s.mu held, or before s is shared.
func (s *stream) noteDirect(viewer chan []byte, r *http.Request) {
	if fromJellyfin(r) {
		return
	}
	if s.direct == nil {
		s.direct = map[chan []byte]struct{}{}
	}
	s.direct[viewer] = struct{}{}
}

func (s *stream) directViewers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.direct)
}

func (s *stream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.viewers)
}

// Aceserve answers 500 while it cannot get a stream going at all, which covers both an engine
// still starting and a content id it fails to resolve. Either is worth retrying. Anything else is
// answered straight away and judged on whether video follows.
func open(ctx context.Context, client *http.Client, target string, headers http.Header, ch string, deadline time.Time) (*http.Response, bool) {
	var lastLog time.Time
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			log.Printf("[proxy] %s bad request: %v", ch, err)
			return nil, false
		}
		for k, vals := range headers {
			for _, v := range vals {
				req.Header.Add(k, v)
			}
		}

		resp, err := client.Do(req)
		if err == nil && resp.StatusCode != http.StatusInternalServerError {
			return resp, true
		}
		if resp != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			log.Printf("[proxy] %s abandoned before it started", ch)
			return nil, false
		}
		if time.Now().After(deadline) {
			log.Printf("[proxy] %s gave up, the engine never produced a stream", ch)
			return nil, false
		}
		if time.Since(lastLog) >= logEvery {
			log.Printf("[proxy] %s no stream yet, retrying", ch)
			lastLog = time.Now()
		}
		time.Sleep(retryInterval)
	}
}

// Reads in its own goroutine so the wait can be reported while it blocks. Closing the body on the
// way out unblocks that read, and the channel is buffered so it never leaks.
func awaitFirstBytes(ctx context.Context, body io.Reader, ch string, start, deadline time.Time) ([]byte, bool) {
	type result struct {
		buf []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf := make([]byte, firstReadSize)
		n, err := io.ReadAtLeast(body, buf, 1)
		done <- result{buf[:n], err}
	}()

	ticker := time.NewTicker(logEvery)
	defer ticker.Stop()
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()

	for {
		select {
		case res := <-done:
			if res.err != nil && len(res.buf) == 0 {
				log.Printf("[proxy] %s stream closed without sending anything: %v", ch, res.err)
				return nil, false
			}
			return res.buf, true
		case <-ticker.C:
			log.Printf("[proxy] %s waiting for the swarm, %s of %s elapsed",
				ch, time.Since(start).Round(time.Second), deadline.Sub(start))
		case <-ctx.Done():
			log.Printf("[proxy] %s abandoned while waiting, after %s", ch, time.Since(start).Round(time.Second))
			return nil, false
		case <-timeout.C:
			return nil, false
		}
	}
}

// tenths is a duration in seconds to one decimal, so a start under a second does not read as 0.
func tenths(d time.Duration) float64 {
	return math.Round(d.Seconds()*10) / 10
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func contentID(requestURI string) string {
	u, err := url.Parse(requestURI)
	if err != nil {
		return ""
	}
	return u.Query().Get("id")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getduration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("[proxy] %s=%q is not a duration, using %s", key, v, def)
	}
	return def
}

func getfloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		log.Printf("[proxy] %s=%q is not a number, using %g", key, v, def)
	}
	return def
}
