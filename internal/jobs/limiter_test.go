package jobs

import (
	"context"
	"testing"
	"time"
)

func TestGatePacesToConfiguredRate(t *testing.T) {
	// 600/min is one request every 100ms.
	gate := NewGate(600)
	ctx := context.Background()

	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	start := time.Now()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Fatalf("expected the gate to space requests ~100ms, waited %s", elapsed)
	}
}

func TestGateWithoutCeilingDoesNotDelay(t *testing.T) {
	// Zero means "provider pacing only": the gate must not add its own delay,
	// it should only react to rate limits.
	gate := NewGate(0)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		start := time.Now()
		if err := gate.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
			t.Fatalf("expected no ceiling delay, waited %s on call %d", elapsed, i)
		}
	}
}

func TestGateThrottleHoldsSubsequentWork(t *testing.T) {
	gate := NewGate(0)
	ctx := context.Background()

	// A provider asked for a two second cooldown.
	gate.Throttle(2 * time.Second)

	start := time.Now()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("wait after throttle: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("expected the gate to honor the 2s cooldown, waited %s", elapsed)
	}

	rateLimits, pauses := gate.Stats()
	if rateLimits != 1 {
		t.Errorf("expected 1 recorded rate limit, got %d", rateLimits)
	}
	if pauses < 1 {
		t.Errorf("expected at least 1 pause, got %d", pauses)
	}
}

func TestGateThrottleWithoutRetryAfterStillBacksOff(t *testing.T) {
	gate := NewGate(0)
	ctx := context.Background()

	// No Retry-After advertised: the gate must still apply a minimum penalty
	// rather than retrying straight back into the same rate limit.
	gate.Throttle(0)

	start := time.Now()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("wait after throttle: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("expected a minimum backoff, waited %s", elapsed)
	}
}

func TestGateBackoffGrowsAndDecays(t *testing.T) {
	gate := NewGate(0)
	gate.maxBackoff = 8 * time.Second

	gate.Throttle(0)
	first := gate.penalty()
	gate.Throttle(0)
	second := gate.penalty()
	if second <= first {
		t.Fatalf("expected backoff to grow: first %s, second %s", first, second)
	}

	// Successes decay the penalty back to zero so a recovered provider is used
	// at full speed again.
	gate.Succeed()
	gate.Succeed()
	if penalty := gate.penalty(); penalty != 0 {
		t.Fatalf("expected backoff to decay to zero, got %s", penalty)
	}
}

func TestGateBackoffIsCapped(t *testing.T) {
	gate := NewGate(0)
	gate.maxBackoff = 4 * time.Second
	for i := 0; i < 20; i++ {
		gate.Throttle(0)
	}
	if penalty := gate.penalty(); penalty > 4*time.Second {
		t.Fatalf("expected penalty capped at 4s, got %s", penalty)
	}
}

func TestGateWaitHonorsContextCancellation(t *testing.T) {
	gate := NewGate(1) // one request per minute: the wait is long.
	// The first unit is admitted immediately and consumes the free slot.
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := gate.Wait(ctx); err == nil {
		t.Fatal("expected Wait to return the context error")
	}
}

func TestGateDoesNotBurstAfterIdleGap(t *testing.T) {
	// 600/min is one request every 100ms.
	gate := NewGate(600)
	ctx := context.Background()

	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("wait after idle: %v", err)
	}

	// Immediate following request must still wait ~100ms and not burst
	start := time.Now()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("subsequent wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Fatalf("expected the gate to space subsequent request ~100ms after idle, waited %s", elapsed)
	}
}

// penalty reports the gate's current backoff for assertions.
func (g *Gate) penalty() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backoff
}

// pause reports how far ahead the gate is currently paused.
func (g *Gate) pause() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Until(g.pausedUntil)
}

// WaitThrottle exists because a run that fans one song out across several
// providers can be partway through it when a provider refuses, and the rest of that
// song must not go out during the cooldown. It has to block without touching the
// steady ceiling, or every lookup would start spending the song budget too.
func TestGateWaitThrottleBlocksUntilTheCooldownExpires(t *testing.T) {
	gate := NewGate(0)
	// An unthrottled gate must not make anyone wait at all.
	if err := gate.WaitThrottle(context.Background()); err != nil {
		t.Fatalf("unthrottled wait: %v", err)
	}

	gate.Throttle(200 * time.Millisecond)

	ctx := context.Background()
	start := time.Now()
	if err := gate.WaitThrottle(ctx); err != nil {
		t.Fatalf("throttled wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("WaitThrottle returned after %s, want it held for the advertised cooldown", elapsed)
	}
	// And the pause is over, so it stops waiting.
	if err := gate.WaitThrottle(ctx); err != nil {
		t.Fatalf("post-throttle wait: %v", err)
	}
}

// A rate limit has to slow in-flight work, not just the next song to start. With
// the ceiling spent, the throttle still holds the caller: that is the whole reason
// the run checks it before each lookup rather than once per song.
func TestGateWaitThrottleIsIndependentOfTheCeiling(t *testing.T) {
	// 600/min is one unit every 100ms, so a naive gate would make the caller wait
	// about that long whether or not a cooldown was active.
	gate := NewGate(600)
	ctx := context.Background()
	if err := gate.Wait(ctx); err != nil {
		t.Fatalf("first wait: %v", err)
	}

	// With no throttle the caller gets straight through, proving WaitThrottle does
	// not consume the steady ceiling.
	start := time.Now()
	if err := gate.WaitThrottle(ctx); err != nil {
		t.Fatalf("unthrottled wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("WaitThrottle took %s with no cooldown active, want it not to spend the ceiling", elapsed)
	}

	gate.Throttle(150 * time.Millisecond)
	start = time.Now()
	if err := gate.WaitThrottle(ctx); err != nil {
		t.Fatalf("throttled wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("WaitThrottle took %s after a rate limit, want it held for the cooldown", elapsed)
	}
}

func TestGateWaitThrottleHonorsContextCancellation(t *testing.T) {
	gate := NewGate(0)
	gate.Throttle(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := gate.WaitThrottle(ctx); err == nil {
		t.Fatal("expected WaitThrottle to return the context error")
	}
}

// A longer advertised cooldown wins over the growing backoff, because the provider
// said how long it needs and guessing shorter would just walk back into the wall.
func TestGateThrottlePrefersTheAdvertisedCooldown(t *testing.T) {
	gate := NewGate(0)
	gate.Throttle(5 * time.Second)
	if pause := gate.pause(); pause < 4*time.Second {
		t.Errorf("pause = %s, want at least the advertised 5s", pause)
	}
	if _, pauses := gate.Stats(); pauses != 0 {
		t.Errorf("pauses = %d, want 0: a pause is only counted when it holds a caller back", pauses)
	}
}
