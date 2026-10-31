// Package access decides who may open the page and lets admins share it through Cloudflare Access.
package access

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

	"github.com/PlatanosVerdes/bifrost/internal/httpx"
	"github.com/PlatanosVerdes/bifrost/internal/i18n"
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

type Access struct {
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

// Config turns sharing on when the team domain, app AUD tag, account and token are all set.
type Config struct {
	StateDir   string
	Admins     string // comma-separated emails that may change who has access
	TeamDomain string
	AUD        string
	JoinAUD    string // the app that lets anyone sign in on /join; empty: no invite links
	AccountID  string
	APIToken   string
}

func New(cfg Config) *Access {
	a := &Access{admins: map[string]bool{}, granted: loadAdminRoles(cfg.StateDir), invites: loadInvites(cfg.StateDir), links: loadJoinLinks(cfg.StateDir)}
	for _, e := range strings.Split(cfg.Admins, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.admins[e] = true
		}
	}
	switch {
	case cfg.TeamDomain != "" && cfg.AUD != "" && cfg.AccountID != "" && cfg.APIToken != "":
		c := newCloudflareAccess(cfg.TeamDomain, cfg.AUD, cfg.AccountID, cfg.APIToken)
		c.joinAUD, a.joinable = cfg.JoinAUD, cfg.JoinAUD != ""
		a.provider = c
		go c.keepWarm()
		log.Printf("[access] sharing through Cloudflare Access %s, %d admins, invite links %v", cfg.TeamDomain, len(a.admins), a.joinable)
	case cfg.TeamDomain != "":
		log.Printf("[access] sharing is off: ACCESS_TEAM_DOMAIN is set but ACCESS_AUD, CLOUDFLARE_ACCOUNT_ID or CLOUDFLARE_API_TOKEN is not")
	}
	return a
}

func (a *Access) who(r *http.Request) (viewer, bool) {
	v, err := a.provider.viewer(r)
	if err != nil {
		if !errors.Is(err, errNoToken) {
			a.rejected.printf("[access] rejected a token: %v", err)
		}
		return viewer{}, false
	}
	return v, v.email != "" && (a.admins[strings.ToLower(v.email)] || a.granted.has(v.email))
}

func (a *Access) Serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.WriteError(w, http.StatusMethodNotAllowed, i18n.T(r, "Use GET"))
		return
	}
	if a.provider == nil {
		httpx.WriteJSON(w, map[string]any{"enabled": false})
		return
	}
	v, admin := a.who(r)
	// The lists cost several Cloudflare round trips, so only the Share dialog asks for them.
	if !admin || r.URL.Query().Get("lists") == "" {
		httpx.WriteJSON(w, map[string]any{"enabled": true, "me": v.email, "admin": admin})
		return
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, r, err)
		return
	}
	httpx.WriteJSON(w, a.adminPayload(v, s))
}

// adminRequest checks a change to who has access; when it answers false it has already replied.
func (a *Access) adminRequest(w http.ResponseWriter, r *http.Request) (viewer, site, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete && r.Method != http.MethodPatch {
		httpx.WriteError(w, http.StatusMethodNotAllowed, i18n.T(r, "Use POST, PATCH or DELETE"))
		return viewer{}, site{}, false
	}
	if a.provider == nil {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, "Sharing is not set up on this bifrost"))
		return viewer{}, site{}, false
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		httpx.WriteError(w, http.StatusUnsupportedMediaType, i18n.T(r, "Send the change as JSON"))
		return viewer{}, site{}, false
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		httpx.WriteError(w, http.StatusForbidden, i18n.T(r, "That request came from another site"))
		return viewer{}, site{}, false
	}
	v, admin := a.who(r)
	if !admin {
		httpx.WriteError(w, http.StatusForbidden, i18n.T(r, "Only an admin of this bifrost can change who has access"))
		return viewer{}, site{}, false
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, r, err)
		return viewer{}, site{}, false
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.origins, strings.ToLower(o)) {
		httpx.WriteError(w, http.StatusForbidden, i18n.T(r, "That request came from another site"))
		return viewer{}, site{}, false
	}
	return v, s, true
}

func (a *Access) ServeMembers(w http.ResponseWriter, r *http.Request) {
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
		httpx.WriteError(w, http.StatusBadRequest, i18n.T(r, "The change could not be read"))
		return
	}
	var err error
	if r.Method == http.MethodPatch {
		a.serveRole(w, r, v, s, body.Email, body.Role)
		return
	}
	if r.Method == http.MethodPost {
		var emails []string
		if emails, err = cleanEmails(body.Emails); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, emailError(r, err))
			return
		}
		if len(emails) == 0 {
			httpx.WriteError(w, http.StatusBadRequest, i18n.T(r, "Give at least one email address"))
			return
		}
		var added []string
		if added, err = a.provider.add(r.Context(), v, body.Group, emails); err == nil {
			a.invites.record(added, strings.ToLower(v.email), time.Now())
		}
	} else {
		var email []string
		if email, err = cleanEmails([]string{body.Email}); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, emailError(r, err))
			return
		}
		if email[0] == strings.ToLower(v.email) {
			httpx.WriteError(w, http.StatusConflict, i18n.T(r, "You cannot remove yourself: ask another admin"))
			return
		}
		if err = a.provider.remove(r.Context(), v, body.Group, email[0]); err == nil {
			err = a.granted.grant(email[0], false)
		}
	}
	if err != nil {
		providerError(w, r, err)
		return
	}
	if s, err = a.provider.site(r.Context(), v); err != nil {
		providerError(w, r, err)
		return
	}
	a.invites.keep(s.lists)
	httpx.WriteJSON(w, a.adminPayload(v, s))
}

func (a *Access) ServeLinks(w http.ResponseWriter, r *http.Request) {
	v, s, ok := a.adminRequest(w, r)
	if !ok {
		return
	}
	if !a.joinable {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, "Invite links are not set up on this bifrost"))
		return
	}
	var body struct {
		Group string `json:"group"`
		ID    string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, i18n.T(r, "The change could not be read"))
		return
	}
	if r.Method == http.MethodDelete {
		a.links.revoke(body.ID)
		httpx.WriteJSON(w, a.adminPayload(v, s))
		return
	}
	// Checked again when the link is used: the list may be shared with another app by then.
	if !slices.ContainsFunc(s.lists, func(l accessList) bool { return l.ID == body.Group && l.Editable }) {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, errNotEditable.Error()))
		return
	}
	token := a.links.create(body.Group, strings.ToLower(v.email), time.Now())
	out := a.adminPayload(v, s)
	out["created"] = s.link + "/join/" + token
	httpx.WriteJSON(w, out)
}

// The link stands in for an admin typing the address.
func (a *Access) ServeJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		joinPage(w, r, http.StatusMethodNotAllowed, "Open the link in a browser", "")
		return
	}
	if a.provider == nil || !a.joinable {
		joinPage(w, r, http.StatusNotFound, "This bifrost does not take invite links", "")
		return
	}
	v, err := a.provider.joiner(r)
	if err != nil {
		if !errors.Is(err, errNoToken) {
			a.rejected.printf("[access] rejected a join token: %v", err)
		}
		joinPage(w, r, http.StatusForbidden, "Sign in first", "Open the link from outside the house, where it asks you to sign in.")
		return
	}
	email, err := cleanEmails([]string{v.email})
	if err != nil {
		joinPage(w, r, http.StatusForbidden, "Sign in with an email address", "")
		return
	}
	token, now := r.PathValue("token"), time.Now()

	// Nothing before the lock calls the provider: anyone can sign in here and try tokens.
	gone := func() {
		joinPage(w, r, http.StatusGone, "This invite link no longer works", "Each link lets one person in and lasts a week. Ask whoever sent it for a new one.")
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
		joinPage(w, r, http.StatusServiceUnavailable, "Could not let you in just now", "Open the link again in a moment.")
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
		joinPage(w, r, http.StatusNotFound, "This invite link no longer works", "The list it adds you to cannot be changed from bifrost any more.")
		return
	}
	if err != nil {
		log.Printf("[access] %v", err)
		// Not 502: Cloudflare replaces an origin's 502 with its own error page, hiding this one.
		joinPage(w, r, http.StatusServiceUnavailable, "Could not let you in just now", "Open the link again in a moment.")
		return
	}
	if len(added) == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.invites.record(added, link.By, now)
	log.Printf("[access] %s joined through a link from %s", added[0], link.By)
	joined(w, r)
}

// joinedWait is how long the page waits before opening bifrost: Cloudflare turned the first visit
// away while it still had the list from before the join.
const joinedWait = 15

func joined(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	joinTemplate.Execute(w, joinView{Lang: i18n.Lang(r), Title: i18n.T(r, "You're in"),
		Detail: i18n.T(r, "Cloudflare takes a few seconds to let you through: bifrost opens by itself in %d s.", joinedWait),
		Open:   i18n.T(r, "Open bifrost"), Wait: joinedWait})
}

type joinView struct {
	Lang, Title, Detail, Open string
	Wait                      int // seconds before opening the page; 0 stays
}

var joinTemplate = template.Must(template.New("join").Parse(`<!doctype html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Wait}}<meta http-equiv="refresh" content="{{.Wait}};url=/">{{end}}<title>bifrost</title><style>
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 16px; box-sizing: border-box;
  font: 16px/1.5 system-ui, sans-serif; background: #f6f5f2; color: #1d1d1b; }
@media (prefers-color-scheme: dark) { body { background: #141413; color: #ecebe6; } p { color: #a9a8a2; } }
main { max-width: 420px; } h1 { font-size: 22px; font-weight: 600; margin: 0 0 8px; } p { margin: 0; color: #66655f; }
a { display: inline-block; margin-top: 16px; font-weight: 600; color: inherit; }
</style></head><body><main><h1>{{.Title}}</h1>{{with .Detail}}<p>{{.}}</p>{{end}}{{with .Open}}<a href="/">{{.}}</a>{{end}}</main></body></html>
`))

func joinPage(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	joinTemplate.Execute(w, joinView{Lang: i18n.Lang(r), Title: i18n.T(r, title), Detail: i18n.T(r, detail)})
}

func (a *Access) serveRole(w http.ResponseWriter, r *http.Request, v viewer, s site, raw, role string) {
	email, err := cleanEmails([]string{raw})
	switch {
	case err != nil:
		httpx.WriteError(w, http.StatusBadRequest, emailError(r, err))
		return
	case role != "admin" && role != "viewer":
		httpx.WriteError(w, http.StatusBadRequest, i18n.T(r, "The role is admin or viewer"))
		return
	case a.admins[email[0]]:
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "That admin is set in bifrost's settings, not from the page"))
		return
	case email[0] == strings.ToLower(v.email):
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, "You cannot change your own role: ask another admin"))
		return
	}
	member := false
	for _, l := range s.lists {
		member = member || l.Editable && slices.Contains(l.Members, email[0])
	}
	if !member {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, "Only someone on this page's members can be made an admin"))
		return
	}
	if err := a.granted.grant(email[0], role == "admin"); err != nil {
		log.Printf("[access] could not save admins: %v", err)
		httpx.WriteError(w, http.StatusServiceUnavailable, i18n.T(r, "Could not save the change. Try again in a moment"))
		return
	}
	log.Printf("[access] %s set %s's role to %s", strings.ToLower(v.email), email[0], role)
	httpx.WriteJSON(w, a.adminPayload(v, s))
}

func (a *Access) adminPayload(v viewer, s site) map[string]any {
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

type badEmail string

func (e badEmail) Error() string { return "Not an email address: " + string(e) }

func emailError(r *http.Request, err error) string {
	var bad badEmail
	if errors.As(err, &bad) {
		return i18n.T(r, "Not an email address: %s", string(bad))
	}
	return err.Error()
}

// One bad address rejects the whole batch, so a typo never half-invites.
func cleanEmails(in []string) ([]string, error) {
	var out []string
	for _, e := range in {
		e = strings.TrimSpace(e)
		// ParseAddress alone takes "Name <a@b>" and dotless domains, which are no use as a rule.
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e || !strings.Contains(e[strings.LastIndex(e, "@"):], ".") {
			return nil, badEmail(e)
		}
		if e = strings.ToLower(e); !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func providerError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errNotEditable) {
		httpx.WriteError(w, http.StatusNotFound, i18n.T(r, errNotEditable.Error()))
		return
	}
	if errors.Is(err, errLastMember) {
		httpx.WriteError(w, http.StatusConflict, i18n.T(r, errLastMember.Error()))
		return
	}
	log.Printf("[access] %v", err)
	httpx.WriteError(w, http.StatusServiceUnavailable, i18n.T(r, "Could not reach the access provider. Try again in a moment"))
}
