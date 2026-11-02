package kimi

import (
	"github.com/pashcock/agent-hub/internal/adapter"
	"sync"
	"time"
)

// A slow caller may lose deltas, never a terminal result or approval. Close
// aborts delivery: the hub stops its consumer before closing the adapter.
type eventQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []adapter.Event
	closed bool
	out    chan adapter.Event
	done   chan struct{}
	once   sync.Once
}

func newQueue() *eventQueue {
	q := &eventQueue{out: make(chan adapter.Event, 256), done: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	go q.drain()
	return q
}
func (q *eventQueue) put(e adapter.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if len(q.items) >= 256 && (e.Kind == adapter.EventMessageDelta || e.Kind == adapter.EventProviderRaw) {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	q.items = append(q.items, e)
	q.cond.Signal()
}
func (q *eventQueue) drain() {
	defer close(q.out)
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		e := q.items[0]
		q.items[0] = adapter.Event{}
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.out <- e:
		case <-q.done:
			return
		}
	}
}
func (q *eventQueue) close() {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.items = nil
		close(q.done)
		q.cond.Broadcast()
		q.mu.Unlock()
	})
}
