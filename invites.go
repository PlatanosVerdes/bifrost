package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type invite struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// The provider cannot say who invited whom: it sees bifrost's token make every change.
type inviteLog struct {
	mu   sync.Mutex
	path string // empty: kept in memory only
	byTo map[string]invite
}

func loadInvites(dir string) *inviteLog {
	l := &inviteLog{byTo: map[string]invite{}}
	if dir == "" {
		return l
	}
	l.path = filepath.Join(dir, "invites.json")
	if b, err := os.ReadFile(l.path); err == nil {
		if err := json.Unmarshal(b, &l.byTo); err != nil {
			log.Printf("[access] %s is unreadable, starting a new one: %v", l.path, err)
			l.byTo = map[string]invite{}
		}
	}
	return l
}

// record keeps the first invite: asking again for someone already on the list changes nothing.
func (l *inviteLog) record(emails []string, by string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for _, e := range emails {
		if _, ok := l.byTo[e]; !ok {
			l.byTo[e] = invite{By: by, At: at.UTC()}
			changed = true
		}
	}
	if changed {
		l.save()
	}
}

// keep forgets everyone on none of the lists, also people removed in the provider's own dashboard.
func (l *inviteLog) keep(lists []accessList) {
	on := map[string]bool{}
	for _, list := range lists {
		for _, m := range list.Members {
			on[m] = true
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for e := range l.byTo {
		if !on[e] {
			delete(l.byTo, e)
			changed = true
		}
	}
	if changed {
		l.save()
	}
}

// People added outside bifrost have no invite.
func (l *inviteLog) of(lists []accessList) map[string]invite {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]invite{}
	for _, list := range lists {
		for _, m := range list.Members {
			if i, ok := l.byTo[m]; ok {
				out[m] = i
			}
		}
	}
	return out
}

func (l *inviteLog) save() {
	if l.path == "" {
		return
	}
	if err := writeState(l.path, l.byTo); err != nil {
		log.Printf("[access] could not save invites: %v", err)
	}
}

// writeState replaces a state file whole, so a crash halfway leaves the old one.
func writeState(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
