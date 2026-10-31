// Package mpegts reads and rewrites MPEG-TS packets: sync, PES timestamps, PCR and PSI sections.
package mpegts

const (
	PacketSize = 188
	SyncByte   = 0x47
	Clock      = 1 << 33 // PTS, DTS and the PCR base wrap at 33 bits of 90 kHz
	// How far into a stream the TS sync is searched for.
	tsSearch = 16 * 1024
)

// Hands out whole 188-byte TS packets so a switch never splices mid-packet. A stream with no sync in
// its first tsSearch bytes is passed through.
type Aligner struct {
	DropLead bool // a spliced-in source starts at its first sync; the opening one loses nothing
	synced   bool
	raw      bool
	carry    []byte
}

func (a *Aligner) Push(b []byte) []byte {
	if a.raw {
		return b
	}
	data := make([]byte, 0, len(a.carry)+len(b))
	data = append(append(data, a.carry...), b...)
	a.carry = nil
	if !a.synced {
		at := SyncAt(data)
		switch {
		case at >= 0:
			a.synced = true
			if a.DropLead {
				data = data[at:]
			} else if at > 0 {
				// Bytes before the first packet go out as they are, and alignment starts after them.
				lead := data[:at]
				rest := a.Push(data[at:])
				return append(lead[:len(lead):len(lead)], rest...)
			}
		case len(data) >= tsSearch:
			a.raw = true
			return data
		default:
			a.carry = data
			return nil
		}
	}
	return a.whole(data)
}

// whole hands out the packets in data and carries the rest. The engine resends stretches that start
// mid-packet, so a packet counts only when the next one starts with the sync byte too, and a broken
// one drops everything up to the next sync. The last packet waits for the next chunk to vouch for it.
func (a *Aligner) whole(data []byte) []byte {
	var out []byte
	run, i := 0, 0 // data[run:i] is a run of whole packets
	for i+PacketSize < len(data) {
		if data[i] == SyncByte && data[i+PacketSize] == SyncByte {
			i += PacketSize
			continue
		}
		out = append(out, data[run:i]...)
		at := SyncAt(data[i+1:])
		if at < 0 {
			// Not enough bytes yet to find three syncs in a row; the tail waits for the next chunk.
			a.carry = append([]byte(nil), data[max(i+1, len(data)-tsSearch):]...)
			return out
		}
		i += 1 + at
		run = i
	}
	if i < len(data) {
		a.carry = append([]byte(nil), data[i:]...)
	}
	if out == nil {
		return data[run:i]
	}
	return append(out, data[run:i]...)
}

// SyncAt is the first offset where three packets in a row start with the sync byte.
func SyncAt(b []byte) int {
	for i := 0; i+2*PacketSize < len(b); i++ {
		if b[i] == SyncByte && b[i+PacketSize] == SyncByte && b[i+2*PacketSize] == SyncByte {
			return i
		}
	}
	return -1
}

func FirstPTS(b []byte) map[uint16]int64 {
	first := map[uint16]int64{}
	for i := 0; i+PacketSize <= len(b); i += PacketSize {
		pid, pts, _, ok := PESTimes(b[i : i+PacketSize])
		if !ok {
			continue
		}
		if f, seen := first[pid]; !seen || WrapDiff(pts, f) < 0 {
			first[pid] = pts
		}
	}
	return first
}

func FirstTimestamps(b []byte) map[uint16]int64 {
	first := map[uint16]int64{}
	for i := 0; i+PacketSize <= len(b); i += PacketSize {
		pid, ts, ok := PESTimestamp(b[i : i+PacketSize])
		if !ok {
			continue
		}
		if f, seen := first[pid]; !seen || WrapDiff(ts, f) < 0 {
			first[pid] = ts
		}
	}
	return first
}

func PSISection(payload []byte) []byte {
	if len(payload) < 1 || 1+int(payload[0])+3 > len(payload) {
		return nil
	}
	sec := payload[1+int(payload[0]):]
	n := 3 + int(sec[1]&0x0f)<<8 + int(sec[2])
	if n < 12 || n > len(sec) {
		return nil
	}
	return sec[:n]
}

func CRC32(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func PESTimestamp(p []byte) (uint16, int64, bool) {
	pid, _, dts, ok := PESTimes(p)
	return pid, dts, ok
}

func PESTimes(p []byte) (pid uint16, pts, dts int64, ok bool) {
	if p[0] != SyncByte || p[1]&0x40 == 0 {
		return 0, 0, 0, false
	}
	pid = uint16(p[1]&0x1f)<<8 | uint16(p[2])
	afc := p[3] >> 4 & 3
	payload := 4
	if afc&2 != 0 {
		payload = 5 + int(p[4])
	}
	if afc&1 == 0 || payload+19 > PacketSize {
		return 0, 0, 0, false
	}
	pes := p[payload:]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 {
		return 0, 0, 0, false
	}
	switch pes[7] >> 6 {
	case 2:
		pts = ReadTS(pes[9:14])
		return pid, pts, pts, true
	case 3:
		return pid, ReadTS(pes[9:14]), ReadTS(pes[14:19]), true
	}
	return 0, 0, 0, false
}

func ReadTS(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

func WriteTS(b []byte, ts int64) {
	b[0] = b[0]&0xf0 | byte(ts>>29)&0x0e | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14)&0xfe | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1)&0xfe | 1
}

// The 27 MHz extension is left as it is.
func ReadPCR(b []byte) int64 {
	return int64(b[0])<<25 | int64(b[1])<<17 | int64(b[2])<<9 | int64(b[3])<<1 | int64(b[4]>>7)
}

func WritePCR(b []byte, base int64) {
	b[0] = byte(base >> 25)
	b[1] = byte(base >> 17)
	b[2] = byte(base >> 9)
	b[3] = byte(base >> 1)
	b[4] = b[4]&0x7f | byte(base&1)<<7
}

func Wrap(ts int64) int64 {
	return (ts%Clock + Clock) % Clock
}

func WrapDiff(a, b int64) int64 {
	return Wrap(a-b+Clock/2) - Clock/2
}

const (
	// A step back this small is reordering, not a replay.
	rewindSlack = 90000
	// A step back further than this is a source restarting its clock, taken as the new start.
	rewindMax = 60 * 90000
)

// Forward drops what a source plays again. One sent a 25 s stretch, went back and sent it again,
// over and over with the same timestamps; the remux renumbered it and the TV showed it twice. Each
// PID's packets are dropped from a PES that goes back until one passes the furthest already sent.
type Forward struct {
	pids map[uint16]*forwardPID
}

type forwardPID struct {
	high     int64
	dropping bool
}

// Push takes whole packets and says how far the source went back when a replay starts.
func (f *Forward) Push(b []byte) (out []byte, back int64) {
	if len(b)%PacketSize != 0 {
		return b, 0
	}
	if f.pids == nil {
		f.pids = map[uint16]*forwardPID{}
	}
	out = b[:0:0]
	kept := 0 // b[kept:i] is a run of packets to keep, appended in one go
	for i := 0; i < len(b); i += PacketSize {
		p := b[i : i+PacketSize]
		drop, went := f.packet(p)
		back = max(back, went)
		if drop {
			out = append(out, b[kept:i]...)
			kept = i + PacketSize
		}
	}
	if kept == 0 {
		return b, back
	}
	return append(out, b[kept:]...), back
}

func (f *Forward) packet(p []byte) (drop bool, back int64) {
	if p[0] != SyncByte {
		return false, 0
	}
	pid, _, dts, ok := PESTimes(p)
	if !ok {
		st := f.pids[uint16(p[1]&0x1f)<<8|uint16(p[2])]
		return st != nil && st.dropping, 0
	}
	st := f.pids[pid]
	if st == nil {
		f.pids[pid] = &forwardPID{high: dts}
		return false, 0
	}
	switch d := WrapDiff(dts, st.high); {
	case d > 0:
		st.high, st.dropping = dts, false
		return false, 0
	case -d > rewindMax:
		st.high, st.dropping = dts, false
		return false, 0
	case st.dropping:
		return true, 0
	case -d > rewindSlack:
		// Every PID goes back at once: only the first one to do so reports it.
		for _, other := range f.pids {
			if other.dropping {
				st.dropping = true
				return true, 0
			}
		}
		st.dropping = true
		return true, -d
	}
	return false, 0
}
