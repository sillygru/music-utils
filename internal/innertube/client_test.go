package innertube

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, captionURL string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/youtubei/v1/next", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"contents": map[string]any{"tabs": []any{map[string]any{
			"browseEndpoint": map[string]any{"browseId": "MPLYt_test123", "params": "ggM"},
		}}}}
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/youtubei/v1/browse", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"contents": map[string]any{"sectionListRenderer": map[string]any{
			"contents": []any{map[string]any{"musicDescriptionShelfRenderer": map[string]any{
				"description": map[string]any{"runs": []any{
					map[string]any{"text": "official lyric line one\nofficial lyric line two"},
				}},
			}}},
		}}}
		_ = json.NewEncoder(w).Encode(payload)
	})
	mux.HandleFunc("/youtubei/v1/player", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{"captions": map[string]any{
			"playerCaptionsTracklistRenderer": map[string]any{"captionTracks": []any{
				map[string]any{"baseUrl": captionURL, "languageCode": "en"},
			}},
		}}
		_ = json.NewEncoder(w).Encode(payload)
	})
	return httptest.NewServer(mux)
}

func TestGetOfficialLyrics(t *testing.T) {
	server := testServer(t, "http://example.invalid/captions")
	defer server.Close()
	client, err := New(server.URL+"/youtubei/v1", "test-key", "test-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := client.GetOfficialLyrics(t.Context(), "video123")
	if err != nil {
		t.Fatalf("official: %v", err)
	}
	if !strings.Contains(result.PlainLyrics, "official lyric line one") {
		t.Fatalf("unexpected lyrics %q", result.PlainLyrics)
	}
}

func TestGetTranscript(t *testing.T) {
	captions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?><transcript>` +
			`<text start="1.5" dur="2.0">hello world</text>` +
			`<text start="4.0" dur="2.0">second caption here</text>` +
			`</transcript>`))
	}))
	defer captions.Close()
	server := testServer(t, captions.URL)
	defer server.Close()

	client, _ := New(server.URL+"/youtubei/v1", "test-key", "test-agent", 5*time.Second)
	result, err := client.GetTranscript(t.Context(), "video123")
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:01.50]hello world") {
		t.Fatalf("unexpected synced %q", result.SyncedLyrics)
	}
	if !strings.Contains(result.PlainLyrics, "second caption") {
		t.Fatalf("unexpected plain %q", result.PlainLyrics)
	}
}

func TestFindLyricsEndpointNoMatch(t *testing.T) {
	id, _ := findLyricsEndpoint(map[string]any{"contents": map[string]any{}})
	if id != "" {
		t.Fatalf("expected no endpoint, got %q", id)
	}
}
