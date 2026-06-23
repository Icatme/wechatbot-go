// Package config provides cached access to WeChat iLink bot config.
package config

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

const (
	ttlMS               = 24 * 60 * 60 * 1000
	initialRetryDelayMS = 2 * 1000
	maxRetryDelayMS     = 60 * 60 * 1000
)

// Provider describes the minimal API surface needed to fetch config.
type Provider interface {
	GetConfig(ctx context.Context, baseURL, token, userID, contextToken string) (*protocol.GetConfigResponse, error)
}

// entry holds cached config and retry metadata for one user.
type entry struct {
	config        CachedConfig
	everSucceeded bool
	nextFetchAt   time.Time
	retryDelay    time.Duration
}

// CachedConfig is the subset of getconfig fields we cache.
type CachedConfig struct {
	TypingTicket string
}

// Cache provides per-user getConfig caching with TTL and exponential backoff.
type Cache struct {
	apiOpts APIOpts
	cache   map[string]*entry
	mu      sync.Mutex
}

// APIOpts holds the parameters needed to call getConfig.
type APIOpts struct {
	BaseURL string
	Token   string
	Client  Provider
}

// NewCache creates a new config cache.
func NewCache(opts APIOpts) *Cache {
	return &Cache{
		apiOpts: opts,
		cache:   make(map[string]*entry),
	}
}

// GetForUser returns the cached config for a user, fetching if necessary.
func (c *Cache) GetForUser(ctx context.Context, userID, contextToken string) (CachedConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	e, exists := c.cache[userID]
	shouldFetch := !exists || now.After(e.nextFetchAt)

	if !shouldFetch {
		return e.config, nil
	}

	if e == nil {
		e = &entry{}
		c.cache[userID] = e
	}

	resp, err := c.apiOpts.Client.GetConfig(ctx, c.apiOpts.BaseURL, c.apiOpts.Token, userID, contextToken)
	if err != nil {
		e.retryDelay = min(e.retryDelay*2, maxRetryDelayMS*time.Millisecond)
		if e.retryDelay == 0 {
			e.retryDelay = initialRetryDelayMS * time.Millisecond
		}
		e.nextFetchAt = now.Add(e.retryDelay)
		return e.config, fmt.Errorf("getConfig failed: %w", err)
	}

	e.config = CachedConfig{TypingTicket: resp.TypingTicket}
	e.everSucceeded = true
	e.retryDelay = initialRetryDelayMS * time.Millisecond
	// Randomize refresh within 24h to avoid thundering herd.
	e.nextFetchAt = now.Add(time.Duration(rand.Intn(ttlMS)) * time.Millisecond)
	return e.config, nil
}

// Clear removes all cached entries.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]*entry)
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
