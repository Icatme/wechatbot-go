package session

import (
	"testing"
	"time"
)

func TestGuardNotPausedByDefault(t *testing.T) {
	g := NewGuard()
	if g.IsPaused() {
		t.Fatal("expected not paused")
	}
	if g.Remaining() != 0 {
		t.Fatalf("expected 0 remaining, got %v", g.Remaining())
	}
}

func TestGuardPause(t *testing.T) {
	g := NewGuard()
	g.Pause()
	if !g.IsPaused() {
		t.Fatal("expected paused")
	}
	if g.Remaining() <= 0 {
		t.Fatal("expected positive remaining")
	}
}

func TestGuardReset(t *testing.T) {
	g := NewGuard()
	g.Pause()
	g.Reset()
	if g.IsPaused() {
		t.Fatal("expected not paused after reset")
	}
}

func TestGuardExpiredPauseAutoClears(t *testing.T) {
	g := NewGuard()
	g.mu.Lock()
	g.pausedUntil = time.Now().Add(-time.Second)
	g.mu.Unlock()

	if g.IsPaused() {
		t.Fatal("expected expired pause to be cleared")
	}
	if g.Remaining() != 0 {
		t.Fatalf("expected 0 remaining after expiry, got %v", g.Remaining())
	}
}

func TestGuardAssertActive(t *testing.T) {
	g := NewGuard()
	g.Pause()
	if err := g.AssertActive(); err == nil {
		t.Fatal("expected error when paused")
	}
	g.Reset()
	if err := g.AssertActive(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
