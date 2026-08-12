package wechatbot

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

var (
	// ErrAlreadyRunning is returned when Run is called while the bot is active.
	ErrAlreadyRunning = errors.New("wechatbot: already running")
	// ErrNoMessageHandler is returned when Run starts without a configured handler.
	ErrNoMessageHandler = errors.New("wechatbot: no message handler configured")
)

// MessageAction controls whether an inbound delivery is acknowledged.
type MessageAction uint8

const (
	// MessageAck marks the message as successfully handled.
	MessageAck MessageAction = iota + 1
	// MessageRetry leaves the message and cursor uncommitted and stops Run with an error.
	MessageRetry
	// MessageDrop intentionally consumes the message without retrying it.
	MessageDrop
)

// MessageResult is the explicit outcome of handling one inbound message.
type MessageResult struct {
	Action MessageAction
	Err    error
}

// AckMessage acknowledges a successfully handled message.
func AckMessage() MessageResult { return MessageResult{Action: MessageAck} }

// RetryMessage requests redelivery. Run returns the supplied error after leaving
// both the replay key and batch cursor uncommitted.
func RetryMessage(err error) MessageResult {
	if err == nil {
		err = errors.New("message handler requested retry")
	}
	return MessageResult{Action: MessageRetry, Err: err}
}

// DropMessage intentionally consumes a message. err is retained as the reason
// written to the bot log; it does not turn the drop into a runtime failure.
func DropMessage(err error) MessageResult {
	return MessageResult{Action: MessageDrop, Err: err}
}

// MessageHandler handles one inbound message and returns its delivery outcome.
// Implementations must honor ctx cancellation so Run can stop cleanly.
type MessageHandler interface {
	HandleMessage(ctx context.Context, msg *IncomingMessage) MessageResult
}

// MessageHandlerFunc adapts a function to MessageHandler.
type MessageHandlerFunc func(ctx context.Context, msg *IncomingMessage) MessageResult

// HandleMessage calls f.
func (f MessageHandlerFunc) HandleMessage(ctx context.Context, msg *IncomingMessage) MessageResult {
	return f(ctx, msg)
}

// Middleware wraps a MessageHandler. Middleware is applied in registration
// order, so the first registered middleware is the outermost wrapper.
type Middleware func(next MessageHandler) MessageHandler

// Handle sets the single inbound message handler. Replacing the handler while
// Run is active takes effect on the next Run.
func (b *Bot) Handle(handler MessageHandler) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

// Use adds a middleware to the incoming message pipeline. Middleware added
// while Run is active takes effect on the next Run.
func (b *Bot) Use(mw Middleware) {
	b.mu.Lock()
	b.middlewares = append(b.middlewares, mw)
	b.mu.Unlock()
}

// Hooks returns the bot's lifecycle hook registry for extension.
func (b *Bot) Hooks() *LifecycleHooks {
	return &b.hooks
}

func (b *Bot) configuredHandler() (MessageHandler, error) {
	b.mu.Lock()
	handler := b.handler
	middlewares := append([]Middleware(nil), b.middlewares...)
	b.mu.Unlock()
	return composeHandler(handler, middlewares)
}

func composeHandler(handler MessageHandler, middlewares []Middleware) (configured MessageHandler, err error) {
	if handler == nil {
		return nil, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			configured = nil
			err = fmt.Errorf("configure message middleware: %v", recovered)
		}
	}()
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		handler = middlewares[i](handler)
		if handler == nil {
			return nil, fmt.Errorf("configure message middleware %d: returned nil handler", i)
		}
	}
	return handler, nil
}

func (b *Bot) invokeHandler(ctx context.Context, handler MessageHandler, msg *IncomingMessage) (result MessageResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			b.log("error", "Message handler panic: %v\n%s", recovered, debug.Stack())
			result = RetryMessage(fmt.Errorf("message handler panic: %v", recovered))
		}
	}()
	return handler.HandleMessage(ctx, msg)
}

func validateMessageResult(result MessageResult) error {
	switch result.Action {
	case MessageAck:
		if result.Err != nil {
			return fmt.Errorf("message handler acknowledged with error: %w", result.Err)
		}
		return nil
	case MessageDrop:
		return nil
	case MessageRetry:
		if result.Err != nil {
			return result.Err
		}
		return errors.New("message handler requested retry")
	default:
		return fmt.Errorf("invalid message action %d", result.Action)
	}
}
