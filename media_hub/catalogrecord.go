package main

// One record per catalog movie with everything known about it, for other processes (AI tagging, a
// stream sorter, scripts on the PC) rather than for people to read. Home network only:
//
//	GET /catalog/movie/<tmdb id, tmdb:<id> or tt id>  one movie
//	GET /catalog/movies                               every movie, by TMDB id
//
// It puts together what the catalog folder (/share/media-hub/catalog/) holds, matched by the movie's
// Stremio id (streamID: the tt id, else tmdb:<id>):
//   - movies.json: TMDB, IMDb's rating and Wikidata's facts (catalog.go, catalogsources.go), plus
//     today's tags, the rows the movie is in and whether corrections.json sets its tags.
//   - streams.json: the PC's stream check for it (what AIOStreams offers; its "id").
//   - plays.jsonl: Playback report's records of plays of it ("content_id"; copied from the PC after
//     each record). Shows' episodes have tt...:season:episode ids, so they never match a movie.
//   - links.jsonl: Playback link tests' tries of its links ("content_id"; copied after each run).
//
// The PC's records are passed on as they are (their fields are described in Playback report's
// scripts); a missing file leaves its part out. Each file is read again when it changes.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type movieRecord struct {
	ID        int               `json:"id"`
	StremioID string            `json:"stremioId"`
	Movie     *catalogMovie     `json:"movie"`
	Tags      []string          `json:"tags"`
	Rows      []string          `json:"rows"`
	Corrected bool              `json:"corrected,omitempty"`
	Streams   json.RawMessage   `json:"streams,omitempty"`
	Plays     []json.RawMessage `json:"plays,omitempty"`
	LinkTests []json.RawMessage `json:"linkTests,omitempty"`
}

// keyedFile: a JSON array or JSON-lines file of records, grouped by "content_id" (else "id"),
// read again only when the file changes.
type keyedFile struct {
	mu   sync.Mutex
	path string
	mod  time.Time
	size int64
	byID map[string][]json.RawMessage
}

func (f *keyedFile) get(path string, lines bool) map[string][]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(path)
	if err != nil {
		f.byID = nil
		return nil
	}
	if f.byID != nil && f.path == path && st.ModTime().Equal(f.mod) && st.Size() == f.size {
		return f.byID
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // PowerShell may write a BOM
	var items []json.RawMessage
	if lines {
		for _, l := range bytes.Split(data, []byte("\n")) {
			if l = bytes.TrimSpace(l); len(l) > 0 && json.Valid(l) {
				items = append(items, json.RawMessage(l))
			}
		}
	} else if err := json.Unmarshal(data, &items); err != nil {
		hubLog.event("catalog_record_unreadable", "file", filepath.Base(path), "error", err)
		return nil
	}
	byID := map[string][]json.RawMessage{}
	for _, it := range items {
		var k struct {
			ID        any `json:"id"`
			ContentID any `json:"content_id"`
		}
		if json.Unmarshal(it, &k) != nil {
			continue
		}
		id, _ := k.ContentID.(string)
		if id == "" {
			id, _ = k.ID.(string)
		}
		if id != "" {
			byID[id] = append(byID[id], it)
		}
	}
	f.path, f.mod, f.size, f.byID = path, st.ModTime(), st.Size(), byID
	return byID
}

// records: the records of these movies (all of them when ids is nil), in TMDB id order.
func (c *catalog) records(ids []int) []movieRecord {
	streams := c.recStreams.get(filepath.Join(c.dir, "streams.json"), false)
	plays := c.recPlays.get(filepath.Join(c.dir, "plays.jsonl"), true)
	links := c.recLinks.get(filepath.Join(c.dir, "links.jsonl"), true)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineupsLocked()
	rows := map[int][]string{}
	for _, r := range c.rowNamesLocked() {
		for _, id := range c.lineups[r.ID] {
			rows[id] = append(rows[id], r.ID)
		}
	}
	if ids == nil {
		for id := range c.movies {
			ids = append(ids, id)
		}
		sort.Ints(ids)
	}
	out := make([]movieRecord, 0, len(ids))
	for _, id := range ids {
		m, ok := c.movies[id]
		if !ok {
			continue
		}
		cp := *m
		sid := m.streamID()
		rec := movieRecord{ID: id, StremioID: sid, Movie: &cp, Tags: c.tagsFor(m), Rows: rows[id],
			Plays: plays[sid], LinkTests: links[sid]}
		if rec.Rows == nil {
			rec.Rows = []string{}
		}
		_, rec.Corrected = c.corrections[id]
		if s := streams[sid]; len(s) > 0 {
			rec.Streams = s[len(s)-1]
		}
		out = append(out, rec)
	}
	return out
}

// movieID: the TMDB id for "603", "tmdb:603" or "tt0133093" (0 if unknown).
func (c *catalog) movieID(key string) int {
	if id, err := strconv.Atoi(strings.TrimPrefix(key, "tmdb:")); err == nil {
		return id
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	best := 0
	for id, m := range c.movies {
		if m.IMDb == key && (best == 0 || id < best) {
			best = id
		}
	}
	return best
}

func (c *catalog) serveMovieRecord(w http.ResponseWriter, r *http.Request) {
	if !fromHome(r) {
		http.NotFound(w, r)
		return
	}
	recs := c.records([]int{c.movieID(r.PathValue("id"))})
	if len(recs) == 0 {
		http.NotFound(w, r)
		return
	}
	writeRecordJSON(w, recs[0])
}

func (c *catalog) serveMovieRecords(w http.ResponseWriter, r *http.Request) {
	if !fromHome(r) {
		http.NotFound(w, r)
		return
	}
	writeRecordJSON(w, c.records(nil))
}

func writeRecordJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hubLog.event("catalog_serve_failed", "error", err)
	}
}
