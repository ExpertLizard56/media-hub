package main

// Reading what a music file says about itself: title, artist, album, track number, length, and
// where its cover picture is. MP3 (ID3v2 tags), WAV (RIFF LIST/INFO or an embedded ID3 chunk) and
// FLAC (Vorbis comments). Anything a file doesn't say is left empty for the library to fill in
// from the folder and file names (music.go). Only the start of a file is read, never the audio.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf16"
)

// tags is what a file says about itself. Zero values mean "not said".
type tags struct {
	Title, Artist, AlbumArtist, Album string
	Track, Disc, Year                 int
	DurationMs                        int64
	// The embedded cover picture, if any: where it is in the file, and its type.
	ArtOffset, ArtLength int64
	ArtMime              string
}

// headLimit is how much of a file is read for tags: covers can be large, audio never is read.
const headLimit = 16 << 20

func readTags(r io.ReaderAt, size int64, format string) (tags, error) {
	switch format {
	case "mp3":
		return readMP3(r, size)
	case "wav":
		return readWAV(r, size)
	case "flac":
		return readFLAC(r, size)
	}
	return tags{}, nil
}

// ---- MP3 ----

func readMP3(r io.ReaderAt, size int64) (tags, error) {
	var t tags
	audioStart := int64(0)
	head := make([]byte, 10)
	if _, err := r.ReadAt(head, 0); err != nil {
		return t, err
	}
	if string(head[:3]) == "ID3" {
		tagSize := int64(syncsafe(head[6:10])) + 10
		if err := readID3(io.NewSectionReader(r, 0, min(tagSize, headLimit)), 0, &t); err != nil {
			return t, err
		}
		audioStart = tagSize
	}
	if t.DurationMs == 0 {
		t.DurationMs = mp3Duration(r, audioStart, size)
	}
	return t, nil
}

// readID3 reads an ID3v2 tag at the start of r; base is where r starts in the file (for the cover).
func readID3(r io.ReaderAt, base int64, t *tags) error {
	head := make([]byte, 10)
	if _, err := r.ReadAt(head, 0); err != nil {
		return err
	}
	if string(head[:3]) != "ID3" {
		return errors.New("not an ID3 tag")
	}
	major := head[3]
	flags := head[5]
	end := int64(syncsafe(head[6:10])) + 10
	pos := int64(10)
	if flags&0x40 != 0 { // extended header: skip it
		ext := make([]byte, 4)
		if _, err := r.ReadAt(ext, pos); err != nil {
			return err
		}
		if major == 4 {
			pos += int64(syncsafe(ext))
		} else {
			pos += int64(binary.BigEndian.Uint32(ext)) + 4
		}
	}
	frame := make([]byte, 10)
	idLen := 4
	if major == 2 {
		idLen = 3
	}
	headLen := int64(idLen * 2)
	if major != 2 {
		headLen = 10
	}
	for pos+headLen <= end {
		if _, err := r.ReadAt(frame[:headLen], pos); err != nil {
			return nil // a tag cut short still gives what it had
		}
		id := string(frame[:idLen])
		if frame[0] == 0 {
			break // padding
		}
		var length int64
		switch major {
		case 2:
			length = int64(frame[3])<<16 | int64(frame[4])<<8 | int64(frame[5])
		case 4:
			length = int64(syncsafe(frame[4:8]))
		default:
			length = int64(binary.BigEndian.Uint32(frame[4:8]))
		}
		body := pos + headLen
		if length <= 0 || body+length > end {
			break
		}
		if major == 2 {
			id = map[string]string{"TT2": "TIT2", "TP1": "TPE1", "TP2": "TPE2", "TAL": "TALB", "TRK": "TRCK", "TPA": "TPOS", "TYE": "TYER", "PIC": "PIC"}[id]
		}
		switch id {
		case "TIT2", "TPE1", "TPE2", "TALB", "TRCK", "TPOS", "TYER", "TDRC":
			data := make([]byte, min(length, 4096))
			if _, err := r.ReadAt(data, body); err == nil {
				setID3Text(t, id, id3Text(data))
			}
		case "APIC", "PIC":
			if t.ArtLength == 0 {
				readID3Picture(r, base, body, length, major == 2, t)
			}
		}
		pos = body + length
	}
	return nil
}

func setID3Text(t *tags, id, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	switch id {
	case "TIT2":
		t.Title = value
	case "TPE1":
		t.Artist = value
	case "TPE2":
		t.AlbumArtist = value
	case "TALB":
		t.Album = value
	case "TRCK":
		t.Track = leadingNumber(value)
	case "TPOS":
		t.Disc = leadingNumber(value)
	case "TYER", "TDRC":
		t.Year = leadingNumber(value)
	}
}

// id3Text decodes a text frame: its first byte names the encoding. Several values (v2.4 separates
// them with a zero) are joined with " / ".
func id3Text(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	enc, body := data[0], data[1:]
	var parts []string
	switch enc {
	case 1, 2: // UTF-16 with a byte-order mark, or big-endian without
		bigEndian := enc == 2
		if len(body) >= 2 && body[0] == 0xFE && body[1] == 0xFF {
			bigEndian, body = true, body[2:]
		} else if len(body) >= 2 && body[0] == 0xFF && body[1] == 0xFE {
			bigEndian, body = false, body[2:]
		}
		units := make([]uint16, 0, len(body)/2)
		for i := 0; i+1 < len(body); i += 2 {
			if bigEndian {
				units = append(units, binary.BigEndian.Uint16(body[i:]))
			} else {
				units = append(units, binary.LittleEndian.Uint16(body[i:]))
			}
		}
		for _, part := range strings.Split(string(utf16.Decode(units)), "\x00") {
			part = strings.TrimPrefix(part, "\ufeff")
			if part != "" {
				parts = append(parts, part)
			}
		}
	case 3:
		for _, part := range strings.Split(string(body), "\x00") {
			if part != "" {
				parts = append(parts, part)
			}
		}
	default:
		for _, part := range strings.Split(latin1(body), "\x00") {
			if part != "" {
				parts = append(parts, part)
			}
		}
	}
	return strings.Join(parts, " / ")
}

// readID3Picture notes where an attached picture's image is, preferring nothing in particular:
// the first one wins (usually the front cover).
func readID3Picture(r io.ReaderAt, base, body, length int64, v22 bool, t *tags) {
	data := make([]byte, min(length, 512))
	if _, err := r.ReadAt(data, body); err != nil || len(data) < 4 {
		return
	}
	enc := data[0]
	i := 1
	mime := ""
	if v22 { // a three-letter format instead of a MIME type
		if len(data) < 5 {
			return
		}
		mime = "image/" + strings.ToLower(strings.TrimSpace(string(data[1:4])))
		if mime == "image/jpg" {
			mime = "image/jpeg"
		}
		i = 4
	} else {
		end := bytes.IndexByte(data[i:], 0)
		if end < 0 {
			return
		}
		mime = strings.ToLower(string(data[i : i+end]))
		i += end + 1
	}
	i++ // picture type
	// The description, ended by a zero (two zeros, aligned, in UTF-16).
	if enc == 1 || enc == 2 {
		for i+1 < len(data) && !(data[i] == 0 && data[i+1] == 0) {
			i += 2
		}
		i += 2
	} else {
		end := bytes.IndexByte(data[min(i, len(data)):], 0)
		if end < 0 {
			return
		}
		i += end + 1
	}
	if int64(i) >= length {
		return
	}
	if mime == "" || !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	t.ArtOffset, t.ArtLength, t.ArtMime = base+body+int64(i), length-int64(i), mime
}

// mp3Duration: from a Xing/Info header (variable bitrate) if the first frame has one, otherwise
// from the first frame's bitrate and the file's size (constant bitrate).
func mp3Duration(r io.ReaderAt, start, size int64) int64 {
	buf := make([]byte, 4096)
	n, _ := r.ReadAt(buf, start)
	buf = buf[:n]
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] != 0xFF || buf[i+1]&0xE0 != 0xE0 {
			continue
		}
		version := (buf[i+1] >> 3) & 3 // 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5
		layer := (buf[i+1] >> 1) & 3   // 1 = Layer III
		bitrateIndex := buf[i+2] >> 4
		rateIndex := (buf[i+2] >> 2) & 3
		if version == 1 || layer != 1 || bitrateIndex == 0 || bitrateIndex == 15 || rateIndex == 3 {
			continue
		}
		mpeg1 := version == 3
		bitrates := [2][15]int{
			{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},
			{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},
		}
		rates := [3]int{44100, 48000, 32000}
		sampleRate := rates[rateIndex]
		samplesPerFrame := 1152
		table := 1
		if !mpeg1 {
			table = 0
			samplesPerFrame = 576
			if version == 2 {
				sampleRate /= 2
			} else {
				sampleRate /= 4
			}
		}
		// Xing/Info: the frame count, after the side information.
		sideInfo := 32
		mono := buf[i+3]>>6 == 3
		switch {
		case mpeg1 && mono:
			sideInfo = 17
		case !mpeg1 && mono:
			sideInfo = 9
		case !mpeg1:
			sideInfo = 17
		}
		x := i + 4 + sideInfo
		if x+12 <= len(buf) && (string(buf[x:x+4]) == "Xing" || string(buf[x:x+4]) == "Info") && buf[x+7]&1 != 0 {
			frames := int64(binary.BigEndian.Uint32(buf[x+8:]))
			return frames * int64(samplesPerFrame) * 1000 / int64(sampleRate)
		}
		kbps := bitrates[table][bitrateIndex]
		return (size - start - int64(i)) * 8 / int64(kbps)
	}
	return 0
}

// ---- WAV ----

func readWAV(r io.ReaderAt, size int64) (tags, error) {
	var t tags
	head := make([]byte, 12)
	if _, err := r.ReadAt(head, 0); err != nil {
		return t, err
	}
	if string(head[:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return t, errors.New("not a WAV file")
	}
	var byteRate, dataSize int64
	pos := int64(12)
	chunk := make([]byte, 8)
	for pos+8 <= size {
		if _, err := r.ReadAt(chunk, pos); err != nil {
			break
		}
		id := string(chunk[:4])
		length := int64(binary.LittleEndian.Uint32(chunk[4:]))
		body := pos + 8
		switch id {
		case "fmt ":
			f := make([]byte, 16)
			if _, err := r.ReadAt(f, body); err == nil {
				byteRate = int64(binary.LittleEndian.Uint32(f[8:12]))
			}
		case "data":
			dataSize = min(length, size-body)
		case "LIST":
			if length <= headLimit {
				list := make([]byte, length)
				if _, err := r.ReadAt(list, body); err == nil {
					readRIFFInfo(list, &t)
				}
			}
		case "id3 ", "ID3 ":
			// A tag that can't be read leaves what the other chunks said as it is.
			_ = readID3(io.NewSectionReader(r, body, min(length, headLimit)), body, &t)
		}
		pos = body + length + length&1
	}
	if byteRate > 0 {
		t.DurationMs = dataSize * 1000 / byteRate
	}
	return t, nil
}

// readRIFFInfo reads a LIST chunk of type INFO: INAM title, IART artist, IPRD album, ITRK track.
func readRIFFInfo(list []byte, t *tags) {
	if len(list) < 4 || string(list[:4]) != "INFO" {
		return
	}
	for i := 4; i+8 <= len(list); {
		id := string(list[i : i+4])
		length := int(binary.LittleEndian.Uint32(list[i+4:]))
		if i+8+length > len(list) {
			return
		}
		value := strings.TrimSpace(strings.TrimRight(string(list[i+8:i+8+length]), "\x00"))
		if value != "" {
			switch id {
			case "INAM":
				t.Title = value
			case "IART":
				t.Artist = value
			case "IPRD":
				t.Album = value
			case "ITRK", "IPRT":
				t.Track = leadingNumber(value)
			case "ICRD":
				t.Year = leadingNumber(value)
			}
		}
		i += 8 + length + length&1
	}
}

// ---- FLAC ----

func readFLAC(r io.ReaderAt, size int64) (tags, error) {
	var t tags
	head := make([]byte, 4)
	if _, err := r.ReadAt(head, 0); err != nil {
		return t, err
	}
	if string(head) != "fLaC" {
		return t, errors.New("not a FLAC file")
	}
	pos := int64(4)
	block := make([]byte, 4)
	for pos+4 <= min(size, headLimit) {
		if _, err := r.ReadAt(block, pos); err != nil {
			break
		}
		last := block[0]&0x80 != 0
		kind := block[0] & 0x7F
		length := int64(block[1])<<16 | int64(block[2])<<8 | int64(block[3])
		body := pos + 4
		switch kind {
		case 0: // STREAMINFO: sample rate and total samples
			info := make([]byte, 18)
			if _, err := r.ReadAt(info, body); err == nil {
				rate := int64(info[10])<<12 | int64(info[11])<<4 | int64(info[12])>>4
				samples := int64(info[13]&0x0F)<<32 | int64(binary.BigEndian.Uint32(info[14:18]))
				if rate > 0 {
					t.DurationMs = samples * 1000 / rate
				}
			}
		case 4: // VORBIS_COMMENT
			if length <= headLimit {
				data := make([]byte, length)
				if _, err := r.ReadAt(data, body); err == nil {
					readVorbisComments(data, &t)
				}
			}
		case 6: // PICTURE
			if t.ArtLength == 0 {
				readFLACPicture(r, body, length, &t)
			}
		}
		if last {
			break
		}
		pos = body + length
	}
	return t, nil
}

func readVorbisComments(data []byte, t *tags) {
	if len(data) < 8 {
		return
	}
	vendor := int(binary.LittleEndian.Uint32(data))
	i := 4 + vendor
	if i+4 > len(data) {
		return
	}
	count := int(binary.LittleEndian.Uint32(data[i:]))
	i += 4
	for n := 0; n < count && i+4 <= len(data); n++ {
		length := int(binary.LittleEndian.Uint32(data[i:]))
		i += 4
		if length < 0 || i+length > len(data) {
			return
		}
		key, value, ok := strings.Cut(string(data[i:i+length]), "=")
		i += length
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			continue
		}
		switch strings.ToUpper(key) {
		case "TITLE":
			t.Title = value
		case "ARTIST":
			if t.Artist == "" {
				t.Artist = value
			} else {
				t.Artist += " / " + value
			}
		case "ALBUMARTIST", "ALBUM ARTIST":
			t.AlbumArtist = value
		case "ALBUM":
			t.Album = value
		case "TRACKNUMBER":
			t.Track = leadingNumber(value)
		case "DISCNUMBER":
			t.Disc = leadingNumber(value)
		case "DATE", "YEAR":
			t.Year = leadingNumber(value)
		}
	}
}

func readFLACPicture(r io.ReaderAt, body, length int64, t *tags) {
	head := make([]byte, min(length, 4096))
	if _, err := r.ReadAt(head, body); err != nil || len(head) < 8 {
		return
	}
	i := 4 // picture type
	mimeLen := int(binary.BigEndian.Uint32(head[i:]))
	i += 4
	if i+mimeLen+4 > len(head) {
		return
	}
	mime := strings.ToLower(string(head[i : i+mimeLen]))
	i += mimeLen
	descLen := int(binary.BigEndian.Uint32(head[i:]))
	i += 4 + descLen + 16 // description, width, height, depth, colours
	if i+4 > len(head) {
		return
	}
	dataLen := int64(binary.BigEndian.Uint32(head[i:]))
	i += 4
	if int64(i)+dataLen > length {
		return
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	t.ArtOffset, t.ArtLength, t.ArtMime = body+int64(i), dataLen, mime
}

// ---- helpers ----

func syncsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

func latin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// leadingNumber reads "7", "07/12" or "2019-05-03" as 7, 7 and 2019; 0 if there's no number.
func leadingNumber(s string) int {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(s[:end])
	return n
}
