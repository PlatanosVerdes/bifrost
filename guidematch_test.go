package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormNameKeepsWhatTellsChannelsApart(t *testing.T) {
	for in, want := range map[string][2]string{
		"DAZN Baloncesto 1080p **": {"dazn baloncesto", "dazn baloncesto"},
		"M+ Liga de Campeones 6":   {"m plus liga de campeones 6", "m plus liga de campeones 6"},
		"BBC HD [PL]":              {"bbc pl", "bbc"},
		"Sky.Sports.F1.HD.uk":      {"sky sports f 1 uk", "sky sports f 1 uk"},
		"SPORT TV3 720p":           {"sport tv 3", "sport tv 3"},
		"DE | Rally TV":            {"de rally tv", "rally tv"},
		"FR · beIN SPORTS 3":       {"fr bein sports 3", "bein sports 3"},
		"Red Bull TV (DAZN)":       {"red bull tv dazn", "red bull tv"},
	} {
		if got := normName(in, false); got != want[0] {
			t.Errorf("strict normName(%q) = %q, want %q", in, got, want[0])
		}
		if got := normName(in, true); got != want[1] {
			t.Errorf("loose normName(%q) = %q, want %q", in, got, want[1])
		}
	}
}

func TestCountryPrefixesMatchOnlyWhenNothingStricterDoes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "g.xml")
	os.WriteFile(file, []byte(`<tv>
  <channel id="DE | Rally TV"><display-name>DE | Rally TV</display-name></channel>
  <channel id="Red Bull TV (DAZN)"><display-name>Red Bull TV (DAZN)</display-name></channel>
  <channel id="Red Bull TV"><display-name>Red Bull TV</display-name></channel>
  <channel id="PT | Sport TV 3"><display-name>PT | Sport TV 3</display-name></channel>
  <channel id="FR · beIN SPORTS 3"><display-name>FR · beIN SPORTS 3</display-name></channel>
  <channel id="UK | Sky News"><display-name>UK | Sky News</display-name></channel>
  <channel id="DE | Sky News"><display-name>DE | Sky News</display-name></channel>
</tv>`), 0o644)
	idx, err := loadGuideIndex([]string{file})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"RALLY TV 1080p":   "DE | Rally TV",
		"Red Bull TV":      "Red Bull TV", // the strict form tells it from the DAZN one
		"SPORT TV3 720p":   "PT | Sport TV 3",
		"Bein Sports 3 TR": "", // a different country is a different channel
		"Sky News":         "", // two countries carry it: no guide beats a wrong one
	} {
		if got := idx.match(name); got != want {
			t.Errorf("match(%q) = %q, want %q", name, got, want)
		}
	}
}

const spanishGuide = `<tv generator-info-name="es">
  <channel id="Eurosport 1 HD"><display-name>Eurosport 1</display-name></channel>
  <channel id="Sky A"><display-name>Sky Sports</display-name></channel>
  <channel id="Sky B"><display-name>Sky Sports</display-name></channel>
  <programme start="20261026200000 +0100" stop="20261026220000 +0100" channel="Eurosport 1 HD"><title>Cycling ES</title></programme>
</tv>`

const otherGuide = `<tv generator-info-name="uk">
  <channel id="Eurosport.1.de"><display-name>Eurosport 1</display-name></channel>
  <channel id="Rally.TV.uk"><display-name>Rally TV</display-name></channel>
  <channel id="Eurosport 1 HD"><display-name>Eurosport 1</display-name></channel>
  <programme start="20261026200000 +0100" stop="20261026220000 +0100" channel="Rally.TV.uk"><title>Rally live</title></programme>
  <programme start="20261026200000 +0100" stop="20261026220000 +0100" channel="Eurosport 1 HD"><title>Cycling twice</title></programme>
</tv>`

func writeGuides(t *testing.T) (string, []string) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.xml"), filepath.Join(dir, "b.xml")
	os.WriteFile(a, []byte(spanishGuide), 0o644)
	os.WriteFile(b, []byte(otherGuide), 0o644)
	return dir, []string{a, b}
}

func TestTheFirstGuideThatKnowsANameDecides(t *testing.T) {
	_, files := writeGuides(t)
	idx, err := loadGuideIndex(files)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"Eurosport 1 1080p": "Eurosport 1 HD", // in both: the first guide wins
		"Rally TV":          "Rally.TV.uk",    // only in the second
		"Sky Sports":        "",               // two channels of that name: no guide beats a wrong one
		"Unknown":           "",
	} {
		if got := idx.match(name); got != want {
			t.Errorf("match(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMatchingNeverReplacesAListsGuideID(t *testing.T) {
	_, files := writeGuides(t)
	idx, _ := loadGuideIndex(files)
	groups := map[string]*channelGroup{
		"rally":     {Name: "Rally TV", extinf: `#EXTINF:-1 group-title="MOTOR",Rally TV`},
		"eurosport": {Name: "Eurosport 1", extinf: `#EXTINF:-1 tvg-id="Their Own" group-title="X",Eurosport 1`},
		"empty":     {Name: "Eurosport 1", extinf: `#EXTINF:-1 tvg-id="",Eurosport 1`},
	}
	matchGuideIDs([]string{"rally", "eurosport", "empty"}, groups, idx)
	if got := groups["rally"].extinf; got != `#EXTINF:-1 tvg-id="Rally.TV.uk" group-title="MOTOR",Rally TV` {
		t.Errorf("rally: %s", got)
	}
	if got := extractAttr(groups["eurosport"].extinf, "tvg-id"); got != "Their Own" {
		t.Errorf("a list's id was replaced: %s", got)
	}
	if got := extractAttr(groups["empty"].extinf, "tvg-id"); got != "Eurosport 1 HD" {
		t.Errorf("an empty id was not filled: %s", groups["empty"].extinf)
	}
}

func TestSeveralGuidesTrimIntoOneWithoutDoubling(t *testing.T) {
	dir, files := writeGuides(t)
	idx, _ := loadGuideIndex(files)
	out := filepath.Join(dir, "guide.xml")
	if _, err := trimGuide(files, out, []string{"Eurosport 1 HD", "Rally.TV.uk"}, idx.owner); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	var doc struct {
		Generator string `xml:"generator-info-name,attr"`
		Channels  []struct {
			ID string `xml:"id,attr"`
		} `xml:"channel"`
		Programmes []struct {
			Title string `xml:"title"`
		} `xml:"programme"`
	}
	if err := xml.Unmarshal(got, &doc); err != nil {
		t.Fatalf("not one valid guide: %v\n%s", err, got)
	}
	var titles []string
	for _, p := range doc.Programmes {
		titles = append(titles, p.Title)
	}
	if doc.Generator != "es" || len(doc.Channels) != 2 || strings.Join(titles, ",") != "Cycling ES,Rally live" {
		t.Fatalf("got %+v\n%s", doc, got)
	}
}

func TestABrokenSecondGuideAddsNothing(t *testing.T) {
	dir, files := writeGuides(t)
	os.WriteFile(files[1], []byte(`<tv><channel id="Rally.TV.uk"><display-name>Rally`), 0o644)
	out := filepath.Join(dir, "guide.xml")
	if _, err := trimGuide(files, out, []string{"Eurosport 1 HD", "Rally.TV.uk"}, map[string]int{"Eurosport 1 HD": 0, "Rally.TV.uk": 1}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if err := xml.Unmarshal(got, new(struct{})); err != nil || strings.Contains(string(got), "Rally") {
		t.Fatalf("got %v\n%s", err, got)
	}
}

func TestGzippedGuidesAreUnpacked(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(spanishGuide))
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(gz.Bytes()) }))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "g.xml")
	if fresh, err := fetchGuide(srv.URL+"/es.xml.gz", cache, time.Hour); err != nil || !fresh {
		t.Fatalf("fresh=%v err=%v", fresh, err)
	}
	if got, _ := os.ReadFile(cache); string(got) != spanishGuide {
		t.Fatalf("cache holds %q", got[:min(len(got), 40)])
	}
}

func TestGuideSourcesKeepTheFirstCacheName(t *testing.T) {
	s := guideSources(" https://a/guide.xml , https://b/es.xml.gz,", "/state")
	if len(s) != 2 || s[0].file != "/state/guide-full.xml" || !strings.HasPrefix(s[1].file, "/state/guide-") || s[1].file == s[0].file {
		t.Fatalf("got %+v", s)
	}
}

func TestAChangedGuideIsRefreshedAgainOnceJellyfinsCopyExpires(t *testing.T) {
	start := time.Now()
	l := &lister{refreshDue: start, guideDirty: true}
	if !l.refreshNow(start) {
		t.Fatal("the first refresh is due")
	}
	l.refreshedAt(start)
	if l.refreshNow(start.Add(30 * time.Minute)) {
		t.Fatal("refreshed again before Jellyfin's copy expired")
	}
	if !l.refreshNow(start.Add(jellyfinGuideCopyAge + 2*time.Minute)) {
		t.Fatal("no refresh once the copy expired")
	}
	l.refreshedAt(start.Add(jellyfinGuideCopyAge + 2*time.Minute))
	if l.refreshNow(start.Add(5 * jellyfinGuideCopyAge)) {
		t.Fatal("the guide did not change again, yet refreshes keep coming")
	}
}
