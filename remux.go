package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// Each source has its own clock, PIDs and GOP, and splicing one into another raw froze the Samsung
// TV on the first switch. So every source runs through its own `ffmpeg -c copy`, which drops
// everything before the first keyframe and writes the same PIDs every time, and the rebaser below
// shifts each run's timestamps and continuity counters to carry on from the run before it.

const (
	remuxBacklog = 256 // chunks queued for ffmpeg's stdin, 8 MB; a full queue means ffmpeg is stuck
	// How long a handed-over source gets to come out of its remux before the switch is abandoned.
	remuxStartWait = 15 * time.Second
	stderrLines    = 10 // per run, so a corrupt source does not flood the log
)

var remuxArgs = []string{"-hide_banner", "-nostdin", "-loglevel", "error",
	"-probesize", "2000000", "-analyzeduration", "2000000", "-f", "mpegts", "-i", "pipe:0",
	"-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", "pipe:1"}

// remux is one ffmpeg run. write never blocks, so the relay cannot deadlock against ffmpeg's output.
type remux struct {
	in        chan []byte
	out       chan []byte // whole TS packets; closed when ffmpeg exits
	cancel    context.CancelFunc
	once      sync.Once
	delivered bool // set by the relay once it has played something from this run
}

func startRemux(parent context.Context, bin, label string) (*remux, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, bin, remuxArgs...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	r := &remux{in: make(chan []byte, remuxBacklog), out: make(chan []byte, 64), cancel: cancel}
	go func() {
		defer stdin.Close()
		for b := range r.in {
			if _, err := stdin.Write(b); err != nil {
				cancel()
				return
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(stderr)
		for n := 0; sc.Scan(); n++ {
			if n < stderrLines {
				log.Printf("[remux] %s: %s", label, sc.Text())
			}
		}
	}()
	go func() {
		defer close(r.out)
		align := &tsAligner{dropLead: true}
		for {
			buf := make([]byte, chunkSize)
			n, err := stdout.Read(buf)
			if out := align.push(buf[:n]); len(out) > 0 {
				select {
				case r.out <- out:
				case <-ctx.Done():
				}
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					log.Printf("[remux] %s: %v", label, err)
				}
				cmd.Wait()
				return
			}
		}
	}()
	return r, nil
}

func (r *remux) write(b []byte) bool {
	select {
	case r.in <- b:
		return true
	default:
		return false
	}
}

func (r *remux) stop() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		close(r.in)
		r.cancel()
	})
}

const (
	tsClock = 1 << 33 // PTS, DTS and the PCR base wrap at 33 bits of 90 kHz
	// Gap left between the last timestamp of one run and the first of the next: one 25 fps frame.
	spliceStep = 90000 / 25
)

// tsRebaser makes successive ffmpeg runs one stream: the same PIDs (ffmpeg's), continuous
// continuity counters, and timestamps that carry on from the last run instead of restarting.
type tsRebaser struct {
	restart []byte // a new run of the same source, held until it can be spliced; nil when none
	offset  int64
	cc      map[uint16]byte  // next continuity counter per PID
	last    map[uint16]int64 // last DTS (or PTS) written per PID, after the offset
}

func newRebaser() *tsRebaser {
	return &tsRebaser{cc: map[uint16]byte{}, last: map[uint16]int64{}}
}

// spliceLimit is how much of a new run is held for ready before it plays anyway: ~4 s of a 4 Mbit/s channel.
const spliceLimit = 2 << 20

// ready says whether b, the start of a new run, holds a timestamp for every PID the stream has
// played, which splice needs to line them all up.
func (t *tsRebaser) ready(b []byte) bool {
	if len(b) >= spliceLimit {
		return true
	}
	first := firstTimestamps(b)
	for pid := range t.last {
		if _, ok := first[pid]; !ok {
			return false
		}
	}
	return true
}

// splice starts a new run with b. ffmpeg starts video at a keyframe but audio straight away, so
// what comes before the latest PID's start is dropped, and the rest is shifted to carry on just
// after where the last run stopped.
func (t *tsRebaser) splice(b []byte) []byte {
	first := firstTimestamps(b)
	start, common := int64(0), false
	for pid, f := range first {
		if _, ok := t.last[pid]; ok && (!common || wrapDiff(f, start) > 0) {
			start, common = f, true
		}
	}
	if !common {
		return t.push(b)
	}
	kept := make([]byte, 0, len(b))
	dropping := map[uint16]bool{}
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		p := b[i : i+tsPacket]
		pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
		if _, ts, ok := pesTimestamp(p); ok {
			dropping[pid] = wrapDiff(ts, start) < 0
		}
		if !dropping[pid] {
			kept = append(kept, p...)
		}
	}
	shift, found := int64(0), false
	for pid, f := range firstTimestamps(kept) {
		if last, ok := t.last[pid]; ok {
			if d := wrapDiff(last, f) + spliceStep; !found || d > shift {
				shift, found = d, true
			}
		}
	}
	t.offset = shift
	return t.push(kept)
}

// firstTimestamps is the earliest DTS (or PTS) of each PID in b.
func firstTimestamps(b []byte) map[uint16]int64 {
	first := map[uint16]int64{}
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		pid, ts, ok := pesTimestamp(b[i : i+tsPacket])
		if !ok {
			continue
		}
		if f, seen := first[pid]; !seen || wrapDiff(ts, f) < 0 {
			first[pid] = ts
		}
	}
	return first
}

// push rewrites whole packets of the current run in place.
func (t *tsRebaser) push(b []byte) []byte {
	for i := 0; i+tsPacket <= len(b); i += tsPacket {
		t.rewrite(b[i : i+tsPacket])
	}
	return b
}

func (t *tsRebaser) rewrite(p []byte) {
	if p[0] != tsSync {
		return
	}
	pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
	afc := p[3] >> 4 & 3
	next := t.cc[pid]
	if afc&1 != 0 {
		p[3] = p[3]&0xf0 | next
		t.cc[pid] = (next + 1) & 0xf
	} else {
		p[3] = p[3]&0xf0 | (next-1)&0xf
	}
	payload := 4
	if afc&2 != 0 {
		alen := int(p[4])
		if alen > 0 && 5+alen <= tsPacket && p[5]&0x10 != 0 && alen >= 7 {
			writePCR(p[6:12], wrap(readPCR(p[6:12])+t.offset))
		}
		payload = 5 + alen
	}
	if afc&1 == 0 || p[1]&0x40 == 0 || payload+14 > tsPacket {
		return
	}
	pes := p[payload:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return
	}
	flags := pes[7] >> 6
	if flags&2 == 0 {
		return
	}
	pts := wrap(readTS(pes[9:14]) + t.offset)
	writeTS(pes[9:14], pts)
	last := pts
	if flags == 3 && payload+19 <= tsPacket {
		dts := wrap(readTS(pes[14:19]) + t.offset)
		writeTS(pes[14:19], dts)
		last = dts
	}
	t.last[pid] = last
}

// pesTimestamp is a packet's DTS, or its PTS when it has no DTS, if it starts a PES that has one.
func pesTimestamp(p []byte) (uint16, int64, bool) {
	if p[0] != tsSync || p[1]&0x40 == 0 {
		return 0, 0, false
	}
	pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
	afc := p[3] >> 4 & 3
	payload := 4
	if afc&2 != 0 {
		payload = 5 + int(p[4])
	}
	if afc&1 == 0 || payload+19 > tsPacket {
		return 0, 0, false
	}
	pes := p[payload:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return 0, 0, false
	}
	switch pes[7] >> 6 {
	case 2:
		return pid, readTS(pes[9:14]), true
	case 3:
		return pid, readTS(pes[14:19]), true
	}
	return 0, 0, false
}

func readTS(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

func writeTS(b []byte, ts int64) {
	b[0] = b[0]&0xf0 | byte(ts>>29)&0x0e | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14)&0xfe | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1)&0xfe | 1
}

// readPCR is the PCR's 90 kHz base; the 27 MHz extension is left as it is.
func readPCR(b []byte) int64 {
	return int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 | int64(b[3])<<1 | int64(b[4]>>7)
}

func writePCR(b []byte, base int64) {
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = b[4]&0x7f | byte(base&1)<<7
}

func wrap(ts int64) int64 {
	return (ts%tsClock + tsClock) % tsClock
}

// wrapDiff is a-b on the 33-bit clock, between -2^32 and 2^32.
func wrapDiff(a, b int64) int64 {
	return wrap(a-b+tsClock/2) - tsClock/2
}
