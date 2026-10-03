package lyricsplus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTTML = `<tt xmlns="http://www.w3.org/ns/ttml"><body><div>` +
	`<p begin="00:01.00" end="00:03.00"><span begin="00:01.00" end="00:02.00">word</span> <span begin="00:02.00" end="00:03.00">line</span></p></div></body></tt>`

func TestGetBinimumWordSyncWins(t *testing.T) {
	var mux *http.ServeMux
	mux = http.NewServeMux()
	var serverURL string
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ttmlJSON := strings.ReplaceAll(testTTML, `"`, `\"`)
		_, _ = w.Write([]byte(`{"total":1,"results":[{"timing_type":"word","lyricsUrl":` +
			`"` + serverURL + `/ttml"}]}`))
		_ = ttmlJSON
	})
	mux.HandleFunc("/ttml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(testTTML))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	client, err := New(server.URL, nil, "test-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := client.Get(t.Context(), "Song", "Artist", "", 0, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !result.WordSynced || !strings.Contains(result.SyncedLyrics, "word line") {
		t.Fatalf("unexpected result %+v", result)
	}
}

func TestGetMirror(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/lyrics/get", func(w http.ResponseWriter, r *http.Request) {
		// The mirror contract is milliseconds, so timings are sent in ms and
		// must come back out as seconds.
		_, _ = w.Write([]byte(`{"type":"Line","lyrics":[` +
			`{"time":7180,"text":"hello","element":{"singer":"v1"}},` +
			`{"time":9000,"text":"backing","element":{"singer":"v2"},"syllabus":[{"time":9000,"duration":1000,"text":"backing","isBackground":true}]}` +
			`]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := New(server.URL, []string{server.URL}, "test-agent", 5*time.Second)
	// Point the Binimum index at a 404 so the mirror path is exercised.
	binimum404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer binimum404.Close()
	client.apiBaseURL = binimum404.URL

	result, err := client.Get(t.Context(), "Song", "Artist", "", 0, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:07.18]hello") || !strings.Contains(result.SyncedLyrics, "[00:09.00]backing") || strings.Contains(result.SyncedLyrics, "{agent:") || strings.Contains(result.SyncedLyrics, "{bg}") {
		t.Fatalf("unexpected agent/bg tags or missing lines: %q", result.SyncedLyrics)
	}
}

func TestISRCQuery(t *testing.T) {
	var gotQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"total":0,"results":[]}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, nil, "test-agent", 5*time.Second)
	_, _ = client.Get(t.Context(), "Song", "Artist", "", 0, "USRC17607839")
	joined := strings.Join(gotQueries, "&")
	if !strings.Contains(joined, "isrc=") {
		t.Fatalf("expected isrc query, got %q", joined)
	}
}

// The mirror reports milliseconds, so the compact rich JSON that gets persisted
// must be seconds. Storing milliseconds here is what made stored richSync
// payloads render at 30:51 instead of 00:30.51.
func TestMirrorWordModeConvertsMillisToSeconds(t *testing.T) {
	lines := []mirrorLine{{
		Time:     1851,
		Duration: 6162,
		Text:     "In my depression I will lie",
	}}
	lines[0].Syllabi = append(lines[0].Syllabi,
		struct {
			Time         float64 `json:"time"`
			Duration     float64 `json:"duration"`
			Text         string  `json:"text"`
			IsBackground bool    `json:"isBackground"`
		}{Time: 1851, Duration: 437, Text: "In"},
		struct {
			Time         float64 `json:"time"`
			Duration     float64 `json:"duration"`
			Text         string  `json:"text"`
			IsBackground bool    `json:"isBackground"`
		}{Time: 2288, Duration: 714, Text: "my"},
	)
	rich := mirrorLinesToRichJSON(lines)
	if strings.Contains(rich, "1851") || strings.Contains(rich, "8013") {
		t.Fatalf("rich JSON still holds millisecond timings: %s", rich)
	}
	for _, want := range []string{"1.851", "8.013", "1.851", "2.288", "3.002"} {
		if !strings.Contains(rich, want) {
			t.Fatalf("rich JSON missing %s: %s", want, rich)
		}
	}
}

func TestConvertMirrorLinesUnitsAndBounds(t *testing.T) {
	// The mirror contract is milliseconds, so 44363 is 44.363 seconds and must
	// render as [00:44.36].
	millis := func(v float64) float64 { return v * 1000 }
	lines := []mirrorLine{
		{Time: millis(0), Text: "first"},
		{Time: millis(44.363), Text: "second"},
		{Time: millis(195.954), Text: "third"},
	}
	synced, plain := convertMirrorLines(lines, false)
	want := "[00:00.00]first\n[00:44.36]second\n[03:15.95]third"
	if synced != want {
		t.Fatalf("mirror timings wrong:\n got %q\nwant %q", synced, want)
	}
	if plain != "first\nsecond\nthird" {
		t.Fatalf("plain lyrics wrong: %q", plain)
	}

	// A mirror that reports seconds where milliseconds were expected hands back a
	// number a thousand times too large. Those lines are dropped rather than
	// stored as a song with an eighteen-hour first verse, and plain text stays in
	// step with the synced lines so the two never disagree.
	bad := []mirrorLine{
		{Time: millis(0), Text: "kept"},
		{Time: 66909000, Text: "seconds read as milliseconds"},
		{Time: millis(4), Text: "also kept"},
	}
	synced, plain = convertMirrorLines(bad, false)
	if synced != "[00:00.00]kept\n[00:04.00]also kept" {
		t.Fatalf("implausible line was not dropped: %q", synced)
	}
	if plain != "kept\nalso kept" {
		t.Fatalf("plain and synced fell out of step: %q", plain)
	}
}
