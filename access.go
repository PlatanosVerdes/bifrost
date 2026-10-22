package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"
)

// Who may open the page is kept by the access provider; bifrost only edits lists no other app uses.
type accessProvider interface {
	// An error means nobody: the identity counts only when the provider's signature proves it.
	viewer(r *http.Request) (viewer, error)
	site(ctx context.Context, v viewer) (site, error)
	// add returns the emails that were not on the list yet.
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

type access struct {
	provider accessProvider // nil: sharing is off
	admins   map[string]bool
	invites  *inviteLog
}

func newAccess(cfg config) *access {
	a := &access{admins: map[string]bool{}, invites: loadInvites(cfg.stateDir)}
	for _, e := range strings.Split(cfg.admins, ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			a.admins[e] = true
		}
	}
	switch {
	case cfg.accessTeamDomain != "" && cfg.accessAUD != "" && cfg.cloudflareAccountID != "" && cfg.cloudflareAPIToken != "":
		a.provider = newCloudflareAccess(cfg.accessTeamDomain, cfg.accessAUD, cfg.cloudflareAccountID, cfg.cloudflareAPIToken)
		log.Printf("[access] sharing through Cloudflare Access %s, %d admins", cfg.accessTeamDomain, len(a.admins))
	case cfg.accessTeamDomain != "":
		log.Printf("[access] sharing is off: ACCESS_TEAM_DOMAIN is set but ACCESS_AUD, CLOUDFLARE_ACCOUNT_ID or CLOUDFLARE_API_TOKEN is not")
	}
	return a
}

func (a *access) who(r *http.Request) (viewer, bool) {
	v, err := a.provider.viewer(r)
	if err != nil {
		if !errors.Is(err, errNoToken) {
			log.Printf("[access] rejected a token: %v", err)
		}
		return viewer{}, false
	}
	return v, v.email != "" && a.admins[strings.ToLower(v.email)]
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
	if !admin {
		writeJSON(w, map[string]any{"enabled": true, "me": v.email, "admin": false})
		return
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, err)
		return
	}
	writeJSON(w, a.adminPayload(v, s))
}

func (a *access) serveMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "Use POST or DELETE")
		return
	}
	if a.provider == nil {
		writeError(w, http.StatusNotFound, "Sharing is not set up on this bifrost")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Send the change as JSON")
		return
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return
	}
	v, admin := a.who(r)
	if !admin {
		writeError(w, http.StatusForbidden, "Only an admin of this bifrost can change who has access")
		return
	}
	s, err := a.provider.site(r.Context(), v)
	if err != nil {
		providerError(w, err)
		return
	}
	if o := r.Header.Get("Origin"); o != "" && !slices.Contains(s.origins, strings.ToLower(o)) {
		writeError(w, http.StatusForbidden, "That request came from another site")
		return
	}

	var body struct {
		Group  string   `json:"group"`
		Emails []string `json:"emails"`
		Email  string   `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "The change could not be read")
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
		err = a.provider.remove(r.Context(), v, body.Group, email[0])
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

func (a *access) adminPayload(v viewer, s site) map[string]any {
	return map[string]any{"enabled": true, "me": v.email, "admin": true, "link": s.link, "groups": s.lists, "invites": a.invites.of(s.lists)}
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
	writeError(w, http.StatusBadGateway, "Could not reach the access provider. Try again in a moment")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
