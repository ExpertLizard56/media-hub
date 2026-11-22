package main

// The file track record: what happened each time a stream FILE was played or tested, movies and episodes
// alike, so the Sorter can rank proven files up and push failed ones down. Built from the catalog folder:
//
//   - plays.jsonl: the PC's Playback report (one record per play: started, start time, rebuffers, the link's
//     fingerprint, and since Nov 22 the Sorter link's hash and file).
//   - links.jsonl: the PC's link tests (one line per link tried).
//   - audio.jsonl: the PC's sound log (one row per minute of a play; silent plays are reported, not judged).
//   - sorter-plays.jsonl: the Sorter's own /play results (did TorBox hand over a link, and how fast).
//
// A file is "<hash>/<file name>" once a Sorter link names it, else "fp:<fingerprint>" (first 12 hex of the
// SHA-256 of the play link; older records only have that). Written to files.json in the same folder (the
// Sorter reads it there) whenever an input changes, checked once a minute; also GET /catalog/files and
// /catalog/file/{hash or fp}, home network only. Fields are only ever added, never renamed.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var fileInputs = []string{"plays.jsonl", "links.jsonl", "audio.jsonl", "sorter-plays.jsonl"}

type fileRecord struct {
	Key          string   `json:"key"`
	Hash         string   `json:"hash,omitempty"`
	File         string   `json:"file,omitempty"`
	Fps          []string `json:"fps,omitempty"`
	Titles       []string `json:"titles,omitempty"` // content ids (tt..., tt...:s:e) it was played or tested for
	Plays        int      `json:"plays"`
	Started      int      `json:"started"`
	Failed       int      `json:"failed"`
	Stalled      int      `json:"stalled"`
	StartMedian  float64  `json:"start_s_median,omitempty"`
	SoundPlays   int      `json:"sound_plays,omitempty"`
	SilentPlays  int      `json:"silent_plays,omitempty"`
	LinksOK      int      `json:"links_ok"`
	LinksFailed  int      `json:"links_failed"`
	LinkMedian   float64  `json:"link_s_median,omitempty"`
	SorterOK     int      `json:"sorter_ok"`
	SorterFailed int      `json:"sorter_failed"`
	LastOK       string   `json:"last_ok,omitempty"`
	LastFail     string   `json:"last_fail,omitempty"`
	LastError    string   `json:"last_error,omitempty"`
	Verdict      string   `json:"verdict"` // proven, failed, stalls, untested

	starts, links []float64
	outcomes      []fileOutcome
	last          time.Time
}

type fileOutcome struct {
	at      time.Time
	ok      bool
	play    bool // a real play (not a link test or a Sorter link)
	stalled bool
}

type fileRecords struct {
	Updated string        `json:"updated"`
	Files   []*fileRecord `json:"files"`
}

type fileTrack struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	sig   string
	built *fileRecords
}

func newFileTrack(dir string) *fileTrack { return &fileTrack{dir: dir, now: time.Now} }

func (t *fileTrack) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog/files", t.serveFiles)
	mux.HandleFunc("GET /catalog/file/{key}", t.serveFile)
}

// keepWritten rebuilds files.json once a minute when an input changed.
func (t *fileTrack) keepWritten() {
	for {
		t.get()
		time.Sleep(time.Minute)
	}
}

// get: the records, rebuilt (and files.json rewritten) only when an input file changed.
func (t *fileTrack) get() *fileRecords {
	sig := ""
	for _, n := range fileInputs {
		if st, err := os.Stat(filepath.Join(t.dir, n)); err == nil {
			sig += n + st.ModTime().Format(time.RFC3339Nano) + itoa(st.Size()) + ";"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.built != nil && sig == t.sig {
		return t.built
	}
	t.built, t.sig = buildFileRecords(t.dir, t.now()), sig
	if sig != "" {
		if err := writeJSONFile(filepath.Join(t.dir, "files.json"), t.built); err != nil {
			hubLog.event("files_write_failed", "error", err)
		}
	}
	return t.built
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (t *fileTrack) serveFiles(w http.ResponseWriter, r *http.Request) {
	if !fromHome(r) {
		http.NotFound(w, r)
		return
	}
	all := t.get()
	if v := r.URL.Query().Get("verdict"); v != "" {
		out := &fileRecords{Updated: all.Updated, Files: []*fileRecord{}}
		for _, f := range all.Files {
			if f.Verdict == v {
				out.Files = append(out.Files, f)
			}
		}
		all = out
	}
	writeJSON(w, all)
}

func (t *fileTrack) serveFile(w http.ResponseWriter, r *http.Request) {
	if !fromHome(r) {
		http.NotFound(w, r)
		return
	}
	key := strings.ToLower(r.PathValue("key"))
	out := []*fileRecord{}
	for _, f := range t.get().Files {
		if f.Hash == key || f.Key == key || contains(f.Fps, key) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, out)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// --- building ---

var sorterLink = regexp.MustCompile(`/play/([0-9a-fA-F]{40})\?f=`)

// readLines: the JSON lines of a file (BOM and bad lines skipped).
func readLines(path string) []json.RawMessage {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, l := range bytes.Split(data, []byte("\n")) {
		l = bytes.TrimPrefix(bytes.TrimSpace(l), []byte("\xef\xbb\xbf"))
		if len(l) > 0 && json.Valid(l) {
			out = append(out, json.RawMessage(l))
		}
	}
	return out
}

// parseTime: RFC 3339, or a local time without a zone (the PC's scripts).
func parseTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	t, _ := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local)
	return t
}

func baseName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// fileEvent: one play, link test or Sorter link, before it is put under its file.
type fileEvent struct {
	hash, file, fp, title string
	kind                  string // play, link, sorter
	at                    time.Time
	ok, stalled           bool
	seconds               float64
	silent, sound         bool
	err                   string
}

func buildFileRecords(dir string, now time.Time) *fileRecords {
	var events []fileEvent

	// Sound per play: box + play start -> any minute with sound.
	type audioPlay struct {
		box   string
		start time.Time
		sound bool
	}
	var audio []*audioPlay
	for _, raw := range readLines(filepath.Join(dir, "audio.jsonl")) {
		var a struct {
			Box         string `json:"box"`
			PlayStarted string `json:"play_started"`
			Sound       bool   `json:"sound"`
		}
		if json.Unmarshal(raw, &a) != nil || a.PlayStarted == "" {
			continue
		}
		st := parseTime(a.PlayStarted)
		var p *audioPlay
		for _, x := range audio {
			if x.box == a.Box && x.start.Equal(st) {
				p = x
			}
		}
		if p == nil {
			p = &audioPlay{box: a.Box, start: st}
			audio = append(audio, p)
		}
		p.sound = p.sound || a.Sound
	}

	for _, raw := range readLines(filepath.Join(dir, "plays.jsonl")) {
		var p struct {
			Time      string   `json:"time"`
			Box       string   `json:"box"`
			PlayTime  string   `json:"play_time"`
			Started   *bool    `json:"started"`
			StartS    *float64 `json:"start_s"`
			Rebuffers int      `json:"rebuffers"`
			Fp        string   `json:"stream_fp"`
			Hash      string   `json:"stream_hash"`
			File      string   `json:"stream_file"`
			ContentID string   `json:"content_id"`
			Offered   []struct {
				Fp       string `json:"fp"`
				Filename string `json:"filename"`
				Hash     string `json:"hash"`
				File     string `json:"file"`
			} `json:"offered"`
		}
		if json.Unmarshal(raw, &p) != nil || p.Started == nil || (p.Fp == "" && p.Hash == "") {
			continue
		}
		e := fileEvent{hash: strings.ToLower(p.Hash), file: baseName(p.File), fp: p.Fp, title: p.ContentID, kind: "play",
			ok: *p.Started, stalled: *p.Started && p.Rebuffers >= 3}
		if e.file == "" {
			for _, o := range p.Offered {
				if o.Fp != "" && o.Fp == p.Fp {
					e.file = baseName(o.Filename)
					if e.hash == "" && o.Hash != "" {
						e.hash, e.file = strings.ToLower(o.Hash), baseName(o.File)
					}
				}
			}
		}
		recorded := parseTime(p.Time)
		e.at = recorded
		if c, err := time.ParseInLocation("15:04:05", p.PlayTime, time.Local); err == nil && !recorded.IsZero() {
			at := time.Date(recorded.Year(), recorded.Month(), recorded.Day(), c.Hour(), c.Minute(), c.Second(), 0, time.Local)
			if at.After(recorded) {
				at = at.AddDate(0, 0, -1)
			}
			e.at = at
		}
		if p.StartS != nil && e.ok {
			e.seconds = *p.StartS
		}
		for _, a := range audio {
			if a.box == p.Box && absDur(a.start.Sub(e.at)) <= 3*time.Minute {
				e.sound, e.silent = a.sound, !a.sound
			}
		}
		if e.at.IsZero() {
			continue
		}
		events = append(events, e)
	}

	for _, raw := range readLines(filepath.Join(dir, "links.jsonl")) {
		var l struct {
			Time      string   `json:"time"`
			ContentID string   `json:"content_id"`
			Fp        string   `json:"fp"`
			Filename  string   `json:"filename"`
			File      string   `json:"file"`
			Hash      string   `json:"stream_hash"`
			SFile     string   `json:"stream_file"`
			OK        bool     `json:"ok"`
			ResolveS  *float64 `json:"resolve_s"`
			TtfbS     *float64 `json:"ttfb_s"`
			Error     string   `json:"error"`
		}
		if json.Unmarshal(raw, &l) != nil || (l.Fp == "" && l.Hash == "") {
			continue
		}
		e := fileEvent{hash: strings.ToLower(l.Hash), file: baseName(l.SFile), fp: l.Fp, title: l.ContentID, kind: "link",
			at: parseTime(l.Time), ok: l.OK, err: l.Error}
		if e.file == "" {
			e.file = baseName(l.Filename)
		}
		if e.file == "" {
			e.file = baseName(l.File)
		}
		if l.ResolveS != nil {
			e.seconds = *l.ResolveS
		} else if l.TtfbS != nil {
			e.seconds = *l.TtfbS
		}
		if !e.at.IsZero() {
			events = append(events, e)
		}
	}

	for _, raw := range readLines(filepath.Join(dir, "sorter-plays.jsonl")) {
		var s struct {
			Time  string  `json:"time"`
			Hash  string  `json:"hash"`
			File  string  `json:"file"`
			OK    bool    `json:"ok"`
			LinkS float64 `json:"link_s"`
			Error string  `json:"error"`
		}
		if json.Unmarshal(raw, &s) != nil || len(s.Hash) != 40 {
			continue
		}
		e := fileEvent{hash: strings.ToLower(s.Hash), file: baseName(s.File), kind: "sorter", at: parseTime(s.Time),
			ok: s.OK, seconds: s.LinkS, err: s.Error}
		if !e.at.IsZero() {
			events = append(events, e)
		}
	}

	// Keys: hash/file when known; an fingerprint seen next to a hash joins that file.
	fpKey := map[string]string{}
	for _, e := range events {
		if e.hash != "" && e.fp != "" {
			fpKey[e.fp] = e.hash + "/" + e.file
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	byKey := map[string]*fileRecord{}
	for _, e := range events {
		key := ""
		switch {
		case e.hash != "":
			key = e.hash + "/" + e.file
		case fpKey[e.fp] != "":
			key = fpKey[e.fp]
		default:
			key = "fp:" + e.fp
		}
		f := byKey[key]
		if f == nil {
			f = &fileRecord{Key: key}
			byKey[key] = f
		}
		if e.hash != "" {
			f.Hash = e.hash
		}
		if e.file != "" && (f.File == "" || e.hash != "") {
			f.File = e.file
		}
		if e.fp != "" && !contains(f.Fps, e.fp) {
			f.Fps = append(f.Fps, e.fp)
		}
		if e.title != "" && !contains(f.Titles, e.title) {
			f.Titles = append(f.Titles, e.title)
		}
		switch e.kind {
		case "play":
			f.Plays++
			if e.ok {
				f.Started++
				if e.seconds > 0 {
					f.starts = append(f.starts, e.seconds)
				}
			} else {
				f.Failed++
			}
			if e.stalled {
				f.Stalled++
			}
			if e.sound {
				f.SoundPlays++
			}
			if e.silent {
				f.SilentPlays++
			}
		case "link":
			if e.ok {
				f.LinksOK++
				if e.seconds > 0 {
					f.links = append(f.links, e.seconds)
				}
			} else {
				f.LinksFailed++
			}
		case "sorter":
			if e.ok {
				f.SorterOK++
			} else {
				f.SorterFailed++
			}
		}
		stamp := e.at.Format(time.RFC3339)
		if e.ok {
			f.LastOK = stamp
		} else {
			f.LastFail = stamp
			if e.err != "" {
				f.LastError = e.err
			}
		}
		f.outcomes = append(f.outcomes, fileOutcome{at: e.at, ok: e.ok, play: e.kind == "play", stalled: e.stalled})
		f.last = e.at
	}

	out := &fileRecords{Updated: now.Format(time.RFC3339), Files: []*fileRecord{}}
	for _, f := range byKey {
		f.StartMedian, f.LinkMedian = median(f.starts), median(f.links)
		f.Verdict = verdict(f)
		out.Files = append(out.Files, f)
	}
	sort.Slice(out.Files, func(i, j int) bool {
		if !out.Files[i].last.Equal(out.Files[j].last) {
			return out.Files[i].last.After(out.Files[j].last)
		}
		return out.Files[i].Key < out.Files[j].Key
	})
	return out
}

// verdict: failed when the latest outcome is a failure; stalls when the latest started play stalled (and
// nothing failed after it); proven after a started play or two good link tests with the latest outcome good.
func verdict(f *fileRecord) string {
	if len(f.outcomes) == 0 {
		return "untested"
	}
	if !f.outcomes[len(f.outcomes)-1].ok {
		return "failed"
	}
	for i := len(f.outcomes) - 1; i >= 0; i-- {
		if o := f.outcomes[i]; o.play && o.ok {
			if o.stalled {
				return "stalls"
			}
			break
		}
	}
	if f.Started > 0 || f.LinksOK >= 2 {
		return "proven"
	}
	return "untested"
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
