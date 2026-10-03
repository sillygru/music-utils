package jobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// day and week are the coarse units -refresh accepts on top of Go's own
// duration syntax.
//
// An age is the unit an operator thinks in when asking for a refresh ("give me
// anything older than three days"), and time.ParseDuration has no word for that.
// Requiring "-refresh 72h" instead would work but reads as an arithmetic
// exercise, and the unit the operator meant would have to be recomputed by hand
// every time.
const (
	day  = 24 * time.Hour
	week = 7 * day
)

// parseAge parses a -refresh value into the age at which a settled answer stops
// counting as current.
//
// It accepts Go's duration syntax plus a d or w suffix for the two units
// operators reach for and Go does not have. A negative age is rejected rather
// than clamped: "-refresh -1h" reads as "refresh anything checked within the
// last hour", which is the opposite of what every other duration flag means, and
// silently reinterpreting it would re-fetch the entire library.
func parseAge(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, fmt.Errorf("refresh age is empty")
	}

	// The suffix is stripped before parsing so the multiplier applies to a
	// plain Go duration. "1h30m" therefore still parses, and only a trailing d
	// or w is treated as a coarse unit.
	multiplier := time.Duration(1)
	if unit := trimmed[len(trimmed)-1]; unit == 'd' || unit == 'w' {
		body := trimmed[:len(trimmed)-1]
		count, err := strconv.ParseFloat(body, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid refresh age %q: want a duration such as 72h, 3d, or 1w", value)
		}
		if unit == 'w' {
			multiplier = week
		} else {
			multiplier = day
		}
		age := time.Duration(count * float64(multiplier))
		if age < 0 {
			return 0, fmt.Errorf("refresh age %q must not be negative", value)
		}
		return age, nil
	}

	age, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid refresh age %q: want a duration such as 72h, 3d, or 1w", value)
	}
	if age < 0 {
		return 0, fmt.Errorf("refresh age %q must not be negative", value)
	}
	return age, nil
}

// ParseRefresh validates a -refresh flag value and reports whether refreshing is
// requested along with the age cutoff.
//
// An empty value is the bare "-refresh" case: re-fetch everything, with the
// never-settled work first. It is distinguished from an explicit zero because
// the two are the same request today but not necessarily tomorrow, and a caller
// that could only see "0" would have no way to ask for one without the other.
func ParseRefresh(value string) (enabled bool, olderThan time.Duration, err error) {
	if strings.TrimSpace(value) == "" {
		return true, 0, nil
	}
	age, err := parseAge(value)
	if err != nil {
		return false, 0, err
	}
	return true, age, nil
}

// refreshSelection describes what a job should work on, given how -refresh was
// used. Both jobs share it so "what does -refresh mean" has one answer rather
// than one per job.
type refreshSelection struct {
	// enabled reports whether -refresh appeared at all. Without it a job only
	// touches work no provider has ever settled.
	enabled bool
	// olderThan is the age at which a settled answer counts as stale. Zero means
	// every settled item is stale, which is what a bare -refresh asks for.
	olderThan time.Duration
}

// selection folds Options into the selection a job's producer pages from.
func selectionFrom(opts Options) refreshSelection {
	return refreshSelection{enabled: opts.Refresh, olderThan: opts.RefreshOlderThan}
}

// cutoff is the instant a settled answer must predate to be refreshed. The zero
// time means "no cutoff", which the selection queries read as "everything".
func (r refreshSelection) cutoff() time.Time {
	if !r.enabled {
		return time.Time{}
	}
	return time.Now().Add(-r.olderThan)
}

// describe renders the mode for a job's header, so an operator can tell a
// default run from a refresh without consulting the command they typed.
func (r refreshSelection) describe() string {
	if !r.enabled {
		return "never-settled work only"
	}
	if r.olderThan <= 0 {
		return "all settled work, never-settled first"
	}
	return fmt.Sprintf("settled work older than %s, never-settled first", r.olderThan)
}
