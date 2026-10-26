package client

import (
	"context"
	"testing"
	"time"
)

// Hand an admitted token to the private Commit continuation so asynchronous
// AfterFunc goroutine-cache growth cannot enter the count. Include producer
// admission and the entire turnover, not just the publication helper.
func TestReadOnlyCommitFinishAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		tx := &Transaction{}
		tx.SetReadVersion(123)
		old := tx.readLife
		op := &readOperation{Context: old.ctx, readAdmission: readAdmission{tx: tx, inc: old, parent: context.Background()}}
		op.lease = tx.enterReadStateInto(op, old, false, false, &op.leaseStorage)
		if op.lease == nil {
			t.Fatal("failed to admit Commit")
		}
		completion, err := tx.admitCommit(op)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.commitAdmitted(op, completion, op.lease); err != nil {
			t.Fatal(err)
		}
		if !tx.hasCommitted || tx.committedVersion != -1 || tx.readLife != nil || tx.readReady != nil || tx.readGen != old.gen+1 {
			t.Fatal("Commit did not publish its lazy replacement")
		}
	})
	if allocs != 8 {
		t.Fatalf("TURNOVER_FINISH_ALLOC: %v allocations, want 8", allocs)
	}
	t.Logf("TURNOVER_FINISH_ALLOC: %v", allocs)
}

func TestTransactFinalCommitTurnoverAndEscapedHandleSafety(t *testing.T) {
	t.Parallel()
	db := &Database{db: &database{timeouts: newTimeoutQueue()}}
	var escapedTx *Transaction
	var initialSpanTrace [16]byte
	_, err := db.Transact(context.Background(), func(tx *Transaction) (any, error) {
		escapedTx = tx
		tx.SetTimeout(60000)
		tx.SetReadVersion(123)
		initialSpanTrace = tx.currentSpan().TraceID
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Database.Transact = %v", err)
	}
	if !escapedTx.hasCommitted || escapedTx.committedVersion != -1 || escapedTx.readLife != nil || escapedTx.readReady != nil || escapedTx.transactFinalCommit.Load() {
		t.Fatal("Database.Transact final Commit did not complete lazy turnover")
	}
	// If the caller retains tx after Database.Transact returns:
	// 1. currentSpan lazily regenerates a fresh trace span (never reuses the committed traceID).
	if got := escapedTx.currentSpan(); got.TraceID == initialSpanTrace || got.SpanID == 0 {
		t.Fatalf("currentSpan after Transact final commit = %+v, want fresh TraceID != %x", got, initialSpanTrace)
	}
	// 2. GetVersionstamp still reports 2021 (read_only_transaction) and lazily arms the
	// remaining timeout on the escaped handle.
	_, err = escapedTx.GetVersionstamp()
	requireLazySuccessorCode(t, err, 2021)
	escapedTx.readErrMu.Lock()
	arm, pending := escapedTx.timeoutArm, escapedTx.timeoutPendingLocked()
	escapedTx.withdrawTimeoutLocked()
	escapedTx.readErrMu.Unlock()
	if !pending {
		t.Fatal("escaped Transact handle did not lazily arm its timeout on post-Transact entry")
	}
	// 3. Delivering the armed timeout on the escaped handle records 1031.
	escapedTx.fireTimeout(arm)
	ctx, release := escapedTx.opContext(context.Background())
	defer release()
	requireLazySuccessorCode(t, escapedTx.readEntryError(ctx), 1031)

	// Also verify a handle retained from Database.ReadTransact preserves its TIMEOUT
	// when reused after ReadTransact returns (both before and after deadline expiry).
	var escapedReadTx *Transaction
	_, err = db.ReadTransact(context.Background(), func(tx *Transaction) (any, error) {
		escapedReadTx = tx
		tx.SetTimeout(60000)
		tx.SetReadVersion(456)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Database.ReadTransact = %v", err)
	}
	escapedReadTx.deadlineNs.Store(time.Now().Add(-2 * time.Minute).UnixNano())
	rCtx, rRelease := escapedReadTx.opContext(context.Background())
	defer rRelease()
	requireLazySuccessorCode(t, escapedReadTx.readEntryError(rCtx), 1031)
}

func TestTurnoverFinishPublishesOnUnwind(t *testing.T) {
	t.Parallel()
	for _, observation := range []string{"unobserved", "observed"} {
		t.Run(observation, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			t.Cleanup(tx.Cancel)
			marker := &struct{}{}
			var ready chan struct{}
			var waiter *readinessTestWaiter
			func() {
				defer func() {
					if got := recover(); got != marker {
						t.Fatalf("panic=%v, want unwind marker", got)
					}
				}()
				finish := tx.startTurnover()
				defer finish.finish()
				ready = tx.readReady
				if ready != unobservedReadReady {
					t.Fatal("turnover allocated an unobserved channel")
				}
				if observation == "observed" {
					tx.readErrMu.Lock()
					inc := tx.readIncarnationLocked()
					tx.readErrMu.Unlock()
					ctx := &readinessTestContext{Context: context.Background(), tx: tx, called: make(chan chan struct{}, 1)}
					// Stack unwind always publishes before test cleanup drains.
					waiter = startReadinessTestWaiter(t, func() {}, func() *executionLease { return tx.enterReadState(ctx, inc, true) })
					ready = receiveReadinessTest(t, ctx.called)
					if ready == nil || ready == unobservedReadReady {
						t.Fatal("unwind waiter did not register a private channel")
					}
					waiter.requireBlocked(t)
				}
				panic(marker)
			}()
			if observation == "unobserved" {
				requireReadinessTestOpen(t, ready)
			} else {
				receiveReadinessTest(t, ready)
				lease := waiter.receive(t)
				defer lease.release()
				if lease == nil || lease.inc != tx.readLife {
					t.Fatal("deferred finish did not admit its waiter")
				}
			}
			if !tx.readErrMu.TryLock() {
				t.Fatal("deferred finish retained the leaf lock")
			}
			barrier := tx.readReady
			tx.readErrMu.Unlock()
			if barrier != nil {
				t.Fatal("deferred finish retained the readiness barrier")
			}
			if !tx.resetMu.TryLock() {
				t.Fatal("deferred finish retained turnover ownership")
			}
			tx.resetMu.Unlock()
		})
	}
}

func TestRejectedTurnoverHasNoFinishOwnership(t *testing.T) {
	t.Parallel()
	reject := func(tx *Transaction, inc *readIncarnation, released *executionLease, want int) {
		t.Helper()
		before, generation, ready := tx.readLife, tx.readGen, tx.readReady
		finish, err := tx.beginTurnover(inc, released)
		if finish.tx != nil {
			t.Fatal("rejected turnover returned publication ownership")
		}
		requireLazySuccessorCode(t, err, want)
		if tx.readLife != before || tx.readGen != generation || tx.readReady != ready {
			t.Fatal("rejected turnover changed the current incarnation")
		}
		if !tx.resetMu.TryLock() {
			t.Fatal("rejected turnover retained turnover ownership")
		}
		tx.resetMu.Unlock()
	}
	tx := &Transaction{}
	active := tx.enterState()
	reject(tx, active.inc, active, 2000)
	active.release()
	foreignTx := &Transaction{}
	foreign := foreignTx.enterState()
	reject(tx, active.inc, foreign, 2000)
	foreign.release()
	finish, err := tx.beginTurnover(active.inc, active)
	if err != nil {
		t.Fatal(err)
	}
	finish.finish()
	current := tx.enterState()
	current.release()
	reject(tx, active.inc, active, 1025)
	tx.Cancel()
	foreignTx.Cancel()
}
