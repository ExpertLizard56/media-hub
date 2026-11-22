package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeviceRecords(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("device-facts.json", "\xef\xbb\xbf"+`{"time":"2026-11-22T10:25:16","devices":[
 {"box":"bedroom","name":"Bedroom Onn box","ip":"192.168.1.242","video_hw":["av1","hevc"],"audio_hw":["ac3","eac3"],"audio_sw":["dts","truehd"],"tv_hdr":["hdr10","dolby-vision"]},
 {"box":"living","ip":"192.168.1.232","audio_out":"stereo PCM"},
 {"box":"firetv","name":"Fire TV","ip":"192.168.2.153","video_hw":["hevc"]}]}`)
	write("plays.jsonl", `{"time":"2026-11-21T10:00:00","box":"bedroom","engine":"ExoPlayer","decoder":"hevc","started":true,"start_s":3,"rebuffers":0,"display":"3840x2160@23.976"}
{"time":"2026-11-21T11:00:00","box":"bedroom","engine":"ExoPlayer","decoder":"HEVC","started":true,"start_s":5,"rebuffers":4,"display":"3840x2160@23.976"}
{"time":"2026-11-21T12:00:00","box":"bedroom","play_time":"11:58:00","engine":"Android player","decoder":null,"started":false}
{"time":"2026-11-21T12:05:00","box":"bedroom","play_time":"11:58:00","engine":"Android player","started":false}
{"time":"2026-11-09T16:39:57","box":"living","app":"app.music","engine":"ExoPlayer, NEVER","started":false}
{"time":"2026-11-21T13:00:00","box":"living","app":"com.stremio.one","ip":"192.168.1.232","engine":"ExoPlayer","decoder":"vc1","started":false}
`)
	write("audio.jsonl", `{"box":"living","hal_format":"DOLBY_DIGITAL_PLUS","to_tv":"Stereo_PCM"}
{"box":"bedroom","hal_format":"DOLBY_DIGITAL_PLUS","to_tv":"Stereo_PCM"}
{"box":"bedroom","hal_format":"PCM","to_tv":"Stereo_PCM"}
`)
	tr := newDeviceTrack(dir)
	tr.now = func() time.Time { return time.Date(2026, 11, 22, 0, 0, 0, 0, time.Local) }
	got := map[string]*deviceRecord{}
	for _, d := range tr.get().Devices {
		got[d.Box] = d
	}
	b := got["bedroom"]
	if b == nil || b.Seen.Plays != 3 || b.Seen.Started != 2 || b.Seen.Failed != 1 || b.Seen.Stalled != 1 || b.Seen.StartMedian != 4 {
		t.Fatalf("bedroom seen: %+v", b.Seen)
	}
	if b.Seen.Decoders["hevc"].Started != 2 || b.Seen.Engines["Android player"].Failed != 1 || b.Seen.Displays["3840x2160@23.976"] != 2 {
		t.Fatalf("bedroom tallies: %+v", b.Seen)
	}
	if c := b.CanPlay; len(c.Audio) != 4 || len(c.AudioToTV) != 1 || c.AudioToTV[0] != "eac3" || len(c.Proven) != 1 || len(c.HDR) != 2 {
		t.Fatalf("bedroom can_play: %+v", c)
	}
	l := got["living"]
	if l == nil || l.IP != "192.168.1.232" || len(l.CanPlay.FailedOnly) != 1 || l.Seen.Plays != 1 || l.Seen.Apps["app.music"] != 1 || l.CanPlay.AudioOut != "stereo PCM" || len(l.CanPlay.AudioToTV) != 0 {
		t.Fatalf("living (stereo PCM, music snapshot left out): %+v", l)
	}
	if f := got["firetv"]; f == nil || f.Seen.Plays != 0 || len(f.CanPlay.Video) != 1 {
		t.Fatalf("firetv (facts only): %+v", f)
	}
	if _, err := os.Stat(filepath.Join(dir, "devices.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceRecordsReal(t *testing.T) {
	dir := os.Getenv("FILES_DIR")
	if dir == "" {
		t.Skip("FILES_DIR not set")
	}
	for _, d := range buildDeviceRecords(dir, time.Now()).Devices {
		t.Logf("%s %s: plays %d started %d; decoders proven %v failed-only %v; video %v hdr %v audio %v to TV %v",
			d.Box, d.IP, d.Seen.Plays, d.Seen.Started, d.CanPlay.Proven, d.CanPlay.FailedOnly, d.CanPlay.Video, d.CanPlay.HDR, d.CanPlay.Audio, d.CanPlay.AudioToTV)
	}
}
