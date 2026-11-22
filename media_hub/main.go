// Media Hub: a Home Assistant add-on that serves the TV and Music apps.
//
//   - Updates: GET /update/update.json and the APK it names, from /share/media-hub/updates
//     (scripts/publish-update.sh copies them there over SSH). GET /install (and GET /) is always
//     the newest APK, for a new box's first install: "homeassistant.local:8765" in the Downloader
//     app (Home Assistant's own name on the home network, so nobody types an address).
//   - Antenna channels from the HDHomeRun, shared between boxes: see antenna.go.
//   - Login sharing: one box hands its provider login, locked, to another device (share.go).
//   - Music: the library in /share/media-hub/music, for the Music app (music.go). The Music app's
//     own updates are in updates/music (scripts/publish-update.sh music).
//   - Diagnostics: POST /diagnostics, a batch of a box's event-log lines; kept per box and day under
//     /share/media-hub/diagnostics/<box>/, for DiagnosticsDays days. GET /diagnostics/ lists them.
//     The hub's own log (the HDHomeRun, streams, guide) goes in diagnostics/hub/: see eventlog.go.
//
// From outside the home, only boxes with the hub key get in, over the hub's own TLS: access.go.
// Optionally a second port serves the same hub over HTTPS with a certificate another add-on renews:
// https.go (options https_port and https_domain).
package main

import (
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	root = "/share/media-hub"
	// A box's diagnostics are kept this long.
	DiagnosticsDays = 30
	// The most one upload may carry, uncompressed: a day of a busy box's log is well under this.
	maxUpload = 8 << 20
)

var (
	updatesDir     = filepath.Join(root, "updates")
	diagnosticsDir = filepath.Join(root, "diagnostics")
	// Box ids are the app's own random ids: letters, digits and dashes only, so they're safe as folder names.
	boxID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// A log line's day, from the time it starts with.
	lineDay = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// One writer per box at a time, so two uploads never interleave lines.
	boxLocks sync.Map
	// Which box is at which address, from their uploads: names the box in the hub's log.
	boxesByAddress sync.Map
)

func main() {
	for _, dir := range []string{updatesDir, musicUpdatesDir, diagnosticsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("can't create %s: %v", dir, err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("GET /update/", http.StripPrefix("/update/", noStore(http.FileServer(http.Dir(updatesDir)))))
	mux.HandleFunc("GET /install", serveInstall)
	// The bare address is the install too, so there's nothing more to type than homeassistant.local:8765.
	mux.HandleFunc("GET /{$}", serveInstall)
	mux.HandleFunc("POST /diagnostics", receiveDiagnostics)
	mux.Handle("GET /diagnostics/", http.StripPrefix("/diagnostics/", http.FileServer(http.Dir(diagnosticsDir))))
	opts := readOptions()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { serveHealth(w, r, opts.RemoteAddress) })
	hubLog.event("hub_start", "version", version, "hdhomerun_option", opts.HDHomeRun)
	ant := newAntenna(opts.HDHomeRun)
	ant.routes(mux)
	newShares().routes(mux)
	music := newMusicLibrary()
	music.routes(mux)
	newFetcher(opts, music).routes(mux)
	go ant.monitor()

	go pruneDaily()
	go watchPublicAddress()
	go logMemoryForever()

	secrets, err := loadAccess()
	if err != nil {
		log.Fatalf("access: %v", err)
	}
	// Where a person finds what an app asks for to connect (Settings, Add-ons, this add-on, Log): only Home
	// Assistant's admins see this log.
	access := newHubAccess(secrets)
	log.Printf("To connect an app: open Media Hub in Home Assistant's sidebar (or this add-on's Open web UI)")
	if opts.RemoteAddress != "" {
		log.Printf("Away from home the app uses %s by itself once connected.", opts.RemoteAddress)
	}
	cert, err := loadCertificate()
	if err != nil {
		log.Fatalf("certificate: %v", err)
	}
	newAdminPage(access, opts.RemoteAddress).routes(mux)
	// The movie catalog (catalog.go) and its home-only Stremio test add-on (stremio.go); off without a TMDB key.
	cat := newCatalog(opts.TMDBKey, catalogDir)
	cat.routes(mux)
	stremioAddon{cat}.routes(mux)
	// The file track record (catalogfiles.go): works without a TMDB key; the Sorter reads its files.json.
	files := newFileTrack(catalogDir)
	files.routes(mux)
	go files.keepWritten()
	// Device finds (catalogdevices.go): what each box and TV can play; the Sorter reads devices.json.
	devices := newDeviceTrack(catalogDir)
	devices.routes(mux)
	go devices.keepWritten()
	if cat.on() {
		go cat.keepFresh()
	}
	addr := ":" + envOr("PORT", "8765")
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("Media Hub listening on %s (HTTP and TLS)", addr)
	// One handler for every way in, so the same access rules apply on the optional HTTPS port too.
	handler := logRequests(guard(access, opts.RemoteAddress, mux))
	server := &http.Server{Handler: handler}
	if opts.HTTPSPort > 0 && opts.HTTPSDomain != "" {
		go serveHTTPS(handler, opts.HTTPSPort, opts.HTTPSDomain)
	} else if opts.HTTPSPort != 0 || opts.HTTPSDomain != "" {
		log.Printf("https: an HTTPS listener needs both https_port and https_domain; one is missing, so only %s is served", addr)
	}
	log.Fatal(server.Serve(newSplitListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
}

// serveHealth answers "ok", plus two headers the apps use to pick their route (app.hub.Hub):
// X-Hub-Where (home or away: where this request came from, as guard sees it) and, to a request from
// home only, X-Hub-Remote (the hub's internet address now), so paired apps follow a change of address.
func serveHealth(w http.ResponseWriter, r *http.Request, remoteAddress string) {
	if fromHome(r) {
		w.Header().Set("X-Hub-Where", "home")
		if remoteAddress != "" {
			w.Header().Set("X-Hub-Remote", "https://"+strings.TrimPrefix(strings.TrimPrefix(remoteAddress, "https://"), "http://"))
		}
	} else {
		w.Header().Set("X-Hub-Where", "away")
	}
	if _, err := io.WriteString(w, "ok\n"); err != nil {
		log.Printf("health: %v", err)
	}
}

// receiveDiagnostics appends a box's upload to that box's file for today.
// Headers: X-Box-Id (required), X-Box-Name and X-App-Version (shown in the box's info file).
func receiveDiagnostics(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Box-Id")
	if !boxID.MatchString(id) {
		http.Error(w, "missing or bad X-Box-Id", http.StatusBadRequest)
		return
	}
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer closeQuietly(gz) // its content is read below; a close error has nothing to add
		body = gz
	}
	data, err := io.ReadAll(io.LimitReader(body, maxUpload+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(data) > maxUpload {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}

	lock, _ := boxLocks.LoadOrStore(id, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	dir := filepath.Join(diagnosticsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, "storage failed", http.StatusInternalServerError)
		log.Printf("diagnostics: mkdir %s: %v", dir, err)
		return
	}
	// Each line into the file for its own day (lines start with their time, "2026-09-29T22:58:42Z …"):
	// a box's first upload carries days of backlog.
	byDay := map[string]*strings.Builder{}
	today := time.Now().UTC().Format("2006-01-02")
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		day := today
		if len(line) >= 10 && lineDay.MatchString(line[:10]) {
			day = line[:10]
		}
		if byDay[day] == nil {
			byDay[day] = &strings.Builder{}
		}
		byDay[day].WriteString(line + "\n")
	}
	for day, lines := range byDay {
		if err := appendFile(filepath.Join(dir, day+".log"), lines.String()); err != nil {
			http.Error(w, "storage failed", http.StatusInternalServerError)
			log.Printf("diagnostics: %v", err)
			return
		}
	}
	boxesByAddress.Store(remoteHost(r), fmt.Sprintf("%s (%.8s)", r.Header.Get("X-Box-Name"), id))
	info, _ := json.MarshalIndent(map[string]string{
		"id":       id,
		"name":     r.Header.Get("X-Box-Name"),
		"version":  r.Header.Get("X-App-Version"),
		"lastSeen": time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "box.json"), info, 0o644); err != nil {
		log.Printf("diagnostics: box.json: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// boxLabel names the box making a request ("Living Room TV (c13c8dfc)"), or
// gives its address if it hasn't sent diagnostics yet.
func boxLabel(r *http.Request) string {
	host := remoteHost(r)
	if name, ok := boxesByAddress.Load(host); ok {
		return name.(string)
	}
	return host
}

func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close() // the write's error is the one to report
		return err
	}
	return f.Close()
}

// pruneDaily deletes diagnostics older than DiagnosticsDays, once at start and then daily.
func pruneDaily() {
	for {
		cutoff := time.Now().AddDate(0, 0, -DiagnosticsDays)
		files, _ := filepath.Glob(filepath.Join(diagnosticsDir, "*", "*.log"))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil && st.ModTime().Before(cutoff) {
				if err := os.Remove(f); err != nil {
					log.Printf("prune %s: %v", f, err)
				}
			}
		}
		time.Sleep(24 * time.Hour)
	}
}

// serveInstall sends the newest TV app, whatever its version, named so the box knows it's an app.
func serveInstall(w http.ResponseWriter, r *http.Request) { serveNewestAPK(w, r, updatesDir) }

// serveNewestAPK sends the APK that dir's update.json names.
func serveNewestAPK(w http.ResponseWriter, r *http.Request, dir string) {
	var latest struct {
		APK string `json:"apk"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "update.json"))
	if err == nil {
		err = json.Unmarshal(data, &latest)
	}
	if err != nil || latest.APK == "" || filepath.Base(latest.APK) != latest.APK {
		http.Error(w, "no version published yet", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	// Named with its version (tv-156.apk): a phone keeps every download, and with one name for
	// all of them the old one is easy to open by mistake.
	w.Header().Set("Content-Disposition", `attachment; filename="`+latest.APK+`"`)
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(dir, latest.APK))
}

// noStore keeps boxes from caching update.json: a stale copy would hide a new version.
func noStore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Cache-Control", "no-store")
		}
		h.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s %s", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// options are the add-on's settings (Home Assistant writes them to /data/options.json).
type options struct {
	// The HDHomeRun's address; empty to find it by SiliconDust's discovery.
	HDHomeRun string `json:"hdhomerun"`
	// For getting new music (musicfetch.go): TorBox downloads, NZBgeek searches.
	TorBoxKey  string `json:"torbox_api_key"`
	NZBgeekKey string `json:"nzbgeek_api_key"`
	// The hub's address from the internet (name:port, the router forwarding that port here), handed to an app as it
	// pairs (access.go); empty when the hub is only used at home.
	RemoteAddress string `json:"remote_address"`
	// Where phone notices go (publicip.go): notify service names, comma-separated. Empty means every
	// notify service whose name starts with mobile_app_.
	// An HTTPS listener beside port 8765 (https.go), with the certificate of /ssl/<HTTPSDomain>: both
	// must be set, and port 0 means off.
	HTTPSPort   int    `json:"https_port"`
	HTTPSDomain string `json:"https_domain"`
	// TMDB's API key (a short v3 key or a long read access token) for the movie catalog (catalog.go);
	// empty = no catalog.
	TMDBKey string `json:"tmdb_api_key"`
}

func readOptions() options {
	var o options
	if data, err := os.ReadFile("/data/options.json"); err == nil {
		if err := json.Unmarshal(data, &o); err != nil {
			log.Printf("options: %v", err)
		}
	}
	return o
}

// closeQuietly closes something the hub is done with: a response body whose content has been read,
// or a file it only read. The work is finished (or has failed with an error of its own), so a close
// error has nothing to add.
func closeQuietly(c io.Closer) { _ = c.Close() }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
