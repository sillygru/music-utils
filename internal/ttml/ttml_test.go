package ttml

import (
	"strings"
	"testing"
)

const sampleTTML = `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xmlns:ttm="http://www.w3.org/ns/ttml#metadata">
<body>
<div>
<p begin="00:01.500" end="00:04.000">Hello <span begin="00:01.500" end="00:02.000">world</span></p>
<p begin="5.0s" end="8s">Second line</p>
<p begin="00:10.00" end="00:12.00" ttm:role="background">Backing vocal</p>
<p>Untimed note</p>
</div>
</body>
</tt>`

func TestParse(t *testing.T) {
	lines, err := Parse(sampleTTML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
	if lines[0].Start != 1.5 {
		t.Fatalf("line 0 start = %v, want 1.5", lines[0].Start)
	}
	if len(lines[0].Words) != 1 || lines[0].Words[0].Text != "world" {
		t.Fatalf("line 0 words = %+v", lines[0].Words)
	}
	if !lines[2].Background {
		t.Fatalf("line 2 should be background")
	}
	if lines[3].Start >= 0 {
		t.Fatalf("untimed line should have negative start")
	}
}

func TestToLRC(t *testing.T) {
	lrc, err := ParseToLRC(sampleTTML)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(lrc, "[00:01.50]Hello world") {
		t.Fatalf("missing first line in %q", lrc)
	}
	if !strings.Contains(lrc, "[00:10.00]Backing vocal") || strings.Contains(lrc, "{bg}") {
		t.Fatalf("unexpected bg tag or missing backing line in %q", lrc)
	}
	if strings.Contains(lrc, "Untimed") {
		t.Fatalf("untimed line should be skipped in %q", lrc)
	}
}

func TestCleanSyncedLyrics(t *testing.T) {
	input := "[00:10.00]{agent:v1}Take me\n[00:12.00]{bg}Backing vocal\n[00:14.00]{agent:v2}Second"
	want := "[00:10.00]Take me\n[00:12.00]Backing vocal\n[00:14.00]Second"
	if got := CleanSyncedLyrics(input); got != want {
		t.Fatalf("CleanSyncedLyrics(%q) = %q, want %q", input, got, want)
	}
}

func TestExtractPlainFromLRC(t *testing.T) {
	input := "[00:10.00]{agent:v1}Take me\n[00:12.00]Backing vocal"
	want := "Take me\nBacking vocal"
	if got := ExtractPlainFromLRC(input); got != want {
		t.Fatalf("ExtractPlainFromLRC(%q) = %q, want %q", input, got, want)
	}
}

func TestParseTime(t *testing.T) {
	cases := map[string]float64{
		"00:01.500": 1.5,
		"01:02.50":  62.5,
		"01:02:03":  3723,
		"5.0s":      5,
		"1500ms":    1.5,
		"2m":        120,
		"1h":        3600,
	}
	for input, want := range cases {
		got, ok := ParseTime(input)
		if !ok || got != want {
			t.Fatalf("ParseTime(%q) = %v,%v want %v", input, got, ok, want)
		}
	}
}
