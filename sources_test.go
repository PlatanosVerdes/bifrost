package main

import (
	"context"
	"net/http"
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
		{"  Stream #0:1[0x101](spa): Audio: aac (LC) ([15][0][0][0] / 0x000F), 48000 Hz, stereo, fltp, 128 kb/s", 0, "AAC stereo"},
		{"  Stream #0:2[0x102]: Audio: ac3 ([129][0][0][0] / 0x0081), 48000 Hz, 5.1(side), fltp, 384 kb/s", 0, "AC3 5.1"},
		{"Input #0, mpegts, from 'pipe:0':", 0, ""},
	}
	for _, c := range cases {
		if h, a := streamInfo(c.line); h != c.height || a != c.audio {
			t.Errorf("%q: got %d %q, want %d %q", c.line, h, a, c.height, c.audio)
		}
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
	h.policy.after = 0
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
		Channels []channelInfo `json:"channels"`
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
	h.policy.after = 0
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
