package main

// Knowing what an album is before getting it. MusicBrainz, the open music database, says what
// albums exist: their artist, year, cover and track list. The app searches that, not Usenet's
// file names; for a chosen album the hub finds the Usenet copies that really are it
// (musicmatch.go), and a fetched album keeps its MusicBrainz details beside its songs
// (album.json), which the library trusts over the files' own tags.
//
//   - GET /music/lookup?q=…        albums (and EPs) matching q; just an artist's name lists theirs
//   - GET /music/album/{id}        one album: its tracks, which you have, and its Usenet copies
//   - GET /music/cover/{id}        its cover, kept by the hub once fetched (covers/)
//
// MusicBrainz asks for one request a second at most, and a name for who's asking.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	musicbrainzAPI = "https://musicbrainz.org/ws/2"
	coverArchive   = "https://coverartarchive.org/release-group"
	mbUserAgent    = "MediaHub/1 (home music server)"
)

var (
	coversDir = filepath.Join(root, "covers")
	mbid      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// albumInfo is an album as MusicBrainz knows it, and as album.json keeps it beside the songs.
type albumInfo struct {
	ID         string      `json:"id"` // the MusicBrainz release group
	Title      string      `json:"title"`
	Artist     string      `json:"artist"`
	Year       int         `json:"year,omitempty"`
	Type       string      `json:"type,omitempty"` // Album, EP, Single
	Releases   int         `json:"-"`              // how many editions: how well known it is
	Votes      int         `json:"-"`              // how many rated it on MusicBrainz: the same, in a discography
	Secondary  bool        `json:"-"`              // a live album, compilation, remix, demo…
	Kind       string      `json:"kind,omitempty"` // which of those: Live, Compilation, Soundtrack…
	Group      string      `json:"group,omitempty"` // the search answer's heading it goes under (lookup answers only)
	Tracks     []albumSong `json:"tracks,omitempty"`
	DurationMs int64       `json:"durationMs,omitempty"`
	InLibrary  bool        `json:"inLibrary,omitempty"` // songs of it are in the library (lookup answers only)
}

type albumSong struct {
	Disc       int    `json:"disc"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Have       bool   `json:"have,omitempty"` // in the library already (the answer only)
}

type musicbrainz struct {
	http *http.Client
	mu   sync.Mutex // one request at a time, a second apart
	last time.Time

	cacheMu sync.Mutex
	cache   map[string]cached // answers kept a day: the same album opened twice asks once
}

type cached struct {
	at   time.Time
	body []byte
}

func newMusicBrainz() *musicbrainz {
	if err := os.MkdirAll(coversDir, 0o755); err != nil {
		log.Printf("musicbrainz: covers folder: %v", err)
	}
	return &musicbrainz{http: &http.Client{Timeout: 20 * time.Second}, cache: map[string]cached{}}
}

// get asks MusicBrainz, a second after the last question at the earliest.
func (m *musicbrainz) get(path string, q url.Values, out any) error {
	q.Set("fmt", "json")
	u := musicbrainzAPI + path + "?" + q.Encode()
	m.cacheMu.Lock()
	c, ok := m.cache[u]
	if len(m.cache) > 2000 {
		m.cache = map[string]cached{}
	}
	m.cacheMu.Unlock()
	if ok && time.Since(c.at) < 24*time.Hour {
		return json.Unmarshal(c.body, out)
	}
	body, err := m.fetch(u)
	if err != nil {
		return err
	}
	m.cacheMu.Lock()
	m.cache[u] = cached{time.Now(), body}
	m.cacheMu.Unlock()
	return json.Unmarshal(body, out)
}

// fetch asks once a second at most. "Busy" (503, 429) is MusicBrainz's way of saying slow down:
// asked again after a pause, up to three times, so a busy moment isn't an error on the phone.
func (m *musicbrainz) fetch(u string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for attempt := 1; ; attempt++ {
		if wait := time.Second - time.Since(m.last); wait > 0 {
			time.Sleep(wait)
		}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", mbUserAgent)
		req.Header.Set("Accept", "application/json")
		resp, err := m.http.Do(req)
		m.last = time.Now()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		closeQuietly(resp.Body) // its content is read above; a close error has nothing to add
		if err != nil {
			return nil, err
		}
		busy := resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests
		if busy && attempt < 3 {
			hubLog.event("musicbrainz_busy", "status", resp.StatusCode, "attempt", attempt)
			time.Sleep(time.Duration(attempt) * 1500 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("MusicBrainz said HTTP %d", resp.StatusCode)
		}
		return body, nil
	}
}

// ---- MusicBrainz's answers, the parts used ----

type mbCredit []struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
	Artist     struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"artist"`
}

func (c mbCredit) String() string {
	var b strings.Builder
	for _, a := range c {
		b.WriteString(a.Name + a.JoinPhrase)
	}
	return strings.TrimSpace(b.String())
}

type mbGroup struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Primary     string   `json:"primary-type"`
	Secondary   []string `json:"secondary-types"`
	FirstDate   string   `json:"first-release-date"`
	Count       int      `json:"count"`
	Rating      struct {
		Votes int `json:"votes-count"`
	} `json:"rating"`
	Credit      mbCredit `json:"artist-credit"`
	ReleaseList []struct {
		ID string `json:"id"`
	} `json:"releases"`
}

func (g mbGroup) album() albumInfo {
	a := albumInfo{ID: g.ID, Title: g.Title, Artist: g.Credit.String(), Type: g.Primary, Releases: max(g.Count, len(g.ReleaseList)), Votes: g.Rating.Votes, Secondary: len(g.Secondary) > 0}
	if len(g.FirstDate) >= 4 {
		if _, err := fmt.Sscan(g.FirstDate[:4], &a.Year); err != nil {
			a.Year = 0 // not a year after all: left out
		}
	}
	for _, k := range g.Secondary {
		// A live compilation (the Collectors' King Crimson series) reads as live: that's what's on it.
		if a.Kind == "" || k == "Live" || (k == "Compilation" && a.Kind != "Live") {
			a.Kind = k
		}
	}
	return a
}

// Headings of a search answer, in order. An artist's own albums are split like a discography.
var lookupGroups = []string{"Top result", "Albums", "EPs", "Live albums", "Compilations", "Other releases", "More"}

func groupOf(a albumInfo) string {
	switch {
	case a.Kind == "Live":
		return "Live albums"
	case a.Kind == "Compilation":
		return "Compilations"
	case a.Secondary:
		return "Other releases"
	case a.Type == "EP":
		return "EPs"
	}
	return "Albums"
}

func groupRank(g string) int {
	for i, x := range lookupGroups {
		if x == g {
			return i
		}
	}
	return len(lookupGroups)
}

// ---- searching ----

// lookup finds albums for what was typed. Both questions are always asked (is it an artist? which
// albums have these words?), so a word that's both, like "dookie" (an album, and also a little-known
// artist's name), finds the album people mean. rankLookup puts them in order.
func (m *musicbrainz) lookup(q string) ([]albumInfo, error) {
	var artists struct {
		Artists []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Score int    `json:"score"`
		} `json:"artists"`
	}
	if err := m.get("/artist", url.Values{"query": {q}, "limit": {"5"}}, &artists); err != nil {
		return nil, err
	}
	var theirs []albumInfo
	for _, a := range artists.Artists {
		if a.Score >= 90 && foldWords(a.Name) == foldWords(q) {
			list, err := m.artistAlbums(a.ID)
			if err != nil {
				return nil, err
			}
			theirs = list
			break
		}
	}
	var groups struct {
		Groups []mbGroup `json:"release-groups"`
	}
	if err := m.get("/release-group", url.Values{"query": {q}, "dismax": {"true"}, "limit": {"40"}}, &groups); err != nil {
		return nil, err
	}
	found := make([]albumInfo, 0, len(groups.Groups))
	for _, g := range groups.Groups {
		found = append(found, g.album())
	}
	return rankLookup(q, theirs, found), nil
}

// Words that make an album someone else's take on music (a tribute, lullaby versions, a book), left
// out of a search unless they were typed.
var notTheirs = []string{
	"karaoke", "tribute", "lullaby", "lullabies", "covers", "covered", "instrumental", "instrumentals",
	"performs", "pickin", "bluegrass", "quartet", "biography", "unauthorised", "unauthorized", "8bit",
}

// rankLookup orders a search's albums, each under a heading (Group) the app shows:
//  1. "Top result": a well-known album with exactly the typed name ("dookie" → Green Day's Dookie);
//  2. when what was typed is an artist's name, their whole discography (theirs, from artistAlbums):
//     Albums and EPs newest first; Live albums, Compilations and Other releases best known first
//     (most rated), since a band like King Crimson has 175 live ones;
//  3. other albums and EPs with the typed words ("More", or "Albums" when nothing came before), at
//     most 25: most words first, then real albums before live ones and compilations, then best
//     known (most editions).
//
// Singles and "other" (interviews, spoken word) are left out: getting music is by the album. So are
// tributes and the like (notTheirs), unless one of those words was typed.
func rankLookup(q string, theirs, found []albumInfo) []albumInfo {
	typed := strings.Fields(foldWords(q))
	typedSet := wordSet(q)
	wantsTributes := false // "dookie lullabies": that's what they're after
	for _, w := range notTheirs {
		wantsTributes = wantsTributes || typedSet[w]
	}
	type ranked struct {
		a       albumInfo
		covered int
	}
	var exact, rest []ranked
	for _, a := range found {
		if a.Type != "Album" && a.Type != "EP" {
			continue
		}
		have := wordSet(a.Title + " " + a.Artist)
		tribute := false
		for _, w := range notTheirs {
			tribute = tribute || have[w]
		}
		if tribute && !wantsTributes {
			continue
		}
		covered := 0
		for _, w := range typed {
			if have[w] {
				covered++
			}
		}
		if covered == 0 {
			continue
		}
		r := ranked{a, covered}
		if foldWords(stripEdition(a.Title)) == foldWords(q) && a.Releases >= 3 && !a.Secondary {
			exact = append(exact, r)
		} else {
			rest = append(rest, r)
		}
	}
	sort.SliceStable(exact, func(i, j int) bool { return exact[i].a.Releases > exact[j].a.Releases })
	sort.SliceStable(rest, func(i, j int) bool {
		a, b := rest[i], rest[j]
		if a.covered != b.covered {
			return a.covered > b.covered
		}
		if a.a.Secondary != b.a.Secondary {
			return !a.a.Secondary
		}
		return a.a.Releases > b.a.Releases
	})
	theirs = append([]albumInfo(nil), theirs...)
	sort.SliceStable(theirs, func(i, j int) bool {
		gi, gj := groupRank(groupOf(theirs[i])), groupRank(groupOf(theirs[j]))
		if gi != gj {
			return gi < gj
		}
		if theirs[i].Secondary && theirs[i].Votes != theirs[j].Votes {
			return theirs[i].Votes > theirs[j].Votes
		}
		return theirs[i].Year > theirs[j].Year
	})
	var out []albumInfo
	seen := map[string]bool{}
	add := func(a albumInfo, group string) {
		if !seen[a.ID] {
			seen[a.ID] = true
			a.Group = group
			out = append(out, a)
		}
	}
	for _, r := range exact {
		add(r.a, "Top result")
	}
	for _, a := range theirs {
		add(a, groupOf(a))
	}
	more := "More"
	if len(out) == 0 {
		more = "Albums"
	}
	for i, r := range rest {
		if i == 25 {
			break
		}
		add(r.a, more)
	}
	return out
}

// artistAlbums: an artist's albums and EPs, live albums and compilations included (rankLookup puts
// them under their headings), at most 300: a page of 100 a second.
func (m *musicbrainz) artistAlbums(artistID string) ([]albumInfo, error) {
	var out []albumInfo
	for offset := 0; offset < 300; offset += 100 {
		var groups struct {
			Count  int       `json:"release-group-count"`
			Groups []mbGroup `json:"release-groups"`
		}
		q := url.Values{"artist": {artistID}, "type": {"album|ep"}, "inc": {"artist-credits+ratings"}, "limit": {"100"}, "offset": {fmt.Sprint(offset)}}
		if err := m.get("/release-group", q, &groups); err != nil {
			if len(out) > 0 { // a later page failed: the pages already read still answer the search
				log.Printf("musicbrainz: %s's albums past %d: %v", artistID, offset, err)
				break
			}
			return nil, err
		}
		for _, g := range groups.Groups {
			out = append(out, g.album())
		}
		if offset+100 >= groups.Count {
			break
		}
	}
	return out, nil
}

// album fills in one album's tracks, from its most usual edition: the track count most of its
// official editions have, digital before CD, earliest first.
func (m *musicbrainz) album(id string) (albumInfo, error) {
	var group mbGroup
	if err := m.get("/release-group/"+id, url.Values{"inc": {"artist-credits"}}, &group); err != nil {
		return albumInfo{}, err
	}
	a := group.album()
	var releases struct {
		Releases []struct {
			Date   string `json:"date"`
			Status string `json:"status"`
			Media  []struct {
				Format string `json:"format"`
				Tracks []struct {
					Position int    `json:"position"`
					Title    string `json:"title"`
					Length   int64  `json:"length"`
				} `json:"tracks"`
			} `json:"media"`
		} `json:"releases"`
	}
	if err := m.get("/release", url.Values{"release-group": {id}, "inc": {"recordings media"}, "limit": {"50"}}, &releases); err != nil {
		return albumInfo{}, err
	}
	count := func(i int) int {
		n := 0
		for _, md := range releases.Releases[i].Media {
			n += len(md.Tracks)
		}
		return n
	}
	usual := map[int]int{}
	for i, r := range releases.Releases {
		if r.Status == "Official" || r.Status == "" {
			usual[count(i)]++
		}
	}
	best, bestCount := -1, 0
	for n, c := range usual {
		if c > bestCount || (c == bestCount && n < best) {
			best, bestCount = n, c
		}
	}
	pick := -1
	for i, r := range releases.Releases {
		if count(i) != best || (r.Status != "Official" && r.Status != "") {
			continue
		}
		if pick < 0 {
			pick = i
			continue
		}
		digital := func(k int) bool {
			return len(releases.Releases[k].Media) > 0 && releases.Releases[k].Media[0].Format == "Digital Media"
		}
		p := releases.Releases[pick]
		if digital(i) && !digital(pick) || digital(i) == digital(pick) && r.Date != "" && (p.Date == "" || r.Date < p.Date) {
			pick = i
		}
	}
	if pick < 0 {
		return a, errors.New("MusicBrainz has no track list for it")
	}
	for d, md := range releases.Releases[pick].Media {
		for _, t := range md.Tracks {
			a.Tracks = append(a.Tracks, albumSong{Disc: d + 1, Number: t.Position, Title: t.Title, DurationMs: t.Length})
			a.DurationMs += t.Length
		}
	}
	return a, nil
}

// ---- serving ----

func (f *fetcher) serveLookup(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		jsonError(w, http.StatusBadRequest, "short_query", "Type a little more")
		return
	}
	start := time.Now()
	albums, err := f.mb.lookup(q)
	if err != nil {
		hubLog.event("music_lookup_failed", "error", err.Error())
		jsonError(w, http.StatusBadGateway, "lookup_failed", "MusicBrainz didn't answer: "+err.Error())
		return
	}
	f.library.markInLibrary(albums)
	hubLog.event("music_lookup", "chars", len(q), "albums", len(albums), "ms", time.Since(start).Milliseconds())
	if albums == nil {
		albums = []albumInfo{}
	}
	writeJSON(w, struct {
		Albums []albumInfo `json:"albums"`
	}{albums})
}

func (f *fetcher) serveAlbum(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !mbid.MatchString(id) {
		jsonError(w, http.StatusBadRequest, "bad_id", "Not an album id")
		return
	}
	start := time.Now()
	a, err := f.mb.album(id)
	if err != nil {
		hubLog.event("music_album_failed", "id", id, "error", err.Error())
		jsonError(w, http.StatusBadGateway, "lookup_failed", "Couldn't look the album up: "+err.Error())
		return
	}
	f.library.markHave(&a)
	var copies []release
	var searchErr string
	if f.indexerKey == "" {
		searchErr = "Add the NZBgeek key in the Media Hub's settings"
	} else if found, err := f.search(a.Artist + " " + stripEdition(a.Title)); err != nil {
		searchErr = "NZBgeek didn't answer: " + redact(err.Error(), f.indexerKey)
	} else {
		copies = matchCopies(a, found)
		f.remember(copies)
		f.mu.Lock()
		f.albums[a.ID] = a
		f.mu.Unlock()
	}
	hubLog.event("music_album", "id", id, "tracks", len(a.Tracks), "copies", len(copies), "ms", time.Since(start).Milliseconds())
	f.mu.Lock()
	var fetching *fetch
	if fe := f.fetches[a.ID]; fe != nil {
		c := fe.public()
		fetching = &c
	}
	f.mu.Unlock()
	writeJSON(w, struct {
		albumInfo
		Copies      []release `json:"copies"`
		CopiesError string    `json:"copiesError,omitempty"`
		Fetch       *fetch    `json:"fetch,omitempty"`
	}{a, nonNilReleases(copies), searchErr, fetching})
}

// serveCover sends an album's cover (250 px), fetched once from the Cover Art Archive and kept.
// One that doesn't exist is remembered too (an empty file), so it isn't asked for again for a week.
func (f *fetcher) serveCover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !mbid.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(coversDir, id+".jpg")
	if st, err := os.Stat(path); err == nil {
		if st.Size() == 0 {
			if time.Since(st.ModTime()) < 7*24*time.Hour {
				http.NotFound(w, r)
				return
			}
		} else {
			w.Header().Set("Cache-Control", "max-age=2592000")
			http.ServeFile(w, r, path)
			return
		}
	}
	data, err := fetchCover(id, 250)
	if err != nil {
		// An empty file remembers there was no cover for a week.
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			log.Printf("musicbrainz: remembering a missing cover: %v", err)
		}
		http.NotFound(w, r)
		return
	}
	if err := writeFileSafely(path, data); err != nil {
		log.Printf("musicbrainz: keeping a cover: %v", err)
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "max-age=2592000")
	if _, err := w.Write(data); err != nil {
		log.Printf("musicbrainz: writing a cover: %v", err)
	}
}

// fetchCover gets an album's front cover at size px (250 or 500) from the Cover Art Archive.
func fetchCover(id string, size int) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/%s/front-%d", coverArchive, id, size), nil)
	req.Header.Set("User-Agent", mbUserAgent)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(resp.Body) // its content is read below; a close error has nothing to add
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err == nil && (len(data) < 1000 || !(data[0] == 0xFF && data[1] == 0xD8 || data[0] == 0x89 && data[1] == 'P')) {
		err = errors.New("not a picture")
	}
	return data, err
}

func writeFileSafely(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- the library's side ----

// markHave marks the album's songs that are in the library already: same title, by the same artist.
func (l *musicLibrary) markHave(a *albumInfo) {
	artist := foldWords(a.Artist)
	l.mu.RLock()
	titles := map[string]bool{}
	for _, t := range l.tracks {
		if foldWords(t.Artist) == artist || foldWords(t.AlbumArtist) == artist {
			titles[foldWords(t.Title)] = true
		}
	}
	l.mu.RUnlock()
	for i := range a.Tracks {
		a.Tracks[i].Have = titles[foldWords(a.Tracks[i].Title)]
	}
}

// markInLibrary marks the albums the library has songs of: same album name (without its edition),
// by the same artist.
func (l *musicLibrary) markInLibrary(albums []albumInfo) {
	have := map[string]bool{}
	l.mu.RLock()
	for _, t := range l.tracks {
		album := foldWords(stripEdition(t.Album))
		have[foldWords(t.AlbumArtist)+"|"+album] = true
		have[foldWords(t.Artist)+"|"+album] = true
	}
	l.mu.RUnlock()
	for i, a := range albums {
		albums[i].InLibrary = have[foldWords(a.Artist)+"|"+foldWords(stripEdition(a.Title))]
	}
}

// albumFile is the MusicBrainz details a fetch leaves beside an album's songs.
const albumFile = "album.json"

// applyAlbumFile: a song in a folder with album.json takes its album, artist and year from it,
// and its title from the track list when its number is there.
func applyAlbumFile(t *musicTrack) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(t.path), albumFile))
	if err != nil {
		return
	}
	var a albumInfo
	if json.Unmarshal(data, &a) != nil || a.Title == "" {
		return
	}
	t.Album, t.AlbumArtist = a.Title, a.Artist
	if t.Artist == "" {
		t.Artist = a.Artist
	}
	if a.Year > 0 {
		t.Year = a.Year
	}
	for _, s := range a.Tracks {
		if s.Number == t.Track && (t.Disc == 0 || s.Disc == t.Disc) {
			t.Title = s.Title
			if t.Disc == 0 && s.Disc > 1 {
				t.Disc = s.Disc
			}
			break
		}
	}
}

// ---- words ----

var notWord = regexp.MustCompile(`[^\p{L}\p{N}]+`)

// foldWords: lower case, "&" as "and", apostrophes dropped, everything else between words a space.
func foldWords(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.NewReplacer("'", "", "’", "", "`", "").Replace(s)
	return strings.Join(strings.Fields(notWord.ReplaceAllString(s, " ")), " ")
}

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(foldWords(s)) {
		set[w] = true
	}
	return set
}

var edition = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]\s*$`)

// stripEdition: "Dookie (30th Anniversary Deluxe)" searched as "Dookie", which finds every edition.
func stripEdition(title string) string {
	if s := strings.TrimSpace(edition.ReplaceAllString(title, "")); s != "" {
		return s
	}
	return title
}
