package client

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

func TestFDBVersionstampSelectionBeforeMetrics(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	key := []byte(t.Name())
	tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
	p := pendingStamp(t, ctx, tx)

	// Freeze the first post-reply consumer, not the native completion. A stamp
	// selected at facade return or after metrics cannot pass this gate.
	db.db.metrics.commitLatency.mu.Lock()
	var unlocked sync.Once
	unlock := func() { unlocked.Do(db.db.metrics.commitLatency.mu.Unlock) }
	defer unlock()
	done := make(chan error, 1)
	go func() { done <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, p.completion.done, "native selection before metrics")
	select {
	case err := <-done:
		t.Fatalf("Commit passed the held metrics gate: %v", err)
	default:
	}
	stamp, err := p.Resolve()
	if err != nil || len(stamp) != 10 {
		t.Fatalf("native stamp: %x, %v", stamp, err)
	}
	tx.Reset()
	replacement := pendingStamp(t, ctx, tx)
	unlock()
	awaitCommitLifetimeResult(t, ctx, done, "detached success after Reset")
	requireStampPending(t, replacement)
	if got := readCommitLifetimeValue(t, ctx, db, key); !bytes.Equal(got, stamp) {
		t.Fatalf("selected CommitID differs from stored stamp: %x != %x", stamp, got)
	}
	if _, err := tx.GetCommittedVersion(); fdbCodeOf(err) != 2015 {
		t.Fatalf("stale success published metadata: %v", err)
	}
}

func TestFDBVersionstampRetirementDoesNotCancelCommit(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"cancel", "reset", "timeout", "caller", "late-poison"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			db := openTestDB(t, ctx)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			key := []byte(t.Name())
			tx.SetTimeout(60000)
			tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
			caller, interrupt := context.WithCancel(ctx)
			defer interrupt()
			p := pendingStamp(t, caller, tx)
			sibling := pendingStamp(t, ctx, tx)
			pause := installOneShotCommitPause(t, db)
			defer pause.unblock()
			done := make(chan error, 1)
			go func() { done <- tx.Commit(caller) }()
			waitCommitLifetimeGate(t, ctx, pause.reached, "detached commit")
			switch mode {
			case "cancel":
				tx.Cancel()
			case "reset":
				tx.Reset()
			case "timeout":
				tx.readErrMu.Lock()
				inc, gen := tx.readLife, tx.readLife.timerGen
				tx.readErrMu.Unlock()
				tx.fireReadTimeout(inc, gen)
				tx.Cancel() // must not erase the earlier timeout
			case "caller":
				interrupt()
			case "late-poison":
				tx.Atomic(MutationType(1), []byte("invalid"), nil)
			}
			if mode != "late-poison" {
				_, err := p.Resolve()
				if mode == "caller" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("caller cancellation: %v", err)
					}
					requireStampPending(t, sibling)
				} else {
					want := 1025
					if mode == "timeout" {
						want = 1031
					}
					requireCommitLifetimeCode(t, err, want, "retired promise before detached reply")
				}
			}
			pause.unblock()
			awaitCommitLifetimeResult(t, ctx, done, "detached Commit must retain success")
			stored := readCommitLifetimeValue(t, ctx, db, key)
			if len(stored) != 10 {
				t.Fatalf("detached write missing: %x", stored)
			}
			if mode == "caller" || mode == "late-poison" {
				stamp, err := sibling.Resolve()
				if err != nil || !bytes.Equal(stamp, stored) {
					t.Fatalf("healthy admission stamp=%x stored=%x err=%v", stamp, stored, err)
				}
			} else {
				// This handle was NOT resolved before the successful reply. The
				// retirement result must be sealed in the shared record, not just
				// memoized by the earlier consumer's context-cancellation branch.
				_, err := sibling.Resolve()
				want := 1025
				if mode == "timeout" {
					want = 1031
				}
				requireCommitLifetimeCode(t, err, want, "retirement still wins after native success")
			}
		})
	}
}

func TestFDBVersionstampWaitsForUncertaintyBarrier(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.Set([]byte(t.Name()), []byte("value"))
	if _, err := tx.GetReadVersion(ctx); err != nil {
		t.Fatalf("GRV: %v", err)
	}
	p := pendingStamp(t, ctx, tx)
	proxy, _, err := db.db.getCommitProxy()
	if err != nil {
		t.Fatal(err)
	}
	var injected atomic.Bool
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		if injected.CompareAndSwap(false, true) {
			return (&types.ErrorOrError{ErrorCode: 1100}).MarshalFDB(), false
		}
		return body, false
	})
	sd.armAddr(proxy.Address)
	barrier := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
	defer barrier.unblock()
	var calls atomic.Int32
	hook := func() {
		if calls.Add(1) == 2 {
			close(barrier.reached)
			<-barrier.release
		}
	}
	db.db.beforeCommitProxySelect.Store(&hook)
	defer db.db.beforeCommitProxySelect.Store(nil)
	done := make(chan error, 1)
	go func() { done <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, barrier.reached, "dummy synchronization commit")
	if !injected.Load() {
		t.Fatal("barrier reached without canonical 1100 injection")
	}
	requireStampPending(t, p)
	select {
	case err := <-done:
		t.Fatalf("Commit returned before uncertainty barrier: %v", err)
	default:
	}
	barrier.unblock()
	select {
	case err := <-done:
		requireCommitLifetimeCode(t, err, 1021, "normalized Commit result")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err = p.Resolve()
	requireCommitLifetimeCode(t, err, 2020, "native failure after uncertainty barrier")
}

// Overlapping Commit is part of Go's existing concurrent-call surface, not a
// claim that C++ permits concurrent commit producers on the same handle.
func TestFDBVersionstampOverlappingProducers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	key := []byte(t.Name())
	tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
	first := pendingStamp(t, ctx, tx)
	pause := installOneShotCommitPause(t, db)
	defer pause.unblock()
	firstDone := make(chan error, 1)
	go func() { firstDone <- tx.Commit(ctx) }()
	waitCommitLifetimeGate(t, ctx, pause.reached, "first producer's detached snapshot")
	second := tx.PrepareCommit(ctx)
	latest := pendingStamp(t, ctx, tx)
	if latest.completion == first.completion {
		t.Fatal("second producer stole first producer's promise")
	}
	if err := second.Resolve(); err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	stamp, err := latest.Resolve()
	if err != nil || len(stamp) != 10 {
		t.Fatalf("latest producer stamp: %x, %v", stamp, err)
	}
	_, err = first.Resolve()
	requireCommitLifetimeCode(t, err, 1025, "successful turnover retires earlier pending stamp")
	pause.unblock()
	select {
	case err := <-firstDone:
		// Both snapshots share the original SC read/write range. The second
		// commit wins, so the server rejects the delayed first snapshot.
		requireCommitLifetimeCode(t, err, 1020, "first producer's own Commit result")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	retained, err := tx.GetVersionstamp()
	if err != nil || !bytes.Equal(retained, stamp) {
		t.Fatalf("late failure changed latest stamp: %x, %v", retained, err)
	}
	if got := readCommitLifetimeValue(t, ctx, db, key); !bytes.Equal(got, stamp) {
		t.Fatalf("stored stamp: %x != %x", got, stamp)
	}
}

func TestFDBVersionstampSynchronousGetterTurnover(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"reset", "commit"} {
		for _, head := range []string{"nil-head", "replacement-head"} {
			t.Run(mode+"/"+head, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				db := openTestDB(t, ctx)
				tx := db.CreateTransaction()
				key := []byte(t.Name())
				tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
				lease := tx.enterState()
				old := lease.inc
				lease.release()
				pause := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
				defer pause.unblock()
				tx.beforeVersionstampLookup = func() {
					close(pause.reached)
					<-pause.release
				}
				getter := make(chan error, 1)
				go func() {
					_, err := tx.GetVersionstamp()
					getter <- err
				}()
				waitCommitLifetimeGate(t, ctx, pause.reached, "getter before completion lookup")
				turnover := make(chan error, 1)
				go func() {
					if mode == "reset" {
						tx.Reset()
						turnover <- nil
					} else {
						turnover <- tx.Commit(ctx)
					}
				}()
				waitCommitLifetimeGate(t, ctx, old.ctx.Done(), "turnover before getter lease drain")
				requireLifetimeTestBlocked(t, turnover, "turnover with a live getter lease")
				tx.readErrMu.Lock()
				nilHead := tx.readLife == nil
				if head == "replacement-head" {
					tx.readIncarnationLocked()
				}
				tx.readErrMu.Unlock()
				if !nilHead {
					t.Fatal("turnover did not clear the current incarnation")
				}
				pause.unblock()
				// No Cancel cleanup until the getter has released its lease: the
				// broken nil-head path deadlocks in panic unwinding, and must fail
				// this bounded assertion rather than wedging the test's cleanup.
				err := receiveLifetimeTest(t, getter, "getter on retired incarnation")
				requireCommitLifetimeCode(t, err, 1025, "captured retirement, never successor state")
				awaitCommitLifetimeResult(t, ctx, turnover, "turnover after getter lease release")
				defer tx.Cancel()
				tx.beforeVersionstampLookup = nil
				stamp, err := tx.GetVersionstamp()
				if mode == "reset" {
					requireCommitLifetimeCode(t, err, 2015, "new incarnation after Reset")
				} else if stored := readCommitLifetimeValue(t, ctx, db, key); err != nil || len(stored) != 10 || !bytes.Equal(stamp, stored) {
					t.Fatalf("new getter lost selected CommitID: stamp=%x stored=%x err=%v", stamp, stored, err)
				}
			})
		}
	}
}

// Latest-producer retention is Go's concurrent/auto-reuse extension. The native
// size-failure mapping (Commit 2101, stamp 2020) is separately differential-pinned.
func TestFDBVersionstampOlderSuccessRetainsLatestProducer(t *testing.T) {
	t.Parallel()
	for _, gate := range []string{"before-dispatch", "after-selection"} {
		for _, newer := range []string{"native-failure", "pending"} {
			t.Run(gate+"/"+newer, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				db := openTestDB(t, ctx)
				tx := db.CreateTransaction()
				defer tx.Cancel()
				tx.SetSizeLimit(1000)
				key := []byte(t.Name())
				tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
				first := pendingStamp(t, ctx, tx)
				var reached <-chan struct{}
				var unblock func()
				if gate == "before-dispatch" {
					pause := installOneShotCommitPause(t, db)
					reached, unblock = pause.reached, pause.unblock
				} else {
					db.db.metrics.commitLatency.mu.Lock()
					var once sync.Once
					reached = first.completion.done
					unblock = func() { once.Do(db.db.metrics.commitLatency.mu.Unlock) }
				}
				defer unblock()
				firstDone := make(chan error, 1)
				go func() { firstDone <- tx.Commit(ctx) }()
				waitCommitLifetimeGate(t, ctx, reached, "older producer gate")
				tx.Set(append(append([]byte(nil), key...), '/'), make([]byte, 4000))
				second := tx.PrepareCommit(ctx)
				defer second.cleanup()
				latest := pendingStamp(t, ctx, tx)
				if latest.completion == first.completion {
					t.Fatal("later producer reused the older completion")
				}
				want := 1025
				if newer == "native-failure" {
					requireCommitLifetimeCode(t, second.Resolve(), 2101, "newer native size failure")
					_, err := latest.Resolve()
					requireCommitLifetimeCode(t, err, 2020, "newer stamp before older success")
					want = 2020
				} else {
					requireStampPending(t, latest)
				}
				unblock()
				awaitCommitLifetimeResult(t, ctx, firstDone, "older successful Commit")
				stamp, err := first.Resolve()
				stored := readCommitLifetimeValue(t, ctx, db, key)
				if err != nil || len(stored) != 10 || !bytes.Equal(stamp, stored) {
					t.Fatalf("older immutable result: stamp=%x stored=%x err=%v", stamp, stored, err)
				}
				version, err := tx.GetCommittedVersion()
				if err != nil || version <= 0 {
					t.Fatalf("successful Commit metadata: %d, %v", version, err)
				}
				_, err = latest.Resolve()
				requireCommitLifetimeCode(t, err, want, "latest handle after older turnover")
				_, err = tx.GetVersionstamp()
				requireCommitLifetimeCode(t, err, want, "synchronous getter retains latest producer")
				value, pending, err := tx.GetVersionstampPending(ctx)
				if pending != nil {
					pending.cleanup()
					t.Fatal("retained latest completion became pending")
				}
				if value != nil {
					t.Fatalf("latest producer failure became a stamp: %x", value)
				}
				requireCommitLifetimeCode(t, err, want, "future getter retains latest producer")
				if newer == "pending" {
					requireCommitLifetimeCode(t, second.Resolve(), 1025, "retired newer producer cannot execute")
				}
			})
		}
	}
}

func TestFDBVersionstampPanicCannotPublishSuccess(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"before-dispatch", "uncertainty-barrier"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			db, sd := newSimTestDB(t, ctx)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			key := []byte(t.Name())
			tx.Atomic(MutSetVersionstampedValue, key, make([]byte, 14))
			if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatal(err)
			}
			p := pendingStamp(t, ctx, tx)
			var injected atomic.Bool
			panicAt := int32(1)
			if phase == "uncertainty-barrier" {
				proxy, _, err := db.db.getCommitProxy()
				if err != nil {
					t.Fatal(err)
				}
				sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
					if injected.CompareAndSwap(false, true) {
						return (&types.ErrorOrError{ErrorCode: 1100}).MarshalFDB(), false
					}
					return body, false
				})
				sd.armAddr(proxy.Address)
				panicAt = 2
			}
			panicValue := &struct{ phase string }{phase}
			var calls atomic.Int32
			hook := func() {
				if calls.Add(1) == panicAt {
					panic(panicValue)
				}
			}
			db.db.beforeCommitProxySelect.Store(&hook)
			defer db.db.beforeCommitProxySelect.Store(nil)
			var caught any
			func() {
				defer func() { caught = recover() }()
				_ = tx.Commit(ctx)
			}()
			if caught != panicValue || calls.Load() != panicAt {
				t.Fatalf("Commit swallowed/changed panic or missed phase: %v, calls=%d", caught, calls.Load())
			}
			if phase == "uncertainty-barrier" && !injected.Load() {
				t.Fatal("barrier panic without canonical maybe-delivered injection")
			}
			requireStampPending(t, p)
			if phase == "before-dispatch" {
				if stored := readCommitLifetimeValue(t, ctx, db, key); stored != nil {
					t.Fatalf("pre-dispatch panic wrote a value: %x", stored)
				}
			}
			tx.Cancel()
			_, err := p.Resolve()
			requireCommitLifetimeCode(t, err, 1025, "unselected stamp still retires normally after panic")
		})
	}
}
