package wechatbot

import (
	"context"
	"fmt"
	"sync"
)

const (
	defaultMaxConcurrentHandlers = 4
	maximumConcurrentHandlers    = 256
	fallbackConversationKey      = "\x00fallback"
)

type dispatchBatchState struct {
	remaining int
	cursor    string
	failed    bool
}

// keyedDispatcher serializes a conversation's complete delivery unit while
// allowing unrelated conversations to make progress concurrently.
type keyedDispatcher struct {
	bot     *Bot
	handler MessageHandler
	ctx     context.Context
	cancel  context.CancelFunc

	active     chan struct{}
	pending    chan struct{}
	batchSlots chan struct{}
	failed     chan struct{}
	wg         sync.WaitGroup

	mu            sync.Mutex
	tails         map[string]chan struct{}
	batches       map[uint64]*dispatchBatchState
	nextBatchID   uint64
	nextCommitID  uint64
	firstError    error
	failureClosed bool
}

func newKeyedDispatcher(parent context.Context, bot *Bot, handler MessageHandler, maxConcurrent int) *keyedDispatcher {
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrentHandlers
	}
	if maxConcurrent > maximumConcurrentHandlers {
		maxConcurrent = maximumConcurrentHandlers
	}
	ctx, cancel := context.WithCancel(parent)
	return &keyedDispatcher{
		bot:        bot,
		handler:    handler,
		ctx:        ctx,
		cancel:     cancel,
		active:     make(chan struct{}, maxConcurrent),
		pending:    make(chan struct{}, maxConcurrent*updateQueueCapacity),
		batchSlots: make(chan struct{}, updateQueueCapacity),
		failed:     make(chan struct{}),
		tails:      make(map[string]chan struct{}),
		batches:    make(map[uint64]*dispatchBatchState),
	}
}

func (d *keyedDispatcher) failure() <-chan struct{} { return d.failed }

func (d *keyedDispatcher) submit(batch updateBatch) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	select {
	case d.batchSlots <- struct{}{}:
	case <-d.ctx.Done():
		return d.ctx.Err()
	}
	if err := d.ctx.Err(); err != nil {
		<-d.batchSlots
		return err
	}
	d.mu.Lock()
	batchID := d.nextBatchID
	d.nextBatchID++
	d.batches[batchID] = &dispatchBatchState{
		remaining: len(batch.messages),
		cursor:    batch.cursor,
	}
	if len(batch.messages) == 0 {
		d.advanceCursorLocked()
	}
	d.mu.Unlock()

	for i, raw := range batch.messages {
		if err := d.ctx.Err(); err != nil {
			d.cancelUnscheduled(batchID, len(batch.messages)-i)
			return err
		}
		wire, err := d.bot.decodeWireMessage(raw)
		if err != nil {
			d.complete(batchID, err, true)
			d.cancelUnscheduled(batchID, len(batch.messages)-i-1)
			return err
		}
		if err := d.submitMessage(batchID, wire); err != nil {
			d.cancelUnscheduled(batchID, len(batch.messages)-i)
			return err
		}
	}
	return nil
}

func (d *keyedDispatcher) submitMessage(batchID uint64, wire *WireMessage) error {
	select {
	case d.pending <- struct{}{}:
	case <-d.ctx.Done():
		return d.ctx.Err()
	}

	key := conversationKey(wire)
	done := make(chan struct{})
	d.mu.Lock()
	previous := d.tails[key]
	d.tails[key] = done
	d.wg.Add(1)
	d.mu.Unlock()

	go d.runMessage(batchID, key, previous, done, wire)
	return nil
}

func (d *keyedDispatcher) runMessage(batchID uint64, key string, previous, done chan struct{}, wire *WireMessage) {
	defer d.wg.Done()
	defer func() {
		close(done)
		d.mu.Lock()
		if d.tails[key] == done {
			delete(d.tails, key)
		}
		d.mu.Unlock()
		<-d.pending
	}()

	if previous != nil {
		select {
		case <-previous:
		case <-d.ctx.Done():
			d.complete(batchID, d.ctx.Err(), false)
			return
		}
	}
	if err := d.ctx.Err(); err != nil {
		d.complete(batchID, err, false)
		return
	}

	select {
	case d.active <- struct{}{}:
	case <-d.ctx.Done():
		d.complete(batchID, d.ctx.Err(), false)
		return
	}
	if err := d.ctx.Err(); err != nil {
		<-d.active
		d.complete(batchID, err, false)
		return
	}
	err := d.bot.processWireMessage(d.ctx, d.handler, wire)
	<-d.active
	d.complete(batchID, err, true)
}

func (d *keyedDispatcher) complete(batchID uint64, err error, executed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	state := d.batches[batchID]
	if state == nil || state.remaining <= 0 {
		return
	}
	state.remaining--
	if err != nil {
		state.failed = true
		if executed {
			d.recordFailureLocked(err)
		}
	}
	if state.remaining == 0 && !state.failed {
		d.advanceCursorLocked()
	}
}

func (d *keyedDispatcher) cancelUnscheduled(batchID uint64, count int) {
	if count <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.batches[batchID]
	if state == nil {
		return
	}
	if count > state.remaining {
		count = state.remaining
	}
	state.failed = true
	state.remaining -= count
}

func (d *keyedDispatcher) advanceCursorLocked() {
	for {
		state, ok := d.batches[d.nextCommitID]
		if !ok || state.remaining != 0 || state.failed {
			return
		}
		if state.cursor != "" && state.cursor != d.bot.cursorStore.Get() {
			if err := d.bot.cursorStore.Set(state.cursor); err != nil {
				state.failed = true
				d.recordFailureLocked(fmt.Errorf("commit cursor: %w", err))
				return
			}
		}
		delete(d.batches, d.nextCommitID)
		d.nextCommitID++
		<-d.batchSlots
	}
}

func (d *keyedDispatcher) recordFailureLocked(err error) {
	if err == nil || d.firstError != nil {
		return
	}
	d.firstError = err
	if !d.failureClosed {
		close(d.failed)
		d.failureClosed = true
	}
	d.cancel()
}

func (d *keyedDispatcher) wait() error {
	d.cancel()
	return d.drain()
}

func (d *keyedDispatcher) drain() error {
	d.wg.Wait()
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.firstError
}

func conversationKey(wire *WireMessage) string {
	if userID := peerUserID(wire); userID != "" {
		return "user:" + userID
	}
	return fallbackConversationKey
}
