package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
)

// adminRoles are the admins made from the page, on top of BIFROST_ADMINS, kept in STATE_DIR.
type adminRoles struct {
	mu   sync.Mutex
	path string
	set  map[string]bool
}

func loadAdminRoles(dir string) *adminRoles {
	r := &adminRoles{path: filepath.Join(dir, "admins.json"), set: map[string]bool{}}
	if b, err := os.ReadFile(r.path); err == nil {
		var list []string
		if json.Unmarshal(b, &list) == nil {
			for _, e := range list {
				r.set[strings.ToLower(e)] = true
			}
		}
	}
	return r
}

func (r *adminRoles) has(email string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.set[strings.ToLower(email)]
}

func (r *adminRoles) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.set))
	for e := range r.set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

func (r *adminRoles) grant(email string, admin bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	email = strings.ToLower(email)
	if r.set[email] == admin {
		return nil
	}
	if admin {
		r.set[email] = true
	} else {
		delete(r.set, email)
	}
	list := make([]string, 0, len(r.set))
	for e := range r.set {
		list = append(list, e)
	}
	slices.Sort(list)
	b, _ := json.Marshal(list)
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
