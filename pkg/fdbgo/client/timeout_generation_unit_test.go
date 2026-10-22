package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func armedTimeoutTransaction(t *testing.T) (*Transaction, *timeoutQueue, func()) {
	t.Helper()
	q, clock, _ := manualTimeoutQueue()
	tx := timeoutTestTransaction(q)
	tx.SetTimeout(time.Hour.Milliseconds())
	t.Cleanup(tx.Cancel)
	fire := func() { clock.Store(max(tx.timeoutNode.wake, tx.timeoutNode.deadline+int64(timeoutSlack))); q.fire() }
	return tx, q, fire
}

// pauseTimeoutDelivery parks a popped, claimed arm before fireTimeout can
// acquire readErrMu. The queue lock is not held at this boundary.
func pauseTimeoutDelivery(t *testing.T, tx *Transaction, q *timeoutQueue, fire func()) (func(), <-chan struct{}) {
	t.Helper()
	var tasks timeoutQueueTestTasks
	t.Cleanup(func() { tasks.finish(t) })
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	q.beforeDeliver = func(n *timeoutNode) {
		if n == tx.timeoutNode {
			close(parked)
			waitTimeoutQueueTest(t, resume, "TIMEOUT_GENERATION_DELIVERY_RESUME: beforeDeliver release")
		}
	}
	done := tasks.start("generation timeout delivery drainer", fire)
	awaitTimeoutQueueTest(t, parked, "TIMEOUT_GENERATION_DELIVERY_PAUSE: beforeDeliver did not park")
	return release, done
}

// Mutation: ignore arm identity in fireTimeout.
func TestTimeoutPopThenRearm(t *testing.T) {
	t.Parallel()
	tx, q, fire := armedTimeoutTransaction(t)
	inc, node, arm := tx.readLife, tx.timeoutNode, tx.timeoutArm
	resume, done := pauseTimeoutDelivery(t, tx, q, fire)
	tx.SetTimeout(2 * time.Hour.Milliseconds())
	resume()
	awaitTimeoutQueueTest(t, done, "stale arm delivery after rearm")
	if tx.readIncarnationCause(inc) != nil || !tx.timeoutArmed || tx.timeoutArm == arm || tx.timeoutNode != node {
		t.Fatal("STALE_REARM: popped old arm fired or replaced the reusable node")
	}
	q.beforeDeliver = nil
	fire()
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
}

// Mutations: ignore timeoutArmed during turnover; ignore arm identity after
// publication. Each subtest exposes one guard without the other masking it.
func TestTimeoutPopThenReset(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"mid-turnover", "after-publication"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			tx, q, fire := armedTimeoutTransaction(t)
			old, arm := tx.readLife, tx.timeoutArm
			resume, done := pauseTimeoutDelivery(t, tx, q, fire)
			token := tx.startTurnover()
			finish := sync.OnceFunc(token.finish)
			defer finish()
			tx.resetFields(false) // retry preserves the absolute timeout budget
			if phase == "after-publication" {
				finish()
			}
			resume()
			awaitTimeoutQueueTest(t, done, "stale arm delivery during "+phase)
			if phase == "mid-turnover" {
				if tx.readLife != nil || tx.timeoutArmed || tx.timeoutArm != arm {
					t.Error("STALE_MID_TURNOVER: stale arm constructed or fired an unpublished successor")
				}
				finish()
			}
			inc := tx.readLife
			if inc == nil || inc == old || inc.cause != nil || !tx.timeoutArmed || tx.timeoutArm == arm {
				t.Fatal("STALE_PUBLISHED: popped old arm poisoned or disarmed the published successor")
			}
			q.beforeDeliver = nil
			fire()
			requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
		})
	}
}

// Mutation: turnover overwrites the timeout's already-recorded cause.
func TestTimeoutRecordThenReset(t *testing.T) {
	t.Parallel()
	tx, _, fire := armedTimeoutTransaction(t)
	old := tx.readLife
	var tasks timeoutQueueTestTasks
	defer tasks.finish(t)
	parked := make(chan struct{})
	resume, release := tasks.barrier()
	tx.afterTimeoutRecord = func() {
		close(parked)
		waitTimeoutQueueTest(t, resume, "TIMEOUT_GENERATION_RECORD_RESUME: afterTimeoutRecord release")
	}
	done := tasks.start("generation timeout record drainer", fire)
	awaitTimeoutQueueTest(t, parked, "TIMEOUT_GENERATION_RECORD_PAUSE: afterTimeoutRecord did not park")
	token := tx.startTurnover()
	tx.resetFields(false)
	token.finish()
	release()
	awaitTimeoutQueueTest(t, done, "timeout record delivery after reset publication")
	requireTimeoutCode(t, context.Cause(old.ctx), ErrTransactionTimedOut)
	if tx.readLife == old || tx.readLife.cause != nil || !tx.timeoutArmed {
		t.Fatal("RECORD_THEN_RESET: successor inherited old recorded timeout")
	}
}

// Mutation: overwrite a previously recorded cause in recordCauseLocked.
func TestTimeoutPopThenCancel(t *testing.T) {
	t.Parallel()
	tx, q, fire := armedTimeoutTransaction(t)
	inc := tx.readLife
	resume, done := pauseTimeoutDelivery(t, tx, q, fire)
	tx.Cancel()
	first := tx.readIncarnationCause(inc)
	resume()
	awaitTimeoutQueueTest(t, done, "stale arm delivery after Cancel")
	tx.Cancel() // a second public publication must preserve the first error too
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionCancelled)
	requireTimeoutCode(t, tx.readIncarnationCause(inc), ErrTransactionCancelled)
	if tx.readIncarnationCause(inc) != first {
		t.Fatal("CANCEL_FIRST_RECORDED: later publication replaced the first cause")
	}
}

// Mutation: overwrite a previously recorded cause in recordCauseLocked.
func TestCancelAfterTimeout(t *testing.T) {
	t.Parallel()
	tx, _, fire := armedTimeoutTransaction(t)
	inc := tx.readLife
	fire()
	tx.Cancel()
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
	requireTimeoutCode(t, tx.readIncarnationCause(inc), ErrTransactionTimedOut)
}

// Real retirement always records the old cause first, so a later publication
// returns at recordCauseLocked's sticky-cause guard. The identity check below
// is defence in depth and is not reachable end to end on this path.
func TestCauseOnRetiredIncarnationKeepsSuccessorArm(t *testing.T) {
	t.Parallel()
	tx, _, _ := armedTimeoutTransaction(t)
	old := tx.readLife
	token := tx.startTurnover()
	tx.resetFields(false)
	token.finish()
	arm := tx.timeoutArm
	tx.failCapturedIncarnation(old, &wire.FDBError{Code: ErrTransactionTimedOut})
	if !tx.timeoutArmed || tx.timeoutArm != arm || tx.readLife.cause != nil {
		t.Fatal("RETIRED_CAUSE: retired publication disarmed its clean successor")
	}
	requireTimeoutCode(t, context.Cause(old.ctx), ErrTransactionCancelled)
}

// Synthetic clean-retired state pins a defence-in-depth guard. Production
// turnover always records the cause before retirement, so removing only the
// identity guard is NOT an end-to-end reachable mutation.
// Mutation: disarm unconditionally instead of checking inc == tx.readLife.
func TestCauseOnCleanRetiredIncarnationGuardSynthetic(t *testing.T) {
	t.Parallel()
	tx, _, _ := armedTimeoutTransaction(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	retired := &readIncarnation{ctx: ctx, cancel: cancel, gen: tx.readGen - 1}
	tx.readErrMu.Lock()
	tx.recordCauseLocked(retired, &wire.FDBError{Code: ErrTransactionTimedOut})
	armed := tx.timeoutArmed
	tx.readErrMu.Unlock()
	if !armed {
		t.Fatal("SYNTHETIC_RETIRED_GUARD: clean retired cause disarmed successor")
	}
}

// The unmaterialized variant exercises the Phase1 generation machinery directly;
// production positive-timeout successors remain eager until the separate B3a.
// Mutation: construction arms and synchronously compares the old deadline.
func TestPendingTimeoutDisabledBeforeDelivery(t *testing.T) {
	t.Parallel()
	for _, unmaterialized := range []bool{false, true} {
		tx, q, fire := armedTimeoutTransaction(t)
		// Model elapsed wall time without dispatching the manual queue. Only
		// delivery (or a new expired arm), not construction, may record 1031.
		tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())
		if unmaterialized {
			tx.readLife = nil
			tx.readVersionState = nil
		}
		node, arm := tx.timeoutNode, tx.timeoutArm
		tx.SetTimeout(0)
		if tx.timeoutArmed || tx.timeoutArm != arm || tx.timeoutNode != node || timeoutHeapLen(q) != 1 || !node.queued.Load() {
			t.Fatal("DISABLE_PENDING_STUB: disabling changed the arm or eagerly unlinked its physical stub")
		}
		fire()
		if tx.readLife == nil || tx.readLife.cause != nil || tx.timeoutArmed || timeoutHeapLen(q) != 0 {
			t.Fatal("DISABLE_PENDING: disabling an undelivered timeout recorded a cause")
		}
		tx.Set([]byte("key"), []byte("value"))
		if got, err := tx.Get(context.Background(), []byte("key")); string(got) != "value" || err != nil {
			t.Fatalf("DISABLE_PENDING_READ: value=%q err=%v", got, err)
		}
	}
}

// Mutation: construction compares a pending old deadline before option replacement.
func TestPendingTimeoutExtendedBeforeDelivery(t *testing.T) {
	t.Parallel()
	for _, unmaterialized := range []bool{false, true} {
		tx, q, fire := armedTimeoutTransaction(t)
		tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())
		if unmaterialized {
			tx.readLife = nil
			tx.readVersionState = nil
		}
		tx.SetTimeout(2 * time.Hour.Milliseconds())
		if tx.readLife.cause != nil || !tx.timeoutArmed {
			t.Fatal("EXTEND_PENDING: option replacement recorded the old elapsed deadline")
		}
		// A stale callback at the original budget cannot deliver the rearm.
		oldArm := tx.timeoutArm - 1
		tx.fireTimeout(oldArm)
		if tx.readLife.cause != nil {
			t.Fatal("EXTEND_STALE: old arm delivered after extension")
		}
		fire()
		requireTimeoutCode(t, context.Cause(tx.readLife.ctx), ErrTransactionTimedOut)
		if timeoutHeapLen(q) != 0 {
			t.Fatal("delivered rearm remained queued")
		}
	}
}

// An elapsed deadline whose timeout is still pending is no cause (C++ C4,
// RYW:1567-1574): entry checks admit until the queue delivers 1031.
// Mutation: publish 1031 at an entry check once the deadline has passed.
func TestElapsedPendingTimeoutIsNoCause(t *testing.T) {
	t.Parallel()
	tx, _, fire := armedTimeoutTransaction(t)
	tx.Set([]byte("key"), []byte("value"))
	inc := tx.readLife
	// The deadline passes while the manual queue holds the arm undelivered.
	tx.creationTime = time.Now().Add(-2 * time.Hour)
	tx.deadlineNs.Store(tx.creationTime.Add(tx.timeoutDur()).UnixNano())
	if got, err := tx.Get(context.Background(), []byte("key")); err != nil || string(got) != "value" {
		t.Fatalf("PENDING_READ: value=%q err=%v", got, err)
	}
	if cause := tx.readIncarnationCause(inc); cause != nil {
		t.Fatalf("PENDING_CAUSE: %v recorded before delivery", cause)
	}
	fire()
	requireTimeoutCode(t, tx.readIncarnationCause(inc), ErrTransactionTimedOut)
	_, err := tx.Get(context.Background(), []byte("key"))
	requireTimeoutCode(t, err, ErrTransactionTimedOut)
	requireTimeoutCode(t, tx.OnError(context.Background(), &wire.FDBError{Code: 1020}), ErrTransactionTimedOut)
}

// Mutation: skip construction when delivery finds an unmaterialized generation.
func TestDeliveredTimeoutIsSticky(t *testing.T) {
	t.Parallel()
	tx, _, fire := armedTimeoutTransaction(t)
	tx.readLife = nil
	tx.readVersionState = nil
	fire()
	inc := tx.readLife
	if inc == nil {
		t.Fatal("IDLE_GENERATION_DELIVERY: delivery did not construct its live generation")
	}
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
	tx.SetTimeout(0)
	_, err := tx.GetReadVersion(context.Background())
	requireTimeoutCode(t, err, ErrTransactionTimedOut)
}

// Mutation: omit synchronous delivery when an actual arm is already expired.
func TestExpiredArmIsSynchronous(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"SetTimeout", "retry-publication", "OnError"} {
		t.Run(mode, func(t *testing.T) {
			q, clock, timer := manualTimeoutQueue()
			tx := timeoutTestTransaction(q)
			defer tx.Cancel()
			tx.creationTime = time.Now().Add(-time.Hour)
			var node *timeoutNode
			var arm, seq uint64
			var wake int64
			var requests int
			if mode == "SetTimeout" {
				tx.SetTimeout(1)
			} else {
				tx.SetTimeout(2 * time.Hour.Milliseconds())
				node, arm, seq = tx.timeoutNode, tx.timeoutArm, q.root.seq.Load()
				wake = node.wake
				timer = node.queue.timer.(*manualTimeoutTimer)
				requests = timer.requests
				if mode == "retry-publication" {
					token := tx.startTurnover()
					tx.resetFields(false)
					tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())
					token.finish() // OnError's applyPersistentOptions re-arm (C3/C5)
				} else {
					// The deadline has passed but the arm is undelivered: no cause
					// at OnError entry (C4). The retry re-arms from creationTime,
					// already expired, and that records 1031 synchronously (C3).
					tx.creationTime = time.Now().Add(-3 * time.Hour)
					tx.deadlineNs.Store(tx.creationTime.Add(tx.timeoutDur()).UnixNano())
					tx.backoffJitter = func() float64 { return 0 }
					old := tx.readLife
					if err := tx.OnError(context.Background(), &wire.FDBError{Code: 1020}); err != nil {
						t.Fatalf("PENDING_ONERROR: OnError with an undelivered timeout = %v, want retry", err)
					}
					requireTimeoutCode(t, tx.readIncarnationCause(old), ErrTransactionCancelled)
				}
			}
			if tx.readLife == nil || tx.readLife.cause == nil {
				t.Fatal("EXPIRED_ARM_SYNC: arm did not record before returning")
			}
			inc, cause := tx.readLife, tx.readLife.cause
			requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
			if tx.timeoutArmed || tx.timeoutArm != arm || tx.timeoutNode != node || q.root.seq.Load() != seq || timer.requests != requests {
				t.Fatal("EXPIRED_ARM_REGISTRATION: synchronous expiry created a new arm or timer request")
			}
			if node == nil {
				if timeoutHeapLen(q) != 0 {
					t.Fatal("expired initial arm queued a timer")
				}
				return
			}
			if timeoutHeapLen(q) != 1 || !node.queued.Load() || node.wake != wake {
				t.Fatal("EXPIRED_ARM_STUB: synchronous expiry did not preserve the old physical stub")
			}
			clock.Store(wake)
			q.fire()
			if timeoutHeapLen(q) != 0 || tx.readLife != inc || tx.readLife.cause != cause {
				t.Fatal("EXPIRED_ARM_DRAIN: old physical stub remained queued or replaced the sticky cause")
			}
			requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
		})
	}
}

// Mutation: share a mutable exported FDBError to make real delivery allocation-free.
func TestTimeoutDeliveryOwnsError(t *testing.T) {
	t.Parallel()
	a, _, fireA := armedTimeoutTransaction(t)
	b, _, fireB := armedTimeoutTransaction(t)
	fireA()
	fireB()
	ea := a.readLife.cause.(*wire.FDBError)
	eb := b.readLife.cause.(*wire.FDBError)
	if ea == eb {
		t.Fatal("PRIVATE_TIMEOUT_ERROR: deliveries shared a mutable error")
	}
	ea.Code = ErrTransactionCancelled
	if eb.Code != ErrTransactionTimedOut {
		t.Fatal("PRIVATE_TIMEOUT_ERROR: one caller mutated another transaction's cause")
	}
}
