package client

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// Done observes registration and takes the leaf lock: reaching called proves
// both registration-before-select and that context methods run outside the lock.
type readinessTestContext struct {
	context.Context
	tx       *Transaction
	called   chan chan struct{}
	resume   <-chan struct{}
	errCalls atomic.Int32
}

func (ctx *readinessTestContext) Done() <-chan struct{} {
	ctx.tx.readErrMu.Lock()
	ready := ctx.tx.readReady
	ctx.tx.readErrMu.Unlock()
	ctx.called <- ready
	if ctx.resume != nil {
		<-ctx.resume
	}
	return ctx.Context.Done()
}

func (ctx *readinessTestContext) Err() error {
	ctx.errCalls.Add(1)
	return ctx.Context.Err()
}

func receiveReadinessTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for readiness test barrier")
		var zero T
		return zero
	}
}

func requireReadinessTestOpen(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	if ch == nil {
		t.Fatal("missing readiness channel")
	}
	select {
	case <-ch:
		t.Fatal("readiness channel closed before publication")
	default:
	}
}

func startReadinessTestTurnover(t *testing.T, tx *Transaction) func() {
	t.Helper()
	token := tx.startTurnover()
	published := false
	finish := func() {
		if !published {
			token.finish()
			published = true
		}
	}
	t.Cleanup(finish)
	return finish
}

// A registered waiter must be drained even if an assertion fails before finish.
type readinessTestWaiter struct {
	result   chan *executionLease
	received bool
}

func startReadinessTestWaiter(t *testing.T, finish func(), admit func() *executionLease) *readinessTestWaiter {
	t.Helper()
	w := &readinessTestWaiter{result: make(chan *executionLease, 1)}
	go func() { w.result <- admit() }()
	t.Cleanup(func() {
		finish()
		if !w.received {
			w.receive(t).release()
		}
	})
	return w
}

func (w *readinessTestWaiter) receive(t *testing.T) *executionLease {
	t.Helper()
	lease := receiveReadinessTest(t, w.result)
	w.received = true
	return lease
}

func (w *readinessTestWaiter) requireBlocked(t *testing.T) {
	t.Helper()
	select {
	case lease := <-w.result:
		w.received = true
		lease.release()
		t.Fatal("registered waiter admitted before publication")
	default:
	}
}

func TestTurnoverReadinessAdmissionOrder(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		for _, barrier := range []string{"nil", "unobserved", "observed"} {
			for _, wait := range []bool{false, true} {
				for _, canceled := range []bool{false, true} {
					for _, issued := range []bool{false, true} {
						name := fmt.Sprintf("stale=%v/%s/wait=%v/canceled=%v/issued=%v", stale, barrier, wait, canceled, issued)
						t.Run(name, func(t *testing.T) {
							t.Parallel()
							tx := &Transaction{}
							t.Cleanup(tx.Cancel)
							old := tx.enterState()
							old.release()
							finish := startReadinessTestTurnover(t, tx)
							tx.readErrMu.Lock()
							current := tx.readIncarnationLocked()
							tx.readErrMu.Unlock()
							if tx.readReady != unobservedReadReady {
								t.Fatal("capturing the successor registered a waiter")
							}
							switch barrier {
							case "nil":
								finish()
							case "observed":
								ctx, cancel := context.WithCancel(context.Background())
								cancel()
								if lease := tx.enterReadState(ctx, current, true); lease != nil {
									lease.release()
									t.Fatal("canceled observer acquired a lease")
								}
								if tx.readReady == nil || tx.readReady == unobservedReadReady {
									t.Fatal("canceled observer did not register")
								}
							}
							before := tx.readReady
							inc := current
							if stale {
								inc = old.inc
							}
							parent, cancel := context.WithCancel(context.Background())
							defer cancel()
							if canceled {
								cancel()
							}
							ctx := &readinessTestContext{Context: parent, tx: tx, called: make(chan chan struct{}, 1)}
							var storage executionLease
							admit := func() *executionLease {
								return tx.enterReadStateInto(ctx, inc, wait, issued, &storage)
							}
							waiting := !stale && barrier != "nil" && wait
							var lease *executionLease
							if waiting {
								w := startReadinessTestWaiter(t, finish, admit)
								registered := receiveReadinessTest(t, ctx.called)
								if registered == nil || registered == unobservedReadReady || (barrier == "observed" && registered != before) {
									t.Fatal("waiter did not register or reuse its private channel before Done")
								}
								if !canceled {
									w.requireBlocked(t)
									finish()
								}
								lease = w.receive(t)
							} else {
								lease = admit()
								if tx.readReady != before {
									t.Fatal("nonwaiting admission changed readiness")
								}
							}
							defer lease.release()
							select {
							case <-ctx.called:
								t.Fatal("admission unexpectedly consulted Done")
							default:
							}
							if ctx.errCalls.Load() != 0 {
								t.Fatal("admission unexpectedly consulted Err")
							}
							accepted := !stale && (barrier == "nil" || (wait && !canceled))
							wantUsers, wantIssued := 0, 0
							if accepted {
								wantUsers = 1
								if issued {
									wantIssued = 1
								}
								if lease != &storage || !lease.active.Load() || lease.inc != current || lease.issued != issued {
									t.Fatal("admission did not claim the captured owner's inline lease")
								}
							} else if lease != nil {
								t.Fatal("refused admission returned a lease")
							}
							if current.users != wantUsers || current.issuedReads != wantIssued || old.inc.users != 0 || old.inc.issuedReads != 0 {
								t.Fatalf("admission counters: current=(%d,%d), old=(%d,%d), want=(%d,%d) and (0,0)", current.users, current.issuedReads, old.inc.users, old.inc.issuedReads, wantUsers, wantIssued)
							}
						})
					}
				}
			}
		}
	}
}

func TestTurnoverReadinessContextFreePublication(t *testing.T) {
	t.Parallel()
	for _, observerFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("observer-first=%v", observerFirst), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			t.Cleanup(tx.Cancel)
			tx.SetReadVersion(123)
			old := tx.readLife
			finish := startReadinessTestTurnover(t, tx)
			marker := tx.readReady
			if marker != unobservedReadReady {
				t.Fatal("turnover allocated an unobserved channel")
			}
			var lease *executionLease
			if observerFirst {
				w := startReadinessTestWaiter(t, finish, tx.enterState)
				// Context-free admission has no context callback. The locked channel
				// replacement is its registration barrier, not goroutine scheduling.
				deadline := time.After(time.Second)
				var ready chan struct{}
				for ready == nil || ready == marker {
					tx.readErrMu.Lock()
					ready = tx.readReady
					tx.readErrMu.Unlock()
					select {
					case <-deadline:
						t.Fatal("context-free waiter did not register")
					default:
						runtime.Gosched()
					}
				}
				w.requireBlocked(t)
				requireReadinessTestOpen(t, ready)
				tx.postCommitResetFields()
				finish()
				receiveReadinessTest(t, ready)
				lease = w.receive(t)
			} else {
				tx.postCommitResetFields()
				finish()
				lease = tx.enterState()
			}
			defer lease.release()
			requireReadinessTestOpen(t, marker)
			if tx.readReady != nil || lease.inc != tx.readLife || lease.inc == old || lease.inc.gen != old.gen+1 || !lease.active.Load() || tx.hasReadVersion {
				t.Fatal("context-free admission missed the fully reset replacement")
			}
		})
	}
}

func TestTurnoverReadinessCancelAfterRegistration(t *testing.T) {
	t.Parallel()
	for _, issued := range []bool{false, true} {
		t.Run(fmt.Sprintf("issued=%v", issued), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			t.Cleanup(tx.Cancel)
			finish := startReadinessTestTurnover(t, tx)
			tx.readErrMu.Lock()
			inc := tx.readIncarnationLocked()
			tx.readErrMu.Unlock()
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &readinessTestContext{Context: parent, tx: tx, called: make(chan chan struct{}, 1)}
			w := startReadinessTestWaiter(t, finish, func() *executionLease { return tx.enterReadStateFor(ctx, inc, true, issued) })
			ready := receiveReadinessTest(t, ctx.called)
			if ready == nil || ready == unobservedReadReady {
				t.Fatal("waiter did not register before cancellation")
			}
			w.requireBlocked(t)
			cancel()
			if lease := w.receive(t); lease != nil {
				lease.release()
				t.Fatal("canceled waiter acquired a lease")
			}
			if inc.users != 0 || inc.issuedReads != 0 || tx.readReady != ready {
				t.Fatal("canceled waiter changed admission counters or readiness")
			}
			requireReadinessTestOpen(t, ready)
			finish()
			receiveReadinessTest(t, ready)
		})
	}
}

func TestTurnoverReadinessRevalidatesAfterWake(t *testing.T) {
	t.Parallel()
	for _, issued := range []bool{false, true} {
		t.Run(fmt.Sprintf("issued=%v", issued), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			t.Cleanup(tx.Cancel)
			finish := startReadinessTestTurnover(t, tx)
			tx.readErrMu.Lock()
			inc := tx.readIncarnationLocked()
			tx.readErrMu.Unlock()
			resume := make(chan struct{})
			ctx := &readinessTestContext{Context: context.Background(), tx: tx, called: make(chan chan struct{}, 1), resume: resume}
			w := startReadinessTestWaiter(t, finish, func() *executionLease { return tx.enterReadStateFor(ctx, inc, true, issued) })
			resumed := false
			defer func() {
				if !resumed {
					close(resume)
				}
			}()
			ready := receiveReadinessTest(t, ctx.called)
			if ready == nil || ready == unobservedReadReady {
				t.Fatal("waiter did not register before its select barrier")
			}
			finish()
			receiveReadinessTest(t, ready)
			// Hold the first waiter's select until its captured successor has
			// itself retired; waking must not admit it into another incarnation.
			finishNext := startReadinessTestTurnover(t, tx)
			finishNext()
			requireLazySuccessorCode(t, context.Cause(inc.ctx), 1025)
			close(resume)
			resumed = true
			if lease := w.receive(t); lease != nil {
				lease.release()
				t.Fatal("woken stale waiter admitted into another incarnation")
			}
			if tx.readLife != nil || tx.readReady != nil || inc.users != 0 || inc.issuedReads != 0 {
				t.Fatal("woken stale waiter materialized or leased the successor")
			}
		})
	}
}

func TestTurnoverReadinessWaitersShareOnlyTheirTurnover(t *testing.T) {
	t.Parallel()
	txs := [2]*Transaction{{}, {}}
	var finishes [2]func()
	var channels [2]chan struct{}
	var waiters [2][3]*readinessTestWaiter
	for i, tx := range txs {
		t.Cleanup(tx.Cancel)
		finishes[i] = startReadinessTestTurnover(t, tx)
		tx.readErrMu.Lock()
		inc := tx.readIncarnationLocked()
		tx.readErrMu.Unlock()
		for j := range waiters[i] {
			ctx := &readinessTestContext{Context: context.Background(), tx: tx, called: make(chan chan struct{}, 1)}
			waiters[i][j] = startReadinessTestWaiter(t, finishes[i], func() *executionLease { return tx.enterReadState(ctx, inc, true) })
			ready := receiveReadinessTest(t, ctx.called)
			if ready == nil || ready == unobservedReadReady || (j != 0 && ready != channels[i]) {
				t.Fatal("waiters did not share a private readiness channel")
			}
			channels[i] = ready
			waiters[i][j].requireBlocked(t)
		}
	}
	if channels[0] == channels[1] {
		t.Fatal("separate transactions shared a readiness channel")
	}
	finishes[0]()
	for _, w := range waiters[0] {
		lease := w.receive(t)
		if lease == nil || lease.inc != txs[0].readLife {
			t.Fatal("published waiter lost its owner")
		}
		lease.release()
	}
	for _, w := range waiters[1] {
		w.requireBlocked(t)
	}
	requireReadinessTestOpen(t, channels[1])
	finishes[1]()
	for _, w := range waiters[1] {
		lease := w.receive(t)
		if lease == nil || lease.inc != txs[1].readLife {
			t.Fatal("second transaction's waiter lost its owner")
		}
		lease.release()
	}
	finish := startReadinessTestTurnover(t, txs[0])
	txs[0].readErrMu.Lock()
	inc := txs[0].readIncarnationLocked()
	txs[0].readErrMu.Unlock()
	ctx := &readinessTestContext{Context: context.Background(), tx: txs[0], called: make(chan chan struct{}, 1)}
	w := startReadinessTestWaiter(t, finish, func() *executionLease { return txs[0].enterReadState(ctx, inc, true) })
	ready := receiveReadinessTest(t, ctx.called)
	if ready == nil || ready == unobservedReadReady || ready == channels[0] || ready == channels[1] {
		t.Fatal("successive turnover reused a publication channel")
	}
	w.requireBlocked(t)
	finish()
	lease := w.receive(t)
	defer lease.release()
	if lease == nil || lease.inc != inc {
		t.Fatal("successive turnover did not admit its own waiter")
	}
}
