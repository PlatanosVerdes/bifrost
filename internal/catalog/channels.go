package catalog

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/PlatanosVerdes/bifrost/internal/guide"
	"github.com/PlatanosVerdes/bifrost/internal/m3u"
)

// Jellyfin keys a channel on its URL and the list rotates content ids hourly: one URL per slug, and
// the proxy resolves it against the current ids.

type Group struct {
	Name   string   `json:"name"`
	IDs    []string `json:"ids"`
	Extinf string
	Kept   map[string]bool // sources the list dropped and the memory still holds
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

func groupChannels(sources []channel) ([]string, map[string]*Group) {
	var order []string
	groups := map[string]*Group{}
	firstList := map[string]int{}
	for _, src := range sources {
		key := m3u.GroupKey(src.name)
		id := m3u.Slug(key)
		g, ok := groups[id]
		if !ok && src.list > 0 {
			id, ok = sameChannel(src, order, groups, firstList)
			g = groups[id]
		}
		if !ok {
			id = m3u.Slug(key)
			firstList[id] = src.list
			// The first source's tags (logo, tvg-id, group) describe the channel; only the name changes.
			extinf := src.extinf
			if i := strings.LastIndex(extinf, ","); i >= 0 {
				extinf = extinf[:i+1] + key
			}
			g = &Group{Name: key, Extinf: extinf}
			groups[id] = g
			order = append(order, id)
		}
		g.IDs = append(g.IDs, src.hash)
	}
	return order, groups
}

// sameChannel finds the channel an earlier list calls by another name: the one with the same guide id
// and resolution. None, or more than one, and it stays a channel of its own.
func sameChannel(src channel, order []string, groups map[string]*Group, firstList map[string]int) (string, bool) {
	tvg := m3u.Attr(src.extinf, "tvg-id")
	if tvg == "" {
		return "", false
	}
	match := ""
	for _, id := range order {
		g := groups[id]
		if firstList[id] < src.list && m3u.Attr(g.Extinf, "tvg-id") == tvg && m3u.Height(g.Name) == m3u.Height(m3u.GroupKey(src.name)) {
			if match != "" {
				return "", false
			}
			match = id
		}
	}
	return match, match != ""
}

// A URL of the slug alone, so the playlist only changes when a channel appears or disappears.
func stableM3U(header, channelBaseURL string, order []string, groups map[string]*Group) string {
	var b strings.Builder
	b.WriteString(header + "\n")
	for _, id := range order {
		fmt.Fprintf(&b, "%s\n%s%s\n", groups[id].Extinf, channelBaseURL, id)
	}
	return b.String()
}

// Lower lists the sources of slug's lower versions, closest resolution first.
func (c *Catalog) Lower(slug string) []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	own := c.groups[slug]
	if own == nil {
		return nil
	}
	tvg, stem, top := m3u.Attr(own.Extinf, "tvg-id"), m3u.ResolutionSuffix.ReplaceAllString(own.Name, ""), m3u.Height(own.Name)
	if tvg == "" || top == 0 {
		return nil
	}
	var siblings []string
	for s, g := range c.groups {
		h := m3u.Height(g.Name)
		if s != slug && h > 0 && h < top && m3u.Attr(g.Extinf, "tvg-id") == tvg && m3u.ResolutionSuffix.ReplaceAllString(g.Name, "") == stem {
			siblings = append(siblings, s)
		}
	}
	sort.Slice(siblings, func(i, j int) bool {
		return m3u.Height(c.groups[siblings[i]].Name) > m3u.Height(c.groups[siblings[j]].Name)
	})
	var ids []string
	for _, s := range siblings {
		ids = append(ids, c.Unblocked(s, c.groups[s].IDs)...)
	}
	return ids
}

func (c *Catalog) HeightOf(id string) int {
	return m3u.Height(c.NameOf(id))
}

// matchGuideIDs gives a guide id to the channels whose lists named none, in the tags the playlist
// carries. A tvg-id the list wrote is never replaced.
func matchGuideIDs(order []string, groups map[string]*Group, idx *guide.Index) {
	if idx == nil {
		return
	}
	var matched int
	var missed []string
	for _, slug := range order {
		g := groups[slug]
		if g == nil || m3u.Attr(g.Extinf, "tvg-id") != "" {
			continue
		}
		id := idx.Match(g.Name)
		if id == "" {
			missed = append(missed, g.Name)
			continue
		}
		g.Extinf = withTvgID(g.Extinf, id)
		matched++
	}
	if matched+len(missed) > 0 {
		sort.Strings(missed)
		log.Printf("Guide: matched %d of %d channels with no guide id; none for: %s", matched, matched+len(missed), strings.Join(missed, ", "))
	}
}

func withTvgID(extinf, id string) string {
	id = strings.ReplaceAll(id, `"`, "")
	if strings.Contains(extinf, `tvg-id=""`) {
		return strings.Replace(extinf, `tvg-id=""`, `tvg-id="`+id+`"`, 1)
	}
	if i := strings.Index(extinf, " "); i >= 0 {
		return extinf[:i] + ` tvg-id="` + id + `"` + extinf[i:]
	}
	if i := strings.LastIndex(extinf, ","); i >= 0 {
		return extinf[:i] + ` tvg-id="` + id + `"` + extinf[i:]
	}
	return extinf
}

// Sorted so a set of ids compares as a string.
func tvgIDs(groups map[string]*Group) []string {
	seen := map[string]bool{}
	for _, g := range groups {
		if m := guide.TvgIDAttr.FindStringSubmatch(g.Extinf); m != nil && m[1] != "" {
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
