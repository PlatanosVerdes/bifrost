package access

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type linksReply struct {
	Created  string              `json:"created"`
	Joinable bool                `json:"joinable"`
	Links    map[string]joinLink `json:"links"`
	Groups   []accessList        `json:"groups"`
	Error    string              `json:"error"`
}

func (f *fakeCloudflare) joinAccess(t *testing.T) *Access {
	a := New(Config{Admins: "boss@example.com", StateDir: t.TempDir()})
	c := f.provider()
	c.joinAUD = "aud-join"
	a.provider, a.joinable = c, true
	return a
}

func sendLinks(t *testing.T, a *Access, method, token, body string) (int, linksReply) {
	t.Helper()
	r := httptest.NewRequest(method, "/api/access/links", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://tv.example.com")
	if token != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	w := httptest.NewRecorder()
	a.ServeLinks(w, r)
	var got linksReply
	json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got
}

func join(a *Access, token, link string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/join/"+link, nil)
	r.SetPathValue("token", link)
	if token != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	w := httptest.NewRecorder()
	a.ServeJoin(w, r)
	return w
}

func members(t *testing.T, f *fakeCloudflare, a *Access) []string {
	t.Helper()
	_, got, _ := get(t, a, f.token("boss@example.com"))
	return got.Groups[0].Members
}

func TestJoinLinkLetsOnePersonIn(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)

	code, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	if code != 200 || !got.Joinable || !strings.HasPrefix(got.Created, "https://tv.example.com/join/") || len(got.Links) != 1 {
		t.Fatalf("create: %d %+v", code, got)
	}
	link := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")
	for id, l := range got.Links {
		if l.Group != "g-own" || l.By != "boss@example.com" || strings.Contains(id, link) {
			t.Fatalf("pending link %s: %+v", id, l)
		}
	}

	// The sender checking the link is already on the list, so it stays usable.
	if w := join(a, f.tokenFor("ana@example.com", "aud-join"), link); w.Code != 303 {
		t.Fatalf("ana: %d %s", w.Code, w.Body)
	}
	if w := join(a, f.tokenFor("Bob@example.com", "aud-join"), link); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("bob: %d %s", w.Code, w.Body)
	}
	if m := members(t, f, a); !reflect.DeepEqual(m, []string{"ana@example.com", "bob@example.com"}) {
		t.Fatalf("members %v", m)
	}
	if by := a.invites.byTo["bob@example.com"].By; by != "boss@example.com" {
		t.Fatalf("bob credited to %q", by)
	}
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), link); w.Code != 303 {
		t.Fatalf("bob again: %d %s", w.Code, w.Body)
	}
	if w := join(a, f.tokenFor("eve@example.com", "aud-join"), link); w.Code != 410 {
		t.Fatalf("eve on a used link: %d %s", w.Code, w.Body)
	}
	if m := members(t, f, a); len(m) != 2 {
		t.Fatalf("members %v", m)
	}
}

func TestJoinNeedsTheJoinApp(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	_, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	link := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")

	for _, c := range []struct{ name, token string }{
		{"not signed in", ""},
		{"a token for the page's app", f.token("bob@example.com")},
		{"a token for another app", f.tokenFor("bob@example.com", "aud-photos")},
	} {
		if w := join(a, c.token, link); w.Code != 403 {
			t.Fatalf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), "not-a-link"); w.Code != 410 {
		t.Fatalf("unknown link: %d", w.Code)
	}
	if m := members(t, f, a); len(m) != 1 || len(f.puts) != 0 {
		t.Fatalf("members %v, %d PUTs", m, len(f.puts))
	}

	off := f.access("boss@example.com")
	if w := join(off, f.tokenFor("bob@example.com", "aud-join"), link); w.Code != 404 {
		t.Fatalf("without a join app: %d", w.Code)
	}
	if code, _ := sendLinks(t, off, "POST", f.token("boss@example.com"), `{"group":"g-own"}`); code != 404 {
		t.Fatalf("creating without a join app: %d", code)
	}
}

func TestJoinLinkAdminsOnly(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	if code, _ := sendLinks(t, a, "POST", f.token("ana@example.com"), `{"group":"g-own"}`); code != 403 {
		t.Fatalf("a viewer made a link: %d", code)
	}
	if code, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-family"}`); code != 404 || got.Created != "" {
		t.Fatalf("a link to a list another app uses: %d %+v", code, got)
	}
	if len(a.links.byID) != 0 {
		t.Fatalf("links made: %v", a.links.byID)
	}
}

func TestJoinLinkRevokedAndExpired(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	_, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	revoked := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")
	code, got := sendLinks(t, a, "DELETE", f.token("boss@example.com"), `{"id":"`+linkID(revoked)+`"}`)
	if code != 200 || len(got.Links) != 0 {
		t.Fatalf("revoke: %d %+v", code, got)
	}
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), revoked); w.Code != 410 {
		t.Fatalf("revoked link: %d", w.Code)
	}

	old := a.links.create("g-own", "boss@example.com", time.Now().Add(-joinLinkTTL-time.Minute))
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), old); w.Code != 410 {
		t.Fatalf("expired link: %d", w.Code)
	}
	if p := a.links.pending(time.Now()); len(p) != 0 {
		t.Fatalf("expired link still pending: %v", p)
	}
}

func TestJoinLinksKeepOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	l := loadJoinLinks(dir)
	token := l.create("g-own", "boss@example.com", time.Now())
	b, err := os.ReadFile(filepath.Join(dir, "join-links.json"))
	if err != nil || strings.Contains(string(b), token) {
		t.Fatalf("%v, file %s", err, b)
	}
	if _, ok := loadJoinLinks(dir).find(token, time.Now()); !ok {
		t.Fatal("not kept across a restart")
	}
}

// Anyone can sign in to the join app, so a wrong link must cost no Cloudflare call.
func TestJoinMissesCallNoAPI(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	_, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	link := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), link); w.Code != 303 {
		t.Fatalf("bob: %d", w.Code)
	}
	before := f.calls.Load()
	for _, l := range []string{"x", strings.Repeat("a", 32), link} {
		if w := join(a, f.tokenFor("eve@example.com", "aud-join"), l); w.Code != 410 {
			t.Fatalf("eve on %q: %d", l, w.Code)
		}
	}
	if w := join(a, f.tokenFor("BOB@example.com", "aud-join"), link); w.Code != 303 {
		t.Fatalf("bob reopening: %d", w.Code)
	}
	if n := f.calls.Load() - before; n != 0 {
		t.Fatalf("%d Cloudflare calls", n)
	}
}

func TestJoinTokenIsNoViewer(t *testing.T) {
	f := newFakeCloudflare(t)
	a := New(Config{Admins: "boss@example.com"})
	c := f.provider()
	c.joinAUD = "aud-join"
	a.provider, a.joinable = c, true
	tok := f.tokenFor("boss@example.com", "aud-join")
	if code, got, raw := get(t, a, tok); code != 200 || got.Admin || *got.Me != "" {
		t.Fatalf("api/access: %d %s", code, raw)
	}
	if code, _ := sendLinks(t, a, "POST", tok, `{"group":"g-own"}`); code != 403 {
		t.Fatalf("made a link with a join token: %d", code)
	}
}

// A link that cannot be marked used on disk lets nobody in: a restart would hand it out again.
func TestJoinFailsClosedWhenTheClaimIsNotSaved(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	_, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	link := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")
	dir := filepath.Dir(a.links.path)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), link); w.Code != 503 {
		t.Fatalf("bob: %d %s", w.Code, w.Body)
	}
	if len(f.puts) != 0 {
		t.Fatal("bob was added")
	}
	os.Chmod(dir, 0o700)
	if w := join(a, f.tokenFor("bob@example.com", "aud-join"), link); w.Code != 303 || len(f.puts) != 1 {
		t.Fatalf("retry: %d, %d PUTs", w.Code, len(f.puts))
	}
}

func TestJoinLinkUsedBySomeoneAlreadyOnTheListStaysUnused(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.joinAccess(t)
	_, got := sendLinks(t, a, "POST", f.token("boss@example.com"), `{"group":"g-own"}`)
	link := strings.TrimPrefix(got.Created, "https://tv.example.com/join/")
	if w := join(a, f.tokenFor("ana@example.com", "aud-join"), link); w.Code != 303 {
		t.Fatalf("ana: %d", w.Code)
	}
	if p := a.links.pending(time.Now()); len(p) != 1 {
		t.Fatalf("pending %v", p)
	}
	if reloaded, _ := loadJoinLinks(filepath.Dir(a.links.path)).find(link, time.Now()); reloaded.UsedBy != "" {
		t.Fatalf("on disk: %+v", reloaded)
	}
}
