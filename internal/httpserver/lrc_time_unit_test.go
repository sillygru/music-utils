package httpserver

import (
	"strings"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

// The stored LRC below is what the LyricsPlus mirror wrote for The Pretty
// Reckless' "Only You" before v0.17.0 converted its millisecond timings. Tags
// past the first minute carry the millisecond value formatted as seconds.
const storedMilliLRC = "[00:00.00]Oh, boy, have you seen my head?\n" +
	"[00:44.36]I've lost my mind, so I forget and\n" +
	"[00:59.40]Only you can bring me back to life\n" +
	"[1115:09.00]Only you can put me into right\n" +
	"[3265:54.00]I will arise\n"

func TestNormalizeStoredSyncedLyrics(t *testing.T) {
	// A stored millisecond payload is rescaled on the way out, so a row that
	// predates the provider fix stops reaching clients broken.
	got := normalizeStoredSyncedLyrics(storedMilliLRC, "lyricsplus")
	if strings.Contains(got, "1115:") || strings.Contains(got, "3265:") {
		t.Fatalf("millisecond tags survived: %q", got)
	}
	if !strings.Contains(got, "[01:06.90]Only you can put me into right") {
		t.Fatalf("expected the inflated tag to be repaired, got %q", got)
	}
	if !strings.Contains(got, "[03:15.95]I will arise") {
		t.Fatalf("expected the final tag to be repaired, got %q", got)
	}
	// Sub-minute tags are untouched, so the part that was already right stays
	// byte-identical.
	if !strings.Contains(got, "[00:44.36]I've lost my mind, so I forget and") {
		t.Fatalf("a correct tag was modified: %q", got)
	}
}

func TestNormalizeStoredSyncedLyricsLeavesGoodContentAlone(t *testing.T) {
	for _, s := range []string{
		"[00:44.36]second\n[03:18.44]I will arise\n",
		"[00:00.00]start\n[119:32.10]long live set\n",
		"plain untimed lyric\n",
		"",
	} {
		if got := normalizeStoredSyncedLyrics(s, "lyricsplus"); got != s {
			t.Fatalf("seconds payload was modified: got %q want %q", got, s)
		}
	}
}

func TestNormalizeStoredSyncedLyricsStillStripsVoiceTags(t *testing.T) {
	// The guard wraps CleanSyncedLyrics rather than replacing it, so the voice
	// tags that function existed to strip are still stripped.
	got := normalizeStoredSyncedLyrics("{agent:v1}[00:01.00]{bg}hey\n", "lyricsplus")
	if got != "[00:01.00]hey\n" {
		t.Fatalf("voice tags not stripped: %q", got)
	}
}

func TestToLyricsResponseRepairsStoredMillisecondTimings(t *testing.T) {
	// The end-to-end path a request takes: a stored row out of the database and
	// into the response body.
	track := &db.Track{ID: 1, Name: "Only You", ArtistName: "The Pretty Reckless", Duration: 217}
	lyrics := &db.Lyrics{PlainLyrics: "plain", SyncedLyrics: storedMilliLRC, Source: "lyricsplus"}
	response := toLyricsResponse(track, lyrics)
	if strings.Contains(response.SyncedLyrics, "[1115:") {
		t.Fatalf("response carried millisecond timings: %q", response.SyncedLyrics)
	}
	if !strings.Contains(response.SyncedLyrics, "[03:15.95]I will arise") {
		t.Fatalf("response did not carry repaired timings: %q", response.SyncedLyrics)
	}
	if response.PlainLyrics != "plain" {
		t.Fatalf("plain lyrics should be left as stored, got %q", response.PlainLyrics)
	}
}

func TestBuildLyricsFileUsesRepairedTimings(t *testing.T) {
	// buildLyricsFile feeds parseLRC, whose pattern historically could not match
	// a four-digit minute field at all, so the raw tag leaked into the rendered
	// text. With the repair in front of it the stamps parse and the text is the
	// lyric only.
	track := &db.Track{ID: 1, Name: "Only You", ArtistName: "The Pretty Reckless", Duration: 217}
	lyrics := &db.Lyrics{SyncedLyrics: storedMilliLRC, Source: "lyricsplus"}
	out := buildLyricsFile(track, lyrics)
	if strings.Contains(out, "[1115:") || strings.Contains(out, "1115:") {
		t.Fatalf("raw millisecond tag leaked into the lyricsfile:\n%s", out)
	}
	if !strings.Contains(out, "start_ms: 195950") {
		t.Fatalf("expected the final repaired stamp in the lyricsfile:\n%s", out)
	}
	if !strings.Contains(out, "text: I will arise") {
		t.Fatalf("expected lyric text without timestamp debris:\n%s", out)
	}
}
