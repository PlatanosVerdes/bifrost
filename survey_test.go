package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSurveySkipsFreshSourcesAndRunsOneAtATime(t *testing.T) {
	h := newHub("http://127.0.0.1:1", 0, 0)
	h.thumbs = &thumbs{dir: t.TempDir(), ffmpeg: "false", taken: map[string]time.Time{idA: time.Now()}, tried: map[string]time.Time{}}
	h.table.set([]string{"one"}, map[string]*channelGroup{"one": {Name: "One", IDs: []string{idA, idB}}})
	h.facts[idA] = &sourceFacts{ID: idA, SeenAt: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/channels/{slug}/survey", h.serveSurvey)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h.mu.Lock()
	h.surveys["other"] = &surveyState{Slug: "other"}
	h.mu.Unlock()
	if code := send(t, http.MethodPost, srv.URL+"/api/channels/one/survey"); code != http.StatusConflict {
		t.Fatalf("a second survey answered %d, want 409", code)
	}
	h.mu.Lock()
	delete(h.surveys, "other")
	h.mu.Unlock()

	resp, err := http.Post(srv.URL+"/api/channels/one/survey", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Checking int }
	json.NewDecoder(resp.Body).Decode(&body)
	if body.Checking != 1 {
		t.Errorf("checking %d sources, want only the one not seen lately", body.Checking)
	}
}
