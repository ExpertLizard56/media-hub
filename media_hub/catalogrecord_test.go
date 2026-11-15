package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMovieRecords(t *testing.T) {
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.movies = map[int]*catalogMovie{
		603: {ID: 603, IMDb: "tt0133093", Title: "The Matrix", Released: "1999-03-31", Rating: 8.2, Votes: 20000,
			GenreIDs: []int{28}, IMDbRating: 8.7, IMDbVotes: 2000000, OscarWins: 4, Poster: "/p.jpg",
			WikidataChecked: time.Date(2026, 3, 1, 8, 0, 0, 0, time.Local)},
		7: {ID: 7, Title: "No IMDb", Released: "2001-01-01", Added: time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)},
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("streams.json", `[{"id":"tt0133093","title":"The Matrix","streams":12,"streams_4k":3,"top":"Comet 2160p","seconds":2.5,"error":null,"checked":"2026-03-02T10:00:00"},
		{"id":"tmdb:7","streams":0,"error":null,"checked":"2026-03-02T11:00:00"}]`)
	write("plays.jsonl", "\xef\xbb\xbf"+
		`{"content_id":"tt0133093","time":"2026-03-01T20:00:00","engine":"Exo","start_s":4,"started":true,"rebuffers":2,"played_rank":1,"offered":[{"x":1}],"ram":"4GB","earlier_plays":["19:58:00 Exo, NEVER STARTED"],"served":{"host":"tb1","file":"Matrix.mkv","bytes":20000000000}}`+"\n"+
		"not json\n"+
		`{"content_id":"tt0133093","time":"2026-03-02T21:00:00","engine":"Exo","start_s":2.5,"started":null,"rebuffers":null,"torbox":{"file":"Matrix.2160p.mkv","file_gb":55.1},"media_hub":{"row":"oscar-winners"}}`+"\n"+
		`{"content_id":"tt0133093:1:2","engine":"Exo"}`+"\n"+`{"content_id":null}`+"\n")
	write("links.jsonl",
		`{"content_id":"tt0133093","time":"2026-03-01T10:00:00","name":"[TB⚡] Comet 2160p","description":"🔍 StremThru | 🌎 en","resolution":"2160p","file":"A.mkv","rank":2,"ok":true,"mbps":300,"resolve_s":1.5,"bytes":30000000000}`+"\n"+
			`{"content_id":"tt0133093","time":"2026-03-02T10:00:00","name":"[TB⚡] Comet 2160p","file":"A.mkv","rank":1,"ok":true,"mbps":200.5,"resolve_s":2.5}`+"\n"+
			`{"content_id":"tt0133093","time":"2026-03-02T11:00:00","name":"[TB] MediaFusion 1080p","file":"B.mkv","offered_bytes":null,"ok":false,"error":"timeout"}`+"\n")

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
		body := res.Body.String()
		if rec.ID != 603 || rec.IMDb != "tt0133093" || rec.Year != 1999 || rec.Poster != "https://image.tmdb.org/t/p/w500/p.jpg" ||
			rec.Ratings.IMDb == nil || rec.Ratings.SortBy != "imdb" || rec.Facts == nil || rec.Facts.OscarWins != 4 ||
			len(rec.Catalog.Tags) == 0 || rec.Catalog.Rows == nil || rec.Updated != "2026-03-02T21:00:00" {
			t.Errorf("%s: %s", path, body)
		}
		// Nothing from other sections repeated, nothing big copied over.
		for _, gone := range []string{`"offered"`, `"ram"`, `"stremioId"`, `"movie"`, `"The Matrix","streams"`} {
			if strings.Contains(body, gone) {
				t.Errorf("%s: has %s", path, gone)
			}
		}
		if s := rec.Streams; s == nil || s.Count != 12 || s.Count4K != 3 || s.Top != "Comet 2160p" || s.Error != "" {
			t.Errorf("streams: %+v", s)
		}
		p := rec.Plays
		if p == nil || p.Count != 2 || p.Started != 1 || p.Rebuffers != 2 || p.AvgStartS != 3.3 || p.Last != "2026-03-02T21:00:00" ||
			p.List[0].File != "Matrix.2160p.mkv" || p.List[0].GB != 55.1 || p.List[0].Row != "oscar-winners" ||
			p.List[1].File != "Matrix.mkv" || p.List[1].GB != 20 || p.List[1].FailedTries != 1 || p.List[1].Rank != 1 {
			t.Errorf("plays: %+v", p)
		}
		l := rec.Links
		if l == nil || l.Tries != 3 || l.Worked != 2 || l.MedianMbps != 250.25 || l.MedianResolve != 2 || len(l.Files) != 2 ||
			l.Files[0].File != "A.mkv" || l.Files[0].Addon != "Comet" || l.Files[0].Indexer != "StremThru" || l.Files[0].GB != 30 ||
			l.Files[0].BestRank != 1 || l.Files[0].BestMbps != 300 || l.Files[1].Addon != "MediaFusion" || l.Files[1].LastError != "timeout" {
			t.Errorf("link tests: %+v", l)
		}
	}
	if res := get("/catalog/movie/603", "203.0.113.9"); res.Code != http.StatusNotFound {
		t.Errorf("from outside: %d", res.Code)
	}
	if res := get("/catalog/movie/tt9999999", "192.168.1.5"); res.Code != http.StatusNotFound {
		t.Errorf("unknown movie: %d", res.Code)
	}

	var all struct {
		Generated string
		Count     int
		Movies    []movieRecord
	}
	res := get("/catalog/movies", "192.168.1.5")
	if err := json.Unmarshal(res.Body.Bytes(), &all); err != nil || all.Count != 2 || len(all.Movies) != 2 ||
		all.Movies[0].ID != 7 || all.Movies[1].ID != 603 || all.Generated == "" {
		t.Fatalf("all: %v %s", err, res.Body.String())
	}
	if m := all.Movies[0]; m.Streams == nil || !m.Catalog.NoStreams || m.Plays != nil || m.Facts != nil || m.Ratings.SortBy != "tmdb" {
		t.Errorf("movie 7: %+v", m)
	}
	res = get("/catalog/movies?since=2026-03-02T12:00:00", "192.168.1.5")
	if err := json.Unmarshal(res.Body.Bytes(), &all); err != nil || all.Count != 1 || all.Movies[0].ID != 603 {
		t.Errorf("since: %v %s", err, res.Body.String())
	}

	// A changed file is read again; a removed one leaves its part out.
	write("plays.jsonl", `{"content_id":"tt0133093"}`+"\n"+`{"content_id":"tt0133093"}`+"\n"+`{"content_id":"tt0133093"}`+"\n")
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(c.dir, "plays.jsonl"), later, later)
	_ = os.Remove(filepath.Join(c.dir, "links.jsonl"))
	if r := c.records([]int{603}); len(r) != 1 || r[0].Plays == nil || r[0].Plays.Count != 3 || r[0].Links != nil {
		t.Errorf("after changes: %+v", r)
	}
}
