package main

import (
	"bytes"
	"net/http"
	"encoding/binary"
	"testing"
)

func TestNamesFromPath(t *testing.T) {
	cases := []struct {
		rel                   string
		title, artist, album  string
		track                 int
	}{
		// Files in one flat folder, titles only.
		{"21 Guns.wav", "21 Guns", "", "", 0},
		{"100 Ways to Hate.wav", "100 Ways to Hate", "", "", 0},
		{"B.Y.O.B..wav", "B.Y.O.B.", "", "", 0},
		{"Dead & Bloated.mp3", "Dead & Bloated", "", "", 0},
		{"Music/Chop Suey.wav", "Chop Suey", "", "Music", 0},
		// The usual layouts.
		{"Green Day/American Idiot/03 - Holiday.flac", "Holiday", "Green Day", "American Idiot", 3},
		{"Green Day/American Idiot (2004)/03. Holiday.flac", "Holiday", "Green Day", "American Idiot", 3},
		{"Green Day/American Idiot/CD1/03 Holiday.flac", "Holiday", "Green Day", "American Idiot", 3},
		{"Green Day - American Idiot [2004]/1-03 Holiday.mp3", "Holiday", "Green Day", "American Idiot", 3},
		{"Green Day - Holiday.mp3", "Holiday", "Green Day", "", 0},
		{"Linkin Park/Linkin Park - Meteora/07 - Numb.mp3", "Numb", "Linkin Park", "Meteora", 7},
	}
	for _, c := range cases {
		got := namesFromPath(c.rel)
		if got.Title != c.title || got.Artist != c.artist || got.Album != c.album || got.Track != c.track {
			t.Errorf("%s: got title=%q artist=%q album=%q track=%d, want %q %q %q %d",
				c.rel, got.Title, got.Artist, got.Album, got.Track, c.title, c.artist, c.album, c.track)
		}
	}
}

func id3Frame(id string, body []byte) []byte {
	var b bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	b.WriteString(id)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(body)))
	b.Write([]byte{0, 0})
	b.Write(body)
	return b.Bytes()
}

func id3Tag(frames ...[]byte) []byte {
	body := bytes.Join(frames, nil)
	size := len(body)
	head := []byte{'I', 'D', '3', 3, 0, 0, byte(size >> 21 & 0x7F), byte(size >> 14 & 0x7F), byte(size >> 7 & 0x7F), byte(size & 0x7F)}
	return append(head, body...)
}

func TestReadMP3(t *testing.T) {
	utf16Title := []byte{1, 0xFF, 0xFE, 'H', 0, 'o', 0, 'l', 0, 'i', 0, 'd', 0, 'a', 0, 'y', 0}
	picture := append([]byte{0}, []byte("image/png\x00\x03cover\x00PNGDATA")...)
	tag := id3Tag(
		id3Frame("TIT2", utf16Title),
		id3Frame("TPE1", append([]byte{0}, "Green Day"...)),
		id3Frame("TALB", append([]byte{3}, "American Idiot"...)),
		id3Frame("TRCK", append([]byte{0}, "3/13"...)),
		id3Frame("TYER", append([]byte{0}, "2004"...)),
		id3Frame("APIC", picture),
	)
	// One MPEG-1 Layer III frame header at 128 kbps, 44.1 kHz, then "audio".
	audio := append([]byte{0xFF, 0xFB, 0x90, 0x00}, make([]byte, 16000-4)...)
	file := append(tag, audio...)
	got, err := readTags(bytes.NewReader(file), int64(len(file)), "mp3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Holiday" || got.Artist != "Green Day" || got.Album != "American Idiot" || got.Track != 3 || got.Year != 2004 {
		t.Errorf("tags: %+v", got)
	}
	if got.DurationMs != 1000 { // 16000 bytes at 128 kbps
		t.Errorf("duration %d ms, want 1000", got.DurationMs)
	}
	if got.ArtMime != "image/png" || string(file[got.ArtOffset:got.ArtOffset+got.ArtLength]) != "PNGDATA" {
		t.Errorf("art: %q at %d+%d", got.ArtMime, got.ArtOffset, got.ArtLength)
	}
}

func riffChunk(id string, body []byte) []byte {
	var b bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	b.WriteString(id)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
	b.Write(body)
	if len(body)%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestReadWAV(t *testing.T) {
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:], 0x11) // IMA ADPCM, as some WAVs are
	binary.LittleEndian.PutUint16(fmtBody[2:], 2)
	binary.LittleEndian.PutUint32(fmtBody[4:], 44100)
	binary.LittleEndian.PutUint32(fmtBody[8:], 44359) // bytes per second
	info := append([]byte("INFO"), riffChunk("INAM", []byte("Aerials\x00"))...)
	info = append(info, riffChunk("IART", []byte("System of a Down\x00"))...)
	info = append(info, riffChunk("IPRD", []byte("Toxicity\x00"))...)
	body := append([]byte("WAVE"), riffChunk("fmt ", fmtBody)...)
	body = append(body, riffChunk("fact", make([]byte, 4))...)
	body = append(body, riffChunk("data", make([]byte, 44359*3))...)
	body = append(body, riffChunk("LIST", info)...)
	file := riffChunk("RIFF", body)
	got, err := readTags(bytes.NewReader(file), int64(len(file)), "wav")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Aerials" || got.Artist != "System of a Down" || got.Album != "Toxicity" || got.DurationMs != 3000 {
		t.Errorf("tags: %+v", got)
	}
}

func TestReadWAVWithoutTags(t *testing.T) {
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint32(fmtBody[8:], 1000)
	body := append([]byte("WAVE"), riffChunk("fmt ", fmtBody)...)
	body = append(body, riffChunk("data", make([]byte, 2500))...)
	file := riffChunk("RIFF", body)
	got, err := readTags(bytes.NewReader(file), int64(len(file)), "wav")
	if err != nil || got.Title != "" || got.DurationMs != 2500 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestReadFLAC(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("fLaC")
	// STREAMINFO: 44100 Hz, 441000 samples (10 s).
	info := make([]byte, 34)
	rate := uint32(44100)
	info[10], info[11], info[12] = byte(rate>>12), byte(rate>>4), byte(rate<<4)
	binary.BigEndian.PutUint32(info[14:18], 441000)
	b.Write([]byte{0, 0, 0, 34})
	b.Write(info)
	// VORBIS_COMMENT, the last block.
	var vc bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	_ = binary.Write(&vc, binary.LittleEndian, uint32(3))
	vc.WriteString("enc")
	comments := []string{"TITLE=Numb", "ARTIST=Linkin Park", "ALBUM=Meteora", "TRACKNUMBER=13", "DATE=2003-03-25"}
	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(c)))
		vc.WriteString(c)
	}
	n := vc.Len()
	b.Write([]byte{0x80 | 4, byte(n >> 16), byte(n >> 8), byte(n)})
	b.Write(vc.Bytes())
	file := b.Bytes()
	got, err := readTags(bytes.NewReader(file), int64(len(file)), "flac")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Numb" || got.Artist != "Linkin Park" || got.Album != "Meteora" || got.Track != 13 || got.Year != 2003 || got.DurationMs != 10000 {
		t.Errorf("tags: %+v", got)
	}
}

func TestNeedsMobile(t *testing.T) {
	cases := []struct {
		name       string
		bytes, ms  int64
		want       bool
	}{
		{"ADPCM WAV, ~90 kbps", 3_568_188, 321_452, false},
		{"plain 16-bit WAV, 1411 kbps", 42_336_000, 240_000, true},
		{"FLAC, ~900 kbps", 27_000_000, 240_000, true},
		{"320 kbps MP3", 9_600_000, 240_000, false},
		{"no length known", 9_600_000, 0, false},
	}
	for _, c := range cases {
		got := needsMobile(&musicTrack{ID: c.name, Bytes: c.bytes, DurationMs: c.ms})
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFromHome(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.20:51000":                     true,
		"[fe80::4c9d:ccff:fe25:ad9c%end0]:44912": true, // link-local IPv6, with its interface
		"[fe80::1]:80":                           true,
		"172.27.66.2:4000":                       true, // WireGuard
		"127.0.0.1:1":                            true,
		"203.0.113.7:5000":                       false,
		"[2001:db8::1]:443":                      false,
	}
	for addr, want := range cases {
		if got := fromHome(&http.Request{RemoteAddr: addr}); got != want {
			t.Errorf("%s: got %v, want %v", addr, got, want)
		}
	}
}

func TestMeasureWave(t *testing.T) {
	// 2 seconds at 8 kHz: silent first half, a steady tone at half volume, a loud click at the end.
	var pcm bytes.Buffer // binary.Write into a bytes.Buffer can't fail
	for n := 0; n < 16000; n++ {
		v := int16(0)
		if n >= 8000 {
			v = 16000
		}
		if n == 15999 {
			v = -32000
		}
		_ = binary.Write(&pcm, binary.LittleEndian, v)
	}
	peaks, err := measure(&pcm, 2000)
	if err != nil {
		t.Fatal(err)
	}
	bars := scaleBars(peaks)
	if len(bars) != waveBars || bars[0] != 0 || bars[waveBars/4] != 0 {
		t.Errorf("silence should be 0: %v", bars[:4])
	}
	if bars[waveBars-1] != 255 {
		t.Errorf("the loudest slice should be 255, got %d", bars[waveBars-1])
	}
	if b := bars[waveBars*3/4]; b < 240 { // the same steady tone as the last slice, bar the click
		t.Errorf("a steady tone should be near the top, got %d", b)
	}
	if _, err := measure(bytes.NewReader(nil), 0); err == nil {
		t.Error("no length should be an error")
	}
}
