package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func blockingHub(t *testing.T, dir string) (*hub, *httptest.Server) {
	t.Helper()
	h := newHub("http://engine", 0, 0)
	h.table.blocked = loadBlocklist(dir)
	h.table.set([]string{"one-1080p", "two"}, map[string]*channelGroup{
		"one-1080p": {Name: "One 1080p", IDs: []string{idA, idB}},
		"two":       {Name: "Two", IDs: []string{idA}},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/channels", h.serveChannels)
	mux.HandleFunc("POST /api/channels/{slug}/blocked/{id}", h.serveBlock)
	mux.HandleFunc("DELETE /api/channels/{slug}/blocked/{id}", h.serveBlock)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func send(t *testing.T, method, url string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestBlockedSourceLeavesOnlyItsChannel(t *testing.T) {
	dir := t.TempDir()
	h, srv := blockingHub(t, dir)

	if code := send(t, http.MethodPost, srv.URL+"/api/channels/one-1080p/blocked/"+idA); code != http.StatusOK {
		t.Fatalf("block answered %d", code)
	}
	if _, ids := h.table.lookup("one-1080p"); !slices.Equal(ids, []string{idB}) {
		t.Errorf("one-1080p sources = %v, want only %s", ids, idB)
	}
	if _, ids := h.table.lookup("two"); !slices.Equal(ids, []string{idA}) {
		t.Errorf("another channel lost the id: %v", ids)
	}

	resp, err := http.Get(srv.URL + "/api/channels")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Channels []channelInfo }
	json.NewDecoder(resp.Body).Decode(&body)
	one := body.Channels[0]
	if len(one.Sources) != 1 || len(one.Blocked) != 1 || one.Blocked[0].ID != idA {
		t.Errorf("channels answer sources %v, blocked %v", one.Sources, one.Blocked)
	}

	if !loadBlocklist(dir).has("one-1080p", idA) {
		t.Error("the block was not kept on disk")
	}

	if code := send(t, http.MethodDelete, srv.URL+"/api/channels/one-1080p/blocked/"+idA); code != http.StatusOK {
		t.Fatalf("restore answered %d", code)
	}
	if _, ids := h.table.lookup("one-1080p"); len(ids) != 2 {
		t.Errorf("restored channel has %v", ids)
	}
}

func TestBlockKeepsTheLastSource(t *testing.T) {
	_, srv := blockingHub(t, t.TempDir())
	if code := send(t, http.MethodPost, srv.URL+"/api/channels/two/blocked/"+idA); code != http.StatusConflict {
		t.Errorf("blocking the last source answered %d, want 409", code)
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/channels/two/blocked/"+idB); code != http.StatusNotFound {
		t.Errorf("blocking another channel's id answered %d, want 404", code)
	}
}

func TestLadderSkipsBlockedSources(t *testing.T) {
	c := &catalog{blocked: loadBlocklist(t.TempDir())}
	c.set([]string{"x-1080p", "x-720p"}, map[string]*channelGroup{
		"x-1080p": {Name: "X 1080p", IDs: []string{idA}, extinf: `#EXTINF:-1 tvg-id="X",X 1080p`},
		"x-720p":  {Name: "X 720p", IDs: []string{idA, idB}, extinf: `#EXTINF:-1 tvg-id="X",X 720p`},
	})
	c.blocked.mark("x-720p", idA, true)
	if got := c.lower("x-1080p"); !slices.Equal(got, []string{idB}) {
		t.Errorf("lower = %v, want only %s", got, idB)
	}
}
