package stream

import (
	"bufio"
	"context"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/mpegts"
)

// Splicing sources raw froze the Samsung TV: each has its own clock, PIDs and GOP. So each source
// gets its own `ffmpeg -c copy` run, and tsRebaser makes the runs one continuous stream.

const (
	// 2 MB: well under a viewer's backlog, since ffmpeg passes its whole queue on at once when it starts.
	remuxBacklog    = 64
	remuxStuckAfter = 10 * time.Second
	remuxStartWait  = 15 * time.Second
	stderrLines     = 5 // the end of ffmpeg's stderr, logged when it exits on its own
)

// Loglevel info gives the stream listing the page shows; the rest of stderr is probing-decoder noise.
var remuxArgs = []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "info",
	"-probesize", "2000000", "-analyzeduration", "2000000", "-f", "mpegts", "-i", "pipe:0",
	"-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", "pipe:1"}

type remux struct {
	fullSince time.Time // when the relay last found the queue full; zero while it has room
	in        chan []byte
	out       chan []byte // whole TS packets; closed when ffmpeg exits
	cancel    context.CancelFunc
	once      sync.Once
	delivered bool // set by the relay once it has played something from this run
}

func startRemux(parent context.Context, bin, label string, onInfo func(height int, audio string)) (*remux, error) {
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
		var info listing
		var last []string
		listed := false
		for sc.Scan() {
			line := sc.Text()
			if !listed {
				if strings.HasPrefix(line, "Output #0") {
					listed = true
					onInfo(info.height, info.audios())
				} else {
					info.read(line)
				}
			}
			if last = append(last, line); len(last) > stderrLines {
				last = last[1:]
			}
		}
		if ctx.Err() == nil {
			for _, line := range last {
				log.Printf("[remux] %s: %s", label, line)
			}
		}
	}()
	go func() {
		defer close(r.out)
		align := &mpegts.Aligner{DropLead: true}
		for {
			// Whole chunks: a viewer's backlog counts chunks, and few-KB pipe reads filled it and dropped the viewer.
			buf := make([]byte, chunkSize)
			n, err := io.ReadFull(stdout, buf)
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			if out := align.Push(buf[:n]); len(out) > 0 {
				select {
				case r.out <- out:
				case <-ctx.Done():
				}
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					log.Printf("[remux] %s: %v", label, err)
				}
				if err := cmd.Wait(); err != nil && ctx.Err() == nil {
					log.Printf("[remux] %s: ffmpeg %v", label, err)
				}
				return
			}
		}
	}()
	return r, nil
}

// A full queue stops reading the engine, which holds the rest, instead of dropping what does not fit.
func (r *remux) feed(chunks chan []byte) <-chan []byte {
	if r == nil {
		return chunks
	}
	if len(r.in) < cap(r.in) {
		r.fullSince = time.Time{}
		return chunks
	}
	if r.fullSince.IsZero() {
		r.fullSince = time.Now()
	}
	return nil
}

func (r *remux) write(b []byte) {
	r.in <- b
}

func (r *remux) stuckFor() time.Duration {
	if r == nil || r.fullSince.IsZero() {
		return 0
	}
	return time.Since(r.fullSince)
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
	spliceStep = 90000 / 25
)

type tsRebaser struct {
	restart []byte // a new run of the same source, held until it can be spliced; nil when none
	offset  int64
	start   int64            // the new run's start, which cut drops each PID's packets up to
	cutting map[uint16]bool  // PIDs whose packets cut drops now
	pending map[uint16]bool  // PIDs that have not reached start yet; nil when nothing is cut
	cc      map[uint16]byte  // next continuity counter per PID
	last    map[uint16]int64 // last DTS (or PTS) written per PID, after the offset
	// The last spacing between PES starts per PID: an audio PES holds several frames, so a run
	// carries on that far after the last PES start, not one frame.
	gap  map[uint16]int64
	pmts map[uint16]bool // PMT PIDs, from the PAT
	// Every ffmpeg run writes PMT version 0, so a changed PMT gets the next one or the player ignores it.
	pmtBody    map[uint16]string
	pmtVersion map[uint16]byte
}

func newRebaser() *tsRebaser {
	return &tsRebaser{cc: map[uint16]byte{}, last: map[uint16]int64{}, gap: map[uint16]int64{}, pmts: map[uint16]bool{},
		pmtBody: map[uint16]string{}, pmtVersion: map[uint16]byte{}}
}

const spliceLimit = 2 << 20

func (t *tsRebaser) ready(b []byte) bool {
	if len(b) >= spliceLimit {
		return true
	}
	first := mpegts.FirstTimestamps(b)
	for pid := range t.last {
		if _, ok := first[pid]; !ok {
			return false
		}
	}
	return true
}

// ffmpeg starts video at a keyframe but audio straight away: audio before the keyframe is dropped,
// until each PID reaches it, since some of it comes after the keyframe in the stream.
func (t *tsRebaser) splice(b []byte) []byte {
	first := mpegts.FirstTimestamps(b)
	start, common := int64(0), false
	for pid, f := range mpegts.FirstPTS(b) {
		if _, ok := t.last[pid]; ok && (!common || mpegts.WrapDiff(f, start) > 0) {
			start, common = f, true
		}
	}
	if !common {
		return t.push(b)
	}
	t.start, t.cutting, t.pending = start, map[uint16]bool{}, map[uint16]bool{}
	for pid := range first {
		if _, ok := t.last[pid]; ok {
			t.pending[pid] = true
		}
	}
	kept := t.cut(b)
	keptFirst := mpegts.FirstTimestamps(kept)
	shift, found := int64(0), false
	for pid := range first {
		last, ok := t.last[pid]
		if !ok {
			continue
		}
		f, seen := keptFirst[pid]
		if !seen {
			f = start
		}
		step := t.gap[pid]
		if step == 0 {
			step = spliceStep
		}
		if d := mpegts.WrapDiff(last, f) + step; !found || d > shift {
			shift, found = d, true
		}
	}
	t.offset = shift
	t.rewriteAll(kept)
	return kept
}

// For a run that begins exactly where the last one ended.
func (t *tsRebaser) advance(ticks int64, b []byte) []byte {
	t.offset += ticks
	return t.push(b)
}

func (t *tsRebaser) cut(b []byte) []byte {
	if t.pending == nil {
		return b
	}
	kept := make([]byte, 0, len(b))
	for i := 0; i+mpegts.PacketSize <= len(b); i += mpegts.PacketSize {
		p := b[i : i+mpegts.PacketSize]
		pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
		if _, pts, _, ok := mpegts.PESTimes(p); ok && t.pending[pid] || t.cutting[pid] {
			if ok {
				t.cutting[pid] = mpegts.WrapDiff(pts, t.start) < 0
				if !t.cutting[pid] {
					delete(t.pending, pid)
				}
			}
		}
		if !t.cutting[pid] {
			kept = append(kept, p...)
		}
	}
	if len(t.pending) == 0 {
		t.pending, t.cutting = nil, nil
	}
	return kept
}

func (t *tsRebaser) push(b []byte) []byte {
	b = t.cut(b)
	t.rewriteAll(b)
	return b
}

func (t *tsRebaser) rewriteAll(b []byte) {
	for i := 0; i+mpegts.PacketSize <= len(b); i += mpegts.PacketSize {
		t.rewrite(b[i : i+mpegts.PacketSize])
	}
}

func (t *tsRebaser) rewrite(p []byte) {
	if p[0] != mpegts.SyncByte {
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
		if alen > 0 && 5+alen <= mpegts.PacketSize && p[5]&0x10 != 0 && alen >= 7 {
			mpegts.WritePCR(p[6:12], mpegts.Wrap(mpegts.ReadPCR(p[6:12])+t.offset))
		}
		payload = 5 + alen
	}
	if afc&1 == 0 || p[1]&0x40 == 0 || payload+14 > mpegts.PacketSize {
		return
	}
	switch {
	case pid == 0:
		t.notePAT(p[payload:])
		return
	case t.pmts[pid]:
		t.versionPMT(pid, p[payload:])
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
	pts := mpegts.Wrap(mpegts.ReadTS(pes[9:14]) + t.offset)
	mpegts.WriteTS(pes[9:14], pts)
	last := pts
	if flags == 3 && payload+19 <= mpegts.PacketSize {
		dts := mpegts.Wrap(mpegts.ReadTS(pes[14:19]) + t.offset)
		mpegts.WriteTS(pes[14:19], dts)
		last = dts
	}
	if prev, ok := t.last[pid]; ok {
		if d := mpegts.WrapDiff(last, prev); d > 0 && d < 90000 {
			t.gap[pid] = d
		}
	}
	t.last[pid] = last
}

func (t *tsRebaser) notePAT(payload []byte) {
	sec := mpegts.PSISection(payload)
	if sec == nil || sec[0] != 0 {
		return
	}
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		if program := int(sec[i])<<8 | int(sec[i+1]); program != 0 {
			t.pmts[uint16(sec[i+2]&0x1f)<<8|uint16(sec[i+3])] = true
		}
	}
}

func (t *tsRebaser) versionPMT(pid uint16, payload []byte) {
	sec := mpegts.PSISection(payload)
	if sec == nil || sec[0] != 2 {
		return
	}
	body := string(sec[:5]) + string(sec[6:len(sec)-4])
	version, seen := t.pmtVersion[pid]
	switch {
	case !seen:
		version = sec[5] >> 1 & 0x1f
	case body != t.pmtBody[pid]:
		version = (version + 1) & 0x1f
	}
	t.pmtBody[pid], t.pmtVersion[pid] = body, version
	if sec[5]>>1&0x1f == version {
		return
	}
	sec[5] = sec[5]&0xc1 | version<<1
	crc := mpegts.CRC32(sec[:len(sec)-4])
	sec[len(sec)-4], sec[len(sec)-3], sec[len(sec)-2], sec[len(sec)-1] = byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)
}
