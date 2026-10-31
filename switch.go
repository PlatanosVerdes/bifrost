package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"slices"
	"time"
)

// A channel's sources do not degrade together (one sat at 0.65-0.95x for minutes while another got
// twice that), so a starving channel probes one other source and splices it in only when clearly better.

const (
	tsPacket = 188
	tsSync   = 0x47
	// How far into a stream the TS sync is searched for.
	tsSearch = 16 * 1024
	// Opening a probe gets at most this, on top of the sampling window.
	probeOpenWait = 15 * time.Second
)

type switchPolicy struct {
	// Video the playing source owes before a probe: it fills fast on a deep starve and slowly on a
	// source that flaps around the bitrate, which no "starving for so long" ever caught. 0 turns it off.
	behind      time.Duration
	judgeOver   time.Duration // the playing source's average over this is what a candidate must beat
	probeFor    time.Duration // how long a candidate is sampled once it delivers
	cooldown    time.Duration // per channel, from the end of one probe to the start of the next
	minHeadroom float64       // a candidate that keeps up needs this
	margin      float64       // and this much over the playing source
	// Download under this times the bitrate counts as short. Only lowered or raised to test.
	starvingBelow float64
}

var defaultPolicy = switchPolicy{behind: 5 * time.Second, judgeOver: 30 * time.Second, probeFor: 20 * time.Second,
	cooldown: 3 * time.Minute, minHeadroom: 1.1, margin: 0.2, starvingBelow: 1}

func (p switchPolicy) probeDue(now time.Time, behind time.Duration, lastProbe time.Time, busy bool, others int) (bool, string) {
	switch {
	case p.behind <= 0:
		return false, "switching is off"
	case others == 0:
		return false, "no other source"
	case behind < p.behind:
		return false, "not far enough behind"
	case busy:
		return false, "a probe is running"
	case !lastProbe.IsZero() && now.Sub(lastProbe) < p.cooldown:
		return false, "cooling down"
	}
	return true, ""
}

// reading is a source's swarm averaged over a window, its download against the channel's bitrate.
type reading struct {
	Headroom float64 `json:"headroom"`
	Peers    float64 `json:"peers"`
	Samples  int     `json:"samples"`
	Stalled  bool    `json:"stalled,omitempty"` // the last sample was not downloading
}

func readingOf(samples []swarmSample, rate float64) reading {
	var r reading
	if rate <= 0 || len(samples) == 0 {
		return r
	}
	var down, peers float64
	for _, s := range samples {
		if s.stat.Status != "dl" {
			continue
		}
		down += float64(s.stat.SpeedDown * kib)
		peers += float64(s.stat.Peers)
		r.Samples++
	}
	r.Stalled = samples[len(samples)-1].stat.Status != "dl"
	if r.Samples > 0 {
		r.Headroom = math.Round(down/float64(r.Samples)/rate*100) / 100
		r.Peers = math.Round(peers/float64(r.Samples)*10) / 10
	}
	return r
}

// candidateReading drops the first sample, taken while the engine fills its opening buffer as fast
// as it can, and measures against the higher of the channel's bitrate and the candidate's own.
func candidateReading(samples []swarmSample, channelRate float64) reading {
	if len(samples) < 2 {
		return reading{}
	}
	return readingOf(samples[1:], math.Max(channelRate, bitrate(samples)))
}

func (p switchPolicy) judge(current, candidate reading, stillStarving bool) (bool, string) {
	need := current.Headroom + p.margin
	// Fewer than half the peers is a thinner swarm, quicker to fall apart: ask for twice the margin.
	if candidate.Peers < current.Peers/2 {
		need += p.margin
	}
	switch {
	case candidate.Samples < 2:
		return false, fmt.Sprintf("too few samples (%d)", candidate.Samples)
	case candidate.Stalled:
		return false, "the candidate stalled"
	// A source that flaps around the bitrate "recovers" between its starves; only its average counts,
	// and one that keeps up on average is left only for clearly more headroom than a step up asks.
	case !stillStarving && current.Headroom >= p.starvingBelow && candidate.Headroom < math.Max(stepUpHeadroom, need+p.margin/2):
		return false, "the playing source recovered"
	case !stillStarving && current.Headroom >= p.starvingBelow:
		return true, fmt.Sprintf("far more headroom, %.2fx against %.2fx", candidate.Headroom, current.Headroom)
	case candidate.Headroom >= math.Max(need, p.minHeadroom):
		return true, fmt.Sprintf("keeps up at %.2fx against %.2fx", candidate.Headroom, current.Headroom)
	// No source keeps up: the least bad one is worth a discontinuity only by a wide margin.
	case candidate.Headroom >= need+p.margin:
		return true, fmt.Sprintf("least bad, %.2fx against %.2fx", candidate.Headroom, current.Headroom)
	case candidate.Headroom < 1:
		return false, fmt.Sprintf("the candidate is short too, %.2fx against %.2fx", candidate.Headroom, current.Headroom)
	}
	return false, fmt.Sprintf("not clearly better, %.2fx against %.2fx, needs %.2fx", candidate.Headroom, current.Headroom,
		math.Max(need, p.minHeadroom))
}

// nextCandidate picks the source to probe: one never probed, in the list's order, else the one
// probed longest ago.
func nextCandidate(ids []string, current string, probedAt map[string]time.Time) string {
	best := ""
	for _, id := range ids {
		if id == current {
			continue
		}
		at, seen := probedAt[id]
		if !seen {
			return id
		}
		if best == "" || at.Before(probedAt[best]) {
			best = id
		}
	}
	return best
}

func others(ids []string, current string) int {
	n := 0
	for _, id := range ids {
		if id != current {
			n++
		}
	}
	return n
}

// Hands out whole 188-byte TS packets so a switch never splices mid-packet. A stream with no sync in
// its first tsSearch bytes is passed through.
type tsAligner struct {
	dropLead bool // a spliced-in source starts at its first sync; the opening one loses nothing
	synced   bool
	raw      bool
	carry    []byte
}

func (a *tsAligner) push(b []byte) []byte {
	if a.raw {
		return b
	}
	data := make([]byte, 0, len(a.carry)+len(b))
	data = append(append(data, a.carry...), b...)
	a.carry = nil
	if !a.synced {
		at := tsSyncAt(data)
		switch {
		case at >= 0:
			a.synced = true
			if a.dropLead {
				data = data[at:]
			} else if at > 0 {
				// Bytes before the first packet go out as they are, and alignment starts after them.
				lead := data[:at]
				rest := a.push(data[at:])
				return append(lead[:len(lead):len(lead)], rest...)
			}
		case len(data) >= tsSearch:
			a.raw = true
			return data
		default:
			a.carry = data
			return nil
		}
	}
	whole := len(data) - len(data)%tsPacket
	if whole < len(data) {
		a.carry = append([]byte(nil), data[whole:]...)
	}
	return data[:whole]
}

// tsSyncAt is the first offset where three packets in a row start with the sync byte.
func tsSyncAt(b []byte) int {
	for i := 0; i+2*tsPacket < len(b); i++ {
		if b[i] == tsSync && b[i+tsPacket] == tsSync && b[i+2*tsPacket] == tsSync {
			return i
		}
	}
	return -1
}

// upstream is one engine session read by its own goroutine, so the relay never waits on a read that a
// starving engine holds.
type upstream struct {
	id     string
	stat   string
	chunks chan []byte // closed when the body ends; err says why
	err    error
	ctx    context.Context
	cancel context.CancelFunc
}

func feed(parent context.Context, id string, resp *http.Response, first []byte, session *engineSession) *upstream {
	ctx, cancel := context.WithCancel(parent)
	context.AfterFunc(ctx, session.stop)
	u := &upstream{id: id, stat: statURL(resp), chunks: make(chan []byte, 4), ctx: ctx, cancel: cancel}
	go u.pump(resp.Body, first)
	return u
}

func (u *upstream) pump(body io.ReadCloser, first []byte) {
	defer close(u.chunks)
	defer body.Close()
	stop := context.AfterFunc(u.ctx, func() { body.Close() })
	defer stop()
	chunk := first
	for {
		if len(chunk) > 0 {
			select {
			case u.chunks <- chunk:
			case <-u.ctx.Done():
				u.err = u.ctx.Err()
				return
			}
		}
		// Whole chunks, so the backlog is the 8 MB it claims: a few KB per read filled its 256 in the opening
		// burst and dropped the TV.
		buf := make([]byte, chunkSize)
		n, err := io.ReadFull(body, buf)
		chunk = buf[:n]
		if err != nil {
			if n > 0 {
				select {
				case u.chunks <- chunk:
				case <-u.ctx.Done():
				}
			}
			u.err = err
			return
		}
	}
}

type handover struct {
	up      *upstream
	samples []swarmSample
	drained int64
	before  reading
	after   reading
	reason  string
	outcome chan string // "" once the relay plays from it, else why it never did
}

type switchRecord struct {
	At             time.Time `json:"at"`
	From           string    `json:"from"`
	To             string    `json:"to"`
	Reason         string    `json:"reason"`
	HeadroomBefore float64   `json:"headroom_before"`
	HeadroomAfter  float64   `json:"headroom_after"`
}

type probeRecord struct {
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Outcome   string    `json:"outcome"` // switched, kept
	Reason    string    `json:"reason"`
	Current   reading   `json:"current"`
	Candidate *reading  `json:"candidate,omitempty"`
}

func (s *stream) maybeProbe(w *swarm) {
	if st := w.status(); st != nil && !st.SampledAt.IsZero() {
		s.mu.Lock()
		playing := s.source
		s.mu.Unlock()
		s.h.noteSource(playing, func(f *sourceFacts) {
			f.Peers, f.SeenAt, f.Outcome = st.Peers, st.SampledAt, "played"
			if st.Headroom > 0 {
				f.Headroom = st.Headroom
			}
		})
	}
	if s.slug == "" {
		return
	}
	_, ids := s.h.table.lookup(s.slug)
	pool := ids
	if s.h.ladder {
		pool = append(slices.Clone(ids), s.h.table.lower(s.slug)...)
	}
	now := time.Now()

	s.h.mu.Lock()
	s.mu.Lock()
	current, switched := s.source, s.lastSwitch
	s.mu.Unlock()
	stepUp := s.h.ladder && !slices.Contains(ids, current) && w.starvingSince().IsZero() && switched != nil &&
		now.Sub(switched.At) >= stepUpAfter
	var due bool
	if stepUp {
		pool = ids
		last := s.h.lastProbe[s.slug]
		due = s.h.policy.behind > 0 && s.h.probing == "" && (last.IsZero() || now.Sub(last) >= s.h.policy.cooldown)
	} else {
		due, _ = s.h.policy.probeDue(now, w.behindBy(), s.h.lastProbe[s.slug], s.h.probing != "", others(pool, current))
	}
	if !due {
		s.h.mu.Unlock()
		return
	}
	if s.h.probedAt[s.slug] == nil {
		s.h.probedAt[s.slug] = map[string]time.Time{}
	}
	cand := nextCandidate(pool, current, s.h.probedAt[s.slug])
	s.h.probing = s.slug
	s.h.probedAt[s.slug][cand] = now
	s.h.mu.Unlock()

	s.mu.Lock()
	s.probing = cand
	s.mu.Unlock()
	go s.probe(s.ctx, cand, current, w, stepUp)
}

// Nothing here touches the playing source.
func (s *stream) probe(ctx context.Context, cand, current string, w *swarm, stepUp bool) {
	p := s.h.policy
	start := time.Now()
	before := w.reading(start.Add(-p.judgeOver))
	var after *reading
	keep := func(reason string) {
		log.Printf("[proxy] %s kept %s: probe %s %s", s.label, short(current), short(cand), reason)
		s.probed(probeRecord{At: start, Source: cand, Outcome: "kept", Reason: reason, Current: before, Candidate: after})
	}
	defer func() {
		s.h.mu.Lock()
		s.h.probing = ""
		s.h.lastProbe[s.slug] = time.Now()
		s.h.mu.Unlock()
		s.mu.Lock()
		s.probing = ""
		s.mu.Unlock()
	}()

	if stepUp {
		log.Printf("[proxy] %s probing %s to step back up from %s", s.label, short(cand), short(current))
	} else {
		log.Printf("[proxy] %s probing %s: %s is %s of video behind, %.2fx, %.0f peers", s.label, short(cand), short(current),
			w.behindBy().Round(100*time.Millisecond), before.Headroom, before.Peers)
	}
	label := s.label + " [probe " + short(cand) + "]"
	openBy := start.Add(min(s.h.sourceWait, probeOpenWait))
	// The engine tells players apart by address unless given a pid, and a new download stops a player's
	// old one: without a pid the probe killed the stream it was meant to rescue.
	target := fmt.Sprintf("%s/ace/getstream?id=%s&pid=bifrost-probe-%d", s.h.base, cand, s.h.probes.Add(1))
	pctx, abandon := context.WithCancel(ctx)
	handed := false
	defer func() {
		if !handed {
			abandon()
		}
	}()
	// open() looks at its deadline only between attempts; a request the engine holds would hold the probe.
	expired := time.AfterFunc(time.Until(openBy), abandon)
	resp, session, ok := openEngine(pctx, s.h.client, target, s.headers, label, openBy)
	if !ok {
		if ctx.Err() == nil {
			s.h.noteOutcome(cand, "failed")
			keep("never produced a stream")
		}
		return
	}
	first, ok := awaitFirstBytes(pctx, resp.Body, label, start, openBy)
	if !expired.Stop() || !ok {
		resp.Body.Close()
		session.stop()
		if ctx.Err() == nil {
			s.h.noteOutcome(cand, "failed")
			keep("sent no video")
		}
		return
	}
	up := feed(pctx, cand, resp, first, session)
	if up.stat == "" {
		keep("has no engine stat to measure")
		return
	}

	samples, drained, err := s.sampleProbe(up)
	if ctx.Err() != nil {
		return
	}
	if err != "" {
		keep(err)
		return
	}
	r := candidateReading(samples, w.rate())
	after = &r
	s.h.noteSource(cand, func(f *sourceFacts) {
		f.Headroom, f.Peers, f.SeenAt, f.Outcome = r.Headroom, int(r.Peers), time.Now(), "probed"
	})
	stillStarving := w.status().Starving
	log.Printf("[swarm] %s probe %s: %.2fx, %.0f peers over %d samples; playing %s: %.2fx, %.0f peers, starving %t",
		s.label, short(cand), r.Headroom, r.Peers, r.Samples, short(current), before.Headroom, before.Peers, stillStarving)
	better, reason := p.judge(before, r, stillStarving)
	if stepUp {
		better = r.Samples >= 2 && !r.Stalled && r.Headroom >= stepUpHeadroom
		reason = fmt.Sprintf("its own source keeps up at %.2fx", r.Headroom)
		if !better {
			reason = fmt.Sprintf("its own source measured %.2fx, needs %.1fx to step back up", r.Headroom, stepUpHeadroom)
		}
	}
	if !better {
		keep(reason)
		return
	}

	h := handover{up: up, samples: samples, drained: drained, before: before, after: r, reason: reason,
		outcome: make(chan string, 1)}
	select {
	case s.handover <- h:
		handed = true
	case <-ctx.Done():
		return
	}
	if failed := <-h.outcome; failed != "" {
		keep(failed)
		return
	}
	s.probed(probeRecord{At: start, Source: cand, Outcome: "switched", Reason: reason, Current: before, Candidate: after})
}

// Drains the candidate's video while sampling, so the engine delivers it as it would to a player.
func (s *stream) sampleProbe(up *upstream) ([]swarmSample, int64, string) {
	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(s.h.swarmEvery)
	defer ticker.Stop()
	end := time.NewTimer(s.h.policy.probeFor)
	defer end.Stop()
	var samples []swarmSample
	var drained int64
	take := func() {
		if sample, err := fetchStat(up.ctx, client, up.stat); err == nil {
			sample.relayed = drained
			samples = append(samples, sample)
		}
	}
	for {
		select {
		case chunk, open := <-up.chunks:
			if !open {
				return nil, 0, "stream ended while measured"
			}
			drained += int64(len(chunk))
		case <-ticker.C:
			take()
		case <-end.C:
			take()
			return samples, drained, ""
		case <-up.ctx.Done():
			return nil, 0, "abandoned"
		}
	}
}

// Runs in the relay between two chunks; the old session closes only once the new one feeds the viewers.
func (s *stream) switchTo(old *upstream, h handover) {
	old.cancel()
	shift := s.relayed.Load() - h.drained
	nw := &swarm{}
	s.mu.Lock()
	from := s.source
	nw.threshold, nw.stalls = s.swarm.threshold, s.swarm.stalledSoFar()
	for _, sample := range h.samples {
		sample.relayed += shift
		nw.samples = append(nw.samples, sample)
	}
	s.source, s.swarm = h.up.id, nw
	s.switches++
	s.lastSwitch = &switchRecord{At: time.Now(), From: from, To: h.up.id, Reason: h.reason,
		HeadroomBefore: h.before.Headroom, HeadroomAfter: h.after.Headroom}
	s.mu.Unlock()

	s.h.mu.Lock()
	s.h.lastGood[s.slug] = h.up.id
	s.h.switches[s.slug]++
	s.h.mu.Unlock()
	log.Printf("[proxy] %s switched %s -> %s, headroom %.2fx -> %.2fx: %s", s.label, short(from), short(h.up.id),
		h.before.Headroom, h.after.Headroom, h.reason)
	if was, now := s.h.table.heightOf(from), s.h.table.heightOf(h.up.id); was > 0 && now > 0 && was != now {
		text := fmt.Sprintf("Back to %dp", now)
		if now < was {
			text = fmt.Sprintf("Switched to %dp: the %dp source was too slow", now, was)
		}
		log.Printf("[proxy] %s %s", s.label, text)
		go s.h.announce(s.label, text)
	}
	if h.up.stat != "" {
		go s.watchSwarm(h.up.ctx, h.up.stat, nw)
	}
	h.outcome <- ""
}

func (s *stream) probed(r probeRecord) {
	s.mu.Lock()
	s.lastProbe = &r
	s.mu.Unlock()
}
