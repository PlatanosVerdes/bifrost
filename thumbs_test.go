package main

import (
	"testing"
	"time"
)

func TestThumbGrabWaitsForTheSourceToSettle(t *testing.T) {
	th := &thumbs{taken: map[string]time.Time{}, tried: map[string]time.Time{}}
	g := th.grab(idA)
	g.push(make([]byte, 1000))
	if len(g.buf) != 0 {
		t.Fatal("collected bytes before the source played thumbAfter")
	}

	g.from = time.Now().Add(-thumbAfter)
	g.push(make([]byte, 1000))
	if len(g.buf) != 1000 {
		t.Fatalf("collected %d bytes once settled, want 1000", len(g.buf))
	}

	fresh := th.grab(idB)
	fresh.from = time.Now().Add(-thumbAfter)
	th.taken[idB] = time.Now()
	fresh.push(make([]byte, 1000))
	if len(fresh.buf) != 0 {
		t.Error("collected bytes for a source whose thumbnail is recent")
	}
}

func TestNoThumbsWithoutFFmpeg(t *testing.T) {
	th := newThumbs(t.TempDir(), "")
	th.grab(idA).push(make([]byte, 10))
	if !th.at(idA).IsZero() {
		t.Error("a thumbnail time without ffmpeg")
	}
}
