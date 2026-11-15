package main

// A Stremio add-on over the catalog (catalog.go), only for trying the rows out at home before the TV
// app reads them itself. It's never public: guard (access.go) answers it only on the home network
// (and WireGuard), so the link needs no key. Install link:
// https://<https_domain>:<https_port>/stremio/manifest.json (Stremio wants HTTPS with a real certificate).
//
// Movies with an IMDb id are sent as tt ids, so Stremio's own add-ons find their streams; the rest as
// tmdb:<id>, whose details come from meta below.
//
// Each catalog and meta request is logged (stremio_catalog ip= row= skip=, stremio_meta ip= id=) in the
// hub log, /share/media-hub/diagnostics/hub/<day>.log, so Playback report can match a box's play to
// the row it was opened from. Meta is declared only for tmdb:, so tt movies' details come from
// Cinemeta and opening one isn't logged here.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const stremioPage = 100

type stremioAddon struct{ cat *catalog }

func (s stremioAddon) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /stremio/manifest.json", s.manifest)
	mux.HandleFunc("GET /stremio/catalog/movie/{rest...}", s.catalogRow)
	mux.HandleFunc("GET /stremio/meta/movie/{rest...}", s.meta)
}

// semver: Stremio refuses a manifest whose version isn't x.y.z.
var semver = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func (s stremioAddon) manifest(w http.ResponseWriter, r *http.Request) {
	v := version
	if !semver.MatchString(v) {
		v = "0.0.0"
	}
	type extra struct {
		Name string `json:"name"`
	}
	type catalogOut struct {
		Type  string  `json:"type"`
		ID    string  `json:"id"`
		Name  string  `json:"name"`
		Extra []extra `json:"extra"`
	}
	catalogs := []catalogOut{}
	if s.cat.on() {
		for _, row := range s.cat.rowNames() {
			catalogs = append(catalogs, catalogOut{"movie", row.ID, row.Name, []extra{{"skip"}}})
		}
	}
	stremioJSON(w, map[string]any{
		"id":          "lan.mediahub.catalog",
		"version":     v,
		"name":        "Media Hub Movies",
		"description": "The Media Hub catalog's movie rows (home only, for testing).",
		"types":       []string{"movie"},
		"catalogs":    catalogs,
		"resources": []any{"catalog",
			map[string]any{"name": "meta", "types": []string{"movie"}, "idPrefixes": []string{"tmdb:"}}},
		"behaviorHints": map[string]any{"configurable": false},
	})
}

// catalogRow: /stremio/catalog/movie/<row>.json or /stremio/catalog/movie/<row>/skip=<n>.json
func (s stremioAddon) catalogRow(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(r.PathValue("rest"), ".json")
	id, extra, _ := strings.Cut(rest, "/")
	skip := 0
	for _, part := range strings.Split(extra, "&") {
		if v, ok := strings.CutPrefix(part, "skip="); ok {
			skip, _ = strconv.Atoi(v)
		}
	}
	hubLog.event("stremio_catalog", "ip", remoteHost(r), "row", id, "skip", skip)
	metas := []map[string]any{}
	if row, ok := s.cat.row(id); ok && skip >= 0 && skip < len(row.Movies) {
		for _, m := range row.Movies[skip:min(len(row.Movies), skip+stremioPage)] {
			metas = append(metas, stremioMeta(m, false))
		}
	}
	stremioJSON(w, map[string]any{"metas": metas})
}

// meta: details of a tmdb:<id> movie (tt ids are Cinemeta's).
func (s stremioAddon) meta(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimSuffix(r.PathValue("rest"), ".json")
	id, err := strconv.Atoi(strings.TrimPrefix(rest, "tmdb:"))
	m, ok := s.cat.movie(id)
	hubLog.event("stremio_meta", "ip", remoteHost(r), "id", rest)
	if err != nil || !strings.HasPrefix(rest, "tmdb:") || !ok {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.NotFound(w, r)
		return
	}
	stremioJSON(w, map[string]any{"meta": stremioMeta(m, true)})
}

func stremioMeta(m *catalogMovie, full bool) map[string]any {
	out := map[string]any{"id": m.streamID(), "type": "movie", "name": m.Title, "description": m.Overview, "genres": m.Tags}
	if m.Poster != "" {
		out["poster"] = "https://image.tmdb.org/t/p/w500" + m.Poster
	}
	if m.Backdrop != "" {
		out["background"] = "https://image.tmdb.org/t/p/w1280" + m.Backdrop
	}
	if y := m.Year(); y > 0 {
		out["releaseInfo"] = strconv.Itoa(y)
	}
	if m.Rating > 0 {
		out["imdbRating"] = strconv.FormatFloat(m.Rating, 'f', 1, 64)
	}
	if full {
		if m.Runtime > 0 {
			out["runtime"] = strconv.Itoa(m.Runtime) + " min"
		}
		if m.Released != "" {
			out["released"] = m.Released + "T00:00:00.000Z"
		}
	}
	return out
}

func stremioJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=3600")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hubLog.event("catalog_serve_failed", "error", err)
	}
}
