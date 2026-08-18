package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sillygru/music-utils/internal/db"
)

func TestCompactRichSyncContent(t *testing.T) {
	content := `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" ttp:timeBase="media" xmlns:ttp="http://www.w3.org/ns/ttml#parameter"><head><metadata><ttm:title>Somebody's Pleasure</ttm:title><ttm:agent xml:id="v1" type="person"><ttm:name>Aziz Hedra</ttm:name></ttm:agent></metadata></head><body dur="3:43.980"><div><p begin="0:07.184" end="0:13.436"><span begin="0:07.184" end="0:07.532">I've</span> <span begin="0:07.532" end="0:07.819">been</span> <span begin="0:07.819" end="0:08.112">so</span> <span begin="0:08.112" end="0:09.420">busy,</span></p></div></body></tt>`

	value := compactRichSyncContent(content, "ttml")
	got, ok := value.(compactRichSync)
	if !ok {
		t.Fatalf("expected compact richsync object, got %T: %v", value, value)
	}
	if got.Title != "Somebody's Pleasure" || got.Artist != "Aziz Hedra" || got.Duration != 223.98 {
		t.Fatalf("unexpected metadata: %+v", got)
	}
	if len(got.Lines) != 1 {
		t.Fatalf("expected one line, got %d", len(got.Lines))
	}
	line := got.Lines[0]
	if line.Begin != 7.184 || line.End != 13.436 || line.Text != "I've been so busy," {
		t.Fatalf("unexpected line: %+v", line)
	}
	if len(line.Words) != 4 || line.Words[0].Begin != 7.184 || line.Words[0].End != 7.532 || line.Words[0].Text != "I've" {
		t.Fatalf("unexpected words: %+v", line.Words)
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal compact richsync: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode compact richsync: %v", err)
	}
	if decoded["title"] != "Somebody's Pleasure" || decoded["artist"] != "Aziz Hedra" || decoded["duration"] != 223.98 {
		t.Fatalf("unexpected compact fields: %s", encoded)
	}
	lines, ok := decoded["lines"].([]any)
	if !ok || len(lines) != 1 {			t.Fatalf("unexpected compact lines: %s", encoded)
	}
	lineTuple, ok := lines[0].([]any)
	if !ok || len(lineTuple) != 4 || lineTuple[2] != "I've been so busy," {
		t.Fatalf("unexpected line tuple: %s", encoded)
	}
	words, ok := lineTuple[3].([]any)
	if !ok || len(words) != 4 {
		t.Fatalf("unexpected word tuples: %s", encoded)
	}
}

func TestMigrateRichLyricsStoresCompactJSON(t *testing.T) {
	_, lyricsDB := testHTTPDatabases(t)
	content := `<tt><head><metadata><title>Song</title><agent><name>Artist</name></agent></metadata></head><body dur="0:10"><div><p begin="0:01" end="0:02"><span begin="0:01" end="0:02">word</span></p></div></body></tt>`
	if err := db.UpsertRichLyrics(context.Background(), lyricsDB, db.RichLyrics{TrackID: 42, Content: content, Format: "ttml", SyncType: "word", Source: "unison"}); err != nil {
		t.Fatalf("seed rich lyrics: %v", err)
	}

	migrated, err := db.MigrateRichLyrics(context.Background(), lyricsDB, compactRichSyncForStorage)
	if err != nil || migrated != 1 {
		t.Fatalf("migrate rich lyrics: rows=%d err=%v", migrated, err)
	}
	got, err := db.FindRichLyrics(context.Background(), lyricsDB, 42, "word")
	if err != nil {
		t.Fatalf("find migrated rich lyrics: %v", err)
	}
	if got.Format != "json" {
		t.Fatalf("expected json storage format, got %q", got.Format)
	}
	parsed, ok := parseStoredCompactRichSync(got.Content)
	if !ok || parsed.Title != "Song" || parsed.Artist != "Artist" || parsed.Duration != 10 {
		t.Fatalf("unexpected migrated content: %q", got.Content)
	}
	if migrated, err := db.MigrateRichLyrics(context.Background(), lyricsDB, compactRichSyncForStorage); err != nil || migrated != 0 {
		t.Fatalf("expected idempotent migration, rows=%d err=%v", migrated, err)
	}
}

func TestCompactRichSyncContentIgnoresLinesWithoutArray(t *testing.T) {
	if got := compactRichSyncContent(`{"title":"Song","lines":null}`, "json"); got != `{"title":"Song","lines":null}` {
		t.Fatalf("expected non-array lines payload to remain unchanged, got %v", got)
	}
	if got := compactRichSyncContent(`null`, "json"); got != `null` {
		t.Fatalf("expected JSON null payload to remain unchanged, got %v", got)
	}
}

func TestCompactRichSyncContentLeavesUnsupportedPayloadsAlone(t *testing.T) {
	content := "[00:01.00]line"
	if got := compactRichSyncContent(content, "lrc"); got != content {
		t.Fatalf("expected non-TTML payload to remain unchanged, got %v", got)
	}
	invalid := "<tt><p>broken"
	if got := compactRichSyncContent(invalid, "ttml"); got != invalid {
		t.Fatalf("expected invalid TTML payload to remain unchanged, got %v", got)
	}
}

func TestWriteLyricsResponseJSONCompactLayout(t *testing.T) {
	res := lyricsResponse{
		ID: 42, Name: "Somebody's Pleasure", TrackName: "Somebody's Pleasure",
		ArtistName: "Aziz Hedra", AlbumName: "Album", Duration: 229,
		RichSync: &richSyncResult{
			Content: compactRichSync{
				Title:    "Somebody's Pleasure",
				Artist:   "Aziz Hedra",
				Duration: 223.98,
				Lines: []compactRichLine{
					{
						Begin: 7.184, End: 13.436, Text: "I've been so busy,",
						Words: []compactRichWord{
							{Begin: 7.184, End: 7.532, Text: "I've"},
							{Begin: 7.532, End: 7.819, Text: "been"},
							{Begin: 8.112, End: 9.42, Text: "busy,"},
						},
					},
				},
			},
			Format: "json", SyncType: "word", Source: "unison",
		},
	}
	var b bytes.Buffer
	if err := writeLyricsResponseJSON(&b, res); err != nil {
		t.Fatalf("write lyrics json: %v", err)
	}
	body := b.String()

	// The line tuple stays on a single line with the words array opened inline.
	if !strings.Contains(body, `        [7.184, 13.436, "I've been so busy,", [`+"\n") {
		t.Fatalf("expected compact line tuple, got:\n%s", body)
	}
	// Each word tuple sits on its own line.
	for _, word := range []string{
		`          [7.184, 7.532, "I've"],`,
		`          [7.532, 7.819, "been"],`,
		`          [8.112, 9.42, "busy,"]`,
	} {
		if !strings.Contains(body, word+"\n") {
			t.Fatalf("expected word line %q, got:\n%s", word, body)
		}
	}
	// No word tuple is expanded across multiple lines.
	if strings.Contains(body, "7.184,\n          13.436") {
		t.Fatalf("word tuple was expanded across lines:\n%s", body)
	}
	if strings.Count(body, "\n") > 30 {
		t.Fatalf("response unexpectedly verbose (%d lines):\n%s", strings.Count(body, "\n")+1, body)
	}

	// The pretty body still decodes to the same values.
	var decoded struct {
		ID       int64 `json:"id"`
		RichSync struct {
			Content map[string]any `json:"content"`
			Format  string         `json:"format"`
			Sync    string         `json:"syncType"`
			Source  string         `json:"source"`
		} `json:"richSync"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode compact lyrics json: %v", err)
	}
	if decoded.ID != 42 || decoded.RichSync.Format != "json" || decoded.RichSync.Sync != "word" || decoded.RichSync.Source != "unison" {
		t.Fatalf("unexpected decoded wrapper: %+v", decoded)
	}
	lines, ok := decoded.RichSync.Content["lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("unexpected decoded lines: %#v", decoded.RichSync.Content["lines"])
	}
	line, ok := lines[0].([]any)
	if !ok || len(line) != 4 || line[2] != "I've been so busy," {
		t.Fatalf("unexpected decoded line: %#v", lines[0])
	}
	words, ok := line[3].([]any)
	if !ok || len(words) != 3 {
		t.Fatalf("unexpected decoded words: %#v", line[3])
	}
	first, ok := words[0].([]any)
	if !ok || first[0] != 7.184 || first[1] != 7.532 || first[2] != "I've" {
		t.Fatalf("unexpected decoded word: %#v", words[0])
	}
}

func TestWriteLyricsResponseJSONUnparseableContentStaysString(t *testing.T) {
	res := lyricsResponse{
		ID: 7, Name: "Rich Only", TrackName: "Rich Only", ArtistName: "Artist",
		RichSync: &richSyncResult{
			Content:  "<tt>rich only</tt>",
			Format:   "ttml",
			SyncType: "word",
			Source:   "unison",
		},
	}
	var b bytes.Buffer
	if err := writeLyricsResponseJSON(&b, res); err != nil {
		t.Fatalf("write lyrics json: %v", err)
	}
	body := b.String()
	if !strings.Contains(body, `    "content": "\u003ctt\u003erich only\u003c/tt\u003e",`) {
		t.Fatalf("expected string content kept inline, got:\n%s", body)
	}
	var decoded struct {
		RichSync struct {
			Content string `json:"content"`
		} `json:"richSync"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode string content: %v", err)
	}
	if decoded.RichSync.Content != "<tt>rich only</tt>" {
		t.Fatalf("unexpected decoded content: %q", decoded.RichSync.Content)
	}
}
