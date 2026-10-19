package client

import (
	"context"
	"testing"
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
	if allocs != 9 {
		t.Fatalf("TURNOVER_FINISH_ALLOC: %v allocations, want 9", allocs)
	}
	t.Logf("TURNOVER_FINISH_ALLOC: %v", allocs)
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
