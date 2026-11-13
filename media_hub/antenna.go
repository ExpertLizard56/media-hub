package main

// Antenna channels, from the HDHomeRun on the home network:
//
//   - GET /antenna/lineup.json: the channels the tuner receives, with their broadcast numbers,
//     names and logos.
//   - GET /antenna/guide.json: the next day of listings by channel number, from SiliconDust's guide
//     service (free for HDHomeRun owners; it answers a few hours per request, so it's paged).
//   - GET /antenna/stream/{number}: the channel as MPEG-TS. Every box on the same channel shares
//     one tuner: the hub reads the tuner once and passes the stream to each of them.
//
// The HDHomeRun is found by broadcasting on the home network (discover.go), or set with the
// add-on's hdhomerun option. Nothing here is transcoded.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// The guide covers this far ahead.
	guideHours = 24
	// Lineup and guide answers are reused for this long.
	lineupTTL = 10 * time.Minute
	guideTTL  = 30 * time.Minute
	// A channel nobody's watching keeps its tuner this long, so flicking away and back doesn't
	// tune from scratch.
	lingerAfterLast = 5 * time.Second
	// What a box that joins a channel already playing gets first, so it starts at once.
	joinBacklog = 2 << 20
	// A box that falls this far behind is dropped rather than slowing the others, so one slow
	// device never holds up the rest. A box joining a channel that is already playing starts
	// joinBacklog behind. Counted in bytes, as chunks vary in size; about 25 s of an HD channel.
	viewerMaxBehind = 24 << 20
	// Room in a viewer's queue: chunks are shared, so it costs only their references.
	viewerQueue = 32 << 10
)

var httpClient = &http.Client{Timeout: 20 * time.Second, Transport: namedTransport{}}

// streamClient has no overall timeout: a stream runs as long as it's watched.
var streamClient = &http.Client{Transport: namedTransport{}}

// namedTransport sends the hub's own name: SiliconDust's services refuse Go's default one (403).
type namedTransport struct{}

func (namedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", "Media-Hub")
	return http.DefaultTransport.RoundTrip(r)
}

type antenna struct {
	configured string // the add-on's hdhomerun option: an address, or "" to discover

	mu       sync.Mutex
	base     string // the tuner's address, "http://<address>"
	lineup   []lineupChannel
	lineupAt time.Time
	guide    map[string][]listing
	guideAt  time.Time
	streams  map[string]*channelStream

	// What was last logged about the device and the lineup, to log changes (monitor.go).
	lastDevice deviceInfo
	lastLineup map[string]bool
}

type lineupChannel struct {
	Number    string `json:"number"`
	Name      string `json:"name"`
	Logo      string `json:"logo,omitempty"`
	Affiliate string `json:"affiliate,omitempty"`
	HD        bool   `json:"hd"`
	Stream    string `json:"stream"`
}

type listing struct {
	Title    string `json:"title"`
	Number   string `json:"episodeNumber,omitempty"`
	Episode  string `json:"episode,omitempty"`
	Synopsis string `json:"synopsis,omitempty"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
}

func newAntenna(configured string) *antenna {
	return &antenna{configured: strings.TrimSpace(configured), streams: map[string]*channelStream{}}
}

func (a *antenna) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /antenna/lineup.json", a.serveLineup)
	mux.HandleFunc("GET /antenna/guide.json", a.serveGuide)
	mux.HandleFunc("GET /antenna/stream/{number}", a.serveStream)
	mux.HandleFunc("GET /antenna/status.json", a.serveStatus)
}

// tuner is the HDHomeRun's address: the configured one, or the one that answers discovery.
func (a *antenna) tuner() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.base != "" {
		return a.base, nil
	}
	if a.configured != "" {
		a.base = "http://" + strings.TrimPrefix(a.configured, "http://")
		hubLog.event("hdhomerun_found", "address", a.base, "via", "option")
		return a.base, nil
	}
	started := time.Now()
	base, err := discoverTuner()
	if err != nil {
		hubLog.event("hdhomerun_not_found", "error", err, "ms", time.Since(started).Milliseconds())
		return "", fmt.Errorf("finding the HDHomeRun: %w", err)
	}
	a.base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), ":80")
	hubLog.event("hdhomerun_found", "address", a.base, "via", "discovery", "ms", time.Since(started).Milliseconds())
	return a.base, nil
}

func (a *antenna) serveLineup(w http.ResponseWriter, _ *http.Request) {
	channels, err := a.channels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, channels)
}

func (a *antenna) channels() ([]lineupChannel, error) {
	a.mu.Lock()
	if a.lineup != nil && time.Since(a.lineupAt) < lineupTTL {
		defer a.mu.Unlock()
		return a.lineup, nil
	}
	a.mu.Unlock()
	base, err := a.tuner()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	var raw []struct {
		GuideNumber    string
		GuideName      string
		VideoCodec     string
		AudioCodec     string
		HD             int
		DRM            int
		SignalStrength int // as of the last channel scan
		SignalQuality  int
	}
	if err := getJSON(base+"/lineup.json", &raw); err != nil {
		a.forgetTuner(err)
		hubLog.event("lineup_failed", "error", err)
		return nil, fmt.Errorf("reading the HDHomeRun's lineup: %w", err)
	}
	// Logos and network affiliations come with SiliconDust's guide; a channel it doesn't cover
	// just has none.
	logos := map[string][2]string{}
	if guide, err := a.fetchGuidePage(base, 0); err == nil {
		for _, c := range guide {
			logos[c.GuideNumber] = [2]string{c.ImageURL, c.Affiliate}
		}
	}
	var channels []lineupChannel
	numbers := map[string]bool{}
	var added, removed, encrypted []string
	for _, c := range raw {
		if c.DRM != 0 {
			encrypted = append(encrypted, c.GuideNumber)
			continue // encrypted: an HDHomeRun can't play it
		}
		numbers[c.GuideNumber] = true
		if a.lastLineup == nil || !a.lastLineup[c.GuideNumber] {
			added = append(added, c.GuideNumber)
			hubLog.event("lineup_channel", "channel", c.GuideNumber, "name", c.GuideName, "video", c.VideoCodec, "audio", c.AudioCodec,
				"hd", c.HD == 1, "scan_strength", c.SignalStrength, "scan_quality", c.SignalQuality,
				"guide", logos[c.GuideNumber][0] != "" || logos[c.GuideNumber][1] != "")
		}
		channels = append(channels, lineupChannel{
			Number:    c.GuideNumber,
			Name:      c.GuideName,
			Logo:      logos[c.GuideNumber][0],
			Affiliate: logos[c.GuideNumber][1],
			HD:        c.HD == 1,
			Stream:    "/antenna/stream/" + c.GuideNumber,
		})
	}
	for number := range a.lastLineup {
		if !numbers[number] {
			removed = append(removed, number)
		}
	}
	if len(added) > 0 || len(removed) > 0 {
		sort.Strings(removed)
		hubLog.event("lineup_loaded", "channels", len(channels), "added", strings.Join(added, ","), "removed", strings.Join(removed, ","),
			"encrypted", strings.Join(encrypted, ","), "ms", time.Since(started).Milliseconds())
	}
	a.mu.Lock()
	a.lineup, a.lineupAt, a.lastLineup = channels, time.Now(), numbers
	a.mu.Unlock()
	return channels, nil
}

type guideChannel struct {
	GuideNumber string
	Affiliate   string
	ImageURL    string
	Guide       []struct {
		StartTime     int64
		EndTime       int64
		Title         string
		EpisodeTitle  string
		EpisodeNumber string
		Synopsis      string
	}
}

// fetchGuidePage asks SiliconDust for listings from start (0 = now). It needs the tuner's current
// DeviceAuth, which changes, so that's read from the tuner each time.
func (a *antenna) fetchGuidePage(base string, start int64) ([]guideChannel, error) {
	var device struct{ DeviceAuth string }
	if err := getJSON(base+"/discover.json", &device); err != nil {
		return nil, err
	}
	address := "https://api.hdhomerun.com/api/guide.php?DeviceAuth=" + url.QueryEscape(device.DeviceAuth)
	if start > 0 {
		address += fmt.Sprintf("&Start=%d", start)
	}
	var page []guideChannel
	err := getJSON(address, &page)
	return page, err
}

func (a *antenna) serveGuide(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	if a.guide != nil && time.Since(a.guideAt) < guideTTL {
		guide := a.guide
		a.mu.Unlock()
		writeJSON(w, guide)
		return
	}
	a.mu.Unlock()
	base, err := a.tuner()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Page through until a day ahead is covered (each page is a few hours).
	started := time.Now()
	pages := 0
	guide := map[string][]listing{}
	until := time.Now().Add(guideHours * time.Hour).Unix()
	var start int64
	for page := 0; page < 12; page++ {
		channels, err := a.fetchGuidePage(base, start)
		if err != nil {
			hubLog.event("guide_failed", "page", page, "error", err)
			if page == 0 {
				http.Error(w, "guide: "+err.Error(), http.StatusBadGateway)
				return
			}
			break
		}
		pages++
		latest := int64(0)
		for _, c := range channels {
			seen := map[int64]bool{}
			for _, l := range guide[c.GuideNumber] {
				seen[l.Start] = true
			}
			for _, p := range c.Guide {
				if seen[p.StartTime] {
					continue
				}
				guide[c.GuideNumber] = append(guide[c.GuideNumber], listing{
					Title: p.Title, Number: p.EpisodeNumber, Episode: p.EpisodeTitle, Synopsis: p.Synopsis, Start: p.StartTime, End: p.EndTime,
				})
				if p.EndTime > latest {
					latest = p.EndTime
				}
			}
		}
		if latest <= start || latest >= until {
			break
		}
		start = latest
	}
	listings, latest := 0, int64(0)
	for number := range guide {
		sort.Slice(guide[number], func(i, j int) bool { return guide[number][i].Start < guide[number][j].Start })
		listings += len(guide[number])
		if end := guide[number][len(guide[number])-1].End; end > latest {
			latest = end
		}
	}
	hubLog.event("guide_loaded", "pages", pages, "channels", len(guide), "listings", listings,
		"hours_ahead", fmt.Sprintf("%.1f", time.Until(time.Unix(latest, 0)).Hours()), "ms", time.Since(started).Milliseconds())
	a.mu.Lock()
	a.guide, a.guideAt = guide, time.Now()
	a.mu.Unlock()
	writeJSON(w, guide)
}

// The address may change (DHCP): after a failure, find it again next time.
func (a *antenna) forgetTuner(cause error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hubLog.event("hdhomerun_lost", "address", a.base, "error", cause)
	if a.configured == "" {
		a.base = ""
	}
}

// channelStream is one tuner's worth of one channel, passed to every box watching it.
type channelStream struct {
	number  string
	cancel  context.CancelFunc
	mu      sync.Mutex
	viewers map[chan []byte]*viewer // each viewer's feed, and who it is
	backlog [][]byte                // the last joinBacklog bytes, for boxes that join
	size    int
	linger  *time.Timer
	ended   bool

	// For the log: how it's going, and how it went when it ends.
	started     time.Time
	bytes       int64
	ts          *tsStats
	reported    tsCounts // what reportErrors last covered
	signal      signalStats
	lastSignal  *tunerStatus
	peakViewers int
	dropped     int
}

func (a *antenna) serveStream(w http.ResponseWriter, r *http.Request) {
	number := r.PathValue("number")
	who := boxLabel(r)
	feed, v, backlog, err := a.join(number, who)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNoTuner) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	since := time.Now()
	var sent int64
	reason := "box stopped"
	defer func() { a.leave(number, feed, who, since, sent, reason) }()
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	for _, chunk := range backlog {
		if _, err := w.Write(chunk); err != nil {
			return
		}
		sent += int64(len(chunk))
	}
	for {
		select {
		case chunk, open := <-feed:
			if !open {
				reason = "dropped or tuner ended"
				return
			}
			v.behind.Add(-int64(len(chunk)))
			if _, err := w.Write(chunk); err != nil {
				return
			}
			sent += int64(len(chunk))
			if flusher != nil {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

var errNoTuner = errors.New("all the tuners are in use")

// viewer is one box watching a channel.
type viewer struct {
	who    string       // boxLabel
	behind atomic.Int64 // bytes queued for it and not yet sent
}

// join adds a viewer to the channel, tuning it if nobody's watching yet.
func (a *antenna) join(number, who string) (chan []byte, *viewer, [][]byte, error) {
	a.mu.Lock()
	s := a.streams[number]
	if s == nil || s.isEnded() {
		a.mu.Unlock()
		started, err := a.tune(number)
		if err != nil {
			hubLog.event("tune_failed", "channel", number, "box", who, "busy", errors.Is(err, errNoTuner), "error", err)
			return nil, nil, nil, err
		}
		a.mu.Lock()
		if existing := a.streams[number]; existing != nil && !existing.isEnded() {
			started.stop() // someone else tuned it meanwhile
			s = existing
		} else {
			a.streams[number] = started
			s = started
		}
	}
	a.mu.Unlock()

	feed := make(chan []byte, viewerQueue)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.linger != nil {
		s.linger.Stop()
		s.linger = nil
	}
	v := &viewer{who: who}
	s.viewers[feed] = v
	s.peakViewers = max(s.peakViewers, len(s.viewers))
	backlog := append([][]byte(nil), s.backlog...)
	hubLog.event("viewer_joined", "channel", number, "box", who, "viewers", len(s.viewers), "backlog_kb", s.size/1024,
		"tuned_s", int(time.Since(s.started).Seconds()))
	return feed, v, backlog, nil
}

func (a *antenna) leave(number string, feed chan []byte, who string, since time.Time, sent int64, reason string) {
	a.mu.Lock()
	s := a.streams[number]
	a.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.viewers, feed)
	hubLog.event("viewer_left", "channel", number, "box", who, "watched_s", int(time.Since(since).Seconds()),
		"mb", fmt.Sprintf("%.1f", float64(sent)/1e6), "viewers", len(s.viewers), "reason", reason)
	if len(s.viewers) == 0 && !s.ended && s.linger == nil {
		s.linger = time.AfterFunc(lingerAfterLast, func() {
			s.mu.Lock()
			idle := len(s.viewers) == 0
			s.mu.Unlock()
			if idle {
				s.stop()
			}
		})
	}
}

// tune opens the channel on the HDHomeRun and starts passing it on.
func (a *antenna) tune(number string) (*channelStream, error) {
	base, err := a.tuner()
	if err != nil {
		return nil, err
	}
	// The tuner streams on port 5004; "auto" picks any free tuner.
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("tuner address %q: %w", base, err)
	}
	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.Hostname()+":5004/auto/v"+number, nil)
	resp, err := streamClient.Do(req)
	if err != nil {
		cancel()
		a.forgetTuner(err)
		return nil, fmt.Errorf("tuning: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		closeQuietly(resp.Body) // nothing reads the body: the status is what matters
		cancel()
		if resp.StatusCode == http.StatusServiceUnavailable {
			return nil, errNoTuner
		}
		return nil, fmt.Errorf("tuning: HDHomeRun answered %s", resp.Status)
	}
	s := &channelStream{number: number, cancel: cancel, viewers: map[chan []byte]*viewer{}, started: time.Now(), ts: newTSStats()}
	hubLog.event("tuned", "channel", number, "ms", time.Since(started).Milliseconds())
	go s.pump(resp.Body)
	return s, nil
}

// pump reads the tuner and hands each chunk to every viewer; a viewer too far behind is dropped.
func (s *channelStream) pump(body io.ReadCloser) {
	defer closeQuietly(body) // what ended the stream is logged below
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			s.bytes += int64(n)
			s.ts.add(chunk)
			s.backlog = append(s.backlog, chunk)
			s.size += n
			for s.size > joinBacklog && len(s.backlog) > 1 {
				s.size -= len(s.backlog[0])
				s.backlog = s.backlog[1:]
			}
			for feed, v := range s.viewers {
				queued := false
				if v.behind.Load() < viewerMaxBehind {
					select {
					case feed <- chunk:
						v.behind.Add(int64(n))
						queued = true
					default:
					}
				}
				if !queued {
					delete(s.viewers, feed)
					close(feed)
					s.dropped++
					hubLog.event("viewer_dropped", "channel", s.number, "box", v.who, "reason", "fell behind",
						"behind_kb", v.behind.Load()/1024)
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			s.mu.Lock()
			s.ended = true
			for feed := range s.viewers {
				delete(s.viewers, feed)
				close(feed)
			}
			s.mu.Unlock()
			reason := "nobody watching"
			if !errors.Is(err, context.Canceled) {
				reason = "tuner stream ended: " + err.Error()
			}
			s.logEnd(reason)
			return
		}
	}
}

// logEnd sums up the channel's time on the tuner.
func (s *channelStream) logEnd(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seconds := max(time.Since(s.started).Seconds(), 1)
	var byPID []string
	for pid, n := range s.ts.lostByPID {
		byPID = append(byPID, fmt.Sprintf("%d:%d", pid, n))
	}
	sort.Strings(byPID)
	fields := []any{"channel", s.number, "reason", reason, "seconds", int(seconds),
		"mb", fmt.Sprintf("%.1f", float64(s.bytes)/1e6), "kbps", int64(float64(s.bytes*8/1000) / seconds),
		"packets", s.ts.packets, "lost", s.ts.lost, "corrupt", s.ts.corrupt, "sync_losses", s.ts.syncLosses,
		"lost_by_pid", strings.Join(byPID, ","), "peak_viewers", s.peakViewers, "dropped_viewers", s.dropped}
	if s.lastSignal != nil {
		fields = append(fields, "tuner", s.lastSignal.Resource, "frequency_mhz", s.lastSignal.Frequency/1_000_000)
	}
	hubLog.event("tuner_released", append(fields, s.signal.fields()...)...)
}

// noteSignal records a reading of this channel's tuner (monitor.go).
func (s *channelStream) noteSignal(t tunerStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signal.add(t)
	s.lastSignal = &t
}

// reportErrors logs packets lost since the last report, with the signal as it was then.
func (s *channelStream) reportErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.ts.counts()
	delta := now.minus(s.reported)
	s.reported = now
	if !delta.troubled() {
		return
	}
	fields := []any{"channel", s.number, "packets", delta.packets, "lost", delta.lost, "corrupt", delta.corrupt,
		"sync_losses", delta.syncLosses, "viewers", len(s.viewers)}
	hubLog.event("stream_errors", append(fields, signalFields(s.lastSignal)...)...)
}

// describe is the stream as it is now, for status.json.
func (s *channelStream) describe() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var boxes []string
	for _, v := range s.viewers {
		boxes = append(boxes, v.who)
	}
	sort.Strings(boxes)
	return map[string]any{
		"channel": s.number, "viewers": boxes, "since": s.started.Format(time.RFC3339), "mb": s.bytes / 1_000_000,
		"packets": s.ts.packets, "lost": s.ts.lost, "corrupt": s.ts.corrupt, "sync_losses": s.ts.syncLosses,
		"signal": s.lastSignal,
	}
}

func (s *channelStream) stop() { s.cancel() }

func (s *channelStream) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

func getJSON(address string, into any) error {
	resp, err := httpClient.Get(address)
	if err != nil {
		return err
	}
	defer closeQuietly(resp.Body) // its content is read below; a close error has nothing to add
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", address, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}
