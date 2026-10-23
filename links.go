package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const joinLinkTTL = 7 * 24 * time.Hour

type joinLink struct {
	Group   string    `json:"group"`
	By      string    `json:"by"`
	Expires time.Time `json:"expires"`
}

// joinLinks keeps only a hash of each token, so the file alone lets nobody in.
type joinLinks struct {
	mu   sync.Mutex
	path string // empty: kept in memory only
	byID map[string]joinLink
}

func loadJoinLinks(dir string) *joinLinks {
	l := &joinLinks{byID: map[string]joinLink{}}
	if dir == "" {
		return l
	}
	l.path = filepath.Join(dir, "join-links.json")
	if b, err := os.ReadFile(l.path); err == nil {
		if err := json.Unmarshal(b, &l.byID); err != nil {
			log.Printf("[access] %s is unreadable, starting a new one: %v", l.path, err)
			l.byID = map[string]joinLink{}
		}
	}
	return l
}

func linkID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (l *joinLinks) create(group, by string, now time.Time) (token string) {
	b := make([]byte, 24)
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byID[linkID(token)] = joinLink{Group: group, By: by, Expires: now.Add(joinLinkTTL).UTC()}
	l.save()
	return token
}

func (l *joinLinks) find(token string, now time.Time) (joinLink, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	j, ok := l.byID[linkID(token)]
	return j, ok && now.Before(j.Expires)
}

func (l *joinLinks) use(token string) { l.revoke(linkID(token)) }

func (l *joinLinks) revoke(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byID[id]; ok {
		delete(l.byID, id)
		l.save()
	}
}

// pending forgets expired links on the way.
func (l *joinLinks) pending(now time.Time) map[string]joinLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]joinLink{}
	changed := false
	for id, j := range l.byID {
		if now.Before(j.Expires) {
			out[id] = j
		} else {
			delete(l.byID, id)
			changed = true
		}
	}
	if changed {
		l.save()
	}
	return out
}

func (l *joinLinks) save() {
	if l.path == "" {
		return
	}
	if err := writeState(l.path, l.byID); err != nil {
		log.Printf("[access] could not save join links: %v", err)
	}
}
