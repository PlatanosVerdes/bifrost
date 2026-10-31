// Package slate plays the embedded off-air and loading loops in real time.
package slate

import (
	_ "embed"
	"errors"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/mpegts"
)

// Made by render/render.py.
//
//go:embed offair.ts
var OffAir []byte

// Made by render/loading.py.
//
//go:embed loading.ts
var Loading []byte

const (
	Tick = 100 * time.Millisecond
	Lead = 2 * time.Second // sent at once, so a player starts without waiting
)

var Retry = 30 * time.Second

// Its tracks last exactly as long as each other, so each run carries the clock on by ticks.
type Slate struct {
	loop     []byte
	head     int
	Ticks    int64
	duration time.Duration
}

func New(b []byte) (*Slate, error) {
	b = b[:len(b)/mpegts.PacketSize*mpegts.PacketSize]
	if len(b) == 0 || mpegts.SyncAt(b) != 0 {
		return nil, errors.New("not a transport stream")
	}
	pids := mpegts.FirstTimestamps(b)
	if len(pids) == 0 {
		return nil, errors.New("no timestamps")
	}
	head := 0
	for ; head < len(b); head += mpegts.PacketSize {
		if len(mpegts.FirstTimestamps(b[:head])) == len(pids) {
			break
		}
	}
	// The video is the PID with the most PES starts, one a frame: its span plus a frame is the run.
	count, first, last, gap := map[uint16]int{}, map[uint16]int64{}, map[uint16]int64{}, map[uint16]int64{}
	video := uint16(0)
	for i := 0; i+mpegts.PacketSize <= len(b); i += mpegts.PacketSize {
		pid, ts, ok := mpegts.PESTimestamp(b[i : i+mpegts.PacketSize])
		if !ok {
			continue
		}
		if count[pid] == 0 {
			first[pid] = ts
		} else {
			gap[pid] = mpegts.WrapDiff(ts, last[pid])
		}
		last[pid] = ts
		if count[pid]++; count[pid] > count[video] {
			video = pid
		}
	}
	ticks := mpegts.WrapDiff(last[video], first[video]) + gap[video]
	if ticks <= 0 {
		return nil, errors.New("no duration")
	}
	return &Slate{loop: b, head: head, Ticks: ticks, duration: time.Duration(ticks) * time.Second / 90000}, nil
}

type Player struct {
	Slate   *Slate
	Lead    time.Duration
	pos     int
	started time.Time
	Sent    int64
}

func (p *Player) Next(now time.Time) ([]byte, bool) {
	if p.started.IsZero() {
		p.started = now
	}
	rate := float64(len(p.Slate.loop)) / p.Slate.duration.Seconds()
	due := int64(rate*(now.Sub(p.started)+p.Lead).Seconds()) - p.Sent
	if due < mpegts.PacketSize {
		return nil, false
	}
	if p.pos == len(p.Slate.loop) {
		p.pos = 0
	}
	fresh := p.pos == 0
	n := int(due) / mpegts.PacketSize * mpegts.PacketSize
	if fresh {
		n = max(n, p.Slate.head)
	}
	n = min(n, len(p.Slate.loop)-p.pos)
	out := append([]byte(nil), p.Slate.loop[p.pos:p.pos+n]...)
	p.pos += n
	p.Sent += int64(n)
	return out, fresh
}

// Duration is how long one run of the loop plays.
func (s *Slate) Duration() time.Duration {
	return s.duration
}
