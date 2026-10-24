package main

import (
	"bytes"
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
		r, err := startRemux(context.Background(), bin, "test", func(int, string) {})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for rest := src; len(rest) > 0; {
				n := min(len(rest), chunkSize)
				r.write(rest[:n])
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
		if prev < 7 {
			t.Errorf("%s ends at %.1fs over %d packets, want both sources in it", stream, prev, n)
		}
	}
}

func psiPacket(pid uint16, section []byte) []byte {
	p := bytes.Repeat([]byte{0xff}, tsPacket)
	p[0], p[1], p[2], p[3], p[4] = tsSync, 0x40|byte(pid>>8), byte(pid), 0x10, 0
	crc := crc32MPEG(section)
	copy(p[5:], append(section, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)))
	return p
}

func pmtSection(streamType byte) []byte {
	// table 2, length 18, program 1, version 0, PCR PID 0x100, one stream of streamType on 0x100
	return []byte{2, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0, 0xf0, 0, streamType, 0xe1, 0, 0xf0, 0}
}

func TestRebaserVersionsAChangedPMT(t *testing.T) {
	pat := psiPacket(0, []byte{0, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0})
	reb := newRebaser()
	section := func(p []byte) []byte { return psiSection(p[4:]) }

	h264 := psiPacket(0x1000, pmtSection(0x1b))
	want := append([]byte(nil), h264...)
	reb.push(append(append([]byte(nil), pat...), h264...))
	if !bytes.Equal(h264, want) {
		t.Error("the first PMT was changed")
	}

	again := reb.splice(append(append([]byte(nil), pat...), psiPacket(0x1000, pmtSection(0x1b))...))[tsPacket:]
	if !bytes.Equal(section(again), section(want)) {
		t.Error("a PMT the same as the last one was changed")
	}

	hevc := reb.splice(append(append([]byte(nil), pat...), psiPacket(0x1000, pmtSection(0x24))...))[tsPacket:]
	sec := section(hevc)
	if v := sec[5] >> 1 & 0x1f; v != 1 {
		t.Errorf("a PMT with another codec kept version %d, want 1", v)
	}
	if crc32MPEG(sec) != 0 {
		t.Error("the versioned PMT's CRC does not check")
	}

	repeat := psiPacket(0x1000, pmtSection(0x24))
	reb.push(repeat)
	if v := section(repeat)[5] >> 1 & 0x1f; v != 1 {
		t.Errorf("the run's repeated PMT has version %d, want 1 like its first", v)
	}
}

func TestCRCMatchesFFmpeg(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	out, err := exec.Command(bin, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=25", "-t", "1",
		"-c:v", "libx264", "-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg cannot make a test source: %v", err)
	}
	checked := 0
	for i := 0; i+tsPacket <= len(out); i += tsPacket {
		p := out[i : i+tsPacket]
		pid := uint16(p[1]&0x1f)<<8 | uint16(p[2])
		if p[1]&0x40 == 0 || (pid != 0 && pid != 0x1000) {
			continue
		}
		if sec := psiSection(p[4:]); sec != nil {
			if crc32MPEG(sec) != 0 {
				t.Fatalf("PID %#x: ffmpeg's section does not check against our CRC", pid)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("found no PAT or PMT to check")
	}
}

func TestSwitchThroughTheProxyKeepsOneClock(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	dir := t.TempDir()
	source := func(name, offset string, extra ...string) []byte {
		path := filepath.Join(dir, name)
		args := append([]string{"-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-f", "lavfi",
			"-i", "sine=sample_rate=48000", "-t", "30", "-c:v", "libx264", "-g", "50", "-preset", "ultrafast",
			"-b:v", "300k", "-c:a", "aac", "-output_ts_offset", offset}, extra...)
		if out, err := exec.Command(bin, append(args, "-f", "mpegts", path)...).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot make a test source: %v %s", err, out)
		}
		b, _ := os.ReadFile(path)
		return b
	}
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 4000})
	engine.media = map[string][]byte{slowSource: source("slow.ts", "5000"),
		fastSource: source("fast.ts", "70000", "-mpegts_start_pid", "0x300", "-mpegts_pmt_start_pid", "0x1400")}
	srv, h := newSwitchingProxy(t, engine, slowSource, fastSource)
	h.ffmpeg = bin

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/dazn-laliga-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	h.mu.Lock()
	switches := h.switches["dazn-laliga-1080p"]
	h.mu.Unlock()
	if switches != 1 {
		t.Fatalf("%d switches, want the slow source switched to the fast one once", switches)
	}
	for _, f := range h.sourcesOf([]string{slowSource, fastSource}) {
		if f.Height != 240 || f.Audio != "AAC mono" {
			t.Errorf("%s: %dp %q from ffmpeg's listing, want 240p AAC mono", short(f.ID), f.Height, f.Audio)
		}
	}
	path := filepath.Join(dir, "watched.ts")
	os.WriteFile(path, got[:len(got)/tsPacket*tsPacket], 0o644)
	// Spliced raw, the new source's PIDs show up as a second video and audio the player never plays.
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-of", "json", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	var found struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &found); err != nil || len(found.Streams) != 2 {
		t.Errorf("the viewer got streams %+v, want one video and one audio", found.Streams)
	}
	size := int64(len(got) / tsPacket * tsPacket)
	for _, stream := range []string{"v:0", "a:0"} {
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", stream, "-show_entries", "packet=dts_time,pos",
			"-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var prev float64
		var lastPos int64
		n := 0
		for _, line := range strings.Fields(string(out)) {
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
		// Spliced raw, the playing track just ends at the switch and the rest is on PIDs it never reads.
		if lastPos < size*9/10 {
			t.Errorf("%s stops at byte %d of %d, after %d packets: the new source never reached it", stream, lastPos, size, n)
		}
	}
}

func TestOpeningBurstIsHeldBackNotDropped(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("no ffprobe")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "burst.ts")
	if out, err := exec.Command(bin, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25", "-t", "20",
		"-vf", "noise=alls=60:allf=t", "-c:v", "libx264", "-g", "50", "-preset", "ultrafast", "-b:v", "8000k", "-f", "mpegts", path).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot make a test source: %v %s", err, out)
	}
	media, _ := os.ReadFile(path)
	if len(media) < remuxBacklog*chunkSize*3/2 {
		t.Fatalf("the source is %d bytes, too small to overflow the queue", len(media))
	}
	engine := newSourceEngine(map[string]int{slowSource: 4000})
	engine.media = map[string][]byte{slowSource: media}
	engine.burst = len(media)
	srv, h := newSwitchingProxy(t, engine, slowSource)
	// An ffmpeg that takes a second to start reading, as one probing its input on a Pi does.
	slow := filepath.Join(dir, "slow-ffmpeg")
	os.WriteFile(slow, []byte("#!/bin/sh\nsleep 1\nexec "+bin+" \"$@\"\n"), 0o755)
	h.ffmpeg = slow

	frames := func(b []byte) int {
		p := filepath.Join(dir, "probe.ts")
		os.WriteFile(p, b, 0o644)
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-count_packets",
			"-show_entries", "stream=nb_read_packets", "-of", "csv=p=0", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		// The count comes twice, once under the program and once on its own.
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			t.Fatal("ffprobe counted no video")
		}
		n, err := strconv.Atoi(strings.TrimSuffix(fields[0], ","))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	want := frames(media)
	if want < 400 {
		t.Fatalf("the source has %d frames, want 20 s of them", want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/channel/dazn-laliga-1080p", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	// ffmpeg keeps its last few packets until more input comes, and none does.
	if n := frames(got[:len(got)/tsPacket*tsPacket]); n < want-25 {
		t.Errorf("the viewer got %d of the source's %d frames", n, want)
	}
}
