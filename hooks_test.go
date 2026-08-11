package wechatbot

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestMiddlewareStopsPipeline(t *testing.T) {
	bot := New(Options{})
	called := false
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		called = true
		return AckMessage()
	}))
	bot.Use(func(MessageHandler) MessageHandler {
		return MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
			return DropMessage(errors.New("filtered"))
		})
	})

	handler, err := bot.configuredHandler()
	if err != nil {
		t.Fatalf("configure handler: %v", err)
	}
	result := handler.HandleMessage(context.Background(), &IncomingMessage{})
	if result.Action != MessageDrop {
		t.Fatalf("middleware action = %d, want drop", result.Action)
	}
	if called {
		t.Fatal("handler should not be called when middleware drops the message")
	}
}

func TestMiddlewareRegistrationOrder(t *testing.T) {
	bot := New(Options{})
	var calls []string
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		calls = append(calls, "handler")
		return AckMessage()
	}))
	register := func(name string) {
		bot.Use(func(next MessageHandler) MessageHandler {
			return MessageHandlerFunc(func(ctx context.Context, msg *IncomingMessage) MessageResult {
				calls = append(calls, name+":before")
				result := next.HandleMessage(ctx, msg)
				calls = append(calls, name+":after")
				return result
			})
		})
	}
	register("first")
	register("second")

	handler, err := bot.configuredHandler()
	if err != nil {
		t.Fatalf("configure handler: %v", err)
	}
	result := handler.HandleMessage(context.Background(), &IncomingMessage{})
	if result.Action != MessageAck {
		t.Fatalf("middleware action = %d, want ack", result.Action)
	}
	want := []string{"first:before", "second:before", "handler", "second:after", "first:after"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
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
	registry.Register(func(int) error {
		return errors.New("stop")
	})
	registry.Register(func(int) error {
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
