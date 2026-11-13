package main

// Each song's shape, for the Music app's waveform progress bar: waveBars loudness values, one
// byte each (0-255), worked out once per track in the background (ffmpeg decodes it to 8 kHz mono
// at the lowest priority; a song takes well under a second) and kept in music-waves/<id>.wave.
// GET /music/wave/{id} serves them; the library says when a track's was made ("wave", unix seconds; 0 = none yet).

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var waveDir = filepath.Join(root, "music-waves")

const (
	waveBars = 160
	waveRate = 8000
)

func wavePath(id string) string { return filepath.Join(waveDir, id+".wave") }

// waveMade is when a track's shape was made (unix seconds), or 0 if it has none.
func waveMade(id string) int64 {
	if st, err := os.Stat(wavePath(id)); err == nil && st.Size() == waveBars {
		return st.ModTime().Unix()
	}
	return 0
}

// makeWaves works out the shapes the library doesn't have yet, and drops ones whose track left.
func (l *musicLibrary) makeWaves() {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return
	}
	if err := os.MkdirAll(waveDir, 0o755); err != nil {
		return
	}
	l.mu.RLock()
	tracks := l.tracks
	l.mu.RUnlock()
	wanted := map[string]bool{}
	var todo []*musicTrack
	for _, t := range tracks {
		wanted[t.ID+".wave"] = true
		if !t.HasWave {
			todo = append(todo, t)
		}
	}
	if entries, err := os.ReadDir(waveDir); err == nil {
		for _, e := range entries {
			if !wanted[e.Name()] {
				os.Remove(filepath.Join(waveDir, e.Name()))
			}
		}
	}
	if len(todo) == 0 {
		return
	}
	start := time.Now()
	made, failed := 0, 0
	for _, t := range todo {
		bars, err := waveOf(t)
		if err == nil {
			err = os.WriteFile(wavePath(t.ID), bars, 0o644)
		}
		if err != nil {
			failed++
			hubLog.event("music_wave_failed", "track", t.ID, "error", err.Error())
			continue
		}
		made++
	}
	hubLog.event("music_waves_made", "made", made, "failed", failed, "s", int(time.Since(start).Seconds()))
	releaseMemory()
	logMemory("after_waves")
	l.scan()
}

// waveOf decodes a track and measures it in waveBars equal slices of time: each slice's average
// loudness (RMS), scaled so the loudest slice is 255. Peaks were tried first: loud rock hits the
// top in almost every slice, so the bar came out a solid block.
func waveOf(t *musicTrack) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nice", "-n", "19", "ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", t.path, "-vn", "-ac", "1", "-ar", "8000", "-f", "s16le", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	peaks, err := measure(bufio.NewReaderSize(out, 64<<10), t.DurationMs)
	if waitErr := cmd.Wait(); err == nil {
		err = waitErr
	}
	dropCache(t.path)
	if err != nil {
		return nil, err
	}
	return scaleBars(peaks), nil
}

// measure reads 16-bit samples and works out each slice's RMS. The length comes from the track's
// header; if it's off, samples past the end go in the last slice and a short track leaves the
// rest silent, rather than failing.
func measure(r io.Reader, durationMs int64) ([]float64, error) {
	total := durationMs * waveRate / 1000
	if total <= 0 {
		return nil, errors.New("length unknown")
	}
	perBar := float64(total) / waveBars
	sums := make([]float64, waveBars)
	counts := make([]float64, waveBars)
	var sample [2]byte
	for n := int64(0); ; n++ {
		if _, err := io.ReadFull(r, sample[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		v := float64(int16(binary.LittleEndian.Uint16(sample[:])))
		bar := min(int(float64(n)/perBar), waveBars-1)
		sums[bar] += v * v
		counts[bar]++
	}
	levels := make([]float64, waveBars)
	for i := range levels {
		if counts[i] > 0 {
			levels[i] = math.Sqrt(sums[i] / counts[i])
		}
	}
	return levels, nil
}

func scaleBars(peaks []float64) []byte {
	top := 0.0
	for _, p := range peaks {
		top = max(top, p)
	}
	bars := make([]byte, len(peaks))
	if top == 0 {
		return bars
	}
	for i, p := range peaks {
		bars[i] = byte(math.Round(p / top * 255))
	}
	return bars
}

func (l *musicLibrary) serveWave(w http.ResponseWriter, r *http.Request) {
	t := l.track(r.PathValue("id"))
	if t == nil || !t.HasWave {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, wavePath(t.ID))
}
