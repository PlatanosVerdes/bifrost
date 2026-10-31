package catalog

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func pass(t *testing.T, m *memory, list string, failed ...string) (string, []string, map[string]*Group) {
	t.Helper()
	return passAt(t, m, list, time.Now(), failed...)
}

func passAt(t *testing.T, m *memory, list string, now time.Time, failed ...string) (string, []string, map[string]*Group) {
	t.Helper()
	sources, header := parseAndDedup([]byte(list))
	order, groups := groupChannels(sources)
	order = m.keep(order, groups, func(id string) bool { return strings.Contains(strings.Join(failed, " "), id) }, now)
	return stableM3U(header, "http://bifrost:6879/channel/", order, groups), order, groups
}

func dropLines(list string, drop ...string) string {
	lines := strings.Split(list, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) && strings.HasPrefix(lines[i], "#EXTINF") && slices.ContainsFunc(drop, func(id string) bool { return strings.Contains(lines[i+1], id) }) {
			i++
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

const third = `#EXTINF:-1 tvg-id="Z" group-title="OTHER",Last Channel 1080p
acestream://5555555555555555555555555555555555555555
`

func TestDroppedChannelsAndSourcesAreKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered.json")
	full := sample + third
	before, _, _ := pass(t, &memory{path: path}, full)

	// The list drops one source of the first channel and the whole second channel.
	m3u, order, groups := pass(t, &memory{path: path}, dropLines(full, "7f9857c45764ef2c7cdfb619b89db3056a57a51f", "3ace1463a7cad979c3a4002b245487e9982e9f1a"))
	if m3u != before {
		t.Errorf("the playlist changed when the list dropped things:\n%s\n---\n%s", before, m3u)
	}
	first := groups[order[0]]
	if len(first.IDs) != 3 || first.IDs[2] != "7f9857c45764ef2c7cdfb619b89db3056a57a51f" || !first.Kept["7f9857c45764ef2c7cdfb619b89db3056a57a51f"] {
		t.Errorf("first channel %+v, want the dropped source kept last", first)
	}
	if len(order) != 3 || !groups[order[1]].Kept["3ace1463a7cad979c3a4002b245487e9982e9f1a"] {
		t.Errorf("order %v, want the dropped channel kept in its place", order)
	}
}

func TestKeptSourceIsForgottenOnceItFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered.json")
	full := sample + third
	pass(t, &memory{path: path}, full)
	dropped := dropLines(full, "7f9857c45764ef2c7cdfb619b89db3056a57a51f", "3ace1463a7cad979c3a4002b245487e9982e9f1a")
	_, order, groups := pass(t, &memory{path: path}, dropped, "7f9857c45764ef2c7cdfb619b89db3056a57a51f", "3ace1463a7cad979c3a4002b245487e9982e9f1a")
	if len(order) != 2 || len(groups[order[0]].IDs) != 2 {
		t.Errorf("order %v, first %+v: want the failed source and the channel left with none forgotten", order, groups[order[0]])
	}
	// Forgotten for good: no longer failing does not bring them back.
	if _, order, _ := pass(t, &memory{path: path}, dropped); len(order) != 2 {
		t.Errorf("order %v, want what was forgotten to stay gone", order)
	}
}

// A failure of a source the list still has is the list's business, not the memory's.
func TestListedSourceStaysWhenItFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered.json")
	_, order, groups := pass(t, &memory{path: path}, sample, "7f9857c45764ef2c7cdfb619b89db3056a57a51f")
	if len(groups[order[0]].IDs) != 3 || groups[order[0]].Kept != nil {
		t.Errorf("first channel %+v, want its listed sources untouched", groups[order[0]])
	}
}

func TestKeptSourceIsForgottenThreeDaysAfterTheListDroppedIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remembered.json")
	full := sample + third
	start := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	passAt(t, &memory{path: path}, full, start)
	dropped := dropLines(full, "7f9857c45764ef2c7cdfb619b89db3056a57a51f", "3ace1463a7cad979c3a4002b245487e9982e9f1a")
	passAt(t, &memory{path: path}, dropped, start.Add(time.Hour))

	// The source is back in the list for a pass, then dropped again: its clock starts over.
	passAt(t, &memory{path: path}, dropLines(full, "3ace1463a7cad979c3a4002b245487e9982e9f1a"), start.Add(2*24*time.Hour))
	passAt(t, &memory{path: path}, dropped, start.Add(2*24*time.Hour+time.Hour))

	_, order, groups := passAt(t, &memory{path: path}, dropped, start.Add(4*24*time.Hour))
	if len(order) != 2 || len(groups[order[0]].IDs) != 3 {
		t.Errorf("order %v, first %+v: want the channel dropped four days ago gone and the source dropped two days ago kept", order, groups[order[0]])
	}
	_, order, groups = passAt(t, &memory{path: path}, dropped, start.Add(5*24*time.Hour+2*time.Hour))
	if len(order) != 2 || len(groups[order[0]].IDs) != 2 {
		t.Errorf("order %v, first %+v: want the source forgotten three days after it left again", order, groups[order[0]])
	}
}

func TestAShorterWindowForgetsSooner(t *testing.T) {
	m := &memory{keepFor: time.Hour}
	if m.window() != time.Hour || (&memory{}).window() != KeepDroppedFor {
		t.Fatal("window")
	}
}
