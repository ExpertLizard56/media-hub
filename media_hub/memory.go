package main

// The hub's own memory, in its log every 15 minutes (hub_memory), so "how lean is it" is
// measured, not guessed. Two kinds:
//
//   - go_*: what the hub itself holds (Go's heap and what it has taken from the system).
//   - cg_anon / cg_file: the add-on's container as Linux counts it. cg_file is files the hub has
//     read or served, cached by Linux; it's counted against the add-on but given back whenever
//     anything else needs the room. Home Assistant's figure for the add-on includes it.
//
// After a big job (making waveforms or small copies) the hub hands its unused heap back at once
// (releaseMemory), rather than whenever Go gets round to it.

import (
	"bufio"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const memoryLogEvery = 15 * time.Minute

func logMemoryForever() {
	for {
		logMemory("periodic")
		time.Sleep(memoryLogEvery)
	}
}

func logMemory(why string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fields := []any{
		"why", why,
		"go_heap_mb", mb(m.HeapAlloc),
		"go_sys_mb", mb(m.Sys - m.HeapReleased),
		"goroutines", runtime.NumGoroutine(),
	}
	for k, v := range cgroupMemory() {
		fields = append(fields, "cg_"+k+"_mb", mb(v))
	}
	hubLog.event("hub_memory", fields...)
}

// releaseMemory gives Go's unused heap back to the system now (after a big job).
func releaseMemory() {
	debug.FreeOSMemory()
}

// cgroupMemory reads anon (the hub's own) and file (cached files) from the container's memory.stat.
func cgroupMemory() map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open("/sys/fs/cgroup/memory.stat")
	if err != nil {
		return out
	}
	defer closeQuietly(f) // read only: a close error says nothing
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		name, value, ok := strings.Cut(scan.Text(), " ")
		if !ok || (name != "anon" && name != "file") {
			continue
		}
		if n, err := strconv.ParseUint(value, 10, 64); err == nil {
			out[name] = n
		}
	}
	return out
}

func mb(bytes uint64) string { return strconv.FormatFloat(float64(bytes)/(1<<20), 'f', 1, 64) }

// dropCache tells Linux the hub is done with a file it read in bulk (ffmpeg making a waveform or a
// small copy of it), so its cached copy isn't kept and charged to the add-on: reading a whole
// library for its waveforms otherwise leaves far more cache on the add-on's account than the
// hub's own memory. Harmless if it fails: the cache is freed when needed anyway.
func dropCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer closeQuietly(f) // read only: a close error says nothing
	// posix_fadvise(fd, 0, 0, POSIX_FADV_DONTNEED), with fadvise64's number for this architecture:
	// the add-on is built for arm64 and amd64 (config.yaml, arch), and nowhere else is it tried.
	if nr, ok := fadvise64[runtime.GOARCH]; ok {
		_, _, _ = syscall.Syscall6(nr, f.Fd(), 0, 0, 4, 0, 0)
	}
}

// fadvise64 is Linux's syscall number on each architecture the add-on builds for (4 is DONTNEED).
var fadvise64 = map[string]uintptr{"arm64": 223, "amd64": 221}
