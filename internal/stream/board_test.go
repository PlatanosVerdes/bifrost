package stream

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChannelsSayWhatAirs(t *testing.T) {
	srv, h := newStatusProxy(t, &perIDEngine{}, twoSources...)
	path := filepath.Join(t.TempDir(), "guide.xml")
	os.WriteFile(path, []byte(airingGuide(time.Now())), 0644)
	if err := h.schedule.Load(path); err != nil {
		t.Fatal(err)
	}
	var ch struct {
		Channels []ChannelInfo `json:"channels"`
	}
	getJSON(t, srv.URL+"/api/channels", &ch)
	if len(ch.Channels) != 1 || ch.Channels[0].Now == nil || !strings.Contains(ch.Channels[0].Now.Title, "Evening") {
		t.Errorf("channels = %+v, want what airs now", ch.Channels)
	}
}

// airingGuide has Channel A airing "Evening News" now, between a programme before and one after.
func airingGuide(now time.Time) string {
	f := func(t time.Time) string { return t.Format("20060102150405 -0700") }
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<tv>
<channel id="Channel A HD"><display-name>Channel A</display-name></channel>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Preview</title></programme>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Evening News</title></programme>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Recap</title></programme>
</tv>`,
		f(now.Add(-2*time.Hour)), f(now.Add(-time.Hour)),
		f(now.Add(-time.Hour)), f(now.Add(time.Hour)),
		f(now.Add(time.Hour)), f(now.Add(2*time.Hour)))
}
