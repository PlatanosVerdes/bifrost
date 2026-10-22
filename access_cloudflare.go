package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Cloudflare Access in front of bifrost. Every request it lets through carries a JWT signed by the
// team's keys and meant for this instance's app (ACCESS_AUD); that tag finds the app, its link and
// its lists (Access groups) through the API without knowing any names.

const (
	cfAPI          = "https://api.cloudflare.com/client/v4"
	cfSnapshotTTL  = 5 * time.Minute
	cfKeysMinFetch = 30 * time.Second // an unknown kid refetches the keys at most this often
	cfClockSkew    = 30 * time.Second
	cfMaxBody      = 8 << 20
)

type cloudflareAccess struct {
	team    string // example.cloudflareaccess.com
	aud     string // the app's Application Audience (AUD) tag
	account string
	token   string
	api     string
	client  *http.Client

	keysMu   sync.Mutex
	keys     map[string]*rsa.PublicKey
	keysAt   time.Time
	certsURL string

	mu   sync.Mutex
	snap cfSnapshot

	editMu sync.Mutex // a group edit is a read and a write; two at once would lose one
}

func newCloudflareAccess(team, aud, account, token string) *cloudflareAccess {
	team = strings.TrimSuffix(strings.TrimPrefix(team, "https://"), "/")
	return &cloudflareAccess{
		team: team, aud: aud, account: account, token: token, api: cfAPI,
		client:   &http.Client{Timeout: 10 * time.Second},
		certsURL: "https://" + team + "/cdn-cgi/access/certs",
		keys:     map[string]*rsa.PublicKey{},
	}
}

// --- who is looking: the JWT ---------------------------------------------------------------------

func (c *cloudflareAccess) viewer(r *http.Request) (viewer, error) {
	tok := r.Header.Get("Cf-Access-Jwt-Assertion")
	if tok == "" {
		return viewer{}, errNoToken
	}
	claims, err := c.verify(tok, time.Now())
	if err != nil {
		return viewer{}, err
	}
	return viewer{email: claims.Email}, nil
}

type cfClaims struct {
	Iss   string   `json:"iss"`
	Aud   audience `json:"aud"`
	Email string   `json:"email"`
	Exp   float64  `json:"exp"`
	Nbf   float64  `json:"nbf"`
}

// audience is a JWT aud: one string or a list of them.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(a))
}

func (c *cloudflareAccess) verify(tok string, now time.Time) (cfClaims, error) {
	var claims cfClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return claims, errors.New("not a JWT")
	}
	var head struct{ Alg, Kid string }
	if err := decodeSegment(parts[0], &head); err != nil {
		return claims, fmt.Errorf("header: %w", err)
	}
	if head.Alg != "RS256" {
		return claims, fmt.Errorf("alg %q, want RS256", head.Alg)
	}
	key, err := c.key(head.Kid)
	if err != nil {
		return claims, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, errors.New("signature is not base64url")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return claims, errors.New("bad signature")
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return claims, fmt.Errorf("claims: %w", err)
	}
	if claims.Iss != "https://"+c.team {
		return claims, fmt.Errorf("issuer %q is not this team", claims.Iss)
	}
	if claims.Exp == 0 || now.After(time.Unix(int64(claims.Exp), 0).Add(cfClockSkew)) {
		return claims, errors.New("expired")
	}
	if claims.Nbf != 0 && now.Add(cfClockSkew).Before(time.Unix(int64(claims.Nbf), 0)) {
		return claims, errors.New("not valid yet")
	}
	// Without this, a token another app of the team issued would pass, and bifrost is not only
	// reachable through Cloudflare.
	if !slices.Contains(claims.Aud, c.aud) {
		return claims, fmt.Errorf("audience %v is not this app", claims.Aud)
	}
	return claims, nil
}

func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// key returns the team's signing key by kid, fetching the keys again when Cloudflare has rotated them.
func (c *cloudflareAccess) key(kid string) (*rsa.PublicKey, error) {
	c.keysMu.Lock()
	defer c.keysMu.Unlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	if time.Since(c.keysAt) < cfKeysMinFetch {
		return nil, fmt.Errorf("unknown key %q", kid)
	}
	c.keysAt = time.Now()
	keys, err := c.fetchKeys()
	if err != nil {
		return nil, fmt.Errorf("team keys: %w", err)
	}
	c.keys = keys
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown key %q", kid)
}

func (c *cloudflareAccess) fetchKeys() (map[string]*rsa.PublicKey, error) {
	resp, err := c.client.Get(c.certsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", c.certsURL, resp.Status)
	}
	var set struct {
		Keys []struct{ Kid, Kty, N, E string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(k.N)
		e, errE := base64.RawURLEncoding.DecodeString(k.E)
		if errN != nil || errE != nil || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() >= 2048 {
			keys[k.Kid] = pub
		}
	}
	return keys, nil
}

// --- this instance's app and groups: the API ----------------------------------------------------

// cfSnapshot is what discovery found for one app. Group members are not kept: they are read fresh.
type cfSnapshot struct {
	link    string
	origins []string
	groups  []string        // ids in this app's include rules, in policy order
	shared  map[string]bool // also referenced by another app
	at      time.Time
}

type cfApp struct {
	ID                string     `json:"id"`
	Domain            string     `json:"domain"`
	SelfHostedDomains []string   `json:"self_hosted_domains"`
	AUD               string     `json:"aud"`
	Policies          []cfPolicy `json:"policies"`
}

type cfPolicy struct {
	ID      string            `json:"id"`
	Include []json.RawMessage `json:"include"`
	Exclude []json.RawMessage `json:"exclude"`
	Require []json.RawMessage `json:"require"`
}

type cfRule struct {
	Group *struct {
		ID string `json:"id"`
	} `json:"group"`
	Email *struct {
		Email string `json:"email"`
	} `json:"email"`
}

func parseRule(raw json.RawMessage) cfRule {
	var r cfRule
	_ = json.Unmarshal(raw, &r)
	return r
}

func ruleGroups(rules []json.RawMessage) []string {
	var ids []string
	for _, raw := range rules {
		if r := parseRule(raw); r.Group != nil && r.Group.ID != "" {
			ids = append(ids, r.Group.ID)
		}
	}
	return ids
}

// snapshot finds this instance's app and which of its groups other apps also use.
// Edits ask for a fresh one: a group attached to another app a minute ago is no longer ours.
func (c *cloudflareAccess) snapshot(ctx context.Context, fresh bool) (cfSnapshot, error) {
	c.mu.Lock()
	s := c.snap
	c.mu.Unlock()
	if !fresh && !s.at.IsZero() && time.Since(s.at) < cfSnapshotTTL {
		return s, nil
	}

	var apps []cfApp
	if err := c.list(ctx, "/access/apps", &apps); err != nil {
		return cfSnapshot{}, err
	}
	var reusable []cfPolicy
	if err := c.list(ctx, "/access/policies", &reusable); err != nil {
		return cfSnapshot{}, err
	}
	byID := map[string]cfPolicy{}
	for _, p := range reusable {
		byID[p.ID] = p
	}

	// An app lists its reusable policies by reference, sometimes without their rules.
	rules := func(p cfPolicy) cfPolicy {
		if len(p.Include)+len(p.Exclude)+len(p.Require) == 0 {
			if full, ok := byID[p.ID]; ok {
				return full
			}
		}
		return p
	}
	appsOf := map[string]map[string]bool{} // group id -> app ids
	var mine *cfApp
	for i, app := range apps {
		for _, p := range app.Policies {
			p = rules(p)
			for _, g := range append(append(ruleGroups(p.Include), ruleGroups(p.Exclude)...), ruleGroups(p.Require)...) {
				if appsOf[g] == nil {
					appsOf[g] = map[string]bool{}
				}
				appsOf[g][app.ID] = true
			}
		}
		if app.AUD == c.aud {
			mine = &apps[i]
		}
	}
	if mine == nil {
		return cfSnapshot{}, fmt.Errorf("no Access app in the account has the AUD tag %s", c.aud)
	}

	s = cfSnapshot{link: "https://" + mine.Domain, shared: map[string]bool{}, at: time.Now()}
	for _, d := range append([]string{mine.Domain}, mine.SelfHostedDomains...) {
		if host, _, _ := strings.Cut(d, "/"); host != "" {
			if o := "https://" + strings.ToLower(host); !slices.Contains(s.origins, o) {
				s.origins = append(s.origins, o)
			}
		}
	}
	for _, p := range mine.Policies {
		for _, g := range ruleGroups(rules(p).Include) {
			if !slices.Contains(s.groups, g) {
				s.groups = append(s.groups, g)
			}
		}
	}
	for g, users := range appsOf {
		for id := range users {
			if id != mine.ID {
				s.shared[g] = true
			}
		}
	}

	c.mu.Lock()
	c.snap = s
	c.mu.Unlock()
	return s, nil
}

func (c *cloudflareAccess) site(ctx context.Context, v viewer) (site, error) {
	s, err := c.snapshot(ctx, false)
	if err != nil {
		return site{}, err
	}
	out := site{link: s.link, origins: s.origins, lists: []accessList{}}
	for _, id := range s.groups {
		g, err := c.group(ctx, id)
		if err != nil {
			return site{}, err
		}
		out.lists = append(out.lists, accessList{ID: id, Name: g.name(), Editable: !s.shared[id], Members: g.emails()})
	}
	return out, nil
}

func (c *cloudflareAccess) add(ctx context.Context, v viewer, id string, emails []string) error {
	return c.edit(ctx, v, id, func(include []json.RawMessage) []json.RawMessage {
		have := map[string]bool{}
		for _, raw := range include {
			if r := parseRule(raw); r.Email != nil {
				have[strings.ToLower(r.Email.Email)] = true
			}
		}
		for _, e := range emails {
			if !have[e] {
				rule, _ := json.Marshal(map[string]any{"email": map[string]string{"email": e}})
				include = append(include, rule)
			}
		}
		return include
	})
}

func (c *cloudflareAccess) remove(ctx context.Context, v viewer, id, email string) error {
	return c.edit(ctx, v, id, func(include []json.RawMessage) []json.RawMessage {
		var kept []json.RawMessage
		for _, raw := range include {
			if r := parseRule(raw); r.Email == nil || strings.ToLower(r.Email.Email) != email {
				kept = append(kept, raw)
			}
		}
		return kept
	})
}

// edit rewrites one editable group's include rules, leaving every other field and rule kind as is.
func (c *cloudflareAccess) edit(ctx context.Context, v viewer, id string, change func([]json.RawMessage) []json.RawMessage) error {
	s, err := c.snapshot(ctx, true)
	if err != nil {
		return err
	}
	if !slices.Contains(s.groups, id) || s.shared[id] {
		return errNotEditable
	}
	c.editMu.Lock()
	defer c.editMu.Unlock()
	g, err := c.group(ctx, id)
	if err != nil {
		return err
	}
	before := len(g.include)
	after := change(append([]json.RawMessage{}, g.include...))
	if len(after) == before {
		return nil
	}
	if len(after) == 0 {
		return errLastMember // Cloudflare refuses a group with no include rule
	}
	body := map[string]any{"include": after}
	for _, k := range []string{"name", "exclude", "require", "is_default"} {
		if raw, ok := g.raw[k]; ok {
			body[k] = raw
		}
	}
	return c.call(ctx, http.MethodPut, "/access/groups/"+url.PathEscape(id), body, nil)
}

type cfGroup struct {
	raw     map[string]json.RawMessage
	include []json.RawMessage
}

func (g cfGroup) name() string {
	var n string
	_ = json.Unmarshal(g.raw["name"], &n)
	return n
}

func (g cfGroup) emails() []string {
	out := []string{}
	for _, raw := range g.include {
		if r := parseRule(raw); r.Email != nil {
			out = append(out, strings.ToLower(r.Email.Email))
		}
	}
	return out
}

func (c *cloudflareAccess) group(ctx context.Context, id string) (cfGroup, error) {
	var g cfGroup
	if err := c.call(ctx, http.MethodGet, "/access/groups/"+url.PathEscape(id), nil, &g.raw); err != nil {
		return g, err
	}
	if inc, ok := g.raw["include"]; ok {
		if err := json.Unmarshal(inc, &g.include); err != nil {
			return g, fmt.Errorf("group %s: %w", id, err)
		}
	}
	return g, nil
}

// list reads every page of an account listing.
func (c *cloudflareAccess) list(ctx context.Context, path string, into any) error {
	var all []json.RawMessage
	for page := 1; ; page++ {
		var got []json.RawMessage
		info, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d&per_page=100", path, page), nil, &got)
		if err != nil {
			return err
		}
		all = append(all, got...)
		if len(got) == 0 || info.TotalPages <= page {
			break
		}
	}
	b, _ := json.Marshal(all)
	return json.Unmarshal(b, into)
}

func (c *cloudflareAccess) call(ctx context.Context, method, path string, body, into any) error {
	_, err := c.do(ctx, method, path, body, into)
	return err
}

type cfResultInfo struct {
	TotalPages int `json:"total_pages"`
}

func (c *cloudflareAccess) do(ctx context.Context, method, path string, body, into any) (cfResultInfo, error) {
	var info cfResultInfo
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return info, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+"/accounts/"+url.PathEscape(c.account)+path, rd)
	if err != nil {
		return info, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	var env struct {
		Success    bool `json:"success"`
		Errors     []struct{ Message string }
		Result     json.RawMessage `json:"result"`
		ResultInfo cfResultInfo    `json:"result_info"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, cfMaxBody)).Decode(&env); err != nil {
		return info, fmt.Errorf("%s %s: %s, unreadable answer", method, path, resp.Status)
	}
	if !env.Success || resp.StatusCode >= 300 {
		msg := resp.Status
		if len(env.Errors) > 0 {
			msg += ": " + env.Errors[0].Message
		}
		return info, fmt.Errorf("%s %s: %s", method, path, msg)
	}
	if into != nil {
		if err := json.Unmarshal(env.Result, into); err != nil {
			return info, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return env.ResultInfo, nil
}
