package main

// Facts about the catalog's movies from outside TMDB (catalog.go), looked up by IMDb id once a day,
// after TMDB's refresh (or on its own on a day the refresh already ran, e.g. after an update):
//
//   - IMDb ratings and vote counts: IMDb's daily data file title.ratings.tsv.gz (about 9 MB, free for
//     personal, non-commercial use), read whole each day and kept only for the catalog's movies.
//   - Wikidata (its SPARQL endpoint, catalogWikidataBatch movies a query, catalogWikidataPause apart):
//     Academy Award wins and nominations, based on a book, based on actual events (genre biographical
//     film, film based on actual events or docudrama) and the film series. Asked again after
//     catalogWikidataDays.
//
// They add tags (so rows) "Oscar Winners", "Based on a Book" and "Based on a True Story"; IMDb's rating
// sorts the rows when it has more votes than TMDB's, and is the rating Stremio shows.
// No keys are needed; a source that doesn't answer is tried again the next day and the rest carries on.

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	catalogWikidataBatch = 150
	catalogWikidataPause = 2 * time.Second
	catalogWikidataDays  = 30
)

// Tags the sources give.
const (
	tagOscarWinners = "Oscar Winners"
	tagBasedOnBook  = "Based on a Book"
	tagTrueStory    = "Based on a True Story"
)

var imdbIDPattern = regexp.MustCompile(`^tt\d{5,10}$`)

// sourceTags: the tags a movie gets from the sources.
func sourceTags(m *catalogMovie) []string {
	var t []string
	if m.OscarWins > 0 {
		t = append(t, tagOscarWinners)
	}
	if m.BasedOnBook {
		t = append(t, tagBasedOnBook)
	}
	if m.TrueStory {
		t = append(t, tagTrueStory)
	}
	return t
}

// bestRating: the rating and vote count to sort by, IMDb's when it has more votes than TMDB's.
func (m *catalogMovie) bestRating() (float64, int) {
	if m.IMDbRating > 0 && m.IMDbVotes > m.Votes {
		return m.IMDbRating, m.IMDbVotes
	}
	return m.Rating, m.Votes
}

// sourcesUserAgent: Wikidata asks every client to name itself.
func sourcesUserAgent() string { return "MediaHub/" + version + " (home media server)" }

// refreshSources runs both sources and saves.
func (c *catalog) refreshSources() {
	start := c.now()
	rated, ratedErr := c.refreshIMDbRatings()
	checked, found, wdErr := c.refreshWikidata(start)
	c.mu.Lock()
	c.lastSources = start
	c.lineupDay = "" // ratings and tags changed: the lineups are worked out again
	c.mu.Unlock()
	c.writeState()
	c.save()
	args := []any{"imdb_rated", rated, "wikidata_checked", checked, "wikidata_found", found,
		"took", c.now().Sub(start).Round(time.Second)}
	if ratedErr != nil {
		args = append(args, "imdb_error", ratedErr)
	}
	if wdErr != nil {
		args = append(args, "wikidata_error", wdErr)
	}
	hubLog.event("catalog_sources", args...)
}

// refreshIMDbRatings reads IMDb's ratings file and sets the rating of every catalog movie in it.
func (c *catalog) refreshIMDbRatings() (int, error) {
	c.mu.Lock()
	want := map[string]bool{}
	for _, m := range c.movies {
		if m.IMDb != "" {
			want[m.IMDb] = true
		}
	}
	c.mu.Unlock()
	if len(want) == 0 {
		return 0, nil
	}
	req, err := http.NewRequest(http.MethodGet, c.imdbRatings, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", sourcesUserAgent())
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("IMDb ratings file: %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return 0, err
	}
	type rating struct {
		r float64
		v int
	}
	got := map[string]rating{}
	sc := bufio.NewScanner(gz)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 || !want[f[0]] {
			continue
		}
		r, err1 := strconv.ParseFloat(f[1], 64)
		v, err2 := strconv.Atoi(f[2])
		if err1 == nil && err2 == nil {
			got[f[0]] = rating{r, v}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("IMDb ratings file: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.movies {
		if g, ok := got[m.IMDb]; ok {
			m.IMDbRating, m.IMDbVotes = g.r, g.v
		}
	}
	return len(got), nil
}

// refreshWikidata asks Wikidata about movies not asked in catalogWikidataDays, a batch at a time.
// It returns how many were asked and how many Wikidata knew; it stops at the first failure.
func (c *catalog) refreshWikidata(start time.Time) (int, int, error) {
	c.mu.Lock()
	var due []string
	for _, m := range c.movies {
		if imdbIDPattern.MatchString(m.IMDb) &&
			(m.WikidataChecked.IsZero() || start.Sub(m.WikidataChecked) > catalogWikidataDays*24*time.Hour) {
			due = append(due, m.IMDb)
		}
	}
	c.mu.Unlock()
	sort.Strings(due)
	checked, found := 0, 0
	for i := 0; i < len(due); i += catalogWikidataBatch {
		if i > 0 {
			time.Sleep(c.wikidataPause)
		}
		batch := due[i:min(len(due), i+catalogWikidataBatch)]
		facts, err := c.askWikidata(batch)
		if err != nil {
			return checked, found, err
		}
		c.mu.Lock()
		byIMDb := map[string][]*catalogMovie{}
		for _, m := range c.movies {
			byIMDb[m.IMDb] = append(byIMDb[m.IMDb], m)
		}
		for _, id := range batch {
			f, ok := facts[id]
			if ok {
				found++
			} else {
				f = &wikidataFacts{} // Wikidata doesn't know it: nothing to add, asked again later
			}
			for _, m := range byIMDb[id] {
				m.Wikidata, m.OscarWins, m.OscarNoms = f.item, len(f.wins), len(f.noms)
				m.BasedOnBook, m.TrueStory, m.Series = f.book, f.trueStory, f.series
				m.WikidataChecked = start
			}
		}
		c.mu.Unlock()
		checked += len(batch)
	}
	return checked, found, nil
}

type wikidataFacts struct {
	item            string
	wins, noms      map[string]bool
	book, trueStory bool
	series          string
}

// askWikidata: one SPARQL query for a batch of IMDb ids (checked against imdbIDPattern by the caller).
func (c *catalog) askWikidata(ids []string) (map[string]*wikidataFacts, error) {
	var values strings.Builder
	for _, id := range ids {
		if imdbIDPattern.MatchString(id) {
			values.WriteString(` "` + id + `"`)
		}
	}
	// Academy Award categories are instances of Q19020; film series Q24856; based on: novel, literary
	// work, book, short story, novella, written work, comic book, play; genres biographical film, film
	// based on actual events, docudrama.
	query := `SELECT ?imdb ?item ?kind ?v WHERE {
  VALUES ?imdb {` + values.String() + ` }
  ?item wdt:P345 ?imdb .
  OPTIONAL {
    { ?item wdt:P166 ?v . ?v wdt:P31 wd:Q19020 . BIND("win" AS ?kind) }
    UNION { ?item wdt:P1411 ?v . ?v wdt:P31 wd:Q19020 . BIND("nom" AS ?kind) }
    UNION { ?item wdt:P179 ?s . ?s wdt:P31 wd:Q24856 . ?s rdfs:label ?v . FILTER(LANG(?v) = "en") BIND("series" AS ?kind) }
    UNION { ?item wdt:P144 ?v . ?v wdt:P31 ?bt .
            VALUES ?bt { wd:Q8261 wd:Q7725634 wd:Q571 wd:Q49084 wd:Q149537 wd:Q47461344 wd:Q1004 wd:Q25379 }
            BIND("book" AS ?kind) }
    UNION { ?item wdt:P136 ?v . VALUES ?v { wd:Q645928 wd:Q28146524 wd:Q622370 } BIND("true" AS ?kind) }
  }
}`
	req, err := http.NewRequest(http.MethodPost, c.wikidata, strings.NewReader(url.Values{"query": {query}}.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/sparql-results+json")
	req.Header.Set("User-Agent", sourcesUserAgent())
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("Wikidata: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Results struct {
			Bindings []map[string]struct {
				Value string `json:"value"`
			} `json:"bindings"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("Wikidata: %v", err)
	}
	facts := map[string]*wikidataFacts{}
	for _, b := range out.Results.Bindings {
		id := b["imdb"].Value
		f := facts[id]
		if f == nil {
			f = &wikidataFacts{wins: map[string]bool{}, noms: map[string]bool{}}
			facts[id] = f
		}
		item := b["item"].Value
		if i := strings.LastIndex(item, "/"); i >= 0 {
			item = item[i+1:]
		}
		if f.item == "" || item < f.item {
			f.item = item
		}
		v := b["v"].Value
		switch b["kind"].Value {
		case "win":
			f.wins[v] = true
		case "nom":
			f.noms[v] = true
		case "book":
			f.book = true
		case "true":
			f.trueStory = true
		case "series":
			if f.series == "" || v < f.series {
				f.series = v
			}
		}
	}
	return facts, nil
}
