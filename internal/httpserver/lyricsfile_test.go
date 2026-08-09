package httpserver

import (
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

func TestBuildLyricsFileSynced(t *testing.T) {
	// Timestamps, blank-line handling, and end_ms chaining mirror the bytes
	// lrclib.net actually serves for this track.
	track := &db.Track{Name: "No Surprises", ArtistName: "Radiohead", AlbumName: "OK Computer", Duration: 229}
	lyrics := &db.Lyrics{
		PlainLyrics:  "A heart that's full up like a landfill\nA job that slowly kills you\n",
		SyncedLyrics: "[00:25.65] A heart that's full up like a landfill\n[00:35.22] A job that slowly kills you\n[00:49.23] \n",
	}
	want := "version: '1.0'\n" +
		"metadata:\n" +
		"  title: No Surprises\n" +
		"  artist: Radiohead\n" +
		"  album: OK Computer\n" +
		"  duration_ms: 229000\n" +
		"  instrumental: false\n" +
		"lines:\n" +
		"- text: A heart that's full up like a landfill\n" +
		"  start_ms: 25650\n" +
		"  end_ms: 35220\n" +
		"- text: A job that slowly kills you\n" +
		"  start_ms: 35220\n" +
		"  end_ms: 49230\n" +
		"- text: ''\n" +
		"  start_ms: 49230\n" +
		"plain: |-\n" +
		"  A heart that's full up like a landfill\n" +
		"  A job that slowly kills you\n" +
		"\n"
	if got := buildLyricsFile(track, lyrics); got != want {
		t.Fatalf("unexpected lyricsfile:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildLyricsFilePlainOnly(t *testing.T) {
	// Interior paragraph separators and the trailing newline artifact are
	// emitted as bare blank lines, matching LRCLIB's plain block bytes.
	track := &db.Track{Name: "Just Words", ArtistName: "Some Artist", AlbumName: "", Duration: 200.5}
	lyrics := &db.Lyrics{PlainLyrics: "Just words\n\nMore words\n"}
	want := "version: '1.0'\n" +
		"metadata:\n" +
		"  title: Just Words\n" +
		"  artist: Some Artist\n" +
		"  album: ''\n" +
		"  duration_ms: 200500\n" +
		"  instrumental: false\n" +
		"lines: []\n" +
		"plain: |-\n" +
		"  Just words\n" +
		"\n" +
		"  More words\n" +
		"\n"
	if got := buildLyricsFile(track, lyrics); got != want {
		t.Fatalf("unexpected lyricsfile:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildLyricsFileInstrumental(t *testing.T) {
	track := &db.Track{Name: "Interlude", ArtistName: "Artist", AlbumName: "Album", Duration: 60}
	lyrics := &db.Lyrics{Instrumental: true}
	want := "version: '1.0'\n" +
		"metadata:\n" +
		"  title: Interlude\n" +
		"  artist: Artist\n" +
		"  album: Album\n" +
		"  duration_ms: 60000\n" +
		"  instrumental: true\n" +
		"lines: []\n"
	if got := buildLyricsFile(track, lyrics); got != want {
		t.Fatalf("unexpected lyricsfile:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestParseLRC(t *testing.T) {
	if lines := parseLRC(""); lines != nil {
		t.Fatalf("expected nil for empty synced lyrics, got %+v", lines)
	}

	lines := parseLRC("[00:01.5]half a second\n[01:02.345]ms precision\n[00:10.00][00:20.00]multi tag\ncarried forward")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(lines), lines)
	}
	if lines[0].startMS != 1500 || lines[0].text != "half a second" {
		t.Fatalf("unexpected first line: %+v", lines[0])
	}
	if lines[1].startMS != 62345 || lines[1].text != "ms precision" {
		t.Fatalf("unexpected second line: %+v", lines[1])
	}
	if lines[2].startMS != 10000 || lines[2].text != "multi tag" {
		t.Fatalf("unexpected multi-tag line: %+v", lines[2])
	}
	if lines[3].startMS != 10000 || lines[3].text != "carried forward" {
		t.Fatalf("unexpected untagged line: %+v", lines[3])
	}

	// Hour-style tags must not partially match the seconds/fraction portion.
	hours := parseLRC("[01:02:03.45]hours")
	if len(hours) != 1 || hours[0].startMS != 0 || hours[0].text != "[01:02:03.45]hours" {
		t.Fatalf("expected hour-style line to fall through untagged, got %+v", hours)
	}
}

func TestYAMLScalar(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Wavin' Flag", "Wavin' Flag"}, // apostrophes stay raw, like LRCLIB
		{"OK Computer", "OK Computer"},
		{"", "''"},
		{"a: b", "'a: b'"},
		{"a: it's b", "'a: it''s b'"}, // quoted values double their apostrophes
		{"#hash", "'#hash'"},
		{" lead", "' lead'"},
		{"trail ", "'trail '"},
		{"it's", "it's"}, // mid-scalar apostrophes stay raw, like LRCLIB
	}
	for _, tc := range cases {
		if got := yamlScalar(tc.in); got != tc.want {
			t.Errorf("yamlScalar(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildLyricsFileQuotesDangerousMetadata(t *testing.T) {
	track := &db.Track{Name: "a: b", ArtistName: "Artist", AlbumName: "Album", Duration: 10}
	lyrics := &db.Lyrics{PlainLyrics: "words"}
	want := "version: '1.0'\n" +
		"metadata:\n" +
		"  title: 'a: b'\n" +
		"  artist: Artist\n" +
		"  album: Album\n" +
		"  duration_ms: 10000\n" +
		"  instrumental: false\n" +
		"lines: []\n" +
		"plain: |-\n" +
		"  words\n"
	if got := buildLyricsFile(track, lyrics); got != want {
		t.Fatalf("unexpected lyricsfile:\ngot:\n%q\nwant:\n%q", got, want)
	}
}
