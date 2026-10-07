package receiver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/HarshitBadhwar8/leadscore/internal/api"
)

// Write queue timing. Variables so tests can
// shrink them; timers use the real clock.
var (
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
	// maxBatchBytes flushes a batch early once its bodies reach this many
	// bytes. Escaping can double a body inside the Sheets request, so this is
	// under half the 10 MB request limit.
	maxBatchBytes = 4 << 20
	// readyBatches is how many closed batches may wait for the writer; past
	// that a batch is refused with 503 rather than blocking every request.
	readyBatches = 64
)

var (
	errClosed   = errors.New("the receiver is shutting down")
	errBusy     = errors.New("the receiver is too busy to store more events now")
	errTooLarge = errors.New("the stored body is over a Sheets cell")
)

// batch is the requests gathered into one AppendEvents call.
type batch struct {
	events []api.RawEvent
	oldest time.Time // real arrival time of the first request
	size   int
	timer  *time.Timer
	done   chan struct{} // closed once errs is set
	errs   []error       // per event: nil once it is stored
}

// queue gathers requests and appends them in batches, one batch at a time,
// in arrival order. A request is answered only from its own result in its
// batch, so nothing is acknowledged before the store said it is durable.
type queue struct {
	events api.EventLog
	window time.Duration // how long requests are gathered before an append
	onErr  func(error)   // logs a failed append

	mu      sync.Mutex
	pending *batch
	closed  bool

	ready  chan *batch // batches waiting for the writer, in order
	exited chan struct{}
	// appendFailed is the result of the last append: /healthz without a timer
	// is 503 while it is true.
	appendFailed atomic.Bool
}

func newQueue(events api.EventLog, window time.Duration, onErr func(error)) *queue {
	q := &queue{events: events, window: window, onErr: onErr, ready: make(chan *batch, readyBatches), exited: make(chan struct{})}
	go q.writer()
	return q
}

// submit adds one event to the open batch and returns that batch and the
// event's place in it; the caller waits on the batch's done channel.
func (q *queue) submit(e api.RawEvent) (*batch, int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, 0, errClosed
	}
	b := q.pending
	if b == nil {
		b = &batch{oldest: time.Now(), done: make(chan struct{})}
		q.pending = b
		b.timer = time.AfterFunc(q.window, func() { q.flush(b) })
	}
	i := len(b.events)
	b.events = append(b.events, e)
	b.size += len(e.Body)
	if b.size >= maxBatchBytes {
		q.sendLocked(b)
	}
	return b, i, nil
}

// flush hands b to the writer if it is still the open batch.
func (q *queue) flush(b *batch) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sendLocked(b)
}

// sendLocked hands b to the writer without blocking: when too many batches
// already wait, b's requests all get 503 at once.
func (q *queue) sendLocked(b *batch) {
	if q.pending != b {
		return // already sent
	}
	q.pending = nil
	b.timer.Stop()
	select {
	case q.ready <- b:
	default:
		b.finish(errBusy)
	}
}

// finish sets one result for every event of b.
func (b *batch) finish(err error) {
	b.errs = make([]error, len(b.events))
	for i := range b.errs {
		b.errs[i] = err
	}
	close(b.done)
}

// writer appends batches one at a time, in the order they closed.
func (q *queue) writer() {
	defer close(q.exited)
	for b := range q.ready {
		q.write(b)
	}
}

// write appends one batch. A panic in the store is that batch's failure, not
// the receiver's. An event too big for the store is left out and refused on
// its own, so it cannot make the rest of its batch fail on every retry.
func (q *queue) write(b *batch) {
	errs := make([]error, len(b.events))
	defer func() {
		if p := recover(); p != nil {
			err := fmt.Errorf("storing events panicked: %v", p)
			q.failed(err)
			for i := range errs {
				errs[i] = err
			}
		}
		b.errs = errs
		close(b.done)
	}()
	ctx, cancel := context.WithDeadline(context.Background(), b.oldest.Add(holdCap))
	defer cancel()
	keep := make([]int, 0, len(b.events))
	for i, e := range b.events {
		if utf8.RuneCount(e.Body) > maxBodyChars {
			errs[i] = errTooLarge
			continue
		}
		keep = append(keep, i)
	}
	evs := make([]api.RawEvent, len(keep))
	for j, i := range keep {
		evs[j] = b.events[i]
	}
	// A store that returns success after the cap has still stored the
	// batch; its requests may already have had 503, and Apollo's retry is
	// de-duplicated by the event key.
	err := q.events.AppendEvents(ctx, evs)
	if err != nil {
		q.failed(err)
		for _, i := range keep {
			errs[i] = err
		}
		return
	}
	q.appendFailed.Store(false)
}

func (q *queue) failed(err error) {
	q.appendFailed.Store(true)
	if q.onErr != nil {
		q.onErr(err)
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
		// Blocking here is fine: nothing else can send any more.
		b := q.pending
		q.pending = nil
		b.timer.Stop()
		q.mu.Unlock()
		q.ready <- b
		q.mu.Lock()
	}
	close(q.ready)
	q.mu.Unlock()
	<-q.exited
}
