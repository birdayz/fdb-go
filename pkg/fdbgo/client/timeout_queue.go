package client

import (
	"cmp"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"weak"
)

// Physical wakes are lower bounds; logical resumptions are ordered globally.
const (
	timeoutSlack    = 100 * time.Microsecond
	timeoutInterval = 10 * time.Second
	inReady         = -2
)

type (
	timeoutTimer interface{ request(time.Duration) }
	realTimer    struct {
		t    *time.Timer
		fire func()
	}
)

func (r *realTimer) request(d time.Duration) {
	if r.t == nil {
		r.t = time.AfterFunc(d, r.fire)
	} else {
		r.t.Reset(d)
	}
}

var monoStart = time.Now()

func monoNow() int64 { return int64(time.Since(monoStart)) }

type (
	timeoutReady struct {
		node     *timeoutNode
		arm, seq uint64
	}
	timeoutQueueRoot struct {
		mu             sync.Mutex
		running, again bool
		_              [54]byte
		seq            atomic.Uint64
		_              [56]byte
		next           atomic.Uint64
		shards         [16]*timeoutQueue
		ready          []timeoutReady // single drainer; collect appends under the current shard lock
		afterFrontier  func(now int64, frontier uint64)
		beforeScan     func(*timeoutQueue)
		afterScan      func(*timeoutQueue)
		afterPop       func(*timeoutNode)
	}
	timeoutQueue struct {
		root                *timeoutQueueRoot
		mu                  sync.Mutex
		heap                []*timeoutNode
		running, requested  bool
		reqAt               int64
		compare             func()
		timer               timeoutTimer
		now                 func() int64
		deliver             func(*Transaction, uint64)
		beforeDeliver       func(*timeoutNode)
		beforeEagerSchedule func(*timeoutNode)
	}
)

type timeoutNode struct {
	tx    weak.Pointer[Transaction]
	queue *timeoutQueue
	// Physical heap state: q.mu; wake writes also hold tx.readErrMu, so
	// the arming caller can read it under that lock for the fast path.
	wake    int64
	index   int
	queued  atomic.Bool
	retired bool // q.mu; the weak association is never changed in place
	// Logical current-arm state: tx.readErrMu, independent of the lower bound.
	deadline, desiredWake int64
	seq                   uint64
}

func newTimeoutQueue() *timeoutQueue {
	root := &timeoutQueueRoot{}
	for i := range root.shards {
		q := &timeoutQueue{root: root, now: monoNow, deliver: (*Transaction).fireTimeout}
		q.timer = &realTimer{fire: q.fire}
		root.shards[i] = q
	}
	return root.shards[0]
}

var detachedTimeouts = newTimeoutQueue()

func (q *timeoutQueue) newNode(tx *Transaction) *timeoutNode {
	root := q.root
	q = root.shards[(root.next.Add(1)-1)%uint64(len(root.shards))]
	return q.newNodeForShard(tx)
}

func (q *timeoutQueue) newNodeForShard(tx *Transaction) *timeoutNode {
	n := &timeoutNode{tx: weak.Make(tx), queue: q, index: -1}
	// Unreachability, not commit, authorizes unlinking the current arm.
	runtime.AddCleanup(tx, func(n *timeoutNode) { n.queue.remove(n) }, n)
	return n
}

// Caller holds tx.readErrMu. A later logical key can reuse an earlier queued
// lower bound; claiming a node or lowering its key requires eager insertion.
func (q *timeoutQueue) schedule(n *timeoutNode, deadline int64) {
	now := q.now()
	n.deadline = deadline
	n.desiredWake = min(deadline+int64(timeoutSlack), now+int64(timeoutInterval))
	if n.queued.Load() && n.desiredWake >= n.wake {
		n.seq = q.root.seq.Add(1)
		return
	}
	if q.beforeEagerSchedule != nil {
		q.beforeEagerSchedule(n)
	}
	q.mu.Lock()
	n.seq = q.root.seq.Add(1)
	if n.index >= 0 {
		n.wake = n.desiredWake
		if !q.down(n.index) {
			q.up(n.index)
		}
	} else {
		q.pushExact(n, n.desiredWake)
	}
	if !q.running && (!q.requested || n.wake < q.reqAt) {
		q.requestLocked(n.wake, q.now())
	}
	q.mu.Unlock()
}

// retire withdraws an obsolete weak identity without touching the replacement.
// Caller holds the owning transaction's readErrMu.
func (q *timeoutQueue) retire(n *timeoutNode) {
	q.mu.Lock()
	n.retired = true
	if n.index >= 0 {
		q.removeAt(n.index)
	}
	n.index = -1
	n.queued.Store(false)
	q.mu.Unlock()
}

func (q *timeoutQueue) remove(n *timeoutNode) {
	q.mu.Lock()
	if n.retired {
		q.mu.Unlock()
		return
	}
	if n.index >= 0 {
		q.removeAt(n.index)
	}
	n.index = -1
	n.queued.Store(false)
	q.mu.Unlock()
}

// Each shard's runtime timer enters the same global drainer. No producer
// takes root.mu, and a callback never holds its shard lock while taking it.
func (q *timeoutQueue) fire() {
	q.mu.Lock()
	q.requested = false
	q.mu.Unlock()
	q.root.fire()
}

func (root *timeoutQueueRoot) fire() {
	root.mu.Lock()
	if root.running {
		root.again = true
		root.mu.Unlock()
		return
	}
	root.running = true
	root.mu.Unlock()
	for {
		for {
			now, frontier := root.shards[0].now(), root.seq.Load()
			if root.afterFrontier != nil {
				root.afterFrontier(now, frontier)
			}
			for _, q := range root.shards {
				if root.beforeScan != nil {
					root.beforeScan(q)
				}
				q.collect(now)
				if root.afterScan != nil {
					root.afterScan(q)
				}
			}
			if len(root.ready) == 0 {
				break
			}
			root.deliverReady(now, frontier)
		}
		for _, q := range root.shards {
			q.mu.Lock()
			if len(q.heap) > 0 {
				q.requestLocked(q.heap[0].wake, q.now())
			}
			q.running = false
			q.mu.Unlock()
		}
		// A callback during the staggered shard exit must not lose its wake.
		root.mu.Lock()
		if root.again {
			root.again = false
			root.mu.Unlock()
			continue
		}
		root.running = false
		root.mu.Unlock()
		return
	}
}

func (q *timeoutQueue) collect(now int64) {
	q.mu.Lock()
	q.running = true
	popped := 0
	for len(q.heap) > 0 && q.heap[0].wake <= now {
		n := q.heap[0]
		q.removeAt(0)
		q.mu.Unlock()
		if q.root.afterPop != nil {
			q.root.afterPop(n)
		}
		tx := n.tx.Value()
		if tx != nil {
			tx.readErrMu.Lock()
			q.mu.Lock()
			if !n.retired && n.index == -1 && tx.timeoutArmed {
				if n.desiredWake > now {
					// A cancelled delay's physical wake only repairs its lower bound.
					q.pushExact(n, n.desiredWake)
				} else {
					n.index = inReady
					q.root.ready = append(q.root.ready, timeoutReady{n, tx.timeoutArm, n.seq})
				}
			}
			q.mu.Unlock()
			tx.readErrMu.Unlock()
		}
		q.mu.Lock()
		popped++
		if popped%16 == 0 {
			q.mu.Unlock()
			runtime.Gosched()
			q.mu.Lock()
		}
	}
	q.mu.Unlock()
}

func (root *timeoutQueueRoot) deliverReady(now int64, frontier uint64) {
	// Immutable logical keys, not physical lower bounds, order all shards.
	slices.SortFunc(root.ready, func(a, b timeoutReady) int { return cmp.Compare(a.seq, b.seq) })
	for _, r := range root.ready {
		n := r.node
		tx := n.tx.Value()
		if tx == nil {
			continue
		}
		tx.readErrMu.Lock()
		q := n.queue
		q.mu.Lock()
		if n.retired || n.index != inReady || !tx.timeoutArmed || tx.timeoutArm != r.arm || n.seq != r.seq {
			q.mu.Unlock()
			tx.readErrMu.Unlock()
			continue
		}
		n.index = -1
		if r.seq > frontier {
			// A producer published after this cohort began. Preserve its key
			// and sequence for the next cohort without resuming its actor.
			q.pushExact(n, n.desiredWake)
		} else if now < n.deadline {
			n.desiredWake = min(n.deadline+int64(timeoutSlack), now+int64(timeoutInterval))
			n.seq = root.seq.Add(1)
			q.pushExact(n, n.desiredWake)
		} else {
			q.mu.Unlock()
			tx.readErrMu.Unlock()
			if hook := root.shards[0].beforeDeliver; hook != nil {
				hook(n)
			}
			// Synchronous cancellation fan-out can delay later entries in this batch.
			root.shards[0].deliver(tx, r.arm)
			continue
		}
		q.mu.Unlock()
		tx.readErrMu.Unlock()
	}
	clear(root.ready)
	root.ready = root.ready[:0]
}

func (q *timeoutQueue) pushExact(n *timeoutNode, wake int64) {
	n.wake = wake
	n.index = len(q.heap)
	q.heap = append(q.heap, n)
	n.queued.Store(true)
	q.up(n.index)
}

func (q *timeoutQueue) requestLocked(wake, now int64) {
	q.requested = true
	q.reqAt = wake
	q.timer.request(time.Duration(max(wake-now, 0)))
}

func (q *timeoutQueue) less(i, j int) bool {
	if q.compare != nil {
		q.compare()
	}
	return q.heap[i].wake < q.heap[j].wake
}

func (q *timeoutQueue) swap(i, j int) {
	h := q.heap
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}

func (q *timeoutQueue) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if !q.less(i, p) {
			return
		}
		q.swap(i, p)
		i = p
	}
}

func (q *timeoutQueue) down(i int) bool {
	start, n := i, len(q.heap)
	for {
		c := 2*i + 1
		if c >= n {
			break
		}
		if r := c + 1; r < n && q.less(r, c) {
			c = r
		}
		if !q.less(c, i) {
			break
		}
		q.swap(i, c)
		i = c
	}
	return i > start
}

func (q *timeoutQueue) removeAt(i int) {
	last := len(q.heap) - 1
	n := q.heap[i]
	if i != last {
		q.swap(i, last)
	}
	q.heap[last] = nil
	q.heap = q.heap[:last]
	if i < last && !q.down(i) {
		q.up(i)
	}
	n.index = -1
	n.queued.Store(false)
}
