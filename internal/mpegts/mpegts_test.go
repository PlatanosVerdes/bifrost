package mpegts

import (
	"bytes"
	"testing"
)

// pes is one TS packet starting a PES on pid with dts, filled with fill.
func pes(pid uint16, dts int64, fill byte) []byte {
	p := bytes.Repeat([]byte{fill}, PacketSize)
	p[0], p[1], p[2], p[3] = SyncByte, 0x40|byte(pid>>8), byte(pid), 0x10
	copy(p[4:], []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10})
	p[13], p[18] = 0x31, 0x11
	WriteTS(p[13:18], dts)
	WriteTS(p[18:23], dts)
	return p
}

// more continues the PES on pid.
func more(pid uint16, fill byte) []byte {
	p := bytes.Repeat([]byte{fill}, PacketSize)
	p[0], p[1], p[2], p[3] = SyncByte, byte(pid>>8), byte(pid), 0x10
	return p
}

func TestAlignerResyncsAfterAStretchThatStartsMidPacket(t *testing.T) {
	var a Aligner
	in := append(append(append([]byte(nil), pes(256, 0, 'a')...), more(256, 'a')...), pes(256, 0, 'b')[:100]...)
	in = append(in, append(append(pes(256, 3600, 'c'), more(256, 'c')...), more(256, 'c')...)...)
	got := a.Push(in)
	// The last packet waits for the next chunk to show it is whole.
	if len(got) != 4*PacketSize {
		t.Fatalf("got %d bytes, want 4 whole packets", len(got))
	}
	if got := a.Push(pes(256, 7200, 'd')); len(got) != PacketSize || got[PacketSize-1] != 'c' {
		t.Fatalf("the held packet did not follow: %d bytes", len(got))
	}
	for i := 0; i < len(got); i += PacketSize {
		if got[i] != SyncByte {
			t.Fatalf("packet %d is not aligned", i/PacketSize)
		}
	}
	if bytes.Contains(got, bytes.Repeat([]byte{'b'}, 50)) {
		t.Error("the half packet went out")
	}
}

func TestForwardDropsWhatTheSourcePlaysAgain(t *testing.T) {
	var f Forward
	second := int64(90000)
	var in []byte
	for s := int64(0); s < 30; s++ {
		in = append(append(in, pes(256, s*second, 'v')...), more(256, 'v')...)
		in = append(in, pes(257, s*second+10, 'a')...)
	}
	// The source goes back 25 s and sends it all again, then carries on.
	for s := int64(5); s < 40; s++ {
		in = append(append(in, pes(256, s*second, 'v')...), more(256, 'v')...)
		in = append(in, pes(257, s*second+10, 'a')...)
	}
	out, back := f.Push(in)
	if back != 25*second-second {
		t.Errorf("reported going back %d, want 24 s", back/second)
	}
	if want := 40 * 3 * PacketSize; len(out) != want {
		t.Fatalf("kept %d packets, want 120 (each second once)", len(out)/PacketSize)
	}
	last := map[uint16]int64{}
	for i := 0; i < len(out); i += PacketSize {
		if pid, _, dts, ok := PESTimes(out[i : i+PacketSize]); ok {
			if prev, seen := last[pid]; seen && dts <= prev {
				t.Fatalf("pid %d went from %d to %d", pid, prev, dts)
			}
			last[pid] = dts
		}
	}
}

func TestForwardTakesAClockResetAsTheNewStart(t *testing.T) {
	var f Forward
	in := append(pes(256, 10*60*90000, 'v'), pes(256, 90000, 'v')...)
	in = append(in, pes(256, 2*90000, 'v')...)
	if out, back := f.Push(in); len(out) != len(in) || back != 0 {
		t.Errorf("a clock ten minutes back was dropped: kept %d of %d, back %d", len(out), len(in), back)
	}
}

func TestForwardLetsSmallStepsBackThrough(t *testing.T) {
	var f Forward
	in := append(append(pes(256, 90000, 'v'), pes(256, 90000-3600, 'v')...), pes(256, 90000+3600, 'v')...)
	if out, _ := f.Push(in); len(out) != len(in) {
		t.Error("a frame 40 ms back was taken for a replay")
	}
}
