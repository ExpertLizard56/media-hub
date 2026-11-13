package main

import "testing"

func TestRankLookup(t *testing.T) {
	dookie := albumInfo{ID: "1", Title: "Dookie", Artist: "Green Day", Type: "Album", Releases: 30}
	demos := albumInfo{ID: "2", Title: "Dookie Demos", Artist: "Green Day", Type: "Album", Releases: 1, Secondary: true}
	lullaby := albumInfo{ID: "3", Title: "Dookie Lullabies: Lullaby Covers of Green Day's Dookie", Artist: "Sparrow Sleeps", Type: "Album", Releases: 1}
	single := albumInfo{ID: "4", Title: "Dookie", Artist: "Someone", Type: "Single", Releases: 1}
	theirEP := albumInfo{ID: "5", Title: "Rocky & Blade", Artist: "Dookie & SKAV", Type: "EP", Releases: 1}

	// "dookie" is also an artist's name: the well-known album still comes first.
	got := rankLookup("dookie", []albumInfo{theirEP}, []albumInfo{demos, lullaby, single, dookie})
	ids := ""
	for _, a := range got {
		ids += a.ID
	}
	if ids != "152" {
		t.Errorf("dookie: got order %q, want 152 (album, the artist's EP, demos; no lullabies or singles)", ids)
	}

	// Typing the word keeps it.
	if got := rankLookup("dookie lullabies", nil, []albumInfo{lullaby}); len(got) != 1 {
		t.Errorf("a typed tribute word should keep the album")
	}

	// An artist's name: their own albums, no duplicates.
	got = rankLookup("green day", []albumInfo{dookie}, []albumInfo{dookie})
	if len(got) != 1 {
		t.Errorf("green day: got %d albums, want 1", len(got))
	}

	// An artist's discography comes under headings, each newest first; others' albums after.
	red := albumInfo{ID: "r", Title: "Red", Artist: "King Crimson", Type: "Album", Year: 1974}
	thrak := albumInfo{ID: "t", Title: "THRAK", Artist: "King Crimson", Type: "Album", Year: 1995}
	vrooom := albumInfo{ID: "v", Title: "VROOOM", Artist: "King Crimson", Type: "EP", Year: 1994}
	usa := albumInfo{ID: "u", Title: "USA", Artist: "King Crimson", Type: "Album", Year: 1975, Secondary: true, Kind: "Live", Votes: 3}
	archive := albumInfo{ID: "a", Title: "Live in Some Town", Artist: "King Crimson", Type: "Album", Year: 2025, Secondary: true, Kind: "Live"}
	guide := albumInfo{ID: "g", Title: "A Young Person's Guide to King Crimson", Artist: "King Crimson", Type: "Album", Year: 1976, Secondary: true, Kind: "Compilation"}
	songbook := albumInfo{ID: "s", Title: "King Crimson Songbook", Artist: "The Crimson Jazz Trio", Type: "Album", Year: 2005}
	got = rankLookup("king crimson", []albumInfo{guide, archive, usa, red, vrooom, thrak}, []albumInfo{songbook, guide})
	order := ""
	for _, a := range got {
		order += a.ID + ":" + a.Group + " "
	}
	if want := "t:Albums r:Albums v:EPs u:Live albums a:Live albums g:Compilations s:More "; order != want {
		t.Errorf("king crimson: got %q, want %q", order, want)
	}
	if got := rankLookup("king crimson songbook", nil, []albumInfo{songbook}); got[0].Group != "Albums" {
		t.Errorf("with nothing before them, others' albums go under Albums, not %q", got[0].Group)
	}
}
