package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"unsafe"

	"fdb.dev/pkg/fdbgo/wire"
)

// The held mutex is the structural guard, not a benchmark threshold. A failed
// observation is unblocked and joined before the test reports the dependency.
func requireLeaseObservationWithoutAccountingLock(t *testing.T, tx *Transaction, observe func() bool) {
	t.Helper()
	tx.readErrMu.Lock()
	started := make(chan struct{})
	result := make(chan bool, 1)
	go func() {
		close(started)
		result <- observe()
	}()
	<-started
	select {
	case ok := <-result:
		tx.readErrMu.Unlock()
		if !ok {
			t.Fatal("lease observation returned the wrong value or authority")
		}
	case <-time.After(5 * time.Second):
		tx.readErrMu.Unlock()
		<-result
		t.Fatal("LEASE_ACTIVE_LOCK_DEPENDENCY: observation waited for the accounting mutex")
	}
}

func TestLeaseActiveObservationDoesNotAcquireAccountingLock(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nil", "active", "released", "borrowed-operation"} {
		t.Run(mode, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			ctx, cleanup, owned := tx.opContextWithLease(context.Background(), false)
			defer cleanup()
			if mode == "released" {
				owned.release()
			}
			requireLeaseObservationWithoutAccountingLock(t, tx, func() bool {
				switch mode {
				case "nil":
					return !(*executionLease)(nil).isActive()
				case "active":
					return owned.isActive()
				case "released":
					return !owned.isActive()
				default:
					borrowed, releaseBorrowed, authority := tx.opContextWithLease(ctx, true)
					releaseBorrowed()
					return borrowed == ctx && authority == nil && tx.readOperation(borrowed).lease == owned
				}
			})
		})
	}
}

func TestLeaseDrainNotificationPublishesInactiveSingleWinner(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, cleanup, owned := tx.opContextWithLease(context.Background(), true)
	defer cleanup()
	inc := tx.readOperation(ctx).inc
	// Register the same zero-users and zero-issued notifications used by
	// turnover/Commit. The actual release path must publish and close them.
	tx.readErrMu.Lock()
	drained, issuedDone := make(chan struct{}), make(chan struct{})
	inc.drained, inc.issuedReadsDone = drained, issuedDone
	tx.readErrMu.Unlock()
	type observation struct {
		active        bool
		users, issued int
		cleared       bool
	}
	observed := make(chan observation, 1)
	watching := make(chan struct{})
	go func() {
		close(watching)
		<-drained
		// Do not acquire readErrMu before the active sample: that would hide
		// an incorrectly ordered notification preceding the atomic store.
		active := owned.isActive()
		tx.readErrMu.Lock()
		got := observation{active, inc.users, inc.issuedReads, inc.drained == nil && inc.issuedReadsDone == nil}
		tx.readErrMu.Unlock()
		observed <- got
	}()
	<-watching
	var releases sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		releases.Add(1)
		go func() {
			defer releases.Done()
			<-start
			owned.release()
		}()
	}
	close(start)
	releases.Wait()
	got := <-observed
	if got.active || got.users != 0 || got.issued != 0 || !got.cleared {
		t.Fatalf("drained publication or duplicate release: %+v", got)
	}
	// Inactive alone does not promise this channel is closed; joined release
	// completion does. Its notification remains outside the bookkeeping lock.
	select {
	case <-issuedDone:
	default:
		t.Fatal("completed release did not notify issued-read completion")
	}
}

func TestExecutionLeaseAndOperationLayout(t *testing.T) {
	pointer := unsafe.Sizeof((*Transaction)(nil))
	wantLease := 2*pointer + 8
	wantOperation := 8*pointer + wantLease
	if got := unsafe.Sizeof(executionLease{}); got != wantLease {
		t.Fatalf("executionLease size=%d, want %d (24 on 64-bit)", got, wantLease)
	}
	if got := unsafe.Sizeof(readOperation{}); got != wantOperation {
		t.Fatalf("readOperation size=%d, want %d (88 on 64-bit)", got, wantOperation)
	}
}

func TestAdmissionOwnedBorrowedFreshAndEffectiveLegacy(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, cleanup, owned := tx.opContextWithLease(context.Background(), false)
	defer cleanup()
	op := tx.readOperation(ctx)
	if owned == nil || op.lease != owned {
		t.Fatal("root did not receive its authoritative token")
	}
	legacy, stop := context.WithCancel(ctx)
	defer stop()
	borrowed, releaseBorrowed, borrower := tx.opContextWithLease(legacy, true)
	if borrowed != legacy || borrower != nil || tx.readOperation(borrowed) != op {
		t.Fatal("nested call changed effective legacy context or acquired release authority")
	}
	releaseBorrowed()
	if !owned.isActive() {
		t.Fatal("borrower's cleanup released the owner")
	}
	owned.release()
	fresh, releaseFresh, next := tx.opContextWithLease(legacy, true)
	defer releaseFresh()
	freshOp := tx.readOperation(fresh)
	if next == nil || next == owned || freshOp == op || freshOp.readAdmission != op.readAdmission {
		t.Fatal("reacquisition changed captured admission or reused released storage")
	}
	stop()
	if fresh.Err() != context.Canceled || ctx.Err() != nil || tx.readIncarnationCause(op.inc) != nil || op.parent.Err() != nil {
		t.Fatal("effective legacy signal was flattened into original caller/incarnation")
	}
	cleanup()
	cleanup() // old whole-operation cleanup must never release a later token
	if !next.isActive() || owned.isActive() {
		t.Fatal("old cleanup affected the fresh token")
	}
	releaseFresh()
	releaseFresh()
	tx.readErrMu.Lock()
	users, issued := op.inc.users, op.inc.issuedReads
	tx.readErrMu.Unlock()
	if users != 0 || issued != 0 {
		t.Errorf("double cleanup corrupted accounting: users=%d issued=%d", users, issued)
	}
	tx.Reset()
	_, releaseStale, stale := tx.opContextWithLease(ctx, false)
	defer releaseStale()
	if stale != nil {
		t.Fatal("retired admission acquired successor authority")
	}
}

func TestBorrowedRangeOwnsOnlyItsIssuedRegistration(t *testing.T) {
	t.Parallel()
	tx := &Transaction{db: &database{}}
	defer tx.Cancel()
	ctx, cleanup, owned := tx.opContextWithLease(context.Background(), false)
	defer cleanup()
	op := tx.readOperation(ctx)
	r := tx.issueRange(ctx, []byte("a"), []byte("z"), 0, 0, false, false)
	if r.finished || r.local || r.inc != op.inc {
		t.Fatalf("nonlocal borrowed issue not counted independently: finished=%v local=%v inc=%v", r.finished, r.local, r.inc == op.inc)
	}
	tx.readErrMu.Lock()
	users, issued := op.inc.users, op.inc.issuedReads
	tx.readErrMu.Unlock()
	if users != 1 || issued != 1 {
		t.Fatalf("borrowed issue counts=%d/%d, want1/1", users, issued)
	}
	// Complete through the actual canceled-read path, whose error publication
	// must precede release of its independent Commit barrier registration.
	tx.Cancel()
	_, _, err := r.run(true)
	requireScopeCode(t, err, 1025)
	tx.readErrMu.Lock()
	users, issued = op.inc.users, op.inc.issuedReads
	tx.readErrMu.Unlock()
	if users != 1 || issued != 0 || !owned.isActive() {
		t.Errorf("borrowed completion released wrong ownership: users=%d issued=%d active=%v", users, issued, owned.isActive())
	}
}

func TestAdmissionFreshTokenPreservesEntryAndCompletionOrder(t *testing.T) {
	t.Parallel()
	caller, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tx := &Transaction{}
	defer tx.Cancel()
	entry := &wire.FDBError{Code: 2006}
	tx.deferredErr.Store(entry)
	ctx, cleanup, owned := tx.opContextWithLease(caller, false)
	defer cleanup()
	owned.release()
	next, releaseNext, fresh := tx.opContextWithLease(ctx, false)
	defer releaseNext()
	if fresh == nil || fresh == owned {
		t.Fatal("no fresh resolver authority")
	}
	tx.Cancel()
	cancel(errors.New("custom caller cause is not Err"))
	if err := tx.readEntryError(next); err != entry {
		t.Errorf("entry=%v, want captured deferred verdict", err)
	}
	if err := tx.mapReadError(next, context.Canceled); err != context.Canceled {
		t.Errorf("completion=%v, want caller Err, not custom Cause or 1025", err)
	}
	protocol := &wire.FDBError{Code: 2004}
	if tx.mapReadError(next, protocol) != protocol || tx.mapReadError(next, nil) != nil {
		t.Fatal("interruption rewrote a completed protocol/success outcome")
	}
}

func TestNoLeaseRefusalIsNotBorrowedMisuse(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"onerror-fdb", "onerror-app", "commit", "prepared"} {
		t.Run(mode, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			finish := tx.startTurnover()
			defer finish.finish()
			caller, cancel := context.WithCancel(context.Background())
			cancel()
			app := errors.New("application verdict")
			var err error
			switch mode {
			case "onerror-fdb":
				err = tx.OnError(caller, &wire.FDBError{Code: 1020})
			case "onerror-app":
				err = tx.OnError(caller, app)
			case "commit":
				err = tx.Commit(caller)
			case "prepared":
				err = tx.PrepareCommit(caller).Resolve()
			}
			want := error(context.Canceled)
			if mode == "onerror-app" {
				want = app
			}
			if err != want {
				t.Errorf("refused admission=%v, want existing %v (not misuse2000)", err, want)
			}
			tx.readErrMu.Lock()
			inc := tx.readLife
			users, issued := inc.users, inc.issuedReads
			tx.readErrMu.Unlock()
			if users != 0 || issued != 0 {
				t.Errorf("refused admission acquired authority: users=%d issued=%d", users, issued)
			}
		})
	}
}

func TestPreparedResolveCannotStealBorrowedOwner(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, cleanup, owned := tx.opContextWithLease(context.Background(), false)
	defer cleanup()
	pending := tx.PrepareCommit(ctx)
	requireScopeCode(t, pending.Resolve(), 2000)
	if !owned.isActive() || pending.completion.ready || tx.readIncarnationCause(owned.inc) != nil {
		t.Fatal("resolver without ownership changed its borrowed owner/producer")
	}
}

func TestRawOwnedNoWriteCommitKeepsExistingCallerBehavior(t *testing.T) {
	t.Parallel()
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &Transaction{}
	defer tx.Cancel()
	if err := tx.Commit(caller); err != nil {
		t.Errorf("raw owned no-write Commit=%v, want existing success", err)
	}
}

func TestOwnedOnErrorPublishesActualTimeout(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	initial := tx.enterState()
	inc := initial.inc
	initial.release()
	// No ambient timer: this must be the owned OnError's synchronous check.
	tx.timeoutNs.Store(int64(time.Second))
	tx.deadlineNs.Store(time.Now().Add(-time.Hour).UnixNano())
	err := tx.OnError(context.Background(), &wire.FDBError{Code: 1020})
	requireScopeCode(t, err, 1031)
	requireScopeCode(t, tx.readIncarnationCause(inc), 1031)
}

func TestCommitOwnedAndPreparedTokenHandoff(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"owned", "prepared", "prepared-reset"} {
		t.Run(mode, func(t *testing.T) {
			tx := &Transaction{}
			defer tx.Cancel()
			ctx, cleanup, owned := tx.opContextWithLease(context.Background(), false)
			defer cleanup()
			op := tx.readOperation(ctx)
			var err error
			if mode == "owned" {
				completion, entryErr := tx.admitCommit(ctx)
				if entryErr != nil {
					t.Fatal(entryErr)
				}
				err = tx.commitAdmitted(ctx, completion, owned)
			} else {
				owned.release()
				pending := tx.PrepareCommit(ctx)
				prepared := tx.readOperation(pending.ctx).lease
				if prepared == owned || prepared.isActive() {
					t.Fatal("preparation did not acquire and release a distinct token")
				}
				if mode == "prepared-reset" {
					tx.Reset()
				}
				err = pending.Resolve()
				if repeated := pending.Resolve(); !errors.Is(repeated, err) {
					t.Errorf("pending result changed: %v / %v", err, repeated)
				}
			}
			if mode == "prepared-reset" {
				requireScopeCode(t, err, 1025)
			} else if err != nil {
				t.Errorf("commit handoff: %v", err)
			}
			cleanup()
			owned.release()
			tx.readErrMu.Lock()
			users, issued, generation := op.inc.users, op.inc.issuedReads, tx.readGen
			tx.readErrMu.Unlock()
			if users != 0 || issued != 0 || generation != op.inc.gen+1 {
				t.Errorf("handoff cleanup accounting/turnover=%d/%d/%d", users, issued, generation)
			}
		})
	}
}
