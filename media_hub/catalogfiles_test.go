package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRecords(t *testing.T) {
	dir := t.TempDir()
	h1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Old play: only stream_fp + offered (BOM first). A stalled new play with a hash. A not-started play.
	write("plays.jsonl", "\xef\xbb\xbf"+`{"time":"2026-11-20T21:00:00","box":"bedroom","play_time":"20:59:00","started":true,"start_s":3.5,"rebuffers":0,"stream_fp":"old111111111","content_id":"tt1:1:1","offered":[{"fp":"old111111111","filename":"Show.S01E01.mkv"}]}
{"time":"2026-11-21T10:00:00","box":"bedroom","play_time":"09:59:00","started":true,"start_s":2,"rebuffers":4,"stream_fp":"new222222222","stream_hash":"`+h1+`","stream_file":"Movie.2020.mkv","content_id":"tt2"}
not json
{"time":"2026-11-21T00:01:00","box":"living","play_time":"23:59:30","started":false,"stream_fp":"bad333333333","stream_hash":"`+h2+`","stream_file":"dir/Ep.mkv"}
`)
	// The old fingerprint later seen next to a hash: the old play joins that file. A link test that failed after.
	write("links.jsonl", `{"time":"2026-11-21T12:00:00","content_id":"tt1:1:1","fp":"old111111111","filename":"Show.S01E01.mkv","stream_hash":"cccccccccccccccccccccccccccccccccccccccc","stream_file":"Show.S01E01.mkv","ok":true,"resolve_s":1.0}
{"time":"2026-11-21T13:00:00","fp":"lnk444444444","filename":"X.mkv","ok":true,"resolve_s":1.5}
{"time":"2026-11-21T14:00:00","fp":"lnk444444444","filename":"X.mkv","ok":false,"error":"timeout"}
`)
	// Sound during the stalled play; the failed file works later through the Sorter.
	write("audio.jsonl", `{"time":"2026-11-21T10:05:00","box":"bedroom","play_started":"2026-11-21T09:58:00","sound":false}
{"time":"2026-11-21T10:06:00","box":"bedroom","play_started":"2026-11-21T09:58:00","sound":true}
`)
	write("sorter-plays.jsonl", `{"time":"2026-11-20T08:00:00-05:00","hash":"`+h2+`","file":"Ep.mkv","ok":true,"link_s":0.8}
`)
	tr := newFileTrack(dir)
	tr.now = func() time.Time { return time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local) }
	got := map[string]*fileRecord{}
	for _, f := range tr.get().Files {
		got[f.Key] = f
	}
	show := got["cccccccccccccccccccccccccccccccccccccccc/Show.S01E01.mkv"]
	if show == nil || show.Plays != 1 || show.LinksOK != 1 || show.Verdict != "proven" || show.StartMedian != 3.5 {
		t.Fatalf("old play should join the hash entry and be proven: %+v", show)
	}
	movie := got[h1+"/Movie.2020.mkv"]
	if movie == nil || movie.Verdict != "stalls" || movie.Stalled != 1 || movie.SoundPlays != 1 {
		t.Fatalf("stalled play: %+v", movie)
	}
	ep := got[h2+"/Ep.mkv"]
	if ep == nil || ep.Failed != 1 || ep.SorterOK != 1 || ep.Verdict != "failed" {
		t.Fatalf("failed after a good Sorter link (the play was the day before 00:01, at 23:59:30): %+v", ep)
	}
	x := got["fp:lnk444444444"]
	if x == nil || x.Verdict != "failed" || x.LastError != "timeout" || x.File != "X.mkv" {
		t.Fatalf("ok then failed link: %+v", x)
	}
	data, err := os.ReadFile(filepath.Join(dir, "files.json"))
	var back fileRecords
	if err != nil || json.Unmarshal(data, &back) != nil || len(back.Files) != 4 {
		t.Fatalf("files.json: %v %d", err, len(back.Files))
	}
}

func TestFileVerdict(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 21, h, 0, 0, 0, time.UTC) }
	f := &fileRecord{Started: 1, outcomes: []fileOutcome{{at: at(1), ok: false, play: true}, {at: at(2), ok: true, play: true}}}
	if v := verdict(f); v != "proven" {
		t.Errorf("failed then played fine: %s", v)
	}
	f = &fileRecord{LinksOK: 1, outcomes: []fileOutcome{{at: at(1), ok: true}}}
	if v := verdict(f); v != "untested" {
		t.Errorf("one good link test alone: %s", v)
	}
}

// TestFileRecordsReal reads a real catalog folder when FILES_DIR is set (not part of the normal run).
func TestFileRecordsReal(t *testing.T) {
	dir := os.Getenv("FILES_DIR")
	if dir == "" {
		t.Skip("FILES_DIR not set")
	}
	n := map[string]int{}
	recs := buildFileRecords(dir, time.Now())
	for _, f := range recs.Files {
		n[f.Verdict]++
	}
	t.Logf("%d files: %v", len(recs.Files), n)
	for i, f := range recs.Files {
		if i < 8 {
			b, _ := json.Marshal(f)
			t.Logf("%s", b)
		}
	}
}
