package app

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/sillygru/music-utils/internal/jobs"
)

func TestNormalizeBareRefreshRewritesAValuelessFlag(t *testing.T) {
	// The flag package always takes the next argument as a string flag's value, so
	// a bare -refresh would otherwise swallow whatever came next.
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"bare at the end", []string{"-refresh"}, "-refresh=0"},
		{"bare before another flag", []string{"-refresh", "-dry-run"}, "-refresh=0"},
		{"double dash before another flag", []string{"--refresh", "--dry-run"}, "--refresh=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeBareRefresh(tc.in)
			if got[0] != tc.want {
				t.Errorf("normalizeBareRefresh(%v)[0] = %q, want %q", tc.in, got[0], tc.want)
			}
			if len(got) != len(tc.in) {
				t.Errorf("normalizeBareRefresh(%v) changed the length: %v", tc.in, got)
			}
		})
	}
}

func TestNormalizeBareRefreshLeavesValuedFormsAlone(t *testing.T) {
	// "-refresh 3d" is the ordinary form and must still reach the flag parser as
	// two tokens so it can consume the age.
	cases := [][]string{
		{"-refresh", "3d"},
		{"-refresh=3d"},
		{"--refresh", "1w"},
		{"-refresh", "72h", "-dry-run"},
	}
	for _, in := range cases {
		got := normalizeBareRefresh(in)
		for i := range in {
			if got[i] != in[i] {
				t.Errorf("normalizeBareRefresh(%v)[%d] = %q, want it unchanged", in, i, got[i])
			}
		}
	}
}

func TestNormalizeBareRefreshKeepsANegativeNumberAsAValue(t *testing.T) {
	// Rewriting "-refresh -5" into a bare refresh would turn a value the parser
	// rejects into a full-library refresh, which is the worst possible reading of
	// a typo.
	for _, in := range [][]string{
		{"-refresh", "-5"},
		{"-refresh", "-0.5d"},
		{"-refresh", "-72h"},
		{"-refresh", "-1h30m"},
	} {
		got := normalizeBareRefresh(in)
		if got[0] != in[0] {
			t.Errorf("normalizeBareRefresh(%v)[0] = %q, want %q", in, got[0], in[0])
		}
	}
}

func TestNormalizeBareRefreshDoesNotMutateItsInput(t *testing.T) {
	in := []string{"-refresh", "-dry-run"}
	_ = normalizeBareRefresh(in)
	if in[0] != "-refresh" {
		t.Errorf("input was mutated: %v", in)
	}
}

func TestLooksLikeAge(t *testing.T) {
	// A malformed age has to be recognized as a value so it is rejected, rather
	// than rewritten into a bare refresh that re-fetches the whole library.
	// "-1w2d" is nonsense as an age but belongs here anyway: reporting an invalid
	// value is safe, silently re-fetching everything is not.
	values := []string{"-5", "-0.5", "-72", "-72h", "-0.5d", "-1.5w", "-30m", "-1h30m", "-.5", "-1w2d"}
	for _, token := range values {
		if !looksLikeAge(token) {
			t.Errorf("looksLikeAge(%q) = false, want true", token)
		}
	}
	flags := []string{"-dry-run", "--refresh", "-", "-x", "-5a", "", "-h", "-s", "-d", "-w"}
	for _, token := range flags {
		if looksLikeAge(token) {
			t.Errorf("looksLikeAge(%q) = true, want false", token)
		}
	}
}

func TestRefreshIsNotSetUnlessTheFlagAppears(t *testing.T) {
	// The whole refresh feature hangs off this: a flag's default value is
	// indistinguishable from one the operator typed, and for -refresh the two mean
	// different things. Getting it wrong makes every default run re-fetch the
	// entire library.
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"no flag at all", []string{}, false},
		{"other flags only", []string{"-dry-run", "-limit", "10"}, false},
		{"bare refresh", []string{"-refresh"}, true},
		{"valued refresh", []string{"-refresh", "3d"}, true},
		{"equals refresh", []string{"-refresh=1w"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The same flag name and the same helper the command uses, so this
			// fails if either drifts.
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			refresh := flags.String("refresh", "", "")
			flags.Bool("dry-run", false, "")
			flags.Int("limit", 0, "")
			if err := flags.Parse(normalizeBareRefresh(tc.args)); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if given := refreshGiven(flags); given != tc.want {
				t.Errorf("-refresh given = %v, want %v", given, tc.want)
			}
			// A bare -refresh must reach the value as an empty string, which is
			// what ParseRefresh reads as "re-fetch everything".
			if tc.want && len(tc.args) > 1 && tc.args[1][0] != '-' && *refresh == "" {
				t.Errorf("the age %q was swallowed", tc.args[1])
			}
		})
	}
}

func TestBothJobsAreDiscoverableAndDocumentRefresh(t *testing.T) {
	for _, name := range []string{"metadata-backfill", "lyrics-backfill"} {
		job, err := jobs.Lookup(name)
		if err != nil {
			t.Errorf("lookup %q: %v", name, err)
			continue
		}
		if !strings.Contains(job.Flags(), "-refresh") {
			t.Errorf("%s advertises flags %q but not -refresh", name, job.Flags())
		}
		if strings.TrimSpace(job.Summary()) == "" {
			t.Errorf("%s has no summary for --jobs", name)
		}
	}
}

func TestLyricsBackfillSaysItCannotReachYouTube(t *testing.T) {
	// YouTube needs a video ID the batch path has no way to obtain, and an
	// operator reading only --jobs should not assume otherwise.
	job, err := jobs.Lookup("lyrics-backfill")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !strings.Contains(job.Summary(), "YouTube") {
		t.Errorf("summary %q should say YouTube is out of reach", job.Summary())
	}
}
