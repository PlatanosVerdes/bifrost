package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func orphanStream(label string) *stream {
	return &stream{label: label, viewers: map[chan []byte]struct{}{make(chan []byte): {}}}
}

func TestAChannelNobodyPlaysIsLetGoOnlyAfterTheWholeWindow(t *testing.T) {
	o := &orphans{after: 15 * time.Minute, unseen: map[*stream]time.Time{}}
	s := orphanStream("M+ Liga de Campeones 1080p")
	start := time.Date(2026, 10, 20, 23, 10, 0, 0, time.UTC)
	nobody := []jellyfinPlayback{}

	for minute := 0; minute < 15; minute++ {
		if got := o.pass(start.Add(time.Duration(minute)*time.Minute), []*stream{s}, nobody); len(got) != 0 {
			t.Fatalf("let go after %d minutes", minute)
		}
	}
	if got := o.pass(start.Add(15*time.Minute), []*stream{s}, nobody); len(got) != 1 {
		t.Fatalf("not let go after 15 minutes unplayed")
	}
}

func TestAPlayedChannelIsNeverLetGo(t *testing.T) {
	o := &orphans{after: time.Minute, unseen: map[*stream]time.Time{}}
	s := orphanStream("M+ Liga de Campeones 1080p")
	playing := []jellyfinPlayback{{Channel: "M+ Liga de Campeones 1080p"}}
	now := time.Now()
	for minute := 0; minute < 60; minute++ {
		if got := o.pass(now.Add(time.Duration(minute)*time.Minute), []*stream{s}, playing); len(got) != 0 {
			t.Fatal("a channel Jellyfin plays was let go")
		}
	}
}

// One sighting resets the window: a TV that was off for a while and came back is a viewer again.
func TestASightingStartsTheWindowAgain(t *testing.T) {
	o := &orphans{after: 15 * time.Minute, unseen: map[*stream]time.Time{}}
	s := orphanStream("DAZN 1 1080p")
	now := time.Now()
	o.pass(now, []*stream{s}, []jellyfinPlayback{})
	o.pass(now.Add(10*time.Minute), []*stream{s}, []jellyfinPlayback{{Channel: "DAZN 1 1080p"}})
	if got := o.pass(now.Add(20*time.Minute), []*stream{s}, []jellyfinPlayback{}); len(got) != 0 {
		t.Fatal("the window did not restart after Jellyfin played it again")
	}
	if got := o.pass(now.Add(35*time.Minute), []*stream{s}, []jellyfinPlayback{}); len(got) != 1 {
		t.Fatal("not let go 15 minutes after the last sighting")
	}
}

func TestNoAnswerFromJellyfinClosesNothing(t *testing.T) {
	o := &orphans{after: time.Minute, unseen: map[*stream]time.Time{}}
	s := orphanStream("DAZN 1 1080p")
	now := time.Now()
	for minute := 0; minute < 30; minute++ {
		if got := o.pass(now.Add(time.Duration(minute)*time.Minute), []*stream{s}, nil); len(got) != 0 {
			t.Fatal("let go while Jellyfin could not be asked")
		}
	}
}

func TestAViewerThatIsNotJellyfinKeepsTheChannel(t *testing.T) {
	o := &orphans{after: time.Minute, unseen: map[*stream]time.Time{}}
	s := orphanStream("DAZN 1 1080p")
	viewer := make(chan []byte)
	direct := httptest.NewRequest("GET", "/channel/dazn-1-1080p", nil)
	direct.Header.Set("User-Agent", "VLC/3.0.21 LibVLC/3.0.21")
	s.noteDirect(viewer, direct)
	now := time.Now()
	for minute := 0; minute < 30; minute++ {
		if got := o.pass(now.Add(time.Duration(minute)*time.Minute), []*stream{s}, []jellyfinPlayback{}); len(got) != 0 {
			t.Fatal("let go a channel a non-Jellyfin player is on")
		}
	}
}

func TestOnlyJellyfinRequestsCountAsJellyfin(t *testing.T) {
	for ua, want := range map[string]bool{
		"Jellyfin-Server/10.11.11": true,
		"VLC/3.0.21 LibVLC/3.0.21": false,
		"curl/8.5.0":               false,
		"":                         false,
	} {
		r := httptest.NewRequest("GET", "/channel/x", nil)
		r.Header.Set("User-Agent", ua)
		if got := fromJellyfin(r); got != want {
			t.Errorf("%q: %v, want %v", ua, got, want)
		}
	}
}
