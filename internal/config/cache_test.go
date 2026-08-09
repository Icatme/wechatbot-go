package config

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

type fakeProvider struct {
	calls  int
	ticket string
	err    error
}

type blockingProvider struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	ticket  string
}

func (f *fakeProvider) GetConfig(ctx context.Context, baseURL, token, userID, contextToken string) (*protocol.GetConfigResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &protocol.GetConfigResponse{TypingTicket: f.ticket}, nil
}

func (p *blockingProvider) GetConfig(context.Context, string, string, string, string) (*protocol.GetConfigResponse, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
	}
	<-p.release
	return &protocol.GetConfigResponse{TypingTicket: p.ticket}, nil
}

func TestCacheFetchesOnFirstCall(t *testing.T) {
	fake := &fakeProvider{ticket: "ticket-1"}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: fake})

	cfg, err := c.GetForUser(context.Background(), "user1", "ctx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TypingTicket != "ticket-1" {
		t.Fatalf("expected ticket-1, got %s", cfg.TypingTicket)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 call, got %d", fake.calls)
	}
}

func TestCacheReturnsCachedValueWithoutCallingAgain(t *testing.T) {
	fake := &fakeProvider{ticket: "ticket-1"}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: fake})

	_, _ = c.GetForUser(context.Background(), "user1", "ctx-1")
	cfg, err := c.GetForUser(context.Background(), "user1", "ctx-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TypingTicket != "ticket-1" {
		t.Fatalf("expected ticket-1, got %s", cfg.TypingTicket)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 call, got %d", fake.calls)
	}
}

func TestCacheConcurrentFirstGetForUserFetchesOnce(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)

	provider := &blockingProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		ticket:  "ticket-1",
	}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: provider})

	type result struct {
		config CachedConfig
		err    error
	}
	results := make(chan result, 3)
	get := func(contextToken string) {
		config, err := c.GetForUser(context.Background(), "user1", contextToken)
		results <- result{config: config, err: err}
	}

	// Queue both cold-cache calls at the cache lock. With one P, Gosched lets
	// both callers reach that lock before it is released.
	c.mu.Lock()
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for _, contextToken := range []string{"ctx-1", "ctx-2"} {
		go func() {
			ready.Done()
			<-start
			get(contextToken)
		}()
	}
	ready.Wait()
	close(start)
	runtime.Gosched()
	c.mu.Unlock()

	<-provider.entered

	// Add a follower while the first fetch is blocked. This also exercises
	// reads of the shared entry while its fields are being populated.
	followerStarted := make(chan struct{})
	go func() {
		close(followerStarted)
		get("ctx-3")
	}()
	<-followerStarted
	runtime.Gosched()

	close(provider.release)
	for range 3 {
		result := <-results
		if result.err != nil {
			t.Fatalf("unexpected error: %v", result.err)
		}
		if result.config.TypingTicket != "ticket-1" {
			t.Fatalf("expected ticket-1, got %s", result.config.TypingTicket)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("expected one provider fetch for concurrent first calls, got %d", calls)
	}
}

func TestCacheBackoffOnError(t *testing.T) {
	fake := &fakeProvider{err: errors.New("network error")}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: fake})

	_, err := c.GetForUser(context.Background(), "user1", "ctx-1")
	if err == nil {
		t.Fatal("expected error")
	}

	// Second call within backoff should not trigger a new request.
	_, _ = c.GetForUser(context.Background(), "user1", "ctx-1")
	if fake.calls != 1 {
		t.Fatalf("expected 1 call during backoff, got %d", fake.calls)
	}
}

func TestCacheRetryAfterBackoff(t *testing.T) {
	fake := &fakeProvider{err: errors.New("network error")}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: fake})

	_, _ = c.GetForUser(context.Background(), "user1", "ctx-1")

	// Manually expire the entry to simulate backoff elapsed.
	c.mu.Lock()
	c.cache["user1"].nextFetchAt = time.Now().Add(-time.Second)
	c.mu.Unlock()

	_, err := c.GetForUser(context.Background(), "user1", "ctx-1")
	if err == nil {
		t.Fatal("expected error")
	}
	if fake.calls != 2 {
		t.Fatalf("expected 2 calls after backoff, got %d", fake.calls)
	}
}

func TestCacheClear(t *testing.T) {
	fake := &fakeProvider{ticket: "ticket-1"}
	c := NewCache(APIOpts{BaseURL: "https://example.com", Token: "tok", Client: fake})

	_, _ = c.GetForUser(context.Background(), "user1", "ctx-1")
	c.Clear()
	_, _ = c.GetForUser(context.Background(), "user1", "ctx-1")

	if fake.calls != 2 {
		t.Fatalf("expected 2 calls after clear, got %d", fake.calls)
	}
}
