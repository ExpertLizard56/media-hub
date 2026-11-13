package main

// Watching the HDHomeRun, for the hub's log (eventlog.go):
//
//   - hdhomerun_device: model, firmware, tuner count, lineup scan state; at start, every few
//     hours, and whenever it changes (a firmware update, a new address).
//   - tuner_status: each tuner in use, every minute: channel, frequency, signal strength, signal
//     quality, symbol quality (100 = every packet received clean), the rate it's sending at.
//   - stream_errors: whenever packets were lost on a channel being watched, with the signal then.
//   - signal_survey: every hour nobody's watching, each station tuned for a few seconds with its
//     signal and losses, so reception can be followed over days (and as the antenna improves).
//
// GET /antenna/status.json shows the same as it is right now.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
)

const (
	// How often the tuners are asked for their signal while something's being watched...
	tunerPollEvery = 10 * time.Second
	// ...and how often that goes in the log (stream errors go in as they happen).
	tunerLogEvery = time.Minute
	// The device's details are logged at least this often.
	deviceLogEvery = 6 * time.Hour
	// A reception survey runs this often, when no tuner is in use.
	surveyEvery = time.Hour
	// Each station is watched this long in a survey.
	surveyListen = 6 * time.Second
)

// tunerStatus is one tuner in the HDHomeRun's status.json; only Resource is set for an idle one.
type tunerStatus struct {
	Resource              string
	VctNumber             string
	VctName               string
	Frequency             int64
	SignalStrengthPercent int
	SignalQualityPercent  int
	SymbolQualityPercent  int
	TargetIP              string
	NetworkRate           int64
}

func (t tunerStatus) inUse() bool { return t.VctNumber != "" }

func (a *antenna) tunerStatuses() ([]tunerStatus, error) {
	base, err := a.tuner()
	if err != nil {
		return nil, err
	}
	var tuners []tunerStatus
	err = getJSON(base+"/status.json", &tuners)
	return tuners, err
}

// signalStats sums a stream's signal readings, for its average and worst.
type signalStats struct {
	samples                         int
	strengthSum, qualitySum, symSum int
	strengthMin, qualityMin, symMin int
}

func (s *signalStats) add(t tunerStatus) {
	if s.samples == 0 || t.SignalStrengthPercent < s.strengthMin {
		s.strengthMin = t.SignalStrengthPercent
	}
	if s.samples == 0 || t.SignalQualityPercent < s.qualityMin {
		s.qualityMin = t.SignalQualityPercent
	}
	if s.samples == 0 || t.SymbolQualityPercent < s.symMin {
		s.symMin = t.SymbolQualityPercent
	}
	s.samples++
	s.strengthSum += t.SignalStrengthPercent
	s.qualitySum += t.SignalQualityPercent
	s.symSum += t.SymbolQualityPercent
}

// fields for a log line: averages and worsts, or nothing if the tuner was never read.
func (s signalStats) fields() []any {
	if s.samples == 0 {
		return nil
	}
	return []any{
		"strength_avg", s.strengthSum / s.samples, "strength_min", s.strengthMin,
		"quality_avg", s.qualitySum / s.samples, "quality_min", s.qualityMin,
		"symbol_avg", s.symSum / s.samples, "symbol_min", s.symMin,
	}
}

func signalFields(t *tunerStatus) []any {
	if t == nil {
		return nil
	}
	return []any{"tuner", t.Resource, "strength", t.SignalStrengthPercent, "quality", t.SignalQualityPercent, "symbol", t.SymbolQualityPercent}
}

// monitor runs for the hub's lifetime.
func (a *antenna) monitor() {
	a.logDevice()
	lastDevice := time.Now()
	lastTunerLog := time.Time{}
	lastSurvey := time.Now().Add(-surveyEvery + 2*time.Minute) // the first one a couple of minutes in
	for range time.Tick(tunerPollEvery) {
		if time.Since(lastDevice) > deviceLogEvery {
			a.logDevice()
			lastDevice = time.Now()
		}
		streams := a.activeStreams()
		if len(streams) == 0 {
			if time.Since(lastSurvey) > surveyEvery {
				a.survey()
				lastSurvey = time.Now()
			}
			continue
		}
		tuners, err := a.tunerStatuses()
		if err != nil {
			continue // the stream itself will say if the tuner's gone
		}
		logTuners := time.Since(lastTunerLog) >= tunerLogEvery
		if logTuners {
			lastTunerLog = time.Now()
		}
		for _, t := range tuners {
			if !t.inUse() {
				continue
			}
			for _, s := range streams {
				if s.number == t.VctNumber {
					s.noteSignal(t)
				}
			}
			if logTuners {
				hubLog.event("tuner_status", "tuner", t.Resource, "channel", t.VctNumber, "name", t.VctName,
					"frequency_mhz", t.Frequency/1_000_000, "strength", t.SignalStrengthPercent, "quality", t.SignalQualityPercent,
					"symbol", t.SymbolQualityPercent, "kbps", t.NetworkRate/1000, "to", t.TargetIP)
			}
		}
		for _, s := range streams {
			s.reportErrors()
		}
	}
}

func (a *antenna) activeStreams() []*channelStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	var active []*channelStream
	for _, s := range a.streams {
		if !s.isEnded() {
			active = append(active, s)
		}
	}
	return active
}

// deviceInfo is what the HDHomeRun says about itself, less DeviceAuth (a credential for
// SiliconDust's services).
type deviceInfo map[string]any

func (a *antenna) device() (deviceInfo, error) {
	base, err := a.tuner()
	if err != nil {
		return nil, err
	}
	info := deviceInfo{}
	if err := getJSON(base+"/discover.json", &info); err != nil {
		return nil, err
	}
	delete(info, "DeviceAuth")
	var scan map[string]any
	if getJSON(base+"/lineup_status.json", &scan) == nil {
		for k, v := range scan {
			info["Lineup"+k] = v
		}
	}
	return info, nil
}

func (a *antenna) logDevice() {
	info, err := a.device()
	if err != nil {
		hubLog.event("hdhomerun_unreachable", "error", err)
		return
	}
	a.mu.Lock()
	changed := !reflect.DeepEqual(info, a.lastDevice)
	a.lastDevice = info
	a.mu.Unlock()
	fields := []any{}
	keys := make([]string, 0, len(info))
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fields = append(fields, k, info[k])
	}
	hubLog.event("hdhomerun_device", append(fields, "changed", changed)...)
}

// survey tunes one channel of each station (its first subchannel: they share a frequency) for a
// few seconds, and logs its signal and what was lost meanwhile.
func (a *antenna) survey() {
	channels, err := a.channels()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, c := range channels {
		major := strings.SplitN(c.Number, ".", 2)[0]
		if seen[major] {
			continue
		}
		seen[major] = true
		if len(a.activeStreams()) > 0 {
			hubLog.event("signal_survey_stopped", "reason", "someone started watching")
			return
		}
		a.surveyChannel(c)
	}
}

func (a *antenna) surveyChannel(c lineupChannel) {
	base, err := a.tuner()
	if err != nil {
		return
	}
	u, _ := url.Parse(base)
	ctx, cancel := context.WithTimeout(context.Background(), surveyListen+5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+u.Hostname()+":5004/auto/v"+c.Number, nil)
	resp, err := streamClient.Do(req)
	if err != nil {
		hubLog.event("signal_survey", "channel", c.Number, "name", c.Name, "error", err)
		return
	}
	defer closeQuietly(resp.Body) // its content is read below; a close error has nothing to add
	if resp.StatusCode != http.StatusOK {
		hubLog.event("signal_survey", "channel", c.Number, "name", c.Name, "error", resp.Status)
		return
	}
	stats := newTSStats()
	var signal signalStats
	var last *tunerStatus
	var bytes int64
	start := time.Now()
	nextPoll := start.Add(2 * time.Second)
	buf := make([]byte, 64<<10)
	for time.Since(start) < surveyListen {
		n, err := resp.Body.Read(buf)
		bytes += int64(n)
		stats.add(buf[:n])
		if time.Now().After(nextPoll) {
			nextPoll = time.Now().Add(2 * time.Second)
			if tuners, err := a.tunerStatuses(); err == nil {
				for i := range tuners {
					if tuners[i].VctNumber == c.Number {
						signal.add(tuners[i])
						last = &tuners[i]
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	seconds := time.Since(start).Seconds()
	fields := []any{"channel", c.Number, "name", c.Name, "seconds", fmt.Sprintf("%.1f", seconds),
		"kbps", int64(float64(bytes*8/1000) / seconds), "packets", stats.packets, "lost", stats.lost,
		"corrupt", stats.corrupt, "sync_losses", stats.syncLosses}
	if last != nil {
		fields = append(fields, "frequency_mhz", last.Frequency/1_000_000)
	}
	hubLog.event("signal_survey", append(fields, signal.fields()...)...)
}

// serveStatus: the HDHomeRun and every stream, as they are now.
func (a *antenna) serveStatus(w http.ResponseWriter, _ *http.Request) {
	status := map[string]any{"hub_version": version}
	if info, err := a.device(); err == nil {
		status["hdhomerun"] = info
	} else {
		status["hdhomerun_error"] = err.Error()
	}
	if tuners, err := a.tunerStatuses(); err == nil {
		byTuner := map[string]any{}
		for _, t := range tuners {
			if t.inUse() {
				byTuner[t.Resource] = t
			} else {
				byTuner[t.Resource] = "idle"
			}
		}
		status["tuners"] = byTuner
	}
	var streams []map[string]any
	for _, s := range a.activeStreams() {
		streams = append(streams, s.describe())
	}
	status["streams"] = streams
	writeJSON(w, status)
}
