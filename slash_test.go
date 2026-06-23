package wechatbot

import (
	"context"
	"testing"
)

func TestCommandRegistryHandle(t *testing.T) {
	reg := NewCommandRegistry("/")
	called := false
	var receivedArgs string
	reg.Register("ping", func(ctx context.Context, msg *IncomingMessage, args string) bool {
		called = true
		receivedArgs = args
		return true
	})

	handled := reg.Handle(context.Background(), &IncomingMessage{Text: "/ping hello world"})
	if !handled {
		t.Fatal("expected command to be handled")
	}
	if !called {
		t.Fatal("expected handler to be called")
	}
	if receivedArgs != "hello world" {
		t.Fatalf("expected args 'hello world', got %q", receivedArgs)
	}
}

func TestCommandRegistryNoMatch(t *testing.T) {
	reg := NewCommandRegistry("/")
	reg.Register("ping", func(ctx context.Context, msg *IncomingMessage, args string) bool {
		return true
	})

	handled := reg.Handle(context.Background(), &IncomingMessage{Text: "hello"})
	if handled {
		t.Fatal("expected no command match")
	}
}

func TestCommandRegistryCaseInsensitive(t *testing.T) {
	reg := NewCommandRegistry("/")
	called := false
	reg.Register("Ping", func(ctx context.Context, msg *IncomingMessage, args string) bool {
		called = true
		return true
	})

	reg.Handle(context.Background(), &IncomingMessage{Text: "/PING"})
	if !called {
		t.Fatal("expected case-insensitive match")
	}
}

func TestCommandRegistryEmptyPrefix(t *testing.T) {
	reg := NewCommandRegistry("")
	reg.Register("ping", func(ctx context.Context, msg *IncomingMessage, args string) bool {
		return true
	})

	if !reg.Handle(context.Background(), &IncomingMessage{Text: "/ping"}) {
		t.Fatal("expected default / prefix")
	}
}
