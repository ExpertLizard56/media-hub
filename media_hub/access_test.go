package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
