package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func entry(name, tvg string, ids ...string) *channelGroup {
	return &channelGroup{Name: name, IDs: ids, extinf: `#EXTINF:-1 tvg-id="` + tvg + `" group-title="X",` + name}
}

func TestLowerListsOnlyTheSameChannelsLowerVersions(t *testing.T) {
	c := &catalog{}
	c.set([]string{"h1080", "h720", "h2", "h3", "hplain"}, map[string]*channelGroup{
		"h1080":  entry("Channel E 1080p", "Hyper HD", "a"),
		"h720":   entry("Channel E 720p", "Hyper HD", "b"),
		"h2":     entry("Channel E 2 720p", "Hyper 2", "c"),
		"h3":     entry("Channel E 3 720p", "Hyper HD", "d"),
		"hplain": entry("Channel E", "Hyper HD", "e"),
	})
	if got := c.lower("h1080"); !slices.Equal(got, []string{"b"}) {
		t.Errorf("lower of the 1080p is %v, want the 720p's source only", got)
	}
	if got := c.lower("h720"); len(got) != 0 {
		t.Errorf("lower of the 720p is %v, want nothing below it", got)
	}
	if h := c.heightOf("a"); h != 1080 {
		t.Errorf("height of a 1080p source is %d", h)
	}
}

// A starving 1080p channel steps down to its 720p version's source, tells the TV, and steps back
// up once its own source keeps up clearly.
func TestStarvingChannelStepsDownAndBackUp(t *testing.T) {
	engine := newSourceEngine(map[string]int{slowSource: 100, fastSource: 4000})
	srv, h := newSwitchingProxy(t, engine, slowSource)
	h.table.set([]string{"channel-a-1080p", "channel-a-720p"}, map[string]*channelGroup{
		"channel-a-1080p": entry("Channel A 1080p", "Channel A HD", slowSource),
		"channel-a-720p":  entry("Channel A 720p", "Channel A HD", fastSource),
	})
	h.ladder = true
	h.policy.cooldown = 100 * time.Millisecond
	after := stepUpAfter
	stepUpAfter = 300 * time.Millisecond
	t.Cleanup(func() { stepUpAfter = after })

	var mu sync.Mutex
	var notices []string
	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Sessions":
			w.Write([]byte(`[{"Id":"tv","NowPlayingItem":{"Name":"Channel A 1080p","Type":"TvChannel"}}]`))
		case r.URL.Path == "/Sessions/tv/Message":
			var m struct{ Text string }
			json.NewDecoder(r.Body).Decode(&m)
			mu.Lock()
			notices = append(notices, m.Text)
			mu.Unlock()
		}
	}))
	t.Cleanup(jellyfin.Close)
	h.jellyfinURL, h.jellyfinKey = jellyfin.URL, "key"

	go func() {
		for engine.count(engine.live, fastSource) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		engine.mu.Lock()
		engine.down[slowSource] = 4000
		engine.mu.Unlock()
	}()
	markers := watch(t, srv.URL+"/channel/channel-a-1080p", 8*time.Second,
		func(m string) bool { return strings.HasSuffix(m, "sfs") })
	if markers != "sfs" {
		t.Fatalf("played %q, want its own source, the 720p's, then its own again", markers)
	}
	waitFor(t, "both notices", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notices) == 2
	})
	want := []string{"Switched to 720p: the 1080p source was too slow", "Back to 1080p"}
	if !slices.Equal(notices, want) {
		t.Errorf("the TV was told %q, want %q", notices, want)
	}
}
