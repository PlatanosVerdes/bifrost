package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pesPacket is a TS packet on pid starting a PES with pts and dts, and a PCR when pcr >= 0.
func pesPacket(pid uint16, cc byte, pts, dts, pcr int64) []byte {
	p := make([]byte, tsPacket)
	p[0], p[1], p[2] = tsSync, 0x40|byte(pid>>8), byte(pid)
	payload := 4
	if pcr >= 0 {
		p[3] = 0x30 | cc
		p[4], p[5] = 7, 0x10
		writePCR(p[6:12], pcr)
		payload = 12
	} else {
		p[3] = 0x10 | cc
	}
	pes := p[payload:]
	pes[2], pes[3], pes[7], pes[8] = 1, 0xe0, 0xc0, 10
	pes[9], pes[14] = 0x30, 0x10
	writeTS(pes[9:14], pts)
	writeTS(pes[14:19], dts)
	return p
}

func TestRebaserCarriesTheClockAcrossRuns(t *testing.T) {
	reb := newRebaser()
	var first []byte
	for i := int64(0); i < 5; i++ {
		ts := tsClock - 5*3600 + i*3600 // the last run ends just before the 33-bit clock wraps
		first = append(first, pesPacket(0x100, byte(i+7), ts+3600, ts, ts-900)...)
	}
	reb.push(first)

	var second []byte
	for i := int64(0); i < 3; i++ {
		ts := 90000*7 + i*3600 // a new run restarts on its own clock, and its counters from 0
		second = append(second, pesPacket(0x100, byte(i), ts+3600, ts, ts-900)...)
	}
	out := reb.splice(second)

	_, last, _ := pesTimestamp(first[4*tsPacket:])
	for i := 0; i < 3; i++ {
		p := out[i*tsPacket : (i+1)*tsPacket]
		_, dts, _ := pesTimestamp(p)
		if want := wrap(last + spliceStep + int64(i)*3600); dts != want {
			t.Errorf("packet %d: DTS %d, want %d, right after the last run", i, dts, want)
		}
		if got := readTS(p[12+9 : 12+14]); got != wrap(dts+3600) {
			t.Errorf("packet %d: PTS %d, want DTS+3600 = %d", i, got, wrap(dts+3600))
		}
		if pcr := readPCR(p[6:12]); pcr != wrap(dts-900) {
			t.Errorf("packet %d: PCR %d, want DTS-900 = %d", i, pcr, wrap(dts-900))
		}
		if cc := p[3] & 0xf; cc != byte(5+i) {
			t.Errorf("packet %d: continuity counter %d, want %d", i, cc, 5+i)
		}
	}
}

func TestRebaserLeavesOtherPacketsAlone(t *testing.T) {
	pat := make([]byte, tsPacket)
	pat[0], pat[1], pat[3] = tsSync, 0x40, 0x10
	copy(pat[4:], []byte{0, 0, 0xb0, 0x0d})
	want := append([]byte(nil), pat...)
	newRebaser().splice(pat)
	if !bytes.Equal(pat, want) {
		t.Error("a PAT was changed")
	}
}

// Two runs of real ffmpeg on two sources with unrelated clocks and PIDs, cut mid-GOP the way a
// switch cuts them, must come out as one stream whose timestamps only move forward.
func TestRemuxedSwitchKeepsOneClock(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	dir := t.TempDir()
	source := func(name string, offset string, extra ...string) []byte {
		path := filepath.Join(dir, name)
		args := append([]string{"-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-f", "lavfi",
			"-i", "sine=sample_rate=48000", "-t", "8", "-c:v", "libx264", "-g", "50", "-preset", "ultrafast", "-c:a", "aac",
			"-output_ts_offset", offset}, extra...)
		if out, err := exec.Command(bin, append(args, "-f", "mpegts", path)...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot make a test source: %v %s", err, out)
		}
		b, _ := os.ReadFile(path)
		return b
	}
	a := source("a.ts", "1000")
	b := source("b.ts", "23000", "-mpegts_start_pid", "0x200", "-mpegts_pmt_start_pid", "0x1500")
	a = a[:len(a)/2/tsPacket*tsPacket]
	b = b[(len(b)/3/tsPacket+7)*tsPacket:]

	reb := newRebaser()
	var joined []byte
	for i, src := range [][]byte{a, b} {
		r, err := startRemux(context.Background(), bin, "test")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for rest := src; len(rest) > 0; {
				n := min(len(rest), chunkSize)
				for !r.write(rest[:n]) {
					time.Sleep(time.Millisecond)
				}
				rest = rest[n:]
			}
			close(r.in)
		}()
		var held []byte
		spliced := i == 0
		for out := range r.out {
			switch {
			case spliced:
				joined = append(joined, reb.push(out)...)
			case reb.ready(append(held, out...)):
				joined = append(joined, reb.splice(append(held, out...))...)
				spliced = true
			default:
				held = append(held, out...)
			}
		}
		if !spliced {
			t.Fatalf("run %d never had a timestamp for every stream", i)
		}
	}
	path := filepath.Join(dir, "joined.ts")
	os.WriteFile(path, joined, 0o644)

	for _, stream := range []string{"v:0", "a:0"} {
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", "packet=dts_time",
			"-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var prev float64
		n := 0
		for _, line := range strings.Fields(string(out)) {
			ts, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64)
			if err != nil {
				continue
			}
			if n > 0 && (ts < prev || ts-prev > 0.5) {
				t.Errorf("%s jumps from %.3f to %.3f", stream, prev, ts)
			}
			prev = ts
			n++
		}
		// About 4 s of the first source and 5 of the second, minus what precedes the second's keyframe.
		if prev < 7 {
			t.Errorf("%s ends at %.1fs over %d packets, want both sources in it", stream, prev, n)
		}
	}
}
