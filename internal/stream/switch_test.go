package stream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/mpegts"
)

func TestProbeIsDueOnceTheSourceIsFarEnoughBehind(t *testing.T) {
	p := DefaultPolicy
	now := time.Date(2026, 10, 20, 21, 30, 0, 0, time.UTC)
	off := p
	off.Behind = 0
	for _, c := range []struct {
		name      string
		policy    Policy
		behind    time.Duration
		lastProbe time.Time
		busy      bool
		others    int
		want      string
	}{
		{"far enough behind", p, 6 * time.Second, time.Time{}, false, 2, ""},
		{"a little behind", p, 2 * time.Second, time.Time{}, false, 2, "not far enough behind"},
		{"one source", p, time.Minute, time.Time{}, false, 0, "no other source"},
		{"another probe runs", p, time.Minute, time.Time{}, true, 2, "a probe is running"},
		{"probed 2 min ago", p, time.Minute, now.Add(-2 * time.Minute), false, 2, "cooling down"},
		{"probed 4 min ago", p, time.Minute, now.Add(-4 * time.Minute), false, 2, ""},
		{"switching off", off, time.Minute, time.Time{}, false, 2, "switching is off"},
	} {
		due, why := c.policy.probeDue(now, c.behind, c.lastProbe, c.busy, c.others)
		if due != (c.want == "") || why != c.want {
			t.Errorf("%s: due %t (%q), want %q", c.name, due, why, c.want)
		}
	}
}

func TestJudgeSwitchesOnlyToAClearlyBetterSource(t *testing.T) {
	p := DefaultPolicy
	cur := func(h float64) reading { return reading{Headroom: h, Peers: 20, Samples: 6} }
	cand := func(h, peers float64) reading { return reading{Headroom: h, Peers: peers, Samples: 3} }
	for _, c := range []struct {
		name      string
		current   reading
		candidate reading
		starving  bool
		want      bool
	}{
		// A real case: the playing source at 0.8x while another delivered twice the bitrate.
		{"twice the bitrate", cur(0.8), cand(1.9, 25), true, true},
		{"keeps up, clear margin", cur(0.8), cand(1.15, 18), true, true},
		{"keeps up, not by the margin", cur(0.95), cand(1.1, 18), true, false},
		{"over the margin, not keeping up", cur(0.7), cand(0.95, 18), true, false},
		{"least bad by a wide margin", cur(0.5), cand(0.95, 18), true, true},
		{"thin swarm needs twice the margin", cur(0.8), cand(1.15, 5), true, false},
		{"thin swarm, far better", cur(0.8), cand(1.6, 5), true, true},
		// Real cases: a probe at 1.50x and one at 1.56x on a thinner swarm were turned down because the
		// playing source, flapping around 1x, had just recovered.
		{"recovered, the candidate far better", reading{Headroom: 1.03, Peers: 9, Samples: 6}, cand(1.5, 8), false, true},
		{"recovered, thinner but far better", reading{Headroom: 1.0, Peers: 17, Samples: 6}, cand(1.56, 6), false, true},
		{"recovered, the candidate only a bit better", cur(1.0), cand(1.25, 25), false, false},
		// A real case: the playing source flapped around 0.85x on 2 peers and was between two starves.
		{"recovered for now, short on average", reading{Headroom: 0.85, Peers: 2, Samples: 6}, cand(1.4, 4), false, true},
		{"stalled candidate", cur(0.8), reading{Headroom: 1.9, Peers: 25, Samples: 3, Stalled: true}, true, false},
		{"too few samples", cur(0.8), reading{Headroom: 1.9, Peers: 25, Samples: 1}, true, false},
		{"every source bad: stay", cur(0.65), cand(0.75, 18), true, false},
	} {
		if got, why := p.judge(c.current, c.candidate, c.starving); got != c.want {
			t.Errorf("%s: switch %t (%s), want %t", c.name, got, why, c.want)
		}
	}
}

func TestCandidatesRotateNeverProbedFirst(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	t0 := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	if got := nextCandidate(ids, "a", nil); got != "b" {
		t.Errorf("first probe %q, want the next source in the list", got)
	}
	probed := map[string]time.Time{"b": t0, "c": t0.Add(time.Minute)}
	if got := nextCandidate(ids, "a", probed); got != "d" {
		t.Errorf("got %q, want the one never probed", got)
	}
	probed["d"] = t0.Add(2 * time.Minute)
	if got := nextCandidate(ids, "a", probed); got != "b" {
		t.Errorf("got %q, want the one probed longest ago", got)
	}
	if got := nextCandidate([]string{"a"}, "a", nil); got != "" {
		t.Errorf("got %q with a single source, want none", got)
	}
}

func TestCandidateReadingSkipsTheOpeningBurst(t *testing.T) {
	samples := []swarmSample{at(5, "dl", 4000, 800, 1), at(10, "dl", 1200, 800, 1), at(15, "prebuf", 0, 800, 1),
		at(20, "dl", 1000, 800, 1)}
	r := candidateReading(samples, 800*1024)
	if r.Samples != 2 || r.Headroom != 1.38 || r.Stalled || r.Peers != 12 {
		t.Errorf("reading = %+v, want the two downloading samples after the first, 1.38x", r)
	}
	if r := candidateReading(append(samples, at(25, "prebuf", 0, 800, 1)), 800*1024); !r.Stalled {
		t.Error("a candidate whose last sample is not downloading did not count as stalled")
	}
	// A candidate of a higher bitrate is measured against its own.
	if r := candidateReading(samples, 400*1024); r.Headroom != 1.38 {
		t.Errorf("against a lower channel rate: %.2fx, want the candidate's own 1.38x", r.Headroom)
	}
}

func tsStream(packets int, marker byte) []byte {
	var b bytes.Buffer
	for i := 0; i < packets; i++ {
		p := make([]byte, mpegts.PacketSize)
		p[0], p[1] = mpegts.SyncByte, marker
		b.Write(p)
	}
	return b.Bytes()
}

func TestAlignerHandsOutWholePackets(t *testing.T) {
	in := tsStream(2000, 'a')
	a := &mpegts.Aligner{}
	var out []byte
	rng := rand.New(rand.NewSource(1))
	for rest := in; len(rest) > 0; {
		n := min(len(rest), 1+rng.Intn(40000))
		chunk := a.Push(rest[:n])
		if len(chunk)%mpegts.PacketSize != 0 || (len(chunk) > 0 && chunk[0] != mpegts.SyncByte) {
			t.Fatalf("chunk of %d bytes starting %#x, want whole packets", len(chunk), chunk[0])
		}
		out = append(out, chunk...)
		rest = rest[n:]
	}
	if !bytes.Equal(out, in) {
		t.Errorf("relayed %d of %d bytes, or out of order", len(out), len(in))
	}

	spliced := &mpegts.Aligner{DropLead: true}
	got := spliced.Push(append([]byte("half a packet from wherever the engine was"), tsStream(10, 'b')...))
	if !bytes.Equal(got, tsStream(10, 'b')) {
		t.Errorf("a spliced-in source started with %q, want its first packet", got[:8])
	}

	opening := &mpegts.Aligner{}
	got = opening.Push(append([]byte("lead"), tsStream(10, 'c')...))
	if !bytes.Equal(got, append([]byte("lead"), tsStream(10, 'c')...)) {
		t.Error("the opening source lost its leading bytes")
	}

	raw := &mpegts.Aligner{}
	notTS := bytes.Repeat([]byte("not a transport stream "), 1000)
	if got := raw.Push(notTS); !bytes.Equal(got, notTS) {
		t.Errorf("a stream that is not TS came out as %d of %d bytes", len(got), len(notTS))
	}
}

// sourceEngine marks each id's TS packets with its first letter and behaves like aceserve: a redirect
// to a session, a new download from the same pid (or address) stops the previous one, and a redirect
// ends only if the client asked to close.
type sourceEngine struct {
	mu      sync.Mutex
	down    map[string]int // KiB/s
	dead    map[string]bool
	opened  map[string]int
	live    map[string]int
	players map[string]context.CancelFunc
	media   map[string][]byte // real TS to play for an id, once, instead of marker packets
	burst   int               // bytes of media sent at once first, like the engine's opening buffer
}

func newSourceEngine(down map[string]int) *sourceEngine {
	return &sourceEngine{down: down, dead: map[string]bool{}, opened: map[string]int{}, live: map[string]int{},
		players: map[string]context.CancelFunc{}}
}

func (e *sourceEngine) count(m map[string]int, id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return m[id]
}

func (e *sourceEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/ace/getstream":
		id := r.URL.Query().Get("id")
		e.mu.Lock()
		e.opened[id]++
		dead := e.dead[id]
		e.mu.Unlock()
		if dead {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		player := r.URL.Query().Get("pid")
		if player == "" {
			player = "address"
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(buf, "HTTP/1.1 302 Found\r\nLocation: /ace/r/%s/%s\r\n\r\n", id, player)
		buf.Flush()
		if r.Close {
			conn.Close()
		} else {
			time.AfterFunc(10*time.Second, func() { conn.Close() })
		}
	case strings.HasPrefix(r.URL.Path, "/ace/r/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/ace/r/"), "/")
		id, player := parts[0], parts[1]
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		e.mu.Lock()
		if stop := e.players[player]; stop != nil {
			stop()
		}
		e.players[player] = cancel
		e.live[id]++
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.live[id]--
			e.mu.Unlock()
		}()
		w.Header().Set("Content-Type", "video/mp2t")
		e.mu.Lock()
		media := e.media[id]
		e.mu.Unlock()
		if media != nil {
			if n := min(e.burst, len(media)); n > 0 {
				if _, err := w.Write(media[:n]); err != nil {
					return
				}
				media = media[n:]
			}
			for rest := media; len(rest) > 0; {
				n := min(len(rest), 7*mpegts.PacketSize)
				if _, err := w.Write(rest[:n]); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				rest = rest[n:]
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Millisecond):
				}
			}
			<-ctx.Done()
			return
		}
		// Seven packets at a time, starting a packet off so the relay has to find its sync.
		piece := tsStream(7, id[0])
		w.Write(piece[mpegts.PacketSize/2:])
		for {
			if _, err := w.Write(piece); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	case strings.HasPrefix(r.URL.Path, "/ace/stat/"):
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/ace/stat/"), "/")[0]
		e.mu.Lock()
		down := e.down[id]
		e.mu.Unlock()
		pos := float64(time.Now().UnixNano()) / 1e9
		fmt.Fprintf(w, `{"response": {"status": "dl", "peers": 20, "speed_down": %d, "speed_up": 100,
		 "livepos": {"pos": "%.3f"}}, "error": null}`, down, pos)
	default:
		http.NotFound(w, r)
	}
}

const (
	slowSource = "s000000000000000000000000000000000000000"
	fastSource = "f000000000000000000000000000000000000000"
)

func newSwitchingProxy(t *testing.T, engine http.Handler, ids ...string) (*httptest.Server, *Hub) {
	t.Helper()
	span := swarmMinSpan
	swarmMinSpan = 100 * time.Millisecond
	t.Cleanup(func() { swarmMinSpan = span })
	srv, h := newStatusProxy(t, engine, ids...)
	h.swarmEvery = 20 * time.Millisecond
	h.policy = Policy{Behind: 30 * time.Millisecond, judgeOver: 100 * time.Millisecond, ProbeFor: 300 * time.Millisecond, Cooldown: time.Minute,
		MinHeadroom: 1.1, Margin: 0.2, StarvingBelow: 1}
	return srv, h
}

// watch reads the channel for d, checking every packet arrives whole, and returns the markers in play order.
func watch(t *testing.T, url string, d time.Duration, until func(markers string) bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	markers := ""
	// The engine's first bytes are half a packet; the viewer finds the sync once, like a player.
	lead := make([]byte, 4*mpegts.PacketSize)
	if _, err := io.ReadFull(resp.Body, lead); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, mpegts.SyncAt(lead))); err != nil {
		t.Fatal(err)
	}
	packet := make([]byte, mpegts.PacketSize)
	for {
		if _, err := io.ReadFull(resp.Body, packet); err != nil {
			if ctx.Err() == nil {
				t.Errorf("the viewer lost the stream: %v", err)
			}
			return markers
		}
		if packet[0] != mpegts.SyncByte {
			t.Fatalf("a packet starts with %#x after %q: the stream was cut mid-packet", packet[0], markers)
		}
		if m := string(packet[1]); !strings.HasSuffix(markers, m) {
			markers += m
		}
		if until(markers) {
			return markers
		}
	}
}

func TestStarvingSourceSwitchesToABetterOne(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 4000})
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)

	markers := watch(t, srv.URL+"/channel/channel-a-1080p", 5*time.Second,
		func(m string) bool { return strings.HasSuffix(m, "f") })
	if markers != "sf" {
		t.Fatalf("played %q, want the slow source then the fast one, with no gap closing the viewer", markers)
	}
	waitFor(t, "the slow session to close", func() bool { return engine.count(engine.live, slowSource) == 0 })

	h.mu.Lock()
	good, switches := h.lastGood["channel-a-1080p"], h.switches["channel-a-1080p"]
	h.mu.Unlock()
	if good != fastSource || switches != 1 {
		t.Errorf("last good %q after %d switches, want the fast source after one", short(good), switches)
	}
}

func TestStatusAndMetricsShowTheSwitch(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 4000})
	srv, _ := newSwitchingProxy(t, engine, slowSource, fastSource)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/channel-a-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body)

	var st struct {
		Streams []StreamStatus `json:"streams"`
	}
	waitFor(t, "the switch to show", func() bool {
		getJSON(t, srv.URL+"/api/status", &st)
		return len(st.Streams) == 1 && st.Streams[0].LastSwitch != nil && st.Streams[0].Swarm != nil
	})
	s := st.Streams[0]
	if s.Source != fastSource || s.Switches != 1 || s.LastSwitch.From != slowSource || s.LastSwitch.To != fastSource ||
		s.LastSwitch.HeadroomBefore >= 1 || s.LastSwitch.HeadroomAfter < 1.1 || s.Probing {
		t.Errorf("stream = %+v, switch = %+v", s, s.LastSwitch)
	}
	if s.LastProbe == nil || s.LastProbe.Outcome != "switched" || s.LastProbe.Source != fastSource {
		t.Errorf("last probe = %+v", s.LastProbe)
	}
	waitFor(t, "the new source's swarm", func() bool {
		getJSON(t, srv.URL+"/api/status", &st)
		return st.Streams[0].Swarm.Download == 4000*1024
	})
}

func TestNoBetterSourceKeepsThePlayingOne(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 150})
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)

	markers := watch(t, srv.URL+"/channel/channel-a-1080p", 1500*time.Millisecond, func(string) bool { return false })
	if markers != "s" {
		t.Errorf("played %q, want the slow source throughout", markers)
	}
	if n := engine.count(engine.opened, fastSource); n != 1 {
		t.Errorf("the candidate was opened %d times, want one probe and then the cooldown", n)
	}
	if n := engine.count(engine.live, fastSource); n != 0 {
		t.Errorf("%d candidate sessions still open after it was judged", n)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.switches["channel-a-1080p"] != 0 || h.lastGood["channel-a-1080p"] != slowSource || h.probing != "" {
		t.Errorf("switches %d, last good %q, probing %q", h.switches["channel-a-1080p"], short(h.lastGood["channel-a-1080p"]), h.probing)
	}
}

func TestAFailedProbeLeavesTheStreamAlone(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 4000})
	engine.dead[fastSource] = true
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)

	// open() retries a 500 every 2 s, so the probe gives up just past that.
	markers := watch(t, srv.URL+"/channel/channel-a-1080p", 3*time.Second, func(string) bool { return false })
	if markers != "s" {
		t.Errorf("played %q, want the slow source throughout", markers)
	}
	var st struct {
		Streams []StreamStatus `json:"streams"`
	}
	getJSON(t, srv.URL+"/api/status", &st)
	if len(st.Streams) != 1 || st.Streams[0].LastProbe == nil || st.Streams[0].LastProbe.Outcome != "kept" ||
		st.Streams[0].LastProbe.Reason != "never produced a stream" {
		t.Errorf("streams = %+v", st.Streams)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.probing != "" || h.lastProbe["channel-a-1080p"].IsZero() {
		t.Error("the failed probe did not release its slot or start the cooldown")
	}
}
