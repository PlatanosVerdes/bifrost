package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func guideXML(now time.Time) string {
	f := func(t time.Time) string { return t.Format(xmltvTime) }
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<tv>
<channel id="Channel A HD"><display-name>Channel A</display-name></channel>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Preview</title></programme>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Evening News</title><desc>Episode 9</desc></programme>
<programme start="%s" stop="%s" channel="Channel A HD"><title lang="es">Recap</title></programme>
</tv>`,
		f(now.Add(-2*time.Hour)), f(now.Add(-time.Hour)),
		f(now.Add(-time.Hour)), f(now.Add(time.Hour)),
		f(now.Add(time.Hour)), f(now.Add(2*time.Hour)))
}

func TestScheduleSaysWhatAirsNowAndNext(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "guide.xml")
	os.WriteFile(path, []byte(guideXML(now)), 0644)
	s := &schedule{}
	if err := s.load(path); err != nil {
		t.Fatal(err)
	}
	cur, next := s.at("Channel A HD", now)
	if cur == nil || cur.Title != "Evening News" || !cur.Stop.Equal(now.Add(time.Hour)) {
		t.Errorf("now = %+v, want the programme until an hour from now", cur)
	}
	if next == nil || next.Title != "Recap" {
		t.Errorf("next = %+v, want Recap", next)
	}
	if cur, next := s.at("Channel A HD", now.Add(3*time.Hour)); cur != nil || next != nil {
		t.Errorf("after the guide ends: %+v %+v, want nothing", cur, next)
	}
	if cur, _ := s.at("Unknown", now); cur != nil {
		t.Errorf("unknown channel airs %+v", cur)
	}
}

func TestChannelsSayWhatAirs(t *testing.T) {
	srv, h := newStatusProxy(t, &perIDEngine{}, twoSources...)
	path := filepath.Join(t.TempDir(), "guide.xml")
	os.WriteFile(path, []byte(guideXML(time.Now())), 0644)
	if err := h.schedule.load(path); err != nil {
		t.Fatal(err)
	}
	var ch struct {
		Channels []channelInfo `json:"channels"`
	}
	getJSON(t, srv.URL+"/api/channels", &ch)
	if len(ch.Channels) != 1 || ch.Channels[0].Now == nil || !strings.Contains(ch.Channels[0].Now.Title, "Evening") {
		t.Errorf("channels = %+v, want what airs now", ch.Channels)
	}
}
