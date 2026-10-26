package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"log"
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
	body := bufio.NewReader(resp.Body)
	var src io.Reader = body
	// Several published guides are gzipped (.xml.gz); kept unpacked so a trim streams plain XML.
	if magic, _ := body.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return false, err
		}
		defer zr.Close()
		src = zr
	}
	tmp := cache + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(f, src); err != nil {
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

// Streams the full guides token by token instead of holding them in memory, into one guide.xml.
// A channel and its programmes come from the guide that owner names, so two guides that both carry
// an id do not double its programmes; without owner every guide counts. Reports whether out changed.
func trimGuide(fulls []string, out string, ids []string, owner map[string]int) (bool, error) {
	var root *xml.StartElement
	var body bytes.Buffer
	for i, full := range fulls {
		want := map[string]bool{}
		for _, id := range ids {
			if o, ok := owner[id]; owner == nil || (ok && o == i) {
				want[id] = true
			}
		}
		start, children, err := trimOne(full, want)
		if err != nil {
			if i == 0 {
				return false, err
			}
			log.Printf("warning: guide %s: %v", filepath.Base(full), err)
			continue
		}
		if root == nil {
			root = &start
		}
		body.Write(children)
	}
	if root == nil {
		return false, fmt.Errorf("no guide could be read")
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	if err := enc.EncodeToken(*root); err != nil {
		return false, err
	}
	if err := enc.Flush(); err != nil {
		return false, err
	}
	buf.Write(body.Bytes())
	buf.WriteString("</" + root.Name.Local + ">")

	if old, err := os.ReadFile(out); err == nil && bytes.Equal(old, buf.Bytes()) {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, out)
}

// trimOne returns a guide's root element and its wanted children, encoded; nothing on an error, so a
// guide broken half way adds none of itself.
func trimOne(full string, want map[string]bool) (xml.StartElement, []byte, error) {
	var root xml.StartElement
	in, err := os.Open(full)
	if err != nil {
		return root, nil, err
	}
	defer in.Close()
	var buf bytes.Buffer
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
			return root, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				root = t.Copy()
				continue
			}
			if depth == 2 {
				key := "id"
				if t.Name.Local == "programme" {
					key = "channel"
				}
				if !want[attr(t, key)] {
					if err := dec.Skip(); err != nil {
						return root, nil, err
					}
					depth--
					continue
				}
			}
			if err := enc.EncodeToken(t.Copy()); err != nil {
				return root, nil, err
			}
		case xml.EndElement:
			depth--
			if depth >= 1 {
				if err := enc.EncodeToken(t); err != nil {
					return root, nil, err
				}
			}
		case xml.CharData:
			if depth >= 2 {
				if err := enc.EncodeToken(t.Copy()); err != nil {
					return root, nil, err
				}
			}
		}
	}
	if err := enc.Flush(); err != nil {
		return root, nil, err
	}
	return root, buf.Bytes(), nil
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
