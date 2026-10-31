package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// engineSession is what the engine opened for one request. It keeps a session some 15 s after its
// player leaves and, past its session limit, stops the oldest one, the one being watched: so bifrost
// stops every session it lets go instead of waiting for the engine to.
type engineSession struct {
	mu   sync.Mutex
	cmds []string // one per redirect the engine answered: open retries may start more than one
}

// note takes the session from the engine's redirect to /ace/r/<infohash>/<session>, whose commands
// live at /ace/cmd/<infohash>/<session>.
func (e *engineSession) note(u *url.URL) {
	rest, ok := strings.CutPrefix(u.Path, "/ace/r/")
	if parts := strings.Split(rest, "/"); !ok || len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return
	}
	cmd := *u
	cmd.Path, cmd.RawPath, cmd.RawQuery = "/ace/cmd/"+rest, "", "method=stop"
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.cmds {
		if c == cmd.String() {
			return
		}
	}
	e.cmds = append(e.cmds, cmd.String())
}

// stop runs in the background: it is called from the relay loop, which must not wait on the engine.
func (e *engineSession) stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	cmds := e.cmds
	e.cmds = nil
	e.mu.Unlock()
	for _, cmd := range cmds {
		go func() {
			resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(cmd)
			if err != nil {
				log.Printf("[proxy] stopping an engine session: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
}

// openEngine is open that also keeps the engine sessions it started, even when it gives up before the
// stream answers.
func openEngine(ctx context.Context, client *http.Client, target string, headers http.Header, ch string, deadline time.Time) (*http.Response, *engineSession, bool) {
	e := &engineSession{}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		e.note(req.URL)
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, ok := open(ctx, &c, target, headers, ch, deadline)
	if !ok {
		e.stop()
	}
	return resp, e, ok
}
