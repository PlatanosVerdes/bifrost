package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The shape aceserve 3.2.17 answers on /ace/stat during a live broadcast, ids replaced.
const liveStat = `{"response": {"status": "dl", "total_progress": 0, "speed_down": 641, "speed_up": 1557,
 "peers": 38, "downloaded": 28311552, "uploaded": 63438848, "playback_session_id": "21c9…", "selected_file_index": -1,
 "infohash": "5e90…", "client_session_id": -1, "is_live": 1, "is_encrypted": 0, "manifest_access_mode": -1,
 "selected_stream_index": 0, "stream_status": 1, "livepos": {"pos": "1792522655", "is_live": "1", "buffer_pieces": "15",
 "last": "1792522657", "live_first": "1792520857", "live_last": "1792522657", "first_ts": "1792520857",
 "last_ts": "1792522657"}, "disk_cache_stats": {"avail": 1, "disk_cache_limit": 1073741824, "active_inuse": 0,
 "inactive_inuse": 0}, "debug_level": 0, "network_monitor_started": false, "network_monitor_status": 0}, "error": null}`

func TestStatParsesTheEngineAnswer(t *testing.T) {
	st, err := parseStat([]byte(liveStat))
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "dl" || st.Peers != 38 || st.SpeedDown != 641 || st.SpeedUp != 1557 || st.Downloaded != 28311552 {
		t.Errorf("stat = %+v", st)
	}
	if pos, _ := st.Livepos.Pos.Float64(); pos != 1792522655 {
		t.Errorf("live position = %v, want the string the engine sent as a number", pos)
	}

	if _, err := parseStat([]byte(`{"response": null, "error": "unknown playback session id"}`)); err == nil ||
		err.Error() != "unknown playback session id" {
		t.Errorf("a stopped session parsed as %v, want the engine's error", err)
	}
}

// at builds a sample t seconds into a stream playing kib KiB/s of video, with the engine
// downloading down KiB/s and its position advancing pos seconds per second of wall time.
func at(t float64, status string, down int, kib, pos float64) swarmSample {
	start := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	return swarmSample{at: start.Add(time.Duration(t * float64(time.Second))),
		stat:    engineStat{Status: status, Peers: 12, SpeedDown: down},
		pos:     1792522000 + t*pos,
		relayed: int64(t * pos * kib * 1024)}
}

func TestBitrateFollowsTheLivePosition(t *testing.T) {
	var keeping, starving []swarmSample
	for s := 0.0; s <= 30; s += 5 {
		keeping = append(keeping, at(s, "dl", 850, 820, 1))
		// The engine relays 30% slower than live: a wall clock would read 574 KiB/s.
		starving = append(starving, at(s, "dl", 574, 820, 0.7))
	}
	for name, samples := range map[string][]swarmSample{"keeping up": keeping, "starving": starving} {
		if got := bitrate(samples) / 1024; got < 815 || got > 825 {
			t.Errorf("%s: bitrate %.0f KiB/s, want 820", name, got)
		}
	}

	if got := bitrate(keeping[:2]); got != 0 {
		t.Errorf("bitrate over 5 s = %.0f, want none until the window is long enough", got)
	}

	leapt := append([]swarmSample(nil), keeping...)
	leapt[len(leapt)-1].pos += 120
	if got := bitrate(leapt) / 1024; got < 815 || got > 825 {
		t.Errorf("bitrate after the engine skipped to live = %.0f KiB/s, want wall time used instead (820)", got)
	}
}

func TestStarvingNeedsASustainedShortfall(t *testing.T) {
	w := &swarm{}
	for s := 0.0; s <= 15; s += 5 {
		w.add(at(s, "dl", 850, 820, 1))
	}
	if st := w.status(); st.Starving || st.Headroom != 1.04 || st.Bitrate != 820*1024 || st.Download != 850*1024 || st.Peers != 12 {
		t.Fatalf("keeping up: %+v", st)
	}

	w.add(at(20, "dl", 600, 820, 1))
	if starting, _, _ := w.add(at(25, "dl", 600, 820, 1)); starting || w.status().Starving {
		t.Error("two slow samples already count as starving")
	}
	if starting, _, _ := w.add(at(30, "dl", 527, 820, 1)); !starting {
		t.Error("three slow samples in a row did not start starving")
	}
	if st := w.status(); !st.Starving || st.Headroom >= 1 {
		t.Errorf("starving: %+v", st)
	}

	if _, recovered, _ := w.add(at(35, "dl", 900, 820, 1)); !recovered || w.status().Starving {
		t.Error("a sample above the bitrate did not end the starving")
	}
}

func TestStallsCountLeavingDownload(t *testing.T) {
	w := &swarm{}
	for i, status := range []string{"prebuf", "dl", "dl", "prebuf", "prebuf", "dl", "wait", "dl"} {
		w.add(at(float64(i*5), status, 800, 820, 1))
	}
	if got := w.status().Stalls; got != 2 {
		t.Errorf("stalls = %d, want 2 (the opening prebuf is not one)", got)
	}
}

func TestSwarmMetricsPerChannel(t *testing.T) {
	out := string(swarmMetrics([]streamStatus{
		{Slug: "channel-plus-c-1080p", Viewers: 1, Swarm: &swarmStatus{Peers: 12, Download: 640 * 1024,
			Upload: 1557 * 1024, Bitrate: 800 * 1024, Headroom: 0.8, Starving: true, Stalls: 3, SampledAt: time.Now()}},
		{Slug: "channel-1-1080p", Viewers: 1},
		{Slug: "channel-a-1080p", Swarm: &swarmStatus{SampledAt: time.Now()}, channelSwitches: 1, remuxRestarts: 2},
	}))
	for _, want := range []string{
		`acestream_stream_peers{channel="channel-plus-c-1080p"} 12`,
		`acestream_stream_download_bytes_per_second{channel="channel-plus-c-1080p"} 655360`,
		`acestream_stream_upload_bytes_per_second{channel="channel-plus-c-1080p"} 1594368`,
		`acestream_stream_bitrate_bytes_per_second{channel="channel-plus-c-1080p"} 819200`,
		`acestream_stream_headroom{channel="channel-plus-c-1080p"} 0.8`,
		`acestream_stream_starving{channel="channel-plus-c-1080p"} 1`,
		`acestream_stream_source_switches_total{channel="channel-a-1080p"} 1`,
		`acestream_stream_remux_restarts_total{channel="channel-a-1080p"} 2`,
		"# TYPE acestream_stream_stalls_total counter",
		`acestream_stream_stalls_total{channel="channel-plus-c-1080p"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "channel-1-1080p") {
		t.Error("a stream with no sample yet was pushed with zeroes")
	}
}

type redirectingEngine struct{ fakeEngine }

func (e *redirectingEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const session = "/5e907fc8f9dd7daf4a0d0cb38aa2be47c2294020/3fc00ba502339bbdabc33bb800cd144f"
	switch {
	case r.URL.Path == "/ace/getstream":
		http.Redirect(w, r, "/ace/r"+session, http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/ace/r/"):
		http.Redirect(w, r, "/content/5e907fc8f9dd7daf4a0d0cb38aa2be47c2294020/0.61", http.StatusFound)
	case r.URL.Path == "/ace/stat"+session:
		io.WriteString(w, liveStat)
	case strings.HasPrefix(r.URL.Path, "/content/"):
		e.fakeEngine.ServeHTTP(w, r)
	default:
		w.Write([]byte(fmt.Sprintf(`{"response": null, "error": "no route %s"}`, r.URL.Path)))
	}
}

// The stat comes from the playback bifrost already has: no second session, no format=json open.
func TestStatusShowsTheSwarmOfWhatIsPlaying(t *testing.T) {
	engine := &redirectingEngine{}
	srv, h := newStatusProxy(t, engine, twoSources[0])
	h.swarmEvery = 20 * time.Millisecond

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
		Streams []streamStatus `json:"streams"`
	}
	waitFor(t, "the swarm to show in the status", func() bool {
		getJSON(t, srv.URL+"/api/status", &st)
		return len(st.Streams) == 1 && st.Streams[0].Swarm != nil && st.Streams[0].Swarm.Peers > 0
	})
	if sw := st.Streams[0].Swarm; sw.Status != "dl" || sw.Peers != 38 || sw.Download != 641*1024 || sw.Upload != 1557*1024 || sw.Unavailable != "" {
		t.Errorf("swarm = %+v", sw)
	}
	if n := engine.opened.Load(); n != 1 {
		t.Errorf("engine streamed %d connections, want the one playing", n)
	}
}
