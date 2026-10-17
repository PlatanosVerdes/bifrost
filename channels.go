package main

import (
	"fmt"
	"regexp"
	"strings"
)

// The list republishes the same channels under new content ids several times an hour, and Jellyfin
// keys a channel on the MD5 of its URL: an id in the URL made every rotation a new channel and a
// guide refresh. So Jellyfin gets one URL per channel that never changes, and the proxy resolves it
// against the catalog, which carries the ids of the moment.

// sourceMarker is the trailing run of stars that tells sources of one channel apart.
var sourceMarker = regexp.MustCompile(`\s*\*+$`)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

type channelGroup struct {
	Name   string   `json:"name"`
	IDs    []string `json:"ids"`
	extinf string
}

func groupKey(name string) string {
	return strings.TrimSpace(sourceMarker.ReplaceAllString(name, ""))
}

// slug keeps "+" apart from nothing at all: "M+ LaLiga" and "M LaLiga" are different channels.
func slug(name string) string {
	s := strings.ReplaceAll(strings.ToLower(name), "+", " plus ")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

// groupChannels folds the sources into channels in order of first appearance, with each channel's
// ids in the order the list gives them.
func groupChannels(sources []channel) ([]string, map[string]*channelGroup) {
	var order []string
	groups := map[string]*channelGroup{}
	for _, src := range sources {
		key := groupKey(src.name)
		id := slug(key)
		g, ok := groups[id]
		if !ok {
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

// stableM3U lists each channel once, at a URL made of its slug alone, so it only changes when a
// channel appears or disappears.
func stableM3U(header, channelBaseURL string, order []string, groups map[string]*channelGroup) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, id := range order {
		fmt.Fprintf(&b, "%s\n%s%s\n", groups[id].extinf, channelBaseURL, id)
	}
	return b.String()
}
