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

// inviteLog remembers which admin let each person in. The provider cannot say it: it sees bifrost's
// token make every change.
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
	for _, e := range emails {
		if _, ok := l.byTo[e]; !ok {
			l.byTo[e] = invite{By: by, At: at.UTC()}
		}
	}
	l.save()
}

func (l *inviteLog) forget(email string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byTo, email)
	l.save()
}

// of returns the invites of the given members; people added outside bifrost have none.
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
	b, _ := json.MarshalIndent(l.byTo, "", "  ")
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("[access] could not save invites: %v", err)
		return
	}
	if err := os.Rename(tmp, l.path); err != nil {
		log.Printf("[access] could not save invites: %v", err)
	}
}
