package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Jellyfin keys a channel on its URL and the list rotates content ids hourly: one URL per slug, and
// the proxy resolves it against the current ids.

// sourceMarker is the trailing run of stars that tells sources of one channel apart.
var sourceMarker = regexp.MustCompile(`\s*\*+$`)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

type channelGroup struct {
	Name   string   `json:"name"`
	IDs    []string `json:"ids"`
	extinf string
	kept   map[string]bool // sources the list dropped and the memory still holds
}

func groupKey(name string) string {
	return strings.TrimSpace(sourceMarker.ReplaceAllString(name, ""))
}

// slug keeps "+" apart from nothing at all: "Channel+ D" and "Channel D" are different channels.
func slug(name string) string {
	s := strings.ReplaceAll(strings.ToLower(name), "+", " plus ")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

// mergeLists reads several lists as one: a content id in two lists counts once, from the first.
func mergeLists(lists [][]byte) ([]channel, string) {
	var all []channel
	header := m3uHeader("")
	seen := map[string]bool{}
	for i, data := range lists {
		channels, h := parseAndDedup(data)
		if header == m3uHeader("") {
			header = h
		}
		for _, c := range channels {
			if !seen[c.hash] {
				seen[c.hash] = true
				c.list = i
				all = append(all, c)
			}
		}
	}
	return all, header
}

func groupChannels(sources []channel) ([]string, map[string]*channelGroup) {
	var order []string
	groups := map[string]*channelGroup{}
	firstList := map[string]int{}
	for _, src := range sources {
		key := groupKey(src.name)
		id := slug(key)
		g, ok := groups[id]
		if !ok && src.list > 0 {
			id, ok = sameChannel(src, order, groups, firstList)
			g = groups[id]
		}
		if !ok {
			id = slug(key)
			firstList[id] = src.list
			// The first source's tags (logo, tvg-id, group) describe the channel; only the name changes.
			extinf := src.extinf
			if i := strings.LastIndex(extinf, ","); i >= 0 {
				extinf = extinf[:i+1] + key
			}
			g = &channelGroup{Name: key, extinf: extinf}
			groups[id] = g
			order = append(order, id)
		}
		g.IDs = append(g.IDs, src.hash)
	}
	return order, groups
}

// sameChannel finds the channel an earlier list calls by another name: the one with the same guide id
// and resolution. None, or more than one, and it stays a channel of its own.
func sameChannel(src channel, order []string, groups map[string]*channelGroup, firstList map[string]int) (string, bool) {
	tvg := extractAttr(src.extinf, "tvg-id")
	if tvg == "" {
		return "", false
	}
	match := ""
	for _, id := range order {
		g := groups[id]
		if firstList[id] < src.list && extractAttr(g.extinf, "tvg-id") == tvg && height(g.Name) == height(groupKey(src.name)) {
			if match != "" {
				return "", false
			}
			match = id
		}
	}
	return match, match != ""
}

// A URL of the slug alone, so the playlist only changes when a channel appears or disappears.
func stableM3U(header, channelBaseURL string, order []string, groups map[string]*channelGroup) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, id := range order {
		fmt.Fprintf(&b, "%s\n%s%s\n", groups[id].extinf, channelBaseURL, id)
	}
	return b.String()
}
