package main

import (
	"context"
	"fmt"
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
	logEvery      = 10 * time.Second
	firstReadSize = 64 * 1024
	chunkSize     = 32 * 1024
	// A viewer this far behind (32 MB, about 30 s of an 8 Mbit/s channel) is dropped: Jellyfin pauses
	// reading for seconds while it refreshes its guide. Chunks are shared, so this bounds memory per channel.
	viewerBacklog = 1024
)

// One upstream per channel, shared: the engine serves one connection per channel, and Jellyfin's codec
// probe got a 500 and made it transcode blind, at half real time on a Pi 4.
type hub struct {
	base    string
	maxWait time.Duration
	linger  time.Duration
	client  *http.Client

	// channelWait stays below Jellyfin's 100 s timeout.
	table       *catalog
	sourceWait  time.Duration
	channelWait time.Duration

	mu       sync.Mutex
	streams  map[string]*stream
	lastGood map[string]string // slug -> the content id that last produced video
	status   *statusBoard

	jellyfinURL, jellyfinKey string // only for /api/status to ask how Jellyfin is playing
	name                     string
	schedule                 *schedule
	swarmEvery               time.Duration

	policy        switchPolicy
	probing       string                          // the slug probing another source: one probe at a time, Pi-wide
	lastProbe     map[string]time.Time            // slug -> when its last probe ended
	probedAt      map[string]map[string]time.Time // slug -> content id -> when it was last probed
	switches      map[string]int                  // slug -> source switches since bifrost started
	remuxRestarts map[string]int
	facts         map[string]*sourceFacts // content id -> what was last seen of it
	factsDirty    bool
	probes        atomic.Int64

	ffmpeg    string // remuxes every source when set; empty relays sources raw
	thumbs    *thumbs
	surveys   map[string]*surveyState // slug -> the check of its sources running now
	surveyMax int                     // channels checked at once, by MACHINE_SIZE
	slate     *slate                  // played once a channel's sources have all failed; needs ffmpeg
	loading   *slate
	ladder    bool // a starving channel may step down to its lower versions' sources
}

type plan struct {
	key       string
	label     string
	slug      string
	sources   []string
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
	opened        time.Time
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
		schedule: &schedule{}, swarmEvery: 5 * time.Second, surveys: map[string]*surveyState{}, surveyMax: 1, policy: defaultPolicy, lastProbe: map[string]time.Time{},
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

// active lists the channels being watched, so the list loop does not drop one mid-broadcast.
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
		handover: make(chan handover), swarm: &swarm{threshold: h.policy.starvingBelow}, opened: time.Now()}
	s.noteDirect(viewer, r)
	h.streams[p.key] = s
	go s.run(ctx, p, headers)
	return s, viewer, false
}

// Sources are tried one at a time: the Pi plays one channel.
func (s *stream) run(ctx context.Context, p plan, headers http.Header) {
	defer s.finish()
	start := time.Now()
	if s.h.slate != nil && s.h.ffmpeg != "" {
		s.runSlate(ctx, p, headers, start)
		return
	}
	src, ok := s.findSource(ctx, p, headers)
	if !ok {
		s.gaveUp(ctx, start)
		close(s.ready)
		return
	}
	s.ok, s.header = true, src.header
	close(s.ready)
	s.started(src, start)
	s.relay(ctx, src.up, start)
}

type found struct {
	up     *upstream
	header http.Header
}

func (s *stream) findSource(ctx context.Context, p plan, headers http.Header) (found, bool) {
	deadline := time.Now().Add(p.budget)
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
		// Each stream its own pid: without one the engine takes every open from bifrost for one player,
		// and opening a second channel stopped the first.
		target := fmt.Sprintf("%s/ace/getstream?id=%s&pid=bifrost-play-%d", s.h.base, id, s.h.probes.Add(1))
		r, session, ok := openEngine(ctx, s.h.client, target, headers, label, srcDeadline)
		if !ok {
			if ctx.Err() == nil {
				s.h.noteOutcome(id, "failed")
			}
			continue
		}
		b, ok := awaitFirstBytes(ctx, r.Body, label, time.Now(), srcDeadline)
		if !ok {
			r.Body.Close()
			session.stop()
			if ctx.Err() == nil {
				s.h.noteOutcome(id, "failed")
			}
			if i < len(p.sources)-1 && ctx.Err() == nil {
				log.Printf("[proxy] %s no video, trying the next source", label)
			}
			continue
		}
		s.h.noteOutcome(id, "played")
		if p.slug != "" {
			s.h.mu.Lock()
			s.h.lastGood[p.slug] = id
			s.h.mu.Unlock()
		}
		return found{up: feed(ctx, id, r, b, session), header: r.Header.Clone()}, true
	}
	return found{}, false
}

func (s *stream) gaveUp(ctx context.Context, start time.Time) {
	outcome := "abandoned"
	if ctx.Err() == nil {
		outcome = "gave_up"
		log.Printf("[proxy] %s gave up, no source produced video in %s", s.label, time.Since(start).Round(time.Second))
	}
	s.h.status.opened(openRecord{Channel: s.label, Slug: s.slug, At: start, Outcome: outcome, Seconds: tenths(time.Since(start))})
}

func (s *stream) started(src found, start time.Time) {
	log.Printf("[proxy] %s playing after %.1fs", s.label, tenths(time.Since(start)))
	s.mu.Lock()
	s.source, s.playingSince, s.startedAfterS = src.up.id, time.Now(), tenths(time.Since(start))
	s.mu.Unlock()
	s.h.status.opened(openRecord{Channel: s.label, Slug: s.slug, Source: short(src.up.id), At: start, Outcome: "playing", Seconds: s.startedAfterS})
	go s.h.noteDevice(start, s.label)
	if src.up.stat != "" {
		go s.watchSwarm(src.up.ctx, src.up.stat, s.swarm)
	}
}

// Keeps looking every slateRetry while someone watches; the first source to deliver takes over.
func (s *stream) runSlate(ctx context.Context, p plan, headers http.Header, start time.Time) {
	s.ok, s.header = true, http.Header{"Content-Type": {"video/mp2t"}}
	s.mu.Lock()
	s.playingSince = start
	s.mu.Unlock()
	close(s.ready)
	log.Printf("[proxy] %s playing the slate while it looks for video", s.label)
	reb := newRebaser()
	player := &slatePlayer{sl: s.h.loading, lead: slateLead}
	if player.sl == nil {
		player.sl = s.h.slate
	}
	spliceNext := false
	tick := time.NewTicker(slateTick)
	defer tick.Stop()
	results := make(chan *found, 1)
	search := func() {
		go func() {
			if src, ok := s.findSource(ctx, p, headers); ok {
				results <- &src
			} else {
				results <- nil
			}
		}()
	}
	search()
	var retry <-chan time.Time
	var next *cutover
	var src *found
	gaveUp := false
	fail := func(reason string) {
		log.Printf("[proxy] %s %s, back to the slate", s.label, reason)
		next.timer.Stop()
		next.mux.stop()
		src.up.cancel()
		next, src, retry = nil, nil, time.After(slateRetry)
	}
	for {
		select {
		case now := <-tick.C:
			if b, fresh := player.next(now); len(b) > 0 {
				switch {
				case spliceNext:
					spliceNext = false
					s.broadcast(reb.splice(b))
				case fresh && player.sent > int64(len(b)):
					s.broadcast(reb.advance(player.sl.ticks, b))
				default:
					s.broadcast(reb.push(b))
				}
			}
		case r := <-results:
			if r == nil {
				if !gaveUp && ctx.Err() == nil {
					s.gaveUp(ctx, start)
					log.Printf("[proxy] %s keeps the slate, looking again every %s", s.label, slateRetry)
					gaveUp = true
					if player.sl != s.h.slate {
						player, spliceNext = &slatePlayer{sl: s.h.slate}, true
					}
				}
				retry = time.After(slateRetry)
				continue
			}
			mux := s.remux(r.up.ctx, r.up)
			if mux == nil {
				r.up.cancel()
				retry = time.After(slateRetry)
				continue
			}
			src = r
			next = &cutover{mux: mux, align: &tsAligner{dropLead: true}, timer: time.NewTimer(remuxStartWait)}
		case <-retry:
			retry = nil
			search()
		case chunk, open := <-next.feedFrom(src):
			if !open {
				fail("the source ended before its remux delivered")
				continue
			}
			if out := next.align.push(chunk); len(out) > 0 {
				s.relayed.Add(int64(len(out)))
				next.mux.write(out)
			}
		case out, open := <-next.output():
			if !open {
				fail("its remux ended before it delivered")
				continue
			}
			if next.held = append(next.held, out...); !reb.ready(next.held) {
				continue
			}
			next.timer.Stop()
			next.mux.delivered = true
			s.started(*src, start)
			s.broadcast(reb.splice(next.held))
			s.relayFrom(ctx, src.up, start, next.align, reb, next.mux)
			return
		case <-next.expired():
			fail("its remux delivered nothing in " + remuxStartWait.String())
		case <-ctx.Done():
			if next != nil {
				next.mux.stop()
				src.up.cancel()
			}
			return
		}
	}
}

func (c *cutover) feedFrom(src *found) <-chan []byte {
	if c == nil {
		return nil
	}
	return c.mux.feed(src.up.chunks)
}

// relay serves viewers from up until a probe hands over a better source, then closes the old session.
func (s *stream) relay(ctx context.Context, up *upstream, start time.Time) {
	s.relayFrom(ctx, up, start, &tsAligner{}, newRebaser(), s.remux(ctx, up))
}

func (s *stream) relayFrom(ctx context.Context, up *upstream, start time.Time, align *tsAligner, reb *tsRebaser, mux *remux) {
	var next *cutover
	grab := s.h.thumbs.grab(up.id)
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
			grab.push(out)
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
				grab = s.h.thumbs.grab(up.id)
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
			grab = s.h.thumbs.grab(up.id)
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

// Returns nil when ffmpeg is off or does not start, and the source is relayed raw.
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

// A viewer a whole backlog behind is dropped so a stalled client cannot freeze the channel.
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

// The upstream is released after the linger period: Jellyfin reopens within seconds when it goes
// from probing to playing.
func (s *stream) leave(viewer chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.viewers, viewer)
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

// Called with s.mu held, or before s is shared.
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

// Aceserve answers 500 while the engine starts or an id will not resolve: retried. Any other status
// is judged on whether video follows.
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

// Aceserve answers 302 for every id whether or not a swarm exists, so only arriving video says a
// channel plays. Reads in its own goroutine so the wait can be reported; closing the body unblocks it.
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

// Rounded to 0.1 s so a start under a second does not read as 0.
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
