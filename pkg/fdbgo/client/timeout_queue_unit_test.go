package client

import (
	"context"
	"math/bits"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"
)

type manualTimeoutTimer struct {
	requests int
	delay    time.Duration
}

func (m *manualTimeoutTimer) request(d time.Duration) { m.requests++; m.delay = d }

func manualTimeoutQueue() (*timeoutQueue, *atomic.Int64, *manualTimeoutTimer) {
	clock := &atomic.Int64{}
	clock.Store(int64(time.Hour))
	q := newTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.now = clock.Load
		shard.timer = &manualTimeoutTimer{}
	}
	return q, clock, q.timer.(*manualTimeoutTimer)
}

func timeoutTestTransaction(q *timeoutQueue) *Transaction {
	return &Transaction{db: &database{timeouts: q}}
}

// Logical arms belong to the transaction; physical scheduling receives only
// the node and absolute deadline. Exercise the same transaction-lock handshake.
func scheduleTimeoutNode(n *timeoutNode, arm uint64, deadline int64) {
	tx := n.tx.Value()
	tx.readErrMu.Lock()
	tx.timeoutNode, tx.timeoutArm, tx.timeoutArmed = n, arm, true
	n.queue.schedule(n, deadline)
	tx.readErrMu.Unlock()
}

func timeoutHeapLen(q *timeoutQueue) int {
	var size int
	for _, shard := range q.root.shards {
		shard.mu.Lock()
		size += len(shard.heap)
		shard.mu.Unlock()
	}
	return size
}

func requireTimeoutCode(t *testing.T, err error, want int) {
	t.Helper()
	if code, ok := fdbErrorCode(err); !ok || code != want {
		t.Fatalf("error = %v, want FDB code %d", err, want)
	}
}

func requireTimeoutHeap(t *testing.T, q *timeoutQueue) {
	t.Helper()
	for _, shard := range q.root.shards {
		shard.mu.Lock()
		for i, n := range shard.heap {
			if n == nil || n.index != i || n.queue != shard || !n.queued.Load() {
				shard.mu.Unlock()
				t.Fatalf("HEAP_INDEX: slot %d has node %v", i, n)
			}
			if i > 0 && n.wake < shard.heap[(i-1)/2].wake {
				shard.mu.Unlock()
				t.Fatalf("HEAP_ORDER: child %d precedes parent", i)
			}
		}
		for i, n := range shard.heap[:cap(shard.heap)][len(shard.heap):] {
			if n != nil {
				slot := len(shard.heap) + i
				shard.mu.Unlock()
				t.Fatalf("HEAP_SLOT: removed node retained at backing slot %d", slot)
			}
		}
		shard.mu.Unlock()
	}
	for _, r := range q.root.ready[:cap(q.root.ready)] {
		if r != (timeoutReady{}) {
			t.Fatal("READY_SLOT: completed batch retained a record")
		}
	}
}

// Mutations: remove slack, remove the interval cap, or use <= at the deadline.
func TestTimeoutQueueWake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		wake     time.Duration
	}{
		{"slack", time.Second, time.Second + 100*time.Microsecond},
		{"interval", 30 * time.Second, 10 * time.Second},
		{"partial-slack", 10*time.Second - 50*time.Microsecond, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, clock, timer := manualTimeoutQueue()
			start := clock.Load()
			tx := &Transaction{}
			n := q.newNode(tx)
			delivered := 0
			q.deliver = func(*Transaction, uint64) { delivered++ }
			scheduleTimeoutNode(n, 1, start+int64(tc.deadline))
			if n.wake != start+int64(tc.wake) || timer.delay != tc.wake {
				t.Fatalf("WAKE_POLICY: wake=%d delay=%v, want %v", n.wake-start, timer.delay, tc.wake)
			}
			clock.Store(n.wake - 1)
			q.fire()
			if delivered != 0 {
				t.Fatal("EARLY_DELIVERY: delivered before requested wake")
			}
			clock.Store(start + int64(tc.wake))
			q.fire()
			if tc.name == "interval" {
				if delivered != 0 || n.index < 0 || n.wake != start+2*int64(timeoutInterval) {
					t.Fatal("INTERVAL_REQUEUE: early interval failed to wait again")
				}
				// The third interval lands exactly on the deadline: deliver,
				// rather than adding a fourth wait for the full slack.
				clock.Store(start + 2*int64(timeoutInterval))
				q.fire()
				clock.Store(start + int64(tc.deadline))
				q.fire()
			}
			if delivered != 1 {
				t.Fatalf("DEADLINE_DELIVERY: deliveries=%d, want 1", delivered)
			}
			runtime.KeepAlive(tx)
		})
	}
}

// Mutation: derive the deadline from the time sampled after taking q.mu.
func TestTimeoutQueueContentionPreservesDeadline(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	tx := &Transaction{}
	n := q.newNode(tx)
	deadline := clock.Load() + int64(time.Second)
	q.mu.Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() { close(started); scheduleTimeoutNode(n, 1, deadline); close(done) }()
	<-started
	clock.Add(int64(500 * time.Millisecond))
	q.mu.Unlock()
	<-done
	if n.deadline != deadline || n.wake != deadline+int64(timeoutSlack) {
		t.Fatal("CONTENTION_BUDGET: queue lock wait extended the deadline")
	}
	runtime.KeepAlive(tx)
}

// Mutation: do not refresh seq when the timebomb's interval requeues it.
func TestTimeoutQueueFIFO(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	a, b := &Transaction{}, &Transaction{}
	na, nb := q.newNode(a), q.newNode(b)
	start := clock.Load()
	var got []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { got = append(got, tx) }
	scheduleTimeoutNode(na, 1, start+int64(20*time.Second))
	scheduleTimeoutNode(nb, 1, start+int64(20*time.Second))
	clock.Add(int64(timeoutInterval))
	q.fire() // Both interval continuations take fresh FIFO positions.
	if len(got) != 0 || na.seq >= nb.seq {
		t.Fatal("FIFO_EQUAL_WAKE: interval continuations lost enqueue order")
	}
	// B is now already queued when A is explicitly rearmed for the same wake.
	scheduleTimeoutNode(na, 2, start+int64(20*time.Second))
	clock.Store(start + int64(20*time.Second))
	q.fire()
	if !slices.Equal(got, []*Transaction{b, a}) {
		t.Fatal("FIFO_REQUEUE: fresh continuation did not follow older queued timer")
	}
	// Pin interval refresh itself, independently of explicit schedule's seq.
	scheduleTimeoutNode(na, 3, clock.Load()+int64(30*time.Second))
	old := na.seq
	clock.Add(int64(timeoutInterval))
	q.fire()
	if na.seq <= old {
		t.Fatal("FIFO_INTERVAL_SEQUENCE: interval requeue reused its old sequence")
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

// Mutation: omit the ready-batch sort and dispatch in heap/wake order.
func TestTimeoutQueueBatchOrder(t *testing.T) {
	t.Parallel()
	for _, dispatch := range []time.Duration{15 * time.Millisecond, 25 * time.Millisecond} {
		q, clock, _ := manualTimeoutQueue()
		start := clock.Load()
		a, b := &Transaction{}, &Transaction{}
		var got []*Transaction
		q.deliver = func(tx *Transaction, _ uint64) { got = append(got, tx) }
		scheduleTimeoutNode(q.newNode(a), 1, start+int64(20*time.Millisecond-timeoutSlack))
		scheduleTimeoutNode(q.newNode(b), 1, start+int64(10*time.Millisecond-timeoutSlack))
		clock.Store(start + int64(dispatch))
		q.fire()
		want := []*Transaction{b}
		if dispatch == 25*time.Millisecond {
			want = []*Transaction{a, b}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("BATCH_FIFO: dispatch=%v delivered wrong order", dispatch)
		}
		runtime.KeepAlive(a)
		runtime.KeepAlive(b)
	}
}

// Mutation: omit the index != inReady withdrawal check.
func TestTimeoutQueueBatchWithdrawal(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	x, y, z := &Transaction{}, &Transaction{}, &Transaction{}
	nx, ny, nz := q.newNode(x), q.newNode(y), q.newNode(z)
	var got []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { got = append(got, tx) }
	deadline := clock.Load() + int64(time.Second)
	for _, n := range []*timeoutNode{nx, ny, nz} {
		scheduleTimeoutNode(n, 1, deadline)
	}
	clock.Store(deadline + int64(timeoutSlack))
	parked, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	q.beforeDeliver = func(n *timeoutNode) {
		if n == nx {
			close(parked)
			<-resume
		}
	}
	go func() { q.fire(); close(done) }()
	<-parked
	ny.queue.remove(ny)
	newDeadline := clock.Load() + int64(time.Second)
	scheduleTimeoutNode(nz, 2, newDeadline)
	close(resume)
	<-done
	if !slices.Equal(got, []*Transaction{x}) || timeoutHeapLen(q) != 1 || len(nz.queue.heap) != 1 || nz.queue.heap[0] != nz || ny.index != -1 {
		t.Fatal("BATCH_WITHDRAWAL: removed/rearmed batch entry delivered or duplicated")
	}
	clock.Store(newDeadline + int64(timeoutSlack))
	q.fire()
	if !slices.Equal(got, []*Transaction{x, z}) {
		t.Fatal("BATCH_RESCHEDULE: replacement was not delivered exactly once")
	}
	requireTimeoutHeap(t, q)
	runtime.KeepAlive(x)
	runtime.KeepAlive(y)
	runtime.KeepAlive(z)
}

// Mutations: omit swap's index update or removeAt's nil assignment.
func TestTimeoutQueueHeapInvariant(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	txs, nodes := make([]*Transaction, 10000), make([]*timeoutNode, 10000)
	for i := range nodes {
		txs[i] = &Transaction{}
		nodes[i] = q.newNode(txs[i])
	}
	rng := rand.New(rand.NewPCG(263, 1))
	for i, n := range nodes {
		scheduleTimeoutNode(n, uint64(i+1), clock.Load()+int64(rng.IntN(1000000)))
	}
	requireTimeoutHeap(t, q)
	for i := 0; i < 30000; i++ {
		n := nodes[rng.IntN(len(nodes))]
		if rng.IntN(3) == 0 {
			n.queue.remove(n)
		} else {
			scheduleTimeoutNode(n, uint64(i+10001), clock.Load()+int64(rng.IntN(1000000)))
		}
		if i%100 == 0 {
			requireTimeoutHeap(t, q)
		}
	}
	for _, n := range nodes {
		n.queue.remove(n)
	}
	requireTimeoutHeap(t, q)
	if timeoutHeapLen(q) != 0 {
		t.Fatal("heap did not empty")
	}
	runtime.KeepAlive(txs)
}

// Mutation: rebuild the heap after each change instead of indexed sifting.
// This counts heap comparisons, not an otherwise invisible linear node lookup;
// remove's index-based addressing is also checked by code inspection.
func TestTimeoutQueueSiftComparisons(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	comparisons := 0
	q.compare = func() { comparisons++ }
	tx := &Transaction{}
	for i := 1; i <= 65536; i++ {
		n := q.newNodeForShard(tx)
		comparisons = 0
		scheduleTimeoutNode(n, uint64(i), clock.Load()+int64(65537-i))
		if i > 1 && comparisons == 0 {
			t.Fatal("COMPARISON_POPULATION: nontrivial insertion counted no comparisons")
		}
		if comparisons > 2*bits.Len(uint(i-1))+2 {
			t.Fatalf("PUSH_COMPLEXITY: n=%d comparisons=%d", i, comparisons)
		}
	}
	for len(q.heap) > 0 {
		n, size := q.heap[len(q.heap)/2], len(q.heap)
		comparisons = 0
		n.queue.remove(n)
		if size > 2 && comparisons == 0 {
			t.Fatal("COMPARISON_POPULATION: nontrivial removal counted no comparisons")
		}
		if comparisons > 2*bits.Len(uint(size-1))+2 {
			t.Fatalf("REMOVE_COMPLEXITY: n=%d comparisons=%d", size, comparisons)
		}
	}
	requireTimeoutHeap(t, q)
	runtime.KeepAlive(tx)
}

// Mutation: remove fire's running guard. Callback B must return while A owns
// delivery, even after a schedule between A's pop and B's invocation.
func TestTimeoutQueueSingleDrainer(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	x, y, z := &Transaction{}, &Transaction{}, &Transaction{}
	nx, ny, nz := q.newNode(x), q.newNode(y), q.newNode(z)
	var mu sync.Mutex
	var got []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { mu.Lock(); got = append(got, tx); mu.Unlock() }
	scheduleTimeoutNode(nx, 1, clock.Load()+int64(time.Second))
	clock.Store(nx.wake)
	parked, resume, aDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	q.beforeDeliver = func(n *timeoutNode) {
		if n == nx {
			close(parked)
			<-resume
		}
	}
	go func() { q.fire(); close(aDone) }()
	<-parked
	scheduleTimeoutNode(ny, 1, clock.Load()-int64(timeoutSlack))
	future := clock.Load() + int64(time.Second)
	scheduleTimeoutNode(nz, 1, future)
	timer := nz.queue.timer.(*manualTimeoutTimer)
	requests := timer.requests
	bDone := make(chan struct{})
	go func() { q.fire(); close(bDone) }()
	<-bDone
	mu.Lock()
	deliveredByB := len(got)
	mu.Unlock()
	close(resume)
	<-aDone
	if deliveredByB != 0 {
		t.Fatal("SINGLE_DRAINER: overlapping callback delivered while A was paused")
	}
	if !slices.Equal(got, []*Transaction{x, y}) || timer.requests <= requests || nz.queue.reqAt != future+int64(timeoutSlack) || !nz.queue.requested {
		t.Fatal("DRAINER_EXIT: owner did not drain newly due work and request the future head")
	}
	runtime.KeepAlive(x)
	runtime.KeepAlive(y)
	runtime.KeepAlive(z)
}

// Mutation: do not replace an outstanding idle request when the head moves earlier.
func TestTimeoutQueueEarlierHeadReplacesRequest(t *testing.T) {
	t.Parallel()
	q, clock, timer := manualTimeoutQueue()
	tx := &Transaction{}
	later, earlier := q.newNodeForShard(tx), q.newNodeForShard(tx)
	scheduleTimeoutNode(later, 1, clock.Load()+int64(time.Second))
	requests := timer.requests
	scheduleTimeoutNode(earlier, 1, clock.Load()+int64(time.Second/2))
	if timer.requests != requests+1 || q.reqAt != earlier.wake || timer.delay != time.Second/2+timeoutSlack {
		t.Fatal("EARLIER_HEAD: idle queue did not replace its later request")
	}
	runtime.KeepAlive(tx)
}

// Mutation: omit the exit timer request.
func TestTimeoutQueueStaleCallback(t *testing.T) {
	t.Parallel()
	q, clock, timer := manualTimeoutQueue()
	tx := &Transaction{}
	n := q.newNode(tx)
	q.deliver = func(*Transaction, uint64) { t.Error("stale callback delivered before the wake") }
	scheduleTimeoutNode(n, 1, clock.Load()+int64(time.Second))
	requests := timer.requests
	q.fire()
	if timer.requests != requests+1 || n.queue.reqAt != n.wake || !n.queue.requested || n.queue.running || q.root.running {
		t.Fatal("STALE_CALLBACK_WAKE: callback lost the future head's request")
	}
	runtime.KeepAlive(tx)
}

// Mutations: request from schedule while draining, or omit the exit request.
func TestTimeoutQueueScheduleDuringDelivery(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	x, z := &Transaction{}, &Transaction{}
	nx, nz := q.newNode(x), q.newNode(z)
	scheduleTimeoutNode(nx, 1, clock.Load()+int64(time.Second))
	clock.Store(nx.wake)
	timer := nz.queue.timer.(*manualTimeoutTimer)
	requests := timer.requests
	q.deliver = func(*Transaction, uint64) {
		scheduleTimeoutNode(nz, 1, clock.Load()+int64(time.Second)) // delivery may reenter
		if timer.requests != requests {
			t.Error("DRAINING_REQUEST: schedule requested while the drainer owned the queue")
		}
	}
	q.fire()
	if timer.requests != requests+1 || nz.queue.reqAt != nz.wake {
		t.Fatal("DELIVERY_WAKE: drainer lost the reentrant schedule")
	}
	runtime.KeepAlive(x)
	runtime.KeepAlive(z)
}

// Mutation: let schedule mutate heap state outside q.mu. Run under -race too.
func TestTimeoutQueueConcurrent(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	q.deliver = func(*Transaction, uint64) {}
	var wg sync.WaitGroup
	txs := make([]*Transaction, 32)
	for i := range txs {
		txs[i] = &Transaction{}
		n := q.newNode(txs[i])
		wg.Go(func() {
			for arm := uint64(1); arm <= 1000; arm++ {
				scheduleTimeoutNode(n, arm, clock.Load()+int64(arm%3)*int64(time.Second))
				if arm%2 == 0 {
					n.queue.remove(n)
				}
				if arm%5 == 0 {
					clock.Add(int64(time.Millisecond))
					q.fire()
				}
			}
			txs[i].readErrMu.Lock()
			txs[i].timeoutArmed = false
			n.queue.remove(n)
			txs[i].readErrMu.Unlock()
		})
	}
	wg.Wait()
	q.fire()
	requireTimeoutHeap(t, q)
	if timeoutHeapLen(q) != 0 || q.root.running {
		t.Fatal("CONCURRENT_QUEUE: queue retained work after all removals")
	}
	runtime.KeepAlive(txs)
}

// Mutations: node holds a strong Transaction or database pointer. The queue
// remains reachable through the timer while both owners must be collectable.
func TestTimeoutQueueDoesNotRetainDatabase(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	var wake int64
	abandon := func() (weak.Pointer[database], weak.Pointer[Transaction]) {
		tx := timeoutTestTransaction(q)
		tx.SetTimeout(time.Hour.Milliseconds())
		n := tx.timeoutNode
		n.queue.mu.Lock()
		queued := n.index >= 0 && n.queued.Load()
		wake = n.wake
		n.queue.mu.Unlock()
		if !queued || !tx.timeoutArmed {
			t.Fatal("retention probe did not register a live positive timeout")
		}
		return weak.Make(tx.db), weak.Make(tx)
	}
	db, tx := abandon()
	for i := 0; i < 20 && (db.Value() != nil || tx.Value() != nil); i++ {
		runtime.GC()
	}
	if db.Value() != nil || tx.Value() != nil {
		t.Fatal("QUEUE_RETAINS_DATABASE: armed queue retained its database or transaction")
	}
	// Cleanup may already have unlinked it; otherwise its old stub must drop.
	clock.Store(wake)
	q.fire()
	if timeoutHeapLen(q) != 0 {
		t.Fatal("DEAD_NODE_DISPATCH: next wake did not drop the cleared weak node")
	}
	requireTimeoutHeap(t, q)
	queue := weak.Make(q)
	q = nil
	for i := 0; i < 20 && queue.Value() != nil; i++ {
		runtime.GC()
	}
	if queue.Value() != nil {
		t.Fatal("EMPTY_QUEUE_RETAINED: drained queue was not collectable")
	}
}

// Mutation: suppress delivery after Database.Close. Close never owned these
// timers in the eager implementation either.
func TestTimeoutAfterDatabaseClose(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	ctx, cancel := context.WithCancel(context.Background())
	db := &Database{db: &database{timeouts: q, ctx: ctx, cancel: cancel}}
	tx := db.CreateTransaction()
	tx.SetTimeout(time.Second.Milliseconds())
	defer tx.Cancel()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Store(tx.timeoutNode.deadline)
	q.fire() // still before slack; control: no delivery yet
	if tx.readIncarnationCause(tx.readLife) != nil {
		t.Fatal("delivered before requested wake")
	}
	clock.Store(tx.timeoutNode.wake)
	q.fire()
	requireTimeoutCode(t, context.Cause(tx.readLife.ctx), ErrTransactionTimedOut)
}
