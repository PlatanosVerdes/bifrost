package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEngine streams bytes for as long as the request lives and counts the connections it gets.
type fakeEngine struct {
	opened atomic.Int32
	live   atomic.Int32
	dead   bool
}

func (e *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.opened.Add(1)
	if e.dead {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	e.live.Add(1)
	defer e.live.Add(-1)
	w.Header().Set("Content-Type", "video/mp2t")
	chunk := make([]byte, 4096)
	for {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func newTestProxy(t *testing.T, engine http.Handler, maxWait, linger time.Duration) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(engine)
	t.Cleanup(upstream.Close)
	h := newHub(upstream.URL, maxWait, linger)
	proxy := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(proxy.Close)
	return proxy
}

func readSome(ctx context.Context, url string, want int) (int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, nil
	}
	return io.ReadAtLeast(resp.Body, make([]byte, want), want)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Jellyfin plays and probes the same channel at once; the engine must see one connection.
func TestTwoViewersShareOneEngineConnection(t *testing.T) {
	engine := &fakeEngine{}
	proxy := newTestProxy(t, engine, 5*time.Second, 200*time.Millisecond)
	url := proxy.URL + "/ace/getstream?id=cc108ae39f92c48f6c946763047bd1c9b7b7d889"

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	got := make([]int, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = readSome(ctx, url, 64*1024)
		}(i)
	}
	wg.Wait()
	cancel()

	for i, n := range got {
		if n < 64*1024 {
			t.Errorf("viewer %d got %d bytes, want video", i, n)
		}
	}
	if n := engine.opened.Load(); n != 1 {
		t.Errorf("engine got %d connections, want 1", n)
	}
	waitFor(t, "the engine connection to close after the last viewer", func() bool { return engine.live.Load() == 0 })
}

// A viewer arriving inside the linger window reuses the connection instead of opening another.
func TestReconnectWithinLingerReusesTheStream(t *testing.T) {
	engine := &fakeEngine{}
	proxy := newTestProxy(t, engine, 5*time.Second, 500*time.Millisecond)
	url := proxy.URL + "/ace/getstream?id=78124d96a1a4601e7910f50e36a16e35efcfb398"

	if n, _ := readSome(context.Background(), url, 8192); n < 8192 {
		t.Fatalf("first viewer got %d bytes", n)
	}
	if n, _ := readSome(context.Background(), url, 8192); n < 8192 {
		t.Fatalf("second viewer got %d bytes", n)
	}
	if n := engine.opened.Load(); n != 1 {
		t.Errorf("engine got %d connections, want 1", n)
	}
}

func TestDeadChannelAnswers503(t *testing.T) {
	engine := &fakeEngine{dead: true}
	proxy := newTestProxy(t, engine, 300*time.Millisecond, 100*time.Millisecond)

	resp, err := http.Get(proxy.URL + "/ace/getstream?id=0000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
}

// The engine writes in small pieces; a viewer reading at a normal pace must not be dropped for it.
type smallWriterEngine struct{}

func (smallWriterEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	piece := make([]byte, 1316) // seven TS packets, what the engine tends to hand over
	for i := 0; ; i++ {
		if _, err := w.Write(piece); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		if i%400 == 0 { // bursts like the opening buffer, then a trickle
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

func TestSmallEngineWritesDoNotDropAViewer(t *testing.T) {
	proxy := newTestProxy(t, smallWriterEngine{}, 5*time.Second, 100*time.Millisecond)
	resp, err := http.Get(proxy.URL + "/ace/getstream?id=9c75f1d190c518cfe0f236415b8dccc1f676ef3b")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	time.Sleep(300 * time.Millisecond) // the viewer starts reading late, as a player does while it sets up
	n, _ := io.ReadAtLeast(resp.Body, make([]byte, 2<<20), 2<<20)
	if n < 2<<20 {
		t.Errorf("viewer got %d bytes before being dropped, want 2 MB", n)
	}
}

// perIDEngine answers 500 for the ids it is told are dead and streams the rest; it records the order
// in which ids were asked for.
type perIDEngine struct {
	fakeEngine
	deadIDs map[string]bool
	mu      sync.Mutex
	asked   []string
}

func (e *perIDEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	e.mu.Lock()
	e.asked = append(e.asked, id)
	e.mu.Unlock()
	if e.deadIDs[id] {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	e.fakeEngine.ServeHTTP(w, r)
}

func (e *perIDEngine) order() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, id := range e.asked {
		if len(out) == 0 || out[len(out)-1] != id {
			out = append(out, id)
		}
	}
	return out
}

func newChannelProxy(t *testing.T, engine http.Handler, ids ...string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(engine)
	t.Cleanup(upstream.Close)
	h := newHub(upstream.URL, time.Second, 50*time.Millisecond)
	h.table.set([]string{"channel-a-1080p"}, map[string]*channelGroup{
		"channel-a-1080p": {Name: "Channel A 1080p", IDs: ids}})
	h.sourceWait, h.channelWait = 400*time.Millisecond, 3*time.Second
	proxy := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(proxy.Close)
	return proxy
}

var twoSources = []string{"dead000000000000000000000000000000000000", "good000000000000000000000000000000000000"}

func TestDeadSourceFallsThroughToTheNext(t *testing.T) {
	engine := &perIDEngine{deadIDs: map[string]bool{"dead000000000000000000000000000000000000": true}}
	proxy := newChannelProxy(t, engine, twoSources...)
	url := proxy.URL + "/channel/channel-a-1080p"

	if n, _ := readSome(context.Background(), url, 8192); n < 8192 {
		t.Fatalf("got %d bytes, want video from the second source", n)
	}
	if got := engine.order(); len(got) != 2 || got[0][:4] != "dead" || got[1][:4] != "good" {
		t.Errorf("sources tried %v, want dead then good, one at a time", got)
	}

	time.Sleep(200 * time.Millisecond) // past the linger, so the next open is a fresh start
	engine.mu.Lock()
	engine.asked = nil
	engine.mu.Unlock()
	if n, _ := readSome(context.Background(), url, 8192); n < 8192 {
		t.Fatalf("second open got %d bytes", n)
	}
	if got := engine.order(); len(got) != 1 || got[0][:4] != "good" {
		t.Errorf("second open tried %v, want the source that worked first", got)
	}
}

func TestUnknownChannelIs404(t *testing.T) {
	proxy := newChannelProxy(t, &perIDEngine{}, twoSources...)
	resp, err := http.Get(proxy.URL + "/channel/not-a-channel")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}
