// Package stream is the proxy Jellyfin plays channels from: one engine session per channel, shared by every viewer.
package stream

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/mpegts"
	"github.com/PlatanosVerdes/bifrost/internal/slate"
)

const (
	retryInterval = 2 * time.Second
	logEvery      = 10 * time.Second
	firstReadSize = 64 * 1024
	chunkSize     = 32 * 1024
)

// One upstream per channel, shared: the engine serves one connection per channel, and Jellyfin's codec
// probe got a 500 and made it transcode blind, at half real time on a Pi 4.
type Hub struct {
	base    string
	maxWait time.Duration
	linger  time.Duration
	client  *http.Client

	// channelWait stays below Jellyfin's 100 s timeout.
	table       *catalog.Catalog
	sourceWait  time.Duration
	channelWait time.Duration

	mu       sync.Mutex
	streams  map[string]*stream
	lastGood map[string]string // slug -> the content id that last produced video
	status   *statusBoard

	jellyfinURL, jellyfinKey string // only for /api/status to ask how Jellyfin is playing
	name                     string
	schedule                 *catalog.Schedule
	swarmEvery               time.Duration

	policy        Policy
	probing       string                          // the slug probing another source: one probe at a time, Pi-wide
	lastProbe     map[string]time.Time            // slug -> when its last probe ended
	probedAt      map[string]map[string]time.Time // slug -> content id -> when it was last probed
	switches      map[string]int                  // slug -> source switches since bifrost started
	remuxRestarts map[string]int
	facts         map[string]*SourceFacts // content id -> what was last seen of it
	factsDirty    bool
	probes        atomic.Int64

	ffmpeg    string // remuxes every source when set; empty relays sources raw
	thumbs    *thumbs
	surveys   map[string]*SurveyState // slug -> the check of its sources running now
	surveyMax int                     // channels checked at once, by MACHINE_SIZE
	backlog   int                     // chunks a viewer may fall behind, by MACHINE_SIZE
	slate     *slate.Slate            // played once a channel's sources have all failed; needs ffmpeg
	loading   *slate.Slate
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
	h     *Hub
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

func newHub(base string, maxWait, linger time.Duration) *Hub {
	return &Hub{base: base, maxWait: maxWait, linger: linger, table: &catalog.Catalog{},
		client: &http.Client{Transport: engineTransport()}, streams: map[string]*stream{}, lastGood: map[string]string{}, status: &statusBoard{},
		schedule: &catalog.Schedule{}, swarmEvery: 5 * time.Second, surveys: map[string]*SurveyState{}, surveyMax: 1, backlog: machineSizes["small"].ViewerBacklog / chunkSize, policy: DefaultPolicy, lastProbe: map[string]time.Time{},
		probedAt: map[string]map[string]time.Time{}, switches: map[string]int{}, remuxRestarts: map[string]int{},
		facts: map[string]*SourceFacts{}}
}

// The engine answers its redirects with no length and keeps the connection open, so a redirect on
// a reused connection waits for a body that never ends: each open gets a connection of its own.
func engineTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableKeepAlives = true
	return t
}

func (h *Hub) planFor(r *http.Request) (plan, bool) {
	if slug, ok := strings.CutPrefix(r.URL.Path, "/channel/"); ok {
		name, ids := h.table.Lookup(slug)
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

func (h *Hub) label(id string) string {
	if name := h.table.NameOf(id); name != "" {
		return name + " (" + short(id) + ")"
	}
	return short(id)
}

// Active lists the channels being watched, so the list loop does not drop one mid-broadcast.
func (h *Hub) Active() map[string]bool {
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

func (h *Hub) Serve(w http.ResponseWriter, r *http.Request) {
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

func (h *Hub) join(p plan, r *http.Request) (*stream, chan []byte, bool) {
	viewer := make(chan []byte, h.backlog)
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
		handover: make(chan handover), swarm: &swarm{threshold: h.policy.StarvingBelow}, opened: time.Now()}
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
	s.h.status.opened(OpenRecord{Channel: s.label, Slug: s.slug, At: start, Outcome: outcome, Seconds: tenths(time.Since(start))})
}

func (s *stream) started(src found, start time.Time) {
	log.Printf("[proxy] %s playing after %.1fs", s.label, tenths(time.Since(start)))
	s.mu.Lock()
	s.source, s.playingSince, s.startedAfterS = src.up.id, time.Now(), tenths(time.Since(start))
	s.mu.Unlock()
	s.h.status.opened(OpenRecord{Channel: s.label, Slug: s.slug, Source: short(src.up.id), At: start, Outcome: "playing", Seconds: s.startedAfterS})
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
	player := &slate.Player{Slate: s.h.loading, Lead: slate.Lead}
	if player.Slate == nil {
		player.Slate = s.h.slate
	}
	spliceNext := false
	tick := time.NewTicker(slate.Tick)
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
		next, src, retry = nil, nil, time.After(slate.Retry)
	}
	for {
		select {
		case now := <-tick.C:
			if b, fresh := player.Next(now); len(b) > 0 {
				switch {
				case spliceNext:
					spliceNext = false
					s.broadcast(reb.splice(b))
				case fresh && player.Sent > int64(len(b)):
					s.broadcast(reb.advance(player.Slate.Ticks, b))
				default:
					s.broadcast(reb.push(b))
				}
			}
		case r := <-results:
			if r == nil {
				if !gaveUp && ctx.Err() == nil {
					s.gaveUp(ctx, start)
					log.Printf("[proxy] %s keeps the slate, looking again every %s", s.label, slate.Retry)
					gaveUp = true
					if player.Slate != s.h.slate {
						player, spliceNext = &slate.Player{Slate: s.h.slate}, true
					}
				}
				retry = time.After(slate.Retry)
				continue
			}
			mux := s.remux(r.up.ctx, r.up)
			if mux == nil {
				r.up.cancel()
				retry = time.After(slate.Retry)
				continue
			}
			src = r
			next = &cutover{mux: mux, align: s.sourceIn(r.up.id, true), timer: time.NewTimer(remuxStartWait)}
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
	s.relayFrom(ctx, up, start, s.sourceIn(up.id, false), newRebaser(), s.remux(ctx, up))
}

func (s *stream) relayFrom(ctx context.Context, up *upstream, start time.Time, align *sourceIn, reb *tsRebaser, mux *remux) {
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
				up, align = h.up, s.sourceIn(h.up.id, true)
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
		s.h.noteSource(up.id, func(f *SourceFacts) { f.Height, f.Audio = height, audio })
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

// sourceIn turns one source's bytes into whole TS packets, without any stretch it plays again.
type sourceIn struct {
	s     *stream
	id    string
	align mpegts.Aligner
	fwd   mpegts.Forward
}

func (s *stream) sourceIn(id string, dropLead bool) *sourceIn {
	return &sourceIn{s: s, id: id, align: mpegts.Aligner{DropLead: dropLead}}
}

func (in *sourceIn) push(b []byte) []byte {
	out, back := in.fwd.Push(in.align.Push(b))
	if back > 0 {
		went := time.Duration(back) * time.Second / 90000
		log.Printf("[proxy] %s %s went back %s to play it again, dropped until it is past what was sent", in.s.label, short(in.id), went.Round(100*time.Millisecond))
		in.s.looped(in.id, went)
	}
	return out
}

// looped notes a source going back to play a stretch again. The playing one owes that stretch, so the
// search for a better source starts at its next sample instead of after its viewers have waited.
func (s *stream) looped(id string, back time.Duration) {
	now := time.Now()
	s.h.noteSource(id, func(f *SourceFacts) {
		if now.Sub(f.LoopedAt) > recentlyFailed {
			f.Loops = 0
		}
		f.Loops++
		f.LoopedAt = now
	})
	s.mu.Lock()
	w, playing := s.swarm, s.source == id
	s.mu.Unlock()
	if playing {
		w.owe(back)
	}
}

// The playing source keeps playing until the handed-over one's remux delivers.
type cutover struct {
	h     handover
	mux   *remux
	align *sourceIn
	timer *time.Timer
	held  []byte // its output so far, played once it can be spliced
}

func (s *stream) startCutover(h handover) *cutover {
	c := &cutover{h: h, align: s.sourceIn(h.up.id, true), timer: time.NewTimer(remuxStartWait)}
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
			log.Printf("[proxy] %s dropped a viewer that fell %d chunks behind", s.label, cap(v))
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

// Config is what main sets up a hub with.
type Config struct {
	Engine      string
	MaxWait     time.Duration
	Linger      time.Duration
	SourceWait  time.Duration
	ChannelWait time.Duration
	JellyfinURL string
	JellyfinKey string
	Policy      Policy
	FFmpeg      string // empty relays sources raw
	Ladder      bool
	Machine     Machine
	ThumbsDir   string
	Slate       *slate.Slate // both need ffmpeg; nil plays none
	Loading     *slate.Slate
	Catalog     *catalog.Catalog
	Schedule    *catalog.Schedule
}

func NewHub(cfg Config) *Hub {
	h := newHub(cfg.Engine, cfg.MaxWait, cfg.Linger)
	h.table, h.schedule = cfg.Catalog, cfg.Schedule
	h.sourceWait, h.channelWait = cfg.SourceWait, cfg.ChannelWait
	h.jellyfinURL, h.jellyfinKey = cfg.JellyfinURL, cfg.JellyfinKey
	h.policy, h.ffmpeg, h.ladder = cfg.Policy, cfg.FFmpeg, cfg.Ladder
	if cfg.Machine.Surveys > 0 {
		h.surveyMax, h.backlog = cfg.Machine.Surveys, cfg.Machine.ViewerBacklog/chunkSize
	}
	h.thumbs = newThumbs(cfg.ThumbsDir, cfg.FFmpeg)
	h.slate, h.loading = cfg.Slate, cfg.Loading
	return h
}
