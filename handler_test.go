package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Icatme/wechatbot-go/internal/auth"
)

func TestMessageResultValidation(t *testing.T) {
	retryErr := errors.New("retry")
	tests := []struct {
		name    string
		result  MessageResult
		wantErr error
	}{
		{name: "ack", result: AckMessage()},
		{name: "drop", result: DropMessage(errors.New("filtered"))},
		{name: "retry", result: RetryMessage(retryErr), wantErr: retryErr},
		{name: "zero value is invalid", result: MessageResult{}, wantErr: errors.New("invalid message action")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMessageResult(tc.result)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateMessageResult() error = %v", err)
				}
				return
			}
			if tc.name == "retry" {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("validateMessageResult() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr.Error()) {
				t.Fatalf("validateMessageResult() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
	ackErr := errors.New("ack mismatch")
	if err := validateMessageResult(MessageResult{Action: MessageAck, Err: ackErr}); !errors.Is(err, ackErr) {
		t.Fatalf("ack with error validation = %v, want %v", err, ackErr)
	}
}

func TestRetryLeavesReplayAndCursorUncommitted(t *testing.T) {
	bot := newHandlerTestBot(t)
	retryErr := errors.New("temporary failure")
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return RetryMessage(retryErr)
	})

	err := bot.processUpdateBatch(context.Background(), handler, handlerTestBatch(t, 101, "cursor-retry"))
	if !errors.Is(err, retryErr) {
		t.Fatalf("processUpdateBatch() error = %v, want %v", err, retryErr)
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want uncommitted", got)
	}
	if bot.replayStore.Seen(handlerReplayKey(101)) {
		t.Fatal("retry message was committed to replay store")
	}
}

func TestBatchRetryReplaysOnlyUncommittedMessages(t *testing.T) {
	bot := newHandlerTestBot(t)
	retryErr := errors.New("second message failed")
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	firstAttempt := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		switch msg.Raw.MessageID {
		case 201:
			firstCalls.Add(1)
			return AckMessage()
		case 202:
			secondCalls.Add(1)
			return RetryMessage(retryErr)
		default:
			return DropMessage(errors.New("unexpected message"))
		}
	})
	batch := updateBatch{
		messages: []json.RawMessage{
			handlerTestBatch(t, 201, "").messages[0],
			handlerTestBatch(t, 202, "").messages[0],
		},
		cursor: "cursor-batch",
	}

	if err := bot.processUpdateBatch(context.Background(), firstAttempt, batch); !errors.Is(err, retryErr) {
		t.Fatalf("first processUpdateBatch() error = %v, want %v", err, retryErr)
	}
	if !bot.replayStore.Seen(handlerReplayKey(201)) {
		t.Fatal("acknowledged prefix message was not committed")
	}
	if bot.replayStore.Seen(handlerReplayKey(202)) {
		t.Fatal("retry message was committed")
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want uncommitted", got)
	}

	secondAttempt := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		switch msg.Raw.MessageID {
		case 201:
			firstCalls.Add(1)
		case 202:
			secondCalls.Add(1)
		}
		return AckMessage()
	})
	if err := bot.processUpdateBatch(context.Background(), secondAttempt, batch); err != nil {
		t.Fatalf("second processUpdateBatch() error = %v", err)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("first message calls = %d, want 1", got)
	}
	if got := secondCalls.Load(); got != 2 {
		t.Fatalf("second message calls = %d, want 2", got)
	}
	if !bot.replayStore.Seen(handlerReplayKey(202)) {
		t.Fatal("retried message was not committed after Ack")
	}
	if got := bot.cursorStore.Get(); got != "cursor-batch" {
		t.Fatalf("cursor = %q, want cursor-batch", got)
	}
}

func TestDropCommitsReplayAndCursor(t *testing.T) {
	bot := newHandlerTestBot(t)
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return DropMessage(errors.New("unsupported input"))
	})

	if err := bot.processUpdateBatch(context.Background(), handler, handlerTestBatch(t, 102, "cursor-drop")); err != nil {
		t.Fatalf("processUpdateBatch() error = %v", err)
	}
	if got := bot.cursorStore.Get(); got != "cursor-drop" {
		t.Fatalf("cursor = %q, want cursor-drop", got)
	}
	if !bot.replayStore.Seen(handlerReplayKey(102)) {
		t.Fatal("dropped message was not committed to replay store")
	}
}

func TestAfterReceiveFailureLeavesDeliveryUncommitted(t *testing.T) {
	bot := newHandlerTestBot(t)
	hookErr := errors.New("hook failed")
	bot.Hooks().AfterReceive.Register(func(*IncomingMessage) error { return hookErr })
	var called atomic.Bool
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		called.Store(true)
		return AckMessage()
	})

	err := bot.processUpdateBatch(context.Background(), handler, handlerTestBatch(t, 103, "cursor-hook"))
	if !errors.Is(err, hookErr) {
		t.Fatalf("processUpdateBatch() error = %v, want %v", err, hookErr)
	}
	if called.Load() {
		t.Fatal("handler ran after AfterReceive failed")
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want uncommitted", got)
	}
	if bot.replayStore.Seen(handlerReplayKey(103)) {
		t.Fatal("hook failure was committed to replay store")
	}
}

func TestHandlerPanicRequestsRetry(t *testing.T) {
	bot := newHandlerTestBot(t)
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		panic("boom")
	})

	err := bot.processUpdateBatch(context.Background(), handler, handlerTestBatch(t, 104, "cursor-panic"))
	if err == nil || !strings.Contains(err.Error(), "message handler panic: boom") {
		t.Fatalf("processUpdateBatch() error = %v, want recovered panic", err)
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want uncommitted", got)
	}
	if bot.replayStore.Seen(handlerReplayKey(104)) {
		t.Fatal("panicked delivery was committed to replay store")
	}
}

func TestRunRequiresHandlerBeforeNetwork(t *testing.T) {
	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}

	err := bot.Run(context.Background())
	if !errors.Is(err, ErrNoMessageHandler) {
		t.Fatalf("Run() error = %v, want %v", err, ErrNoMessageHandler)
	}
}

func TestRunFailsClosedOnInvalidCursorState(t *testing.T) {
	dir := t.TempDir()
	cursorPath := filepath.Join(dir, "cursor.json")
	if err := os.WriteFile(cursorPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	bot := New(Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       cursorPath,
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	})
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))

	err := bot.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "load cursor state") {
		t.Fatalf("Run() error = %v, want cursor state failure", err)
	}
	bot.mu.Lock()
	running := bot.running
	bot.mu.Unlock()
	if running {
		t.Fatal("bot remained running after cursor load failed")
	}
}

func TestBeginRunRejectsConcurrentRunAndResetsState(t *testing.T) {
	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))

	_, _, _, firstCancel, err := bot.beginRun(context.Background())
	if err != nil {
		t.Fatalf("first beginRun() error = %v", err)
	}
	if _, _, _, _, err := bot.beginRun(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second beginRun() error = %v, want %v", err, ErrAlreadyRunning)
	}
	bot.finishRun(firstCancel)

	_, _, _, secondCancel, err := bot.beginRun(context.Background())
	if err != nil {
		t.Fatalf("beginRun() after finish error = %v", err)
	}
	bot.finishRun(secondCancel)
}

func TestStopCancelsHandlerContext(t *testing.T) {
	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	handlerStarted := make(chan struct{})
	bot.Handle(MessageHandlerFunc(func(ctx context.Context, _ *IncomingMessage) MessageResult {
		close(handlerStarted)
		<-ctx.Done()
		return RetryMessage(ctx.Err())
	}))

	_, handler, pollCtx, cancel, err := bot.beginRun(context.Background())
	if err != nil {
		t.Fatalf("beginRun() error = %v", err)
	}
	defer bot.finishRun(cancel)
	resultCh := make(chan MessageResult, 1)
	go func() {
		resultCh <- bot.invokeHandler(pollCtx, handler, &IncomingMessage{})
	}()
	waitForSignal(t, handlerStarted, "handler start")
	bot.Stop()

	select {
	case result := <-resultCh:
		if result.Action != MessageRetry || !errors.Is(result.Err, context.Canceled) {
			t.Fatalf("handler result = %+v, want retry with context cancellation", result)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not observe Stop cancellation")
	}
}

func TestRunPreservesHandlerOutcomeWhenHandlerStopsBot(t *testing.T) {
	retryErr := errors.New("retry after stop")
	tests := []struct {
		name      string
		result    func(context.Context) MessageResult
		wantErr   error
		committed bool
	}{
		{name: "ack", result: func(context.Context) MessageResult { return AckMessage() }, committed: true},
		{name: "retry", result: func(context.Context) MessageResult { return RetryMessage(retryErr) }, wantErr: retryErr},
		{name: "retry cancellation", result: func(ctx context.Context) MessageResult { return RetryMessage(ctx.Err()) }, wantErr: context.Canceled},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bot := newHandlerTestBot(t)
			messageID := int64(105 + i)
			cursor := fmt.Sprintf("cursor-stop-%d", i)
			raw := handlerTestBatch(t, messageID, cursor).messages[0]
			var polls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ilink/bot/msg/notifystart", "/ilink/bot/msg/notifystop":
					_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
				case "/ilink/bot/getupdates":
					if polls.Add(1) == 1 {
						_ = json.NewEncoder(w).Encode(map[string]interface{}{
							"ret":             0,
							"msgs":            []json.RawMessage{raw},
							"get_updates_buf": cursor,
						})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]int{"ret": 0})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			bot.client.HTTP = server.Client()
			bot.creds = &auth.Credentials{BaseURL: server.URL, Token: "token", AccountID: "bot-1"}
			bot.Handle(MessageHandlerFunc(func(ctx context.Context, _ *IncomingMessage) MessageResult {
				bot.Stop()
				return tc.result(ctx)
			}))

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := bot.Run(ctx)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Run() error = %v", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run() error = %v, want %v", err, tc.wantErr)
			}
			if got := bot.replayStore.Seen(handlerReplayKey(messageID)); got != tc.committed {
				t.Fatalf("replay committed = %v, want %v", got, tc.committed)
			}
			wantCursor := ""
			if tc.committed {
				wantCursor = cursor
			}
			if got := bot.cursorStore.Get(); got != wantCursor {
				t.Fatalf("cursor = %q, want %q", got, wantCursor)
			}
		})
	}
}

func TestMiddlewareConfigurationDoesNotHoldBotLock(t *testing.T) {
	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	base := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	})
	bot.Handle(base)
	bot.Use(func(next MessageHandler) MessageHandler {
		bot.Handle(base)
		return next
	})

	done := make(chan error, 1)
	go func() {
		_, _, _, cancel, err := bot.beginRun(context.Background())
		if err == nil {
			bot.finishRun(cancel)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("beginRun() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("middleware configuration deadlocked on Bot lock")
	}
}

func TestMiddlewarePanicResetsRunState(t *testing.T) {
	bot := New()
	bot.creds = &auth.Credentials{BaseURL: "https://example.com", Token: "token"}
	bot.Handle(MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}))
	bot.Use(func(MessageHandler) MessageHandler { panic("bad middleware") })

	if _, _, _, _, err := bot.beginRun(context.Background()); err == nil || !strings.Contains(err.Error(), "bad middleware") {
		t.Fatalf("beginRun() error = %v, want middleware panic", err)
	}
	bot.mu.Lock()
	running := bot.running
	bot.mu.Unlock()
	if running {
		t.Fatal("bot remained running after middleware configuration failed")
	}
}

func newHandlerTestBot(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	bot := New(Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       filepath.Join(dir, "cursor.json"),
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	})
	if err := bot.replayStore.Load(); err != nil {
		t.Fatalf("load replay store: %v", err)
	}
	return bot
}

func handlerTestBatch(t *testing.T, messageID int64, cursor string) updateBatch {
	t.Helper()
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    messageID,
		FromUserID:   "user-1",
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: "context-1",
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	})
	return updateBatch{messages: []json.RawMessage{raw}, cursor: cursor}
}

func handlerReplayKey(messageID int64) string {
	return replayKeys(&WireMessage{
		MessageID:   messageID,
		FromUserID:  "user-1",
		MessageType: MessageTypeUser,
	})[0]
}
