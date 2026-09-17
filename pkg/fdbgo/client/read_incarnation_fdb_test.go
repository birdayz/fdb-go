package client

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

const readIncarnationTestTimeout = 90 * time.Second

func releaseGate(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	releaseIt := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseIt)
	return release, releaseIt
}

func waitReadParked(t *testing.T, ctx context.Context, parked <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatalf("%s did not reach the held FDB reply: %v", operation, ctx.Err())
	}
}

func waitForFDB1025(t *testing.T, done <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-done:
		if fdbCodeOf(err) != 1025 {
			t.Fatalf("%s returned %v, want FDB error 1025", operation, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not stop while its FDB reply remained held", operation)
	}
}

func seedReadIncarnationValues(t *testing.T, ctx context.Context, db *Database, key, secondKey, value, secondValue []byte) int64 {
	t.Helper()
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, value)
		tx.Set(secondKey, secondValue)
		return nil, nil
	}); err != nil {
		t.Fatalf("seed values: %v", err)
	}
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		got, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(got, value) {
			return nil, fmt.Errorf("warm read = %q, want %q", got, value)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("warm storage path: %v", err)
	}
	rv, _, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false)
	if err != nil {
		t.Fatalf("get pinned read version: %v", err)
	}
	return rv
}

func assertIndependentRead(t *testing.T, ctx context.Context, db *Database, key, want []byte) {
	t.Helper()
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("independent success-control read: %v", err)
	}
	got, ok := result.([]byte)
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("independent success-control read = %q, want %q", got, want)
	}
}

func TestReadIncarnation_CancelUnblocksHeldStorageReplies(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		run  func(context.Context, *Transaction, []byte, []byte) error
	}{
		{
			name: "GetPipelined.Resolve",
			run: func(ctx context.Context, tx *Transaction, key, _ []byte) error {
				_, pending, err := tx.GetPipelined(ctx, key)
				if err != nil {
					return err
				}
				if pending == nil {
					return fmt.Errorf("GetPipelined returned nil PendingGet for a server-resident key")
				}
				_, err = pending.Resolve()
				return err
			},
		},
		{
			name: "GetKey",
			run: func(ctx context.Context, tx *Transaction, key, _ []byte) error {
				_, err := tx.GetKey(ctx, key, false, 1)
				return err
			},
		},
		{
			name: "GetRange",
			run: func(ctx context.Context, tx *Transaction, key, secondKey []byte) error {
				end := append(append([]byte(nil), secondKey...), 0)
				_, _, err := tx.GetRange(ctx, key, end, 100)
				return err
			},
		},
		{
			name: "Snapshot.Get",
			run: func(ctx context.Context, tx *Transaction, key, _ []byte) error {
				_, err := tx.Snapshot().Get(ctx, key)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
			defer cancel()

			db, sd := newSimTestDB(t, ctx)
			key := []byte(t.Name() + "_a")
			secondKey := []byte(t.Name() + "_b")
			value := []byte("retained-a")
			secondValue := []byte("retained-b")
			rv := seedReadIncarnationValues(t, ctx, db, key, secondKey, value, secondValue)

			parked := make(chan struct{}, 1)
			release, releaseIt := releaseGate(t)
			sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
				select {
				case parked <- struct{}{}:
					<-release
				default:
				}
				return body, false
			})
			sd.armAddr(storageAddrFor(t, db, ctx, key))

			tx := db.CreateTransaction()
			tx.SetReadVersion(rv)
			tx.rpcTimeoutOverride = time.Hour
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx, tx, key, secondKey) }()

			waitReadParked(t, ctx, parked, tc.name)
			tx.Cancel()
			waitForFDB1025(t, done, tc.name)

			// The real successful reply was retained throughout the cancellation
			// assertion. Release it only now, then prove unrelated reads still work.
			releaseIt()
			sd.setIntercept(nil)
			assertIndependentRead(t, ctx, db, key, value)
			assertIndependentRead(t, ctx, db, secondKey, secondValue)
		})
	}
}

func TestReadIncarnation_RetryReplacementTimeoutInterruptsHeldStorageReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_key")
	value := []byte("held-value")
	rv := seedReadIncarnationValues(t, ctx, db, key, []byte(t.Name()+"_other"), value, []byte("other"))
	addr := storageAddrFor(t, db, ctx, key)

	tx := db.CreateTransaction()
	tx.SetTimeout(500)
	tx.backoffJitter = func() float64 { return 0 }
	if err := tx.OnError(ctx, &wire.FDBError{Code: ErrNotCommitted}); err != nil {
		t.Fatalf("create retry replacement: %v", err)
	}
	tx.SetReadVersion(rv)
	tx.rpcTimeoutOverride = time.Hour

	parked := make(chan struct{}, 1)
	release, releaseIt := releaseGate(t)
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case parked <- struct{}{}:
			<-release
		default:
		}
		return body, false
	})
	sd.armAddr(addr)

	done := make(chan error, 1)
	go func() {
		_, err := tx.Get(ctx, key)
		done <- err
	}()
	waitReadParked(t, ctx, parked, "replacement Get")
	select {
	case err := <-done:
		if code := fdbCodeOf(err); code != ErrTransactionTimedOut {
			t.Fatalf("replacement Get returned %v (code %d), want transaction_timed_out %d", err, code, ErrTransactionTimedOut)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement Get did not time out while its storage reply remained held")
	}

	// The storage reply remains blocked until after the operation has returned
	// 1031. Releasing it here also lets the simulated transport drain cleanly.
	releaseIt()
	sd.setIntercept(nil)
	assertIndependentRead(t, ctx, db, key, value)
}

func TestReadIncarnation_GRVHeldCancellationAndResetIsolation(t *testing.T) {
	t.Parallel()

	for _, action := range []string{"Cancel", "Reset"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
			defer cancel()

			db, sd := newSimTestDB(t, ctx)
			key := []byte(t.Name() + "_key")
			oldValue := []byte("old-generation-value")
			newValue := []byte("new-generation-value")
			if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				tx.Set(key, oldValue)
				return nil, nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			parked := make(chan struct{}, 1)
			release, releaseIt := releaseGate(t)
			sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
				select {
				case parked <- struct{}{}:
				default:
				}
				<-release // No GRV reply can complete, including another proxy's.
				return body, false
			})
			sd.armAll()

			tx := db.CreateTransaction() // No SetReadVersion: the first read must wait on GRV.
			tx.SetSkipGrvCache()
			tx.rpcTimeoutOverride = time.Hour
			oldDone := make(chan error, 1)
			go func() {
				_, err := tx.Get(ctx, key)
				oldDone <- err
			}()
			waitReadParked(t, ctx, parked, "GRV")
			held := false
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				if !tx.readVersionMu.TryLock() {
					held = true
					break
				}
				tx.readVersionMu.Unlock()
				time.Sleep(time.Millisecond)
			}
			if !held {
				t.Fatal("read did not hold readVersionMu across its uncached GRV")
			}

			switch action {
			case "Cancel":
				tx.Cancel()
			case "Reset":
				resetDone := make(chan struct{})
				go func() { tx.Reset(); close(resetDone) }()
				select {
				case <-resetDone:
				case <-time.After(5 * time.Second):
					t.Fatal("Reset waited on the lock held by its own cancelled GRV")
				}
			}
			waitForFDB1025(t, oldDone, "old-generation GRV read after "+action)

			// Let the obsolete real GRV reply arrive only after its operation has
			// completed with 1025. It must neither fail nor satisfy the new incarnation.
			releaseIt()
			sd.setIntercept(nil)
			if action == "Cancel" {
				tx.Reset()
			}

			got, err := tx.Get(ctx, key)
			if err != nil {
				t.Fatalf("new-generation read after %s: %v", action, err)
			}
			if !bytes.Equal(got, oldValue) {
				t.Fatalf("new-generation read after %s = %q, want %q", action, got, oldValue)
			}
			tx.Set(key, newValue)
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("new-generation write+Commit after %s: %v", action, err)
			}
			assertIndependentRead(t, ctx, db, key, newValue)
		})
	}
}

func TestReadIncarnation_EarlyReplacementTimeoutStopsHeldGRV(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db, sd := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_key")
	seedReadIncarnationValues(t, ctx, db, key, []byte(t.Name()+"_other"), []byte("value"), []byte("other"))

	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(60000)
	finish := tx.startTurnover()
	waitingParent, stopWaiting := context.WithCancel(ctx)
	stopWaiting()
	waiting, cleanup := tx.opContext(waitingParent)
	cleanup()
	captured := tx.readOperation(waiting)
	tx.resetFields(false)
	finish()
	if captured == nil || captured.lease != nil {
		t.Fatalf("canceled turnover waiter = %v, want an early capture without a lease", captured)
	}
	tx.SetSkipGrvCache()

	parked := make(chan struct{}, 1)
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
		return body, false
	})
	sd.armAll()
	done := make(chan error, 1)
	go func() { _, err := tx.GetReadVersion(ctx); done <- err }()
	waitReadParked(t, ctx, parked, "replacement GRV")

	tx.readErrMu.Lock()
	inc := tx.readLife
	timer := inc.timer
	tx.readErrMu.Unlock()
	if inc != captured.inc {
		t.Fatal("held GRV did not use the early-captured replacement")
	}
	if timer == nil {
		t.Fatal("replacement GRV is blocked without an armed timeout timer")
	}
	// Expire the actual registered timebomb only once the real FDB reply is
	// held. This removes the scheduling race between GRV dispatch and timeout.
	timer.Reset(0)
	select {
	case err := <-done:
		if code := fdbCodeOf(err); code != 1031 {
			t.Fatalf("held replacement GRV returned %v, want FDB error 1031", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replacement timeout did not stop the held GRV")
	}
	releaseIt()
	sd.setIntercept(nil)
	assertIndependentRead(t, ctx, db, key, []byte("value"))
}

func TestReadIncarnation_ResetBetweenPipelinedSendAndRegistration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()

	db, sd := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_seed")
	newKey := []byte(t.Name() + "_new")
	seedValue := []byte("seed-value")
	newValue := []byte("new-generation-value")
	rv := seedReadIncarnationValues(t, ctx, db, key, []byte(t.Name()+"_other"), seedValue, []byte("other"))
	// The cancellation contract here requires a still-pending response; a
	// response already published before Reset must retain its completed value.
	releaseReply, releaseReplyIt := releaseGate(t)
	defer releaseReplyIt()
	sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
		<-releaseReply
		return body, false
	})
	sd.armAddr(storageAddrFor(t, db, ctx, key))

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	parked := make(chan struct{})
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	tx.afterPendingSend = func(*PendingGet) { close(parked); <-release }
	type result struct {
		value   []byte
		pending *PendingGet
		err     error
	}
	done := make(chan result, 1)
	go func() { value, pending, err := tx.GetPipelined(ctx, key); done <- result{value, pending, err} }()
	waitReadParked(t, ctx, parked, "deferred send before registration")
	resetDone := retireParkedRead(t, ctx, tx)
	releaseIt()
	waitReadParked(t, ctx, resetDone, "reset drain")
	var value []byte
	var pending *PendingGet
	var err error
	select {
	case got := <-done:
		value, pending, err = got.value, got.pending, got.err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if value != nil {
		t.Fatalf("GetPipelined value = %q, want nil after interleaved Reset", value)
	}
	if pending != nil {
		t.Fatalf("GetPipelined pending = %v, want nil after interleaved Reset", pending)
	}
	if fdbCodeOf(err) != 1025 {
		t.Fatalf("GetPipelined after interleaved Reset returned %v, want FDB error 1025", err)
	}
	tx.readErrMu.Lock()
	pendingCount := len(tx.pendingReads)
	tx.readErrMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pendingReads contains %d entries after rejected old-generation registration, want 0", pendingCount)
	}

	// Clear the one-shot seam. The same handle now belongs to the fresh read
	// incarnation and must support both a real commit and independent reads.
	tx.afterPendingSend = nil
	releaseReplyIt()
	sd.setIntercept(nil)
	tx.Set(newKey, newValue)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("new-generation write+Commit: %v", err)
	}
	assertIndependentRead(t, ctx, db, key, seedValue)
	assertIndependentRead(t, ctx, db, newKey, newValue)
}

func TestReadIncarnation_TerminationBeforePipelinedRegistrationRetiresResources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		timeout bool
		reset   bool
		ready   bool
	}{
		{name: "cancel-held"},
		{name: "cancel-ready", ready: true},
		{name: "timeout-held", timeout: true},
		{name: "timeout-ready", timeout: true, ready: true},
		{name: "reset-held", reset: true},
		{name: "reset-ready", reset: true, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
			defer cancel()
			db, sd := newSimTestDB(t, ctx)
			key := []byte(t.Name() + "_key")
			rv := seedReadIncarnationValues(t, ctx, db, key, []byte(t.Name()+"_other"), []byte("value"), []byte("other"))
			addr := storageAddrFor(t, db, ctx, key)
			conn, err := db.db.getOrDial(ctx, addr)
			if err != nil {
				t.Fatal(err)
			}
			responseHeld := make(chan struct{}, 1)
			releaseReply, releaseReplyIt := releaseGate(t)
			defer releaseReplyIt()
			sd.setIntercept(func(_ int, _ transport.UID, body []byte) ([]byte, bool) {
				select {
				case responseHeld <- struct{}{}:
				default:
				}
				<-releaseReply
				return body, false
			})
			sd.armAddr(addr)

			tx := db.CreateTransaction()
			defer tx.Cancel()
			tx.SetReadVersion(rv)
			tx.SetTimeout(60000)
			captured := make(chan *PendingGet, 1)
			releaseSend, releaseSendIt := releaseGate(t)
			defer releaseSendIt()
			tx.afterPendingSend = func(p *PendingGet) { captured <- p; <-releaseSend }
			type result struct {
				value   []byte
				pending *PendingGet
				err     error
			}
			done := make(chan result, 1)
			go func() { value, p, err := tx.GetPipelined(ctx, key); done <- result{value, p, err} }()
			var sent *PendingGet
			select {
			case sent = <-captured:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := conn.FlushContext(ctx); err != nil {
				t.Fatal(err)
			}
			waitReadParked(t, ctx, responseHeld, "unregistered pipelined reply")
			if tc.ready {
				releaseReplyIt()
				for len(sent.replyCh) == 0 {
					select {
					case <-ctx.Done():
						t.Fatal("reply was not published before termination")
					case <-time.After(time.Millisecond):
					}
				}
			}
			wantCode := 1025
			var resetDone <-chan struct{}
			if tc.reset {
				resetDone = retireParkedRead(t, ctx, tx)
			} else if !tc.timeout {
				tx.Cancel()
			} else {
				wantCode = 1031
				inc := tx.readOperation(sent.ctx).inc
				tx.readErrMu.Lock()
				generation := inc.timerGen
				tx.readErrMu.Unlock()
				tx.fireReadTimeout(inc, generation)
			}
			var wantValue []byte
			if tc.ready {
				wantCode = 0
				wantValue = []byte("value")
			}
			matchesError := func(err error) bool {
				if tc.ready {
					return err == nil
				}
				return fdbCodeOf(err) == wantCode
			}
			releaseSendIt()
			select {
			case got := <-done:
				if got.pending != nil || !matchesError(got.err) || !bytes.Equal(got.value, wantValue) {
					t.Errorf("terminated send returned value=%q pending=%v error=%v; want %q, nil, FDB%d", got.value, got.pending != nil, got.err, wantValue, wantCode)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if resetDone != nil {
				waitReadParked(t, ctx, resetDone, "reset after rejected registration")
			}
			// Deliberately never Resolve: terminal registration itself owns the
			// deferred reply handle, timer, and retained operation context.
			tx.readErrMu.Lock()
			pendingCount := len(tx.pendingReads)
			tx.readErrMu.Unlock()
			if pendingCount != 0 {
				t.Errorf("terminated send registered %d pending reads, want 0", pendingCount)
			}
			sent.mu.Lock()
			if !sent.done || sent.replyHandle != nil || sent.timer != nil || sent.cancel != nil || !matchesError(sent.memoErr) || !bytes.Equal(sent.memoVal, wantValue) {
				t.Errorf("unresolved send retained resources: done=%v handle=%v timer=%v cancel=%v error=%v", sent.done, sent.replyHandle != nil, sent.timer != nil, sent.cancel != nil, sent.memoErr)
			}
			sent.mu.Unlock()
			releaseReplyIt()
			sd.setIntercept(nil)
			assertIndependentRead(t, ctx, db, key, []byte("value"))
		})
	}
}

// A completed read-version future owns its answer. Resetting the handle before
// its caller receives that answer cannot substitute the next incarnation's RV.
func TestReadIncarnation_ReadVersionResultSurvivesReuse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	want, err := tx.GetReadVersion(ctx)
	if err != nil || want <= 0 {
		t.Fatalf("initial read version=%d, %v", want, err)
	}
	parked := make(chan struct{})
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	tx.afterReadVersion = func() { close(parked); <-release }
	type result struct {
		version int64
		err     error
	}
	done := make(chan result, 1)
	go func() { version, err := tx.GetReadVersion(ctx); done <- result{version, err} }()
	waitReadParked(t, ctx, parked, "completed read version before delivery")
	resetDone := retireParkedRead(t, ctx, tx)
	releaseIt()
	waitReadParked(t, ctx, resetDone, "reset drain")
	tx.SetReadVersion(want + 100)
	select {
	case got := <-done:
		if got.err != nil || got.version != want {
			t.Fatalf("completed GRV=%d, %v; want original %d", got.version, got.err, want)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tx.afterReadVersion = nil
	if got, err := tx.GetReadVersion(ctx); err != nil || got != want+100 {
		t.Fatalf("new incarnation GRV=%d, %v; want independently set %d", got, err, want+100)
	}
}

func TestReadIncarnation_ReadVersionCannotAcquireReplacement(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	rv, err := tx.GetReadVersion(ctx)
	if err != nil || rv <= 0 {
		t.Fatalf("initial read version=%d, %v", rv, err)
	}
	parked := make(chan struct{})
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	tx.beforeReadVersionLock = func() { close(parked); <-release }
	done := make(chan error, 1)
	go func() { _, err := tx.GetReadVersion(ctx); done <- err }()
	waitReadParked(t, ctx, parked, "read-version acquisition")
	resetDone := retireParkedRead(t, ctx, tx)
	releaseIt()
	waitReadParked(t, ctx, resetDone, "reset drain")
	tx.SetReadVersion(rv + 100)
	waitForFDB1025(t, done, "retired read-version acquisition")
	tx.beforeReadVersionLock = nil
	if got, err := tx.GetReadVersion(ctx); err != nil || got != rv+100 {
		t.Fatalf("fresh read version=%d, %v; want %d", got, err, rv+100)
	}
}

func TestReadIncarnation_ReadCannotUseReplacementWrites(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()
	tx := db.CreateTransaction()
	defer tx.Cancel()
	key := []byte(t.Name())
	tx.Set(key, []byte("old"))
	parked := make(chan struct{})
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	tx.afterReadVersion = func() { close(parked); <-release }
	type result struct {
		value []byte
		err   error
	}
	done := make(chan result, 1)
	go func() { value, err := tx.Get(ctx, key); done <- result{value, err} }()
	waitReadParked(t, ctx, parked, "read before RYW lookup")
	resetDone := retireParkedRead(t, ctx, tx)
	releaseIt()
	waitReadParked(t, ctx, resetDone, "reset drain")
	tx.Set(key, []byte("replacement"))
	select {
	case got := <-done:
		if fdbCodeOf(got.err) != 1025 {
			t.Fatalf("old read=(%q, %v); want 1025", got.value, got.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tx.afterReadVersion = nil
	if got, err := tx.Get(ctx, key); err != nil || string(got) != "replacement" {
		t.Fatalf("new read=(%q, %v), want replacement", got, err)
	}
}

// A parked operation owns an execution lease. Retirement must signal that old
// operation, but cannot publish replacement state until the park is released.
func retireParkedRead(t *testing.T, ctx context.Context, tx *Transaction) <-chan struct{} {
	t.Helper()
	tx.readErrMu.Lock()
	old := tx.readIncarnationLocked()
	tx.readErrMu.Unlock()
	done := make(chan struct{})
	go func() { tx.Reset(); close(done) }()
	waitReadParked(t, ctx, old.ctx.Done(), "old incarnation retirement")
	select {
	case <-done:
		t.Fatal("Reset reused mutable state while the old read was still parked")
	default:
	}
	return done
}

func TestFDBOnErrorCannotEraseTerminalCauseAtTurnover(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"cancel", "timeout"} {
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			db := openTestDB(t, ctx)
			tx := db.CreateTransaction()
			defer tx.Cancel()
			tx.SetTimeout(60000)
			oldKey := []byte(t.Name() + "/discarded")
			tx.Set(oldKey, []byte("old mutation"))
			if _, err := tx.GetReadVersion(ctx); err != nil {
				t.Fatal(err)
			}
			watchKey := []byte(t.Name() + "/watch")
			value, version, span, watchCtx, watchCancel, err := tx.WatchSetup(ctx, watchKey)
			if err != nil {
				t.Fatal(err)
			}
			defer watchCancel()
			watchDone := make(chan error, 1)
			go func() { watchDone <- tx.WatchPoll(watchCtx, watchCancel, watchKey, value, version, span) }()
			if got := db.db.outstandingWatches.Load(); got != 1 {
				t.Fatalf("watch setup charged %d slots, want 1", got)
			}
			tx.readErrMu.Lock()
			old, timerGen := tx.readLife, tx.readLife.timerGen
			tx.readErrMu.Unlock()
			pause := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
			defer pause.unblock()
			tx.beforeTurnoverRetirement = func() {
				close(pause.reached)
				<-pause.release
			}
			done := make(chan error, 1)
			go func() { done <- tx.OnError(ctx, &wire.FDBError{Code: 1020}) }()
			waitCommitLifetimeGate(t, ctx, pause.reached, "OnError immediately before retirement claim")
			want := 1025
			if terminal == "cancel" {
				tx.Cancel()
			} else {
				tx.fireReadTimeout(old, timerGen)
				want = 1031
			}
			waitCommitLifetimeGate(t, ctx, old.ctx.Done(), "terminal cause published before retirement claim")
			pause.unblock()
			err = receiveLifetimeTest(t, done, "OnError after terminal cause")
			requireCommitLifetimeCode(t, err, want, "OnError must not erase completed terminal failure")
			if watchCtx.Err() == nil {
				t.Fatal("terminal OnError left the old watch context live before Reset/Cancel cleanup")
			}
			if watchErr := receiveLifetimeTest(t, watchDone, "terminal watch completion"); watchErr == nil {
				t.Fatal("terminal watch completed successfully on an unchanged key")
			}
			if got := db.db.outstandingWatches.Load(); got != 0 {
				t.Fatalf("terminal OnError leaked %d watch slots before Reset/Cancel cleanup", got)
			}
			tx.readErrMu.Lock()
			current := tx.readLife
			tx.readErrMu.Unlock()
			if current != old {
				t.Fatal("failed conditional turnover installed a replacement")
			}
			_, err = tx.GetReadVersion(ctx)
			requireCommitLifetimeCode(t, err, want, "old transaction remains terminal after OnError")

			// Only an explicit Reset may revive this handle. It must discard
			// old mutations rather than committing them during the next use.
			tx.beforeTurnoverRetirement = nil
			tx.Reset()
			newKey := []byte(t.Name() + "/replacement")
			tx.Set(newKey, []byte("new mutation"))
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("explicit Reset did not restore usability: %v", err)
			}
			if got := readCommitLifetimeValue(t, ctx, db, oldKey); got != nil {
				t.Fatalf("old mutation survived explicit Reset: %q", got)
			}
			if got := readCommitLifetimeValue(t, ctx, db, newKey); string(got) != "new mutation" {
				t.Fatalf("replacement mutation: %q", got)
			}
		})
	}
}

func TestFDBOnErrorReleasedCleanupCannotCancelReplacementWatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.SetTimeout(60000)
	watch := func(key []byte) (context.Context, <-chan error) {
		t.Helper()
		value, version, span, watchCtx, watchCancel, err := tx.WatchSetup(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(watchCancel)
		done := make(chan error, 1)
		go func() { done <- tx.WatchPoll(watchCtx, watchCancel, key, value, version, span) }()
		return watchCtx, done
	}
	_, oldWatchDone := watch([]byte(t.Name() + "/old"))
	tx.readErrMu.Lock()
	old, timerGen := tx.readLife, tx.readLife.timerGen
	tx.readErrMu.Unlock()
	claim := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
	defer claim.unblock()
	cleanup := &pausedCommit{reached: make(chan struct{}), release: make(chan struct{})}
	defer cleanup.unblock()
	tx.beforeTurnoverRetirement = func() {
		close(claim.reached)
		<-claim.release
	}
	tx.beforeOnErrorWatchCleanup = func() {
		close(cleanup.reached)
		<-cleanup.release
	}
	done := make(chan error, 1)
	go func() { done <- tx.OnError(ctx, &wire.FDBError{Code: 1020}) }()
	waitCommitLifetimeGate(t, ctx, claim.reached, "conditional turnover")
	tx.fireReadTimeout(old, timerGen)
	claim.unblock()
	waitCommitLifetimeGate(t, ctx, cleanup.reached, "released-owner terminal cleanup")

	// OnError has rejected turnover and owns no execution lease. Reset must
	// finish now, and its new watch must remain outside that old cleanup.
	tx.Reset()
	if err := receiveLifetimeTest(t, oldWatchDone, "old watch retired by explicit Reset"); err == nil {
		t.Fatal("old watch succeeded on an unchanged key")
	}
	if got := db.db.outstandingWatches.Load(); got != 0 {
		t.Fatalf("explicit Reset left %d old watch slots", got)
	}
	newKey := []byte(t.Name() + "/replacement")
	newWatchCtx, newWatchDone := watch(newKey)
	if got := db.db.outstandingWatches.Load(); got != 1 {
		t.Fatalf("replacement watch charged %d slots, want 1", got)
	}
	cleanup.unblock()
	requireCommitLifetimeCode(t, receiveLifetimeTest(t, done, "old OnError return"), 1031, "old timeout survives Reset")
	if err := newWatchCtx.Err(); err != nil {
		t.Fatalf("old terminal cleanup canceled replacement watch: %v", err)
	}
	requireLifetimeTestBlocked(t, newWatchDone, "replacement watch before a key change")
	if _, err := db.Transact(ctx, func(writer *Transaction) (any, error) {
		// A server-minted value changes even across -test.count iterations.
		writer.Atomic(MutSetVersionstampedValue, newKey, make([]byte, 14))
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	awaitCommitLifetimeResult(t, ctx, newWatchDone, "replacement watch after independent key change")
	if got := db.db.outstandingWatches.Load(); got != 0 {
		t.Fatalf("replacement watch completion left %d slots", got)
	}
}

// A synchronous expiry check must publish into the same resetPromise that its
// operation captured, even when Reset retires that owner before publication.
func TestReadIncarnation_SynchronousTimeoutCannotPoisonReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Transaction, []byte) error
	}{
		{"read-version", func(ctx context.Context, tx *Transaction, _ []byte) error {
			_, err := tx.GetReadVersion(ctx)
			return err
		}},
		{"get", func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.Get(ctx, key)
			return err
		}},
		{"commit", func(ctx context.Context, tx *Transaction, _ []byte) error { return tx.Commit(ctx) }},
		{"prepared-commit", func(ctx context.Context, tx *Transaction, _ []byte) error {
			return tx.PrepareCommit(ctx).Resolve()
		}},
		{"on-error", func(ctx context.Context, tx *Transaction, _ []byte) error {
			return tx.OnError(ctx, &wire.FDBError{Code: 1020})
		}},
		{"estimated-size", func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.GetEstimatedRangeSizeBytes(ctx, key, keyAfterBytes(key))
			return err
		}},
		{"split-points", func(ctx context.Context, tx *Transaction, key []byte) error {
			_, err := tx.GetRangeSplitPoints(ctx, key, keyAfterBytes(key), 100)
			return err
		}},
		{"watch", func(ctx context.Context, tx *Transaction, key []byte) error {
			_, _, _, _, cancel, err := tx.WatchSetup(ctx, key)
			if cancel != nil {
				cancel()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, reset := range []bool{false, true} {
				name := "timeout-first"
				if reset {
					name = "reset-first"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
					defer cancel()
					db := openTestDB(t, ctx)
					key := []byte(t.Name())
					if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
						tx.Set(key, []byte("seed"))
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
					tx := db.CreateTransaction()
					defer tx.Cancel()
					tx.SetTimeout(60000)
					tx.readErrMu.Lock()
					old := tx.readLife
					timer := old.timer
					tx.readErrMu.Unlock()
					if timer == nil || !timer.Stop() {
						t.Fatal("timeout callback must remain undelivered before synchronous expiry")
					}
					// Model a due deadline whose asynchronous callback has not run.
					// Only the synchronous public-operation check can publish 1031.
					tx.deadlineNs.Store(time.Now().Add(-time.Second).UnixNano())
					parked := make(chan struct{})
					release, releaseIt := releaseGate(t)
					defer releaseIt()
					tx.beforeReadTimeoutPublication = func() { close(parked); <-release }
					done := make(chan error, 1)
					go func() { done <- tc.run(ctx, tx, key) }()
					waitReadParked(t, ctx, parked, "synchronous timeout publication")
					var resetDone <-chan struct{}
					want := 1031
					if reset {
						resetDone = retireParkedRead(t, ctx, tx)
						want = 1025
					}
					releaseIt()
					select {
					case err := <-done:
						if fdbCodeOf(err) != want {
							t.Errorf("old operation returned %v, want literal %d from its captured owner", err, want)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if reset {
						waitReadParked(t, ctx, resetDone, "Reset after synchronous timeout lease drain")
					} else {
						tx.Reset()
					}
					tx.beforeReadTimeoutPublication = nil
					if code := fdbCodeOf(tx.readIncarnationCause(old)); code != want {
						t.Errorf("old owner cause = %d, want %d", code, want)
					}
					if got, err := tx.Get(ctx, key); err != nil || string(got) != "seed" {
						t.Fatalf("replacement read = %q, %v; want seed/nil, not retired timeout poison", got, err)
					}
					tx.Set(key, []byte("replacement"))
					if err := tx.Commit(ctx); err != nil {
						t.Fatalf("replacement commit: %v", err)
					}
					assertIndependentRead(t, ctx, db, key, []byte("replacement"))
				})
			}
		})
	}
}
