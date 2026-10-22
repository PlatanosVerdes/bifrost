package main

import (
	"cmp"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testKeyOnce       sync.Once
	testKey, otherKey *rsa.PrivateKey
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	testKeyOnce.Do(func() {
		var err error
		if testKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if otherKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return testKey, otherKey
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func signJWT(key *rsa.PrivateKey, alg, kid string, claims map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": alg, "kid": kid, "typ": "JWT"})
	body, _ := json.Marshal(claims)
	in := b64(head) + "." + b64(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + b64(sig)
}

// fakeCloudflare is the Access API of one account and its team's certs endpoint, over TLS.
type fakeCloudflare struct {
	srv    *httptest.Server
	mu     sync.Mutex
	apps   [][]map[string]any // one slice per page
	pols   []map[string]any
	groups map[string]map[string]any
	puts   []map[string]any
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	key, _ := testKeys(t)
	f := &fakeCloudflare{
		apps: [][]map[string]any{
			{{
				"id": "app-tv", "domain": "tv.example.com", "aud": "aud-tv", "self_hosted_domains": []string{"tv.example.com", "tv.example.net/path"},
				"policies": []any{
					map[string]any{"id": "p-legacy", "include": []any{map[string]any{"group": map[string]any{"id": "g-own"}}}},
					map[string]any{"id": "p-family", "reusable": true},
				},
			}},
			{{
				"id": "app-photos", "domain": "photos.example.com", "aud": "aud-photos",
				"policies": []any{map[string]any{"id": "p-family", "reusable": true, "include": []any{map[string]any{"group": map[string]any{"id": "g-family"}}}}},
			}},
		},
		pols: []map[string]any{{"id": "p-family", "include": []any{map[string]any{"group": map[string]any{"id": "g-family"}}}}},
		groups: map[string]map[string]any{
			"g-own": {"id": "g-own", "name": "TV viewers", "is_default": false, "include": []any{
				map[string]any{"email": map[string]any{"email": "Ana@example.com"}},
				map[string]any{"email_domain": map[string]any{"domain": "example.org"}},
			}, "require": []any{map[string]any{"login_method": map[string]any{"id": "otp"}}}},
			"g-family": {"id": "g-family", "name": "Family", "include": []any{map[string]any{"email": map[string]any{"email": "dad@example.com"}}}},
		},
	}
	n, e := key.PublicKey.N.Bytes(), big.NewInt(int64(key.PublicKey.E)).Bytes()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cdn-cgi/access/certs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kid": "k1", "kty": "RSA", "alg": "RS256", "n": b64(n), "e": b64(e)}}})
	})
	api := "/client/v4/accounts/acc/access"
	ok := func(w http.ResponseWriter, result any, pages int) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result, "result_info": map[string]int{"total_pages": pages}})
	}
	mux.HandleFunc("GET "+api+"/apps", func(w http.ResponseWriter, r *http.Request) {
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		if page < 1 || page > len(f.apps) {
			ok(w, []any{}, len(f.apps))
			return
		}
		ok(w, f.apps[page-1], len(f.apps))
	})
	mux.HandleFunc("GET "+api+"/policies", func(w http.ResponseWriter, r *http.Request) { ok(w, f.pols, 1) })
	mux.HandleFunc("GET "+api+"/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		g, found := f.groups[r.PathValue("id")]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]string{"message": "not found"}}})
			return
		}
		ok(w, g, 0)
	})
	mux.HandleFunc("PUT "+api+"/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		var g map[string]any
		json.NewDecoder(r.Body).Decode(&g)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.puts = append(f.puts, g)
		stored := maps.Clone(g)
		stored["id"] = r.PathValue("id")
		f.groups[r.PathValue("id")] = stored
		ok(w, stored, 0)
	})
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/client/v4/") && r.Header.Get("Authorization") != "Bearer secret-token" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]string{"message": "bad token"}}})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCloudflare) team() string { return strings.TrimPrefix(f.srv.URL, "https://") }

func (f *fakeCloudflare) provider() *cloudflareAccess {
	c := newCloudflareAccess(f.team(), "aud-tv", "acc", "secret-token")
	c.api = f.srv.URL + "/client/v4"
	c.client = f.srv.Client()
	return c
}

func (f *fakeCloudflare) access(admins string) *access {
	a := newAccess(config{admins: admins})
	a.provider = f.provider()
	return a
}

func (f *fakeCloudflare) token(email string) string { return f.tokenFor(email, "aud-tv") }

func (f *fakeCloudflare) tokenFor(email, aud string) string {
	key, _ := testKeys(nil)
	return signJWT(key, "RS256", "k1", map[string]any{
		"iss": "https://" + f.team(), "aud": []string{aud}, "email": email, "exp": time.Now().Add(time.Hour).Unix(),
	})
}

func TestAccessTokens(t *testing.T) {
	f := newFakeCloudflare(t)
	key, other := testKeys(t)
	iss := "https://" + f.team()
	hour := time.Now().Add(time.Hour).Unix()
	claims := func(iss string, exp int64) map[string]any {
		return map[string]any{"iss": iss, "aud": "aud-tv", "email": "ana@example.com", "exp": exp}
	}
	forApp := func(aud any) map[string]any {
		return map[string]any{"iss": iss, "aud": aud, "email": "ana@example.com", "exp": hour}
	}
	for _, c := range []struct {
		name, token, want, err string
	}{
		{"valid", signJWT(key, "RS256", "k1", claims(iss, hour)), "ana@example.com", ""},
		{"one of several audiences", signJWT(key, "RS256", "k1", forApp([]string{"aud-photos", "aud-tv"})), "ana@example.com", ""},
		{"another app of the team", signJWT(key, "RS256", "k1", forApp([]string{"aud-photos"})), "", "audience"},
		{"no audience", signJWT(key, "RS256", "k1", forApp([]string{})), "", "audience"},
		{"expired", signJWT(key, "RS256", "k1", claims(iss, time.Now().Add(-time.Hour).Unix())), "", "expired"},
		{"no expiry", signJWT(key, "RS256", "k1", claims(iss, 0)), "", "expired"},
		{"another team", signJWT(key, "RS256", "k1", claims("https://evil.cloudflareaccess.com", hour)), "", "issuer"},
		{"signed by another key", signJWT(other, "RS256", "k1", claims(iss, hour)), "", "bad signature"},
		{"unknown kid", signJWT(key, "RS256", "k9", claims(iss, hour)), "", "unknown key"},
		{"alg none", signJWT(key, "none", "k1", claims(iss, hour)), "", "alg"},
		{"garbage", "a.b", "", "not a JWT"},
		{"no header", "", "", "no token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/access", nil)
			if c.token != "" {
				r.Header.Set("Cf-Access-Jwt-Assertion", c.token)
			}
			v, err := f.provider().viewer(r)
			if c.err == "" {
				if err != nil || v.email != c.want {
					t.Fatalf("got %+v, %v; want %s", v, err, c.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want an error with %q", err, c.err)
			}
		})
	}
}

type accessReply struct {
	Enabled *bool        `json:"enabled"`
	Me      *string      `json:"me"`
	Admin   bool         `json:"admin"`
	Link    string       `json:"link"`
	Groups  []accessList `json:"groups"`
	Error   string       `json:"error"`
}

func get(t *testing.T, a *access, token string) (int, accessReply, string) {
	r := httptest.NewRequest("GET", "/api/access", nil)
	if token != "" {
		r.Header.Set("Cf-Access-Jwt-Assertion", token)
	}
	w := httptest.NewRecorder()
	a.serve(w, r)
	var got accessReply
	json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got, w.Body.String()
}

func TestAccessGet(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.access(" Boss@Example.com ,other@example.com")

	if _, got, raw := get(t, newAccess(config{admins: "boss@example.com"}), f.token("boss@example.com")); got.Enabled == nil || *got.Enabled || strings.Contains(raw, "me") {
		t.Fatalf("disabled: %s", raw)
	}
	for _, c := range []struct{ name, token, me string }{
		{"not signed in", "", ""},
		{"an admin through another app of the team", f.tokenFor("boss@example.com", "aud-photos"), ""},
		{"a viewer", f.token("ana@example.com"), "ana@example.com"},
	} {
		code, got, raw := get(t, a, c.token)
		if code != 200 || !*got.Enabled || got.Me == nil || *got.Me != c.me || got.Admin || strings.Contains(raw, "groups") || strings.Contains(raw, "link") {
			t.Fatalf("%s: %d %s", c.name, code, raw)
		}
	}

	code, got, raw := get(t, a, f.token("boss@example.com"))
	want := []accessList{
		{ID: "g-own", Name: "TV viewers", Editable: true, Members: []string{"ana@example.com"}},
		{ID: "g-family", Name: "Family", Editable: false, Members: []string{"dad@example.com"}},
	}
	if code != 200 || !got.Admin || got.Link != "https://tv.example.com" || !reflect.DeepEqual(got.Groups, want) {
		t.Fatalf("admin: %d %s", code, raw)
	}
}

func TestAccessMembers(t *testing.T) {
	const own = "https://tv.example.com"
	for _, c := range []struct {
		name        string
		method      string
		who         string
		contentType string
		origin      string
		body        string
		status      int
		err         string
		members     []string // of g-own afterwards
		puts        int
	}{
		{"add, deduped", "POST", "boss", "application/json", own, `{"group":"g-own","emails":[" bob@example.com","BOB@example.com","ana@EXAMPLE.com"]}`, 200, "", []string{"ana@example.com", "bob@example.com"}, 1},
		{"add from another domain of the app", "POST", "boss", "application/json; charset=utf-8", "https://tv.example.net", `{"group":"g-own","emails":["bob@example.com"]}`, 200, "", []string{"ana@example.com", "bob@example.com"}, 1},
		{"add without Origin", "POST", "boss", "application/json", "", `{"group":"g-own","emails":["bob@example.com"]}`, 200, "", []string{"ana@example.com", "bob@example.com"}, 1},
		{"add someone already on", "POST", "boss", "application/json", own, `{"group":"g-own","emails":["ANA@example.com"]}`, 200, "", []string{"ana@example.com"}, 0},
		{"one bad address spoils all", "POST", "boss", "application/json", own, `{"group":"g-own","emails":["bob@example.com","nope"]}`, 400, "Not an email address: nope", []string{"ana@example.com"}, 0},
		{"a display name is not an address", "POST", "boss", "application/json", own, `{"group":"g-own","emails":["Bob <bob@example.com>"]}`, 400, "Not an email address: Bob <bob@example.com>", []string{"ana@example.com"}, 0},
		{"nothing to add", "POST", "boss", "application/json", own, `{"group":"g-own","emails":[]}`, 400, "Give at least one email address", []string{"ana@example.com"}, 0},
		{"a group another app uses", "POST", "boss", "application/json", own, `{"group":"g-family","emails":["bob@example.com"]}`, 404, "That list cannot be edited from bifrost", []string{"ana@example.com"}, 0},
		{"a group not on this app", "POST", "boss", "application/json", own, `{"group":"g-elsewhere","emails":["bob@example.com"]}`, 404, "That list cannot be edited from bifrost", []string{"ana@example.com"}, 0},
		{"a viewer", "POST", "ana", "application/json", own, `{"group":"g-own","emails":["bob@example.com"]}`, 403, "Only an admin", []string{"ana@example.com"}, 0},
		{"an admin through another app of the team", "POST", "boss@photos", "application/json", own, `{"group":"g-own","emails":["bob@example.com"]}`, 403, "Only an admin", []string{"ana@example.com"}, 0},
		{"not signed in", "POST", "", "application/json", own, `{"group":"g-own","emails":["bob@example.com"]}`, 403, "Only an admin", []string{"ana@example.com"}, 0},
		{"a form post", "POST", "boss", "application/x-www-form-urlencoded", own, `{"group":"g-own","emails":["bob@example.com"]}`, 415, "JSON", []string{"ana@example.com"}, 0},
		{"another site", "POST", "boss", "application/json", "https://evil.example", `{"group":"g-own","emails":["bob@example.com"]}`, 403, "another site", []string{"ana@example.com"}, 0},
		{"remove", "DELETE", "boss", "application/json", own, `{"group":"g-own","email":"ANA@example.com"}`, 200, "", []string{}, 1},
		{"remove someone not on it", "DELETE", "boss", "application/json", own, `{"group":"g-own","email":"zoe@example.com"}`, 200, "", []string{"ana@example.com"}, 0},
		{"remove from a group another app uses", "DELETE", "boss", "application/json", own, `{"group":"g-family","email":"dad@example.com"}`, 404, "That list cannot be edited from bifrost", []string{"ana@example.com"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeCloudflare(t)
			a := f.access("boss@example.com")
			r := httptest.NewRequest(c.method, "/api/access/members", strings.NewReader(c.body))
			r.Header.Set("Content-Type", c.contentType)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if who, app, _ := strings.Cut(c.who, "@"); who != "" {
				r.Header.Set("Cf-Access-Jwt-Assertion", f.tokenFor(who+"@example.com", "aud-"+cmp.Or(app, "tv")))
			}
			w := httptest.NewRecorder()
			a.serveMembers(w, r)
			var got accessReply
			json.Unmarshal(w.Body.Bytes(), &got)
			if w.Code != c.status || (c.err == "") != (got.Error == "") || !strings.Contains(got.Error, c.err) {
				t.Fatalf("got %d %s, want %d %q", w.Code, w.Body, c.status, c.err)
			}
			if c.status == 200 && (!got.Admin || got.Link == "" || len(got.Groups) != 2 || !reflect.DeepEqual(got.Groups[0].Members, c.members)) {
				t.Fatalf("reply %s, want g-own members %v", w.Body, c.members)
			}
			if len(f.puts) != c.puts {
				t.Fatalf("%d PUTs, want %d", len(f.puts), c.puts)
			}
			_, after, _ := get(t, a, f.token("boss@example.com"))
			if !reflect.DeepEqual(after.Groups[0].Members, c.members) {
				t.Fatalf("g-own now has %v, want %v", after.Groups[0].Members, c.members)
			}
		})
	}
}

// Rules bifrost does not show (a whole domain, a login method) go back to Cloudflare untouched.
func TestAccessEditKeepsOtherRules(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.access("boss@example.com")
	r := httptest.NewRequest("POST", "/api/access/members", strings.NewReader(`{"group":"g-own","emails":["bob@example.com"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.token("boss@example.com"))
	a.serveMembers(httptest.NewRecorder(), r)
	if len(f.puts) != 1 {
		t.Fatalf("%d PUTs", len(f.puts))
	}
	put, _ := json.Marshal(f.puts[0])
	want := `{"include":[{"email":{"email":"Ana@example.com"}},{"email_domain":{"domain":"example.org"}},{"email":{"email":"bob@example.com"}}],"is_default":false,"name":"TV viewers","require":[{"login_method":{"id":"otp"}}]}`
	if string(put) != want {
		t.Fatalf("PUT\n%s\nwant\n%s", put, want)
	}
}

func TestAccessRefusesToEmptyAGroup(t *testing.T) {
	f := newFakeCloudflare(t)
	f.groups["g-own"]["include"] = []any{map[string]any{"email": map[string]any{"email": "ana@example.com"}}}
	a := f.access("boss@example.com")
	r := httptest.NewRequest("DELETE", "/api/access/members", strings.NewReader(`{"group":"g-own","email":"ana@example.com"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.token("boss@example.com"))
	w := httptest.NewRecorder()
	a.serveMembers(w, r)
	if w.Code != http.StatusConflict || len(f.puts) != 0 {
		t.Fatalf("got %d %s, %d PUTs", w.Code, w.Body, len(f.puts))
	}
}

func TestAccessOffWithoutConfig(t *testing.T) {
	if newAccess(config{accessTeamDomain: "t", accessAUD: "a", cloudflareAccountID: "acc", cloudflareAPIToken: "tok"}).provider == nil {
		t.Fatal("sharing off with everything set")
	}
	for _, cfg := range []config{
		{},
		{accessTeamDomain: "example.cloudflareaccess.com", cloudflareAccountID: "acc", cloudflareAPIToken: "tok"},
		{accessTeamDomain: "example.cloudflareaccess.com", accessAUD: "aud", cloudflareAccountID: "acc"},
		{accessAUD: "aud", cloudflareAccountID: "acc", cloudflareAPIToken: "tok"},
	} {
		a := newAccess(cfg)
		if a.provider != nil {
			t.Fatalf("%+v turned sharing on", cfg)
		}
		r := httptest.NewRequest("POST", "/api/access/members", strings.NewReader(`{}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.serveMembers(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("members with sharing off: %d", w.Code)
		}
	}
}

func TestCloudflareAPIErrorsAreReported(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.access("boss@example.com")
	a.provider.(*cloudflareAccess).token = "wrong"
	code, got, _ := get(t, a, f.token("boss@example.com"))
	if code != http.StatusBadGateway || got.Error == "" || strings.Contains(got.Error, "wrong") {
		t.Fatalf("got %d %q", code, got.Error)
	}
}

func TestInviteLog(t *testing.T) {
	f := newFakeCloudflare(t)
	dir := t.TempDir()
	a := newAccess(config{admins: "boss@example.com,deputy@example.com", stateDir: dir})
	a.provider = f.provider()
	send := func(method, who, body string) map[string]invite {
		t.Helper()
		r := httptest.NewRequest(method, "/api/access/members", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Cf-Access-Jwt-Assertion", f.token(who))
		w := httptest.NewRecorder()
		a.serveMembers(w, r)
		var reply struct {
			Invites map[string]invite `json:"invites"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
		return reply.Invites
	}

	got := send("POST", "boss@example.com", `{"group":"g-own","emails":["bob@example.com"]}`)
	if got["bob@example.com"].By != "boss@example.com" || got["bob@example.com"].At.IsZero() {
		t.Fatalf("bob's invite: %+v", got)
	}
	if _, ok := got["ana@example.com"]; ok {
		t.Fatal("ana was added outside bifrost, so nobody invited her here")
	}
	got = send("POST", "deputy@example.com", `{"group":"g-own","emails":["BOB@example.com"]}`)
	if got["bob@example.com"].By != "boss@example.com" {
		t.Fatalf("inviting again replaced the first invite: %+v", got["bob@example.com"])
	}
	if reloaded := loadInvites(dir).byTo["bob@example.com"]; reloaded.By != "boss@example.com" {
		t.Fatalf("not kept across a restart: %+v", reloaded)
	}
	if got = send("DELETE", "boss@example.com", `{"group":"g-own","email":"bob@example.com"}`); len(got) != 0 {
		t.Fatalf("removed but still listed: %+v", got)
	}
	if _, ok := loadInvites(dir).byTo["bob@example.com"]; ok {
		t.Fatal("removed but still on disk")
	}
}

// Only people the change really added are credited: ana was put on the list in the dashboard.
func TestInviteLogCreditsOnlyNewMembers(t *testing.T) {
	f := newFakeCloudflare(t)
	a := f.access("boss@example.com")
	r := httptest.NewRequest("POST", "/api/access/members", strings.NewReader(`{"group":"g-own","emails":["ANA@example.com","bob@example.com"]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.token("boss@example.com"))
	w := httptest.NewRecorder()
	a.serveMembers(w, r)
	var reply struct {
		Invites map[string]invite `json:"invites"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &reply) != nil {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, ok := reply.Invites["ana@example.com"]; ok || reply.Invites["bob@example.com"].By != "boss@example.com" {
		t.Fatalf("invites %+v, want only bob", reply.Invites)
	}
}

func TestInviteLogKeepsWhoIsStillOnAList(t *testing.T) {
	f := newFakeCloudflare(t)
	dir := t.TempDir()
	stale := map[string]invite{
		"dad@example.com":  {By: "boss@example.com", At: time.Now().UTC()},
		"gone@example.com": {By: "boss@example.com", At: time.Now().UTC()},
	}
	b, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, "invites.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.groups["g-own"]["include"] = append(f.groups["g-own"]["include"].([]any), map[string]any{"email": map[string]any{"email": "dad@example.com"}})
	a := newAccess(config{admins: "boss@example.com", stateDir: dir})
	a.provider = f.provider()
	r := httptest.NewRequest("DELETE", "/api/access/members", strings.NewReader(`{"group":"g-own","email":"dad@example.com"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cf-Access-Jwt-Assertion", f.token("boss@example.com"))
	w := httptest.NewRecorder()
	a.serveMembers(w, r)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got := loadInvites(dir).byTo
	if _, ok := got["dad@example.com"]; !ok {
		t.Fatal("dad left g-own but is still on Family, yet his invite was forgotten")
	}
	if _, ok := got["gone@example.com"]; ok {
		t.Fatal("gone@ is on no list any more and should have been dropped")
	}
}
