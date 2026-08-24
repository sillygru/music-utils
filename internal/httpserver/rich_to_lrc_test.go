package httpserver

import (
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

func TestCompactRichSyncToLRC(t *testing.T) {
	content, format, ok := compactRichSyncForStorage(`<tt><body><p begin="0:01.25" end="0:03">hello <span begin="0:01.25" end="0:02">hello</span></p><p begin="00:10" end="00:12"><span begin="00:10" end="00:11">world</span></p></body></tt>`, "ttml")
	if !ok {
		t.Fatal("expected TTML conversion")
	}
	got := compactRichSyncToLRC(&db.RichLyrics{Content: content, Format: format})
	want := "[00:01.25]hello hello\n[00:10.00]world\n"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestCompactRichSyncToLRCCanUseWordsWhenLineTextMissing(t *testing.T) {
	content := `{"lines":[[2,3,"",[[2,2.5,"one"],[2.5,3,"two"]]]]}`
	got := compactRichSyncToLRC(&db.RichLyrics{Content: content, Format: "json"})
	if got != "[00:02.00]one two\n" {
		t.Fatalf("unexpected LRC: %q", got)
	}
}
