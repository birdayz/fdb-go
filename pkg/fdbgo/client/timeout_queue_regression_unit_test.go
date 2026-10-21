package client

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	"weak"

	"fdb.dev/pkg/fdbgo/wire"
)

const timeoutQueueTestWait = 5 * time.Second

func waitTimeoutQueueTest(t *testing.T, done <-chan struct{}, label string) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(timeoutQueueTestWait):
		t.Errorf("TIMEOUT_QUEUE_WAIT: %s", label)
		return false
	}
}

func awaitTimeoutQueueTest(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	if !waitTimeoutQueueTest(t, done, label) {
		t.FailNow()
	}
}

type timeoutQueueTestWorker struct {
	label string
	done  <-chan struct{}
}

type timeoutQueueTestTasks struct {
	releases []func()
	workers  []timeoutQueueTestWorker
}

func (tasks *timeoutQueueTestTasks) release(fn func()) func() {
	release := sync.OnceFunc(fn)
	tasks.releases = append(tasks.releases, release)
	return release
}

func (tasks *timeoutQueueTestTasks) barrier() (<-chan struct{}, func()) {
	resume := make(chan struct{})
	return resume, tasks.release(func() { close(resume) })
}

func (tasks *timeoutQueueTestTasks) start(label string, fn func()) <-chan struct{} {
	done := make(chan struct{})
	tasks.workers = append(tasks.workers, timeoutQueueTestWorker{label, done})
	go func() {
		defer close(done)
		fn()
	}()
	return done
}

func (tasks *timeoutQueueTestTasks) finish(t *testing.T) {
	t.Helper()
	// Release every barrier before joining any worker, including on Fatal paths.
	for _, release := range tasks.releases {
		release()
	}
	if len(tasks.workers) == 0 {
		return
	}
	// Bound the entire cleanup group, not five seconds per stalled worker.
	timer := time.NewTimer(timeoutQueueTestWait)
	defer timer.Stop()
	expired := false
	for _, worker := range tasks.workers {
		select {
		case <-worker.done:
			continue
		default:
		}
		if !expired {
			select {
			case <-worker.done:
				continue
			case <-timer.C:
				expired = true
			}
		}
		t.Errorf("TIMEOUT_QUEUE_CLEANUP_JOIN: %s", worker.label)
	}
}

type controlledTimeoutTimer struct{}

func (controlledTimeoutTimer) request(time.Duration) {}
func controlledTimeoutQueue() (*timeoutQueue, *atomic.Int64) {
	c := &atomic.Int64{}
	q := newTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.now = c.Load
		shard.timer = controlledTimeoutTimer{}
	}
	return q, c
}

func armTimeoutQueueForTest(tx *Transaction, q *timeoutQueue, deadline int64) {
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	tx.timeoutArmed = true
	tx.timeoutArm++
	if tx.timeoutNode == nil {
		tx.timeoutNode = q.newNode(tx)
	}
	tx.timeoutNode.queue.schedule(tx.timeoutNode, deadline)
}

func TestTimeoutQueueLogicalContinuationFIFO(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	a, b := &Transaction{}, &Transaction{}
	armTimeoutQueueForTest(b, q, int64(100*time.Second))
	clock.Store(int64(2 * time.Second))
	armTimeoutQueueForTest(a, q, int64(100*time.Second))
	clock.Store(int64(3 * time.Second))
	armTimeoutQueueForTest(a, q, int64(100*time.Second))
	clock.Store(int64(4 * time.Second))
	armTimeoutQueueForTest(b, q, int64(100*time.Second))
	if b.timeoutNode.wake != int64(10*time.Second) || a.timeoutNode.wake != int64(12*time.Second) {
		t.Fatal("stub population wrong")
	}
	clock.Store(int64(15 * time.Second))
	q.fire()
	if a.timeoutNode.seq != 5 || b.timeoutNode.seq != 6 || a.timeoutNode.wake != int64(25*time.Second) || b.timeoutNode.wake != int64(25*time.Second) {
		t.Fatalf("CONTINUATION_FIFO: A=%d B=%d wakeA=%d wakeB=%d", a.timeoutNode.seq, b.timeoutNode.seq, a.timeoutNode.wake, b.timeoutNode.wake)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestTimeoutQueueRearmAndDisarmNeedNoQueueLock(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	tx := &Transaction{}
	armTimeoutQueueForTest(tx, q, int64(100*time.Second))
	clock.Store(int64(4 * time.Second))
	q.mu.Lock()
	unlock := tasks.release(q.mu.Unlock)
	done := tasks.start("rearm/disarm worker", func() {
		armTimeoutQueueForTest(tx, q, int64(100*time.Second))
		tx.readErrMu.Lock()
		tx.disarmTimeoutLocked()
		tx.readErrMu.Unlock()
	})
	select {
	case <-done:
		unlock()
	case <-time.After(time.Second):
		unlock()
		awaitTimeoutQueueTest(t, done, "rearm/disarm after queue unlock")
		t.Fatal("FAST_REARM_OR_DISARM_TOOK_QUEUE_LOCK")
	}
	runtime.KeepAlive(tx)
}

func TestTimeoutQueueStubRepairIsNotActorResumption(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	tx := &Transaction{}
	armTimeoutQueueForTest(tx, q, int64(100*time.Second))
	clock.Store(int64(4 * time.Second))
	armTimeoutQueueForTest(tx, q, int64(100*time.Second))
	clock.Store(int64(10 * time.Second))
	q.fire()
	if tx.timeoutNode.wake != int64(14*time.Second) || tx.timeoutNode.seq != 2 || q.root.seq.Load() != 2 {
		t.Fatal("STUB_REPAIR_TOOK_SEQUENCE")
	}
	clock.Store(int64(14 * time.Second))
	q.fire()
	if tx.timeoutNode.seq != 3 || tx.timeoutNode.wake != int64(24*time.Second) {
		t.Fatal("ACTUAL_INTERVAL_DID_NOT_REQUEUE")
	}
	runtime.KeepAlive(tx)
}

func TestTimeoutQueueEarlierKeyIsEager(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	tx := &Transaction{}
	armTimeoutQueueForTest(tx, q, int64(100*time.Second))
	clock.Store(int64(time.Second))
	armTimeoutQueueForTest(tx, q, int64(2*time.Second))
	if tx.timeoutNode.wake != int64(2*time.Second+100*time.Microsecond) || q.reqAt != tx.timeoutNode.wake {
		t.Fatal("EARLIER_KEY_LOST_WAKE")
	}
	runtime.KeepAlive(tx)
}

func TestTimeoutQueuePopRaceWithRearm(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	tx := &Transaction{}
	armTimeoutQueueForTest(tx, q, int64(time.Second))
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	q.beforeDeliver = func(*timeoutNode) {
		close(parked)
		waitTimeoutQueueTest(t, resume, "pop/rearm delivery barrier")
	}
	clock.Store(int64(time.Second + 100*time.Microsecond))
	done := tasks.start("pop/rearm drainer", q.fire)
	awaitTimeoutQueueTest(t, parked, "pop/rearm delivery pause")
	armTimeoutQueueForTest(tx, q, int64(100*time.Second))
	release()
	awaitTimeoutQueueTest(t, done, "pop/rearm drainer completion")
	if tx.readLife != nil || !tx.timeoutArmed || !tx.timeoutNode.queued.Load() {
		t.Fatal("STALE_POP_FIRED")
	}
	runtime.KeepAlive(tx)
}

func TestTimeoutQueueEagerSequencePublishedWithInsertion(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	a, b, d := &Transaction{}, &Transaction{}, &Transaction{}
	var order []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { order = append(order, tx) }
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	q.beforeEagerSchedule = func(n *timeoutNode) {
		if n.tx.Value() == a {
			close(parked)
			waitTimeoutQueueTest(t, resume, "eager publication barrier")
		}
	}
	done := tasks.start("eager publication armer", func() {
		armTimeoutQueueForTest(a, q, int64(time.Second))
	})
	awaitTimeoutQueueTest(t, parked, "eager publication pause")
	armTimeoutQueueForTest(d, q, int64(2*time.Second))
	armTimeoutQueueForTest(b, q, int64(time.Second))
	clock.Store(int64(1500 * time.Millisecond))
	q.fire()
	release()
	awaitTimeoutQueueTest(t, done, "eager publication armer completion")
	clock.Store(int64(3 * time.Second))
	q.fire()
	if len(order) != 3 || order[0] != b || order[1] != d || order[2] != a {
		t.Fatalf("EAGER_PUBLICATION_FIFO: got %v, want [%p %p %p]", order, b, d, a)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	runtime.KeepAlive(d)
}

func TestTimeoutQueueCrossShardUnequalWakes(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	a, b := &Transaction{}, &Transaction{}
	b.timeoutNode = q.newNode(b)
	a.timeoutNode = q.newNode(a)
	armTimeoutQueueForTest(a, q, int64(20*time.Millisecond))
	armTimeoutQueueForTest(b, q, int64(10*time.Millisecond))
	var order []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { order = append(order, tx) }
	clock.Store(int64(25 * time.Millisecond))
	q.fire()
	if a.timeoutNode.queue == b.timeoutNode.queue || len(order) != 2 || order[0] != a || order[1] != b {
		t.Fatalf("CROSS_SHARD_FIFO: got %v, want [%p %p] on distinct shards", order, a, b)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestTimeoutQueueGlobalDrainerCallbackRescans(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	a, b, c := &Transaction{}, &Transaction{}, &Transaction{}
	armTimeoutQueueForTest(a, q, int64(time.Millisecond))
	armTimeoutQueueForTest(b, q, int64(time.Millisecond))
	var order []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { order = append(order, tx) }
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	q.beforeDeliver = func(n *timeoutNode) {
		if n == a.timeoutNode {
			close(parked)
			waitTimeoutQueueTest(t, resume, "global drainer delivery barrier")
		}
	}
	clock.Store(int64(2 * time.Millisecond))
	done := tasks.start("global callback drainer", q.fire)
	awaitTimeoutQueueTest(t, parked, "global drainer delivery pause")
	armTimeoutQueueForTest(c, q, int64(3*time.Millisecond))
	clock.Store(int64(4 * time.Millisecond))
	b.timeoutNode.queue.fire()
	if len(order) != 0 {
		t.Fatal("GLOBAL_DRAINER_OVERLAP: another shard delivered while the owner was paused")
	}
	release()
	awaitTimeoutQueueTest(t, done, "global callback drainer completion")
	if len(order) != 3 || order[0] != a || order[1] != b || order[2] != c {
		t.Fatalf("GLOBAL_DRAINER_RESCAN: got %v, want [%p %p %p]", order, a, b, c)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	runtime.KeepAlive(c)
}

func TestTimeoutQueueShardCohortFrontier(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	a, b := &Transaction{}, &Transaction{}
	a.timeoutNode = q.newNode(a)
	b.timeoutNode = q.newNode(b)
	parkedA, parkedB := make(chan struct{}), make(chan struct{})
	resumeA, releaseA := tasks.barrier()
	resumeB, releaseB := tasks.barrier()
	a.timeoutNode.queue.beforeEagerSchedule = func(*timeoutNode) {
		close(parkedA)
		waitTimeoutQueueTest(t, resumeA, "cohort armer A barrier")
	}
	b.timeoutNode.queue.beforeEagerSchedule = func(*timeoutNode) {
		close(parkedB)
		waitTimeoutQueueTest(t, resumeB, "cohort armer B barrier")
	}
	doneA := tasks.start("cohort armer A", func() { armTimeoutQueueForTest(a, q, int64(time.Second)) })
	awaitTimeoutQueueTest(t, parkedA, "cohort armer A pause")
	doneB := tasks.start("cohort armer B", func() { armTimeoutQueueForTest(b, q, int64(time.Second)) })
	awaitTimeoutQueueTest(t, parkedB, "cohort armer B pause")
	published := false
	q.root.afterScan = func(shard *timeoutQueue) {
		if shard == a.timeoutNode.queue && !published {
			published = true
			releaseA()
			awaitTimeoutQueueTest(t, doneA, "cohort armer A publication")
			releaseB()
			awaitTimeoutQueueTest(t, doneB, "cohort armer B publication")
		}
	}
	var order []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { order = append(order, tx) }
	clock.Store(int64(15 * time.Second))
	q.fire()
	q.fire()
	if !published || a.timeoutNode.seq != 1 || b.timeoutNode.seq != 2 || len(order) != 2 || order[0] != a || order[1] != b {
		t.Fatalf("SHARD_COHORT_FRONTIER: published=%t seqA=%d seqB=%d got %v, want [%p %p]", published, a.timeoutNode.seq, b.timeoutNode.seq, order, a, b)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestTimeoutQueueDeferredHeadDoesNotHideEligibleWork(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	old, late := &Transaction{}, &Transaction{}
	armTimeoutQueueForTest(old, q, int64(2*time.Second))
	for range 15 {
		q.newNode(&Transaction{})
	}
	late.timeoutNode = q.newNode(late)
	if late.timeoutNode.queue != old.timeoutNode.queue {
		t.Fatal("probe must place the deferred head and eligible work in the same shard")
	}
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	late.timeoutNode.queue.beforeEagerSchedule = func(*timeoutNode) {
		close(parked)
		waitTimeoutQueueTest(t, resume, "deferred head armer barrier")
	}
	done := tasks.start("deferred head armer", func() { armTimeoutQueueForTest(late, q, int64(time.Second)) })
	awaitTimeoutQueueTest(t, parked, "deferred head armer pause")
	cohort := 0
	q.root.afterFrontier = func(_ int64, _ uint64) {
		cohort++
		if cohort == 1 {
			release()
			awaitTimeoutQueueTest(t, done, "deferred head publication")
		}
	}
	var order []*Transaction
	var deliveredIn []int
	q.deliver = func(tx *Transaction, _ uint64) {
		order = append(order, tx)
		deliveredIn = append(deliveredIn, cohort)
	}
	clock.Store(int64(3 * time.Second))
	q.fire()
	if len(order) != 2 || order[0] != old || order[1] != late || deliveredIn[0] != 1 || deliveredIn[1] != 2 {
		t.Fatalf("DEFERRED_HEAD_COHORT: got %v in cohorts %v, want [%p %p] in [1 2]", order, deliveredIn, old, late)
	}
	runtime.KeepAlive(old)
	runtime.KeepAlive(late)
}

type hookedTimeoutTimer struct{ requestFn func(time.Duration) }

func (timer hookedTimeoutTimer) request(d time.Duration) { timer.requestFn(d) }

func TestTimeoutQueueStaggeredExitCallbackKeepsWake(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	a, future := &Transaction{}, &Transaction{}
	a.timeoutNode = q.newNode(a)
	armTimeoutQueueForTest(future, q, int64(100*time.Second))
	later := future.timeoutNode.queue
	if a.timeoutNode.queue != q || later == q {
		t.Fatal("probe requires A on shard0 and a future head on a later shard")
	}
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	paused := false
	later.timer = hookedTimeoutTimer{requestFn: func(time.Duration) {
		if !paused {
			paused = true
			close(parked)
			waitTimeoutQueueTest(t, resume, "staggered exit timer barrier")
		}
	}}
	var order []*Transaction
	q.deliver = func(tx *Transaction, _ uint64) { order = append(order, tx) }
	clock.Store(int64(5 * time.Second))
	done := tasks.start("staggered exit drainer", q.fire)
	awaitTimeoutQueueTest(t, parked, "staggered exit timer pause")
	q.mu.Lock()
	exited := !q.running
	q.mu.Unlock()
	if !exited {
		t.Fatal("earlier shard must have finished exit before the callback")
	}
	armTimeoutQueueForTest(a, q, int64(6*time.Second))
	clock.Store(int64(7 * time.Second))
	q.fire()
	release()
	awaitTimeoutQueueTest(t, done, "staggered exit drainer completion")
	if len(order) != 1 || order[0] != a {
		t.Fatalf("STAGGERED_EXIT_LOST_WAKE: got %v, want [%p] without another manual timer fire", order, a)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(future)
}

func TestTimeoutQueueCollectedTransactionUnlinksBeforeWake(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	var n *timeoutNode
	var txWeak weak.Pointer[Transaction]
	control := make(chan struct{})
	func() {
		tx := &Transaction{}
		armTimeoutQueueForTest(tx, q, int64(100*time.Second))
		n = tx.timeoutNode
		txWeak = weak.Make(tx)
		runtime.AddCleanup(tx, func(ch chan struct{}) { close(ch) }, control)
	}()
	runtime.GC()
	select {
	case <-control:
	case <-time.After(2 * time.Second):
		t.Fatal("test cleanup positive control did not run")
	}
	if txWeak.Value() != nil {
		t.Fatal("cleanup ran but the transaction weak pointer remains live")
	}
	until := time.Now().Add(2 * time.Second)
	for {
		n.queue.mu.Lock()
		unlinked := n.index == -1 && !n.queued.Load() && len(n.queue.heap) == 0
		n.queue.mu.Unlock()
		if unlinked {
			break
		}
		if time.Now().After(until) {
			t.Fatal("GC_CLEANUP_DID_NOT_UNLINK: collected node still queued without any timer dispatch")
		}
		time.Sleep(time.Millisecond)
	}
	if clock.Load() != 0 {
		t.Fatal("probe must reclaim before the logical wake clock advances")
	}
}

func TestTimeoutQueueCreateAllocationsWithCleanup(t *testing.T) {
	q, _ := controlledTimeoutQueue()
	for _, shard := range q.root.shards {
		shard.heap = make([]*timeoutNode, 0, 1024)
	}
	db := &Database{db: &database{timeouts: q}}
	db.db.txDefaults.Store(&TransactionDefaults{Timeout: 30000})
	var last *Transaction
	allocs := testing.AllocsPerRun(100, func() { last = db.CreateTransaction() })
	if allocs != 9 {
		t.Fatalf("CLEANUP_CREATE_ALLOC: got %g, want 9", allocs)
	}
	runtime.KeepAlive(last)
}

//go:noinline
func finalizableTimeoutTransaction(q *timeoutQueue) (*timeoutNode, chan *Transaction, chan *Transaction, chan struct{}) {
	db := &Database{db: &database{timeouts: q}}
	db.db.txDefaults.Store(&TransactionDefaults{Timeout: 30000})
	tx := db.CreateTransaction()
	keeper := make(chan *Transaction, 1)
	resurrected := make(chan *Transaction, 1)
	finished := make(chan struct{})
	keeper <- tx
	runtime.SetFinalizer(tx, func(tx *Transaction) {
		resurrected <- tx
		close(finished)
	})
	return tx.timeoutNode, keeper, resurrected, finished
}

func resurrectTimeoutTransaction(t *testing.T, keeper, resurrected chan *Transaction, finished chan struct{}, old *timeoutNode) *Transaction {
	t.Helper()
	select {
	case <-keeper:
	case <-time.After(timeoutQueueTestWait):
		t.Fatal("timeout waiting for resurrection keeper release")
	}
	runtime.GC()
	var held *Transaction
	select {
	case held = <-resurrected:
	case <-time.After(5 * time.Second):
		t.Fatal("resurrection finalizer positive control did not run")
	}
	awaitTimeoutQueueTest(t, finished, "resurrection finalizer completion")
	if old.tx.Value() != nil {
		t.Fatal("finalizer ran without permanently clearing the old weak identity")
	}
	runtime.SetFinalizer(held, nil)
	return held
}

func renewResurrectedTimeout(t *testing.T, held *Transaction, old *timeoutNode) *timeoutNode {
	t.Helper()
	deadline := held.deadlineTime()
	held.SetTimeout(30000)
	return requireTimeoutNodeRenewal(t, held, old, deadline)
}

func requireTimeoutNodeRenewal(t *testing.T, held *Transaction, old *timeoutNode, deadline time.Time) *timeoutNode {
	t.Helper()
	fresh := held.timeoutNode
	if fresh == old {
		t.Fatal("RESURRECTION_NODE_NOT_RENEWED: retained rearm reused a permanently nil weak identity")
	}
	if fresh.queue != old.queue || fresh.tx == old.tx || fresh.tx.Value() != held || old.tx.Value() != nil {
		t.Fatal("renewal did not preserve the shard and create a distinct live weak identity")
	}
	if !held.deadlineTime().Equal(deadline) {
		t.Fatal("weak identity renewal restarted the creation-based deadline")
	}
	old.queue.mu.Lock()
	retired := old.retired && old.index == -1 && !old.queued.Load()
	old.queue.mu.Unlock()
	if !retired {
		t.Fatal("old weak node was not retired and withdrawn")
	}
	return fresh
}

func requireRenewedTimeoutDelivery(t *testing.T, q *timeoutQueue, clock *atomic.Int64, held *Transaction, old, fresh *timeoutNode) {
	t.Helper()
	// Exercise the eventual old cleanup's body directly. Real cleanup waits
	// until the retained receiver becomes unreachable again.
	old.queue.remove(old)
	fresh.queue.mu.Lock()
	stillQueued := fresh.index >= 0 && fresh.queued.Load()
	fresh.queue.mu.Unlock()
	if !stillQueued || !held.timeoutArmed {
		t.Fatal("old cleanup or stale collected work withdrew the fresh arm")
	}
	clock.Store(fresh.deadline + int64(100*time.Microsecond))
	q.fire()
	_, err := held.GetVersionstamp()
	var failure *wire.FDBError
	if !errors.As(err, &failure) || failure.Code != 1031 {
		t.Fatalf("RESURRECTION_RENEWED_DELIVERY: got %v, want 1031", err)
	}
	runtime.KeepAlive(held)
}

func TestTimeoutQueueResurrectionAfterWeakNilDispatch(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	old, keeper, resurrected, finished := finalizableTimeoutTransaction(q)
	held := resurrectTimeoutTransaction(t, keeper, resurrected, finished, old)
	clock.Store(old.deadline + int64(100*time.Microsecond))
	q.fire()
	old.queue.mu.Lock()
	gone := old.index == -1 && !old.queued.Load()
	old.queue.mu.Unlock()
	if !gone {
		t.Fatal("weak-nil dispatch did not drop the old physical node")
	}
	held.readErrMu.Lock()
	idleCause := held.readLife.cause
	held.readErrMu.Unlock()
	if idleCause != nil {
		t.Fatalf("inherited idle-resurrection boundary unexpectedly delivered: %v", idleCause)
	}
	fresh := renewResurrectedTimeout(t, held, old)
	requireRenewedTimeoutDelivery(t, q, clock, held, old, fresh)
}

func TestTimeoutQueueResurrectionReset(t *testing.T) {
	q, clock := controlledTimeoutQueue()
	old, keeper, resurrected, finished := finalizableTimeoutTransaction(q)
	held := resurrectTimeoutTransaction(t, keeper, resurrected, finished, old)
	created := held.creationTime
	held.Reset()
	if !held.creationTime.After(created) || held.timeoutNs.Load() != int64(30*time.Second) {
		t.Fatal("Reset did not restart creation time and restore the positive database timeout")
	}
	fresh := requireTimeoutNodeRenewal(t, held, old, held.creationTime.Add(30*time.Second))
	requireRenewedTimeoutDelivery(t, q, clock, held, old, fresh)
}

func TestTimeoutQueueResurrectionRenewalAgainstCollectedNode(t *testing.T) {
	for _, stage := range []string{"popped-before-weak-read", "immutable-ready-record"} {
		t.Run(stage, func(t *testing.T) {
			var tasks timeoutQueueTestTasks
			defer tasks.finish(t)
			q, clock := controlledTimeoutQueue()
			old, keeper, resurrected, finished := finalizableTimeoutTransaction(q)
			parked := make(chan struct{})
			resume, release := tasks.barrier()
			paused := false
			pause := func() {
				if !paused {
					paused = true
					close(parked)
					waitTimeoutQueueTest(t, resume, "resurrection drainer barrier")
				}
			}
			if stage == "popped-before-weak-read" {
				q.root.afterPop = func(n *timeoutNode) {
					if n == old {
						pause()
					}
				}
			} else {
				q.root.afterScan = func(shard *timeoutQueue) {
					if shard == old.queue {
						pause()
					}
				}
			}
			clock.Store(old.wake)
			done := tasks.start("resurrection drainer", q.fire)
			awaitTimeoutQueueTest(t, parked, "resurrection drainer pause")
			if stage == "immutable-ready-record" {
				old.queue.mu.Lock()
				inBatch := old.index == inReady
				old.queue.mu.Unlock()
				found := false
				for _, r := range q.root.ready {
					found = found || r.node == old
				}
				if !inBatch || !found {
					t.Fatal("immutable-ready probe did not collect the old node")
				}
			}
			held := resurrectTimeoutTransaction(t, keeper, resurrected, finished, old)
			fresh := renewResurrectedTimeout(t, held, old)
			release()
			awaitTimeoutQueueTest(t, done, "resurrection drainer completion")
			if stage == "immutable-ready-record" {
				q.root.afterScan = nil
			} else {
				q.root.afterPop = nil
			}
			requireRenewedTimeoutDelivery(t, q, clock, held, old, fresh)
		})
	}
}

func TestTimeoutQueueExistingNodeRearmAllocations(t *testing.T) {
	q, _ := controlledTimeoutQueue()
	db := &Database{db: &database{timeouts: q}}
	db.db.txDefaults.Store(&TransactionDefaults{Timeout: 30000})
	tx := db.CreateTransaction()
	original := tx.timeoutNode
	allocs := testing.AllocsPerRun(1000, func() {
		tx.readErrMu.Lock()
		tx.armTimeoutLocked()
		tx.readErrMu.Unlock()
	})
	if allocs != 0 || tx.timeoutNode != original {
		t.Fatalf("LIVE_NODE_REARM: allocs=%g changed=%t", allocs, tx.timeoutNode != original)
	}
	runtime.KeepAlive(tx)
}

func TestTimeoutQueueFreshAssociationAllocations(t *testing.T) {
	q, _ := controlledTimeoutQueue()
	var held *Transaction
	carrier := testing.AllocsPerRun(1000, func() { held = &Transaction{} })
	// Each owner must be fresh: repeatedly calling weak.Make on one live owner
	// reuses its runtime weak handle and would undercount true renewal by one.
	allocs := testing.AllocsPerRun(1000, func() {
		held = &Transaction{}
		runtime.KeepAlive(q.newNodeForShard(held))
	})
	if carrier != 1 || allocs-carrier != 4 {
		t.Fatalf("FRESH_ASSOCIATION_ALLOC: got %g minus carrier %g, want 4 (node, weak handle, cleanup registration)", allocs, carrier)
	}
	runtime.KeepAlive(held)
}

func TestTimeoutQueueLayoutAndConstructionCosts(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() { runtime.KeepAlive(newTimeoutQueue()) })
	if unsafe.Sizeof(timeoutNode{}) != 64 {
		t.Fatalf("node size changed: %d", unsafe.Sizeof(timeoutNode{}))
	}
	t.Logf("LAYOUT queue=%d root=%d node=%d ready=%d construction_allocations=%g", unsafe.Sizeof(timeoutQueue{}), unsafe.Sizeof(timeoutQueueRoot{}), unsafe.Sizeof(timeoutNode{}), unsafe.Sizeof(timeoutReady{}), allocs)
}

func TestTimeoutQueueConcurrentArms(t *testing.T) {
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	q, clock := controlledTimeoutQueue()
	q.deliver = func(*Transaction, uint64) {}
	txs := make([]*Transaction, 32)
	for i := range txs {
		txs[i] = &Transaction{}
		tx := txs[i]
		tasks.start("concurrent armer", func() {
			for arm := 0; arm < 1000; arm++ {
				armTimeoutQueueForTest(tx, q, clock.Load()+int64(100*time.Second))
				if arm%2 == 0 {
					tx.readErrMu.Lock()
					tx.disarmTimeoutLocked()
					tx.readErrMu.Unlock()
				}
				if arm%10 == 0 {
					clock.Add(int64(100 * time.Millisecond))
					q.fire()
				}
			}
		})
	}
	for _, worker := range tasks.workers {
		awaitTimeoutQueueTest(t, worker.done, worker.label)
	}
	runtime.KeepAlive(txs)
}
