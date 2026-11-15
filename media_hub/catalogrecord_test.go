package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMovieRecords(t *testing.T) {
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.movies = map[int]*catalogMovie{
		603: {ID: 603, IMDb: "tt0133093", Title: "The Matrix", Released: "1999-03-31", Rating: 8.2, Votes: 20000,
			GenreIDs: []int{28}, IMDbRating: 8.7, OscarWins: 4},
		7: {ID: 7, Title: "No IMDb", Released: "2001-01-01"},
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("streams.json", `[{"id":"tt0133093","streams":12,"checked":"2026-03-02T10:00:00"},{"id":"tmdb:7","streams":0,"checked":"x"}]`)
	write("plays.jsonl", "\xef\xbb\xbf"+`{"content_id":"tt0133093","engine":"Exo","start_s":4.1}`+"\n"+
		"not json\n"+`{"content_id":"tt0133093:1:2","engine":"Exo"}`+"\n"+`{"content_id":null}`+"\n")
	write("links.jsonl", `{"content_id":"tt0133093","ok":true,"mbps":300}`+"\n"+`{"content_id":"tt0133093","ok":false}`+"\n")

	get := func(path string, from string) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		c.routes(mux)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = from + ":5000"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/catalog/movie/603", "/catalog/movie/tt0133093", "/catalog/movie/tmdb:603"} {
		res := get(path, "192.168.1.5")
		var rec movieRecord
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &rec) != nil {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
		if rec.ID != 603 || rec.StremioID != "tt0133093" || rec.Movie.IMDbRating != 8.7 || rec.Movie.OscarWins != 4 ||
			len(rec.Plays) != 1 || len(rec.LinkTests) != 2 || len(rec.Streams) == 0 || len(rec.Tags) == 0 || rec.Rows == nil {
			t.Errorf("%s: %s", path, res.Body.String())
		}
	}
	if res := get("/catalog/movie/603", "203.0.113.9"); res.Code != http.StatusNotFound {
		t.Errorf("from outside: %d", res.Code)
	}
	if res := get("/catalog/movie/tt9999999", "192.168.1.5"); res.Code != http.StatusNotFound {
		t.Errorf("unknown movie: %d", res.Code)
	}

	res := get("/catalog/movies", "192.168.1.5")
	var all []movieRecord
	if err := json.Unmarshal(res.Body.Bytes(), &all); err != nil || len(all) != 2 || all[0].ID != 7 || all[1].ID != 603 {
		t.Fatalf("all: %v %s", err, res.Body.String())
	}
	if all[0].StremioID != "tmdb:7" || string(all[0].Streams) == "" || all[0].Plays != nil {
		t.Errorf("movie 7: %+v", all[0])
	}

	// A changed file is read again; a removed one leaves its part out.
	write("plays.jsonl", `{"content_id":"tt0133093"}`+"\n"+`{"content_id":"tt0133093"}`+"\n"+`{"content_id":"tt0133093"}`+"\n")
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(c.dir, "plays.jsonl"), later, later)
	_ = os.Remove(filepath.Join(c.dir, "links.jsonl"))
	if r := c.records([]int{603}); len(r) != 1 || len(r[0].Plays) != 3 || r[0].LinkTests != nil {
		t.Errorf("after changes: %+v", r)
	}
}
