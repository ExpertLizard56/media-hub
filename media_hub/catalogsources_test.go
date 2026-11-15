package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSourcesIMDbAndWikidata(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("tconst\taverageRating\tnumVotes\ntt0000001\t8.7\t2100000\ntt0000002\t6.1\t50\ntt0099999\t9.9\t9\n"))
	_ = zw.Close()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ratings.tsv.gz":
			_, _ = w.Write(gz.Bytes())
		case "/sparql":
			_ = r.ParseForm()
			q := r.PostForm.Get("query")
			queries = append(queries, q)
			if r.Header.Get("User-Agent") == "" || !strings.Contains(q, `"tt0000001"`) {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/sparql-results+json")
			_, _ = w.Write([]byte(`{"results":{"bindings":[
 {"imdb":{"value":"tt0000001"},"item":{"value":"http://www.wikidata.org/entity/Q83495"},"kind":{"value":"win"},"v":{"value":"http://www.wikidata.org/entity/Q1"}},
 {"imdb":{"value":"tt0000001"},"item":{"value":"http://www.wikidata.org/entity/Q83495"},"kind":{"value":"win"},"v":{"value":"http://www.wikidata.org/entity/Q2"}},
 {"imdb":{"value":"tt0000001"},"item":{"value":"http://www.wikidata.org/entity/Q83495"},"kind":{"value":"win"},"v":{"value":"http://www.wikidata.org/entity/Q2"}},
 {"imdb":{"value":"tt0000001"},"item":{"value":"http://www.wikidata.org/entity/Q83495"},"kind":{"value":"series"},"v":{"value":"The Matrix series"}},
 {"imdb":{"value":"tt0000002"},"item":{"value":"http://www.wikidata.org/entity/Q7"},"kind":{"value":"book"},"v":{"value":"http://www.wikidata.org/entity/Q8"}},
 {"imdb":{"value":"tt0000002"},"item":{"value":"http://www.wikidata.org/entity/Q7"},"kind":{"value":"true"},"v":{"value":"http://www.wikidata.org/entity/Q645928"}},
 {"imdb":{"value":"tt0000003"},"item":{"value":"http://www.wikidata.org/entity/Q9"}}
]}}`))
		}
	}))
	defer srv.Close()
	now := time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local)
	c := testCatalog(t, now)
	c.imdbRatings, c.wikidata, c.wikidataPause = srv.URL+"/ratings.tsv.gz", srv.URL+"/sparql", 0
	c.movies = map[int]*catalogMovie{
		1: {ID: 1, IMDb: "tt0000001", Title: "One", Released: "1999-03-31", Rating: 7.9, Votes: 20000, GenreIDs: []int{28}},
		2: {ID: 2, IMDb: "tt0000002", Title: "Two", Released: "2001-01-01", Rating: 7, Votes: 400},
		3: {ID: 3, IMDb: "tt0000003", Title: "Three", Released: "2001-01-01"},
		4: {ID: 4, IMDb: "bad id", Title: "Four", Released: "2001-01-01"},
		5: {ID: 5, Title: "No IMDb", Released: "2001-01-01"},
		6: {ID: 6, IMDb: "tt0000006", Title: "Unknown to Wikidata", Released: "2001-01-01"},
	}
	c.refreshSources()

	one, _ := c.movie(1)
	if one.IMDbRating != 8.7 || one.IMDbVotes != 2100000 || one.OscarWins != 2 || one.Series != "The Matrix series" ||
		one.Wikidata != "Q83495" || !slices.Contains(one.Tags, tagOscarWinners) {
		t.Errorf("movie 1: %+v", one)
	}
	if r, v := one.bestRating(); r != 8.7 || v != 2100000 {
		t.Errorf("movie 1 should sort by IMDb: %v %v", r, v)
	}
	two, _ := c.movie(2)
	if !two.BasedOnBook || !two.TrueStory || !slices.Contains(two.Tags, tagBasedOnBook) || !slices.Contains(two.Tags, tagTrueStory) {
		t.Errorf("movie 2: %+v", two)
	}
	if r, v := two.bestRating(); r != 7 || v != 400 {
		t.Errorf("movie 2 has more TMDB votes, should sort by TMDB: %v %v", r, v)
	}
	three, _ := c.movie(3)
	if three.Wikidata != "Q9" || three.WikidataChecked.IsZero() || len(sourceTags(three)) != 0 {
		t.Errorf("movie 3: %+v", three)
	}
	if six, _ := c.movie(6); six.Wikidata != "" || six.WikidataChecked.IsZero() {
		t.Errorf("movie 6 (not on Wikidata): %+v", six)
	}
	if four, _ := c.movie(4); !four.WikidataChecked.IsZero() {
		t.Errorf("a bad IMDb id must not be asked about")
	}
	if len(queries) != 1 || strings.Contains(queries[0], "bad id") {
		t.Fatalf("queries: %d", len(queries))
	}

	// Saved, and not asked again within catalogWikidataDays.
	again := newCatalog("", c.dir)
	if m := again.movies[1]; m == nil || m.OscarWins != 2 || again.lastSources.IsZero() {
		t.Fatalf("not saved: %+v", m)
	}
	c.now = func() time.Time { return now.AddDate(0, 0, 1) }
	c.refreshSources()
	if len(queries) != 1 {
		t.Errorf("asked Wikidata again the next day")
	}

	// Stremio shows IMDb's rating and the Oscars.
	meta := stremioMeta(one, true)
	if meta["imdbRating"] != "8.7" || meta["awards"] != "Won 2 Academy Awards" {
		t.Errorf("stremio meta: %v", meta)
	}
}

func TestSourcesFailingKeepsGoing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.imdbRatings, c.wikidata, c.wikidataPause = srv.URL+"/r", srv.URL+"/s", 0
	c.movies = map[int]*catalogMovie{1: {ID: 1, IMDb: "tt0000001", Title: "One", Released: "1999-03-31", Rating: 7.9, Votes: 20000}}
	c.refreshSources()
	m, _ := c.movie(1)
	if m.IMDbRating != 0 || !m.WikidataChecked.IsZero() || c.lastSources.IsZero() {
		t.Errorf("after failures: %+v lastSources=%v", m, c.lastSources)
	}
}
