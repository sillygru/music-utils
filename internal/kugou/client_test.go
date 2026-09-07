package kugou

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func lrcContent() string {
	return "[00:01.00]first line\n[00:05.00]second line\n"
}

func TestGetByHash(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(lrcContent()))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/search/song", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":1,"errcode":0,"error":"","data":{"info":[{"duration":181,"hash":"abc123"}]}}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":1,"info":"","errcode":0,"errmsg":"","expire":0,"candidates":[{"id":99,"product_from":"x","duration":181000,"accesskey":"key1"}]}`))
	})
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":"` + encoded + `"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(server.URL, server.URL, "test-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := client.Get(t.Context(), "Song", "Artist", "Album", 181)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:01.00]first line") {
		t.Fatalf("unexpected synced %q", result.SyncedLyrics)
	}
	if !strings.Contains(result.PlainLyrics, "second line") {
		t.Fatalf("unexpected plain %q", result.PlainLyrics)
	}
}

func TestNormalize(t *testing.T) {
	raw := "[00:00.00]作词: Someone\n[00:01.00]hello\n[00:05.00]world\n[00:09.00]作曲: Someone\njunk line\n"
	got := Normalize(raw)
	if strings.Contains(got, "作词") || strings.Contains(got, "作曲") || strings.Contains(got, "junk") {
		t.Fatalf("normalize kept junk: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Fatalf("normalize dropped lyrics: %q", got)
	}
}
