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
		_, _ = w.Write([]byte(`{"type":"Line","lyrics":[` +
			`{"time":7.18,"text":"hello","element":{"singer":"v1"}},` +
			`{"time":9.0,"text":"backing","element":{"singer":"v2"},"syllabus":[{"time":9.0,"duration":1.0,"text":"backing","isBackground":true}]}` +
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
