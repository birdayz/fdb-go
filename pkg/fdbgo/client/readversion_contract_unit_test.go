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
		setup func(*Transaction)
	}{
		{"cancel", func(tx *Transaction) { tx.Cancel() }},
		{"timeout", func(tx *Transaction) { tx.SetTimeout(1); time.Sleep(5 * time.Millisecond) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &Transaction{}
			tc.setup(tx)
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
