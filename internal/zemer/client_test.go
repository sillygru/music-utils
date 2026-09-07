package zemer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetInlineCanonical(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"videoId":"abc","verified":true,"hasSynced":false,"sources":[` +
			`{"type":"canonical","plain":"line one\nline two\nline three\nline four"}]}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-agent", 5*time.Second)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	result, err := client.Get(t.Context(), "abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.PlainLyrics, "line four") {
		t.Fatalf("unexpected plain %q", result.PlainLyrics)
	}
}

func TestGetPrefersSynced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"videoId":"abc","sources":[` +
			`{"type":"community","plain":"plain one\nplain two\nplain three\nplain four"},` +
			`{"type":"manual","synced":true,"syncedLrc":"[00:01.00]synced one\n[00:05.00]synced two\n[00:09.00]synced three\n[00:13.00]synced four"}]}`))
	}))
	defer server.Close()

	client, _ := New(server.URL, "test-agent", 5*time.Second)
	result, err := client.Get(t.Context(), "abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(result.SyncedLyrics, "[00:01.00]synced one") {
		t.Fatalf("synced source should win: %+v", result)
	}
}

func TestScrapeHTML(t *testing.T) {
	html := `<html><head><title>Song</title><script>var x=1;</script></head><body>` +
		`<div>nav home menu search login</div><div class="lyrics">` +
		`<p>First lyric line of the song here</p><p>Second lyric line of the song here</p>` +
		`<p>Third lyric line of the song here</p><p>Fourth lyric line of the song here</p>` +
		`<p>Fifth lyric line of the song here</p></div></body></html>`
	got := scrapeLyricsHTML("shironet", html)
	for _, want := range []string{"First lyric", "Fifth lyric"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestRankSources(t *testing.T) {
	sources := []Source{
		{Type: "community", Plain: "x"},
		{Type: "jkaraoke", FeedURL: "http://x", SongID: 1, Synced: true},
		{Type: "manual", Plain: "x"},
	}
	ranked := rankSources(sources)
	if ranked[0].Type != "jkaraoke" {
		t.Fatalf("jkaraoke synced should rank first: %+v", ranked)
	}
}
