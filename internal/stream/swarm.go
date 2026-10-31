package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/httpx"
)

// /ace/stat/<infohash>/<session> gives peers and speeds without a second session; the engine gives no
// bitrate, so it is measured from the bytes relayed.

const (
	swarmHistory = 24 // samples kept per stream: two minutes at the default poll
	swarmSpan    = 60 * time.Second
	// Samples in a row with the download under the bitrate before a stream counts as starving: one
	// slow sample is the engine's speed jittering, three are its buffer draining.
	starvingAfter = 3
	// What a stream can owe at most, so a long stall does not take as long to pay back.
	maxBehind = time.Minute
	kib       = 1024
)

// The bitrate is not measured before the samples span this much. A variable only for the tests.
var swarmMinSpan = 10 * time.Second

// Speeds are in KiB/s.
type engineStat struct {
	Status     string `json:"status"` // prebuf, dl, check, wait…
	Peers      int    `json:"peers"`
	SpeedDown  int    `json:"speed_down"`
	SpeedUp    int    `json:"speed_up"`
	Downloaded int64  `json:"downloaded"`
	Livepos    *struct {
		Pos json.Number `json:"pos"` // the playback position in the source's seconds, sent as a string
	} `json:"livepos"`
}

func parseStat(body []byte) (engineStat, error) {
	var answer struct {
		Response *engineStat `json:"response"`
		Error    *string     `json:"error"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return engineStat{}, err
	}
	if answer.Error != nil && *answer.Error != "" {
		return engineStat{}, errors.New(*answer.Error)
	}
	if answer.Response == nil {
		return engineStat{}, errors.New("empty stat answer")
	}
	return *answer.Response, nil
}

func statURL(resp *http.Response) string {
	for req := resp.Request; req != nil; {
		if rest, ok := strings.CutPrefix(req.URL.Path, "/ace/r/"); ok {
			if parts := strings.Split(rest, "/"); len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				u := *req.URL
				u.Path, u.RawPath, u.RawQuery = "/ace/stat/"+parts[0]+"/"+parts[1], "", ""
				return u.String()
			}
		}
		if req.Response == nil {
			break
		}
		req = req.Response.Request
	}
	return ""
}

type swarmSample struct {
	at      time.Time
	stat    engineStat
	pos     float64 // 0 when the engine gives no live position
	relayed int64   // bytes bifrost had relayed by then
}

type swarmStatus struct {
	Status      string    `json:"status"`
	Peers       int       `json:"peers"`
	Download    int64     `json:"download_bytes_per_second"`
	Upload      int64     `json:"upload_bytes_per_second"`
	Bitrate     int64     `json:"bitrate_bytes_per_second,omitempty"`
	Headroom    float64   `json:"headroom,omitempty"` // download over bitrate: below 1 the buffer drains
	Starving    bool      `json:"starving"`
	Stalls      int       `json:"stalls"`
	SampledAt   time.Time `json:"sampled_at"`
	Unavailable string    `json:"unavailable,omitempty"`
}

type swarm struct {
	mu        sync.Mutex
	samples   []swarmSample
	below     int
	stalls    int
	lastErr   string
	since     time.Time // when it started starving
	behind    float64   // seconds of video the download owes the bitrate, paid back when it runs ahead
	threshold float64   // download under this times the bitrate is short; 0 means 1
}

// add records a sample and says whether the stream just started or stopped starving, and whether
// it stalled, so the caller can log it.
func (w *swarm) add(s swarmSample) (starving, recovered, stalled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastErr = ""
	if n := len(w.samples); n > 0 && w.samples[n-1].stat.Status == "dl" && s.stat.Status != "dl" {
		w.stalls++
		stalled = true
	}
	w.samples = append(w.samples, s)
	if len(w.samples) > swarmHistory {
		w.samples = w.samples[len(w.samples)-swarmHistory:]
	}
	wasStarving := w.below >= starvingAfter
	threshold := w.threshold
	if threshold == 0 {
		threshold = 1
	}
	rate := bitrate(w.samples)
	n := len(w.samples)
	// Nothing relayed since the last sample is all owed whatever the bitrate, which a source that
	// sends nothing never lets bifrost measure.
	silent := n > 1 && s.relayed == w.samples[n-2].relayed
	if silent || rate > 0 && float64(s.stat.SpeedDown*kib) < rate*threshold {
		w.below++
	} else {
		w.below = 0
	}
	if n > 1 && (rate > 0 || silent) {
		dt := s.at.Sub(w.samples[n-2].at).Seconds()
		owed := 1.0
		if !silent {
			down := float64(s.stat.SpeedDown * kib)
			if s.stat.Status != "dl" {
				down = 0
			}
			owed = 1 - down/rate
		}
		w.behind = min(max(w.behind+owed*dt, 0), maxBehind.Seconds())
	}
	isStarving := w.below >= starvingAfter
	switch {
	case isStarving && !wasStarving:
		w.since = s.at
	case !isStarving:
		w.since = time.Time{}
	}
	return isStarving && !wasStarving, wasStarving && !isStarving, stalled
}

func (w *swarm) owe(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.behind = min(w.behind+d.Seconds(), maxBehind.Seconds())
}

func (w *swarm) behindBy() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Duration(w.behind * float64(time.Second))
}

func (w *swarm) starvingSince() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.since
}

func (w *swarm) rate() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bitrate(w.samples)
}

func (w *swarm) stalledSoFar() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalls
}

func (w *swarm) reading(since time.Time) reading {
	w.mu.Lock()
	defer w.mu.Unlock()
	var recent []swarmSample
	for _, s := range w.samples {
		if !s.at.Before(since) {
			recent = append(recent, s)
		}
	}
	return readingOf(recent, bitrate(w.samples))
}

func (w *swarm) failed(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastErr = err.Error()
}

func (w *swarm) status() *swarmStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.samples) == 0 {
		if w.lastErr == "" {
			return nil
		}
		return &swarmStatus{Unavailable: w.lastErr}
	}
	last := w.samples[len(w.samples)-1]
	st := &swarmStatus{Status: last.stat.Status, Peers: last.stat.Peers, Download: int64(last.stat.SpeedDown) * kib,
		Upload: int64(last.stat.SpeedUp) * kib, Starving: w.below >= starvingAfter, Stalls: w.stalls, SampledAt: last.at,
		Unavailable: w.lastErr}
	if rate := bitrate(w.samples); rate > 0 {
		st.Bitrate = int64(math.Round(rate))
		st.Headroom = math.Round(float64(st.Download)/rate*100) / 100
	}
	return st
}

// bitrate is the bytes relayed over the last minute divided by how far the engine's live position moved:
// wall time would sink with the download when the engine starves.
func bitrate(samples []swarmSample) float64 {
	if len(samples) < 2 {
		return 0
	}
	last := samples[len(samples)-1]
	first := samples[0]
	for _, s := range samples {
		if last.at.Sub(s.at) <= swarmSpan {
			first = s
			break
		}
	}
	wall := last.at.Sub(first.at).Seconds()
	if wall < swarmMinSpan.Seconds() {
		return 0
	}
	seconds := wall
	// A position that stood still, went back or leapt ahead (the engine skipping to live) is no clock.
	if moved := last.pos - first.pos; first.pos > 0 && moved > 0 && moved <= wall+5 {
		seconds = moved
	}
	return float64(last.relayed-first.relayed) / seconds
}

func (s *stream) watchSwarm(ctx context.Context, url string, w *swarm) {
	client := &http.Client{Timeout: 3 * time.Second}
	ticker := time.NewTicker(s.h.swarmEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sample, err := fetchStat(ctx, client, url)
		if err != nil {
			if ctx.Err() == nil {
				w.failed(err)
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		sample.relayed = s.relayed.Load()
		starving, recovered, stalled := w.add(sample)
		st := w.status()
		switch {
		case starving:
			log.Printf("[swarm] %s starving: %d of %d KB/s, %d peers", s.label, st.Download/kib, st.Bitrate/kib, st.Peers)
		case recovered:
			log.Printf("[swarm] %s keeps up again: %d of %d KB/s, %d peers", s.label, st.Download/kib, st.Bitrate/kib, st.Peers)
		}
		if stalled {
			log.Printf("[swarm] %s stalled, the engine went to %q (%d so far)", s.label, st.Status, st.Stalls)
		}
		s.maybeProbe(w)
	}
}

func fetchStat(ctx context.Context, client *http.Client, url string) (swarmSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return swarmSample{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return swarmSample{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return swarmSample{}, fmt.Errorf("engine stat: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return swarmSample{}, err
	}
	stat, err := parseStat(body)
	if err != nil {
		return swarmSample{}, err
	}
	sample := swarmSample{at: time.Now(), stat: stat}
	if stat.Livepos != nil {
		sample.pos, _ = stat.Livepos.Pos.Float64()
	}
	return sample, nil
}

func swarmMetrics(streams []StreamStatus) []byte {
	var buf bytes.Buffer
	sort.Slice(streams, func(i, j int) bool { return streams[i].Slug < streams[j].Slug })
	metric := func(name, typ, help string, value func(StreamStatus, *swarmStatus) float64) {
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, st := range streams {
			if st.Swarm == nil || st.Swarm.SampledAt.IsZero() {
				continue
			}
			channel := st.Slug
			if channel == "" {
				channel = short(st.Source)
			}
			fmt.Fprintf(&buf, "%s{channel=\"%s\"} %s\n", name, httpx.PrometheusLabel(channel),
				strconv.FormatFloat(value(st, st.Swarm), 'f', -1, 64))
		}
	}
	metric("acestream_stream_viewers", "gauge", "Viewers on the channel's shared stream",
		func(st StreamStatus, _ *swarmStatus) float64 { return float64(st.Viewers) })
	metric("acestream_stream_peers", "gauge", "Peers the engine has for the channel",
		func(_ StreamStatus, w *swarmStatus) float64 { return float64(w.Peers) })
	metric("acestream_stream_download_bytes_per_second", "gauge", "Engine download rate",
		func(_ StreamStatus, w *swarmStatus) float64 { return float64(w.Download) })
	metric("acestream_stream_upload_bytes_per_second", "gauge", "Engine upload rate",
		func(_ StreamStatus, w *swarmStatus) float64 { return float64(w.Upload) })
	metric("acestream_stream_bitrate_bytes_per_second", "gauge", "Stream bitrate, measured from the bytes relayed (0=not known yet)",
		func(_ StreamStatus, w *swarmStatus) float64 { return float64(w.Bitrate) })
	metric("acestream_stream_headroom", "gauge", "Download rate over bitrate, below 1 the engine's buffer drains (0=not known yet)",
		func(_ StreamStatus, w *swarmStatus) float64 { return w.Headroom })
	metric("acestream_stream_starving", "gauge", "1 while the download has stayed under the bitrate",
		func(_ StreamStatus, w *swarmStatus) float64 { return boolGauge(w.Starving) })
	metric("acestream_stream_stalls_total", "counter", "Times the engine left downloading since the stream started",
		func(_ StreamStatus, w *swarmStatus) float64 { return float64(w.Stalls) })
	metric("acestream_stream_source_switches_total", "counter", "Times the channel moved its viewers to a better source since bifrost started",
		func(st StreamStatus, _ *swarmStatus) float64 { return float64(st.channelSwitches) })
	metric("acestream_stream_remux_restarts_total", "counter", "Times the channel's ffmpeg remux was restarted, stuck or ended under a playing source, since bifrost started",
		func(st StreamStatus, _ *swarmStatus) float64 { return float64(st.remuxRestarts) })
	return buf.Bytes()
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// PUT replaces the whole group, so a channel nobody watches disappears. The group is its own job so
// this and the list loop's PUT do not wipe each other.
func (h *Hub) PushSwarm(pushURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	idle := false
	for range time.Tick(h.swarmEvery) {
		streams := h.StreamsNow()
		if len(streams) == 0 && idle {
			continue
		}
		idle = len(streams) == 0
		req, err := http.NewRequest(http.MethodPut, pushURL+"/metrics/job/acestream_streams", bytes.NewReader(swarmMetrics(streams)))
		if err != nil {
			log.Printf("warning: swarm metrics request build failed: %v", err)
			return
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("warning: push swarm metrics failed: %v", err)
			idle = false
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("warning: pushgateway returned %d for the swarm metrics", resp.StatusCode)
			idle = false
		}
	}
}
