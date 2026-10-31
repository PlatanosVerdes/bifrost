package guide

import (
	"bufio"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/PlatanosVerdes/bifrost/internal/m3u"
)

// Source is one of the guides GUIDE_SOURCE_URL lists, in its order, and the file it is kept in.
type Source struct {
	URL  string
	File string
}

// The first keeps the name the single guide always had, so its copy on disk is reused.
func Sources(raw, stateDir string) []Source {
	var out []Source
	for _, u := range strings.Split(raw, ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		file := filepath.Join(stateDir, "guide-full.xml")
		if len(out) > 0 {
			sum := sha256.Sum256([]byte(u))
			file = filepath.Join(stateDir, fmt.Sprintf("guide-%x.xml", sum[:6]))
		}
		out = append(out, Source{URL: u, File: file})
	}
	return out
}

// Index says which guide channel a playlist name stands for, and which guide holds each id.
type Index struct {
	strict []map[string][]string // per guide, in order: normalised name -> its channel ids
	loose  []map[string][]string // the same, with country prefixes and bracketed notes dropped
	Owner  map[string]int        // channel id -> the first guide that has it
}

var (
	qualityWord   = regexp.MustCompile(`\b(hd|fhd|uhd|sd|4k|\d{3,4}p)\b`)
	bracketed     = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	countryPrefix = regexp.MustCompile(`^\s*[A-Za-z]{2,3}\s*[|·:]\s*`)
	letterDigit   = regexp.MustCompile(`(\pL)(\pN)`)
	nonAlnum      = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

// normName is what a playlist name and a guide name share when they mean the same channel: case,
// quality tags, punctuation and source markers go, "+" reads "plus", "TV3" reads "TV 3". Loose also
// drops a country prefix ("DE | Rally TV") and bracketed notes, for when the strict form finds nothing.
func normName(s string, loose bool) string {
	s = m3u.GroupKey(s)
	if loose {
		s = countryPrefix.ReplaceAllString(s, "")
		s = bracketed.ReplaceAllString(s, " ")
	}
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "+", " plus ")
	s = nonAlnum.ReplaceAllString(s, " ")
	s = qualityWord.ReplaceAllString(s, " ")
	s = letterDigit.ReplaceAllString(s, "$1 $2")
	return strings.Join(strings.Fields(s), " ")
}

// LoadIndex reads only the <channel> elements of each guide, streaming.
func LoadIndex(files []string) (*Index, error) {
	idx := &Index{Owner: map[string]int{}}
	for i, file := range files {
		strict, loose := map[string][]string{}, map[string][]string{}
		idx.strict, idx.loose = append(idx.strict, strict), append(idx.loose, loose)
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(bufio.NewReader(f))
		dec.Strict = false
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("%s: %v", filepath.Base(file), err)
			}
			start, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			switch start.Name.Local {
			case "programme":
				dec.Skip()
			case "channel":
				var ch struct {
					ID    string   `xml:"id,attr"`
					Names []string `xml:"display-name"`
				}
				if dec.DecodeElement(&ch, &start) != nil || ch.ID == "" {
					continue
				}
				if _, seen := idx.Owner[ch.ID]; !seen {
					idx.Owner[ch.ID] = i
				}
				for _, n := range append(ch.Names, strings.ReplaceAll(ch.ID, ".", " ")) {
					for tier, names := range []map[string][]string{strict, loose} {
						if k := normName(n, tier == 1); k != "" && !slices.Contains(names[k], ch.ID) {
							names[k] = append(names[k], ch.ID)
						}
					}
				}
			}
		}
		f.Close()
	}
	return idx, nil
}

// Match finds the guide id for a name: the strict form first, in every guide, then the loose one. The
// first guide that knows the name decides, and only when it knows it as one channel. Two channels of
// one name there, or none anywhere, give no guide rather than a wrong one.
func (idx *Index) Match(name string) string {
	if idx == nil {
		return ""
	}
	for tier, guides := range [][]map[string][]string{idx.strict, idx.loose} {
		k := normName(name, tier == 1)
		if k == "" {
			continue
		}
		for _, names := range guides {
			switch ids := names[k]; len(ids) {
			case 0:
				continue
			case 1:
				return ids[0]
			default:
				return ""
			}
		}
	}
	return ""
}
