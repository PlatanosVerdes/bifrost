package stream

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStreamInfoReadsFFmpegsListing(t *testing.T) {
	cases := []struct {
		line   string
		height int
		audio  string
	}{
		{"  Stream #0:0[0x100]: Video: h264 (Main) ([27][0][0][0] / 0x001B), yuv420p(tv, bt709, progressive), 1280x720 [SAR 1:1 DAR 16:9], 25 fps, 25 tbr, 90k tbn", 720, ""},
		{"  Stream #0:1[0x101](spa): Audio: aac (LC) ([15][0][0][0] / 0x000F), 48000 Hz, stereo, fltp, 128 kb/s", 0, "AAC stereo (spa)"},
		{"  Stream #0:2[0x102]: Audio: ac3 ([129][0][0][0] / 0x0081), 48000 Hz, 5.1(side), fltp, 384 kb/s", 0, "AC3 5.1"},
		{"  Stream #0:3[0x103](und): Audio: mp2 ([3][0][0][0] / 0x0003), 48000 Hz, stereo, fltp, 192 kb/s", 0, "MP2 stereo"},
		{"Input #0, mpegts, from 'pipe:0':", 0, ""},
	}
	for _, c := range cases {
		if h, a := streamInfo(c.line); h != c.height || a != c.audio {
			t.Errorf("%q: got %d %q, want %d %q", c.line, h, a, c.height, c.audio)
		}
	}
}

func TestListingKeepsEveryAudioTrack(t *testing.T) {
	var l listing
	for _, line := range []string{
		"  Stream #0:0[0x100]: Video: h264 (Main), yuv420p(progressive), 1920x1080 [SAR 1:1 DAR 16:9], 25 fps",
		"  Stream #0:1[0x101](spa): Audio: aac (LC) ([15][0][0][0] / 0x000F), 48000 Hz, stereo, fltp, 128 kb/s",
		"  Stream #0:2[0x102](eng): Audio: ac3 ([129][0][0][0] / 0x0081), 48000 Hz, 5.1(side), fltp, 384 kb/s",
	} {
		l.read(line)
	}
	if l.height != 1080 || l.audios() != "AAC stereo (spa), AC3 5.1 (eng)" {
		t.Errorf("got %d %q", l.height, l.audios())
	}
}

func pick(t *testing.T, url, body string, header map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	b.Write(buf[:n])
	return resp.StatusCode, b.String()
}

func TestPickSwitchesToAnotherSourceOfTheChannel(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 4000, fastSource: 4000})
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)
	h.policy.Behind = 0
	url := srv.URL + "/api/channels/channel-a-1080p/source"

	if code, _ := pick(t, url, `{"id":"`+fastSource+`"}`, nil); code != http.StatusConflict {
		t.Errorf("a pick on a channel nobody plays answered %d, want 409", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/channel-a-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				return
			}
		}
	}()
	waitFor(t, "the slow source to play", func() bool { return engine.count(engine.live, slowSource) == 1 })

	if code, _ := pick(t, url, `{"id":"0000000000000000000000000000000000000000"}`, nil); code != http.StatusNotFound {
		t.Errorf("a source of another channel answered %d, want 404", code)
	}
	if code, _ := pick(t, url, `{"id":"`+fastSource+`"}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Errorf("a cross-site pick answered %d, want 403", code)
	}
	if code, body := pick(t, url, `{"id":"`+fastSource+`"}`, nil); code != http.StatusOK {
		t.Fatalf("the pick answered %d %s, want 200", code, body)
	}
	waitFor(t, "the slow session to close", func() bool { return engine.count(engine.live, slowSource) == 0 })

	var channels struct {
		Channels []ChannelInfo `json:"channels"`
	}
	getJSON(t, srv.URL+"/api/channels", &channels)
	c := channels.Channels[0]
	if c.Playing == nil || c.Playing.Source != fastSource {
		t.Fatalf("playing %+v after the pick, want the fast source", c.Playing)
	}
	if len(c.Sources) != 2 || c.Sources[0].ID != slowSource || c.Sources[1].ID != fastSource {
		t.Fatalf("sources %+v, want the playlist's order whichever plays", c.Sources)
	}
	if c.Sources[0].Outcome != "played" || c.Sources[0].SeenAt.IsZero() {
		t.Errorf("the slow source's facts %+v, want it played", c.Sources[0])
	}
	h.mu.Lock()
	cooled := !h.lastProbe["channel-a-1080p"].IsZero() && h.probing == ""
	h.mu.Unlock()
	if !cooled {
		t.Error("the pick did not leave the automatic switch a cooldown")
	}
}

func TestPickOfADeadSourceKeepsTheStream(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 4000, fastSource: 4000})
	engine.dead[fastSource] = true
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)
	h.policy.Behind = 0
	h.sourceWait = 300 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/channel-a-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				return
			}
		}
	}()
	waitFor(t, "the slow source to play", func() bool { return engine.count(engine.live, slowSource) == 1 })

	code, body := pick(t, srv.URL+"/api/channels/channel-a-1080p/source", `{"id":"`+fastSource+`"}`, nil)
	if code != http.StatusBadGateway || !strings.Contains(body, "Source 2 did not start") {
		t.Errorf("a dead pick answered %d %s, want 502 naming Source 2", code, body)
	}
	if engine.count(engine.live, slowSource) != 1 {
		t.Error("a dead pick stopped the playing source")
	}
	if f := h.sourcesOf([]string{fastSource})[0]; f.Outcome != "failed" {
		t.Errorf("the dead source's facts %+v, want it failed", f)
	}
}

func TestOrderTriesARecentlyFailedSourceLast(t *testing.T) {
	h := newHub("http://engine", time.Second, time.Second)
	ids := []string{"a", "b", "c"}
	h.noteOutcome("a", "failed")
	h.mu.Lock()
	h.lastGood["ch"] = "c"
	h.mu.Unlock()
	if got := strings.Join(h.ordered("ch", ids), ""); got != "cba" {
		t.Errorf("order %q, want the last good first and the failed one last", got)
	}
	h.noteSource("a", func(f *SourceFacts) { f.SeenAt = time.Now().Add(-recentlyFailed - time.Minute) })
	if got := strings.Join(h.ordered("ch", ids), ""); got != "cab" {
		t.Errorf("order %q, want a failure older than %s forgotten", got, recentlyFailed)
	}
	h.noteOutcome("c", "failed")
	if got := strings.Join(h.ordered("ch", ids), ""); got != "abc" {
		t.Errorf("order %q, want the last good one tried last once it fails", got)
	}
}

func TestSourceFactsSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.json")
	before := newHub("http://engine", time.Second, time.Second)
	before.noteOutcome("dead", "failed")
	before.noteSource("old", func(f *SourceFacts) { f.Outcome, f.SeenAt = "played", time.Now().Add(-factsKept-time.Hour) })
	if err := before.saveFacts(path); err != nil {
		t.Fatal(err)
	}

	after := newHub("http://engine", time.Second, time.Second)
	after.LoadFacts(path)
	if got := strings.Join(after.ordered("ch", []string{"dead", "live"}), ","); got != "live,dead" {
		t.Errorf("after a restart the order is %q, want the source that failed lately last", got)
	}
	if f := after.sourcesOf([]string{"old"})[0]; f.Outcome != "" {
		t.Errorf("a source unseen for over %s came back as %+v", factsKept, f)
	}
}
