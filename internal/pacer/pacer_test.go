package pacer

import (
	"context"
	"testing"
	"time"
)

func TestFirstCallIsNeverDelayed(t *testing.T) {
	p := New(time.Minute)
	started := time.Now()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("first call was delayed %v", elapsed)
	}
}

func TestSubsequentCallsAreSpaced(t *testing.T) {
	p := New(50 * time.Millisecond)
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	started := time.Now()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("second wait: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("second call was not spaced, elapsed %v", elapsed)
	}
}

func TestNilPacerNeverDelays(t *testing.T) {
	var p *Pacer
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("nil pacer wait: %v", err)
	}
}

func TestCanceledContextInterruptsWait(t *testing.T) {
	p := New(time.Hour)
	if err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Wait(ctx); err == nil {
		t.Fatal("expected canceled context to interrupt the wait")
	}
}
