package client

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"
	"weak"

	"fdb.dev/pkg/fdbgo/wire"
)

type operationValueKey struct{}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestNativeReadStateCaptureAllocations(t *testing.T) {
	tx := &Transaction{}
	defer tx.Cancel()
	tx.SetReadVersion(321)
	var input commitInput
	allocs := testing.AllocsPerRun(100, func() {
		tx.captureNativeReadState(&input)
	})
	if allocs != 1 {
		t.Fatalf("native read-state capture allocations = %v, want 1", allocs)
	}
	if input.readState.readVersionState != tx.readVersionState || input.readState.readVersion != 321 {
		t.Fatal("native read-state capture did not share the existing GRV future")
	}
	if size := reflect.TypeOf(input.readState).Elem().Size(); size > 128 {
		t.Fatalf("native read-state capture size = %d bytes, want <= 128", size)
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestNativeReadStateReadyAllocations(t *testing.T) {
	state := &nativeReadState{readVersionState: &readVersionState{
		hasReadVersion: true, readVersion: 321,
	}}
	allocs := testing.AllocsPerRun(100, func() {
		if rv, err := state.readVersionForCommit(context.Background()); err != nil || rv != 321 {
			t.Fatalf("ready native GRV = %d/%v, want 321/nil", rv, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("ready native GRV allocations = %v, want 0", allocs)
	}
}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestReadOperationContextAllocations(t *testing.T) {
	cancellable, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An uncancellable parent needs no derived context: the operation and its
	// release are all it allocates.
	for _, tc := range []struct {
		name   string
		parent context.Context
		max    float64
	}{
		{"background", context.Background(), 2},
		{"value", context.WithValue(context.Background(), operationValueKey{}, "caller"), 2},
		{"cancellable", cancellable, 8},
	} {
		name, parent := tc.name, tc.parent
		t.Run(name, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			n := testing.AllocsPerRun(100, func() {
				ctx, release, _ := tx.opContextFor(parent, true)
				_ = ctx.Done()
				release()
			})
			if n > tc.max {
				t.Fatalf("READ_OPERATION_ALLOC: %v allocations per operation, want <= %v", n, tc.max)
			}
			t.Logf("READ_OPERATION_ALLOC: %s = %v", name, n)
		})
	}
}

func TestReadOperationReentryUsesFreshLease(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	old, releaseOld := tx.opContext(context.Background())
	defer releaseOld()
	oldOp := tx.readOperation(old)
	oldOp.lease.release()
	ctx, release, counted := tx.opContextFor(old, true)
	defer release()
	op := tx.readOperation(ctx)
	if !counted || op == oldOp || op.lease == oldOp.lease || op.inc != oldOp.inc {
		t.Fatal("reentry did not allocate a distinct lease on the captured incarnation")
	}
	releaseOld()
	tx.readErrMu.Lock()
	users, issued, active := op.inc.users, op.inc.issuedReads, op.lease.active.Load()
	tx.readErrMu.Unlock()
	if users != 1 || issued != 1 || !active {
		t.Fatalf("old cleanup released the new lease: users=%d issued=%d active=%v", users, issued, active)
	}
	release()
	tx.readErrMu.Lock()
	users, issued = op.inc.users, op.inc.issuedReads
	tx.readErrMu.Unlock()
	if users != 0 || issued != 0 {
		t.Fatalf("new cleanup retained its lease: users=%d issued=%d", users, issued)
	}
}

func TestReadOperationContextPreservesParent(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancelCause(context.WithValue(context.Background(), operationValueKey{}, "caller"))
	defer cancel(nil)
	deadline := time.Now().Add(time.Hour)
	parent, stop := context.WithDeadline(parent, deadline)
	defer stop()
	tx, other := &Transaction{}, &Transaction{}
	defer tx.Cancel()
	defer other.Cancel()
	ctx, release := tx.opContext(parent)
	defer release()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatalf("operation deadline=(%v, %v), want %v", got, ok, deadline)
	}
	if ctx.Value(operationValueKey{}) != "caller" || ctx.Value(struct{ missing bool }{}) != nil {
		t.Fatal("operation changed caller values")
	}
	override := context.WithValue(ctx, operationValueKey{}, "nested")
	nested, releaseNested := tx.opContext(override)
	defer releaseNested()
	if nested != override || nested.Value(operationValueKey{}) != "nested" || tx.readOperation(nested) != tx.readOperation(ctx) {
		t.Fatal("nested operation did not borrow the caller's context and lease")
	}
	otherCtx, releaseOther := other.opContext(override)
	defer releaseOther()
	if otherCtx.Value(operationValueKey{}) != "nested" || tx.readOperation(otherCtx) != tx.readOperation(ctx) || other.readOperation(otherCtx) == tx.readOperation(ctx) {
		t.Fatal("second transaction shadowed the first transaction or caller value")
	}
	releaseOther()
	if ctx.Err() != nil || parent.Err() != nil {
		t.Fatal("releasing a child operation canceled its parent")
	}
	nilValue := context.WithValue(ctx, operationValueKey{}, nil)
	otherCtx, releaseNil := other.opContext(nilValue)
	defer releaseNil()
	if otherCtx.Value(operationValueKey{}) != nil {
		t.Fatal("operation ignored a nil-valued parent shadow")
	}
	child, cancelChild := context.WithCancelCause(ctx)
	defer cancelChild(nil)
	childCause := errors.New("child interruption")
	cancelChild(childCause)
	if context.Cause(child) != childCause || ctx.Err() != nil {
		t.Fatal("derived child cancellation did not remain local")
	}
	waiting, stopWaiting := context.WithCancel(ctx)
	defer stopWaiting()
	after := make(chan struct{})
	stopAfter := context.AfterFunc(ctx, func() { close(after) })
	defer stopAfter()
	cause := errors.New("caller interruption")
	cancel(cause)
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) || context.Cause(ctx) != cause {
		t.Fatalf("operation interruption=(%v, %v), want caller cancellation and cause", ctx.Err(), context.Cause(ctx))
	}
	for name, done := range map[string]<-chan struct{}{"child": waiting.Done(), "AfterFunc": after} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s derived from operation did not observe cancellation", name)
		}
	}
	if context.Cause(waiting) != cause {
		t.Fatalf("derived child cause=%v, want caller cause", context.Cause(waiting))
	}
}

// An operation whose parent can never be cancelled takes its cancellation
// from the incarnation directly: Done, Err and Cause follow the incarnation,
// contexts derived from the operation see it end, and caller values survive.
func TestReadOperationUncancellableParentFollowsIncarnation(t *testing.T) {
	t.Parallel()
	for name, parent := range map[string]context.Context{
		"background": context.Background(),
		"value":      context.WithValue(context.Background(), operationValueKey{}, "caller"),
		"without":    context.WithoutCancel(context.WithValue(context.Background(), operationValueKey{}, "caller")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{}
			ctx, release := tx.opContext(parent)
			defer release()
			op := tx.readOperation(ctx)
			if op == nil || ctx.Done() == nil || ctx.Done() != op.inc.ctx.Done() || ctx.Err() != nil {
				t.Fatal("operation under an uncancellable parent derived its own context instead of using its incarnation's")
			}
			if got, want := ctx.Value(operationValueKey{}), parent.Value(operationValueKey{}); got != want {
				t.Fatalf("operation value = %v, want the parent's %v", got, want)
			}
			if _, ok := ctx.Deadline(); ok {
				t.Fatal("operation reported a deadline its parent does not have")
			}
			child, stopChild := context.WithCancel(ctx)
			defer stopChild()
			after := make(chan struct{})
			stopAfter := context.AfterFunc(ctx, func() { close(after) })
			defer stopAfter()
			tx.Cancel()
			for what, done := range map[string]<-chan struct{}{"operation": ctx.Done(), "child": child.Done(), "AfterFunc": after} {
				select {
				case <-done:
				case <-time.After(time.Minute):
					t.Fatalf("%s did not observe the incarnation's end", what)
				}
			}
			var fdbErr *wire.FDBError
			if !errors.Is(ctx.Err(), context.Canceled) || !errors.As(context.Cause(ctx), &fdbErr) || fdbErr.Code != ErrTransactionCancelled {
				t.Fatalf("operation interruption = (%v, %v), want the transaction's cancellation", ctx.Err(), context.Cause(ctx))
			}
			if !errors.As(context.Cause(child), &fdbErr) || fdbErr.Code != ErrTransactionCancelled {
				t.Fatalf("derived child cause = %v, want the transaction's cancellation", context.Cause(child))
			}
		})
	}
}

// Releasing an operation under an uncancellable parent returns its lease and
// cancels nothing: the operation's context and a context derived from it end
// only with the incarnation.
func TestReadOperationUncancellableParentReleasesLease(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	ctx, release, counted := tx.opContextFor(context.Background(), true)
	op := tx.readOperation(ctx)
	if !counted || !op.lease.isActive() {
		t.Fatal("operation was not admitted")
	}
	child, stopChild := context.WithCancel(ctx)
	defer stopChild()
	release()
	tx.readErrMu.Lock()
	users, issued := op.inc.users, op.inc.issuedReads
	tx.readErrMu.Unlock()
	if users != 0 || issued != 0 || op.lease.isActive() {
		t.Fatalf("release kept the lease: users=%d issued=%d", users, issued)
	}
	if ctx.Err() != nil || child.Err() != nil {
		t.Fatalf("release cancelled the operation (%v) or its child (%v)", ctx.Err(), child.Err())
	}
	tx.Cancel()
	for what, done := range map[string]<-chan struct{}{"operation": ctx.Done(), "child": child.Done()} {
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Fatalf("%s did not end with the incarnation", what)
		}
	}
}

// Not parallel: AllocsPerRun panics under t.Parallel and counts process-wide.
func TestStateLeaseDoesNotAllocate(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation changes inlining decisions")
	}
	tx := &Transaction{}
	defer tx.Cancel()
	for name, call := range map[string]func(){
		"enterState": func() { tx.enterState().release() },
		"TenantId":   func() { _ = tx.TenantId() },
	} {
		if n := testing.AllocsPerRun(100, call); n != 0 {
			t.Errorf("LEASE_ALLOC: %s allocated %v times per call, want 0", name, n)
		}
	}
}

// The incarnation keeps one native commit inline and the rest in a map; each
// transition keeps every live commit reachable exactly once.
func TestIncarnationNativeCommitStorage(t *testing.T) {
	t.Parallel()
	inc := &readIncarnation{}
	a, b, c := &nativeCommit{}, &nativeCommit{}, &nativeCommit{}
	live := func() map[*nativeCommit]int {
		got := map[*nativeCommit]int{}
		if inc.nativeCommit != nil {
			got[inc.nativeCommit]++
		}
		for n := range inc.nativeCommits {
			got[n]++
		}
		return got
	}
	check := func(step string, want ...*nativeCommit) {
		t.Helper()
		got := live()
		if len(got) != len(want) {
			t.Fatalf("%s: %d live commits, want %d", step, len(got), len(want))
		}
		for _, n := range want {
			if got[n] != 1 {
				t.Fatalf("%s: a live commit is reachable %d times, want once", step, got[n])
			}
		}
	}
	inc.addNativeLocked(a)
	if inc.nativeCommit != a || inc.nativeCommits != nil {
		t.Fatal("the first native commit did not go inline")
	}
	inc.addNativeLocked(b)
	check("second added", a, b)
	inc.removeNativeLocked(a)
	check("inline removed while overflow remains", b)
	inc.addNativeLocked(c)
	if inc.nativeCommit != c {
		t.Fatal("a freed inline slot was not reused")
	}
	check("inline reused", b, c)
	inc.removeNativeLocked(b)
	check("overflow removed", c)
	inc.removeNativeLocked(a) // already gone: no effect
	check("stale removal", c)
	inc.removeNativeLocked(c)
	check("all removed")
}

// Mutation: timeoutNode holds a strong transaction. A GC clears the weak
// handle before the deadline; cleanup or the next dispatch unlinks its stub.
func TestAbandonedTransactionTimeoutDoesNotRetainIt(t *testing.T) {
	t.Parallel()
	q, clock, _ := manualTimeoutQueue()
	var wake int64
	abandon := func() weak.Pointer[Transaction] {
		tx := timeoutTestTransaction(q)
		tx.SetTimeout(time.Hour.Milliseconds())
		if !tx.timeoutArmed || tx.timeoutNode == nil {
			t.Fatal("SetTimeout did not arm the timeout")
		}
		n := tx.timeoutNode
		n.queue.mu.Lock()
		queued := n.index >= 0 && n.queued.Load()
		wake = n.wake
		n.queue.mu.Unlock()
		if !queued {
			t.Fatal("live timeout was not physically registered")
		}
		return weak.Make(tx)
	}
	w := abandon()
	for i := 0; i < 20 && w.Value() != nil; i++ {
		runtime.GC()
	}
	if w.Value() != nil {
		t.Fatal("TIMEOUT_RETAINS: an armed queue kept an abandoned transaction alive")
	}
	// Cleanup can remove it first; any remaining old stub must drop on dispatch.
	clock.Store(wake)
	q.fire()
	if timeoutHeapLen(q) != 0 {
		t.Fatal("cleared weak node remained queued after its next wake")
	}
	requireTimeoutHeap(t, q)
}

// The timebomb of a live transaction still fires, after re-arming for a new
// deadline, with transaction_timed_out.
func TestLiveTransactionTimebombFires(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.SetTimeout(time.Hour.Milliseconds())
	tx.SetTimeout(1) // re-arm the same timer for a 1 ms deadline
	tx.readErrMu.Lock()
	inc := tx.readLife
	tx.readErrMu.Unlock()
	select {
	case <-inc.ctx.Done():
	case <-time.After(time.Minute):
		t.Fatal("the re-armed timebomb never fired")
	}
	if code, ok := fdbErrorCode(context.Cause(inc.ctx)); !ok || code != ErrTransactionTimedOut {
		t.Fatalf("timebomb cause = %v, want transaction_timed_out", context.Cause(inc.ctx))
	}
	runtime.KeepAlive(tx)
}

func armedTimeout(t *testing.T, tx *Transaction) (uint64, *readIncarnation) {
	t.Helper()
	tx.readErrMu.Lock()
	defer tx.readErrMu.Unlock()
	if !tx.timeoutArmed || tx.timeoutNode == nil || tx.readLife == nil {
		t.Fatal("no timeout armed for the current generation")
	}
	return tx.timeoutArm, tx.readLife
}

// A callback of a timebomb that was replaced, or disarmed, fires nothing; the
// replacement's callback fires its incarnation. Callbacks are invoked directly,
// so no timer races the assertions.
func TestTimeoutCallbackIdentity(t *testing.T) {
	t.Parallel()
	tx, _, _ := armedTimeoutTransaction(t)
	old, inc := armedTimeout(t, tx)
	node := tx.timeoutNode
	tx.SetTimeout(2 * time.Hour.Milliseconds())
	replacement, armedFor := armedTimeout(t, tx)
	if replacement == old || armedFor != inc || tx.timeoutNode != node {
		t.Fatal("re-arm did not reuse the node with a fresh arm")
	}
	tx.fireTimeout(old)
	if inc.ctx.Err() != nil {
		t.Fatal("the replaced timeout's callback fired the incarnation")
	}
	tx.SetTimeout(0)
	tx.fireTimeout(replacement)
	if inc.ctx.Err() != nil {
		t.Fatal("a disarmed callback fired the incarnation")
	}
	tx.SetTimeout(3 * time.Hour.Milliseconds())
	reused, _ := armedTimeout(t, tx)
	if tx.timeoutNode != node {
		t.Fatal("re-arm allocated a new node")
	}
	tx.fireTimeout(reused)
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
}

// A callback admitted before a re-arm carries the old generation, which the
// incarnation no longer has, so it fires nothing.
func TestTimeoutAdmittedCallbackLosesToRearm(t *testing.T) {
	t.Parallel()
	tx, _, fire := armedTimeoutTransaction(t)
	staleArm, inc := armedTimeout(t, tx)
	tx.SetTimeout(2 * time.Hour.Milliseconds())
	tx.fireTimeout(staleArm)
	if inc.ctx.Err() != nil {
		t.Fatal("an admitted callback from before re-arm fired the incarnation")
	}
	fire()
	requireTimeoutCode(t, context.Cause(inc.ctx), ErrTransactionTimedOut)
}
