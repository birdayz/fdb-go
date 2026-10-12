package client

import (
	"context"
	"testing"
)

// Use a valid admitted context so asynchronous AfterFunc goroutine-cache growth
// cannot enter the count. The public Commit still performs the entire turnover.
func TestReadOnlyCommitFinishAllocations(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		tx := &Transaction{}
		tx.SetReadVersion(123)
		old := tx.readLife
		op := &readOperation{Context: old.ctx, tx: tx, inc: old, parent: context.Background()}
		op.lease = tx.enterReadStateInto(op, old, false, false, &op.leaseStorage)
		if op.lease == nil {
			t.Fatal("failed to admit Commit")
		}
		if err := tx.Commit(op); err != nil {
			t.Fatal(err)
		}
		if !tx.hasCommitted || tx.committedVersion != -1 || tx.readLife != nil || tx.readReady != nil || tx.readGen != old.gen+1 {
			t.Fatal("Commit did not publish its lazy replacement")
		}
	})
	if allocs != 11 {
		t.Fatalf("TURNOVER_FINISH_ALLOC: %v allocations, want 11", allocs)
	}
	t.Logf("TURNOVER_FINISH_ALLOC: %v", allocs)
}

func TestTurnoverFinishPublishesOnUnwind(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	marker := &struct{}{}
	var ready <-chan struct{}
	func() {
		defer func() {
			if got := recover(); got != marker {
				t.Fatalf("panic=%v, want unwind marker", got)
			}
		}()
		finish := tx.startTurnover()
		ready = tx.readReady
		defer finish.finish()
		panic(marker)
	}()
	select {
	case <-ready:
	default:
		t.Fatal("deferred finish did not close the publication channel")
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
}

func TestRejectedTurnoverHasNoFinishOwnership(t *testing.T) {
	t.Parallel()
	reject := func(tx *Transaction, inc *readIncarnation, released *executionLease, want int) {
		t.Helper()
		before, generation, ready := tx.readLife, tx.readGen, tx.readReady
		finish, err := tx.beginTurnover(inc, released)
		if finish.tx != nil || finish.ready != nil {
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
