package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/stream"
)

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func blockingCatalog(t *testing.T, dir string) (*catalog.Catalog, *httptest.Server) {
	t.Helper()
	cat := &catalog.Catalog{Blocked: catalog.LoadBlocklist(dir)}
	cat.Set([]string{"one-1080p", "two"}, map[string]*catalog.Group{
		"one-1080p": {Name: "One 1080p", IDs: []string{idA, idB}},
		"two":       {Name: "Two", IDs: []string{idA}},
	})
	a := testAPI(stream.Config{Engine: "http://engine", Catalog: cat})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/channels", a.serveChannels)
	mux.HandleFunc("POST /api/channels/{slug}/blocked/{id}", a.serveBlock)
	mux.HandleFunc("DELETE /api/channels/{slug}/blocked/{id}", a.serveBlock)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return cat, srv
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
	cat, srv := blockingCatalog(t, dir)

	if code := send(t, http.MethodPost, srv.URL+"/api/channels/one-1080p/blocked/"+idA); code != http.StatusOK {
		t.Fatalf("block answered %d", code)
	}
	if _, ids := cat.Lookup("one-1080p"); !slices.Equal(ids, []string{idB}) {
		t.Errorf("one-1080p sources = %v, want only %s", ids, idB)
	}
	if _, ids := cat.Lookup("two"); !slices.Equal(ids, []string{idA}) {
		t.Errorf("another channel lost the id: %v", ids)
	}

	resp, err := http.Get(srv.URL + "/api/channels")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct{ Channels []stream.ChannelInfo }
	json.NewDecoder(resp.Body).Decode(&body)
	one := body.Channels[0]
	if len(one.Sources) != 1 || len(one.Blocked) != 1 || one.Blocked[0].ID != idA {
		t.Errorf("channels answer sources %v, blocked %v", one.Sources, one.Blocked)
	}

	if !catalog.LoadBlocklist(dir).Has("one-1080p", idA) {
		t.Error("the block was not kept on disk")
	}

	if code := send(t, http.MethodDelete, srv.URL+"/api/channels/one-1080p/blocked/"+idA); code != http.StatusOK {
		t.Fatalf("restore answered %d", code)
	}
	if _, ids := cat.Lookup("one-1080p"); len(ids) != 2 {
		t.Errorf("restored channel has %v", ids)
	}
}

func TestBlockKeepsTheLastSource(t *testing.T) {
	_, srv := blockingCatalog(t, t.TempDir())
	if code := send(t, http.MethodPost, srv.URL+"/api/channels/two/blocked/"+idA); code != http.StatusConflict {
		t.Errorf("blocking the last source answered %d, want 409", code)
	}
	if code := send(t, http.MethodPost, srv.URL+"/api/channels/two/blocked/"+idB); code != http.StatusNotFound {
		t.Errorf("blocking another channel's id answered %d, want 404", code)
	}
}
