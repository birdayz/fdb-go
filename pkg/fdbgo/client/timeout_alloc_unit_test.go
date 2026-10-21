package client

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// Not parallel: AllocsPerRun measures process-wide allocations. Preallocate
// transaction objects and slice capacity so this counts only each first arm.
func TestTimeoutFirstArmAllocations(t *testing.T) {
	q, _, _ := manualTimeoutQueue()
	const runs = 100
	txs := make([]*Transaction, runs+1)
	for _, shard := range q.root.shards {
		shard.heap = make([]*timeoutNode, 0, len(txs))
	}
	for i := range txs {
		txs[i] = timeoutTestTransaction(q)
		txs[i].timeoutNs.Store(int64(time.Hour))
		txs[i].deadlineNs.Store(time.Now().Add(time.Hour).UnixNano())
	}
	i := 0
	allocs := testing.AllocsPerRun(runs, func() {
		tx := txs[i]
		i++
		tx.readErrMu.Lock()
		tx.armTimeoutLocked()
		tx.readErrMu.Unlock()
	})
	if allocs != 4 {
		t.Fatalf("FIRST_ARM_ALLOC: allocations=%v, want 4 (node, weak handle and cleanup registration)", allocs)
	}
	t.Logf("FIRST_ARM_ALLOC: %v; node=%d bytes; queue=%d bytes (before timer/slices)", allocs, unsafe.Sizeof(timeoutNode{}), unsafe.Sizeof(timeoutQueue{}))
	runtime.KeepAlive(txs)
}

// Not parallel. The fake delivery isolates queue bookkeeping from the escaping,
// private FDBError allocated by real delivery. Both slice capacities are warm.
func TestTimeoutQueueBookkeepingAllocations(t *testing.T) {
	q, clock, _ := manualTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.heap = make([]*timeoutNode, 0, 8)
	}
	q.root.ready = make([]timeoutReady, 0, 8)
	a, b := &Transaction{}, &Transaction{}
	na, nb := q.newNode(a), q.newNode(b)
	deliveries := 0
	q.deliver = func(*Transaction, uint64) { deliveries++ }
	allocs := testing.AllocsPerRun(100, func() {
		now := clock.Load()
		scheduleTimeoutNode(na, 1, now+int64(30*time.Second))
		scheduleTimeoutNode(nb, 1, now+int64(30*time.Second))
		scheduleTimeoutNode(na, 2, now+int64(30*time.Second))
		nb.queue.remove(nb)
		scheduleTimeoutNode(nb, 2, now+int64(30*time.Second))
		q.fire() // stale callback
		clock.Add(int64(timeoutInterval))
		q.fire() // interval requeue, including fresh seq
		clock.Store(now + int64(31*time.Second))
		q.fire() // collect, sort and deliver the complete due batch
	})
	if allocs != 0 || deliveries != 202 {
		t.Fatalf("QUEUE_BOOKKEEPING_ALLOC: allocations=%v deliveries=%d, want 0/202", allocs, deliveries)
	}
	requireTimeoutHeap(t, q)
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

// Not parallel. Real delivery into an existing incarnation allocates its own
// mutable FDBError, separately from the zero-allocation queue bookkeeping.
func TestTimeoutRealDeliveryAllocations(t *testing.T) {
	q, _, _ := manualTimeoutQueue()
	const runs = 100
	txs := make([]*Transaction, runs+1)
	for _, shard := range q.root.shards {
		shard.heap = make([]*timeoutNode, 0, len(txs))
	}
	for i := range txs {
		txs[i] = timeoutTestTransaction(q)
		txs[i].SetTimeout(time.Hour.Milliseconds())
	}
	i := 0
	allocs := testing.AllocsPerRun(runs, func() { tx := txs[i]; i++; tx.fireTimeout(tx.timeoutArm) })
	if allocs != 1 {
		t.Fatalf("REAL_TIMEOUT_DELIVERY_ALLOC: allocations=%v, want 1 private error", allocs)
	}
	for _, tx := range txs {
		requireTimeoutCode(t, tx.readLife.cause, ErrTransactionTimedOut)
	}
	runtime.KeepAlive(txs)
}

// Not parallel. The queue/timer backing storage is a database-level cost,
// excluded from the per-transaction pin after warming capacity.
func TestCreateTransactionTimeoutAllocations(t *testing.T) {
	q, _, _ := manualTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.heap = make([]*timeoutNode, 0, 1024)
	}
	db := &Database{db: &database{timeouts: q}}
	db.db.txDefaults.Store(&TransactionDefaults{Timeout: 30000})
	var last *Transaction
	allocs := testing.AllocsPerRun(100, func() { last = db.CreateTransaction() })
	if allocs != 9 {
		t.Fatalf("CREATE_TIMEOUT_ALLOC: allocations=%v, want 9 (verified baseline 11, two fewer)", allocs)
	}
	t.Logf("CREATE_TIMEOUT_ALLOC: candidate=%v; verified baseline=11; total warmed queue capacity=%d pointers", allocs, cap(q.heap)*len(q.root.shards))
	runtime.KeepAlive(last)
}
