package applemusic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientSearchAndLyrics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/catalog/us/search" {
			_, _ = fmt.Fprint(w, `{"results":{"songs":{"data":[{"id":"42","attributes":{"name":"Song","artistName":"Artist","albumName":"Album","durationInMillis":200000,"isrc":"US123"}}]}}}`)
			return
		}
		if r.URL.Path == "/v1/catalog/us/songs/42/lyrics" {
			_, _ = fmt.Fprint(w, `{"data":[{"attributes":{"ttml":"<ttml>lyrics</ttml>"}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := New(server.URL, server.URL, "us", "test", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	track, err := client.SearchTrack(context.Background(), "Song", "Artist", "Album", 200)
	if err != nil || track.ID != "42" {
		t.Fatalf("track=%+v err=%v", track, err)
	}
	lyrics, err := client.GetLyrics(context.Background(), track.ID)
	if err != nil || lyrics.Content != "<ttml>lyrics</ttml>" {
		t.Fatalf("lyrics=%+v err=%v", lyrics, err)
	}
}
