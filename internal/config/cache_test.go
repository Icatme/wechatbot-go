package config

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/internal/protocol"
)

type fakeProvider struct {
	calls     int
	ticket    string
	err       error
}

func (f *fakeProvider) GetConfig(ctx context.Context, baseURL, token, userID, contextToken string) (*protocol.GetConfigResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &protocol.GetConfigResponse{TypingTicket: f.ticket}, nil
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
