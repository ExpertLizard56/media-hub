package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPhonesQuiet(t *testing.T) {
	old := phoneQuietFile
	phoneQuietFile = filepath.Join(t.TempDir(), "phone-quiet-until.txt")
	t.Cleanup(func() { phoneQuietFile = old })
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		text  string
		write bool
		quiet bool
	}{
		{"missing file", "", false, false},
		{"time still to come", "2026-11-16T12:00:01-05:00", true, true},
		{"time past", "2026-11-16T11:59:59Z", true, false},
		{"exactly now", "2026-11-16T12:00:00Z", true, false},
		{"no zone", "2026-11-16T18:00:00", true, false},
		{"not a time", "later", true, false},
		{"empty file", "", true, false},
		{"trailing newline", "2026-11-16T17:00:00Z\n", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(phoneQuietFile)
			if c.write {
				if err := os.WriteFile(phoneQuietFile, []byte(c.text), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			until, quiet := phonesQuiet(now)
			if quiet != c.quiet {
				t.Fatalf("phonesQuiet = %v (until %v), want %v", quiet, until, c.quiet)
			}
			if quiet && !until.After(now) {
				t.Errorf("quiet until %v, which is not after %v", until, now)
			}
		})
	}
}
