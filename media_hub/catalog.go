package main

// The movie catalog: rows of movies (tag rows, holiday rows, new releases) built from TMDB, for the
// TV app's On Demand section later and, for now, a Stremio add-on used for testing (stremio.go).
// Nothing in here knows about Stremio: both read rows() and movie().
//
// - Turned on by the add-on's option tmdb_api_key; empty = off and nothing else changes.
// - Every day (and on the first start) it pulls movies from TMDB: popular English movies per genre,
//   new releases, and the best-rated; plus details (IMDb id, runtime) of any movie it hasn't seen.
// - Each movie has several tags. The starting tags are the owner's own hand sorting
//   (catalogdata/seed.json, one genre each, plus the holiday lists); other movies get theirs from
//   TMDB's genres. /share/media-hub/catalog/corrections.json ({"<tmdb id>": ["Tag", ...]}) replaces a
//   movie's tags and always wins; it's read again when it changes.
// - The day's lineup of each row: better-rated and new movies first, a little shuffled each day,
//   movies that were on a row's first screen in the last week pushed down; same order all day.
// - Files in /share/media-hub/catalog/: movies.json (everything known), shown.json (what each row
//   showed first, by day), corrections.json (yours), streams.json (below).
// - streams.json: the PC's stream check (Playback report's "Check catalog streams.ps1", a JSON array of
//   {"id": "tt.../tmdb:<id>", "streams": n, "error": ..., "checked": ...}). A movie it checked that came
//   back with no streams and no error is left out of every row until a later check finds some. Movies
//   not in it, or with an error, stay. Read again when it changes, like corrections.json.
// - IMDb ratings and Wikidata facts (Oscars, based on a book or a true story, film series) come from
//   catalogsources.go, once a day after the refresh.
// - plays.jsonl and links.jsonl (the PC's Playback report plays and link tests, copied there) and all
//   of the above come together per movie in catalogrecord.go (/catalog/movie/<id>, /catalog/movies).

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed catalogdata/seed.json
var catalogSeedJSON []byte

var catalogDir = filepath.Join(root, "catalog")

const (
	// A tag needs this many movies to become a row.
	catalogMinRow = 8
	// The part of a row seen without scrolling: what "shown" remembers.
	catalogFirstScreen = 20
	// How long a movie that was on a row's first screen is pushed down.
	catalogShownDays = 7
	// New releases: out within this many days.
	catalogNewDays = 120
	// Details (IMDb id, runtime) are fetched again after this long, for ratings and fixes.
	catalogDetailsDays = 60
	// The daily refresh runs once the local hour reaches this.
	catalogRefreshHour = 4
)

// TMDB's movie genres, with the tag each one gives a movie that isn't in the hand sorting.
var tmdbGenreTags = map[int]string{
	28: "Action", 12: "Adventure", 16: "Animation", 35: "Comedy", 80: "Crime", 99: "Documentary",
	18: "Drama", 10751: "Family", 14: "Fantasy", 36: "History", 27: "Horror", 10402: "Music",
	9648: "Mystery", 10749: "Romance", 878: "Science Fiction", 10770: "TV Movie", 53: "Thriller",
	10752: "War", 37: "Western",
}

// Tags that never become a row of their own (the holidays have their own rows).
var catalogNoRow = map[string]bool{"Seasonal": true}

type catalogMovie struct {
	ID         int       `json:"id"`
	IMDb       string    `json:"imdb,omitempty"`
	Title      string    `json:"title"`
	Released   string    `json:"released,omitempty"` // YYYY-MM-DD
	Overview   string    `json:"overview,omitempty"`
	Poster     string    `json:"poster,omitempty"`   // TMDB image path
	Backdrop   string    `json:"backdrop,omitempty"` // TMDB image path
	Rating     float64   `json:"rating"`
	Votes      int       `json:"votes"`
	Popularity float64   `json:"popularity"`
	Runtime    int       `json:"runtime,omitempty"`
	GenreIDs   []int     `json:"genreIds,omitempty"`
	Added      time.Time `json:"added"`
	Detailed   time.Time `json:"detailed,omitempty"`
	// From catalogsources.go.
	IMDbRating      float64   `json:"imdbRating,omitempty"`
	IMDbVotes       int       `json:"imdbVotes,omitempty"`
	Wikidata        string    `json:"wikidata,omitempty"` // Q id
	OscarWins       int       `json:"oscarWins,omitempty"`
	OscarNoms       int       `json:"oscarNoms,omitempty"`
	BasedOnBook     bool      `json:"basedOnBook,omitempty"`
	TrueStory       bool      `json:"trueStory,omitempty"`
	Series          string    `json:"series,omitempty"`
	WikidataChecked time.Time `json:"wikidataChecked,omitempty"`
	// Tags as served: worked out from the seed, TMDB's genres and the corrections (tagsFor).
	Tags []string `json:"-"`
}

// streamID: the id players look streams up by, the IMDb id if there is one, else tmdb:<id>.
func (m *catalogMovie) streamID() string {
	if m.IMDb != "" {
		return m.IMDb
	}
	return "tmdb:" + strconv.Itoa(m.ID)
}

func (m *catalogMovie) Year() int {
	if len(m.Released) >= 4 {
		y, _ := strconv.Atoi(m.Released[:4])
		return y
	}
	return 0
}

type catalogHoliday struct {
	Name  string `json:"name"`
	Start string `json:"start"` // MM-DD
	End   string `json:"end"`   // MM-DD, may be earlier than Start (wraps the new year)
	IDs   []int  `json:"ids"`
}

type catalogSeed struct {
	Tags     map[string][]string `json:"tags"`
	Holidays []catalogHoliday    `json:"holidays"`
}

// catalogRow is one row as served: its id (stable, used in links), its name and the day's lineup.
type catalogRow struct {
	ID     string
	Name   string
	Movies []*catalogMovie
}

type catalog struct {
	dir     string
	key     string
	tmdb    string // TMDB's API address; a test points it at its own server
	// catalogsources.go's addresses (a test points them at its own server) and pause between queries.
	imdbRatings   string
	wikidata      string
	wikidataPause time.Duration
	client  *http.Client
	seed    catalogSeed
	now     func() time.Time
	pause   time.Duration // between TMDB requests
	refresh sync.Mutex    // one refresh at a time

	mu          sync.Mutex
	movies      map[int]*catalogMovie
	corrections map[int][]string
	corrMod     time.Time
	corrChecked time.Time
	noStreams   map[string]bool // stremio ids (tt... or tmdb:<id>) the stream check found nothing for
	nsMod       time.Time
	nsChecked   time.Time
	shown       map[string]map[int]string // row id -> movie id -> last day on the row's first screen
	lineupDay   string
	lineups     map[string][]int // row id -> the day's order
	lastRefresh time.Time
	lastSources time.Time

	recStreams, recPlays, recLinks keyedFile // catalogrecord.go
}

func newCatalog(key, dir string) *catalog {
	c := &catalog{
		dir: dir, key: strings.TrimSpace(key), tmdb: "https://api.themoviedb.org/3",
		client: &http.Client{Timeout: 30 * time.Second}, now: time.Now, pause: 30 * time.Millisecond,
		imdbRatings: "https://datasets.imdbws.com/title.ratings.tsv.gz", wikidata: "https://query.wikidata.org/sparql",
		wikidataPause: catalogWikidataPause,
		movies: map[int]*catalogMovie{}, corrections: map[int][]string{}, shown: map[string]map[int]string{},
	}
	if err := json.Unmarshal(catalogSeedJSON, &c.seed); err != nil {
		panic("catalogdata/seed.json: " + err.Error()) // embedded at build time; a test catches it
	}
	c.load()
	return c
}

func (c *catalog) on() bool { return c.key != "" }

func (c *catalog) load() {
	var list []*catalogMovie
	if readJSONFile(filepath.Join(c.dir, "movies.json"), &list) {
		for _, m := range list {
			c.movies[m.ID] = m
		}
	}
	readJSONFile(filepath.Join(c.dir, "shown.json"), &c.shown)
	var meta struct{ LastRefresh, LastSources time.Time }
	if readJSONFile(filepath.Join(c.dir, "state.json"), &meta) {
		c.lastRefresh, c.lastSources = meta.LastRefresh, meta.LastSources
	}
	c.loadCorrectionsLocked(true)
}

func readJSONFile(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // Windows PowerShell writes a BOM
	if err := json.Unmarshal(data, v); err != nil {
		hubLog.event("catalog_file_bad", "file", filepath.Base(path), "error", err)
		return false
	}
	return true
}

// writeJSONFile writes through a temporary file, so a crash never leaves half a file.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadCorrectionsLocked reads corrections.json again if it changed (looked at once a minute at most).
func (c *catalog) loadCorrectionsLocked(force bool) {
	now := c.now()
	if !force && now.Sub(c.corrChecked) < time.Minute {
		return
	}
	c.corrChecked = now
	path := filepath.Join(c.dir, "corrections.json")
	st, err := os.Stat(path)
	if err != nil {
		if len(c.corrections) > 0 {
			c.corrections, c.lineupDay = map[int][]string{}, ""
		}
		return
	}
	if !force && st.ModTime().Equal(c.corrMod) {
		return
	}
	c.corrMod = st.ModTime()
	var raw map[string][]string
	if !readJSONFile(path, &raw) {
		return
	}
	corr := map[int][]string{}
	for k, tags := range raw {
		if id, err := strconv.Atoi(strings.TrimSpace(k)); err == nil {
			corr[id] = tags
		}
	}
	c.corrections, c.lineupDay = corr, "" // the lineups are worked out again with them
	hubLog.event("catalog_corrections", "movies", len(corr))
}

// loadNoStreamsLocked reads streams.json again if it changed (looked at once a minute at most).
func (c *catalog) loadNoStreamsLocked(force bool) {
	now := c.now()
	if !force && now.Sub(c.nsChecked) < time.Minute {
		return
	}
	c.nsChecked = now
	path := filepath.Join(c.dir, "streams.json")
	st, err := os.Stat(path)
	if err != nil {
		if len(c.noStreams) > 0 {
			c.noStreams, c.lineupDay = nil, ""
		}
		return
	}
	if !force && st.ModTime().Equal(c.nsMod) {
		return
	}
	c.nsMod = st.ModTime()
	var raw []struct {
		ID      string  `json:"id"`
		Streams *int    `json:"streams"`
		Error   *string `json:"error"`
		Checked string  `json:"checked"`
	}
	if !readJSONFile(path, &raw) {
		return
	}
	none := map[string]bool{}
	for _, e := range raw {
		if e.Checked != "" && e.Streams != nil && *e.Streams == 0 && (e.Error == nil || *e.Error == "") {
			none[e.ID] = true
		}
	}
	c.noStreams, c.lineupDay = none, ""
	hubLog.event("catalog_no_streams", "movies", len(none), "checked", len(raw))
}

// tagsFor: the corrections if the movie has any; otherwise the hand sorting's tags plus the tags of
// TMDB's genres, and the holidays whose list holds it.
func (c *catalog) tagsFor(m *catalogMovie) []string {
	if t, ok := c.corrections[m.ID]; ok {
		return t
	}
	seen := map[string]bool{}
	var tags []string
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	seeded := c.seed.Tags[strconv.Itoa(m.ID)]
	animationSeeded := false
	for _, t := range seeded {
		add(t)
		if strings.HasPrefix(t, "Animation") {
			animationSeeded = true
		}
	}
	family := false
	for _, g := range m.GenreIDs {
		if g == 10751 {
			family = true
		}
	}
	for _, g := range m.GenreIDs {
		t := tmdbGenreTags[g]
		if g == 16 {
			if animationSeeded {
				continue
			}
			if family {
				t = "Animation (Kids)"
			}
		}
		add(t)
	}
	for _, t := range sourceTags(m) {
		add(t)
	}
	for _, h := range c.seed.Holidays {
		for _, id := range h.IDs {
			if id == m.ID {
				add(h.Name)
			}
		}
	}
	return tags
}

// holidayToday: the holiday whose dates hold today; when two do (Summer holds July 4th), the shorter.
func (c *catalog) holidayToday(now time.Time) *catalogHoliday {
	day := now.Format("01-02")
	var best *catalogHoliday
	bestLen := 400
	for i := range c.seed.Holidays {
		h := &c.seed.Holidays[i]
		if n, ok := holidaySpan(h.Start, h.End, day); ok && n < bestLen {
			best, bestLen = h, n
		}
	}
	return best
}

// holidaySpan says whether day (MM-DD) is within start..end (which may wrap the new year) and how
// many days the range is.
func holidaySpan(start, end, day string) (int, bool) {
	// In a year that isn't a leap year (time.Parse's year 0 is one).
	s, err1 := time.Parse("2006-01-02", "2001-"+start)
	e, err2 := time.Parse("2006-01-02", "2001-"+end)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	n := int(e.Sub(s).Hours()/24) + 1
	if end < start {
		n += 365
		return n, day >= start || day <= end
	}
	return n, day >= start && day <= end
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// rowNames lists every row there can be, in the order they're shown: the holidays, new releases,
// then the tags with enough movies (biggest first). A holiday row is empty outside its dates.
func (c *catalog) rowNames() []catalogRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rowNamesLocked()
}

func (c *catalog) rowNamesLocked() []catalogRow {
	var rows []catalogRow
	for _, h := range c.seed.Holidays {
		rows = append(rows, catalogRow{ID: "holiday-" + slug(h.Name), Name: h.Name})
	}
	rows = append(rows, catalogRow{ID: "new", Name: "New Releases"})
	holidays := map[string]bool{}
	for _, h := range c.seed.Holidays {
		holidays[h.Name] = true
	}
	count := map[string]int{}
	for _, m := range c.movies {
		for _, t := range c.tagsFor(m) {
			count[t]++
		}
	}
	var tags []string
	for t, n := range count {
		if n >= catalogMinRow && !holidays[t] && !catalogNoRow[t] {
			tags = append(tags, t)
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		if count[tags[i]] != count[tags[j]] {
			return count[tags[i]] > count[tags[j]]
		}
		return tags[i] < tags[j]
	})
	for _, t := range tags {
		rows = append(rows, catalogRow{ID: "tag-" + slug(t), Name: t})
	}
	return rows
}

// rows: today's rows with movies, in order (empty ones left out).
func (c *catalog) rows() []catalogRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineupsLocked()
	var out []catalogRow
	for _, r := range c.rowNamesLocked() {
		ids := c.lineups[r.ID]
		if len(ids) == 0 {
			continue
		}
		r.Movies = c.moviesLocked(ids)
		out = append(out, r)
	}
	return out
}

// row: one row's lineup by its id; false if there's no such row.
func (c *catalog) row(id string) (catalogRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lineupsLocked()
	for _, r := range c.rowNamesLocked() {
		if r.ID == id {
			r.Movies = c.moviesLocked(c.lineups[id])
			return r, true
		}
	}
	return catalogRow{}, false
}

func (c *catalog) movie(id int) (*catalogMovie, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.movies[id]
	if !ok {
		return nil, false
	}
	cp := *m
	cp.Tags = c.tagsFor(m)
	return &cp, true
}

func (c *catalog) moviesLocked(ids []int) []*catalogMovie {
	out := make([]*catalogMovie, 0, len(ids))
	for _, id := range ids {
		if m, ok := c.movies[id]; ok {
			cp := *m
			cp.Tags = c.tagsFor(m)
			out = append(out, &cp)
		}
	}
	return out
}

// lineupsLocked works out the day's order of every row once a day (or after corrections or a refresh).
func (c *catalog) lineupsLocked() {
	c.loadCorrectionsLocked(false)
	c.loadNoStreamsLocked(false)
	now := c.now()
	day := now.Format("2006-01-02")
	if c.lineupDay == day && c.lineups != nil {
		return
	}
	members := map[string][]*catalogMovie{}
	holiday := c.holidayToday(now)
	newSince := now.AddDate(0, 0, -catalogNewDays).Format("2006-01-02")
	for _, m := range c.movies {
		if m.Released == "" || m.Released > day {
			continue // not out yet
		}
		if c.noStreams[m.streamID()] {
			continue // nothing to play
		}
		tags := c.tagsFor(m)
		for _, t := range tags {
			members["tag-"+slug(t)] = append(members["tag-"+slug(t)], m)
			if holiday != nil && t == holiday.Name {
				members["holiday-"+slug(t)] = append(members["holiday-"+slug(t)], m)
			}
		}
		if m.Released >= newSince {
			members["new"] = append(members["new"], m)
		}
	}
	lineups := map[string][]int{}
	for id, list := range members {
		shown := c.shown[id]
		score := map[int]float64{}
		for _, m := range list {
			r, v := m.bestRating()
			s := r / 10 * math.Log10(float64(v)+10)
			if id == "new" {
				s = math.Log10(m.Popularity + 1)
			} else if m.Released >= newSince {
				s += 0.3
			}
			if last, ok := shown[m.ID]; ok && last != day {
				if t, err := time.Parse("2006-01-02", last); err == nil && now.Sub(t) < catalogShownDays*24*time.Hour {
					s *= 0.5
				}
			}
			s += 0.8 * dailyJitter(day, id, m.ID)
			score[m.ID] = s
		}
		sort.Slice(list, func(i, j int) bool {
			if score[list[i].ID] != score[list[j].ID] {
				return score[list[i].ID] > score[list[j].ID]
			}
			return list[i].ID < list[j].ID
		})
		ids := make([]int, len(list))
		for i, m := range list {
			ids[i] = m.ID
		}
		lineups[id] = ids
		if shown == nil {
			shown = map[int]string{}
			c.shown[id] = shown
		}
		for _, mid := range ids[:min(len(ids), catalogFirstScreen)] {
			if shown[mid] == "" || shown[mid] < day {
				if prev := shown[mid]; prev == "" || now.Sub(mustDay(prev)) >= catalogShownDays*24*time.Hour {
					shown[mid] = day
				}
			}
		}
	}
	// Forget first-screen days older than the push-down window.
	for _, shown := range c.shown {
		for mid, d := range shown {
			if now.Sub(mustDay(d)) > 2*catalogShownDays*24*time.Hour {
				delete(shown, mid)
			}
		}
	}
	c.lineups, c.lineupDay = lineups, day
	if err := writeJSONFile(filepath.Join(c.dir, "shown.json"), c.shown); err != nil {
		hubLog.event("catalog_save_failed", "file", "shown.json", "error", err)
	}
}

func mustDay(d string) time.Time {
	t, _ := time.Parse("2006-01-02", d)
	return t
}

// dailyJitter: a number in [0,1) that's the same all day for a movie in a row, different tomorrow.
func dailyJitter(day, row string, id int) float64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%d", day, row, id)
	return float64(h.Sum64()%1_000_000) / 1_000_000
}

// --- Refreshing from TMDB ---

// keepFresh refreshes on the first start and then every day after catalogRefreshHour (local time).
func (c *catalog) keepFresh() {
	for {
		now := c.now()
		c.mu.Lock()
		last, sources := c.lastRefresh, c.lastSources
		c.mu.Unlock()
		dueAt := func(t time.Time) bool {
			return t.IsZero() || (now.Format("2006-01-02") != t.Format("2006-01-02") && now.Hour() >= catalogRefreshHour)
		}
		if dueAt(last) {
			c.refreshNow()
		}
		if dueAt(sources) {
			c.refreshSources()
		}
		time.Sleep(15 * time.Minute)
	}
}

// refreshNow pulls the lists, adds and updates movies, fetches missing details and saves.
func (c *catalog) refreshNow() {
	c.refresh.Lock()
	defer c.refresh.Unlock()
	start := c.now()
	found := map[int]tmdbListMovie{}
	today := start.Format("2006-01-02")
	lists := []url.Values{}
	for g := range tmdbGenreTags {
		minVotes := "200"
		if g == 99 || g == 10770 {
			minVotes = "40"
		}
		for page := 1; page <= 3; page++ {
			lists = append(lists, url.Values{"with_genres": {strconv.Itoa(g)}, "vote_count.gte": {minVotes},
				"sort_by": {"popularity.desc"}, "page": {strconv.Itoa(page)}})
		}
	}
	for page := 1; page <= 3; page++ {
		lists = append(lists,
			url.Values{"primary_release_date.gte": {start.AddDate(0, 0, -catalogNewDays).Format("2006-01-02")},
				"primary_release_date.lte": {today}, "vote_count.gte": {"20"}, "sort_by": {"popularity.desc"}, "page": {strconv.Itoa(page)}},
			url.Values{"vote_count.gte": {"3000"}, "sort_by": {"vote_average.desc"}, "page": {strconv.Itoa(page)}})
	}
	failed := 0
	for _, q := range lists {
		q.Set("include_adult", "false")
		q.Set("with_original_language", "en")
		q.Set("language", "en-US")
		var resp struct {
			Results []tmdbListMovie `json:"results"`
		}
		if err := c.get("/discover/movie", q, &resp); err != nil {
			failed++
			if failed == 1 {
				hubLog.event("catalog_tmdb_failed", "error", err)
			}
			if failed >= 5 && len(found) == 0 {
				hubLog.event("catalog_refresh_stopped", "reason", "TMDB isn't answering; tried again tomorrow (or on a restart)")
				c.markRefreshed(start)
				return
			}
			continue
		}
		for _, m := range resp.Results {
			found[m.ID] = m
		}
	}
	added := 0
	c.mu.Lock()
	for id, lm := range found {
		m := c.movies[id]
		if m == nil {
			m = &catalogMovie{ID: id, Added: start}
			c.movies[id] = m
			added++
		}
		lm.applyTo(m)
	}
	// The hand-sorted and holiday movies are always in the catalog, found by a list or not.
	var need []int
	for k := range c.seed.Tags {
		if id, err := strconv.Atoi(k); err == nil && c.movies[id] == nil {
			c.movies[id] = &catalogMovie{ID: id, Added: start}
			added++
		}
	}
	for _, h := range c.seed.Holidays {
		for _, id := range h.IDs {
			if c.movies[id] == nil {
				c.movies[id] = &catalogMovie{ID: id, Added: start}
				added++
			}
		}
	}
	for id := range c.corrections {
		if c.movies[id] == nil {
			c.movies[id] = &catalogMovie{ID: id, Added: start}
			added++
		}
	}
	for id, m := range c.movies {
		if m.Detailed.IsZero() || start.Sub(m.Detailed) > catalogDetailsDays*24*time.Hour {
			need = append(need, id)
		}
	}
	c.mu.Unlock()
	sort.Ints(need)
	detailed, missing := 0, 0
	for i, id := range need {
		var d tmdbDetails
		err := c.get("/movie/"+strconv.Itoa(id), url.Values{"append_to_response": {"external_ids"}, "language": {"en-US"}}, &d)
		c.mu.Lock()
		if err != nil {
			if strings.Contains(err.Error(), "404") {
				delete(c.movies, id) // gone from TMDB
				missing++
			}
		} else if m := c.movies[id]; m != nil {
			d.applyTo(m)
			m.Detailed = start
			detailed++
		}
		c.mu.Unlock()
		if i > 0 && i%500 == 0 {
			c.save() // a long first run keeps what it has so far
		}
	}
	c.mu.Lock()
	c.lineupDay = "" // new movies: the lineups are worked out again
	total := len(c.movies)
	c.mu.Unlock()
	c.markRefreshed(start)
	c.save()
	hubLog.event("catalog_refreshed", "movies", total, "new", added, "details", detailed, "gone", missing,
		"list_errors", failed, "took", c.now().Sub(start).Round(time.Second))
}

func (c *catalog) markRefreshed(t time.Time) {
	c.mu.Lock()
	c.lastRefresh = t
	c.mu.Unlock()
	c.writeState()
}

func (c *catalog) writeState() {
	c.mu.Lock()
	state := map[string]time.Time{"LastRefresh": c.lastRefresh, "LastSources": c.lastSources}
	c.mu.Unlock()
	if err := writeJSONFile(filepath.Join(c.dir, "state.json"), state); err != nil {
		hubLog.event("catalog_save_failed", "file", "state.json", "error", err)
	}
}

func (c *catalog) save() {
	c.mu.Lock()
	list := make([]*catalogMovie, 0, len(c.movies))
	for _, m := range c.movies {
		list = append(list, m)
	}
	c.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if err := writeJSONFile(filepath.Join(c.dir, "movies.json"), list); err != nil {
		hubLog.event("catalog_save_failed", "file", "movies.json", "error", err)
	}
}

// get calls TMDB with the key: a long one (a read access token) as a bearer token, a short one as api_key.
func (c *catalog) get(path string, q url.Values, v any) error {
	time.Sleep(c.pause)
	if len(c.key) <= 40 {
		q.Set("api_key", c.key)
	}
	req, err := http.NewRequest(http.MethodGet, c.tmdb+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if len(c.key) > 40 {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	req.Header.Set("Accept", "application/json")
	for attempt := 0; ; attempt++ {
		resp, err := c.client.Do(req)
		if err != nil {
			return fmt.Errorf("TMDB %s: %v", path, redactKey(err.Error(), c.key))
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			closeQuietly(resp.Body)
			time.Sleep(time.Duration(2*(attempt+1)) * time.Second)
			continue
		}
		defer closeQuietly(resp.Body)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			return fmt.Errorf("TMDB %s: %d %s", path, resp.StatusCode, redactKey(strings.TrimSpace(string(body)), c.key))
		}
		return json.NewDecoder(resp.Body).Decode(v)
	}
}

func redactKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[key]")
}

type tmdbListMovie struct {
	ID         int     `json:"id"`
	Title      string  `json:"title"`
	Released   string  `json:"release_date"`
	Overview   string  `json:"overview"`
	Poster     string  `json:"poster_path"`
	Backdrop   string  `json:"backdrop_path"`
	Rating     float64 `json:"vote_average"`
	Votes      int     `json:"vote_count"`
	Popularity float64 `json:"popularity"`
	GenreIDs   []int   `json:"genre_ids"`
}

func (l tmdbListMovie) applyTo(m *catalogMovie) {
	m.Title, m.Released, m.Overview = l.Title, l.Released, l.Overview
	m.Poster, m.Backdrop = l.Poster, l.Backdrop
	m.Rating, m.Votes, m.Popularity = l.Rating, l.Votes, l.Popularity
	if len(l.GenreIDs) > 0 {
		m.GenreIDs = l.GenreIDs
	}
}

type tmdbDetails struct {
	tmdbListMovie
	Runtime int `json:"runtime"`
	Genres  []struct {
		ID int `json:"id"`
	} `json:"genres"`
	IMDb        string `json:"imdb_id"`
	ExternalIDs struct {
		IMDb string `json:"imdb_id"`
	} `json:"external_ids"`
}

func (d tmdbDetails) applyTo(m *catalogMovie) {
	d.GenreIDs = nil
	for _, g := range d.Genres {
		d.GenreIDs = append(d.GenreIDs, g.ID)
	}
	d.tmdbListMovie.applyTo(m)
	m.Runtime = d.Runtime
	if d.ExternalIDs.IMDb != "" {
		m.IMDb = d.ExternalIDs.IMDb
	} else if d.IMDb != "" {
		m.IMDb = d.IMDb
	}
}

// --- Checking it at home ---

func (c *catalog) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog/rows", c.serveRows)
	mux.HandleFunc("GET /catalog/movie/{id}", c.serveMovieRecord)
	mux.HandleFunc("GET /catalog/movies", c.serveMovieRecords)
	c.titleRoutes(mux) // catalogtitles.go: every watched or link-tested movie and show
}

// serveRows: today's rows with their first 20 movies, for checking the catalog from the home network.
func (c *catalog) serveRows(w http.ResponseWriter, r *http.Request) {
	if !fromHome(r) {
		http.NotFound(w, r)
		return
	}
	type movieOut struct {
		ID         int      `json:"id"`
		Title      string   `json:"title"`
		Year       int      `json:"year"`
		Tags       []string `json:"tags"`
		IMDbRating float64  `json:"imdbRating,omitempty"`
		OscarWins  int      `json:"oscarWins,omitempty"`
		Series     string   `json:"series,omitempty"`
	}
	type rowOut struct {
		ID     string     `json:"id"`
		Name   string     `json:"name"`
		Count  int        `json:"count"`
		Movies []movieOut `json:"movies"`
	}
	c.mu.Lock()
	total, last, sources := len(c.movies), c.lastRefresh, c.lastSources
	c.mu.Unlock()
	out := struct {
		On          bool      `json:"on"`
		Movies      int       `json:"movies"`
		LastRefresh time.Time `json:"lastRefresh"`
		LastSources time.Time `json:"lastSources"`
		Rows        []rowOut  `json:"rows"`
	}{On: c.on(), Movies: total, LastRefresh: last, LastSources: sources}
	for _, row := range c.rows() {
		ro := rowOut{ID: row.ID, Name: row.Name, Count: len(row.Movies)}
		for _, m := range row.Movies[:min(len(row.Movies), catalogFirstScreen)] {
			ro.Movies = append(ro.Movies, movieOut{m.ID, m.Title, m.Year(), m.Tags, m.IMDbRating, m.OscarWins, m.Series})
		}
		out.Rows = append(out.Rows, ro)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		hubLog.event("catalog_serve_failed", "error", err)
	}
}
