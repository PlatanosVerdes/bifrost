package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedSlateLoops(t *testing.T) {
	sl, err := newSlate(offairTS)
	if err != nil {
		t.Fatal(err)
	}
	if sl.duration < 23900*time.Millisecond || sl.duration > 24100*time.Millisecond {
		t.Errorf("the slate lasts %s, want 24 s", sl.duration)
	}
	if got := len(firstTimestamps(sl.loop[:sl.head])); got != 2 {
		t.Errorf("its head has timestamps for %d PIDs, want video and audio", got)
	}
}

func TestSlatePlayerKeepsRealTime(t *testing.T) {
	sl, err := newSlate(offairTS)
	if err != nil {
		t.Fatal(err)
	}
	p := &slatePlayer{sl: sl, lead: slateLead}
	t0 := time.Unix(1000, 0)
	first, fresh := p.next(t0)
	rate := float64(len(sl.loop)) / sl.duration.Seconds()
	if !fresh || float64(len(first)) < rate*slateLead.Seconds()*0.9 {
		t.Errorf("the first chunk is %d bytes, fresh %t; want about %s of the loop at once", len(first), fresh, slateLead)
	}
	total := int64(len(first))
	starts := 0
	for now := t0.Add(slateTick); now.Before(t0.Add(3 * sl.duration)); now = now.Add(slateTick) {
		b, fresh := p.next(now)
		if fresh {
			starts++
		}
		total += int64(len(b))
	}
	// The lead puts the player two seconds ahead, so the third run starts within three loops' time.
	if starts != 3 {
		t.Errorf("%d new runs in three loops' time, want 3 after the first", starts)
	}
	if want := rate * (3*sl.duration + slateLead).Seconds(); float64(total) > want*1.02 || float64(total) < want*0.95 {
		t.Errorf("sent %d bytes in three loops, want about %.0f: real time", total, want)
	}
}

// A channel whose only source starts late plays the slate, then the match, as one stream.
func TestSlateHandsOverToTheFirstSourceThatDelivers(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "match.ts")
	if out, err := exec.Command(bin, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-f", "lavfi",
		"-i", "sine=sample_rate=48000", "-t", "30", "-c:v", "libx264", "-g", "50", "-preset", "ultrafast", "-c:a", "aac",
		"-ac", "2", "-output_ts_offset", "40000", "-mpegts_start_pid", "0x300", "-f", "mpegts", path).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot make a test source: %v %s", err, out)
	}
	media, _ := os.ReadFile(path)
	engine := newSourceEngine(map[string]int{slowSource: 4000})
	engine.media = map[string][]byte{slowSource: media}
	engine.dead[slowSource] = true
	srv, h := newSwitchingProxy(t, engine, slowSource)
	h.ffmpeg = bin
	if h.slate, err = newSlate(offairTS); err != nil {
		t.Fatal(err)
	}
	if h.loading, err = newSlate(loadingTS); err != nil {
		t.Fatal(err)
	}
	h.sourceWait, h.channelWait = 300*time.Millisecond, 300*time.Millisecond
	retry := slateRetry
	slateRetry = 500 * time.Millisecond
	t.Cleanup(func() { slateRetry = retry })

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/dazn-laliga-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(2500 * time.Millisecond)
		engine.mu.Lock()
		engine.dead[slowSource] = false
		engine.mu.Unlock()
	}()
	waitingSeen := make(chan bool, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		var st struct {
			Streams []streamStatus `json:"streams"`
		}
		r, err := http.Get(srv.URL + "/api/status")
		if err == nil {
			json.NewDecoder(r.Body).Decode(&st)
			r.Body.Close()
		}
		waitingSeen <- err == nil && len(st.Streams) == 1 && st.Streams[0].Waiting
	}()
	got, _ := io.ReadAll(resp.Body)
	if !<-waitingSeen {
		t.Error("the status did not say the channel was waiting while the slate played")
	}

	h.mu.Lock()
	good := h.lastGood["dazn-laliga-1080p"]
	h.mu.Unlock()
	if good != slowSource {
		t.Fatalf("the source never took over (last good %q)", good)
	}
	out := filepath.Join(dir, "watched.ts")
	os.WriteFile(out, got[:len(got)/tsPacket*tsPacket], 0o644)
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,width", "-of", "json", out).Output()
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if json.Unmarshal(probe, &found); len(found.Streams) != 2 {
		t.Errorf("the viewer got streams %+v, want one video and one audio", found.Streams)
	}
	size := int64(len(got) / tsPacket * tsPacket)
	for _, stream := range []string{"v:0", "a:0"} {
		lines, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", "packet=dts_time,pos",
			"-of", "csv=p=0", out).Output()
		if err != nil {
			t.Fatal(err)
		}
		var prev float64
		var lastPos int64
		n := 0
		for _, line := range strings.Fields(string(lines)) {
			fields := strings.Split(strings.TrimSuffix(line, ","), ",")
			if len(fields) < 2 {
				continue
			}
			ts, err := strconv.ParseFloat(fields[0], 64)
			if err != nil {
				continue
			}
			if n > 0 && (ts < prev || ts-prev > 0.5) {
				t.Errorf("%s jumps from %.3f to %.3f", stream, prev, ts)
			}
			prev, n = ts, n+1
			lastPos, _ = strconv.ParseInt(fields[1], 10, 64)
		}
		if lastPos < size*9/10 {
			t.Errorf("%s stops at byte %d of %d: the match never reached it", stream, lastPos, size)
		}
	}
	widths, _ := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=width",
		"-of", "csv=p=0", out).Output()
	if w := strings.Fields(strings.ReplaceAll(string(widths), ",", "")); len(w) == 0 || w[0] != "1280" || w[len(w)-1] != "640" {
		t.Errorf("the picture went from %v to %v wide, want the 1280 slate then the 640 match", first(w), last(w))
	}
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func TestSlateRunsJoinWithoutAJump(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	sl, err := newSlate(offairTS)
	if err != nil {
		t.Fatal(err)
	}
	p, reb := &slatePlayer{sl: sl}, newRebaser()
	var out []byte
	t0 := time.Unix(1000, 0)
	for now := t0; now.Before(t0.Add(3 * sl.duration)); now = now.Add(slateTick) {
		b, fresh := p.next(now)
		if fresh && p.sent > int64(len(b)) {
			out = append(out, reb.advance(sl.ticks, b)...)
		} else {
			out = append(out, reb.push(b)...)
		}
	}
	path := filepath.Join(t.TempDir(), "loops.ts")
	os.WriteFile(path, out, 0o644)
	for _, stream := range []string{"v:0", "a:0"} {
		lines, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", "packet=dts_time",
			"-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var prev float64
		n := 0
		for _, line := range strings.Fields(strings.ReplaceAll(string(lines), ",", "")) {
			ts, err := strconv.ParseFloat(line, 64)
			if err != nil {
				continue
			}
			if n > 0 && (ts <= prev || ts-prev > 0.1) {
				t.Errorf("%s jumps from %.3f to %.3f", stream, prev, ts)
			}
			prev, n = ts, n+1
		}
		if prev < 2.9*sl.duration.Seconds() {
			t.Errorf("%s ends at %.1fs, want three loops", stream, prev)
		}
	}
}
