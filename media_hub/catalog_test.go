package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testCatalog(t *testing.T, at time.Time) *catalog {
	t.Helper()
	hubLog = &eventLog{dir: t.TempDir()} // the test never writes to the hub's real log
	c := newCatalog("", t.TempDir())
	c.now = func() time.Time { return at }
	c.pause = 0
	return c
}

func TestSeedLoads(t *testing.T) {
	c := testCatalog(t, time.Now())
	if len(c.seed.Tags) < 1000 || len(c.seed.Holidays) < 5 {
		t.Fatalf("seed: %d tagged movies, %d holidays", len(c.seed.Tags), len(c.seed.Holidays))
	}
}

func TestHolidaySpan(t *testing.T) {
	cases := []struct {
		start, end, day string
		in              bool
	}{
		{"10-01", "11-02", "10-31", true},
		{"10-01", "11-02", "11-03", false},
		{"12-26", "01-05", "12-31", true},
		{"12-26", "01-05", "01-03", true},
		{"12-26", "01-05", "01-06", false},
		{"12-26", "01-05", "12-25", false},
	}
	for _, tc := range cases {
		if _, in := holidaySpan(tc.start, tc.end, tc.day); in != tc.in {
			t.Errorf("%s..%s on %s: got %v", tc.start, tc.end, tc.day, in)
		}
	}
	if n, _ := holidaySpan("12-26", "01-05", "12-30"); n != 11 {
		t.Errorf("wrapping span length %d, want 11", n)
	}
}

func TestHolidayTodayPicksShortest(t *testing.T) {
	c := testCatalog(t, time.Now())
	c.seed.Holidays = []catalogHoliday{
		{Name: "Summer", Start: "06-01", End: "08-31"},
		{Name: "Independence Day", Start: "06-25", End: "07-10"},
	}
	if h := c.holidayToday(time.Date(2026, 7, 4, 12, 0, 0, 0, time.Local)); h == nil || h.Name != "Independence Day" {
		t.Fatalf("July 4th: %+v", h)
	}
	if h := c.holidayToday(time.Date(2026, 8, 4, 12, 0, 0, 0, time.Local)); h == nil || h.Name != "Summer" {
		t.Fatalf("Aug 4th: %+v", h)
	}
	if h := c.holidayToday(time.Date(2026, 9, 4, 12, 0, 0, 0, time.Local)); h != nil {
		t.Fatalf("Sep 4th: %+v", h)
	}
}

func TestTagsAndCorrections(t *testing.T) {
	c := testCatalog(t, time.Now())
	c.seed = catalogSeed{
		Tags:     map[string][]string{"1": {"Animation (Kids)"}},
		Holidays: []catalogHoliday{{Name: "Christmas", Start: "11-20", End: "12-25", IDs: []int{1}}},
	}
	seeded := &catalogMovie{ID: 1, GenreIDs: []int{16, 10751, 35}}
	if got, want := c.tagsFor(seeded), []string{"Animation (Kids)", "Family", "Comedy", "Christmas"}; !reflect.DeepEqual(got, want) {
		t.Errorf("seeded: %v, want %v", got, want)
	}
	if got, want := c.tagsFor(&catalogMovie{ID: 2, GenreIDs: []int{16, 10751}}), []string{"Animation (Kids)", "Family"}; !reflect.DeepEqual(got, want) {
		t.Errorf("family animation: %v, want %v", got, want)
	}
	if got, want := c.tagsFor(&catalogMovie{ID: 3, GenreIDs: []int{16, 18}}), []string{"Animation", "Drama"}; !reflect.DeepEqual(got, want) {
		t.Errorf("animation: %v, want %v", got, want)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "corrections.json"), []byte(`{"1": ["Horror"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c.loadCorrectionsLocked(true)
	if got := c.tagsFor(seeded); !reflect.DeepEqual(got, []string{"Horror"}) {
		t.Errorf("corrected: %v", got)
	}
}

func TestLineupSameAllDayAndPushDown(t *testing.T) {
	day := time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local)
	c := testCatalog(t, day)
	c.seed = catalogSeed{Tags: map[string][]string{}}
	for i := 1; i <= 60; i++ {
		c.movies[i] = &catalogMovie{ID: i, Title: "M", Released: "2001-01-01", Rating: 7, Votes: 1000 * i, GenreIDs: []int{27}}
	}
	first, ok := c.row("tag-horror")
	if !ok || len(first.Movies) != 60 {
		t.Fatalf("row: ok=%v len=%d", ok, len(first.Movies))
	}
	c.now = func() time.Time { return day.Add(10 * time.Hour) }
	c.lineupDay = "" // as if worked out again later the same day
	again, _ := c.row("tag-horror")
	for i := range first.Movies {
		if first.Movies[i].ID != again.Movies[i].ID {
			t.Fatalf("order changed within the day at %d", i)
		}
	}
	top := map[int]bool{}
	for _, m := range first.Movies[:catalogFirstScreen] {
		top[m.ID] = true
	}
	c.now = func() time.Time { return day.AddDate(0, 0, 1) }
	next, _ := c.row("tag-horror")
	stayed := 0
	for _, m := range next.Movies[:catalogFirstScreen] {
		if top[m.ID] {
			stayed++
		}
	}
	if stayed > catalogFirstScreen/2 {
		t.Errorf("%d of yesterday's first screen are still on it; they should be pushed down", stayed)
	}
}

func TestRowsLeaveOutSmallTagsAndUnreleased(t *testing.T) {
	now := time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local)
	c := testCatalog(t, now)
	c.seed = catalogSeed{Tags: map[string][]string{}}
	for i := 1; i <= 10; i++ {
		c.movies[i] = &catalogMovie{ID: i, Released: "2000-01-01", GenreIDs: []int{37}}
	}
	for i := 11; i <= 13; i++ {
		c.movies[i] = &catalogMovie{ID: i, Released: "2000-01-01", GenreIDs: []int{10752}}
	}
	c.movies[14] = &catalogMovie{ID: 14, Released: "2026-02-20", GenreIDs: []int{37}}
	c.movies[15] = &catalogMovie{ID: 15, Released: "2026-12-01", GenreIDs: []int{37}}
	var ids []string
	for _, r := range c.rows() {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "new,tag-western" {
		t.Fatalf("rows %s", got)
	}
	west, _ := c.row("tag-western")
	if len(west.Movies) != 11 {
		t.Errorf("western has %d movies, want 11 (the unreleased one left out)", len(west.Movies))
	}
}

func TestRefreshFromTMDB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "abc" {
			http.Error(w, "no key", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/discover/movie":
			_, _ = w.Write([]byte(`{"results":[{"id":500,"title":"Found","release_date":"1999-05-01","vote_average":8,"vote_count":900,"genre_ids":[27]}]}`))
		case strings.HasPrefix(r.URL.Path, "/movie/500"):
			_, _ = w.Write([]byte(`{"id":500,"title":"Found","release_date":"1999-05-01","runtime":101,"genres":[{"id":27}],"external_ids":{"imdb_id":"tt0000500"}}`))
		case strings.HasPrefix(r.URL.Path, "/movie/"):
			http.Error(w, `{"status_code":34}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.key, c.tmdb = "abc", srv.URL
	c.seed = catalogSeed{Tags: map[string][]string{"77": {"Drama"}}}
	c.refreshNow()
	m, ok := c.movie(500)
	if !ok || m.IMDb != "tt0000500" || m.Runtime != 101 || m.Year() != 1999 {
		t.Fatalf("movie 500: %+v", m)
	}
	if _, ok := c.movie(77); ok {
		t.Errorf("a seeded movie TMDB doesn't know should be dropped")
	}
	again := newCatalog("abc", c.dir)
	if _, ok := again.movie(500); !ok || again.lastRefresh.IsZero() {
		t.Errorf("not saved: ok=%v lastRefresh=%v", ok, again.lastRefresh)
	}
}

func TestStremioCatalog(t *testing.T) {
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.key = "x"
	c.seed = catalogSeed{Tags: map[string][]string{}}
	for i := 1; i <= 130; i++ {
		c.movies[i] = &catalogMovie{ID: i, Title: "M", Released: "2000-01-01", GenreIDs: []int{37}}
	}
	c.movies[1].IMDb = "tt1"
	mux := http.NewServeMux()
	stremioAddon{c}.routes(mux)
	get := func(path string, v any) {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	var man struct {
		Version  string
		Catalogs []struct{ ID string }
	}
	get("/stremio/manifest.json", &man)
	if man.Version == "" || len(man.Catalogs) == 0 {
		t.Fatalf("manifest: %+v", man)
	}
	var page struct{ Metas []struct{ ID string } }
	get("/stremio/catalog/movie/tag-western.json", &page)
	if len(page.Metas) != stremioPage {
		t.Fatalf("first page %d", len(page.Metas))
	}
	get("/stremio/catalog/movie/tag-western/skip=100.json", &page)
	if len(page.Metas) != 30 {
		t.Fatalf("second page %d", len(page.Metas))
	}
	var meta struct{ Meta struct{ ID, Name string } }
	get("/stremio/meta/movie/tmdb:5.json", &meta)
	if meta.Meta.ID != "tmdb:5" {
		t.Fatalf("meta %+v", meta)
	}
}

func TestNoStreamsLeftOut(t *testing.T) {
	c := testCatalog(t, time.Date(2026, 3, 3, 9, 0, 0, 0, time.Local))
	c.seed = catalogSeed{Tags: map[string][]string{}}
	for i := 1; i <= 12; i++ {
		c.movies[i] = &catalogMovie{ID: i, IMDb: "tt" + strconv.Itoa(i), Released: "2000-01-01", GenreIDs: []int{37}}
	}
	c.movies[12].IMDb = ""
	// 1: none found (left out); 2: an error (stays); 3: not checked yet (stays); 12: tmdb id, none found.
	file := "\xef\xbb\xbf" + `[{"id":"tt1","streams":0,"error":null,"checked":"x"},{"id":"tt2","streams":0,"error":"timeout","checked":"x"},
		{"id":"tt3"},{"id":"tt4","streams":5,"error":null,"checked":"x"},{"id":"tmdb:12","streams":0,"checked":"x"}]`
	if err := os.WriteFile(filepath.Join(c.dir, "streams.json"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	west, _ := c.row("tag-western")
	got := map[int]bool{}
	for _, m := range west.Movies {
		got[m.ID] = true
	}
	if len(west.Movies) != 10 || got[1] || got[12] || !got[2] || !got[3] {
		t.Fatalf("western: %d movies %v", len(west.Movies), got)
	}
}
