package main

// One record per catalog movie with everything known about it, for other processes (AI tagging, a
// stream sorter, scripts on the PC) rather than for people to read. Home network only:
//
//	GET /catalog/movie/<tmdb id, tmdb:<id> or tt id>   one movie
//	GET /catalog/movies[?since=2026-11-15T10:00:00]    {"generated", "count", "movies": [...]}, by TMDB
//	                                                   id; since = only records updated at or after it
//
// It puts together what the catalog folder (/share/media-hub/catalog/) holds, matched by the movie's
// Stremio id (streamID: the tt id, else tmdb:<id>):
//   - movies.json: TMDB, IMDb's rating and Wikidata's facts (catalog.go, catalogsources.go), plus
//     today's tags, the rows the movie is in and whether corrections.json sets its tags.
//   - streams.json: the PC's stream check (what AIOStreams offers; matched by "id").
//   - plays.jsonl: Playback report's records of plays (copied from the PC after each record).
//   - links.jsonl: Playback link tests' tries of single links (copied after each run).
//
// Each source appears once, in its own section, without what another section already says: the
// stream check's own title/year/rows, the plays' box hardware and their list of offered streams stay
// in the PC's files. Plays and link tests are summed up (counts, medians), with each play newest first
// and the link tests grouped by file, best first. Shows' episodes (tt...:season:episode) never match
// a movie. Times are local, 2006-01-02T15:04:05, like the PC's; "updated" is the newest of them.
// A missing file leaves its section out; each file is read again when it changes.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const recTime = "2006-01-02T15:04:05"

type movieRecord struct {
	ID       int    `json:"id"` // TMDB
	IMDb     string `json:"imdb,omitempty"`
	Wikidata string `json:"wikidata,omitempty"`
	Title    string `json:"title"`
	Year     int    `json:"year,omitempty"`
	Released string `json:"released,omitempty"`
	Runtime  int    `json:"runtime,omitempty"` // minutes
	Overview string `json:"overview,omitempty"`
	Poster   string `json:"poster,omitempty"`   // full image URLs
	Backdrop string `json:"backdrop,omitempty"` //
	Updated  string `json:"updated"`

	Ratings recRatings  `json:"ratings"`
	Facts   *recFacts   `json:"facts,omitempty"` // Wikidata, once asked
	Catalog recCatalog  `json:"catalog"`
	Streams *recStreams `json:"streams,omitempty"`
	Plays   *recPlays   `json:"plays,omitempty"`
	Links   *recLinks   `json:"linkTests,omitempty"`
}

type recRating struct {
	Rating float64 `json:"rating"`
	Votes  int     `json:"votes"`
}

type recRatings struct {
	TMDB       recRating  `json:"tmdb"`
	IMDb       *recRating `json:"imdb,omitempty"`
	SortBy     string     `json:"sortBy"` // the one the rows sort by (bestRating)
	Popularity float64    `json:"popularity"`
}

type recFacts struct {
	OscarWins   int    `json:"oscarWins"`
	OscarNoms   int    `json:"oscarNoms"`
	BasedOnBook bool   `json:"basedOnBook"`
	TrueStory   bool   `json:"trueStory"`
	Series      string `json:"series,omitempty"`
	Checked     string `json:"checked"`
}

type recCatalog struct {
	Tags      []string `json:"tags"`
	Rows      []string `json:"rows"`                // today's rows it's in
	Corrected bool     `json:"corrected,omitempty"` // corrections.json sets its tags
	NoStreams bool     `json:"noStreams,omitempty"` // left out of the rows: the stream check found none
	Added     string   `json:"added"`
	Detailed  string   `json:"detailed,omitempty"` // TMDB details last read
}

type recStreams struct {
	Count   int     `json:"count"`
	Count4K int     `json:"count4k"`
	Top     string  `json:"top,omitempty"`
	AskS    float64 `json:"askSeconds,omitempty"`
	Error   string  `json:"error,omitempty"`
	Checked string  `json:"checked"`
}

type recPlays struct {
	Count     int       `json:"count"`
	Started   int       `json:"started"`
	AvgStartS float64   `json:"avgStartSeconds,omitempty"`
	Rebuffers int       `json:"rebuffers"`
	Last      string    `json:"last"`
	List      []recPlay `json:"list"` // newest first
}

type recPlay struct {
	Time        string   `json:"time"`
	Box         string   `json:"box,omitempty"`
	Engine      string   `json:"engine,omitempty"`
	Decoder     string   `json:"decoder,omitempty"`
	Display     string   `json:"display,omitempty"`
	Started     *bool    `json:"started,omitempty"`
	StartS      *float64 `json:"startSeconds,omitempty"`
	Rebuffers   int      `json:"rebuffers,omitempty"`
	Errors      int      `json:"errors,omitempty"`
	FailedTries int      `json:"failedTries,omitempty"` // plays just before it that never started
	Rank        int      `json:"rank,omitempty"`        // its place in AIOStreams' list
	File        string   `json:"file,omitempty"`
	GB          float64  `json:"gb,omitempty"`
	Host        string   `json:"host,omitempty"` // TorBox server
	Row         string   `json:"row,omitempty"`  // Media Hub row it was on, if any
}

type recLinks struct {
	Tries         int           `json:"tries"`
	Worked        int           `json:"worked"`
	MedianMbps    float64       `json:"medianMbps,omitempty"`
	MedianResolve float64       `json:"medianResolveSeconds,omitempty"`
	Last          string        `json:"last"`
	Files         []recLinkFile `json:"files"` // working and fastest first
}

type recLinkFile struct {
	File          string  `json:"file"`
	Resolution    string  `json:"resolution,omitempty"`
	GB            float64 `json:"gb,omitempty"`
	Addon         string  `json:"addon,omitempty"`   // Comet, MediaFusion, ...
	Indexer       string  `json:"indexer,omitempty"` // where the addon found it
	Tries         int     `json:"tries"`
	Worked        int     `json:"worked"`
	BestRank      int     `json:"bestRank,omitempty"`
	BestMbps      float64 `json:"bestMbps,omitempty"`
	MedianResolve float64 `json:"medianResolveSeconds,omitempty"`
	LastError     string  `json:"lastError,omitempty"`
	Last          string  `json:"last"`
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
		byID[id] = append(byID[id], it) // id-less records under "": title records match some by title
	}
	f.path, f.mod, f.size, f.byID = path, st.ModTime(), st.Size(), byID
	return byID
}

func recStamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(recTime)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + m) / 2
	}
	return float64(int(m*100+0.5)) / 100
}

// Partial decodes below: a field of an unexpected type is left empty and the rest still read
// (json.Unmarshal keeps going after a type error), so the PC's records can grow without breaking this.

func recStreamsFrom(raw json.RawMessage) *recStreams {
	var in struct {
		Streams   *float64 `json:"streams"`
		Streams4K float64  `json:"streams_4k"`
		Top       string   `json:"top"`
		Seconds   float64  `json:"seconds"`
		Error     *string  `json:"error"`
		Checked   string   `json:"checked"`
	}
	_ = json.Unmarshal(raw, &in)
	out := &recStreams{Count4K: int(in.Streams4K), Top: in.Top, AskS: in.Seconds, Checked: in.Checked}
	if in.Streams != nil {
		out.Count = int(*in.Streams)
	}
	if in.Error != nil {
		out.Error = *in.Error
	}
	return out
}

func recPlaysFrom(raws []json.RawMessage) *recPlays {
	out := &recPlays{List: []recPlay{}}
	var starts []float64
	for _, raw := range raws {
		var in struct {
			Time, Box, Engine, Decoder, Display string
			Started                             *bool    `json:"started"`
			StartS                              *float64 `json:"start_s"`
			Rebuffers                           float64  `json:"rebuffers"`
			PlaybackErrors                      float64  `json:"playback_errors"`
			PlayedRank                          float64  `json:"played_rank"`
			EarlierPlays                        []string `json:"earlier_plays"`
			Served                              *struct {
				Host, File string
				Bytes      float64
			} `json:"served"`
			TorBox *struct {
				File   string  `json:"file"`
				FileGB float64 `json:"file_gb"`
			} `json:"torbox"`
			MediaHub *struct {
				Row string `json:"row"`
			} `json:"media_hub"`
		}
		_ = json.Unmarshal(raw, &in)
		p := recPlay{Time: in.Time, Box: in.Box, Engine: in.Engine, Decoder: in.Decoder, Display: in.Display,
			Started: in.Started, StartS: in.StartS, Rebuffers: int(in.Rebuffers), Errors: int(in.PlaybackErrors),
			Rank: int(in.PlayedRank)}
		for _, e := range in.EarlierPlays {
			if strings.Contains(e, "NEVER STARTED") {
				p.FailedTries++
			}
		}
		if in.Served != nil {
			p.File, p.Host, p.GB = in.Served.File, in.Served.Host, float64(int(in.Served.Bytes/1e7+0.5))/100
		}
		if p.File == "" && in.TorBox != nil {
			p.File, p.GB = in.TorBox.File, in.TorBox.FileGB
		}
		if in.MediaHub != nil {
			p.Row = in.MediaHub.Row
		}
		out.Count++
		if p.Started != nil && *p.Started {
			out.Started++
		}
		if p.StartS != nil {
			starts = append(starts, *p.StartS)
		}
		out.Rebuffers += p.Rebuffers
		out.List = append(out.List, p)
	}
	sort.SliceStable(out.List, func(i, j int) bool { return out.List[i].Time > out.List[j].Time })
	if len(out.List) > 0 {
		out.Last = out.List[0].Time
	}
	if len(starts) > 0 {
		sum := 0.0
		for _, s := range starts {
			sum += s
		}
		out.AvgStartS = float64(int(sum/float64(len(starts))*10+0.5)) / 10
	}
	return out
}

var (
	addonName   = regexp.MustCompile(`^\s*\[[^\]]*\]\s*([^\s\[]+)`) // "[TB⚡] Comet 2160p" -> Comet
	indexerName = regexp.MustCompile(`🔍\s*([^|🌎📁📝\n]+)`)            // "🔍 StremThru | ..." -> StremThru
)

func recLinksFrom(raws []json.RawMessage) *recLinks {
	out := &recLinks{Files: []recLinkFile{}}
	byFile := map[string]*recLinkFile{}
	var order []string
	var mbps, resolve []float64
	fileResolve := map[string][]float64{}
	for _, raw := range raws {
		var in struct {
			Time, Name, Description, Resolution, File, Filename, Error string
			OK                                                         bool
			Rank, Bytes, Mbps                                          float64
			OfferedBytes                                               float64  `json:"offered_bytes"`
			ResolveS                                                   *float64 `json:"resolve_s"`
		}
		_ = json.Unmarshal(raw, &in)
		key := in.File
		if key == "" {
			key = in.Filename
		}
		if key == "" {
			continue
		}
		f := byFile[key]
		if f == nil {
			f = &recLinkFile{File: key, Resolution: in.Resolution}
			if m := addonName.FindStringSubmatch(in.Name); m != nil {
				f.Addon = m[1]
			}
			if m := indexerName.FindStringSubmatch(in.Description); m != nil {
				f.Indexer = strings.TrimSpace(m[1])
			}
			byFile[key] = f
			order = append(order, key)
		}
		if b := max(in.Bytes, in.OfferedBytes); b > 0 {
			f.GB = float64(int(b/1e7+0.5)) / 100
		}
		f.Tries++
		out.Tries++
		if r := int(in.Rank); r > 0 && (f.BestRank == 0 || r < f.BestRank) {
			f.BestRank = r
		}
		if in.OK {
			f.Worked++
			out.Worked++
			f.BestMbps = max(f.BestMbps, in.Mbps)
			if in.Mbps > 0 {
				mbps = append(mbps, in.Mbps)
			}
			if in.ResolveS != nil {
				resolve = append(resolve, *in.ResolveS)
				fileResolve[key] = append(fileResolve[key], *in.ResolveS)
			}
		} else if in.Error != "" && in.Time >= f.Last {
			f.LastError = in.Error
		}
		if in.Time > f.Last {
			f.Last = in.Time
		}
		if in.Time > out.Last {
			out.Last = in.Time
		}
	}
	for _, k := range order {
		f := byFile[k]
		f.MedianResolve = median(fileResolve[k])
		out.Files = append(out.Files, *f)
	}
	sort.SliceStable(out.Files, func(i, j int) bool {
		a, b := out.Files[i], out.Files[j]
		if (a.Worked > 0) != (b.Worked > 0) {
			return a.Worked > 0
		}
		return a.BestMbps > b.BestMbps
	})
	out.MedianMbps, out.MedianResolve = median(mbps), median(resolve)
	return out
}

// records: the records of these movies (all of them when ids is nil), in TMDB id order.
func (c *catalog) records(ids []int) []movieRecord {
	streams := c.recStreams.get(filepath.Join(c.dir, "streams.json"), false)
	plays := c.recPlays.get(filepath.Join(c.dir, "plays.jsonl"), true)
	links := c.recLinks.get(filepath.Join(c.dir, "links.jsonl"), true)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadNoStreamsLocked(false)
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
		sid := m.streamID()
		rec := movieRecord{ID: id, IMDb: m.IMDb, Wikidata: m.Wikidata, Title: m.Title, Year: m.Year(),
			Released: m.Released, Runtime: m.Runtime, Overview: m.Overview,
			Ratings: recRatings{TMDB: recRating{m.Rating, m.Votes}, SortBy: "tmdb", Popularity: m.Popularity},
			Catalog: recCatalog{Tags: c.tagsFor(m), Rows: rows[id], NoStreams: c.noStreams[sid],
				Added: recStamp(m.Added), Detailed: recStamp(m.Detailed)},
		}
		if m.Poster != "" {
			rec.Poster = "https://image.tmdb.org/t/p/w500" + m.Poster
		}
		if m.Backdrop != "" {
			rec.Backdrop = "https://image.tmdb.org/t/p/w1280" + m.Backdrop
		}
		if m.IMDbRating > 0 {
			rec.Ratings.IMDb = &recRating{m.IMDbRating, m.IMDbVotes}
			if r, v := m.bestRating(); r == m.IMDbRating && v == m.IMDbVotes {
				rec.Ratings.SortBy = "imdb"
			}
		}
		if !m.WikidataChecked.IsZero() {
			rec.Facts = &recFacts{m.OscarWins, m.OscarNoms, m.BasedOnBook, m.TrueStory, m.Series, recStamp(m.WikidataChecked)}
		}
		if rec.Catalog.Tags == nil {
			rec.Catalog.Tags = []string{}
		}
		if rec.Catalog.Rows == nil {
			rec.Catalog.Rows = []string{}
		}
		_, rec.Catalog.Corrected = c.corrections[id]
		if s := streams[sid]; len(s) > 0 {
			rec.Streams = recStreamsFrom(s[len(s)-1])
		}
		if p := plays[sid]; len(p) > 0 {
			rec.Plays = recPlaysFrom(p)
		}
		if l := links[sid]; len(l) > 0 {
			rec.Links = recLinksFrom(l)
		}
		rec.Updated = max(rec.Catalog.Added, rec.Catalog.Detailed)
		if rec.Facts != nil {
			rec.Updated = max(rec.Updated, rec.Facts.Checked)
		}
		if rec.Streams != nil {
			rec.Updated = max(rec.Updated, rec.Streams.Checked)
		}
		if rec.Plays != nil {
			rec.Updated = max(rec.Updated, rec.Plays.Last)
		}
		if rec.Links != nil {
			rec.Updated = max(rec.Updated, rec.Links.Last)
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
	recs := c.records(nil)
	if since := r.URL.Query().Get("since"); since != "" {
		keep := recs[:0]
		for _, rec := range recs {
			if rec.Updated >= since {
				keep = append(keep, rec)
			}
		}
		recs = keep
	}
	writeRecordJSON(w, map[string]any{"generated": recStamp(c.now()), "count": len(recs), "movies": recs})
}

func writeRecordJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		hubLog.event("catalog_serve_failed", "error", err)
	}
}
