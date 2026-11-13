package main

// Getting new music: an album (musicbrainz.go) is searched for on Usenet through the indexer
// (NZBgeek, a Newznab API), TorBox downloads the chosen copy (its Usenet downloader), and the
// songs and cover are copied into the music folder, where the library finds them.
//
//   - POST /music/fetch?id=…&album=…   get that copy of that album (ids from /music/album)
//   - GET  /music/fetches              what's being got, and what was got lately
//
// The keys (TorBox's and NZBgeek's) are the add-on's options: they never leave the hub, and a
// search result's NZB link carries the indexer key, so the phone only ever gets an id.
//
// Each fetch checks itself as it goes, with many small safety checks: TorBox must say the
// download is finished and list files; only audio and cover pictures are copied; each copy must
// come out the size TorBox says, or it's thrown away and tried again; a release with no audio at
// all is a failure, not an empty folder. Done, the download is removed from TorBox (its slots are
// limited) and the library is scanned. Fetches survive a hub restart (fetches.json).

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	torboxAPI   = "https://api.torbox.app/v1/api"
	nzbgeekAPI  = "https://api.nzbgeek.info/api"
	fetchPoll   = 15 * time.Second
	fetchGiveUp = 6 * time.Hour
	// Newznab's audio categories: 3000 is all audio (MP3 3010, video 3020, audiobook 3030, lossless 3040).
	audioCategories = "3000"
)

var fetchesFile = filepath.Join(root, "fetches.json")

type release struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Bytes    int64  `json:"bytes"`
	Posted   string `json:"posted,omitempty"`
	Grabs    int    `json:"grabs,omitempty"`
	Category string `json:"category,omitempty"`
	Quality  string `json:"quality,omitempty"` // FLAC, MP3 320… (musicmatch.go)
	link     string // the NZB's address, with the indexer key in it: stays on the hub
}

// fetch is one album being got. State: sending, downloading, copying, done, failed.
type fetch struct {
	ID       string     `json:"id"` // the album's MusicBrainz id
	Title    string     `json:"title"`
	Album    *albumInfo `json:"album,omitempty"`
	State    string     `json:"state"`
	Progress float64    `json:"progress"` // 0-1, TorBox's download then the copying
	Error    string     `json:"error,omitempty"`
	Songs    int        `json:"songs,omitempty"`
	Folder   string     `json:"folder,omitempty"`
	Started  time.Time  `json:"started"`
	Updated  time.Time  `json:"updated"`
	UsenetID int64      `json:"usenetId,omitempty"`
	// The copies to try, best first; Try is the one being tried. A copy that fails (TorBox can't
	// get it, songs missing) is dropped for the next. Kept on the hub only: the links hold the key.
	Copies []copyRef `json:"copies,omitempty"`
	Try    int       `json:"try"`
	Tries  int       `json:"tries,omitempty"` // len(Copies), for the app
}

type copyRef struct {
	Title string `json:"title"`
	Link  string `json:"link"`
}

type fetcher struct {
	torboxKey, indexerKey string
	library               *musicLibrary
	mb                    *musicbrainz
	http                  *http.Client
	mu                    sync.Mutex
	found                 map[string]release   // search results by id, for /music/fetch
	albums                map[string]albumInfo // albums looked at, for /music/fetch?album=
	fetches               map[string]*fetch
}

func newFetcher(opts options, library *musicLibrary) *fetcher {
	f := &fetcher{
		torboxKey:  strings.TrimSpace(opts.TorBoxKey),
		indexerKey: strings.TrimSpace(opts.NZBgeekKey),
		library:    library,
		mb:         newMusicBrainz(),
		http:       &http.Client{Timeout: 60 * time.Second},
		found:      map[string]release{},
		albums:     map[string]albumInfo{},
		fetches:    map[string]*fetch{},
	}
	f.load()
	hubLog.event("music_fetch_ready", "torbox", f.torboxKey != "", "indexer", f.indexerKey != "", "unfinished", f.unfinished())
	for _, fe := range f.fetches {
		if fe.State != "done" && fe.State != "failed" {
			go f.run(fe)
		}
	}
	return f
}

func (f *fetcher) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /music/lookup", f.serveLookup)
	mux.HandleFunc("GET /music/album/{id}", f.serveAlbum)
	mux.HandleFunc("GET /music/cover/{id}", f.serveCover)
	mux.HandleFunc("POST /music/fetch", f.serveFetch)
	mux.HandleFunc("GET /music/fetches", f.serveFetches)
}

// remember keeps search results for a later /music/fetch.
func (f *fetcher) remember(found []release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.found) > 5000 {
		f.found = map[string]release{} // old searches; a fetch from one is answered "search again"
	}
	for _, rel := range found {
		f.found[rel.ID] = rel
	}
}

// ---- searching ----

func nonNilReleases(r []release) []release {
	if r == nil {
		return []release{}
	}
	return r
}

// The Newznab RSS answer, the parts used.
type newznabRSS struct {
	Channel struct {
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
			Enclose struct {
				Length int64 `xml:"length,attr"`
			} `xml:"enclosure"`
			Attrs []struct {
				Name  string `xml:"name,attr"`
				Value string `xml:"value,attr"`
			} `xml:"attr"`
			Category string `xml:"category"`
		} `xml:"item"`
	} `xml:"channel"`
}

// A Newznab error is its own document: <error code="100" description="Incorrect user credentials"/>.
type newznabError struct {
	XMLName     xml.Name `xml:"error"`
	Code        string   `xml:"code,attr"`
	Description string   `xml:"description,attr"`
}

func (f *fetcher) search(q string) ([]release, error) {
	u := nzbgeekAPI + "?" + url.Values{
		"t": {"search"}, "q": {q}, "cat": {audioCategories}, "limit": {"60"}, "extended": {"1"}, "apikey": {f.indexerKey},
	}.Encode()
	resp, err := f.http.Get(u)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(resp.Body) // its content is read below
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseNewznab(body)
}

func parseNewznab(body []byte) ([]release, error) {
	var bad newznabError
	if xml.Unmarshal(body, &bad) == nil {
		return nil, fmt.Errorf("indexer error %s: %s", bad.Code, bad.Description)
	}
	var rss newznabRSS
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("unreadable answer: %v", err)
	}
	var out []release
	for _, it := range rss.Channel.Items {
		if it.Link == "" || it.Title == "" {
			continue
		}
		rel := release{Title: it.Title, Bytes: it.Enclose.Length, Category: it.Category, link: it.Link}
		for _, a := range it.Attrs {
			switch a.Name {
			case "size":
				if n, err := strconv.ParseInt(a.Value, 10, 64); err == nil {
					rel.Bytes = n
				}
			case "grabs":
				rel.Grabs, _ = strconv.Atoi(a.Value)
			}
		}
		if t, err := time.Parse(time.RFC1123Z, it.PubDate); err == nil {
			rel.Posted = t.Format("2006-01-02")
		}
		sum := sha1.Sum([]byte(it.Link))
		rel.ID = hex.EncodeToString(sum[:8])
		out = append(out, rel)
	}
	return out, nil
}

// ---- fetching ----

// Songs are copied into a hidden folder (the library skips those) and moved into place only once
// they're all there and checked, so a half-copied album never shows up.
var incomingDir = filepath.Join(musicDir, ".incoming")

// POST /music/fetch?id=<copy>[&album=<MusicBrainz id>]: with album, the album's other matching
// copies are lined up behind the chosen one, in case it fails.
func (f *fetcher) serveFetch(w http.ResponseWriter, r *http.Request) {
	if f.torboxKey == "" {
		jsonError(w, http.StatusServiceUnavailable, "no_torbox_key", "Add the TorBox key in the Media Hub's settings")
		return
	}
	id, key := r.URL.Query().Get("id"), r.URL.Query().Get("album")
	f.mu.Lock()
	rel, ok := f.found[id]
	album, haveAlbum := f.albums[key]
	existing := f.fetches[key]
	f.mu.Unlock()
	if existing != nil && existing.State != "failed" {
		writeJSON(w, existing.public()) // already being got (or got): not twice
		return
	}
	if !ok || !haveAlbum {
		jsonError(w, http.StatusNotFound, "unknown_release", "Open it again: that result has expired")
		return
	}
	fe := &fetch{ID: key, Title: album.Artist + " – " + album.Title, Album: &album, State: "sending", Started: time.Now(), Updated: time.Now()}
	fe.Copies = []copyRef{{rel.Title, rel.link}}
	f.mu.Lock()
	all := make([]release, 0, len(f.found))
	for _, r := range f.found {
		all = append(all, r)
	}
	f.mu.Unlock()
	for _, other := range matchCopies(album, all) {
		if other.ID != rel.ID && len(fe.Copies) < 4 {
			fe.Copies = append(fe.Copies, copyRef{other.Title, other.link})
		}
	}
	fe.Tries = len(fe.Copies)
	f.mu.Lock()
	f.fetches[key] = fe
	f.mu.Unlock()
	f.save()
	hubLog.event("music_fetch_started", "title", fe.Title, "copy", rel.Title, "bytes", rel.Bytes, "spares", len(fe.Copies)-1)
	go f.run(fe)
	writeJSON(w, fe.public())
}

func (f *fetcher) serveFetches(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	list := make([]fetch, 0, len(f.fetches))
	for _, fe := range f.fetches {
		list = append(list, fe.public())
	}
	f.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Started.After(list[j].Started) })
	writeJSON(w, struct {
		Fetches []fetch `json:"fetches"`
	}{list})
}

// public: what the app sees; no links, and no track list (it has its own).
func (fe *fetch) public() fetch {
	c := *fe
	c.Copies = nil
	if c.Album != nil {
		a := *c.Album
		a.Tracks = nil
		c.Album = &a
	}
	return c
}

// run takes a fetch from wherever it is to done or failed, trying its copies in turn.
// One goroutine per fetch.
func (f *fetcher) run(fe *fetch) {
	var last error
	for fe.Try < len(fe.Copies) {
		err := f.tryCopy(fe)
		if err == nil {
			return
		}
		last = err
		msg := redact(err.Error(), f.indexerKey, f.torboxKey)
		hubLog.event("music_fetch_copy_failed", "title", fe.Title, "copy", fe.Copies[fe.Try].Title, "try", fe.Try+1, "of", len(fe.Copies), "error", msg)
		if fe.UsenetID != 0 {
			f.torboxDelete(fe.UsenetID)
		}
		os.RemoveAll(filepath.Join(incomingDir, fe.workName()))
		f.update(fe, func(x *fetch) {
			x.Try++
			x.UsenetID = 0
			x.Progress = 0
			x.State = "sending"
			x.Error = msg // shown while the next copy is tried
		})
	}
	if last == nil {
		last = errors.New("no copies to try")
	}
	f.update(fe, func(x *fetch) {
		x.State = "failed"
		x.Error = redact(last.Error(), f.indexerKey, f.torboxKey)
		if len(x.Copies) > 1 {
			x.Error = fmt.Sprintf("None of the %d copies worked. Last: %s", len(x.Copies), x.Error)
		}
	})
	hubLog.event("music_fetch_failed", "title", fe.Title, "error", redact(last.Error(), f.indexerKey, f.torboxKey))
}

func (fe *fetch) workName() string { return fmt.Sprintf("%s-%d", safeName(fe.ID), fe.Try) }

// tryCopy gets fe's current copy: TorBox downloads it, the songs are copied and checked, and
// moved into the library.
func (f *fetcher) tryCopy(fe *fetch) error {
	c := fe.Copies[fe.Try]
	if fe.UsenetID == 0 {
		f.update(fe, func(x *fetch) { x.State = "sending" })
		id, err := f.torboxCreate(c.Link, c.Title)
		if err != nil {
			return fmt.Errorf("TorBox wouldn't take it: %w", err)
		}
		f.update(fe, func(x *fetch) { x.UsenetID = id; x.State = "downloading" })
	}
	// Wait for TorBox to finish downloading (and repairing, unpacking) it.
	var files []torboxFile
	waitStart := time.Now()
	for {
		if time.Since(waitStart) > fetchGiveUp {
			return errors.New("TorBox didn't finish within 6 hours")
		}
		item, err := f.torboxItem(fe.UsenetID)
		if err != nil {
			hubLog.event("music_fetch_poll_failed", "title", fe.Title, "error", redact(err.Error(), f.torboxKey))
		} else {
			if item.Finished && len(item.Files) > 0 {
				files = item.Files
				break
			}
			if bad := strings.ToLower(item.State); strings.Contains(bad, "fail") || strings.Contains(bad, "error") || strings.Contains(bad, "missing") {
				return fmt.Errorf("TorBox says %q", item.State)
			}
			p := item.Progress
			if p > 1 {
				p /= 100 // a percent, not a fraction
			}
			f.update(fe, func(x *fetch) { x.State = "downloading"; x.Progress = min(max(p, 0), 1) * 0.8 })
		}
		time.Sleep(fetchPoll)
	}

	// Copy the songs and pictures into a hidden folder first.
	f.update(fe, func(x *fetch) { x.State = "copying"; x.Progress = 0.8 })
	work := filepath.Join(incomingDir, fe.workName())
	var wanted []torboxFile
	for _, file := range files {
		ext := strings.ToLower(filepath.Ext(file.Name))
		if _, audio := musicFormats[ext]; audio || ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
			wanted = append(wanted, file)
		}
	}
	songs := 0
	for i, file := range wanted {
		target := filepath.Join(work, innerPath(file.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := f.copyFile(fe.UsenetID, file, target); err != nil {
			return fmt.Errorf("copying %s: %w", filepath.Base(file.Name), err)
		}
		if _, audio := musicFormats[strings.ToLower(filepath.Ext(file.Name))]; audio {
			songs++
		}
		f.update(fe, func(x *fetch) { x.Progress = 0.8 + 0.18*float64(i+1)/float64(len(wanted)) })
	}

	// Checks: songs there at all, and at least as many as the album's track list.
	a := fe.Album
	if songs == 0 {
		return fmt.Errorf("no songs in it (%d files, none of them audio)", len(files))
	}
	if len(a.Tracks) > 0 && songs < len(a.Tracks) {
		return fmt.Errorf("only %d of the album's %d songs were in it", songs, len(a.Tracks))
	}
	year := ""
	if a.Year > 0 {
		year = fmt.Sprintf(" (%d)", a.Year)
	}
	folder := filepath.Join(musicDir, safeName(a.Artist), safeName(a.Title+year))
	// Its details beside the songs, for the library (applyAlbumFile), in every folder with songs.
	data, _ := json.MarshalIndent(a, "", "  ")
	// A folder these can't be written to is logged and the fetch goes on: the songs are what matter.
	if err := filepath.WalkDir(work, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && hasAudio(path) {
			if err := os.WriteFile(filepath.Join(path, albumFile), data, 0o644); err != nil {
				log.Printf("fetch: %s: %v", albumFile, err)
			}
			if !hasPicture(path) {
				if pic, cerr := fetchCover(a.ID, 500); cerr == nil {
					if err := os.WriteFile(filepath.Join(path, "cover.jpg"), pic, 0o644); err != nil {
						log.Printf("fetch: cover.jpg: %v", err)
					}
				}
			}
		}
		return nil
	}); err != nil {
		log.Printf("fetch: %s: %v", work, err)
	}
	// Into place. Something already there by that name is kept: this one gets " (2)".
	if err := os.MkdirAll(filepath.Dir(folder), 0o755); err != nil {
		return err
	}
	final := folder
	for n := 2; ; n++ {
		if _, err := os.Stat(final); os.IsNotExist(err) {
			break
		}
		final = fmt.Sprintf("%s (%d)", folder, n)
	}
	if err := os.Rename(work, final); err != nil {
		return fmt.Errorf("moving it into the library: %w", err)
	}
	f.torboxDelete(fe.UsenetID) // its slot back; the songs are here now
	f.update(fe, func(x *fetch) {
		x.State = "done"
		x.Progress = 1
		x.Songs = songs
		x.Error = ""
		x.Folder, _ = filepath.Rel(musicDir, final)
	})
	hubLog.event("music_fetch_done", "title", fe.Title, "copy", c.Title, "songs", songs, "minutes", int(time.Since(fe.Started).Minutes()))
	f.library.scan()
	return nil
}

// innerPath: a file's place inside its download, without the download's own top folder, each
// part made safe ("Release/CD1/01 Song.flac" → "CD1/01 Song.flac").
func innerPath(name string) string {
	parts := strings.Split(strings.ReplaceAll(name, "\\", "/"), "/")
	if len(parts) > 1 {
		parts = parts[1:]
	}
	for i, p := range parts {
		parts[i] = safeName(p)
	}
	return filepath.Join(parts...)
}

func hasAudio(dir string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if _, ok := musicFormats[strings.ToLower(filepath.Ext(e.Name()))]; ok && !e.IsDir() {
			return true
		}
	}
	return false
}

func hasPicture(dir string) bool {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && coverName(e.Name()) {
			return true
		}
	}
	return false
}

// copyFile downloads one of a finished download's files to target, checking its size; tried
// three times before it counts as failed.
func (f *fetcher) copyFile(usenetID int64, file torboxFile, target string) error {
	if st, err := os.Stat(target); err == nil && file.Size > 0 && st.Size() == file.Size {
		return nil // already here (a restart part-way)
	}
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		last = f.copyOnce(usenetID, file, target)
		if last == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt) * 10 * time.Second)
	}
	return last
}

func (f *fetcher) copyOnce(usenetID int64, file torboxFile, target string) error {
	link, err := f.torboxLink(usenetID, file.ID)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(link)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body) // its content is read below
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	part := target + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr // a close can be the first to hear about a full disk
	}
	if err == nil && file.Size > 0 && n != file.Size {
		err = fmt.Errorf("got %d of %d bytes", n, file.Size)
	}
	if err == nil && n == 0 {
		err = errors.New("empty file")
	}
	if err != nil {
		os.Remove(part)
		return err
	}
	return os.Rename(part, target)
}

// ---- TorBox ----

type torboxFile struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type torboxItem struct {
	State    string       `json:"download_state"`
	Finished bool         `json:"download_finished"`
	Progress float64      `json:"progress"`
	Files    []torboxFile `json:"files"`
}

func (f *fetcher) torboxCreate(link, name string) (int64, error) {
	var body strings.Builder
	form := multipart.NewWriter(&body)
	// multipart's writes into a strings.Builder can't fail.
	_ = form.WriteField("link", link)
	_ = form.WriteField("name", name)
	_ = form.Close()
	req, _ := http.NewRequest(http.MethodPost, torboxAPI+"/usenet/createusenetdownload", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", form.FormDataContentType())
	var out struct {
		Data struct {
			ID int64 `json:"usenetdownload_id"`
		} `json:"data"`
	}
	if err := f.torbox(req, &out); err != nil {
		return 0, err
	}
	if out.Data.ID == 0 {
		return 0, errors.New("no download id in TorBox's answer")
	}
	return out.Data.ID, nil
}

func (f *fetcher) torboxItem(id int64) (torboxItem, error) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/usenet/mylist?id=%d&bypass_cache=true", torboxAPI, id), nil)
	var out struct {
		Data torboxItem `json:"data"`
	}
	err := f.torbox(req, &out)
	return out.Data, err
}

func (f *fetcher) torboxLink(usenetID, fileID int64) (string, error) {
	q := url.Values{"token": {f.torboxKey}, "usenet_id": {strconv.FormatInt(usenetID, 10)}, "file_id": {strconv.FormatInt(fileID, 10)}}
	req, _ := http.NewRequest(http.MethodGet, torboxAPI+"/usenet/requestdl?"+q.Encode(), nil)
	var out struct {
		Data string `json:"data"`
	}
	if err := f.torbox(req, &out); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out.Data, "http") {
		return "", errors.New("no file address in TorBox's answer")
	}
	return out.Data, nil
}

func (f *fetcher) torboxDelete(id int64) {
	body, _ := json.Marshal(map[string]any{"usenet_id": id, "operation": "delete"})
	req, _ := http.NewRequest(http.MethodPost, torboxAPI+"/usenet/controlusenetdownload", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if err := f.torbox(req, nil); err != nil {
		hubLog.event("music_fetch_cleanup_failed", "usenet_id", id, "error", redact(err.Error(), f.torboxKey))
	}
}

// torbox sends a request with the key and reads TorBox's {success, detail, data} answer into out.
func (f *fetcher) torbox(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+f.torboxKey)
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body) // its content is read below; a close error has nothing to add
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var head struct {
		Success bool   `json:"success"`
		Detail  string `json:"detail"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		log.Printf("fetch: TorBox's answer: %v", err) // nothing to read from it: the status decides
	}
	if resp.StatusCode != http.StatusOK || !head.Success {
		msg := head.Detail
		if msg == "" {
			msg = head.Error
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// ---- keeping track ----

func (f *fetcher) update(fe *fetch, change func(*fetch)) {
	f.mu.Lock()
	change(fe)
	fe.Updated = time.Now()
	f.mu.Unlock()
	f.save()
}

func (f *fetcher) unfinished() int {
	n := 0
	for _, fe := range f.fetches {
		if fe.State != "done" && fe.State != "failed" {
			n++
		}
	}
	return n
}

func (f *fetcher) load() {
	data, err := os.ReadFile(fetchesFile)
	if err != nil {
		return
	}
	var list []*fetch
	if err := json.Unmarshal(data, &list); err != nil {
		hubLog.event("music_fetches_unreadable", "error", err.Error())
		return
	}
	for _, fe := range list {
		if fe.Album == nil {
			continue // from before every fetch was an album's
		}
		// Finished ones older than a month are dropped: the list is for what's going on now.
		if (fe.State == "done" || fe.State == "failed") && time.Since(fe.Updated) > 30*24*time.Hour {
			continue
		}
		f.fetches[fe.ID] = fe
	}
}

func (f *fetcher) save() {
	f.mu.Lock()
	list := make([]*fetch, 0, len(f.fetches))
	for _, fe := range f.fetches {
		c := *fe
		list = append(list, &c)
	}
	f.mu.Unlock()
	data, _ := json.Marshal(list)
	tmp := fetchesFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		if err := os.Rename(tmp, fetchesFile); err != nil {
			log.Printf("fetch: saving fetches: %v", err)
		}
	}
}

// ---- helpers ----

var unsafeName = regexp.MustCompile(`[^\p{L}\p{N} ._\-()\[\]&',!]+`)

// safeName makes a release or file name safe as one folder or file name.
func safeName(name string) string {
	name = strings.TrimSpace(unsafeName.ReplaceAllString(name, "_"))
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "download"
	}
	if len(name) > 150 {
		name = name[:150]
	}
	return name
}

// redact takes keys out of a message before it's logged or shown.
func redact(msg string, keys ...string) string {
	for _, k := range keys {
		if k != "" {
			msg = strings.ReplaceAll(msg, k, "<key>")
		}
	}
	return msg
}

func jsonError(w http.ResponseWriter, code int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]string{"error": kind, "message": message}); err != nil {
		log.Printf("writing response: %v", err)
	}
}
