package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// sessionEngine answers getstream with a redirect to a session, as the engine does, and counts stops.
type sessionEngine struct {
	stops atomic.Int32
	hold  bool // /ace/r never answers, as for a source that does not start
}

func (e *sessionEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/ace/getstream":
		http.Redirect(w, r, "/ace/r/infohash1/session1", http.StatusFound)
	case r.URL.Path == "/ace/r/infohash1/session1":
		if e.hold {
			<-r.Context().Done()
			return
		}
		w.Write(make([]byte, 188*10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case r.URL.Path == "/ace/cmd/infohash1/session1" && r.URL.Query().Get("method") == "stop":
		e.stops.Add(1)
	}
}

func waitForStops(t *testing.T, e *sessionEngine, want int32) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if e.stops.Load() == want {
			return
		}
	}
	t.Fatalf("engine got %d stops, want %d", e.stops.Load(), want)
}

func TestASourceLetGoStopsItsEngineSession(t *testing.T) {
	engine := &sessionEngine{}
	srv := httptest.NewServer(engine)
	defer srv.Close()

	ctx := context.Background()
	resp, session, ok := openEngine(ctx, srv.Client(), srv.URL+"/ace/getstream?id=x", nil, "test", time.Now().Add(time.Second))
	if !ok {
		t.Fatal("did not open")
	}
	first, ok := awaitFirstBytes(ctx, resp.Body, "test", time.Now(), time.Now().Add(time.Second))
	if !ok {
		t.Fatal("no first bytes")
	}
	up := feed(ctx, "x", resp, first, session)
	if engine.stops.Load() != 0 {
		t.Fatal("stopped while still in use")
	}
	up.cancel()
	waitForStops(t, engine, 1)
}

func TestASourceAbandonedBeforeItAnswersStopsItsEngineSession(t *testing.T) {
	engine := &sessionEngine{hold: true}
	srv := httptest.NewServer(engine)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, ok := openEngine(ctx, srv.Client(), srv.URL+"/ace/getstream?id=x", nil, "test", time.Now().Add(time.Second)); ok {
		t.Fatal("opened a source that never answered")
	}
	waitForStops(t, engine, 1)
}
