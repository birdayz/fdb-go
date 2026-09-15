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

func TestReadIncarnation_ResetBetweenPipelinedSendAndRegistration(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), readIncarnationTestTimeout)
	defer cancel()

	db, _ := newSimTestDB(t, ctx)
	key := []byte(t.Name() + "_seed")
	newKey := []byte(t.Name() + "_new")
	seedValue := []byte("seed-value")
	newValue := []byte("new-generation-value")
	rv := seedReadIncarnationValues(t, ctx, db, key, []byte(t.Name()+"_other"), seedValue, []byte("other"))

	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	parked := make(chan struct{})
	release, releaseIt := releaseGate(t)
	defer releaseIt()
	tx.afterPendingSend = func() { close(parked); <-release }
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
	tx.Set(newKey, newValue)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("new-generation write+Commit: %v", err)
	}
	assertIndependentRead(t, ctx, db, key, seedValue)
	assertIndependentRead(t, ctx, db, newKey, newValue)
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
