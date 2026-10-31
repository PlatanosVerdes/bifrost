package web

import (
	"net/http"
	"slices"

	"github.com/PlatanosVerdes/bifrost/internal/catalog"
	"github.com/PlatanosVerdes/bifrost/internal/httpx"
	"github.com/PlatanosVerdes/bifrost/internal/stream"
)

// API serves the page's JSON from what the list loop and the proxy keep.
type API struct {
	Name    string
	Version string
	Hub     *stream.Hub
	Catalog *catalog.Catalog
	List    *catalog.Board
}

func (a *API) serveStatus(w http.ResponseWriter, r *http.Request) {
	list, jellyfin, guide := a.List.Snapshot()
	body := map[string]any{
		"version":  a.Version,
		"name":     a.Name,
		"guide":    guide,
		"list":     list,
		"jellyfin": jellyfin,
		"recent":   a.Hub.RecentOpens(),
		"streams":  a.Hub.StreamsNow(),
	}
	if playing, asked, err := a.Hub.JellyfinPlaying(); asked && err != nil {
		body["jellyfin_error"] = err.Error()
	} else if asked {
		body["jellyfin_playing"] = playing
	}
	httpx.WriteJSON(w, body)
}

// serveChannels is the catalog for players that go to the engine directly, the source that last produced
// video first.
func (a *API) serveChannels(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, map[string]any{"channels": a.Hub.Channels()})
}

func (a *API) serveThumb(w http.ResponseWriter, r *http.Request) {
	file := a.Hub.ThumbFile(r.PathValue("id"))
	if file == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, file)
}

// serveBlock takes a source out of a channel (POST) or puts it back (DELETE).
func (a *API) serveBlock(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		httpx.WriteError(w, http.StatusForbidden, "That request came from another site")
		return
	}
	slug, id := r.PathValue("slug"), r.PathValue("id")
	g := a.Catalog.Group(slug)
	if g == nil || !slices.Contains(g.IDs, id) {
		httpx.WriteError(w, http.StatusNotFound, "That is not one of this channel's sources")
		return
	}
	block := r.Method == http.MethodPost
	if block {
		if _, left := a.Catalog.Lookup(slug); len(left) == 1 && left[0] == id {
			httpx.WriteError(w, http.StatusConflict, "This is the channel's last source")
			return
		}
		if a.Hub.PlayingSource(slug) == id {
			httpx.WriteError(w, http.StatusConflict, "The channel is playing this source: pick another one first")
			return
		}
	}
	if err := a.Catalog.Blocked.Mark(slug, id, block); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "The blocklist could not be saved")
		return
	}
	httpx.WriteJSON(w, map[string]any{"id": id, "blocked": block})
}
