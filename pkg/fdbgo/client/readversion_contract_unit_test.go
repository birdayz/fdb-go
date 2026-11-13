package client

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func requireReadVersionCode(t *testing.T, tx *Transaction, want int) {
	t.Helper()
	_, err := tx.GetReadVersion(context.Background())
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != want {
		t.Fatalf("GetReadVersion=%v want%d", err, want)
	}
}

func TestSetReadVersionDefersNativeValidation(t *testing.T) {
	t.Parallel()
	for _, version := range []int64{-1, 0} {
		tx := &Transaction{}
		tx.SetReadVersion(version)
		requireReadVersionCode(t, tx, 2011)
		tx.SetReadVersion(123)
		requireReadVersionCode(t, tx, 2011)
		tx.Reset()
		tx.SetReadVersion(123)
		got, err := tx.GetReadVersion(context.Background())
		if err != nil || got != 123 {
			t.Fatalf("valid version after Reset=%d,%v", got, err)
		}
	}
	for _, second := range []int64{-1, 0, 123, 456} {
		tx := &Transaction{}
		tx.SetReadVersion(123)
		tx.SetReadVersion(second)
		requireReadVersionCode(t, tx, 2010)
		if tx.readVersion != 123 {
			t.Fatalf("rejected setter overwrote version with%d", tx.readVersion)
		}
	}
}

func TestSetReadVersionDeferredErrorLeavesCachedFieldsUntouched(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.SetReadVersion(0)
	if tx.readVersion != 0 || tx.hasReadVersion || tx.userSetReadVersion {
		t.Fatalf("invalid pin changed cached fields: version=%d has=%v userSet=%v", tx.readVersion, tx.hasReadVersion, tx.userSetReadVersion)
	}
	tx.SetReadVersion(123)
	if tx.readVersion != 0 || tx.hasReadVersion || tx.userSetReadVersion {
		t.Fatalf("deferred error allowed cached fields to change: version=%d has=%v userSet=%v", tx.readVersion, tx.hasReadVersion, tx.userSetReadVersion)
	}
	requireReadVersionCode(t, tx, 2011)

	tx.Reset()
	tx.SetReadVersion(123)
	if tx.readVersion != 123 || !tx.hasReadVersion || !tx.userSetReadVersion {
		t.Fatalf("valid pin after Reset did not populate cached fields: version=%d has=%v userSet=%v", tx.readVersion, tx.hasReadVersion, tx.userSetReadVersion)
	}
	got, err := tx.GetReadVersion(context.Background())
	if err != nil || got != 123 {
		t.Fatalf("valid version after Reset=%d,%v", got, err)
	}
}

func TestSetReadVersionRejectsPendingGRV(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.SetReadVersion(123)
	tx.readVersionMu.Lock()
	tx.hasReadVersion = false
	pending := &pendingReadVersion{}
	tx.pendingReadVersion = pending
	tx.readVersionMu.Unlock()
	tx.SetReadVersion(456)
	requireReadVersionCode(t, tx, 2010)
	if tx.pendingReadVersion != pending {
		t.Fatal("rejected setter displaced pending GRV")
	}
}

func TestSetReadVersionDeferredErrorPrecedesCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Transaction)
	}{
		{"cancel", func(t *testing.T, tx *Transaction) {
			tx.Cancel()
			requireReadVersionCode(t, tx, ErrTransactionCancelled)
		}},
		{"timeout", func(t *testing.T, tx *Transaction) {
			q, clock, _ := manualTimeoutQueue()
			tx.db = &database{timeouts: q}
			tx.SetTimeout(1)
			if tx.timeoutNode == nil {
				t.Fatal("timeout was not armed")
			}
			clock.Store(tx.timeoutNode.deadline + int64(time.Millisecond))
			q.fire()
			requireReadVersionCode(t, tx, ErrTransactionTimedOut)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &Transaction{}
			tc.setup(t, tx)
			tx.SetReadVersion(0)
			requireReadVersionCode(t, tx, 2011)
		})
	}
}

func TestOnErrorRejectsPostPinnedVersionCodes(t *testing.T) {
	t.Parallel()
	for _, code := range []int{ErrThrottledHotShard, ErrRangeLocked} {
		tx := &Transaction{}
		err := tx.OnError(context.Background(), &wire.FDBError{Code: code})
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != code {
			t.Fatalf("OnError(%d)=%v", code, err)
		}
		if tx.retryCount != 0 {
			t.Fatalf("OnError(%d) retried%d times", code, tx.retryCount)
		}
	}
}

func TestGetReadVersionReturnsAcceptedPinWithoutDatabaseValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		pin         int64
		storageCode int
	}{
		{"stale", 123, 1007},
		{"far_future", math.MaxInt64, 1009},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := &database{}
			db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.grvCache.epochNow(), version: 1000})
			if db.minAcceptable() != 1000 {
				t.Fatal("fixture did not install a current-epoch floor1000")
			}
			tx := &Transaction{db: db}
			defer tx.Cancel()
			before, after := 0, 0
			tx.beforeReadVersionLock = func() { before++ }
			tx.afterReadVersion = func() { after++ }
			tx.SetReadVersion(tc.pin)
			got, err := tx.GetReadVersion(context.Background())
			if err != nil || got != tc.pin {
				t.Fatalf("transaction getter=%d,%v want%d,nil", got, err, tc.pin)
			}
			got, err = tx.Snapshot().GetReadVersion(context.Background())
			if err != nil || got != tc.pin {
				t.Fatalf("snapshot getter=%d,%v want%d,nil", got, err, tc.pin)
			}
			if before != 2 || after != 2 {
				t.Fatalf("getter bypassed hooks: before=%d after=%d", before, after)
			}
			if !tx.hasReadVersion || !tx.userSetReadVersion || tx.readVersion != tc.pin {
				t.Fatalf("getter changed installed pin: has=%v user=%v rv=%d", tx.hasReadVersion, tx.userSetReadVersion, tx.readVersion)
			}

			// Independent controls: storage-facing wrappers must keep validation enabled.
			checkCode := func(name string, err error) {
				t.Helper()
				var fe *wire.FDBError
				if !errors.As(err, &fe) || fe.Code != tc.storageCode {
					t.Fatalf("%s=%v want%d", name, err, tc.storageCode)
				}
			}
			checkCode("database validator", db.validateVersion(tc.pin))
			checkCode("ensureReadVersionFlags", tx.ensureReadVersionFlags(context.Background(), 0))
			_, err = tx.operationReadVersion(context.Background())
			checkCode("operationReadVersion", err)
			if db.minAcceptable() != 1000 {
				t.Fatalf("getter/storage helpers changed known floor=%d", db.minAcceptable())
			}
		})
	}
}

// Unexpected GRV admission is observable synchronously and cannot open an RPC.
// The synthetic in-flight batch keeps requests in this test-owned window.
type explicitGetterWindow struct {
	admissions int
	cancel     context.CancelFunc
}

func (w *explicitGetterWindow) arm(time.Duration) bool { w.admissions++; w.cancel(); return true }
func (w *explicitGetterWindow) stop()                  {}

func explicitGetterNoGRVFixture(t *testing.T) (*database, *explicitGetterWindow, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	window := &explicitGetterWindow{cancel: cancel}
	db := &database{}
	db.grvBatchers[grvBatcherDefault] = &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour, window: window}
	if db.minAcceptable() != 0 || db.minAcceptableReadVersion.Load() != nil {
		t.Fatal("fixture floor was already initialized")
	}
	return db, window, ctx
}

func TestGetReadVersionAcceptedPinDoesNotBootstrapGRV(t *testing.T) {
	t.Parallel()
	// Positive control: this observer must detect and end a real batcher admission.
	t.Run("observer_positive_control", func(t *testing.T) {
		db, window, ctx := explicitGetterNoGRVFixture(t)
		tx := &Transaction{db: db}
		req := tx.readVersionRequest()
		_, _, _, err := db.grvBatchers[grvBatcherDefault].getReadVersion(db, ctx, req.flags, req.span, req.tags, false, false)
		if !errors.Is(err, context.Canceled) || window.admissions != 1 || len(db.grvBatchers[grvBatcherDefault].pending) != 1 {
			t.Fatalf("observer control: err=%v admissions=%d queued=%d", err, window.admissions, len(db.grvBatchers[grvBatcherDefault].pending))
		}
	})
	for _, pin := range []int64{123, math.MaxInt64} {
		for _, snapshot := range []bool{false, true} {
			// Fresh context/DB/transaction for every getter surface and pin.
			db, window, ctx := explicitGetterNoGRVFixture(t)
			tx := &Transaction{db: db}
			tx.SetReadVersion(pin)
			var got int64
			var err error
			if snapshot {
				got, err = tx.Snapshot().GetReadVersion(ctx)
			} else {
				got, err = tx.GetReadVersion(ctx)
			}
			if err != nil || got != pin || ctx.Err() != nil || window.admissions != 0 || len(db.grvBatchers[grvBatcherDefault].pending) != 0 || db.minAcceptableReadVersion.Load() != nil || db.minAcceptable() != 0 {
				t.Fatalf("pin=%d snapshot=%v got=%d err=%v context=%v admissions=%d queued=%d floor=%d stamp=%v", pin, snapshot, got, err, ctx.Err(), window.admissions, len(db.grvBatchers[grvBatcherDefault].pending), db.minAcceptable(), db.minAcceptableReadVersion.Load())
			}
			tx.Cancel()
		}
	}
}
