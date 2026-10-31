package slate

import (
	"testing"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/mpegts"
)

func TestEmbeddedSlatesLoop(t *testing.T) {
	for name, ts := range map[string][]byte{"off-air": OffAir, "loading": Loading} {
		sl, err := New(ts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sl.duration < 23900*time.Millisecond || sl.duration > 24100*time.Millisecond {
			t.Errorf("%s lasts %s, want 24 s", name, sl.duration)
		}
		if got := len(mpegts.FirstTimestamps(sl.loop[:sl.head])); got != 2 {
			t.Errorf("%s head has timestamps for %d PIDs, want video and audio", name, got)
		}
	}
}

func TestSlatePlayerKeepsRealTime(t *testing.T) {
	sl, err := New(OffAir)
	if err != nil {
		t.Fatal(err)
	}
	p := &Player{Slate: sl, Lead: Lead}
	t0 := time.Unix(1000, 0)
	first, fresh := p.Next(t0)
	rate := float64(len(sl.loop)) / sl.duration.Seconds()
	if !fresh || float64(len(first)) < rate*Lead.Seconds()*0.9 {
		t.Errorf("the first chunk is %d bytes, fresh %t; want about %s of the loop at once", len(first), fresh, Lead)
	}
	total := int64(len(first))
	starts := 0
	for now := t0.Add(Tick); now.Before(t0.Add(3 * sl.duration)); now = now.Add(Tick) {
		b, fresh := p.Next(now)
		if fresh {
			starts++
		}
		total += int64(len(b))
	}
	// The lead puts the player two seconds ahead, so the third run starts within three loops' time.
	if starts != 3 {
		t.Errorf("%d new runs in three loops' time, want 3 after the first", starts)
	}
	if want := rate * (3*sl.duration + Lead).Seconds(); float64(total) > want*1.02 || float64(total) < want*0.95 {
		t.Errorf("sent %d bytes in three loops, want about %.0f: real time", total, want)
	}
}
