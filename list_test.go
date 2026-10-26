package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestASourceFileCanSwitchListsOffWithComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lists.json")
	os.WriteFile(path, []byte(`[
  // Without the curated list: uncomment these two and comment out the last line.
  // {"url": "http://a", "backup": ["http://a2"]},
  // {"url": "http://b"},
  {"url": "http://curated"}
]`), 0o644)
	lists, err := readSourceFile(path)
	if err != nil || len(lists) != 1 || lists[0][0] != "http://curated" {
		t.Fatalf("got %v, %v", lists, err)
	}
	os.WriteFile(path, []byte(`[
  {"url": "http://a", "backup": ["http://a2"]},
  {"url": "http://b"}
  // {"url": "http://curated"}
]`), 0o644)
	if lists, err = readSourceFile(path); err != nil || len(lists) != 2 || lists[0][1] != "http://a2" {
		t.Fatalf("rolled back: got %v, %v", lists, err)
	}
}
