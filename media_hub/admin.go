package main

// The hub's own page in Home Assistant (the add-on's web UI, "ingress"): how to connect a device (the
// address and install code an app asks for); every device, paired or reporting in, with its logs and what can be
// done with it (rename, block, remove); a new install code; and the hub's own log split by what it's about. Logs
// download whole (a device's every day or one day, a section, or everything as one zip), never by line counts.
//
// It shows the install code, so it answers only through Home Assistant (which signs the person in): requests from
// Home Assistant's own proxy address. Links are relative, since Home Assistant serves it under a path of its own.

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ingressProxy is the address Home Assistant's ingress proxy always uses to reach an add-on.
const ingressProxy = "172.30.32.2"

// logSection is one part of the hub's own log: the events it holds, by the start of their names.
type logSection struct {
	ID, Title, About string
	Prefixes         []string
}

// The hub's log, split by what it's about. An event in none of these is in "hub".
var logSections = []logSection{
	{"antenna", "Antenna & HDHomeRun", "The tuner, its signal, the lineup and guide, who's watching, stream errors.",
		[]string{"hdhomerun_", "signal_survey", "tuner_", "tuned", "tune_failed", "stream_errors", "viewer_", "lineup_", "guide_"}},
	{"music", "Music library", "Scanning the music folder, covers, the phone-sized copies.",
		[]string{"music_scanned", "music_wave", "music_mobile", "musicbrainz_"}},
	{"downloads", "Music downloads", "Albums looked up and fetched (MusicBrainz, the indexer, the download service).",
		[]string{"music_fetch", "music_album", "music_lookup"}},
	{"connections", "Connections", "Devices pairing, installing or refused from outside, blocked or removed, logins handed between devices.",
		[]string{"remote_", "app_paired", "login_share_", "access_created", "certificate_created", "install_code_", "device_"}},
	{"hub", "Hub", "Starts, memory, the home's internet address, notifications.", nil},
}

func sectionOf(event string) string {
	for _, s := range logSections {
		for _, p := range s.Prefixes {
			if strings.HasPrefix(event, p) {
				return s.ID
			}
		}
	}
	return "hub"
}

type adminPage struct {
	access        *hubAccess
	remoteAddress string
}

func newAdminPage(access *hubAccess, remoteAddress string) *adminPage {
	return &adminPage{access: access, remoteAddress: remoteAddress}
}

func (a *adminPage) routes(mux *http.ServeMux) {
	mux.Handle("GET /admin/{$}", a.only(a.index))
	mux.Handle("GET /admin/device-logs", a.only(a.deviceLogs))
	mux.Handle("GET /admin/section/{id}", a.only(a.sectionLog))
	mux.Handle("GET /admin/everything.zip", a.only(a.everything))
	mux.Handle("POST /admin/device/{id}/{action}", a.only(a.deviceAction))
	mux.Handle("POST /admin/new-code", a.only(a.newCode))
}

// only lets Home Assistant's proxy in, and nobody else (not the home network, not the internet).
func (a *adminPage) only(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if remoteHost(r) != ingressProxy {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	})
}

type deviceRow struct {
	ID, Name, Version, LastSeen string
	// "own" (paired, a key of its own), "shared" (reports in, uses the one shared key or none), "blocked".
	Access string
	Paired bool
	// Seen in the last week: listed first; the rest fold under "Older devices".
	Recent bool
	seen   time.Time
	Days   []string
}

// devices lists every device: those paired (devices.json) and those that report in (diagnostics/<id>), newest first.
func (a *adminPage) devices() []deviceRow {
	rows := map[string]*deviceRow{}
	row := func(id string) *deviceRow {
		if rows[id] == nil {
			rows[id] = &deviceRow{ID: id, Name: "Unnamed device", Access: "shared"}
		}
		return rows[id]
	}
	entries, _ := os.ReadDir(diagnosticsDir)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "hub" || !boxID.MatchString(e.Name()) {
			continue
		}
		r := row(e.Name())
		if data, err := os.ReadFile(filepath.Join(diagnosticsDir, e.Name(), "box.json")); err == nil {
			var info map[string]string
			if json.Unmarshal(data, &info) == nil {
				if info["name"] != "" {
					r.Name = info["name"]
				}
				r.Version = info["version"]
				if t, err := time.Parse(time.RFC3339, info["lastSeen"]); err == nil {
					r.seen = t
				}
			}
		}
		r.Days = logDays(filepath.Join(diagnosticsDir, e.Name()))
	}
	for _, d := range a.access.list() {
		r := row(d.ID)
		if d.Name != "" {
			r.Name = d.Name
		}
		if d.Key != "" {
			r.Paired = true
			r.Access = "own"
			if d.Blocked {
				r.Access = "blocked"
			}
		}
		if d.LastAway.After(r.seen) {
			r.seen = d.LastAway
		}
		if d.Paired.After(r.seen) {
			r.seen = d.Paired
		}
	}
	out := make([]deviceRow, 0, len(rows))
	for _, r := range rows {
		r.LastSeen = when(r.seen)
		r.Recent = time.Since(r.seen) < 7*24*time.Hour
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seen.After(out[j].seen) })
	return out
}

// when says a time the short way: "Today 2:14 PM", "Yesterday 9:02 AM", "Nov 3, 9:02 AM"; "" for none.
func when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.Local()
	now := time.Now()
	switch {
	case t.Format("2006-01-02") == now.Format("2006-01-02"):
		return "Today " + t.Format("3:04 PM")
	case t.Format("2006-01-02") == now.AddDate(0, 0, -1).Format("2006-01-02"):
		return "Yesterday " + t.Format("3:04 PM")
	default:
		return t.Format("Jan 2, 3:04 PM")
	}
}

// logDays lists a folder's day files ("2026-11-09"), newest first.
func logDays(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	var days []string
	for _, f := range files {
		day := strings.TrimSuffix(filepath.Base(f), ".log")
		if lineDay.MatchString(day) {
			days = append(days, day)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	return days
}

// lanAddress is the hub's own address on the home network, for devices that can't find homeassistant.local.
func lanAddress() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.To4() != nil && ip.IP.IsPrivate() && !strings.HasPrefix(ip.IP.String(), "172.") {
			return ip.IP.String()
		}
	}
	return ""
}

func (a *adminPage) index(w http.ResponseWriter, _ *http.Request) {
	// Each section's last entry: its newest day file is enough, unless a section had nothing that day.
	last := map[string]time.Time{}
	for _, day := range logDays(hubLog.dir) {
		forEachLine(filepath.Join(hubLog.dir, day+".log"), func(line string) {
			if t, err := time.Parse(time.RFC3339Nano, strings.SplitN(line, " ", 2)[0]); err == nil {
				if s := sectionOf(eventName(line)); t.After(last[s]) {
					last[s] = t
				}
			}
		})
		if len(last) == len(logSections) {
			break
		}
	}
	type sectionRow struct {
		logSection
		Last string
	}
	var sections []sectionRow
	for _, s := range logSections {
		sections = append(sections, sectionRow{s, when(last[s.ID])})
	}
	var recent, older []deviceRow
	for _, d := range a.devices() {
		if d.Recent {
			recent = append(recent, d)
		} else {
			older = append(older, d)
		}
	}
	data := map[string]any{
		"Recent":   recent,
		"Older":    older,
		"Version":  version,
		"Code":     a.access.installCode(),
		"LAN":      lanAddress(),
		"Remote":   a.remoteAddress,
		"Sections": sections,
		"KeepDays": DiagnosticsDays,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := adminTemplate.Execute(w, data); err != nil {
		log.Printf("admin page: %v", err)
	}
}

// deviceAction: rename (form "name"), block, unblock or remove, then back to the page.
func (a *adminPage) deviceAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !boxID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("action") {
	case "rename":
		if name := strings.TrimSpace(r.FormValue("name")); name != "" && len(name) <= 80 {
			a.access.rename(id, name)
		}
	case "block":
		a.access.setBlocked(id, true)
	case "unblock":
		a.access.setBlocked(id, false)
	case "remove":
		a.access.remove(id)
	default:
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "../../", http.StatusSeeOther)
}

func (a *adminPage) newCode(w http.ResponseWriter, r *http.Request) {
	if err := a.access.newInstallCode(); err != nil {
		http.Error(w, "couldn't save the new code", http.StatusInternalServerError)
		log.Printf("new install code: %v", err)
		return
	}
	http.Redirect(w, r, "./", http.StatusSeeOther)
}

// deviceLogs sends the chosen devices' logs (?id=…&id=…, or all=1) for the last ?days= days (0: every day kept):
// one device as a plain log, several as a zip with a folder each.
func (a *adminPage) deviceLogs(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	chosen := map[string]bool{}
	for _, id := range r.URL.Query()["id"] {
		chosen[id] = true
	}
	var picked []deviceRow
	for _, d := range a.devices() {
		if (r.URL.Query().Get("all") == "1" || chosen[d.ID]) && len(d.Days) > 0 {
			picked = append(picked, d)
		}
	}
	if len(picked) == 0 {
		http.Error(w, "No logs for the devices chosen.", http.StatusNotFound)
		return
	}
	// The window starts at local midnight [days]-1 days ago ("Today" = since midnight). Day files are named by
	// UTC day, so the files that can hold it are picked by UTC date and each line is checked by its own time.
	var since time.Time
	if days > 0 {
		y, m, d := time.Now().Date()
		since = time.Date(y, m, d-(days-1), 0, 0, 0, 0, time.Local)
	}
	// A device's day files to send, oldest first.
	files := func(d deviceRow) []string {
		var out []string
		first := since.UTC().Format("2006-01-02")
		for i := len(d.Days) - 1; i >= 0; i-- {
			if days == 0 || d.Days[i] >= first {
				out = append(out, d.Days[i])
			}
		}
		return out
	}
	copyDay := func(out io.Writer, d deviceRow, day string) {
		forEachLine(filepath.Join(diagnosticsDir, d.ID, day+".log"), func(line string) {
			stamp, _, _ := strings.Cut(line, " ")
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil && t.Before(since) {
				return
			}
			_, _ = io.WriteString(out, localTime(line)+"\n")
		})
	}
	if len(picked) == 1 {
		d := picked[0]
		sendLog(w, safeName(d.Name)+".log", func(out io.Writer) {
			for _, day := range files(d) {
				copyDay(out, d, day)
			}
		})
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="device-logs-`+time.Now().Format("2006-01-02")+`.zip"`)
	z := zip.NewWriter(w)
	for _, d := range picked {
		folder := safeName(d.Name) + " (" + d.ID[:min(8, len(d.ID))] + ")/"
		for _, day := range files(d) {
			if f, err := z.Create(folder + day + ".log"); err == nil {
				copyDay(f, d, day)
			}
		}
	}
	if err := z.Close(); err != nil {
		log.Printf("device logs zip: %v", err)
	}
}

// sectionLog sends one section of the hub's own log, every day kept, oldest first.
func (a *adminPage) sectionLog(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("id"), ".log")
	known := false
	for _, s := range logSections {
		known = known || s.ID == id
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	days := logDays(hubLog.dir)
	sendLog(w, "hub-"+id+".log", func(out io.Writer) {
		for i := len(days) - 1; i >= 0; i-- {
			forEachLine(filepath.Join(hubLog.dir, days[i]+".log"), func(line string) {
				if sectionOf(eventName(line)) == id {
					_, _ = io.WriteString(out, localTime(line)+"\n")
				}
			})
		}
	})
}

// everything sends all the logs kept, devices and hub, as one zip, each device in a folder by its name.
func (a *adminPage) everything(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="media-hub-logs-`+time.Now().Format("2006-01-02")+`.zip"`)
	z := zip.NewWriter(w)
	add := func(name, path string) {
		f, err := z.Create(name)
		if err == nil {
			copyLog(f, path)
		}
	}
	for _, day := range logDays(hubLog.dir) {
		add("hub/"+day+".log", filepath.Join(hubLog.dir, day+".log"))
	}
	for _, d := range a.devices() {
		folder := "devices/" + safeName(d.Name) + " (" + d.ID[:min(8, len(d.ID))] + ")/"
		for _, day := range d.Days {
			add(folder+day+".log", filepath.Join(diagnosticsDir, d.ID, day+".log"))
		}
	}
	if err := z.Close(); err != nil {
		log.Printf("admin zip: %v", err)
	}
}

func sendLog(w http.ResponseWriter, name string, write func(io.Writer)) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	write(w)
}

// copyLog copies a log file with each line's time in the hub's own time zone, the way a person reads it.
func copyLog(out io.Writer, path string) {
	forEachLine(path, func(line string) { _, _ = io.WriteString(out, localTime(line)+"\n") })
}

// localTime turns a line's leading time (UTC, "2026-11-10T01:02:15.022875Z") into local time
// ("2026-11-09 8:02:15 PM"); a line without one is left as it is.
func localTime(line string) string {
	stamp, rest, _ := strings.Cut(line, " ")
	t, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return line
	}
	return t.Local().Format("2006-01-02 3:04:05 PM") + " " + rest
}

func forEachLine(path string, each func(string)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer closeQuietly(f)
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		each(s.Text())
	}
}

// eventName is a log line's event: the word after its time.
func eventName(line string) string {
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"days": func(n int) string {
		if n == 1 {
			return "1 day"
		}
		return strconv.Itoa(n) + " days"
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Media Hub</title>
<style>
:root { color-scheme: light dark; --bg:#f5f6f8; --card:#fff; --text:#1c1f24; --dim:#5f6670; --line:#e1e4e8; --accent:#03a9f4; --bad:#d93025; }
@media (prefers-color-scheme: dark) { :root { --bg:#111418; --card:#1c2026; --text:#e6e8eb; --dim:#9aa1aa; --line:#2b3038; --bad:#f28b82; } }
* { box-sizing:border-box; }
body { margin:0; background:var(--bg); color:var(--text); font:15px/1.45 system-ui, sans-serif; }
main { max-width:760px; margin:0 auto; padding:20px 14px 48px; }
h1 { font-size:24px; margin:0; } h2 { font-size:17px; margin:0 0 4px; }
.dim { color:var(--dim); font-size:14px; } .card { background:var(--card); border:1px solid var(--line); border-radius:14px; padding:16px; margin:14px 0; }
.big { font:600 22px/1.3 ui-monospace, monospace; letter-spacing:.5px; }
.pair { display:flex; flex-wrap:wrap; gap:18px 32px; margin:12px 0; }
.item { border-top:1px solid var(--line); padding:12px 0; } .item:first-of-type { border-top:none; }
.top { display:flex; align-items:center; gap:8px; flex-wrap:wrap; } .name { font-weight:600; }
.actions { display:flex; flex-wrap:wrap; gap:6px; margin-top:8px; align-items:center; }
a { color:var(--accent); text-decoration:none; }
.btn, button { display:inline-block; padding:5px 12px; border:1px solid var(--accent); border-radius:999px; background:none; color:var(--accent); font:inherit; font-size:14px; cursor:pointer; white-space:nowrap; line-height:1.3; }
button.bad { border-color:var(--bad); color:var(--bad); } form { display:inline; margin:0; }
input[type=text] { font:inherit; padding:5px 9px; border:1px solid var(--line); border-radius:8px; background:var(--bg); color:var(--text); width:200px; max-width:100%; }
.tag { font-size:12px; padding:1px 8px; border-radius:999px; border:1px solid var(--line); color:var(--dim); white-space:nowrap; }
.tag.blocked { border-color:var(--bad); color:var(--bad); }
details > summary { list-style:none; cursor:pointer; } details > summary::-webkit-details-marker { display:none; }
.more { margin-top:8px; display:flex; flex-wrap:wrap; gap:6px 12px; }
.older > summary { color:var(--accent); padding-top:10px; }
.picker { margin-top:10px; } .check { display:flex; gap:10px; align-items:center; padding:7px 0; border-top:1px solid var(--line); }
.check input { width:18px; height:18px; } select { font:inherit; padding:5px 8px; border-radius:8px; border:1px solid var(--line); background:var(--bg); color:var(--text); margin:6px 8px 0 0; }
</style></head><body><main>
<h1>Media Hub</h1>
<div class="dim">Version {{.Version}}</div>

<div class="card">
<h2>Connect a device</h2>
<div class="dim">TV app: Settings, Sources, Media Hub. The Music app asks when it opens.</div>
<div class="pair">
<div><div class="dim">Hub address</div><div class="big">homeassistant.local</div>{{if .LAN}}<div class="dim">or {{.LAN}}</div>{{end}}</div>
<div><div class="dim">Install code</div><div class="big">{{.Code}}</div></div>
</div>
<div class="actions"><form method="post" action="new-code" onsubmit="return confirm('Make a new install code? Nobody can connect with the old one. Connected devices keep working.')"><button>New install code</button></form></div>
<p class="dim">{{if .Remote}}Away from home, connected devices use {{.Remote}} by themselves.{{else}}No internet address set (Configuration, remote_address): devices work at home only.{{end}}
Only give the code to people you'd let in.</p>
</div>

{{define "device"}}<div class="item">
<div class="top"><span class="name">{{.Name}}</span>{{if eq .Access "own"}}<span class="tag">Own key</span>{{else if eq .Access "blocked"}}<span class="tag blocked">Blocked</span>{{else}}<span class="tag">Shared key</span>{{end}}</div>
<div class="dim">{{if .LastSeen}}Last seen {{.LastSeen}}{{end}}{{if .Version}} · App {{.Version}}{{end}} · {{printf "%.8s" .ID}}</div>
<div class="actions">
<details><summary class="btn">Rename</summary><div class="more"><form method="post" action="device/{{.ID}}/rename"><input type="text" name="name" value="{{.Name}}" maxlength="80"> <button>Save</button></form></div></details>
{{if eq .Access "own"}}<form method="post" action="device/{{.ID}}/block"><button class="bad">Block</button></form>{{else if eq .Access "blocked"}}<form method="post" action="device/{{.ID}}/unblock"><button>Unblock</button></form>{{end}}
<form method="post" action="device/{{.ID}}/remove" onsubmit="return confirm('Remove {{.Name}}? Its key stops working and its logs are deleted.')"><button class="bad">Remove</button></form>
</div>
</div>{{end}}

<div class="card">
<h2>Devices</h2>
<div class="dim">Block works from outside the home only. Their logs are under Logs, below.</div>
{{range .Recent}}{{template "device" .}}{{else}}<p class="dim">No device seen this week.</p>{{end}}
{{if .Older}}<details class="older"><summary>Older devices ({{len .Older}})</summary>{{range .Older}}{{template "device" .}}{{end}}</details>{{end}}
</div>

<div class="card">
<h2>Logs</h2>
<div class="dim">Kept {{.KeepDays}} days.</div>
<div class="item">
<details id="devlogs"><summary><span class="name">Device logs</span> <span class="dim">&#9662;</span><div class="dim">Each phone, tablet and box's health and playback log.</div></summary>
<form method="get" action="device-logs" class="picker">
<label class="check"><input type="checkbox" name="all" value="1" id="allDevices"> <strong>All devices</strong></label>
{{range .Recent}}{{if .Days}}<label class="check"><input type="checkbox" name="id" value="{{.ID}}" class="dev"> {{.Name}} <span class="dim">{{days (len .Days)}}</span></label>{{end}}{{end}}
{{range .Older}}{{if .Days}}<label class="check"><input type="checkbox" name="id" value="{{.ID}}" class="dev"> {{.Name}} <span class="dim">{{days (len .Days)}} · older</span></label>{{end}}{{end}}
<div id="howFar" hidden>
<div class="dim" style="margin-top:10px">How far back</div>
<select name="days"><option value="1">Today</option><option value="3">Last 3 days</option><option value="7" selected>Last 7 days</option><option value="14">Last 14 days</option><option value="0">Everything kept</option></select>
<button>Download</button>
</div>
</form>
</details>
</div>
{{range .Sections}}<div class="item">
<div class="top"><span class="name">{{.Title}}</span><span class="tag">Hub</span></div>
<div class="dim">{{.About}}</div>
<div class="actions"><a class="btn" href="section/{{.ID}}.log">Download</a><span class="dim">{{if .Last}}Last entry {{.Last}}{{else}}Nothing yet{{end}}</span></div>
</div>{{end}}
<div class="actions" style="margin-top:12px"><a class="btn" href="everything.zip">Download everything (zip)</a></div>
</div>
</main>
<script>
// Device logs: All devices ticks every device; the How far back choice shows once one is ticked.
const all = document.getElementById("allDevices"), devs = [...document.querySelectorAll(".dev")], howFar = document.getElementById("howFar");
function update() { howFar.hidden = !(all.checked || devs.some(d => d.checked)); }
all.addEventListener("change", () => { devs.forEach(d => { d.checked = all.checked; d.disabled = all.checked; }); update(); });
devs.forEach(d => d.addEventListener("change", update));
</script>
</body></html>`))
