package paxsenix

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetELRC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog/us/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":{"songs":{"data":[{"id":"42","type":"songs"}]}},` +
			`"resources":{"songs":{"42":{"attributes":{"name":"Song","artistName":"Artist","albumName":"Album","durationInMillis":180000}}}}}`))
	})
	mux.HandleFunc("/apple-music/lyrics", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "42" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"type":"Line","elrc":"[00:01.00]hello","plain":"hello"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := NewWithToken(server.URL, "test-agent", "fixed-token", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	client.catalogBaseURL = server.URL
	result, err := client.Get(t.Context(), "Song", "Artist", "Album")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:01.00]hello") {
		t.Fatalf("unexpected synced %q", result.SyncedLyrics)
	}
}

func TestGetTTMLPriority(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/catalog/us/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":{"songs":{"data":[{"id":"7","type":"songs"}]}},` +
			`"resources":{"songs":{"7":{"attributes":{"name":"Song","artistName":"Artist"}}}}}`))
	})
	mux.HandleFunc("/apple-music/lyrics", func(w http.ResponseWriter, r *http.Request) {
		ttml := `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="00:02.00" end="00:04.00"><span begin="00:02.00" end="00:03.00">hi</span></p></div></body></tt>`
		quoted := strings.ReplaceAll(ttml, `"`, `\"`)
		_, _ = w.Write([]byte(`{"type":"Syllable","ttmlContent":"` + quoted + `","elrc":"[00:01.00]stale"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, _ := NewWithToken(server.URL, "test-agent", "fixed-token", 5*time.Second)
	client.catalogBaseURL = server.URL
	result, err := client.Get(t.Context(), "Song", "Artist", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:02.00]hi") {
		t.Fatalf("ttml should win over elrc: %q", result.SyncedLyrics)
	}
	if !result.WordSynced {
		t.Fatalf("ttml result should be word synced")
	}
}

func TestBuildFromContent(t *testing.T) {
	payload := lyricsPayload{
		Type: "Syllable",
		Content: []lyricsContent{
			{Timestamp: 1000, Text: []lyricText{{Text: "hello"}}},
			{Timestamp: 2000, Text: []lyricText{{Text: "bg vocal"}}, Background: true},
			{Timestamp: 3000, Text: []lyricText{{Text: "other"}}, OppositeTurn: true},
		},
	}
	synced, _, wordSynced := buildFromContent(payload)
	if !wordSynced {
		t.Fatalf("syllable type should be word synced")
	}
	if !strings.Contains(synced, "[00:01.00]hello") || !strings.Contains(synced, "[00:02.00]bg vocal") || strings.Contains(synced, "{agent:") || strings.Contains(synced, "{bg}") {
		t.Fatalf("unexpected agent tags or missing lines: %q", synced)
	}
}
