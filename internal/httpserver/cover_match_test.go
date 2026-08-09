package httpserver

import (
	"testing"

	"github.com/sillygru/music-utils/internal/cover"
)

func TestCoverResultMatchesAlbum(t *testing.T) {
	cases := []struct {
		name   string
		input  cover.Input
		result cover.Result
		want   bool
	}{
		{"exact match", cover.Input{ArtistName: "Radiohead", AlbumName: "OK Computer"}, cover.Result{ArtistName: "Radiohead", AlbumName: "OK Computer"}, true},
		{"remastered variant", cover.Input{ArtistName: "Radiohead", AlbumName: "OK Computer"}, cover.Result{ArtistName: "Radiohead", AlbumName: "OK Computer (Remastered)"}, true},
		{"unrelated album", cover.Input{ArtistName: "Radiohead", AlbumName: "six seven"}, cover.Result{ArtistName: "Fleetwood Mac", AlbumName: "Live (Deluxe Edition)"}, false},
		{"artist mismatch tolerated on exact album", cover.Input{ArtistName: "Radiohead", AlbumName: "OK Computer"}, cover.Result{ArtistName: "Various Artists", AlbumName: "OK Computer"}, true},
		{"artist mismatch rejected on fuzzy album", cover.Input{ArtistName: "Radiohead", AlbumName: "OK Computer"}, cover.Result{ArtistName: "Various Artists", AlbumName: "OK Computer Remix"}, false},
		{"near-miss artist rejected on exact album", cover.Input{ArtistName: "Eagles", AlbumName: "Imagine"}, cover.Result{ArtistName: "Wings On Eagles", AlbumName: "Imagine"}, false},
		{"near-miss artist rejected on fuzzy album", cover.Input{ArtistName: "Eagles", AlbumName: "Imagine"}, cover.Result{ArtistName: "Wings On Eagles", AlbumName: "Imagine - Single"}, false},
		{"empty artist input", cover.Input{ArtistName: "", AlbumName: "OK Computer"}, cover.Result{ArtistName: "Radiohead", AlbumName: "OK Computer"}, true},
		{"empty album", cover.Input{ArtistName: "Radiohead"}, cover.Result{ArtistName: "Radiohead", AlbumName: "OK Computer"}, false},
	}
	for _, tc := range cases {
		if got := coverResultMatches(cover.Album, tc.input, tc.result); got != tc.want {
			t.Errorf("%s: coverResultMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCoverResultMatchesArtist(t *testing.T) {
	cases := []struct {
		name   string
		input  cover.Input
		result cover.Result
		want   bool
	}{
		{"exact", cover.Input{ArtistName: "Radiohead"}, cover.Result{ArtistName: "Radiohead"}, true},
		{"definite article", cover.Input{ArtistName: "Beatles"}, cover.Result{ArtistName: "The Beatles"}, true},
		{"wrong artist", cover.Input{ArtistName: "Radiohead"}, cover.Result{ArtistName: "Fleetwood Mac"}, false},
		{"missing artist input", cover.Input{ArtistName: ""}, cover.Result{ArtistName: "Radiohead"}, false},
	}
	for _, tc := range cases {
		if got := coverResultMatches(cover.Artist, tc.input, tc.result); got != tc.want {
			t.Errorf("%s: coverResultMatches = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCoverResultMatchesSongAlwaysTrue(t *testing.T) {
	if !coverResultMatches(cover.Song, cover.Input{ArtistName: "Radiohead", TrackName: "Creep"}, cover.Result{ArtistName: "Someone Else", TrackName: "Something Else"}) {
		t.Fatal("expected song results to pass through unfiltered")
	}
}
