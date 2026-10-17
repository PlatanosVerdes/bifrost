package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `#EXTM3U url-tvg="https://example.com/guide.xml" refresh="3600"
#EXTINF:-1 tvg-id="Channel A HD" tvg-logo="https://x/l.png" group-title="GROUP A",Channel A 1080p *
acestream://cc108ae39f92c48f6c946763047bd1c9b7b7d889
#EXTINF:-1 tvg-id="Channel A HD" tvg-logo="https://x/l.png" group-title="GROUP A",Channel A 1080p **
acestream://7f9857c45764ef2c7cdfb619b89db3056a57a51f
#EXTINF:-1 tvg-id="Channel+ D HD" tvg-logo="https://x/m.png" group-title="GROUP A",Channel+ D 720p *
acestream://3ace1463a7cad979c3a4002b245487e9982e9f1a
#EXTINF:-1 tvg-id="Channel A HD" tvg-logo="https://x/l.png" group-title="GROUP A",Channel A 1080p **
acestream://9c588ac92554746c489bc553773d959f6d2dfd8e
`

func build(t *testing.T, list string) (string, map[string]*channelGroup) {
	t.Helper()
	sources, header := parseAndDedup([]byte(list))
	order, groups := groupChannels(sources)
	return stableM3U(header, "http://bifrost:6879/channel/", order, groups), groups
}

func TestSourcesFoldIntoChannels(t *testing.T) {
	m3u, groups := build(t, sample)

	first := groups["channel-a-1080p"]
	if first == nil || len(first.IDs) != 3 || first.IDs[0] != "cc108ae39f92c48f6c946763047bd1c9b7b7d889" {
		t.Fatalf("first group = %+v, want its three ids in list order", first)
	}
	if groups["channel-plus-d-720p"] == nil {
		t.Errorf("groups = %v, want Channel+ kept apart as channel-plus-d-720p", groups)
	}
	if !strings.Contains(m3u, `group-title="GROUP A",Channel A 1080p`+"\nhttp://bifrost:6879/channel/channel-a-1080p\n") {
		t.Errorf("m3u lost the tags or the stable URL:\n%s", m3u)
	}
	if strings.Count(m3u, "#EXTINF") != 2 {
		t.Errorf("m3u has %d entries, want one per channel:\n%s", strings.Count(m3u, "#EXTINF"), m3u)
	}
	if !strings.HasPrefix(m3u, `#EXTM3U url-tvg="https://example.com/guide.xml"`) {
		t.Errorf("guide header lost: %q", strings.SplitN(m3u, "\n", 2)[0])
	}
}

// The whole point: new ids for the same channels must leave Jellyfin's playlist byte for byte.
func TestRotatedIDsLeaveThePlaylistAlone(t *testing.T) {
	before, _ := build(t, sample)
	rotated := strings.NewReplacer(
		"cc108ae39f92c48f6c946763047bd1c9b7b7d889", "1111111111111111111111111111111111111111",
		"7f9857c45764ef2c7cdfb619b89db3056a57a51f", "2222222222222222222222222222222222222222",
	).Replace(sample)
	after, groups := build(t, rotated)
	if before != after {
		t.Errorf("playlist changed on a rotation:\n%s\n---\n%s", before, after)
	}
	if groups["channel-a-1080p"].IDs[0] != "1111111111111111111111111111111111111111" {
		t.Errorf("table kept the old id: %v", groups["channel-a-1080p"].IDs)
	}
}

// A dropped id must vanish from the catalog on the next pass, not linger.
func TestCatalogIsReplacedWhole(t *testing.T) {
	cat := &catalog{}
	_, groups := build(t, sample)
	order, _ := groupChannels(func() []channel { s, _ := parseAndDedup([]byte(sample)); return s }())
	cat.set(order, groups)

	trimmed := strings.Replace(sample, "acestream://9c588ac92554746c489bc553773d959f6d2dfd8e\n", "acestream://\n", 1)
	sources, _ := parseAndDedup([]byte(trimmed))
	cat.set(groupChannels(sources))
	_, ids := cat.lookup("channel-a-1080p")
	for _, id := range ids {
		if id == "9c588ac92554746c489bc553773d959f6d2dfd8e" {
			t.Error("the dropped id is still in the catalog")
		}
	}
	if cat.nameOf("9c588ac92554746c489bc553773d959f6d2dfd8e") != "" {
		t.Error("the dropped id still has a name")
	}
}

// A channel being watched stays in Jellyfin's playlist when the list skips it for a pass.
func TestWatchedChannelIsKeptWhenTheListDropsIt(t *testing.T) {
	cat := &catalog{}
	sources, _ := parseAndDedup([]byte(sample))
	cat.set(groupChannels(sources))

	h := newHub("http://unused", time.Second, time.Second)
	h.streams["channel:channel-plus-d-720p"] = &stream{viewers: map[chan []byte]struct{}{}}
	l := &lister{catalog: cat, hub: h}

	without := strings.Replace(sample, "acestream://3ace1463a7cad979c3a4002b245487e9982e9f1a\n", "acestream://\n", 1)
	sources, _ = parseAndDedup([]byte(without))
	order, groups := groupChannels(sources)
	order = l.keepWatched(order, groups)
	if groups["channel-plus-d-720p"] == nil || order[len(order)-1] != "channel-plus-d-720p" {
		t.Errorf("order %v: the watched channel was dropped", order)
	}
}

const fullGuide = `<?xml version="1.0" encoding="UTF-8"?>
<tv generator-info-name="test">
  <channel id="Channel A HD"><display-name>Channel A</display-name></channel>
  <channel id="Some Other"><display-name>Other</display-name></channel>
  <programme start="20261018200000 +0200" stop="20261018220000 +0200" channel="Channel A HD"><title>Evening News</title></programme>
  <programme start="20261018200000 +0200" stop="20261018220000 +0200" channel="Some Other"><title>Not ours</title></programme>
</tv>`

func TestGuideKeepsOnlyOurChannels(t *testing.T) {
	dir := t.TempDir()
	full, out := filepath.Join(dir, "full.xml"), filepath.Join(dir, "guide.xml")
	if err := os.WriteFile(full, []byte(fullGuide), 0644); err != nil {
		t.Fatal(err)
	}
	_, groups := build(t, sample)
	ids := tvgIDs(groups)
	if !sameIDs(ids, []string{"Channel A HD", "Channel+ D HD"}) {
		t.Fatalf("tvg ids = %v", ids)
	}
	if changed, err := trimGuide(full, out, ids); err != nil || !changed {
		t.Fatalf("trim: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "Evening News") || strings.Contains(string(got), "Not ours") ||
		strings.Contains(string(got), `id="Some Other"`) {
		t.Errorf("trimmed guide:\n%s", got)
	}
	if changed, _ := trimGuide(full, out, ids); changed {
		t.Error("an identical guide was rewritten")
	}
}
