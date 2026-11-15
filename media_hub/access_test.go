package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAwayInstallOverHTTPS: a new box away from home installs with the install code over plain HTTP or
// HTTPS (no key yet); everything else away still needs the key.
func TestAwayInstallOverHTTPS(t *testing.T) {
	hubLog = &eventLog{dir: t.TempDir()}
	dir := t.TempDir()
	old := updatesDir
	updatesDir = dir
	t.Cleanup(func() { updatesDir = old })
	if err := os.WriteFile(filepath.Join(dir, "update.json"), []byte(`{"apk":"tv-9.apk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tv-9.apk"), []byte("apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	access := &hubAccess{secrets: accessSecrets{Key: "k", InstallCode: "c0de"}, devices: map[string]*pairedDevice{}}
	h := guard(access, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hub")) }))
	get := func(path string, overTLS bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "203.0.113.5:4000"
		if overTLS {
			r.TLS = &tls.ConnectionState{}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	for _, overTLS := range []bool{false, true} {
		if rec := get("/i/c0de", overTLS); rec.Code != http.StatusOK || rec.Body.String() != "apk" {
			t.Errorf("install (tls=%v): %d %q", overTLS, rec.Code, rec.Body.String())
		}
		if rec := get("/i/wrong", overTLS); rec.Code != http.StatusNotFound {
			t.Errorf("wrong code (tls=%v): %d", overTLS, rec.Code)
		}
		if rec := get("/catalog/rows", overTLS); rec.Code != http.StatusNotFound {
			t.Errorf("no key (tls=%v): %d", overTLS, rec.Code)
		}
	}
}

// TestPairAtHome: /pair/home pairs only from home and only while the window file holds a time to come.
func TestPairAtHome(t *testing.T) {
	hubLog = &eventLog{dir: t.TempDir()}
	dir := t.TempDir()
	oldTLS, oldFile, oldDevices := tlsDir, pairHomeFile, devicesFile
	tlsDir, pairHomeFile, devicesFile = dir, filepath.Join(dir, "pair-home-until.txt"), filepath.Join(dir, "devices.json")
	t.Cleanup(func() { tlsDir, pairHomeFile, devicesFile = oldTLS, oldFile, oldDevices })
	if err := os.WriteFile(filepath.Join(dir, "pin.txt"), []byte("sha256/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	access := &hubAccess{secrets: accessSecrets{Key: "k", InstallCode: "c0de"}, devices: map[string]*pairedDevice{}}
	h := guard(access, "pi.example:8766", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	pair := func(from string) int {
		r := httptest.NewRequest(http.MethodGet, "/pair/home?id=box-1&name=Box", nil)
		r.RemoteAddr = from + ":4000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), `"remoteUrl":"https://pi.example:8766"`) {
			t.Errorf("answer %s", rec.Body.String())
		}
		return rec.Code
	}
	if c := pair("192.168.1.50"); c != http.StatusNotFound {
		t.Errorf("no window: %d", c)
	}
	write := func(until time.Time) {
		if err := os.WriteFile(pairHomeFile, []byte(until.Format(time.RFC3339)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Now().Add(time.Hour))
	if c := pair("192.168.1.50"); c != http.StatusOK {
		t.Errorf("open, from home: %d", c)
	}
	if c := pair("203.0.113.5"); c != http.StatusNotFound {
		t.Errorf("open, from away: %d", c)
	}
	write(time.Now().Add(-time.Minute))
	if c := pair("192.168.1.50"); c != http.StatusNotFound {
		t.Errorf("window passed: %d", c)
	}
}
