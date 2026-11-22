package main

// Device finds (the record keeper cog): what each streaming box and the TV it feeds can play.
// Two halves, joined per box:
//   - device-facts.json, written by the PC's "Device facts.ps1" over adb: hardware decoders, the TV's HDR
//     support (EDID), the HDR/frame-rate settings, the box's IP, the sound path.
//   - what the plays showed: plays.jsonl (engine, decoder, started, start time, display mode) and
//     audio.jsonl (the audio format each minute went out as).
// Served at GET /catalog/devices and /catalog/device/{box or ip} (home only), and written to devices.json
// in the catalog folder for the Sorter (it can match a request's client IP to a box). Fields are only added.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var deviceInputs = []string{"device-facts.json", "plays.jsonl", "audio.jsonl"}

type deviceRecords struct {
	Updated time.Time       `json:"updated"`
	Facts   string          `json:"facts_time,omitempty"` // when the PC last read the boxes
	Devices []*deviceRecord `json:"devices"`
}

// tally: plays of one kind (a decoder, an engine) and how they went.
type tally struct {
	Plays   int `json:"plays"`
	Started int `json:"started"`
	Failed  int `json:"failed"`
}

type deviceSeen struct {
	Apps         map[string]int    `json:"apps"` // every recorded play by app; the rest counts Stremio's only
	Plays        int               `json:"plays"`
	Started      int               `json:"started"`
	Failed       int               `json:"failed"`
	Stalled      int               `json:"stalled"`
	StartMedian  float64           `json:"start_s_median,omitempty"`
	LastPlay     time.Time         `json:"last_play"`
	Decoders     map[string]*tally `json:"decoders"`
	Engines      map[string]*tally `json:"engines"`
	Displays     map[string]int    `json:"displays"`       // display mode during plays -> plays
	AudioFormats map[string]int    `json:"audio_formats"`  // format the box sent -> minutes
	AudioToTV    map[string]int    `json:"audio_to_tv"`    // what reached the TV -> minutes
}

type deviceCanPlay struct {
	Video      []string `json:"video"`       // hardware video decoders
	HDR        []string `json:"hdr"`         // what the TV shows (the Fire TV: its own panel)
	Audio      []string `json:"audio"`       // decoded on the box (hardware, or Stremio's ffmpeg)
	AudioOut   string   `json:"audio_out,omitempty"` // the box's surround setting (device facts): stereo PCM or bitstream
	AudioToTV  []string `json:"audio_to_tv"` // formats seen going out undecoded (none when the box is set to stereo PCM)
	Proven     []string `json:"proven"`      // decoders with at least one started play here
	FailedOnly []string `json:"failed_only"` // decoders tried here that never started
}

type deviceRecord struct {
	Box     string          `json:"box"`
	Name    string          `json:"name,omitempty"`
	IP      string          `json:"ip,omitempty"`
	Facts   json.RawMessage `json:"facts,omitempty"` // device-facts.json entry as written
	Seen    *deviceSeen     `json:"seen"`
	CanPlay *deviceCanPlay  `json:"can_play"`

	starts []float64
}

type deviceTrack struct {
	dir string
	now func() time.Time

	mu    sync.Mutex
	sig   string
	built *deviceRecords
}

func newDeviceTrack(dir string) *deviceTrack { return &deviceTrack{dir: dir, now: time.Now} }

func (t *deviceTrack) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /catalog/devices", func(w http.ResponseWriter, r *http.Request) {
		if !fromHome(r) {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, t.get())
	})
	mux.HandleFunc("GET /catalog/device/{box}", func(w http.ResponseWriter, r *http.Request) {
		if !fromHome(r) {
			http.NotFound(w, r)
			return
		}
		k := strings.ToLower(r.PathValue("box"))
		for _, d := range t.get().Devices {
			if d.Box == k || d.IP == k {
				writeJSON(w, d)
				return
			}
		}
		http.NotFound(w, r)
	})
}

func (t *deviceTrack) keepWritten() {
	for {
		t.get()
		time.Sleep(time.Minute)
	}
}

func (t *deviceTrack) get() *deviceRecords {
	sig := ""
	for _, n := range deviceInputs {
		if st, err := os.Stat(filepath.Join(t.dir, n)); err == nil {
			sig += n + st.ModTime().Format(time.RFC3339Nano) + itoa(st.Size()) + ";"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.built != nil && sig == t.sig {
		return t.built
	}
	t.built, t.sig = buildDeviceRecords(t.dir, t.now()), sig
	if sig != "" {
		if err := writeJSONFile(filepath.Join(t.dir, "devices.json"), t.built); err != nil {
			hubLog.event("devices_write_failed", "error", err)
		}
	}
	return t.built
}

type deviceFact struct {
	Box     string   `json:"box"`
	Name    string   `json:"name"`
	IP      string   `json:"ip"`
	VideoHW []string `json:"video_hw"`
	AudioHW []string `json:"audio_hw"`
	AudioSW []string `json:"audio_sw"`
	TVHDR   []string `json:"tv_hdr"`
	AudioOut string  `json:"audio_out"`
}

// Android audio HAL format names -> the short codec names device-facts.json uses.
var halCodec = map[string]string{"DOLBY_DIGITAL": "ac3", "AC3": "ac3", "DOLBY_DIGITAL_PLUS": "eac3", "E_AC3": "eac3",
	"EAC3": "eac3", "DTS": "dts", "DTS_HD": "dts-hd", "DOLBY_TRUEHD": "truehd", "AC4": "ac4"}

func buildDeviceRecords(dir string, now time.Time) *deviceRecords {
	out := &deviceRecords{Updated: now, Devices: []*deviceRecord{}}
	byBox := map[string]*deviceRecord{}
	facts := map[string]deviceFact{}
	get := func(box string) *deviceRecord {
		box = strings.ToLower(strings.TrimSpace(box))
		if d := byBox[box]; d != nil {
			return d
		}
		d := &deviceRecord{Box: box, Seen: &deviceSeen{Decoders: map[string]*tally{}, Engines: map[string]*tally{},
			Apps: map[string]int{}, Displays: map[string]int{}, AudioFormats: map[string]int{}, AudioToTV: map[string]int{}}}
		byBox[box] = d
		out.Devices = append(out.Devices, d)
		return d
	}

	if data, err := os.ReadFile(filepath.Join(dir, "device-facts.json")); err == nil {
		var f struct {
			Time    string            `json:"time"`
			Devices []json.RawMessage `json:"devices"`
		}
		if json.Unmarshal(trimBOM(data), &f) == nil {
			out.Facts = f.Time
			for _, raw := range f.Devices {
				var df deviceFact
				if json.Unmarshal(raw, &df) != nil || df.Box == "" {
					continue
				}
				d := get(df.Box)
				d.Name, d.IP, d.Facts = df.Name, df.IP, raw
				facts[d.Box] = df
			}
		}
	}

	seenPlay := map[string]bool{}
	for _, raw := range readLines(filepath.Join(dir, "plays.jsonl")) {
		var p struct {
			Time      string   `json:"time"`
			App       string   `json:"app"`
			Box       string   `json:"box"`
			IP        string   `json:"ip"`
			Engine    string   `json:"engine"`
			Decoder   string   `json:"decoder"`
			Started   *bool    `json:"started"`
			StartS    *float64 `json:"start_s"`
			Rebuffers int      `json:"rebuffers"`
			Display   string   `json:"display"`
			PlayTime  string   `json:"play_time"`
		}
		if json.Unmarshal(raw, &p) != nil || p.Box == "" || p.Started == nil {
			continue
		}
		d := get(p.Box)
		if d.IP == "" {
			d.IP = p.IP
		}
		s := d.Seen
		if p.App != "" {
			s.Apps[p.App]++
		}
		// Other apps' snapshots are often no play at all (the TV asleep, the engine never opened).
		if p.App != "" && p.App != "com.stremio.one" {
			continue
		}
		// A second snapshot of the same play (same box, day and play_time) counts once.
		if p.PlayTime != "" {
			k := d.Box + " " + strings.SplitN(p.Time, "T", 2)[0] + " " + p.PlayTime
			if seenPlay[k] {
				continue
			}
			seenPlay[k] = true
		}
		s.Plays++
		count := func(m map[string]*tally, k string) {
			if k = strings.TrimSpace(k); k == "" || k == "never" || k == "none" || k == "unknown" {
				return
			}
			if m[k] == nil {
				m[k] = &tally{}
			}
			m[k].Plays++
			if *p.Started {
				m[k].Started++
			} else {
				m[k].Failed++
			}
		}
		count(s.Engines, p.Engine)
		count(s.Decoders, strings.ToLower(p.Decoder))
		if *p.Started {
			s.Started++
			if p.Rebuffers >= 3 {
				s.Stalled++
			}
			if p.StartS != nil {
				d.starts = append(d.starts, *p.StartS)
			}
			if p.Display != "" {
				s.Displays[p.Display]++
			}
		} else {
			s.Failed++
		}
		if at := parseTime(p.Time); at.After(s.LastPlay) {
			s.LastPlay = at
		}
	}

	for _, raw := range readLines(filepath.Join(dir, "audio.jsonl")) {
		var a struct {
			Box  string `json:"box"`
			HAL  string `json:"hal_format"`
			ToTV string `json:"to_tv"`
		}
		if json.Unmarshal(raw, &a) != nil || a.Box == "" {
			continue
		}
		d := get(a.Box)
		if a.HAL != "" {
			d.Seen.AudioFormats[a.HAL]++
		}
		if a.ToTV != "" {
			d.Seen.AudioToTV[a.ToTV]++
		}
	}

	for _, d := range out.Devices {
		d.Seen.StartMedian = median(d.starts)
		f := facts[d.Box]
		c := &deviceCanPlay{Video: nonNilStrings(f.VideoHW), HDR: nonNilStrings(f.TVHDR), Audio: union(f.AudioHW, f.AudioSW),
			AudioToTV: []string{}, Proven: []string{}, FailedOnly: []string{}}
		c.AudioOut = f.AudioOut
		pass := map[string]bool{}
		for hal := range d.Seen.AudioFormats {
			if f.AudioOut == "stereo PCM" {
				break
			}
			if n := halCodec[strings.ToUpper(hal)]; n != "" {
				pass[n] = true
			}
		}
		for n := range pass {
			c.AudioToTV = append(c.AudioToTV, n)
		}
		sort.Strings(c.AudioToTV)
		for k, t := range d.Seen.Decoders {
			if t.Started > 0 {
				c.Proven = append(c.Proven, k)
			} else {
				c.FailedOnly = append(c.FailedOnly, k)
			}
		}
		sort.Strings(c.Proven)
		sort.Strings(c.FailedOnly)
		d.CanPlay = c
	}
	sort.Slice(out.Devices, func(i, j int) bool { return out.Devices[i].Box < out.Devices[j].Box })
	return out
}

func trimBOM(b []byte) []byte { return []byte(strings.TrimPrefix(string(b), "\xef\xbb\xbf")) }

func nonNilStrings(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range [][]string{a, b} {
		for _, x := range l {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}
