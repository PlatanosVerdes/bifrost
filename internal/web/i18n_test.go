package web

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// Every text app.js hands to t() or plural() has its Spanish in i18n.js, with the same things filled in.
func TestEveryPageTextHasItsSpanish(t *testing.T) {
	app, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	table, err := os.ReadFile("static/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	// A parameter named t hid the translation and left the page blank.
	if m := regexp.MustCompile(`\(\s*t\s*[,)]|,\s*t\s*\)\s*=>|\b(?:const|let|var)\s+t\b`).Find(app); m != nil {
		t.Errorf("app.js names something t, which hides the translation: %q", m)
	}
	str := `"(?:[^"\\]|\\.)*"`
	es := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(`+str+`):\s*(?:\n\s*)?(`+str+`),`).FindAllStringSubmatch(string(table), -1) {
		key, _ := strconv.Unquote(m[1])
		es[key], _ = strconv.Unquote(m[2])
	}
	var texts []string
	for _, m := range regexp.MustCompile(`\bt\(\s*(`+str+`)`).FindAllStringSubmatch(string(app), -1) {
		texts = append(texts, m[1])
	}
	for _, m := range regexp.MustCompile(`\bt\(\s*[^,()"]+\?\s*(`+str+`)\s*:\s*(`+str+`)`).FindAllStringSubmatch(string(app), -1) {
		texts = append(texts, m[1], m[2])
	}
	for _, m := range regexp.MustCompile(`\bplural\([^,]+,\s*(`+str+`),\s*(`+str+`)`).FindAllStringSubmatch(string(app), -1) {
		texts = append(texts, m[1], m[2])
	}
	if len(texts) < 150 {
		t.Fatalf("found only %d texts: the patterns missed app.js", len(texts))
	}
	slots := regexp.MustCompile(`\{\w+\}`)
	for _, quoted := range texts {
		text, _ := strconv.Unquote(quoted)
		spanish, ok := es[text]
		switch {
		case !ok:
			t.Errorf("no Spanish for %q", text)
		case len(slots.FindAllString(text, -1)) != len(slots.FindAllString(spanish, -1)):
			t.Errorf("%q fills in other things in Spanish: %q", text, spanish)
		}
	}
}
