package stream

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Thumbnails come from the bytes a playing source already sends, so they cost no extra stream and
// never show the slate. One ffmpeg at a time decoding only a keyframe keeps the Pi out of it.
const (
	thumbAfter = 20 * time.Second // past what the engine had buffered when the source opened
	thumbEvery = 2 * time.Minute  // the playing source's picture on the page; a cut costs a fraction of a second
	thumbRetry = time.Minute
	thumbBytes = 3 << 20 // a keyframe well inside, even at 1080p
	thumbWait  = 20 * time.Second
)

// Loglevel info lists the input's streams, so one run also gives the source's height and audio.
var thumbArgs = []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-nostats", "-skip_frame", "nokey",
	"-f", "mpegts", "-i", "pipe:0", "-map", "0:v:0", "-frames:v", "1", "-vf", "scale=640:-2", "-pix_fmt", "yuvj420p",
	"-f", "mjpeg", "pipe:1"}

var hexID = regexp.MustCompile(`^[0-9a-f]{40}$`)

type thumbs struct {
	dir    string
	ffmpeg string
	run    sync.Mutex // one ffmpeg at a time
	mu     sync.Mutex
	taken  map[string]time.Time // id -> when its thumbnail was cut
	tried  map[string]time.Time // id -> last attempt, so a failing source is not retried every chunk
}

func newThumbs(dir, ffmpeg string) *thumbs {
	if ffmpeg == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("warning: no thumbnails: %v", err)
		return nil
	}
	t := &thumbs{dir: dir, ffmpeg: ffmpeg, taken: map[string]time.Time{}, tried: map[string]time.Time{}}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jpg")
		info, err := e.Info()
		if !ok || err != nil {
			continue
		}
		if time.Since(info.ModTime()) > factsKept {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		t.taken[id] = info.ModTime()
	}
	return t
}

func (t *thumbs) at(id string) time.Time {
	if t == nil {
		return time.Time{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.taken[id]
}

func (t *thumbs) due(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return time.Since(t.taken[id]) > thumbEvery && time.Since(t.tried[id]) > thumbRetry
}

// cut makes the thumbnail in the background, or skips it while another is being made.
func (t *thumbs) cut(id string, ts []byte) {
	if !t.run.TryLock() {
		return
	}
	go func() {
		defer t.run.Unlock()
		t.make(id, ts)
	}()
}

// cutNow waits its turn and also answers what the stream carries.
func (t *thumbs) cutNow(id string, ts []byte) (height int, audio string) {
	t.run.Lock()
	defer t.run.Unlock()
	return t.make(id, ts)
}

func (t *thumbs) make(id string, ts []byte) (height int, audio string) {
	t.mu.Lock()
	t.tried[id] = time.Now()
	t.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), thumbWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.ffmpeg, thumbArgs...)
	cmd.Stdin = bytes.NewReader(ts)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	jpg, err := cmd.Output()
	for _, line := range strings.Split(stderr.String(), "\n") {
		if h, a := streamInfo(line); h > 0 && height == 0 {
			height = h
		} else if a != "" && audio == "" {
			audio = a
		}
	}
	if err != nil || len(jpg) == 0 {
		log.Printf("[thumbs] %s: no frame: %v", short(id), err)
		return height, audio
	}
	path := filepath.Join(t.dir, id+".jpg")
	if err := os.WriteFile(path+".tmp", jpg, 0o644); err != nil {
		log.Printf("[thumbs] %s: %v", short(id), err)
		return height, audio
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		log.Printf("[thumbs] %s: %v", short(id), err)
		return height, audio
	}
	t.mu.Lock()
	t.taken[id] = time.Now()
	t.mu.Unlock()
	return height, audio
}

// thumbGrab collects one source's bytes for a thumbnail while it plays.
type thumbGrab struct {
	t    *thumbs
	id   string
	from time.Time
	buf  []byte
}

func (t *thumbs) grab(id string) *thumbGrab {
	if t == nil {
		return nil
	}
	return &thumbGrab{t: t, id: id, from: time.Now()}
}

func (g *thumbGrab) push(b []byte) {
	if g == nil || time.Since(g.from) < thumbAfter {
		return
	}
	if len(g.buf) == 0 && !g.t.due(g.id) {
		return
	}
	if g.buf = append(g.buf, b...); len(g.buf) >= thumbBytes {
		g.t.cut(g.id, g.buf)
		g.buf = nil
	}
}

// file is where a source's thumbnail is, empty for an id that cannot have one.
func (t *thumbs) file(id string) string {
	if t == nil || !hexID.MatchString(id) {
		return ""
	}
	return filepath.Join(t.dir, id+".jpg")
}
