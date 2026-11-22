package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTitleRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("plays.jsonl", "\xef\xbb\xbf"+`{"time":"2026-11-08T15:18:59","app":"com.stremio.one","title":"The Walking Dead, Chupacabra","started":false}
{"time":"2026-11-08T15:20:39","app":"com.stremio.one","content_id":"tt1520211:2:5","title":"The Walking Dead, Chupacabra","episode":"S02E05","started":true,"start_s":4}
{"time":"2026-11-21T15:21:23","app":"com.stremio.one","content_id":"tt1520211:2:11","title":"The Walking Dead, Judge, Jury, Executioner","started":true,"start_s":2,"rebuffers":1}
{"time":"2026-11-21T21:33:16","app":"com.stremio.one","content_id":"tt1520211:3:1","title":"The Walking Dead, Seed","started":false}
{"time":"2026-11-21T13:07:29","app":"com.stremio.one","content_id":"tt31193180","title":"Sinners","started":true}
{"time":"2026-11-10T06:39:12","app":"org.smarttube.stable","content_id":"tt2235759","title":"Some video","started":true}
`)
	write("links.jsonl", `{"time":"2026-11-22T10:17:18","content_id":"tt0133093","filename":"The Matrix.mkv","ok":true,"resolve_s":6}
{"time":"2026-11-20T10:00:00","content_id":"tt1520211:2:12","filename":"TWD.S02E12.mkv","ok":false,"error":"timeout"}
`)
	c := &catalog{dir: dir, movies: map[int]*catalogMovie{603: {ID: 603, IMDb: "tt0133093", Title: "The Matrix", Released: "1999-03-31"}}}
	got := map[string]*titleRecord{}
	for _, tr := range c.titleRecords() {
		got[tr.ID] = tr
	}
	if len(got) != 3 || got["tt2235759"] != nil {
		t.Fatalf("want The Matrix, Sinners and The Walking Dead only (no SmartTube): %v", got)
	}
	m := got["tt0133093"]
	if m.Kind != "movie" || m.CatalogID != 603 || m.Name != "The Matrix" || m.Links == nil || m.Links.Worked != 1 || m.Updated != "2026-11-22T10:17:18" {
		t.Fatalf("catalog movie: %+v", m)
	}
	if s := got["tt31193180"]; s.Name != "Sinners" || s.CatalogID != 0 || s.Plays.Started != 1 {
		t.Fatalf("non-catalog movie: %+v", s)
	}
	w := got["tt1520211"]
	if w.Kind != "show" || w.Name != "The Walking Dead" || len(w.Episodes) != 4 || w.Updated != "2026-11-21T21:33:16" {
		b, _ := json.Marshal(w)
		t.Fatalf("show: %s", b)
	}
	if e := w.Episodes[0]; e.ID != "tt1520211:2:5" || e.Name != "Chupacabra" || e.Plays.Count != 2 || e.Plays.Started != 1 {
		t.Fatalf("first episode (with the id-less play joined by title): %+v", e)
	}
	if e := w.Episodes[1]; e.Name != "Judge, Jury, Executioner" {
		t.Fatalf("episode name with commas: %q", e.Name)
	}
	tot := w.Totals
	if tot.Episodes != 4 || tot.Played != 2 || tot.Plays != 4 || tot.Started != 2 || tot.LinkTries != 1 || tot.LastSeason != 2 || tot.LastEpisode != 11 {
		t.Fatalf("totals: %+v", tot)
	}
}

func TestTitleRecordsReal(t *testing.T) {
	dir := os.Getenv("FILES_DIR")
	if dir == "" {
		t.Skip("FILES_DIR not set")
	}
	c := &catalog{dir: dir, movies: map[int]*catalogMovie{}}
	for _, tr := range c.titleRecords() {
		n := 0
		if tr.Plays != nil {
			n = tr.Plays.Count
		}
		t.Logf("%s %s %q updated %s plays %d eps %d totals %+v", tr.ID, tr.Kind, tr.Name, tr.Updated, n, len(tr.Episodes), tr.Totals)
	}
}
