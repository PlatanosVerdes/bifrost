package catalog

import (
	"slices"
	"testing"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestLadderSkipsBlockedSources(t *testing.T) {
	c := &Catalog{Blocked: LoadBlocklist(t.TempDir())}
	c.Set([]string{"x-1080p", "x-720p"}, map[string]*Group{
		"x-1080p": {Name: "X 1080p", IDs: []string{idA}, Extinf: `#EXTINF:-1 tvg-id="X",X 1080p`},
		"x-720p":  {Name: "X 720p", IDs: []string{idA, idB}, Extinf: `#EXTINF:-1 tvg-id="X",X 720p`},
	})
	c.Blocked.Mark("x-720p", idA, true)
	if got := c.Lower("x-1080p"); !slices.Equal(got, []string{idB}) {
		t.Errorf("lower = %v, want only %s", got, idB)
	}
}
