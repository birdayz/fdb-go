// Package sendqueue orders cold-path admission with FIFO permits.
package sendqueue

import (
	"sync"
	"sync/atomic"
)

// Queue grants its head permit and parks later reservations. Its zero value is
// ready to use. A Queue must not be copied after first use.
type Queue struct {
	mu      sync.Mutex // leaf lock: no callbacks or waits while held
	pending atomic.Bool
	head    *Permit
	tail    *Permit
	sealed  bool // under mu; no reservation may extend a completed dial generation
	drained chan struct{}
}

// Permit is one reservation in a Queue. Its links and membership are guarded by
// the queue's mutex; queue and ready are immutable after construction.
type Permit struct {
	queue      *Queue
	ready      chan struct{}
	prev, next *Permit
	linked     bool
}

// Pending reports whether the queue has any unreleased reservations.
func (q *Queue) Pending() bool { return q.pending.Load() }

// Reserve appends a permit and reports whether it started an empty queue. It
// panics if the queue was sealed; callers holding a stale queue use TryReserve.
func (q *Queue) Reserve() (*Permit, bool) {
	p, first, ok := q.TryReserve()
	if !ok {
		panic("sendqueue.Queue.Reserve: sealed queue")
	}
	return p, first
}

// TryReserve appends a permit unless the queue is sealed. Only the head is ready;
// its successor becomes ready when the head releases. A refusal allocates nothing.
func (q *Queue) TryReserve() (*Permit, bool, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.sealed {
		return nil, false, false
	}
	p := &Permit{queue: q, ready: make(chan struct{}), linked: true}
	first := q.head == nil
	p.prev = q.tail
	if q.tail != nil {
		q.tail.next = p
	} else {
		q.head = p
	}
	q.tail = p
	if first {
		q.pending.Store(true)
		close(p.ready)
	}
	return p, first, true
}

// Seal fixes the generation's membership. The shared channel closes after its
// last existing permit releases, immediately if the generation is already empty.
func (q *Queue) Seal() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.sealed {
		q.sealed = true
		if q.drained == nil {
			q.drained = make(chan struct{})
		}
		if q.head == nil {
			close(q.drained)
		}
	}
	return q.drained
}

// Drained returns one stable completion channel, also before Seal. It closes
// only once the generation is sealed and empty; callers never allocate a per-waiter signal.
func (q *Queue) Drained() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.drained == nil {
		q.drained = make(chan struct{})
	}
	return q.drained
}

// Ready closes once the permit becomes head. Releasing a permit before it is
// granted leaves Ready open. A nil or zero-value permit has a nil Ready channel.
func (p *Permit) Ready() <-chan struct{} {
	if p == nil {
		return nil
	}
	return p.ready
}

// Release removes this reservation, granting the next head if needed. It is
// safe concurrently, is idempotent, and does nothing for nil or zero permits.
func (p *Permit) Release() {
	if p == nil || p.queue == nil {
		return
	}
	q := p.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if !p.linked {
		return
	}
	wasHead := q.head == p
	if p.prev != nil {
		p.prev.next = p.next
	} else {
		q.head = p.next
	}
	if p.next != nil {
		p.next.prev = p.prev
	} else {
		q.tail = p.prev
	}
	p.prev, p.next, p.linked = nil, nil, false
	if wasHead {
		if q.head != nil {
			close(q.head.ready)
		} else {
			q.pending.Store(false)
			if q.sealed {
				close(q.drained)
			}
		}
	}
}
