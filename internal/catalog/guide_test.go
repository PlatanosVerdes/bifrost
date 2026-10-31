package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PlatanosVerdes/bifrost/internal/guide"
	"github.com/PlatanosVerdes/bifrost/internal/m3u"
)

func TestMatchingNeverReplacesAListsGuideID(t *testing.T) {
	file := filepath.Join(t.TempDir(), "guide.xml")
	os.WriteFile(file, []byte(`<tv>
<channel id="Eurosport 1 HD"><display-name>Eurosport 1</display-name></channel>
<channel id="Rally.TV.uk"><display-name>Rally TV</display-name></channel>
</tv>`), 0o644)
	idx, _ := guide.LoadIndex([]string{file})
	groups := map[string]*Group{
		"rally":     {Name: "Rally TV", Extinf: `#EXTINF:-1 group-title="MOTOR",Rally TV`},
		"eurosport": {Name: "Eurosport 1", Extinf: `#EXTINF:-1 tvg-id="Their Own" group-title="X",Eurosport 1`},
		"empty":     {Name: "Eurosport 1", Extinf: `#EXTINF:-1 tvg-id="",Eurosport 1`},
	}
	matchGuideIDs([]string{"rally", "eurosport", "empty"}, groups, idx)
	if got := groups["rally"].Extinf; got != `#EXTINF:-1 tvg-id="Rally.TV.uk" group-title="MOTOR",Rally TV` {
		t.Errorf("rally: %s", got)
	}
	if got := m3u.Attr(groups["eurosport"].Extinf, "tvg-id"); got != "Their Own" {
		t.Errorf("a list's id was replaced: %s", got)
	}
	if got := m3u.Attr(groups["empty"].Extinf, "tvg-id"); got != "Eurosport 1 HD" {
		t.Errorf("an empty id was not filled: %s", groups["empty"].Extinf)
	}
}

func TestAChangedGuideIsRefreshedAgainOnceJellyfinsCopyExpires(t *testing.T) {
	start := time.Now()
	l := &Lister{refreshDue: start, guideDirty: true}
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
