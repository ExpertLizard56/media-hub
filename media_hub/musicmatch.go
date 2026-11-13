package main

// Which Usenet copies are really a given album (musicbrainz.go). A copy's name has to hold the
// artist's and the album's words; karaoke, tributes, covers, live and the like are left out
// unless the album itself is one; and its size has to fit the album's length (a single or a
// video isn't the album). The rest are ordered best first: CD-quality FLAC, then MP3 320 or
// hi-res FLAC, then the rest; the closer the name, the better; then the most downloaded.

import (
	"sort"
	"strings"
)

// Words that make a copy something else than the album, unless the album's own title has them.
var notTheAlbum = []string{
	"karaoke", "instrumental", "instrumentals", "tribute", "covers", "cover", "lullaby", "lullabies",
	"acapella", "acappella", "remix", "remixes", "remixed", "live", "demo", "demos", "bootleg",
	"8bit", "piano", "orchestral", "reimagined", "sped", "slowed", "nightcore", "cdm", "cds", "single",
}

// Words that say it's a bigger edition: allowed, but the plain one is closer.
var biggerEdition = []string{"deluxe", "expanded", "anniversary", "edition", "remaster", "remastered", "bonus", "box", "boxset"}

// matchCopies: the copies of found that are album a, best first.
func matchCopies(a albumInfo, found []release) []release {
	need := strings.Fields(foldWords(a.Artist + " " + stripEdition(a.Title)))
	own := wordSet(a.Artist + " " + a.Title)
	minutes := float64(a.DurationMs) / 60000
	type scored struct {
		r       release
		quality int
		extra   int
	}
	var list []scored
	for _, r := range found {
		words := wordSet(strings.ReplaceAll(r.Title, "_", " "))
		ok := true
		for _, w := range need {
			if !words[w] && w != "the" && w != "and" {
				ok = false
				break
			}
		}
		for _, w := range notTheAlbum {
			if words[w] && !own[w] {
				ok = false
			}
		}
		if !ok {
			continue
		}
		mb := float64(r.Bytes) / (1 << 20)
		if minutes > 0 && r.Bytes > 0 && (mb < minutes*0.7 || mb > minutes*45) {
			continue // too small for the whole album (about 96 kbps at least), or far too big
		}
		extra := 0
		for _, w := range biggerEdition {
			if words[w] && !own[w] {
				extra++
			}
		}
		for w := range words {
			if !own[w] {
				extra++ // every other word (group, year, "web") counts a little
			}
		}
		perMinute := 0.0
		if minutes > 0 {
			perMinute = mb / minutes
		}
		list = append(list, scored{r, copyQuality(words, perMinute), extra})
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.quality != b.quality {
			return a.quality > b.quality
		}
		if a.r.Grabs != b.r.Grabs {
			return a.r.Grabs > b.r.Grabs
		}
		return a.extra < b.extra
	})
	out := make([]release, 0, len(list))
	for _, s := range list {
		s.r.Quality = qualityName[s.quality]
		out = append(out, s.r)
	}
	return out
}

var qualityName = map[int]string{4: "FLAC", 3: "FLAC (hi-res or vinyl)", 2: "MP3 320", 1: "MP3", 0: ""}

func copyQuality(words map[string]bool, mbPerMinute float64) int {
	flac := words["flac"] || words["lossless"]
	// Hi-res by its name ("24BIT", "24-96", "Hi-Res") or by its size: CD-quality FLAC is about
	// 5-9 MB a minute, 24-bit two to four times that.
	hires := words["24bit"] || words["24"] && (words["bit"] || words["96"] || words["192"] || words["88"] || words["176"] || words["48"] || words["44"]) ||
		words["96khz"] || words["192khz"] || words["hi"] && words["res"] || words["hires"] || mbPerMinute > 14
	switch {
	case flac && !hires && !words["vinyl"]:
		return 4
	case flac:
		return 3 // hi-res is several times bigger for no difference on a phone; vinyl rips carry the record's noise
	case words["320"] || words["320kbps"]:
		return 2
	case words["mp3"] || words["v0"] || words["vbr"]:
		return 1
	}
	return 0
}
