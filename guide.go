package main

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Jellyfin re-parsed the full 34 MB guide (642 channels, 23 of them ours) on every refresh, about 2.5
// minutes of a Pi 4. It reads this cut-down guide.xml instead, so a refresh takes seconds.

var tvgIDAttr = regexp.MustCompile(`tvg-id="([^"]*)"`)

// The publisher updates the full guide once a day, so passes in between reuse the copy on disk.
func fetchGuide(sourceURL, cache string, maxAge time.Duration) (bool, error) {
	if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) < maxAge {
		return false, nil
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(sourceURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("guide download: HTTP %d", resp.StatusCode)
	}
	tmp := cache + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, cache)
}

// Sorted so a set of ids compares as a string.
func tvgIDs(groups map[string]*channelGroup) []string {
	seen := map[string]bool{}
	for _, g := range groups {
		if m := tvgIDAttr.FindStringSubmatch(g.extinf); m != nil && m[1] != "" {
			seen[m[1]] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Streams the full guide token by token instead of holding it in memory. Reports whether out changed.
func trimGuide(full, out string, ids []string) (bool, error) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	in, err := os.Open(full)
	if err != nil {
		return false, err
	}
	defer in.Close()

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	dec := xml.NewDecoder(bufio.NewReader(in))
	dec.Strict = false

	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				if err := enc.EncodeToken(t.Copy()); err != nil {
					return false, err
				}
				continue
			}
			if depth == 2 {
				key := "id"
				if t.Name.Local == "programme" {
					key = "channel"
				}
				if !want[attr(t, key)] {
					if err := dec.Skip(); err != nil {
						return false, err
					}
					depth--
					continue
				}
			}
			if err := enc.EncodeToken(t.Copy()); err != nil {
				return false, err
			}
		case xml.EndElement:
			depth--
			if err := enc.EncodeToken(t); err != nil {
				return false, err
			}
		case xml.CharData:
			if depth >= 2 {
				if err := enc.EncodeToken(t.Copy()); err != nil {
					return false, err
				}
			}
		}
	}
	if err := enc.Flush(); err != nil {
		return false, err
	}

	if old, err := os.ReadFile(out); err == nil && bytes.Equal(old, buf.Bytes()) {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, out)
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func sameIDs(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}
