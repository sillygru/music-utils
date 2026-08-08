package lrclib

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSearch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/search" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "no surprises" {
			t.Fatalf("unexpected query: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"No Surprises","trackName":"No Surprises","artistName":"Radiohead","albumName":"OK Computer","duration":229.12,"instrumental":false,"plainLyrics":"lyrics","syncedLyrics":"[00:01]lyrics"},{"id":2,"name":"No Surprises","trackName":"No Surprises","artistName":"Other Artist","albumName":"Cover","duration":230,"instrumental":false,"plainLyrics":"other","syncedLyrics":""}]`))
	}))
	defer server.Close()

	client, err := New(server.URL+"/api", "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	results, err := client.Search(context.Background(), "no surprises")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 || results[0].ID != 1 || results[1].ArtistName != "Other Artist" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestGetExact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/get" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != "test-agent" {
			t.Fatalf("unexpected user agent: %q", got)
		}
		query := r.URL.Query()
		if query.Get("track_name") != "Track Name" || query.Get("artist_name") != "Artist" || query.Get("album_name") != "Album" || query.Get("duration") != "123.5" {
			t.Fatalf("unexpected query: %v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"trackName":"Track Name","artistName":"Artist","albumName":"Album","duration":123.5,"instrumental":false,"plainLyrics":"lyrics","syncedLyrics":"[00:01]lyrics"}`))
	}))
	defer server.Close()

	client, err := New(server.URL+"/api", "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	result, err := client.GetExact(context.Background(), "Track Name", "Artist", "Album", 123.5)
	if err != nil {
		t.Fatalf("get exact: %v", err)
	}
	if result.PlainLyrics != "lyrics" || result.Duration != 123.5 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestGetExactNotFound(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	client, err := New(server.URL, "test-agent", time.Second)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.GetExact(context.Background(), "Track", "Artist", "", 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
