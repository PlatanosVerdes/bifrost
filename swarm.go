package main

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
	"strings"
	"sync"
	"time"
)

// The engine's live statistics for a running stream. Aceserve redirects /ace/getstream to
// /ace/r/<infohash>/<session>, and /ace/stat/<infohash>/<session> answers peers and speeds for that
// same playback: reading it changes nothing about the stream, so bifrost can watch a channel
// without opening a second session. The engine does not say the stream's bitrate, so it is measured
// from the bytes bifrost relays.

const (
	swarmHistory = 24 // samples kept per stream: two minutes at the default poll
	// The bitrate is measured over this much of the history, and not before it spans swarmMinSpan.
	swarmSpan    = 60 * time.Second
	swarmMinSpan = 10 * time.Second
	// Samples in a row with the download under the bitrate before a stream counts as starving: one
	// slow sample is the engine's speed jittering, three are its buffer draining.
	starvingAfter = 3
	kib           = 1024
)

// engineStat is the part of the engine's stat answer worth keeping. Speeds are in KiB/s.
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

// statURL finds the engine session behind a stream in its redirect chain.
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

// swarmStatus is what /api/status says about a stream's swarm.
type swarmStatus struct {
	Status      string    `json:"status"`
	Peers       int       `json:"peers"`
	SpeedDown   int       `json:"speed_down_kbps"`
	SpeedUp     int       `json:"speed_up_kbps"`
	Bitrate     int       `json:"bitrate_kbps,omitempty"`
	Headroom    float64   `json:"headroom,omitempty"` // download over bitrate: below 1 the buffer drains
	Starving    bool      `json:"starving"`
	Stalls      int       `json:"stalls"`
	SampledAt   time.Time `json:"sampled_at"`
	Unavailable string    `json:"unavailable,omitempty"`
}

type swarm struct {
	mu      sync.Mutex
	samples []swarmSample
	below   int
	stalls  int
	lastErr string
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
	if rate := bitrate(w.samples); rate > 0 && float64(s.stat.SpeedDown) < rate {
		w.below++
	} else {
		w.below = 0
	}
	isStarving := w.below >= starvingAfter
	return isStarving && !wasStarving, wasStarving && !isStarving, stalled
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
	st := &swarmStatus{Status: last.stat.Status, Peers: last.stat.Peers, SpeedDown: last.stat.SpeedDown,
		SpeedUp: last.stat.SpeedUp, Starving: w.below >= starvingAfter, Stalls: w.stalls, SampledAt: last.at,
		Unavailable: w.lastErr}
	if rate := bitrate(w.samples); rate > 0 {
		st.Bitrate = int(math.Round(rate))
		st.Headroom = math.Round(float64(last.stat.SpeedDown)/rate*100) / 100
	}
	return st
}

// bitrate is the stream's rate in KiB/s: the bytes relayed over the last minute, divided by how far
// the engine's live position moved in it. Wall time would do while the swarm keeps up, but a
// starving engine relays slower than the stream plays, and the bitrate would sink with the download.
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
	return float64(last.relayed-first.relayed) / seconds / kib
}

// watchSwarm polls the engine's stat for as long as the stream runs.
func (s *stream) watchSwarm(ctx context.Context, url string) {
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
				s.swarm.failed(err)
			}
			continue
		}
		sample.relayed = s.relayed.Load()
		starving, recovered, stalled := s.swarm.add(sample)
		st := s.swarm.status()
		switch {
		case starving:
			log.Printf("[swarm] %s starving: %d of %d KB/s, %d peers", s.label, st.SpeedDown, st.Bitrate, st.Peers)
		case recovered:
			log.Printf("[swarm] %s keeps up again: %d of %d KB/s, %d peers", s.label, st.SpeedDown, st.Bitrate, st.Peers)
		}
		if stalled {
			log.Printf("[swarm] %s stalled, the engine went to %q (%d so far)", s.label, st.Status, st.Stalls)
		}
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

// swarmMetrics writes one gauge per playing channel in the pushgateway's text format.
func swarmMetrics(streams []streamStatus) []byte {
	var buf bytes.Buffer
	sort.Slice(streams, func(i, j int) bool { return streams[i].Slug < streams[j].Slug })
	metric := func(name, typ, help string, value func(streamStatus, *swarmStatus) float64) {
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
		for _, st := range streams {
			if st.Swarm == nil || st.Swarm.SampledAt.IsZero() {
				continue
			}
			channel := st.Slug
			if channel == "" {
				channel = short(st.Source)
			}
			fmt.Fprintf(&buf, "%s{channel=\"%s\"} %g\n", name, prometheusLabel(channel), value(st, st.Swarm))
		}
	}
	metric("acestream_stream_viewers", "gauge", "Viewers on the channel's shared stream",
		func(st streamStatus, _ *swarmStatus) float64 { return float64(st.Viewers) })
	metric("acestream_stream_peers", "gauge", "Peers the engine has for the channel",
		func(_ streamStatus, w *swarmStatus) float64 { return float64(w.Peers) })
	metric("acestream_stream_speed_down_kbps", "gauge", "Engine download rate in KiB/s",
		func(_ streamStatus, w *swarmStatus) float64 { return float64(w.SpeedDown) })
	metric("acestream_stream_speed_up_kbps", "gauge", "Engine upload rate in KiB/s",
		func(_ streamStatus, w *swarmStatus) float64 { return float64(w.SpeedUp) })
	metric("acestream_stream_bitrate_kbps", "gauge", "Stream bitrate in KiB/s, measured from the bytes relayed (0=not known yet)",
		func(_ streamStatus, w *swarmStatus) float64 { return float64(w.Bitrate) })
	metric("acestream_stream_headroom", "gauge", "Download rate over bitrate, below 1 the engine's buffer drains (0=not known yet)",
		func(_ streamStatus, w *swarmStatus) float64 { return w.Headroom })
	metric("acestream_stream_starving", "gauge", "1 while the download has stayed under the bitrate",
		func(_ streamStatus, w *swarmStatus) float64 { return boolGauge(w.Starving) })
	metric("acestream_stream_stalls_total", "counter", "Times the engine left downloading since the stream started",
		func(_ streamStatus, w *swarmStatus) float64 { return float64(w.Stalls) })
	return buf.Bytes()
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// pushSwarm sends the playing channels' gauges on every poll. PUT replaces the whole group, so a
// channel nobody watches any more disappears instead of keeping its last values; the group is its
// own job so this and the list loop's PUT do not wipe each other.
func (h *hub) pushSwarm(pushURL string) {
	client := &http.Client{Timeout: 10 * time.Second}
	idle := false
	for range time.Tick(h.swarmEvery) {
		streams := h.streamsNow()
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
