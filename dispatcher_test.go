package wechatbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestConversationKeyUsesPeerUserID(t *testing.T) {
	tests := []struct {
		name string
		wire WireMessage
		want string
	}{
		{name: "user message", wire: WireMessage{MessageType: MessageTypeUser, FromUserID: "alice", ToUserID: "bot"}, want: "user:alice"},
		{name: "bot message", wire: WireMessage{MessageType: MessageTypeBot, FromUserID: "bot", ToUserID: "alice"}, want: "user:alice"},
		{name: "missing peer", wire: WireMessage{MessageType: MessageTypeUser}, want: fallbackConversationKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationKey(&tc.wire); got != tc.want {
				t.Fatalf("conversationKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyedDispatcherSerializesConversationAcrossBatches(t *testing.T) {
	bot := newDispatcherTestBot(t)
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	thirdStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		switch msg.Raw.MessageID {
		case 301:
			close(firstStarted)
			<-releaseFirst
		case 302:
			close(secondStarted)
			<-releaseSecond
		case 303:
			close(thirdStarted)
		}
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 301, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 302, "alice", "cursor-2")); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, firstStarted, "first same-conversation handler")
	select {
	case <-secondStarted:
		t.Fatal("second same-conversation handler started before the first completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	waitForSignal(t, secondStarted, "second same-conversation handler")
	if err := dispatcher.submit(dispatcherTestBatch(t, 303, "alice", "cursor-3")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-thirdStarted:
		t.Fatal("tail cleanup let the third handler bypass the second")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSecond)
	waitForSignal(t, thirdStarted, "third same-conversation handler")
	waitForCondition(t, "third replay commit", func() bool {
		return bot.replayStore.Seen(dispatcherReplayKey(303, "alice"))
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
	if got := bot.cursorStore.Get(); got != "cursor-3" {
		t.Fatalf("cursor = %q, want cursor-3", got)
	}
}

func TestEmptyBatchesAdvanceCursorFrontier(t *testing.T) {
	bot := newDispatcherTestBot(t)
	dispatcher := newKeyedDispatcher(context.Background(), bot, MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 2)
	for _, cursor := range []string{"cursor-1", "", "cursor-3"} {
		if err := dispatcher.submit(updateBatch{cursor: cursor}); err != nil {
			t.Fatal(err)
		}
	}
	if got := bot.cursorStore.Get(); got != "cursor-3" {
		t.Fatalf("cursor = %q, want cursor-3", got)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 304, "alice", "")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(updateBatch{cursor: "cursor-5"}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "non-empty empty-cursor frontier", func() bool {
		return bot.replayStore.Seen(dispatcherReplayKey(304, "alice")) && bot.cursorStore.Get() == "cursor-5"
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
}

func TestCanceledDispatcherRejectsEmptyCursorBatch(t *testing.T) {
	bot := newDispatcherTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := newKeyedDispatcher(ctx, bot, MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 1)
	cancel()
	if err := dispatcher.submit(updateBatch{cursor: "cursor-canceled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("submit error = %v, want context cancellation", err)
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want unchanged", got)
	}
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
}

func TestMalformedMessageFailsDispatcherWithoutAdvancingCursor(t *testing.T) {
	bot := newDispatcherTestBot(t)
	if err := bot.cursorStore.Set("cursor-before"); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}
	dispatcher := newKeyedDispatcher(context.Background(), bot, MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 1)

	err := dispatcher.submit(updateBatch{
		messages: []json.RawMessage{json.RawMessage(`{"message_id":"invalid"}`)},
		cursor:   "cursor-after",
	})
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("submit error = %v, want JSON type error", err)
	}
	waitForSignal(t, dispatcher.failure(), "malformed dispatcher failure")
	if err := dispatcher.wait(); !errors.As(err, &typeErr) {
		t.Fatalf("dispatcher wait error = %v, want JSON type error", err)
	}
	if got := bot.cursorStore.Get(); got != "cursor-before" {
		t.Fatalf("cursor = %q, want cursor-before", got)
	}
	assertDispatcherQuiescent(t, dispatcher)
}

func TestCursorFailureDoesNotAdvanceBatchFrontier(t *testing.T) {
	dir := t.TempDir()
	bot := New(Options{
		ContextTokenPath: filepath.Join(dir, "context_tokens.json"),
		CursorPath:       dir,
		ReplayPath:       filepath.Join(dir, "replay_dedupe.json"),
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 1)
	if err := dispatcher.submit(dispatcherTestBatch(t, 305, "alice", "cursor-failed")); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, dispatcher.failure(), "cursor failure")
	if err := dispatcher.wait(); err == nil || !strings.Contains(err.Error(), "commit cursor") {
		t.Fatalf("dispatcher wait error = %v, want cursor commit failure", err)
	}
	dispatcher.mu.Lock()
	nextCommitID := dispatcher.nextCommitID
	dispatcher.mu.Unlock()
	if nextCommitID != 0 {
		t.Fatalf("next commit batch = %d, want 0", nextCommitID)
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("in-memory cursor = %q, want uncommitted", got)
	}
	if !bot.replayStore.Seen(dispatcherReplayKey(305, "alice")) {
		t.Fatal("Acked message replay key was not committed before cursor failure")
	}
}

func TestKeyedDispatcherRunsDifferentConversationsConcurrently(t *testing.T) {
	bot := newDispatcherTestBot(t)
	started := make(chan string, 2)
	release := make(chan struct{})
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		started <- msg.UserID
		<-release
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 311, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 312, "bob", "cursor-2")); err != nil {
		t.Fatal(err)
	}

	users := map[string]bool{}
	for len(users) < 2 {
		select {
		case userID := <-started:
			users[userID] = true
		case <-time.After(time.Second):
			t.Fatalf("handlers did not overlap; started users = %v", users)
		}
	}
	close(release)
	waitForCondition(t, "both replay commits", func() bool {
		return bot.replayStore.Seen(dispatcherReplayKey(311, "alice")) && bot.replayStore.Seen(dispatcherReplayKey(312, "bob"))
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
}

func TestCursorWaitsForContiguousCompletedBatchPrefix(t *testing.T) {
	bot := newDispatcherTestBot(t)
	releaseFirst := make(chan struct{})
	secondHandled := make(chan struct{})
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		if msg.Raw.MessageID == 321 {
			<-releaseFirst
		} else {
			close(secondHandled)
		}
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 321, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 322, "bob", "cursor-2")); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, secondHandled, "later conversation handler")
	waitForCondition(t, "later replay commit", func() bool {
		return bot.replayStore.Seen(dispatcherReplayKey(322, "bob"))
	})
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor advanced past incomplete prefix: %q", got)
	}
	close(releaseFirst)
	waitForCondition(t, "cursor frontier", func() bool {
		return bot.cursorStore.Get() == "cursor-2"
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
}

func TestLaterAckCommitsReplayButNotCursorAfterEarlierRetry(t *testing.T) {
	bot := newDispatcherTestBot(t)
	retryErr := errors.New("retry first batch")
	releaseFirst := make(chan struct{})
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		if msg.Raw.MessageID == 331 {
			<-releaseFirst
			return RetryMessage(retryErr)
		}
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 331, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 332, "bob", "cursor-2")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "later replay commit", func() bool {
		return bot.replayStore.Seen(dispatcherReplayKey(332, "bob"))
	})
	close(releaseFirst)
	waitForSignal(t, dispatcher.failure(), "dispatcher failure")
	if err := dispatcher.wait(); !errors.Is(err, retryErr) {
		t.Fatalf("dispatcher wait error = %v, want %v", err, retryErr)
	}
	if bot.replayStore.Seen(dispatcherReplayKey(331, "alice")) {
		t.Fatal("retry message was committed")
	}
	if got := bot.cursorStore.Get(); got != "" {
		t.Fatalf("cursor = %q, want uncommitted", got)
	}
}

func TestConcurrentReplayInvokesHandlerOnceWithinConversation(t *testing.T) {
	bot := newDispatcherTestBot(t)
	var calls atomic.Int32
	release := make(chan struct{})
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		calls.Add(1)
		<-release
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 341, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 341, "alice", "cursor-2")); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, "first handler call", func() bool { return calls.Load() == 1 })
	close(release)
	waitForCondition(t, "duplicate batch cursor", func() bool {
		return bot.cursorStore.Get() == "cursor-2"
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

func TestRetryCancelsQueuedConversationWork(t *testing.T) {
	bot := newDispatcherTestBot(t)
	retryErr := errors.New("retry queued conversation")
	release := make(chan struct{})
	var secondCalled atomic.Bool
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		if msg.Raw.MessageID == 351 {
			<-release
			return RetryMessage(retryErr)
		}
		secondCalled.Store(true)
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	if err := dispatcher.submit(dispatcherTestBatch(t, 351, "alice", "cursor-1")); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.submit(dispatcherTestBatch(t, 352, "alice", "cursor-2")); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitForSignal(t, dispatcher.failure(), "dispatcher failure")
	if err := dispatcher.wait(); !errors.Is(err, retryErr) {
		t.Fatalf("dispatcher wait error = %v, want %v", err, retryErr)
	}
	if secondCalled.Load() {
		t.Fatal("queued same-conversation handler ran after retry failure")
	}
}

func TestDispatcherCancellationUnblocksQueuedWork(t *testing.T) {
	t.Run("waiting for conversation tail", func(t *testing.T) {
		bot := newDispatcherTestBot(t)
		ctx, cancel := context.WithCancel(context.Background())
		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		var secondCalled atomic.Bool
		handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
			if msg.Raw.MessageID == 371 {
				close(firstStarted)
				<-releaseFirst
			} else {
				secondCalled.Store(true)
			}
			return AckMessage()
		})
		dispatcher := newKeyedDispatcher(ctx, bot, handler, 2)
		if err := dispatcher.submit(dispatcherTestBatch(t, 371, "alice", "cursor-1")); err != nil {
			t.Fatal(err)
		}
		if err := dispatcher.submit(dispatcherTestBatch(t, 372, "alice", "cursor-2")); err != nil {
			t.Fatal(err)
		}
		dispatcher.mu.Lock()
		queuedDone := dispatcher.tails["user:alice"]
		dispatcher.mu.Unlock()
		waitForSignal(t, firstStarted, "active handler")
		cancel()
		waitForSignal(t, queuedDone, "tail waiter cancellation")
		close(releaseFirst)
		if err := dispatcher.wait(); err != nil {
			t.Fatalf("dispatcher wait: %v", err)
		}
		assertDispatcherQuiescent(t, dispatcher)
		if secondCalled.Load() {
			t.Fatal("tail-queued handler ran after cancellation")
		}
		if got := bot.cursorStore.Get(); got != "cursor-1" {
			t.Fatalf("cursor = %q, want cursor-1", got)
		}
	})

	t.Run("waiting for active slot", func(t *testing.T) {
		bot := newDispatcherTestBot(t)
		ctx, cancel := context.WithCancel(context.Background())
		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		var secondCalled atomic.Bool
		handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
			if msg.Raw.MessageID == 381 {
				close(firstStarted)
				<-releaseFirst
			} else {
				secondCalled.Store(true)
			}
			return AckMessage()
		})
		dispatcher := newKeyedDispatcher(ctx, bot, handler, 1)
		if err := dispatcher.submit(dispatcherTestBatch(t, 381, "alice", "cursor-1")); err != nil {
			t.Fatal(err)
		}
		waitForSignal(t, firstStarted, "active handler")
		if err := dispatcher.submit(dispatcherTestBatch(t, 382, "bob", "cursor-2")); err != nil {
			t.Fatal(err)
		}
		dispatcher.mu.Lock()
		queuedDone := dispatcher.tails["user:bob"]
		dispatcher.mu.Unlock()
		cancel()
		waitForSignal(t, queuedDone, "active-slot waiter cancellation")
		close(releaseFirst)
		if err := dispatcher.wait(); err != nil {
			t.Fatalf("dispatcher wait: %v", err)
		}
		assertDispatcherQuiescent(t, dispatcher)
		if secondCalled.Load() {
			t.Fatal("active-slot queued handler ran after cancellation")
		}
		if got := bot.cursorStore.Get(); got != "cursor-1" {
			t.Fatalf("cursor = %q, want cursor-1", got)
		}
	})

	t.Run("waiting for pending slot", func(t *testing.T) {
		bot := newDispatcherTestBot(t)
		ctx, cancel := context.WithCancel(context.Background())
		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		var calls atomic.Int32
		handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
			if calls.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			return AckMessage()
		})
		dispatcher := newKeyedDispatcher(ctx, bot, handler, 1)
		batch := updateBatch{cursor: "cursor-pending"}
		for i := 0; i < updateQueueCapacity+1; i++ {
			batch.messages = append(batch.messages, dispatcherTestBatch(t, int64(390+i), "alice", "").messages[0])
		}
		submitDone := make(chan error, 1)
		go func() { submitDone <- dispatcher.submit(batch) }()
		waitForSignal(t, firstStarted, "pending-saturation handler")
		waitForCondition(t, "pending queue saturation", func() bool {
			return len(dispatcher.pending) == cap(dispatcher.pending)
		})
		select {
		case err := <-submitDone:
			t.Fatalf("submit returned before pending queue saturated: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		cancel()
		var submitErr error
		select {
		case submitErr = <-submitDone:
		case <-time.After(time.Second):
			t.Fatal("pending submit did not unblock on cancellation")
		}
		if submitErr != nil && !errors.Is(submitErr, context.Canceled) {
			t.Fatalf("submit error = %v, want context cancellation", submitErr)
		}
		close(releaseFirst)
		if err := dispatcher.wait(); err != nil {
			t.Fatalf("dispatcher wait: %v", err)
		}
		assertDispatcherQuiescent(t, dispatcher)
		if got := calls.Load(); got != 1 {
			t.Fatalf("handler calls = %d, want 1", got)
		}
		if got := bot.cursorStore.Get(); got != "" {
			t.Fatalf("cursor = %q, want uncommitted", got)
		}
	})
}

func TestDispatcherBoundsUncommittedBatchFrontier(t *testing.T) {
	bot := newDispatcherTestBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	handler := MessageHandlerFunc(func(_ context.Context, msg *IncomingMessage) MessageResult {
		if msg.Raw.MessageID == 410 {
			close(firstStarted)
			<-releaseFirst
		}
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(ctx, bot, handler, 4)
	if err := dispatcher.submit(dispatcherTestBatch(t, 410, "blocked", "cursor-0")); err != nil {
		t.Fatal(err)
	}
	waitForSignal(t, firstStarted, "frontier-blocking handler")
	for i := 1; i < updateQueueCapacity; i++ {
		if err := dispatcher.submit(dispatcherTestBatch(t, int64(410+i), fmt.Sprintf("user-%d", i), fmt.Sprintf("cursor-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	waitForCondition(t, "batch frontier saturation", func() bool {
		return len(dispatcher.batchSlots) == cap(dispatcher.batchSlots)
	})
	overflowBatch := dispatcherTestBatch(t, 500, "overflow", "cursor-overflow")
	submitDone := make(chan error, 1)
	go func() {
		submitDone <- dispatcher.submit(overflowBatch)
	}()
	select {
	case err := <-submitDone:
		t.Fatalf("submit bypassed full batch frontier: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-submitDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked submit error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("batch-frontier submit did not unblock on cancellation")
	}
	close(releaseFirst)
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
	assertDispatcherQuiescent(t, dispatcher)
}

func TestKeyedDispatcherConcurrencyBound(t *testing.T) {
	bot := newDispatcherTestBot(t)
	const total = 8
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, total)
	release := make(chan struct{})
	handler := MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return AckMessage()
	})
	dispatcher := newKeyedDispatcher(context.Background(), bot, handler, 2)
	for i := 0; i < total; i++ {
		if err := dispatcher.submit(dispatcherTestBatch(t, int64(360+i), fmt.Sprintf("user-%d", i), fmt.Sprintf("cursor-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	waitForSignal(t, started, "first bounded handler")
	waitForSignal(t, started, "second bounded handler")
	select {
	case <-started:
		t.Fatal("more than two handlers ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitForCondition(t, "bounded deliveries", func() bool {
		return bot.cursorStore.Get() == "cursor-7"
	})
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum active handlers = %d, want 2", got)
	}
}

func TestDefaultHandlerConcurrency(t *testing.T) {
	dispatcher := newKeyedDispatcher(context.Background(), newDispatcherTestBot(t), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), 0)
	if got := cap(dispatcher.active); got != defaultMaxConcurrentHandlers {
		t.Fatalf("default concurrency = %d, want %d", got, defaultMaxConcurrentHandlers)
	}
	if err := dispatcher.wait(); err != nil {
		t.Fatalf("dispatcher wait: %v", err)
	}
	capped := newKeyedDispatcher(context.Background(), newDispatcherTestBot(t), MessageHandlerFunc(func(context.Context, *IncomingMessage) MessageResult {
		return AckMessage()
	}), maximumConcurrentHandlers+1)
	if got := cap(capped.active); got != maximumConcurrentHandlers {
		t.Fatalf("capped concurrency = %d, want %d", got, maximumConcurrentHandlers)
	}
	if err := capped.wait(); err != nil {
		t.Fatalf("capped dispatcher wait: %v", err)
	}
}

func newDispatcherTestBot(t *testing.T) *Bot {
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

func dispatcherTestBatch(t *testing.T, messageID int64, userID, cursor string) updateBatch {
	t.Helper()
	raw := marshalWireMessage(t, WireMessage{
		MessageID:    messageID,
		FromUserID:   userID,
		ToUserID:     "bot-1",
		MessageType:  MessageTypeUser,
		MessageState: MessageStateFinish,
		ContextToken: fmt.Sprintf("context-%d", messageID),
		ItemList: []MessageItem{
			{Type: ItemText, TextItem: &TextItem{Text: "hello"}},
		},
	})
	return updateBatch{messages: []json.RawMessage{raw}, cursor: cursor}
}

func dispatcherReplayKey(messageID int64, userID string) string {
	return replayKeys(&WireMessage{
		MessageID:   messageID,
		FromUserID:  userID,
		MessageType: MessageTypeUser,
	})[0]
}

func waitForCondition(t *testing.T, label string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", label)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertDispatcherQuiescent(t *testing.T, dispatcher *keyedDispatcher) {
	t.Helper()
	if got := len(dispatcher.active); got != 0 {
		t.Fatalf("active slots still held: %d", got)
	}
	if got := len(dispatcher.pending); got != 0 {
		t.Fatalf("pending slots still held: %d", got)
	}
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if got := len(dispatcher.tails); got != 0 {
		t.Fatalf("conversation tails still retained: %d", got)
	}
	for batchID, state := range dispatcher.batches {
		if state.remaining != 0 {
			t.Fatalf("batch %d remaining = %d, want 0", batchID, state.remaining)
		}
	}
}
