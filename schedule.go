package main

import (
	"bufio"
	"encoding/xml"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// airing is one programme of the guide, as the page shows it.
type airing struct {
	Title string    `json:"title"`
	Start time.Time `json:"start"`
	Stop  time.Time `json:"stop"`
}

// schedule is guide.xml held in memory: it is already cut down to our channels, a few hundred KB.
type schedule struct {
	mu        sync.RWMutex
	byChannel map[string][]airing // guide channel id -> its programmes, by start time
}

// load replaces the schedule with what guide.xml holds now.
func (s *schedule) load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	by, err := parseGuide(f)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.byChannel = by
	s.mu.Unlock()
	return nil
}

// at returns what a guide channel airs at t and the programme after it; either may be nil.
func (s *schedule) at(channel string, t time.Time) (now, next *airing) {
	if s == nil || channel == "" {
		return nil, nil
	}
	s.mu.RLock()
	list := s.byChannel[channel]
	s.mu.RUnlock()
	i := sort.Search(len(list), func(i int) bool { return list[i].Stop.After(t) })
	if i < len(list) && !list[i].Start.After(t) {
		a := list[i]
		now = &a
		i++
	}
	if i < len(list) {
		a := list[i]
		next = &a
	}
	return now, next
}

const xmltvTime = "20060102150405 -0700"

func parseGuide(r io.Reader) (map[string][]airing, error) {
	dec := xml.NewDecoder(bufio.NewReader(r))
	dec.Strict = false
	by := map[string][]airing{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		el, ok := tok.(xml.StartElement)
		if !ok || el.Name.Local != "programme" {
			continue
		}
		var p struct {
			Title []string `xml:"title"`
		}
		if err := dec.DecodeElement(&p, &el); err != nil {
			return nil, err
		}
		start, err1 := time.Parse(xmltvTime, attr(el, "start"))
		stop, err2 := time.Parse(xmltvTime, attr(el, "stop"))
		if err1 != nil || err2 != nil || len(p.Title) == 0 {
			continue
		}
		ch := attr(el, "channel")
		by[ch] = append(by[ch], airing{Title: strings.TrimSpace(p.Title[0]), Start: start, Stop: stop})
	}
	for _, list := range by {
		sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
	}
	return by, nil
}
