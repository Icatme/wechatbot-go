package wechatbot

import (
	"errors"
	"testing"
)

func TestMiddlewareStopsPipeline(t *testing.T) {
	bot := New(Options{})
	called := false
	bot.Use(func(msg *IncomingMessage) bool {
		return false
	})
	bot.OnMessage(func(msg *IncomingMessage) {
		called = true
	})
	bot.runMiddleware(&IncomingMessage{})
	if called {
		t.Fatal("handler should not be called when middleware returns false")
	}
}

func TestMiddlewareAllowsPipeline(t *testing.T) {
	bot := New(Options{})
	called := false
	bot.Use(func(msg *IncomingMessage) bool {
		return true
	})
	bot.OnMessage(func(msg *IncomingMessage) {
		called = true
	})
	if !bot.runMiddleware(&IncomingMessage{}) {
		t.Fatal("middleware should allow message")
	}
	if called {
		t.Fatal("runMiddleware should not invoke handlers")
	}
}

func TestHookRegistryRun(t *testing.T) {
	var registry HookRegistry[int]
	var sum int
	registry.Register(func(n int) error {
		sum += n
		return nil
	})
	registry.Register(func(n int) error {
		sum += n * 2
		return nil
	})
	if err := registry.Run(3); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sum != 9 {
		t.Fatalf("expected 9, got %d", sum)
	}
}

func TestHookRegistryStopsOnError(t *testing.T) {
	var registry HookRegistry[int]
	called := false
	registry.Register(func(n int) error {
		return errors.New("stop")
	})
	registry.Register(func(n int) error {
		called = true
		return nil
	})
	if err := registry.Run(0); err == nil {
		t.Fatal("expected error")
	}
	if called {
		t.Fatal("second hook should not run after error")
	}
}

func TestBeforeSendHookMutatesContent(t *testing.T) {
	bot := New(Options{})
	bot.Hooks().BeforeSend.Register(func(c *SendContent) error {
		c.Text = "hooked"
		return nil
	})
	content := SendText("original")
	if err := bot.hooks.BeforeSend.Run(&content); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content.Text != "hooked" {
		t.Fatalf("expected hooked, got %s", content.Text)
	}
}
