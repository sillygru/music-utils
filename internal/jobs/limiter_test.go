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
