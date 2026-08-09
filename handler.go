package wechatbot

import (
	"context"
	"errors"
	"fmt"
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
