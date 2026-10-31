// Package m3u reads the names and attributes of a channel list's #EXTINF lines.
package m3u

import (
	"fmt"
	"regexp"
	"strings"
)

func Attr(line, attr string) string {
	prefix := attr + `="`
	idx := strings.Index(line, prefix)
	if idx == -1 {
		return ""
	}
	rest := line[idx+len(prefix):]
	end := strings.Index(rest, `"`)
	if end == -1 {
		return ""
	}
	return rest[:end]
}

// sourceMarker is the trailing run of stars that tells sources of one channel apart.
var sourceMarker = regexp.MustCompile(`\s*\*+$`)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func GroupKey(name string) string {
	return strings.TrimSpace(sourceMarker.ReplaceAllString(name, ""))
}

// Slug keeps "+" apart from nothing at all: "Channel+ D" and "Channel D" are different channels.
func Slug(name string) string {
	s := strings.ReplaceAll(strings.ToLower(name), "+", " plus ")
	return strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
}

var ResolutionSuffix = regexp.MustCompile(`(?i)\s+(\d{3,4}p|4k)$`)

// Height is a channel's resolution from its name, 0 when it carries none.
func Height(name string) int {
	m := ResolutionSuffix.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	if strings.EqualFold(m[1], "4k") {
		return 2160
	}
	var h int
	fmt.Sscanf(m[1], "%dp", &h)
	return h
}
