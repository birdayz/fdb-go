package client

import (
	"context"
	"testing"
	"time"
)

func requireLazySuccessorCode(t *testing.T, err error, want int) {
	t.Helper()
	if code, ok := fdbErrorCode(err); !ok || code != want {
		t.Fatalf("error=%v, want FDB code %d", err, want)
	}
}

func TestReadOnlyCommitLeavesUnusedSuccessorLazy(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	tx.SetReadVersion(123)
	old, oldState := tx.readLife, tx.readVersionState
	oldState.metricStart = time.Unix(123, 0)
	oldState.readVersionInstant = time.Unix(124, 0)
	var native commitInput
	tx.captureNativeReadState(&native)
	_, stamp, err := tx.GetVersionstampPending(context.Background())
	if err != nil || stamp == nil {
		t.Fatalf("precommit stamp=%v, err=%v", stamp, err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tx.readLife != nil || tx.readVersionState != nil || tx.readReady != nil {
		t.Fatal("read-only Commit eagerly constructed an unused successor")
	}
	if tx.readGen != old.gen+1 || !tx.hasCommitted || tx.committedVersion != -1 || tx.lastVersionstamp != stamp.completion {
		t.Fatal("Commit lost its generation or committed identity")
	}
	requireLazySuccessorCode(t, context.Cause(old.ctx), 1025)
	if value, err := stamp.Resolve(); value != nil {
		t.Fatalf("no-write stamp returned %x", value)
	} else {
		requireLazySuccessorCode(t, err, 2021)
	}
	if tx.readLife != nil || tx.readVersionState != nil {
		t.Fatal("resolving the retained stamp admitted a successor")
	}
	if native.readState.readVersionState != oldState || !oldState.hasReadVersion || !oldState.userSetReadVersion || oldState.readVersion != 123 || !oldState.metricStart.Equal(time.Unix(123, 0)) || !oldState.readVersionInstant.Equal(time.Unix(124, 0)) {
		t.Fatal("Commit changed the retained native GRV state")
	}
	if instant, ok := tx.ReadVersionInstant(); ok || !instant.IsZero() {
		t.Fatalf("first getter retained the old read-version instant: %v, %v", instant, ok)
	}
	if tx.readLife == nil || tx.readLife == old || tx.readLife.gen != old.gen+1 || tx.readVersionState == nil || tx.readVersionState == oldState || tx.hasReadVersion || tx.userSetReadVersion || tx.pendingReadVersion != nil || !tx.metricStart.IsZero() {
		t.Fatal("first context-free getter did not admit clean replacement state")
	}
	tx.SetReadVersion(456)
	if rv, err := tx.GetReadVersion(context.Background()); err != nil || rv != 456 {
		t.Fatalf("reused GRV=%d, err=%v", rv, err)
	}
	if version, err := tx.GetCommittedVersion(); err != nil || version != -1 || tx.lastVersionstamp != stamp.completion {
		t.Fatalf("reuse changed committed identity: %d, %v", version, err)
	}
}

func TestLazySuccessorFirstSetterAndOldContext(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	defer tx.Cancel()
	ctx, cleanup := tx.opContext(context.Background())
	defer cleanup()
	old := tx.readOperation(ctx)
	old.lease.release()
	tx.Reset()
	if tx.readLife != nil || tx.readVersionState != nil {
		t.Fatal("Reset eagerly constructed an unused successor")
	}
	tx.SetReadVersion(789)
	if tx.readLife == old.inc || tx.readLife.gen != old.inc.gen+1 {
		t.Fatal("first setter did not admit the new incarnation")
	}
	if rv, err := tx.GetReadVersion(context.Background()); err != nil || rv != 789 {
		t.Fatalf("first setter GRV=%d, err=%v", rv, err)
	}
	stale, release := tx.opContext(ctx)
	defer release()
	op := tx.readOperation(stale)
	tx.readErrMu.Lock()
	active := op.lease != nil && op.lease.active
	users := tx.readLife.users
	tx.readErrMu.Unlock()
	if op.inc != old.inc || active || users != 0 {
		t.Fatal("old context acquired a replacement lease")
	}
	requireLazySuccessorCode(t, tx.readEntryError(stale), 1025)
}

func TestLazySuccessorPublication(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"commit", "user-reset", "retry"} {
		for _, capture := range []string{"none", "before-fields", "after-fields"} {
			for _, timeout := range []string{"zero", "future", "expired"} {
				t.Run(mode+"/"+capture+"/"+timeout, func(t *testing.T) {
					t.Parallel()
					tx := &Transaction{}
					defer tx.Cancel()
					tx.SetReadVersion(321)
					old, oldState := tx.readLife, tx.readVersionState
					anchor, instant := time.Unix(100, 0), time.Unix(101, 0)
					oldState.metricStart, oldState.readVersionInstant = anchor, instant
					pending := &pendingReadVersion{done: make(chan struct{})}
					oldState.pendingReadVersion = pending
					var native commitInput
					tx.captureNativeReadState(&native)
					finish := tx.startTurnover()
					published := false
					defer func() {
						if !published {
							finish.finish()
						}
					}()
					ready := tx.readReady
					var captured *readIncarnation
					captureEarly := func() {
						parent, cancel := context.WithCancel(context.Background())
						cancel()
						ctx, cleanup := tx.opContext(parent)
						defer cleanup()
						op := tx.readOperation(ctx)
						if op == nil || op.lease != nil || op.inc == old {
							t.Fatal("canceled turnover waiter did not capture a leaseless replacement")
						}
						captured = op.inc
						if captured.timer != nil {
							t.Fatal("capturing the replacement armed a timer before publication")
						}
					}
					if capture == "before-fields" {
						captureEarly()
					}
					switch mode {
					case "commit":
						tx.postCommitResetFields()
					case "user-reset":
						tx.resetFields(true)
					case "retry":
						tx.resetFields(false)
					}
					if capture == "after-fields" {
						captureEarly()
					}
					if timeout != "zero" {
						tx.timeoutNs.Store(int64(time.Hour))
						deadline := time.Now().Add(time.Hour)
						if timeout == "expired" {
							deadline = time.Now().Add(-time.Hour)
						}
						tx.deadlineNs.Store(deadline.UnixNano())
					}
					select {
					case <-ready:
						t.Fatal("replacement was published before finish")
					default:
					}
					finish.finish()
					published = true
					select {
					case <-ready:
					default:
						t.Fatal("finish did not publish the replacement")
					}
					if tx.readReady != nil || tx.readGen != old.gen+1 {
						t.Fatal("finish left a readiness barrier or lost the generation")
					}
					inc := tx.readLife
					if capture == "none" && timeout == "zero" {
						if inc != nil || (mode != "retry" && tx.readVersionState != nil) {
							t.Fatal("finish eagerly materialized an unused zero-timeout successor")
						}
					} else if inc == nil || inc == old || (captured != nil && inc != captured) {
						t.Fatal("finish dropped a captured or timed replacement")
					}
					if timeout == "future" {
						if inc.timer == nil || inc.timerGen != 1 {
							t.Fatalf("replacement timebomb: timer=%v, generation=%d", inc.timer, inc.timerGen)
						}
						inc.timer.Stop()
						tx.fireReadTimeout(inc, inc.timerGen)
					}
					if timeout != "zero" {
						requireLazySuccessorCode(t, context.Cause(inc.ctx), 1031)
						tx.SetTimeout(0)
						requireLazySuccessorCode(t, context.Cause(inc.ctx), 1031)
					}
					lease := tx.enterState()
					defer lease.release()
					state := tx.readVersionState
					if state == nil || state == oldState || state.hasReadVersion || state.userSetReadVersion || state.readVersion != 0 || state.pendingReadVersion != nil || !state.readVersionInstant.IsZero() {
						t.Fatal("successor retained old GRV fields")
					}
					if mode == "retry" {
						if !state.metricStart.Equal(anchor) {
							t.Fatal("retry lost its metric anchor")
						}
					} else if !state.metricStart.IsZero() {
						t.Fatal("new logical transaction retained the metric anchor")
					}
					if native.readState.readVersionState != oldState || !oldState.hasReadVersion || !oldState.userSetReadVersion || oldState.readVersion != 321 || oldState.pendingReadVersion != pending || !oldState.metricStart.Equal(anchor) || !oldState.readVersionInstant.Equal(instant) {
						t.Fatal("turnover cleared or reused state retained by native work")
					}
				})
			}
		}
	}
}

// Not parallel: use an admitted Commit context for each fresh transaction, so
// asynchronous AfterFunc goroutine-cache growth cannot enter this allocation pin.
func TestUnusedSuccessorAvoidsFourAllocations(t *testing.T) {
	measure := func(materialize bool) float64 {
		return testing.AllocsPerRun(100, func() {
			tx := &Transaction{}
			tx.SetReadVersion(123)
			op := &readOperation{Context: tx.readLife.ctx, tx: tx, inc: tx.readLife, parent: context.Background()}
			op.lease = tx.enterReadStateInto(op, op.inc, false, false, &op.leaseStorage)
			if op.lease == nil {
				t.Fatal("failed to admit Commit")
			}
			if err := tx.Commit(op); err != nil {
				t.Fatal(err)
			}
			if materialize {
				tx.readErrMu.Lock()
				tx.readIncarnationLocked()
				tx.readErrMu.Unlock()
			}
		})
	}
	discarded, observed := measure(false), measure(true)
	if observed-discarded != 4 {
		t.Fatalf("LAZY_SUCCESSOR_ALLOC: discarded=%v, observed=%v; want exactly four fewer allocations", discarded, observed)
	}
	t.Logf("LAZY_SUCCESSOR_ALLOC: discarded=%v, observed=%v", discarded, observed)
}
