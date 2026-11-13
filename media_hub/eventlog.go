package main

// The hub's own diagnostics, kept next to the boxes' (diagnostics/hub/<day>.log, pruned with
// them) in the same line format, so both read the same way and line up by time:
//
//	2026-09-30T04:24:16.022875Z stream_errors channel=16.15 lost=21 signal_quality=49
//
// What goes in: the HDHomeRun (device, lineup, each tuner's signal while in use, an hourly
// reception survey) and the hub's own work (tuning, who's watching, stream errors, guide fetches).

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// version is the add-on's version, set at build time (Dockerfile).
var version = "dev"

type eventLog struct {
	mu  sync.Mutex
	dir string
}

var hubLog = &eventLog{dir: filepath.Join(diagnosticsDir, "hub")}

// event writes one line: the event's name, then key=value pairs (fields alternate key, value).
// Values with spaces or quotes are quoted; nil values are left out.
func (l *eventLog) event(name string, fields ...any) {
	var b strings.Builder
	now := time.Now().UTC()
	b.WriteString(now.Format(time.RFC3339Nano))
	b.WriteString(" " + name)
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i+1] == nil {
			continue
		}
		text := fmt.Sprint(fields[i+1])
		if strings.ContainsAny(text, " \t\n\"") {
			text = `"` + strings.ReplaceAll(strings.ReplaceAll(text, `"`, `\"`), "\n", " ") + `"`
		}
		fmt.Fprintf(&b, " %v=%s", fields[i], text)
	}
	line := b.String()
	log.Print(line[len(now.Format(time.RFC3339Nano))+1:])

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		log.Printf("hub log: %v", err)
		return
	}
	if err := appendFile(filepath.Join(l.dir, now.Format("2006-01-02")+".log"), line+"\n"); err != nil {
		log.Printf("hub log: %v", err)
	}
}
