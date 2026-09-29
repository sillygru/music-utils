package jobs

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sillygru/music-utils/internal/db"
	"github.com/sillygru/music-utils/internal/metadata"
)

// TestClassifyLookupOnlyPersistsGenuineMisses is the safety property the whole
// backfill rests on: a track may only be recorded as having no upstream data
// when the providers genuinely said so. Everything else must stay retryable.
func TestClassifyLookupOnlyPersistsGenuineMisses(t *testing.T) {
	throttle := &metadata.RateLimitError{Provider: "itunes", Status: http.StatusTooManyRequests, RetryAfter: time.Second}

	tests := []struct {
		name  string
		track *db.Track
		err   error
		want  Outcome
	}{
		{
			name:  "resolved",
			track: &db.Track{Name: "Song", ArtistName: "Artist"},
			want:  OutcomeSucceeded,
		},
		{
			name: "genuine miss",
			err:  metadata.ErrNotFound,
			want: OutcomeMissing,
		},
		{
			name: "genuine miss wrapped in a join",
			err:  errors.Join(metadata.ErrNotFound, errors.New("context detail")),
			want: OutcomeMissing,
		},
		{
			name: "rate limited",
			err:  errors.Join(metadata.ErrNotFound, metadata.ErrInconclusive, throttle),
			want: OutcomeThrottled,
		},
		{
			name: "transient provider error",
			err:  errors.Join(metadata.ErrNotFound, metadata.ErrInconclusive, errors.New("iTunes returned HTTP 500")),
			want: OutcomeFailed,
		},
		{
			name: "bare inconclusive",
			err:  errors.Join(metadata.ErrNotFound, metadata.ErrInconclusive),
			want: OutcomeFailed,
		},
		{
			name: "unrecognised error",
			err:  errors.New("something else entirely"),
			want: OutcomeFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyLookup(tc.track, tc.err)
			if got != tc.want {
				t.Fatalf("classifyLookup = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyLookupNeverPersistsTransientAsMissing is the regression this
// change exists for, stated directly: a provider error must never be recorded
// as "this song has no upstream data".
func TestClassifyLookupNeverPersistsTransientAsMissing(t *testing.T) {
	transient := errors.Join(metadata.ErrNotFound, metadata.ErrInconclusive,
		errors.New("iTunes returned HTTP 500"))
	if got := classifyLookup(nil, transient); got == OutcomeMissing {
		t.Fatal("a transient provider failure was classified as a genuine miss; " +
			"the track would be permanently marked as having no upstream data")
	}
}

// TestClassifyLookupThrottleBeatsInconclusive guards the ordering: a throttled
// lookup is joined with ErrInconclusive too, so the throttle must be detected
// first, otherwise the job would stop backing off when a provider is pushing
// back.
func TestClassifyLookupThrottleBeatsInconclusive(t *testing.T) {
	throttled := errors.Join(metadata.ErrNotFound, metadata.ErrInconclusive,
		&metadata.RateLimitError{Provider: "deezer", Status: http.StatusServiceUnavailable})
	if got := classifyLookup(nil, throttled); got != OutcomeThrottled {
		t.Fatalf("classifyLookup = %v, want %v", got, OutcomeThrottled)
	}
}

// TestClassifyLookupPrefersTrackOverError documents the ordering when a
// provider returns both a usable track and a non-nil error.
func TestClassifyLookupPrefersTrackOverError(t *testing.T) {
	track := &db.Track{Name: "Song", ArtistName: "Artist"}
	if got := classifyLookup(track, metadata.ErrNotFound); got != OutcomeSucceeded {
		t.Fatalf("classifyLookup = %v, want %v", got, OutcomeSucceeded)
	}
}
