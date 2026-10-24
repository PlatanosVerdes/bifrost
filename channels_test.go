package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `#EXTM3U url-tvg="https://example.com/guide.xml" refresh="3600"
#EXTINF:-1 tvg-id="DAZN LaLiga HD" tvg-logo="https://x/l.png" group-title="LA LIGA",DAZN LaLiga 1080p *
acestream://cc108ae39f92c48f6c946763047bd1c9b7b7d889
#EXTINF:-1 tvg-id="DAZN LaLiga HD" tvg-logo="https://x/l.png" group-title="LA LIGA",DAZN LaLiga 1080p **
acestream://7f9857c45764ef2c7cdfb619b89db3056a57a51f
#EXTINF:-1 tvg-id="M+ LaLiga HD" tvg-logo="https://x/m.png" group-title="LA LIGA",M+ LaLiga 720p *
acestream://3ace1463a7cad979c3a4002b245487e9982e9f1a
#EXTINF:-1 tvg-id="DAZN LaLiga HD" tvg-logo="https://x/l.png" group-title="LA LIGA",DAZN LaLiga 1080p **
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

	dazn := groups["dazn-laliga-1080p"]
	if dazn == nil || len(dazn.IDs) != 3 || dazn.IDs[0] != "cc108ae39f92c48f6c946763047bd1c9b7b7d889" {
		t.Fatalf("dazn group = %+v, want its three ids in list order", dazn)
	}
	if groups["m-plus-laliga-720p"] == nil {
		t.Errorf("groups = %v, want M+ kept apart as m-plus-laliga-720p", groups)
	}
	if !strings.Contains(m3u, `group-title="LA LIGA",DAZN LaLiga 1080p`+"\nhttp://bifrost:6879/channel/dazn-laliga-1080p\n") {
		t.Errorf("m3u lost the tags or the stable URL:\n%s", m3u)
	}
	if strings.Count(m3u, "#EXTINF") != 2 {
		t.Errorf("m3u has %d entries, want one per channel:\n%s", strings.Count(m3u, "#EXTINF"), m3u)
	}
	if !strings.HasPrefix(m3u, `#EXTM3U url-tvg="https://example.com/guide.xml"`) {
		t.Errorf("guide header lost: %q", strings.SplitN(m3u, "\n", 2)[0])
	}
}

// New ids for the same channels must leave the playlist byte for byte.
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
	if groups["dazn-laliga-1080p"].IDs[0] != "1111111111111111111111111111111111111111" {
		t.Errorf("table kept the old id: %v", groups["dazn-laliga-1080p"].IDs)
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
	_, ids := cat.lookup("dazn-laliga-1080p")
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
	h.streams["channel:m-plus-laliga-720p"] = &stream{viewers: map[chan []byte]struct{}{}}
	l := &lister{catalog: cat, hub: h}

	without := strings.Replace(sample, "acestream://3ace1463a7cad979c3a4002b245487e9982e9f1a\n", "acestream://\n", 1)
	sources, _ = parseAndDedup([]byte(without))
	order, groups := groupChannels(sources)
	order = l.keepWatched(order, groups)
	if groups["m-plus-laliga-720p"] == nil || order[len(order)-1] != "m-plus-laliga-720p" {
		t.Errorf("order %v: the watched channel was dropped", order)
	}
}

const fullGuide = `<?xml version="1.0" encoding="UTF-8"?>
<tv generator-info-name="test">
  <channel id="DAZN LaLiga HD"><display-name>DAZN LaLiga</display-name></channel>
  <channel id="Some Other"><display-name>Other</display-name></channel>
  <programme start="20261018200000 +0200" stop="20261018220000 +0200" channel="DAZN LaLiga HD"><title>Betis - Sevilla</title></programme>
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
	if !sameIDs(ids, []string{"DAZN LaLiga HD", "M+ LaLiga HD"}) {
		t.Fatalf("tvg ids = %v", ids)
	}
	if changed, err := trimGuide(full, out, ids); err != nil || !changed {
		t.Fatalf("trim: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "Betis - Sevilla") || strings.Contains(string(got), "Not ours") ||
		strings.Contains(string(got), `id="Some Other"`) {
		t.Errorf("trimmed guide:\n%s", got)
	}
	if changed, _ := trimGuide(full, out, ids); changed {
		t.Error("an identical guide was rewritten")
	}
}
