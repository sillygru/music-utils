package musixmatch

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
		if r.URL.Path == "/ws/1.1/track.search" {
			_, _ = fmt.Fprint(w, `{"message":{"body":{"track_list":[{"track":{"track_id":42,"commontrack_id":7,"track_name":"Song","artist_name":"Artist","album_name":"Album","track_length":200,"track_isrc":"US123"}}]}}}`)
			return
		}
		if r.URL.Path == "/ws/1.1/track.lyrics.get" {
			_, _ = fmt.Fprint(w, `{"message":{"body":{"lyrics":{"lyrics_body":"line one\nline two","lyrics_language":"en"}}}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := New(server.URL, "secret", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	track, err := client.SearchTrack(context.Background(), "Song", "Artist", "Album", 200)
	if err != nil || track.ID != "42" {
		t.Fatalf("track=%+v err=%v", track, err)
	}
	lyrics, err := client.GetLyrics(context.Background(), track.ID, track.ISRC)
	if err != nil || lyrics.PlainLyrics != "line one\nline two" {
		t.Fatalf("lyrics=%+v err=%v", lyrics, err)
	}
}
