package main

// Small copies of big songs, for phones away from home and for downloads: each track that's big
// for its length (over 320 kbps: FLAC, plain WAV) is converted once, in the background at the lowest priority, to
// 192 kbps AAC in music-mobile/<id>.m4a. MP3s and other already-small files are left as they are.
// The library lists a track's copy once it's there ("mobileBytes"), and /music/track/{id}?quality=mobile
// serves it. Copies of tracks that leave the library are deleted.
//
// Converting once, rather than on every play as other music servers do, keeps the hub idle while
// music plays: a 4-minute song takes a few seconds once, then nothing.

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var mobileDir = filepath.Join(root, "music-mobile")

// mobileOver: a track bigger than this per second gets a small copy. 192 kbps AAC sounds the same
// as the original; a copy of anything already near that size would save little or be bigger
// (ADPCM WAVs, for one, are about 90 kbps).
const mobileOver = 320_000 / 8

func needsMobile(t *musicTrack) bool {
	if _, skip := notWorthIt.Load(t.ID); skip {
		return false
	}
	return t.DurationMs > 0 && t.Bytes*1000/t.DurationMs > mobileOver
}

// notWorthIt: tracks whose copy came out less than a quarter smaller than the original (an odd
// file, or a wrong length in its header). Their copy is thrown away and not tried again until the
// hub restarts. A safety net under mobileOver: a "small" copy is never bigger.
var notWorthIt sync.Map

func mobilePath(id string) string { return filepath.Join(mobileDir, id+".m4a") }

// convertMobile makes the copies a scan found missing, one at a time, and removes ones no longer
// needed. It runs after each scan; a copy half-made when the hub stops is made again next time.
func (l *musicLibrary) convertMobile() {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("music: no ffmpeg, so no small copies: %v", err)
		return
	}
	if err := os.MkdirAll(mobileDir, 0o755); err != nil {
		log.Printf("music: %v", err)
		return
	}
	l.mu.RLock()
	tracks := l.tracks
	l.mu.RUnlock()

	wanted := map[string]bool{}
	var todo []*musicTrack
	for _, t := range tracks {
		if !needsMobile(t) {
			continue
		}
		wanted[t.ID+".m4a"] = true
		if st, err := os.Stat(mobilePath(t.ID)); err != nil || st.ModTime().Before(t.mod) {
			todo = append(todo, t)
		}
	}
	if entries, err := os.ReadDir(mobileDir); err == nil {
		for _, e := range entries {
			if !wanted[e.Name()] {
				os.Remove(filepath.Join(mobileDir, e.Name()))
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	start := time.Now()
	made, failed := 0, 0
	for _, t := range todo {
		if err := makeMobile(t); err != nil {
			failed++
			log.Printf("music: small copy of %s: %v", t.path, err)
			continue
		}
		made++
	}
	hubLog.event("music_mobile_made", "made", made, "failed", failed, "s", int(time.Since(start).Seconds()))
	releaseMemory()
	logMemory("after_small_copies")
	l.scan() // list the new copies
}

func makeMobile(t *musicTrack) error {
	final := mobilePath(t.ID)
	temp := final + ".part.m4a"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// nice: the TV streams and everything else the hub does come first.
	cmd := exec.CommandContext(ctx, "nice", "-n", "19", "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", t.path, "-vn", "-map_metadata", "-1", "-c:a", "aac", "-b:a", "192k", "-movflags", "+faststart", temp)
	out, err := cmd.CombinedOutput()
	dropCache(t.path)
	dropCache(temp)
	if err != nil {
		os.Remove(temp)
		return &convertError{err, strings.TrimSpace(string(out))}
	}
	if st, err := os.Stat(temp); err == nil && st.Size() > t.Bytes*3/4 {
		os.Remove(temp)
		notWorthIt.Store(t.ID, true)
		hubLog.event("music_mobile_not_smaller", "track", t.ID, "original", t.Bytes, "copy", st.Size())
		return nil
	}
	return os.Rename(temp, final)
}

type convertError struct {
	err    error
	output string
}

func (e *convertError) Error() string { return e.err.Error() + ": " + e.output }

// mobileSize is the size of a track's small copy, or 0 if it has none (yet).
func mobileSize(id string) int64 {
	if st, err := os.Stat(mobilePath(id)); err == nil {
		return st.Size()
	}
	return 0
}
