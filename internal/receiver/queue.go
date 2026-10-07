package receiver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Write queue timing (contracts sections 5.1 and 11). Variables so tests can
// shrink them; timers use the real clock.
var (
	// batchWindow is how long requests are gathered before one append.
	batchWindow = 2 * time.Second
	// holdCap bounds how long a request waits: the append's context ends this
	// long after the oldest request in its batch arrived, and every request in
	// the batch then gets 5xx so Apollo can retry it.
	//
	// S0 confirms: Apollo's request timeout and whether it retries a 5xx or a
	// timeout. Until then the safe default holds: nothing is answered 2xx
	// before it is stored, and every request is answered within this cap,
	// which is well inside common webhook timeouts. A request Apollo gave up on
	// that was stored anyway is sent again and de-duplicated by its event key.
	holdCap = 10 * time.Second
	// maxBatchChars flushes a batch early once its bodies reach this many
	// bytes, keeping one append well under the Sheets request size limit.
	maxBatchChars = 8 << 20
)

var errClosed = errors.New("the receiver is shutting down")

// batch is the requests gathered into one AppendEvents call.
type batch struct {
	events []api.RawEvent
	oldest time.Time // real arrival time of the first request
	size   int
	timer  *time.Timer
	done   chan struct{} // closed once err is set
	err    error
}

// queue gathers requests and appends them in batches, one batch at a time,
// in arrival order. A request is answered only from its batch's result, so
// nothing is acknowledged before the store said it is durable.
type queue struct {
	events api.EventLog
	onErr  func(error) // logs a failed append

	mu      sync.Mutex
	pending *batch
	closed  bool

	ready  chan *batch // batches waiting for the writer, in order
	exited chan struct{}
	// appendFailed is the result of the last append: /healthz without a timer
	// is 503 while it is true.
	appendFailed atomic.Bool
}

func newQueue(events api.EventLog, onErr func(error)) *queue {
	q := &queue{events: events, onErr: onErr, ready: make(chan *batch, 1024), exited: make(chan struct{})}
	go q.writer()
	return q
}

// submit adds one event to the open batch and returns that batch; the caller
// waits on its done channel.
func (q *queue) submit(e api.RawEvent) (*batch, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, errClosed
	}
	b := q.pending
	if b == nil {
		b = &batch{oldest: time.Now(), done: make(chan struct{})}
		q.pending = b
		b.timer = time.AfterFunc(batchWindow, func() { q.flush(b) })
	}
	b.events = append(b.events, e)
	b.size += len(e.Body)
	if b.size >= maxBatchChars {
		q.sendLocked(b)
	}
	return b, nil
}

// flush hands b to the writer if it is still the open batch.
func (q *queue) flush(b *batch) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sendLocked(b)
}

func (q *queue) sendLocked(b *batch) {
	if q.pending != b {
		return // already sent
	}
	q.pending = nil
	b.timer.Stop()
	q.ready <- b
}

// writer appends batches one at a time, in the order they closed.
func (q *queue) writer() {
	defer close(q.exited)
	for b := range q.ready {
		ctx, cancel := context.WithDeadline(context.Background(), b.oldest.Add(holdCap))
		// A store that returns success after the cap has still stored the
		// batch; its requests may already have had 503, and Apollo's retry is
		// de-duplicated by the event key.
		err := q.events.AppendEvents(ctx, b.events)
		cancel()
		q.appendFailed.Store(err != nil)
		if err != nil && q.onErr != nil {
			q.onErr(err)
		}
		b.err = err
		close(b.done)
	}
}

// close sends the open batch at once, refuses new requests, and returns once
// every batch is written (the drain at shutdown).
func (q *queue) close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		<-q.exited
		return
	}
	q.closed = true
	if q.pending != nil {
		q.sendLocked(q.pending)
	}
	close(q.ready)
	q.mu.Unlock()
	<-q.exited
}
