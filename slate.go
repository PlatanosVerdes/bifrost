package main

import (
	_ "embed"
	"errors"
	"time"
)

// Made by slate/render.py.
//
//go:embed slate/offair.ts
var offairTS []byte

// Made by slate/loading.py.
//
//go:embed slate/loading.ts
var loadingTS []byte

const (
	slateTick = 100 * time.Millisecond
	slateLead = 2 * time.Second // sent at once, so a player starts without waiting
)

var slateRetry = 30 * time.Second

// Its tracks last exactly as long as each other, so each run carries the clock on by ticks.
type slate struct {
	loop     []byte
	head     int
	ticks    int64
	duration time.Duration
}

func newSlate(b []byte) (*slate, error) {
	b = b[:len(b)/tsPacket*tsPacket]
	if len(b) == 0 || tsSyncAt(b) != 0 {
		return nil, errors.New("not a transport stream")
	}
	pids := firstTimestamps(b)
	if len(pids) == 0 {
		return nil, errors.New("no timestamps")
	}
	head := 0
	for ; head < len(b); head += tsPacket {
		if len(firstTimestamps(b[:head])) == len(pids) {
			break
		}
	}
	// The video is the PID with the most PES starts, one a frame: its span plus a frame is the run.
	count, first, last, gap := map[uint16]int{}, map[uint16]int64{}, map[uint16]int64{}, map[uint16]int64{}
	video := uint16(0)
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		pid, ts, ok := pesTimestamp(b[i : i+tsPacket])
		if !ok {
			continue
		}
		if count[pid] == 0 {
			first[pid] = ts
		} else {
			gap[pid] = wrapDiff(ts, last[pid])
		}
		last[pid] = ts
		if count[pid]++; count[pid] > count[video] {
			video = pid
		}
	}
	ticks := wrapDiff(last[video], first[video]) + gap[video]
	if ticks <= 0 {
		return nil, errors.New("no duration")
	}
	return &slate{loop: b, head: head, ticks: ticks, duration: time.Duration(ticks) * time.Second / 90000}, nil
}

type slatePlayer struct {
	sl      *slate
	lead    time.Duration
	pos     int
	started time.Time
	sent    int64
}

func (p *slatePlayer) next(now time.Time) ([]byte, bool) {
	if p.started.IsZero() {
		p.started = now
	}
	rate := float64(len(p.sl.loop)) / p.sl.duration.Seconds()
	due := int64(rate*(now.Sub(p.started)+p.lead).Seconds()) - p.sent
	if due < tsPacket {
		return nil, false
	}
	if p.pos == len(p.sl.loop) {
		p.pos = 0
	}
	fresh := p.pos == 0
	n := int(due) / tsPacket * tsPacket
	if fresh {
		n = max(n, p.sl.head)
	}
	n = min(n, len(p.sl.loop)-p.pos)
	out := append([]byte(nil), p.sl.loop[p.pos:p.pos+n]...)
	p.pos += n
	p.sent += int64(n)
	return out, fresh
}
