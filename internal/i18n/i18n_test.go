package i18n

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestLanguageFollowsTheBrowser(t *testing.T) {
	for header, want := range map[string]string{"es-ES,es;q=0.9,en;q=0.8": "es", "es": "es", "en-GB,es;q=0.5": "en", "": "en"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Language", header)
		if got := Lang(r); got != want {
			t.Errorf("Accept-Language %q: %s, want %s", header, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Language", "es")
	if got := T(r, "Source %d did not start", 3); got != "La fuente 3 no arrancó" {
		t.Errorf("filled in: %q", got)
	}
	if got := T(r, "a text nobody translated"); got != "a text nobody translated" {
		t.Errorf("untranslated: %q", got)
	}
}

// Every text the server hands to T, or to the join page that passes its texts through T, has its
// Spanish, with the same things filled in.
func TestEveryTextHasItsSpanish(t *testing.T) {
	verbs := regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)
	seen := 0
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var texts []ast.Expr
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "i18n" && fn.Sel.Name == "T" && len(call.Args) > 1 {
					texts = call.Args[1:2]
				}
			case *ast.Ident:
				if fn.Name == "joinPage" && len(call.Args) == 5 {
					texts = call.Args[3:]
				}
			}
			for _, arg := range texts {
				lit, ok := arg.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				text, _ := strconv.Unquote(lit.Value)
				if text == "" {
					continue
				}
				seen++
				es, ok := es[text]
				switch {
				case !ok:
					t.Errorf("%s: no Spanish for %q", path, text)
				case strings.Join(verbs.FindAllString(text, -1), " ") != strings.Join(verbs.FindAllString(es, -1), " "):
					t.Errorf("%q fills in other things in Spanish: %q", text, es)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 40 {
		t.Fatalf("found only %d texts: the walk missed the handlers", seen)
	}
}
