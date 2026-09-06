package richlyrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientGetUnisonEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lyrics" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("song") != "Example Song" || query.Get("artist") != "Example Artist" || query.Get("album") != "Example Album" {
			t.Fatalf("unexpected query: %v", query)
		}
		if _, ok := query["duration"]; ok {
			t.Fatalf("duration must never be sent upstream: %v", query)
		}
		if r.Header.Get("User-Agent") != "test-agent" {
			t.Fatalf("unexpected user agent: %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"lyrics":"<tt>words</tt>","format":"ttml","syncType":"word"}}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	result, err := client.Get(context.Background(), "Example Song", "Example Artist", "Example Album")
	if err != nil {
		t.Fatalf("get rich lyrics: %v", err)
	}
	if result.Content != "<tt>words</tt>" || result.Format != "ttml" || result.SyncType != "word" || result.Source != "unison" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestClientGetDirectPayloadAndMiss(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("song") == "Missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lyrics":"[00:01.00]line","format":"lrc","sync_type":"syllable"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	result, err := client.Get(context.Background(), "Song", "Artist", "")
	if err != nil || result.SyncType != "syllable" || result.Format != "lrc" {
		t.Fatalf("unexpected direct result: %+v, %v", result, err)
	}
	if _, err := client.Get(context.Background(), "Missing", "Artist", ""); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
