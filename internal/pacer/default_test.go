package pacer

import (
	"testing"
	"time"
)

// OrDefault is what lets a provider client accept an optional shared pacer without
// losing its own pacing when the caller passes nothing.
func TestOrDefaultSubstitutesWhenNil(t *testing.T) {
	got := OrDefault(nil, time.Millisecond)
	if got == nil {
		t.Fatal("OrDefault(nil) returned nil, which would panic on Wait")
	}
	// A nil *Pacer satisfies Wait by doing nothing, so the substituted value has to
	// be a real one for pacing to happen at all.
	pace, ok := got.(*Pacer)
	if !ok {
		t.Fatalf("OrDefault(nil) = %T, want *Pacer", got)
	}
	if pace.interval != time.Millisecond {
		t.Errorf("interval = %s, want 1ms", pace.interval)
	}
}

func TestOrDefaultKeepsAProvidedWaiter(t *testing.T) {
	shared := NewShared(nil, "lrclib", time.Millisecond, time.Millisecond, 0)
	provided := shared.ForJob()
	got := OrDefault(provided, time.Hour)
	// A provided waiter must come back untouched, or a job would stop sharing its
	// upstream budget with the live server. Comparing the interface values is enough
	// here because the fallback would be a different dynamic type.
	if _, isShared := got.(*sharedWaiter); !isShared {
		t.Fatalf("OrDefault replaced a shared waiter with %T", got)
	}
	// And the substitution interval must not have leaked into it.
	if _, isPacer := got.(*Pacer); isPacer {
		t.Error("OrDefault replaced a shared waiter with a local pacer")
	}
}
