// Package session provides session lifecycle guards for the WeChat iLink bot.
package session

import (
	"fmt"
	"sync"
	"time"
)

const (
	// PauseDuration is how long to back off after receiving a session expired (-14) error.
	PauseDuration = 60 * time.Minute
)

// Guard tracks whether a bot session is currently paused due to expiry.
type Guard struct {
	mu        sync.RWMutex
	pausedUntil time.Time
}

// NewGuard creates a new session guard.
func NewGuard() *Guard {
	return &Guard{}
}

// Pause marks the session as paused for PauseDuration.
func (g *Guard) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pausedUntil = time.Now().Add(PauseDuration)
}

// IsPaused reports whether the session is still within its pause window.
func (g *Guard) IsPaused() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.pausedUntil.IsZero() {
		return false
	}
	if time.Now().Before(g.pausedUntil) {
		return true
	}
	// Auto-clear expired pause.
	g.pausedUntil = time.Time{}
	return false
}

// Remaining returns the duration until the pause expires, or 0 if not paused.
func (g *Guard) Remaining() time.Duration {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.pausedUntil.IsZero() {
		return 0
	}
	remaining := g.pausedUntil.Sub(time.Now())
	if remaining <= 0 {
		g.pausedUntil = time.Time{}
		return 0
	}
	return remaining
}

// AssertActive returns an error if the session is currently paused.
func (g *Guard) AssertActive() error {
	if g.IsPaused() {
		return fmt.Errorf("session paused, %v remaining", g.Remaining().Round(time.Second))
	}
	return nil
}

// Reset clears any active pause.
func (g *Guard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pausedUntil = time.Time{}
}
