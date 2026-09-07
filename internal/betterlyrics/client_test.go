package betterlyrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTTML = `<tt xmlns="http://www.w3.org/ns/ttml"><body><div>` +
	`<p begin="00:01.00" end="00:03.00">first line</p>` +
	`<p begin="00:04.00" end="00:06.00">second line</p>` +
	`</div></body></tt>`

func TestGet(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ttml":` + quoteJSON(testTTML) + `}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := client.Get(t.Context(), "Song", "Artist", "Album", 200)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:01.00]first line") {
		t.Fatalf("unexpected synced %q", result.SyncedLyrics)
	}
	if !strings.Contains(gotQuery, "d=") {
		t.Fatalf("duration should be forwarded in ms: %q", gotQuery)
	}
}

func TestGetNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-agent", 5*time.Second)
	if _, err := client.Get(t.Context(), "Song", "Artist", "", 0); err == nil {
		t.Fatalf("expected error")
	}
}

func quoteJSON(s string) string {
	out := strings.ReplaceAll(s, `\`, `\\`)
	out = strings.ReplaceAll(out, `"`, `\"`)
	return `"` + out + `"`
}
