package main

import (
	"net/http"
	"net/http/httptest"
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
	if changed, err := trimGuide([]string{full}, out, ids, nil); err != nil || !changed {
		t.Fatalf("trim: changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(out)
	if !strings.Contains(string(got), "Evening News") || strings.Contains(string(got), "Not ours") ||
		strings.Contains(string(got), `id="Some Other"`) {
		t.Errorf("trimmed guide:\n%s", got)
	}
	if changed, _ := trimGuide([]string{full}, out, ids, nil); changed {
		t.Error("an identical guide was rewritten")
	}
}

func TestSourceListsParse(t *testing.T) {
	one, err := parseSourceLists("http://a/list.m3u, http://b/list.m3u")
	if err != nil || len(one) != 1 || len(one[0]) != 2 {
		t.Errorf("comma routes = %v, %v; want one list with two routes", one, err)
	}
	two, err := parseSourceLists(`[{"url": "http://a/list.m3u", "backup": ["http://b/list.m3u"]}, {"url": "http://c/other.m3u"}]`)
	if err != nil || len(two) != 2 || len(two[0]) != 2 || two[1][0] != "http://c/other.m3u" {
		t.Errorf("JSON = %v, %v; want two lists, the first with its backup", two, err)
	}
	if _, err := parseSourceLists(`[{"backup": ["http://b"]}]`); err == nil {
		t.Error("a list without a url was accepted")
	}
}

const otherList = `#EXTM3U
#EXTINF:-1 tvg-id="Channel A HD" group-title="Elsewhere",Chan A FHD 1080p
acestream://aaaa000000000000000000000000000000000001
#EXTINF:-1 tvg-id="Channel A HD" group-title="Elsewhere",Channel A 1080p
acestream://cc108ae39f92c48f6c946763047bd1c9b7b7d889
#EXTINF:-1 group-title="Elsewhere",Channel+ D 720p
acestream://aaaa000000000000000000000000000000000003
#EXTINF:-1 tvg-id="Shared HD" group-title="Elsewhere",Channel Z 1080p
acestream://aaaa000000000000000000000000000000000002
`

func TestListsJoinOnNameOrGuideID(t *testing.T) {
	sources, header := mergeLists([][]byte{[]byte(sample), []byte(otherList)})
	order, groups := groupChannels(sources)
	a := groups["channel-a-1080p"]
	if a == nil || len(a.IDs) != 4 || a.IDs[3] != "aaaa000000000000000000000000000000000001" {
		t.Fatalf("channel a = %+v, want its three ids, then the other list's under another name, the repeated id once", a)
	}
	if m := groups["channel-plus-d-720p"]; m == nil || len(m.IDs) != 2 {
		t.Errorf("channel m = %+v, want the other list's source joined by name", m)
	}
	if groups["chan-a-fhd-1080p"] != nil {
		t.Error("the other list's name for channel a became a channel of its own")
	}
	if groups["channel-z-1080p"] == nil || len(order) != 3 {
		t.Errorf("channels %v, want the other list's own channel added", order)
	}
	if !strings.Contains(a.extinf, `group-title="GROUP A"`) || !strings.HasPrefix(header, `#EXTM3U url-tvg=`) {
		t.Errorf("the first list's tags lost: %q, %q", a.extinf, header)
	}
}

// Two channels of a list share a guide id: one of another list matching both joins neither.
func TestAmbiguousGuideIDStaysApart(t *testing.T) {
	first := `#EXTINF:-1 tvg-id="Twin",Twin One 720p
acestream://1000000000000000000000000000000000000000
#EXTINF:-1 tvg-id="Twin",Twin Two 720p
acestream://2000000000000000000000000000000000000000
`
	second := `#EXTINF:-1 tvg-id="Twin",Twin 720p
acestream://3000000000000000000000000000000000000000
#EXTINF:-1 tvg-id="Twin",Twin One 1080p
acestream://4000000000000000000000000000000000000000
`
	sources, _ := mergeLists([][]byte{[]byte(first), []byte(second)})
	_, groups := groupChannels(sources)
	if len(groups) != 4 {
		t.Errorf("channels %v, want the ambiguous one and the other resolution kept apart", groups)
	}
}

func TestListDownFallsBackToItsLastCopy(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up || r.URL.Path != "/backup.m3u" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	l := &lister{cfg: config{stateDir: t.TempDir()}}
	routes := []string{srv.URL + "/main.m3u", srv.URL + "/backup.m3u"}
	var s state
	if got := l.readList(routes, &s, map[string]int{}); string(got) != sample || s.errors != 1 {
		t.Fatalf("read %d bytes with %d errors, want the backup's list after one failed route", len(got), s.errors)
	}
	up = false
	if got := l.readList(routes, &s, map[string]int{}); string(got) != sample {
		t.Errorf("with every route down got %q, want the last copy", got)
	}
	if got := l.readList([]string{srv.URL + "/never.m3u"}, &s, map[string]int{}); got != nil {
		t.Errorf("a list never read gave %q, want nothing", got)
	}
}

func TestSourceListsFromAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lists.json")
	os.WriteFile(path, []byte(`[
  {
    "url": "http://a/list.m3u",
    "backup": ["http://b/list.m3u"]
  },
  {"url": "http://c/other.m3u"}
]
`), 0o644)
	lists, err := readSourceFile(path)
	if err != nil || len(lists) != 2 || len(lists[0]) != 2 {
		t.Errorf("lists = %v, %v; want the file's two lists", lists, err)
	}
	if _, err := readSourceFile(path + ".missing"); err == nil {
		t.Error("a missing file read as lists")
	}
}
