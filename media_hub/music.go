package main

// The music library, for the Music app: every audio file under /share/media-hub/music, described
// by its own tags (musictags.go) and, where those say nothing, by its folders and file name:
//
//	music/Artist/Album/03 - Title.flac     artist, album, track 3
//	music/Artist - Album/Title.mp3         the same, in one folder
//	music/Title.wav                        just a title
//
//   - GET /music/library.json    every track ({"tracks": [...]}, see musicTrack); the app groups them
//   - GET /music/track/{id}      the file itself, with seeking (Range); ?quality=mobile for its small
//                                copy when it has one (musicmobile.go)
//   - GET /music/art/{id}        the track's cover: embedded, else cover/folder/front.jpg|png beside it
//   - GET /music/wave/{id}       the track's shape, for the waveform progress bar (musicwave.go)
//   - GET /music/install         the newest Music app, for a phone's first install
//
// The folder is looked at again every few minutes; files already read are only read again if
// they changed. Albums got through TorBox (musicfetch.go) land in the same folder, as Artist/Album (Year).

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	musicDir        = filepath.Join(root, "music")
	musicUpdatesDir = filepath.Join(updatesDir, "music")
)

const musicRescan = 5 * time.Minute

// musicTrack is one track as the app sees it. Empty and zero fields are left out.
type musicTrack struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Artist      string `json:"artist,omitempty"`
	AlbumArtist string `json:"albumArtist,omitempty"`
	Album       string `json:"album,omitempty"`
	Track       int    `json:"track,omitempty"`
	Disc        int    `json:"disc,omitempty"`
	Year        int    `json:"year,omitempty"`
	DurationMs  int64  `json:"durationMs,omitempty"`
	Format      string `json:"format"`
	Bytes       int64  `json:"bytes"`
	HasArt      bool   `json:"hasArt,omitempty"`
	// The small copy's size, once it's made (musicmobile.go); 0 when there's none.
	MobileBytes int64 `json:"mobileBytes,omitempty"`
	// When its shape for the waveform progress bar was made (unix seconds; musicwave.go), 0 if it
	// hasn't been yet. The app keeps shapes, so a new one needs a new number.
	Wave    int64 `json:"wave,omitempty"`
	HasWave bool  `json:"-"`
	// When the file arrived (its modification time, ms): "recently added".
	Added int64 `json:"added"`

	path string
	mod  time.Time
	art  tags // where the embedded cover is, if any
	// A cover file beside it, when there's no embedded one.
	artFile string
}

type musicLibrary struct {
	// One scan at a time (the 5-minute one, and one after a fetch).
	scanning sync.Mutex
	mu     sync.RWMutex
	tracks []*musicTrack
	byID   map[string]*musicTrack
	json   []byte
	// The same, gzipped once per scan: a big library's list is several MB, and compresses to a fraction of that.
	gz []byte
}

var musicFormats = map[string]string{".mp3": "mp3", ".flac": "flac", ".wav": "wav", ".m4a": "m4a", ".aac": "aac", ".ogg": "ogg", ".opus": "opus"}

var musicTypes = map[string]string{"mp3": "audio/mpeg", "flac": "audio/flac", "wav": "audio/wav", "m4a": "audio/mp4", "aac": "audio/aac", "ogg": "audio/ogg", "opus": "audio/ogg"}

func newMusicLibrary() *musicLibrary {
	l := &musicLibrary{byID: map[string]*musicTrack{}}
	if err := os.MkdirAll(musicDir, 0o755); err != nil {
		log.Printf("music: %v", err)
	}
	l.scan()
	go func() {
		l.makeWaves()
		l.convertMobile()
		for range time.Tick(musicRescan) {
			l.scan()
			l.makeWaves()
			l.convertMobile()
		}
	}()
	return l
}

func (l *musicLibrary) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /music/library.json", l.serveLibrary)
	mux.HandleFunc("GET /music/track/{id}", l.serveTrack)
	mux.HandleFunc("GET /music/art/{id}", l.serveArt)
	mux.HandleFunc("GET /music/wave/{id}", l.serveWave)
	mux.HandleFunc("GET /music/install", func(w http.ResponseWriter, r *http.Request) { serveNewestAPK(w, r, musicUpdatesDir) })
}

// scan reads the music folder, reusing what's known about files that haven't changed.
func (l *musicLibrary) scan() {
	l.scanning.Lock()
	defer l.scanning.Unlock()
	start := time.Now()
	l.mu.RLock()
	known := l.byID
	l.mu.RUnlock()

	covers := map[string]string{} // folder -> its cover file
	var tracks []*musicTrack
	read := 0
	err := filepath.WalkDir(musicDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Printf("music: %v", err)
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != musicDir {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if cover := coverName(d.Name()); cover {
			dir := filepath.Dir(path)
			if _, has := covers[dir]; !has || strings.HasPrefix(strings.ToLower(d.Name()), "cover") {
				covers[dir] = path
			}
			return nil
		}
		format, ok := musicFormats[ext]
		if !ok || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(musicDir, path)
		id := musicID(rel)
		if old, ok := known[id]; ok && old.Bytes == info.Size() && old.mod.Equal(info.ModTime()) {
			reused := *old // a copy: the published one may be being served right now
			tracks = append(tracks, &reused)
			return nil
		}
		read++
		tracks = append(tracks, describeTrack(path, rel, id, format, info))
		return nil
	})
	if err != nil {
		log.Printf("music: scan: %v", err)
	}
	for _, t := range tracks {
		t.artFile = ""
		if t.art.ArtLength == 0 {
			t.artFile = covers[filepath.Dir(t.path)]
		}
		t.HasArt = t.art.ArtLength > 0 || t.artFile != ""
		t.Wave = waveMade(t.ID)
		t.HasWave = t.Wave > 0
		t.MobileBytes = 0
		if needsMobile(t) {
			t.MobileBytes = mobileSize(t.ID)
		}
	}
	sort.Slice(tracks, func(i, j int) bool { return tracks[i].path < tracks[j].path })
	byID := make(map[string]*musicTrack, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
	}
	body, err := json.Marshal(struct {
		Tracks []*musicTrack `json:"tracks"`
	}{Tracks: nonNil(tracks)})
	if err != nil {
		log.Printf("music: %v", err)
		return
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if _, err := zw.Write(body); err != nil {
		log.Printf("music: %v", err)
	}
	if err := zw.Close(); err != nil {
		log.Printf("music: %v", err)
	}
	l.mu.Lock()
	changed := len(l.tracks) != len(tracks) || read > 0
	l.tracks, l.byID, l.json, l.gz = tracks, byID, body, gz.Bytes()
	l.mu.Unlock()
	if changed {
		hubLog.event("music_scanned", "tracks", len(tracks), "read", read, "ms", time.Since(start).Milliseconds())
	}
}

func nonNil(t []*musicTrack) []*musicTrack {
	if t == nil {
		return []*musicTrack{}
	}
	return t
}

// describeTrack: the file's own tags first, then its folders and name for what they don't say.
func describeTrack(path, rel, id, format string, info fs.FileInfo) *musicTrack {
	t := &musicTrack{ID: id, Format: format, Bytes: info.Size(), Added: info.ModTime().UnixMilli(), path: path, mod: info.ModTime()}
	if f, err := os.Open(path); err == nil {
		tg, err := readTags(f, info.Size(), format)
		closeQuietly(f) // read only
		if err != nil {
			log.Printf("music: %s: %v", rel, err)
		}
		t.Title, t.Artist, t.AlbumArtist, t.Album = tg.Title, tg.Artist, tg.AlbumArtist, tg.Album
		t.Track, t.Disc, t.Year, t.DurationMs = tg.Track, tg.Disc, tg.Year, tg.DurationMs
		t.art = tags{ArtOffset: tg.ArtOffset, ArtLength: tg.ArtLength, ArtMime: tg.ArtMime}
	}
	fromName := namesFromPath(rel)
	if t.Title == "" {
		t.Title = fromName.Title
	}
	if t.Artist == "" {
		t.Artist = fromName.Artist
	}
	if t.Album == "" {
		t.Album = fromName.Album
	}
	if t.Track == 0 {
		t.Track = fromName.Track
	}
	applyAlbumFile(t) // a fetched album's MusicBrainz details beat its files' tags (musicbrainz.go)
	return t
}

var (
	// "03 - Title", "3. Title", "1-03 Title" (disc-track). A bare number and a space counts only
	// with a leading zero ("03 Title"): "21 Guns" and "100 Ways to Hate" are titles.
	leadingTrack     = regexp.MustCompile(`^(?:\d{1,2}-)?(\d{1,3})\s*[-.]\s+(.+)$`)
	leadingZeroTrack = regexp.MustCompile(`^(?:\d{1,2}-)?(0\d{1,2})\s+(.+)$`)
	// "Artist - Album" as one folder name, with an optional "(2019)" or "[2019]" after it.
	artistAlbum = regexp.MustCompile(`^(.+?)\s+-\s+(.+?)(?:\s*[\(\[]\d{4}[\)\]])?$`)
	trailingYear = regexp.MustCompile(`\s*[\(\[]\d{4}[\)\]]$`)
)

// namesFromPath reads a track's title, number, artist and album from where it is:
// Artist/Album/03 - Title, "Artist - Album"/Title, or just Title.
func namesFromPath(rel string) tags {
	var t tags
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	name := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	name = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	m := leadingTrack.FindStringSubmatch(name)
	if m == nil {
		m = leadingZeroTrack.FindStringSubmatch(name)
	}
	if m != nil {
		t.Track = leadingNumber(m[1])
		name = m[2]
	}
	// "Artist - Title" in the file name itself, when the folders don't say.
	folders := parts[:len(parts)-1]
	if len(folders) == 0 {
		if artist, title, ok := strings.Cut(name, " - "); ok {
			t.Artist, name = strings.TrimSpace(artist), strings.TrimSpace(title)
		}
	}
	t.Title = name
	// Folders: a "CD1"/"Disc 2" folder is part of its album.
	if n := len(folders); n > 0 && discFolder.MatchString(folders[n-1]) {
		folders = folders[:n-1]
	}
	switch n := len(folders); {
	case n >= 2:
		t.Artist = folders[n-2]
		t.Album = trailingYear.ReplaceAllString(folders[n-1], "")
		if m := artistAlbum.FindStringSubmatch(folders[n-1]); m != nil && strings.EqualFold(m[1], t.Artist) {
			t.Album = m[2]
		}
	case n == 1:
		if m := artistAlbum.FindStringSubmatch(folders[0]); m != nil {
			t.Artist, t.Album = m[1], m[2]
		} else {
			t.Album = trailingYear.ReplaceAllString(folders[0], "")
		}
	}
	return t
}

var discFolder = regexp.MustCompile(`(?i)^(cd|disc|disk)\s*\d+$`)

func coverName(name string) bool {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)
	if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return false
	}
	base := strings.TrimSuffix(lower, ext)
	return base == "cover" || base == "folder" || base == "front" || base == "album"
}

// musicID is a track's id: from its place in the folder, so it stays the same across scans.
func musicID(rel string) string {
	sum := sha1.Sum([]byte(filepath.ToSlash(rel)))
	return hex.EncodeToString(sum[:8])
}

func (l *musicLibrary) serveLibrary(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	body, gz := l.json, l.gz
	l.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		body = gz
	}
	if _, err := w.Write(body); err != nil {
		log.Printf("music: writing the library: %v", err)
	}
}

func (l *musicLibrary) track(id string) *musicTrack {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.byID[id]
}

func (l *musicLibrary) serveTrack(w http.ResponseWriter, r *http.Request) {
	t := l.track(r.PathValue("id"))
	if t == nil {
		http.NotFound(w, r)
		return
	}
	path, kind := t.path, musicTypes[t.Format]
	// The small copy when it's asked for and there is one; otherwise the original.
	if r.URL.Query().Get("quality") == "mobile" && t.MobileBytes > 0 {
		path, kind = mobilePath(t.ID), "audio/mp4"
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer closeQuietly(f) // read only
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", kind)
	// A track's file never changes under the same id and size; the app may keep it.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (l *musicLibrary) serveArt(w http.ResponseWriter, r *http.Request) {
	t := l.track(r.PathValue("id"))
	if t == nil || !t.HasArt {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if t.art.ArtLength > 0 {
		f, err := os.Open(t.path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer closeQuietly(f) // read only
		w.Header().Set("Content-Type", t.art.ArtMime)
		http.ServeContent(w, r, "", t.mod, io.NewSectionReader(f, t.art.ArtOffset, t.art.ArtLength))
		return
	}
	http.ServeFile(w, r, t.artFile)
}
