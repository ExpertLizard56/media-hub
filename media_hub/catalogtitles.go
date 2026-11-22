package main

// Title records (the record keeper cog): one record for every title he watched or the PC link-tested,
// catalog or not: movies (tt id or tmdb:<id>) and shows, each show holding its episodes (tt...:season:episode).
// Built from plays.jsonl and links.jsonl with the same play and link summaries a catalog movie record has.
//
//	GET /catalog/titles[?kind=movie|show][&since=2026-11-15T10:00:00]   {"generated","count","titles":[...]}
//	GET /catalog/title/<tt id, tt id:season:episode or tmdb:<id>>       one title (an episode id gives its show)
//
// Home network only. Only Stremio's plays count: other apps (SmartTube, the Music app) get a made-up content
// id from the PC's guess. A Stremio play with no content_id joins the episode or movie whose title matches
// exactly. Catalog movies also keep their full /catalog/movie record; here they carry its TMDB id.
// Fields are only ever added.

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type titleRecord struct {
	ID        string `json:"id"`   // tt id, or tmdb:<id>
	Kind      string `json:"kind"` // movie, show
	Name      string `json:"name,omitempty"`
	Year      int    `json:"year,omitempty"`
	CatalogID int    `json:"catalogId,omitempty"` // TMDB id when it is a catalog movie (/catalog/movie/<id>)
	Updated   string `json:"updated"`

	// Movies: the plays and link tests themselves.
	Plays *recPlays `json:"plays,omitempty"`
	Links *recLinks `json:"linkTests,omitempty"`

	// Shows: totals over all episodes, then each episode (season, episode order).
	Totals   *titleTotals     `json:"totals,omitempty"`
	Episodes []*episodeRecord `json:"episodes,omitempty"`
}

type titleTotals struct {
	Episodes    int `json:"episodes"`    // episodes with any play or link test
	Played      int `json:"played"`      // episodes with a started play
	Plays       int `json:"plays"`
	Started     int `json:"started"`
	Rebuffers   int `json:"rebuffers"`
	LinkTries   int `json:"linkTries"`
	LinkWorked  int `json:"linkWorked"`
	LastSeason  int `json:"lastSeason,omitempty"`  // the latest episode played (started)
	LastEpisode int `json:"lastEpisode,omitempty"` //
}

type episodeRecord struct {
	ID      string    `json:"id"` // tt...:season:episode
	Season  int       `json:"season"`
	Episode int       `json:"episode"`
	Name    string    `json:"name,omitempty"`
	Updated string    `json:"updated"`
	Plays   *recPlays `json:"plays,omitempty"`
	Links   *recLinks `json:"linkTests,omitempty"`
}

func (c *catalog) titleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog/titles", func(w http.ResponseWriter, r *http.Request) {
		if !fromHome(r) {
			http.NotFound(w, r)
			return
		}
		kind, since := r.URL.Query().Get("kind"), r.URL.Query().Get("since")
		out := []*titleRecord{}
		for _, t := range c.titleRecords() {
			if (kind == "" || t.Kind == kind) && (since == "" || t.Updated >= since) {
				out = append(out, t)
			}
		}
		writeRecordJSON(w, map[string]any{"generated": recStamp(c.now()), "count": len(out), "titles": out})
	})
	mux.HandleFunc("GET /catalog/title/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !fromHome(r) {
			http.NotFound(w, r)
			return
		}
		id, _, _ := splitEpisodeID(r.PathValue("id"))
		for _, t := range c.titleRecords() {
			if t.ID == id {
				writeRecordJSON(w, t)
				return
			}
		}
		http.NotFound(w, r)
	})
}

// splitEpisodeID: "tt1520211:2:5" -> ("tt1520211", 2, 5); a movie id comes back alone with 0, 0.
func splitEpisodeID(id string) (string, int, int) {
	p := strings.Split(strings.TrimSpace(id), ":")
	if len(p) == 3 && strings.HasPrefix(p[0], "tt") {
		s, err1 := strconv.Atoi(p[1])
		e, err2 := strconv.Atoi(p[2])
		if err1 == nil && err2 == nil {
			return p[0], s, e
		}
	}
	return strings.TrimSpace(id), 0, 0
}

// titleRecords: every watched or link-tested title, newest first.
func (c *catalog) titleRecords() []*titleRecord {
	plays := c.recPlays.get(filepath.Join(c.dir, "plays.jsonl"), true)
	links := c.recLinks.get(filepath.Join(c.dir, "links.jsonl"), true)
	c.mu.Lock()
	catalogIDs := map[string]int{}
	names := map[string]string{}
	years := map[string]int{}
	for id, m := range c.movies {
		sid := m.streamID()
		catalogIDs[sid], names[sid], years[sid] = id, m.Title, m.Year()
	}
	c.mu.Unlock()
	return buildTitleRecords(plays, links, catalogIDs, names, years)
}

type playMeta struct {
	App, Title, Episode string
}

func buildTitleRecords(plays, links map[string][]json.RawMessage, catalogIDs map[string]int, catNames map[string]string, catYears map[string]int) []*titleRecord {
	// Stremio plays by content id; the id-less ones wait to be matched by title.
	byID := map[string][]json.RawMessage{}
	titleOf := map[string]string{} // content id -> play title as Stremio shows it
	var loose []json.RawMessage
	var looseTitles []string
	for id, raws := range plays {
		for _, raw := range raws {
			var m playMeta
			_ = json.Unmarshal(raw, &m)
			if m.App != "" && m.App != "com.stremio.one" {
				continue
			}
			if id == "" {
				continue
			}
			byID[id] = append(byID[id], raw)
			if m.Title != "" {
				titleOf[id] = m.Title
			}
		}
	}
	// keyedFile drops records without an id, so the id-less Stremio plays are read from "" if present.
	for _, raw := range plays[""] {
		var m playMeta
		_ = json.Unmarshal(raw, &m)
		if (m.App == "" || m.App == "com.stremio.one") && m.Title != "" {
			loose, looseTitles = append(loose, raw), append(looseTitles, m.Title)
		}
	}
	for i, raw := range loose {
		for id, t := range titleOf {
			if t == looseTitles[i] {
				byID[id] = append(byID[id], raw)
				break
			}
		}
	}

	ids := map[string]bool{}
	for id := range byID {
		ids[id] = true
	}
	for id := range links {
		if id != "" {
			ids[id] = true
		}
	}

	titles := map[string]*titleRecord{}
	get := func(id, kind string) *titleRecord {
		t := titles[id]
		if t == nil {
			t = &titleRecord{ID: id, Kind: kind, CatalogID: catalogIDs[id], Name: catNames[id], Year: catYears[id]}
			titles[id] = t
		}
		return t
	}
	for id := range ids {
		show, s, e := splitEpisodeID(id)
		var p *recPlays
		var l *recLinks
		if r := byID[id]; len(r) > 0 {
			p = recPlaysFrom(r)
		}
		if r := links[id]; len(r) > 0 {
			l = recLinksFrom(r)
		}
		if s == 0 && e == 0 && !strings.Contains(id, ":") || strings.HasPrefix(id, "tmdb:") {
			t := get(id, "movie")
			t.Plays, t.Links = p, l
			if t.Name == "" {
				t.Name = titleOf[id]
			}
			continue
		}
		if s == 0 && e == 0 {
			continue // some other id shape
		}
		t := get(show, "show")
		ep := &episodeRecord{ID: id, Season: s, Episode: e, Plays: p, Links: l}
		if name := titleOf[id]; name != "" {
			if i := strings.Index(name, ", "); i > 0 {
				if t.Name == "" {
					t.Name = name[:i]
				}
				ep.Name = name[i+2:]
			}
		}
		t.Episodes = append(t.Episodes, ep)
	}

	out := make([]*titleRecord, 0, len(titles))
	for _, t := range titles {
		if t.Kind == "movie" {
			t.Updated = lastOf(t.Plays, t.Links)
		} else {
			sort.Slice(t.Episodes, func(i, j int) bool {
				a, b := t.Episodes[i], t.Episodes[j]
				return a.Season < b.Season || a.Season == b.Season && a.Episode < b.Episode
			})
			tot := &titleTotals{}
			lastPlayed := ""
			for _, ep := range t.Episodes {
				ep.Updated = lastOf(ep.Plays, ep.Links)
				t.Updated = max(t.Updated, ep.Updated)
				tot.Episodes++
				if ep.Plays != nil {
					tot.Plays += ep.Plays.Count
					tot.Started += ep.Plays.Started
					tot.Rebuffers += ep.Plays.Rebuffers
					if ep.Plays.Started > 0 {
						tot.Played++
						for _, pl := range ep.Plays.List {
							if pl.Started != nil && *pl.Started && pl.Time > lastPlayed {
								lastPlayed, tot.LastSeason, tot.LastEpisode = pl.Time, ep.Season, ep.Episode
							}
						}
					}
				}
				if ep.Links != nil {
					tot.LinkTries += ep.Links.Tries
					tot.LinkWorked += ep.Links.Worked
				}
			}
			t.Totals = tot
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated != out[j].Updated {
			return out[i].Updated > out[j].Updated
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func lastOf(p *recPlays, l *recLinks) string {
	s := ""
	if p != nil {
		s = p.Last
	}
	if l != nil {
		s = max(s, l.Last)
	}
	return s
}
