package betterlyrics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStreamExtractsAllProviderVariants(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"provider\":\"musixmatch\",\"results\":{\"wordByWord\":\"[00:00]word\",\"synced\":\"[00:00]line\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"provider\":\"qq\",\"results\":{\"lyrics\":\"{\\\"lyrics\\\":\\\"qrc\\\"}\"}}\n\n")
	}))
	defer server.Close()
	client, err := New(server.URL, "test-agent", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []Result
	err = client.Stream(context.Background(), "Song", "Artist", "", 0, func(result Result) {
		mu.Lock()
		got = append(got, result)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected three variants, got %+v", got)
	}
	if got[0].Source != "musixmatch" || got[0].SyncType != "word" || got[1].SyncType != "line" || got[2].Source != "better_lyrics_portato" {
		t.Fatalf("unexpected variants: %+v", got)
	}
}
