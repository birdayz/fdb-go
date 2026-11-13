package client

import (
	"context"
	"errors"
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
