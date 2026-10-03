package jobs

import (
	"strings"
	"testing"
	"time"
)

func TestParseAgeAcceptsCoarseUnits(t *testing.T) {
	// The d and w suffixes are the reason parseAge exists: time.ParseDuration
	// rejects both, and an age is the unit an operator actually thinks in.
	cases := map[string]time.Duration{
		"3d":   72 * time.Hour,
		"1d":   24 * time.Hour,
		"14d":  14 * 24 * time.Hour,
		"1w":   7 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"0.5d": 12 * time.Hour,
		"1.5w": 10.5 * 24 * time.Hour,
		" 7d ": 7 * 24 * time.Hour,
	}
	for input, want := range cases {
		got, err := parseAge(input)
		if err != nil {
			t.Errorf("parseAge(%q): unexpected error %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseAge(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestParseAgeAcceptsGoDurations(t *testing.T) {
	cases := map[string]time.Duration{
		"0":     0,
		"72h":   72 * time.Hour,
		"30m":   30 * time.Minute,
		"1h30m": 90 * time.Minute,
		"24h":   24 * time.Hour,
	}
	for input, want := range cases {
		got, err := parseAge(input)
		if err != nil {
			t.Errorf("parseAge(%q): unexpected error %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("parseAge(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestParseAgeRejectsUnusableValues(t *testing.T) {
	// A negative age reads as "refresh anything settled within the last hour",
	// which is the opposite of every other duration flag. Rejecting it is safer
	// than clamping, because clamping would silently re-fetch the whole library.
	for _, input := range []string{"", "   ", "-1h", "-3d", "soon", "3", "3x", "d", "w", "3dd"} {
		if got, err := parseAge(input); err == nil {
			t.Errorf("parseAge(%q) = %s, want an error", input, got)
		}
	}
}

func TestParseAgeErrorsNameTheAcceptedForms(t *testing.T) {
	_, err := parseAge("soon")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The message is the operator's only clue about what they may type.
	for _, want := range []string{"72h", "3d", "1w"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestParseRefreshDistinguishesBareFromAbsent(t *testing.T) {
	// An empty value is the bare -refresh case and must re-fetch everything.
	enabled, age, err := ParseRefresh("")
	if err != nil || !enabled || age != 0 {
		t.Fatalf("bare -refresh: got (%v, %s, %v), want (true, 0, nil)", enabled, age, err)
	}
	// An explicit zero means the same thing here, but is spelled out.
	enabled, age, err = ParseRefresh("0")
	if err != nil || !enabled || age != 0 {
		t.Fatalf("-refresh 0: got (%v, %s, %v), want (true, 0, nil)", enabled, age, err)
	}
	enabled, age, err = ParseRefresh("3d")
	if err != nil || !enabled || age != 72*time.Hour {
		t.Fatalf("-refresh 3d: got (%v, %s, %v)", enabled, age, err)
	}
	if _, _, err := ParseRefresh("-3d"); err == nil {
		t.Fatal("-refresh -3d: expected an error")
	}
}

func TestSelectionFromOptionsCarriesTheFlag(t *testing.T) {
	// A zero RefreshOlderThan must not read as "refresh everything" when the
	// flag was never given, or every default run would re-fetch the library.
	off := selectionFrom(Options{})
	if off.enabled {
		t.Error("an Options with Refresh false must not select a refresh")
	}
	if !off.cutoff().IsZero() {
		t.Error("a disabled selection must have no cutoff")
	}
	if got := off.describe(); got != "never-settled work only" {
		t.Errorf("describe() = %q", got)
	}

	all := selectionFrom(Options{Refresh: true})
	if !all.enabled {
		t.Error("Refresh true must select a refresh")
	}
	if got := all.describe(); !strings.Contains(got, "all settled work") {
		t.Errorf("describe() = %q", got)
	}

	aged := selectionFrom(Options{Refresh: true, RefreshOlderThan: 72 * time.Hour})
	if got := aged.describe(); !strings.Contains(got, "older than") {
		t.Errorf("describe() = %q", got)
	}
}

func TestSelectionDescribeStatesPriority(t *testing.T) {
	// A refresh has to say that never-settled work goes first. That ordering is
	// the operator-visible part of the contract, and it is not obvious from the
	// counts alone. The default mode is "never-settled only", so it has no
	// ordering to state.
	for _, opts := range []Options{
		{Refresh: true},
		{Refresh: true, RefreshOlderThan: time.Hour},
	} {
		if got := selectionFrom(opts).describe(); !strings.Contains(got, "first") {
			t.Errorf("describe() = %q, want it to state the priority", got)
		}
	}
}

func TestSelectionCutoffMovesWithTheAge(t *testing.T) {
	// A cutoff has to be in the past, and further in the past for a larger age.
	// Otherwise a refresh would select the freshly settled rows it just created.
	now := time.Now()
	small := selectionFrom(Options{Refresh: true, RefreshOlderThan: time.Hour}).cutoff()
	large := selectionFrom(Options{Refresh: true, RefreshOlderThan: 30 * 24 * time.Hour}).cutoff()
	if !small.Before(now) {
		t.Errorf("cutoff %s should be before now", small)
	}
	if !large.Before(small) {
		t.Errorf("a 30d cutoff (%s) should be earlier than a 1h cutoff (%s)", large, small)
	}
	// A bare refresh has no age, so its cutoff is now: every settled row is older
	// than it except the ones written during this very run.
	all := selectionFrom(Options{Refresh: true}).cutoff()
	if all.After(time.Now().Add(time.Second)) {
		t.Errorf("a bare refresh cutoff should be now, got %s", all)
	}
	if !all.After(large) {
		t.Errorf("a bare refresh cutoff (%s) should be later than a 30d cutoff (%s)", all, large)
	}
}
