package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Icatme/wechatbot-go/internal/auth"
	"github.com/Icatme/wechatbot-go/internal/protocol"
)

const updateQueueCapacity = 16

type updateBatch struct {
	messages          []json.RawMessage
	cursor            string
	sessionGeneration uint64
}

type authenticatedSession struct {
	creds      *auth.Credentials
	generation uint64
}

type handlerSessionGenerationKey struct{}

func (b *Bot) beginRun(ctx context.Context) (authenticatedSession, MessageHandler, context.Context, context.CancelFunc, error) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, ErrAlreadyRunning
	}
	if state := b.reauth; state != nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, waitReauthState(state)
	}
	if b.creds == nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, fmt.Errorf("%w; call Login() first", ErrNotLoggedIn)
	}
	if b.handler == nil {
		b.mu.Unlock()
		return authenticatedSession{}, nil, nil, nil, ErrNoMessageHandler
	}

	session := authenticatedSession{creds: b.creds, generation: b.sessionGeneration}
	b.sessionInitialized = true
	handler := b.handler
	middlewares := append([]Middleware(nil), b.middlewares...)
	pollCtx, cancel := context.WithCancel(ctx)
	b.running = true
	b.cancelPoll = cancel
	b.mu.Unlock()

	handler, err := composeHandler(handler, middlewares)
	if err != nil {
		b.finishRun(cancel)
		return authenticatedSession{}, nil, nil, nil, err
	}
	return session, handler, pollCtx, cancel, nil
}

func (b *Bot) finishRun(cancel context.CancelFunc) {
	cancel()
	b.mu.Lock()
	b.running = false
	b.cancelPoll = nil
	b.mu.Unlock()
}

// Run starts the long-poll loop. It blocks until Stop is called, ctx is
// cancelled, or a delivery requests retry. Only one Run may be active.
func (b *Bot) Run(ctx context.Context) (runErr error) {
	session, handler, pollCtx, cancel, err := b.beginRun(ctx)
	if err != nil {
		return err
	}
	defer b.finishRun(cancel)
	defer func() {
		runErr = b.normalizeSessionChangeError(session.generation, runErr)
	}()

	b.log("info", "Long-poll loop started")
	if loadErr := b.cursorStore.Load(); loadErr != nil {
		return fmt.Errorf("load cursor state: %w", loadErr)
	}
	if err := b.replayStore.Load(); err != nil {
		return fmt.Errorf("load replay state: %w", err)
	}
	notifyStartToken, notifyStartErr := b.authenticatedRequest(pollCtx, session.generation, func(requestContext context.Context, baseURL, token string) error {
		return b.client.NotifyStart(requestContext, baseURL, token)
	})
	if notifyStartToken == "" {
		return notifyStartErr
	}
	if err := notifyStartErr; err != nil {
		err = b.handleAuthenticatedErrorForSession(session.generation, notifyStartToken, err)
		if errors.Is(err, ErrReauthRequired) {
			return err
		}
		b.log("warn", "NotifyStart failed: %v", err)
	}
	defer func() {
		stopToken, stopErr := b.authenticatedRequest(context.Background(), session.generation, func(requestContext context.Context, baseURL, token string) error {
			return b.client.NotifyStop(requestContext, baseURL, token)
		})
		if stopToken == "" {
			return
		}
		if stopToken != session.creds.Token {
			return
		}
		if stopErr != nil {
			stopErr = b.handleAuthenticatedErrorForSession(session.generation, stopToken, stopErr)
			if errors.Is(stopErr, ErrReauthRequired) {
				runErr = errors.Join(runErr, stopErr)
				return
			}
			b.log("warn", "NotifyStop failed: %v", stopErr)
		}
	}()

	batches := make(chan updateBatch, updateQueueCapacity)
	processorErrCh := make(chan error, 1)
	processorDone := make(chan struct{})
	go func() {
		defer close(processorDone)
		if err := b.processUpdateBatches(pollCtx, handler, batches); err != nil {
			processorErrCh <- err
			cancel()
		}
	}()
	defer func() {
		cancel()
		<-processorDone
	}()

	processorErr := func() error {
		select {
		case err := <-processorErrCh:
			return fmt.Errorf("process updates: %w", err)
		default:
			return nil
		}
	}
	sessionErr := func() error {
		return b.sessionError(session.generation)
	}
	stopResult := func() error {
		cancel()
		<-processorDone
		result := processorErr()
		if err := sessionErr(); err != nil {
			result = errors.Join(result, err)
		}
		if result == nil {
			b.log("info", "Long-poll loop stopped")
		}
		return result
	}

	retryDelay := time.Second
	pollTimeout := 45 * time.Second
	pollCursor := b.cursorStore.Get()

	for {
		if err := processorErr(); err != nil {
			return errors.Join(err, sessionErr())
		}
		select {
		case <-pollCtx.Done():
			return stopResult()
		default:
		}

		updates, requestToken, err := authenticatedRequestResult(b, pollCtx, session.generation, func(requestContext context.Context, baseURL, token string) (*protocol.GetUpdatesResponse, error) {
			return b.client.GetUpdates(requestContext, baseURL, token, pollCursor, pollTimeout)
		})
		if requestToken == "" {
			return errors.Join(err, stopResult())
		}
		if err != nil {
			err = b.handleAuthenticatedErrorForSession(session.generation, requestToken, err)
			if errors.Is(err, ErrReauthRequired) {
				return errors.Join(err, stopResult())
			}
			if pollCtx.Err() != nil {
				return stopResult()
			}

			b.reportError(err)
			timer := time.NewTimer(retryDelay)
			select {
			case <-pollCtx.Done():
				timer.Stop()
				return stopResult()
			case <-timer.C:
			}
			retryDelay = min(retryDelay*2, 10*time.Second)
			continue
		}

		if updates.LongPollingTimeoutMs > 0 {
			pollTimeout = time.Duration(updates.LongPollingTimeoutMs) * time.Millisecond
		}
		retryDelay = time.Second

		nextCursor := pollCursor
		if updates.GetUpdatesBuf != "" {
			nextCursor = updates.GetUpdatesBuf
		}
		if len(updates.Msgs) > 0 || nextCursor != pollCursor {
			batch := updateBatch{messages: updates.Msgs, cursor: nextCursor, sessionGeneration: session.generation}
			select {
			case batches <- batch:
				pollCursor = nextCursor
			case err := <-processorErrCh:
				return errors.Join(fmt.Errorf("process updates: %w", err), sessionErr())
			case <-pollCtx.Done():
				return stopResult()
			}
		}
	}
}

func (b *Bot) processUpdateBatches(ctx context.Context, handler MessageHandler, batches <-chan updateBatch) error {
	dispatcher := newKeyedDispatcher(ctx, b, handler, b.opts.MaxConcurrentHandlers)
	for {
		select {
		case <-ctx.Done():
			return dispatcher.wait()
		case <-dispatcher.failure():
			return dispatcher.wait()
		case batch, ok := <-batches:
			if !ok {
				return dispatcher.drain()
			}
			if err := dispatcher.submit(batch); err != nil {
				return dispatcher.wait()
			}
		}
	}
}

func (b *Bot) processUpdateBatch(ctx context.Context, handler MessageHandler, batch updateBatch) error {
	for _, rawMsg := range batch.messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		wire, err := b.decodeWireMessage(rawMsg)
		if err != nil {
			return err
		}
		if err := b.processWireMessage(ctx, handler, batch.sessionGeneration, wire); err != nil {
			return err
		}
	}
	if batch.cursor != "" && batch.cursor != b.cursorStore.Get() {
		if err := b.cursorStore.Set(batch.cursor); err != nil {
			return fmt.Errorf("commit cursor: %w", err)
		}
	}
	return nil
}

func (b *Bot) decodeWireMessage(rawMsg json.RawMessage) (*WireMessage, error) {
	var wire WireMessage
	if err := json.Unmarshal(rawMsg, &wire); err != nil {
		return nil, fmt.Errorf("decode incoming message: %w", err)
	}
	return &wire, nil
}

func (b *Bot) processWireMessage(ctx context.Context, handler MessageHandler, sessionGeneration uint64, wire *WireMessage) error {
	if err := b.validateDeliverySession(sessionGeneration); err != nil {
		return err
	}
	keys := replayKeys(wire)
	// replayKeys orders aliases strongest-first. A weaker alias may collide on a
	// distinct delivery, so only the strongest identity present may suppress it.
	if len(keys) > 0 && b.replayStore.SeenAny(keys[0]) {
		b.log("debug", "Skipping replayed message with identity %s", keys[0])
		return nil
	}

	if err := b.rememberContextForSession(sessionGeneration, wire); err != nil {
		return err
	}
	incoming := b.parseMessage(wire)
	if incoming != nil {
		incoming.sessionGeneration = sessionGeneration
		incoming.sessionBound = true
		if err := b.hooks.AfterReceive.Run(incoming); err != nil {
			return fmt.Errorf("AfterReceive hook failed: %w", err)
		}
		handlerCtx := context.WithValue(ctx, handlerSessionGenerationKey{}, sessionGeneration)
		result := b.invokeHandler(handlerCtx, handler, incoming)
		if err := validateMessageResult(result); err != nil {
			return fmt.Errorf("handle message: %w", err)
		}
		if result.Action == MessageDrop && result.Err != nil {
			b.log("warn", "Message intentionally dropped: %v", result.Err)
		}
	}
	if len(keys) > 0 {
		if err := b.replayStore.CommitAll(keys...); err != nil {
			return fmt.Errorf("commit replay identities %v: %w", keys, err)
		}
	}
	return nil
}

func replayKeys(wire *WireMessage) []string {
	peer := peerUserID(wire)
	if peer == "" {
		return nil
	}

	prefix := "peer:" + strconv.Itoa(len(peer)) + ":" + peer + ":"
	keys := make([]string, 0, 3)
	if wire.MessageID != 0 {
		keys = append(keys, prefix+"message:"+strconv.FormatInt(wire.MessageID, 10))
	}
	if wire.ClientID != "" {
		keys = append(keys, prefix+"client:"+wire.ClientID)
	}
	if wire.Seq != 0 {
		keys = append(keys, prefix+"seq:"+strconv.FormatInt(wire.Seq, 10))
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

func peerUserID(wire *WireMessage) string {
	if wire == nil {
		return ""
	}
	if wire.MessageType == MessageTypeBot {
		return wire.ToUserID
	}
	if wire.MessageType == MessageTypeUser {
		return wire.FromUserID
	}
	return ""
}

// Stop gracefully stops the poll loop.
func (b *Bot) Stop() {
	b.mu.Lock()
	cancel := b.cancelPoll
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
