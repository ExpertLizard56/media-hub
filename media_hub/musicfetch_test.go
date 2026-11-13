package main

import (
	"strings"
	"testing"
)

const newznabSample = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">
<channel>
<item>
  <title>Green Day - 21st Century Breakdown (2009) [FLAC]</title>
  <link>https://api.nzbgeek.info/api?t=get&amp;id=abc&amp;apikey=SECRET</link>
  <pubDate>Tue, 12 May 2009 10:00:00 +0000</pubDate>
  <category>Audio &gt; Lossless</category>
  <enclosure url="x" length="123" type="application/x-nzb"/>
  <newznab:attr name="size" value="456789"/>
  <newznab:attr name="grabs" value="42"/>
</item>
<item><title></title><link>skip-me</link></item>
</channel>
</rss>`

func TestParseNewznab(t *testing.T) {
	got, err := parseNewznab([]byte(newznabSample))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d releases, want 1", len(got))
	}
	r := got[0]
	if r.Bytes != 456789 || r.Grabs != 42 || r.Posted != "2009-05-12" || r.ID == "" || !strings.Contains(r.link, "SECRET") {
		t.Errorf("bad release %+v", r)
	}
	if _, err := parseNewznab([]byte(`<error code="100" description="Incorrect user credentials"/>`)); err == nil {
		t.Error("an indexer error should be an error")
	}
}

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"Green Day - Dookie (1994) [FLAC]": "Green Day - Dookie (1994) [FLAC]",
		"../../etc/passwd":                 "_.._etc_passwd",
		`a/b\c:d`:                      "a_b_c_d",
		"   ":                              "download",
		"..":                               "download",
	} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := redact("GET https://x/?apikey=SECRET failed", "SECRET", ""); strings.Contains(got, "SECRET") {
		t.Errorf("key not redacted: %s", got)
	}
}

func TestMatchCopies(t *testing.T) {
	a := albumInfo{Title: "Dookie", Artist: "Green Day", DurationMs: 40 * 60000}
	mb := int64(1 << 20)
	found := []release{
		{ID: "lullaby", Title: "Dookie Lullabies Lullaby Covers of Green Days Dookie FLAC", Bytes: 200 * mb},
		{ID: "mp3", Title: "Green Day - Dookie (1994) MP3 320kbps", Bytes: 95 * mb},
		{ID: "flac", Title: "Green.Day-Dookie.1994.FLAC", Bytes: 280 * mb},
		{ID: "hires", Title: "Green Day-Dookie 30th Anniversary Deluxe Edition-24BIT-WEB-FLAC-2023", Bytes: 1400 * mb},
		{ID: "single", Title: "Green Day - Dookie Single FLAC", Bytes: 30 * mb},
		{ID: "tiny", Title: "Green Day - Dookie FLAC", Bytes: 10 * mb},
		{ID: "other", Title: "Green Day - Nimrod FLAC", Bytes: 300 * mb},
		{ID: "karaoke", Title: "Green Day - Dookie Karaoke FLAC", Bytes: 300 * mb},
	}
	var ids []string
	for _, r := range matchCopies(a, found) {
		ids = append(ids, r.ID)
	}
	if got := strings.Join(ids, ","); got != "flac,hires,mp3" {
		t.Errorf("matchCopies = %s, want flac,hires,mp3", got)
	}
	// A live album keeps its live copies.
	live := albumInfo{Title: "Bullet in a Bible (Live)", Artist: "Green Day", DurationMs: 70 * 60000}
	if got := matchCopies(live, []release{{ID: "x", Title: "Green Day - Bullet In A Bible Live FLAC", Bytes: 500 * mb}}); len(got) != 1 {
		t.Errorf("live album lost its live copy")
	}
}

func TestFoldAndEdition(t *testing.T) {
	if got := foldWords("Simon & Garfunkel’s  Greatest-Hits!"); got != "simon and garfunkels greatest hits" {
		t.Errorf("foldWords = %q", got)
	}
	if got := stripEdition("Dookie (30th Anniversary Deluxe)"); got != "Dookie" {
		t.Errorf("stripEdition = %q", got)
	}
	if got := innerPath("Release/CD1/01 Song.flac"); got != "CD1/01 Song.flac" {
		t.Errorf("innerPath = %q", got)
	}
	if got := innerPath("01 Song.flac"); got != "01 Song.flac" {
		t.Errorf("innerPath = %q", got)
	}
}
