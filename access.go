package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"
)

// Who may open the page is kept by the access provider; bifrost only edits lists no other app uses.
type accessProvider interface {
	// An error means nobody: the identity counts only when the provider's signature proves it.
	viewer(r *http.Request) (viewer, error)
	// joiner is anyone signed in through the join app, on a list or not.
	joiner(r *http.Request) (viewer, error)
	site(ctx context.Context, v viewer) (site, error)
	add(ctx context.Context, v viewer, list string, emails []string) ([]string, error)
	remove(ctx context.Context, v viewer, list, email string) error
}

type viewer struct {
	email string
}

type site struct {
	link    string   // what to send people
	origins []string // scheme://host the page is opened from, for the CSRF check
	lists   []accessList
}

type accessList struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Editable bool     `json:"editable"` // no other app uses it, so bifrost may change it
	Members  []string `json:"members"`
}

var errNotEditable = errors.New("That list cannot be edited from bifrost")

var errLastMember = errors.New("A list cannot be left empty: add someone else first")

// errNoToken is a request the provider did not vouch for at all, such as one from inside the house.
var errNoToken = errors.New("no token from the access provider")

var errNoJoin = errors.New("no join app is set up")

type access struct {
	provider accessProvider  // nil: sharing is off
	admins   map[string]bool // BIFROST_ADMINS: cannot be changed from the page
	granted  *adminRoles
	invites  *inviteLog
	links    *joinLinks
	joinable bool       // a join app lets people sign in before they are on a list
	joinMu   sync.Mutex // two people on one link would both get in
	rejected quietLog
}

// quietLog prints at most one line a minute: whoever can reach bifrost can send it bad tokens.
type quietLog struct {
	mu      sync.Mutex
	last    time.Time
	skipped int
}

func (q *quietLog) printf(format string, args ...any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if time.Since(q.last) < time.Minute {
		q.skipped++
		return
	}
	msg := fmt.Sprintf(format, args...)
	if q.skipped > 0 {
		msg += fmt.Sprintf(" (and %d more since the last line)", q.skipped)
	}
	log.Print(msg)
	q.last, q.skipped = time.Now(), 0
}

func newAccess(cfg config) *access {
	a := &access{admins: map[string]bool{}, granted: loadAdminRoles(cfg.stateDir), invites: loadInvites(cfg.stateDir), links: loadJoinLinks(cfg.stateDir)}
	for _, e := range strings.Split(cfg.admins, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.admins[e] = true
		}
	}
	switch {
	case cfg.accessTeamDomain != "" && cfg.accessAUD != "" && cfg.cloudflareAccountID != "" && cfg.cloudflareAPIToken != "":
		c := newCloudflareAccess(cfg.accessTeamDomain, cfg.accessAUD, cfg.cloudflareAccountID, cfg.cloudflareAPIToken)
		c.joinAUD, a.joinable = cfg.accessJoinAUD, cfg.accessJoinAUD != ""
		a.provider = c
		go c.keepWarm()
		log.Printf("[access] sharing through Cloudflare Access %s, %d admins, invite links %v", cfg.accessTeamDomain, len(a.admins), a.joinable)
	case cfg.accessTeamDomain != "":
		log.Printf("[access] sharing is off: ACCESS_TEAM_DOMAIN is set but ACCESS_AUD, CLOUDFLARE_ACCOUNT_ID or CLOUDFLARE_API_TOKEN is not")
	}
	return a
}

func (a *access) who(r *http.Request) (viewer, bool) {
	v, err := a.provider.viewer(r)
	if err != nil {
		if !errors.Is(err, errNoToken) {
			a.rejected.printf("[access] rejected a token: %v", err)
		}
		return viewer{}, false
	}
	return v, v.email != "" && (a.admins[strings.ToLower(v.email)] || a.granted.has(v.email))
}

func (a *access) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Use GET")
		return
	}
	if a.provider == nil {
		writeJSON(w, map[string]any{"enabled": false})
		return
	}
	v, admin := a.who(r)
	// The lists cost several Cloudflare round trips, so only the Share dialog asks for them.
	if !admin || r.URL.Query().Get("lists") == "" {
		writeJSON(w, map[string]any{"enabled": true, "me": v.email, "admin": admin})
		return
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, err)
		return
	}
	writeJSON(w, a.adminPayload(v, s))
}

// adminRequest checks a change to who has access; when it answers false it has already replied.
func (a *access) adminRequest(w http.ResponseWriter, r *http.Request) (viewer, site, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "Use POST, PATCH or DELETE")
		return viewer{}, site{}, false
	}
	if a.provider == nil {
		writeError(w, http.StatusNotFound, "Sharing is not set up on this bifrost")
		return viewer{}, site{}, false
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Send the change as JSON")
		return viewer{}, site{}, false
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return viewer{}, site{}, false
	}
	v, admin := a.who(r)
	if !admin {
		writeError(w, http.StatusForbidden, "Only an admin of this bifrost can change who has access")
		return viewer{}, site{}, false
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, err)
		return viewer{}, site{}, false
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.origins, strings.ToLower(o)) {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return viewer{}, site{}, false
	}
	return v, s, true
}

func (a *access) serveMembers(w http.ResponseWriter, r *http.Request) {
	v, s, ok := a.adminRequest(w, r)
	if !ok {
		return
	}
	var body struct {
		Group  string   `json:"group"`
		Emails []string `json:"emails"`
		Email  string   `json:"email"`
		Role   string   `json:"role"` // PATCH: admin or viewer
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "The change could not be read")
		return
	}
	var err error
	if r.Method == http.MethodPatch {
		a.serveRole(w, v, s, body.Email, body.Role)
		return
	}
	if r.Method == http.MethodPost {
		var emails []string
		if emails, err = cleanEmails(body.Emails); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(emails) == 0 {
			writeError(w, http.StatusBadRequest, "Give at least one email address")
			return
		}
		var added []string
		if added, err = a.provider.add(r.Context(), v, body.Group, emails); err == nil {
			a.invites.record(added, strings.ToLower(v.email), time.Now())
		}
	} else {
		var email []string
		if email, err = cleanEmails([]string{body.Email}); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if email[0] == strings.ToLower(v.email) {
			writeError(w, http.StatusConflict, "You cannot remove yourself: ask another admin")
			return
		}
		if err = a.provider.remove(r.Context(), v, body.Group, email[0]); err == nil {
			err = a.granted.grant(email[0], false)
		}
	}
	if err != nil {
		providerError(w, err)
		return
	}
	if s, err = a.provider.site(r.Context(), v); err != nil {
		providerError(w, err)
		return
	}
	a.invites.keep(s.lists)
	writeJSON(w, a.adminPayload(v, s))
}

func (a *access) serveLinks(w http.ResponseWriter, r *http.Request) {
	v, s, ok := a.adminRequest(w, r)
	if !ok {
		return
	}
	if !a.joinable {
		writeError(w, http.StatusNotFound, "Invite links are not set up on this bifrost")
		return
	}
	var body struct {
		Group string `json:"group"`
		ID    string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "The change could not be read")
		return
	}
	if r.Method == http.MethodDelete {
		a.links.revoke(body.ID)
		writeJSON(w, a.adminPayload(v, s))
		return
	}
	// Checked again when the link is used: the list may be shared with another app by then.
	if !slices.ContainsFunc(s.lists, func(l accessList) bool { return l.ID == body.Group && l.Editable }) {
		writeError(w, http.StatusNotFound, errNotEditable.Error())
		return
	}
	token := a.links.create(body.Group, strings.ToLower(v.email), time.Now())
	out := a.adminPayload(v, s)
	out["created"] = s.link + "/join/" + token
	writeJSON(w, out)
}

// The link stands in for an admin typing the address.
func (a *access) serveJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		joinPage(w, http.StatusMethodNotAllowed, "Open the link in a browser", "")
		return
	}
	if a.provider == nil || !a.joinable {
		joinPage(w, http.StatusNotFound, "This bifrost does not take invite links", "")
		return
	}
	v, err := a.provider.joiner(r)
	if err != nil {
		if !errors.Is(err, errNoToken) {
			a.rejected.printf("[access] rejected a join token: %v", err)
		}
		joinPage(w, http.StatusForbidden, "Sign in first", "Open the link from outside the house, where it asks you to sign in.")
		return
	}
	email, err := cleanEmails([]string{v.email})
	if err != nil {
		joinPage(w, http.StatusForbidden, "Sign in with an email address", "")
		return
	}
	token, now := r.PathValue("token"), time.Now()

	// Nothing before the lock calls the provider: anyone can sign in here and try tokens.
	gone := func() {
		joinPage(w, http.StatusGone, "This invite link no longer works", "Each link lets one person in and lasts a week. Ask whoever sent it for a new one.")
	}
	a.joinMu.Lock()
	defer a.joinMu.Unlock()
	link, ok := a.links.find(token, now)
	switch {
	case !ok:
		gone()
		return
	case link.UsedBy == email[0]:
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	case link.UsedBy != "":
		gone()
		return
	}
	if err := a.links.claim(token, email[0]); err != nil {
		log.Printf("[access] could not save join links: %v", err)
		joinPage(w, http.StatusServiceUnavailable, "Could not let you in just now", "Open the link again in a moment.")
		return
	}
	added, err := a.provider.add(r.Context(), v, link.Group, email)
	// Only a link that let someone new in is used up, so its sender opening it to check costs nothing.
	if err != nil || len(added) == 0 {
		if uerr := a.links.unclaim(token); uerr != nil {
			log.Printf("[access] could not save join links: %v", uerr)
		}
	}
	if errors.Is(err, errNotEditable) {
		joinPage(w, http.StatusNotFound, "This invite link no longer works", "The list it adds you to cannot be changed from bifrost any more.")
		return
	}
	if err != nil {
		log.Printf("[access] %v", err)
		// Not 502: Cloudflare replaces an origin's 502 with its own error page, hiding this one.
		joinPage(w, http.StatusServiceUnavailable, "Could not let you in just now", "Open the link again in a moment.")
		return
	}
	if len(added) > 0 {
		a.invites.record(added, link.By, now)
		log.Printf("[access] %s joined through a link from %s", added[0], link.By)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var joinTemplate = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>bifrost</title><style>
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 16px; box-sizing: border-box;
  font: 16px/1.5 system-ui, sans-serif; background: #f6f5f2; color: #1d1d1b; }
@media (prefers-color-scheme: dark) { body { background: #141413; color: #ecebe6; } p { color: #a9a8a2; } }
main { max-width: 420px; } h1 { font-size: 22px; font-weight: 600; margin: 0 0 8px; } p { margin: 0; color: #66655f; }
</style></head><body><main><h1>{{.Title}}</h1>{{with .Detail}}<p>{{.}}</p>{{end}}</main></body></html>
`))

func joinPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	joinTemplate.Execute(w, struct{ Title, Detail string }{title, detail})
}

func (a *access) serveRole(w http.ResponseWriter, v viewer, s site, raw, role string) {
	email, err := cleanEmails([]string{raw})
	switch {
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case role != "admin" && role != "viewer":
		writeError(w, http.StatusBadRequest, "The role is admin or viewer")
		return
	case a.admins[email[0]]:
		writeError(w, http.StatusConflict, "That admin is set in bifrost's settings, not from the page")
		return
	case email[0] == strings.ToLower(v.email):
		writeError(w, http.StatusConflict, "You cannot change your own role: ask another admin")
		return
	}
	member := false
	for _, l := range s.lists {
		member = member || l.Editable && slices.Contains(l.Members, email[0])
	}
	if !member {
		writeError(w, http.StatusNotFound, "Only someone on this page's members can be made an admin")
		return
	}
	if err := a.granted.grant(email[0], role == "admin"); err != nil {
		log.Printf("[access] could not save admins: %v", err)
		writeError(w, http.StatusServiceUnavailable, "Could not save the change. Try again in a moment")
		return
	}
	log.Printf("[access] %s set %s's role to %s", strings.ToLower(v.email), email[0], role)
	writeJSON(w, a.adminPayload(v, s))
}

func (a *access) adminPayload(v viewer, s site) map[string]any {
	owners := make([]string, 0, len(a.admins))
	for e := range a.admins {
		owners = append(owners, e)
	}
	slices.Sort(owners)
	out := map[string]any{"enabled": true, "me": v.email, "admin": true, "link": s.link, "groups": s.lists, "invites": a.invites.of(s.lists), "joinable": a.joinable,
		"owners": owners, "admins": a.granted.list()}
	if a.joinable {
		out["links"] = a.links.pending(time.Now())
	}
	return out
}

// One bad address rejects the whole batch, so a typo never half-invites.
func cleanEmails(in []string) ([]string, error) {
	var out []string
	for _, e := range in {
		e = strings.TrimSpace(e)
		// ParseAddress alone takes "Name <a@b>" and dotless domains, which are no use as a rule.
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
			return nil, fmt.Errorf("Not an email address: %s", e)
		}
		if e = strings.ToLower(e); !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func providerError(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotEditable) {
		writeError(w, http.StatusNotFound, errNotEditable.Error())
		return
	}
	if errors.Is(err, errLastMember) {
		writeError(w, http.StatusConflict, errLastMember.Error())
		return
	}
	log.Printf("[access] %v", err)
	writeError(w, http.StatusServiceUnavailable, "Could not reach the access provider. Try again in a moment")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
