package fdb_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
)

func seedFutureKey(t *testing.T, db fdb.Database) fdb.Key {
	t.Helper()
	key := fdb.Key(t.Name())
	if _, err := db.Transact(func(tr fdb.WritableTransaction) (any, error) {
		tr.Set(key, []byte("v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return key
}

// TestGetFuture_IsReadyPolling pins that a pipelined Get future becomes ready
// while only IsReady is polled, as an fdb_future_is_ready loop does.
func TestGetFuture_IsReadyPolling(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := seedFutureKey(t, db)
	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		f := tr.Get(key)
		deadline := time.Now().Add(10 * time.Second)
		for !f.IsReady() {
			if time.Now().After(deadline) {
				return nil, errors.New("Get future never became ready while IsReady was polled")
			}
			time.Sleep(time.Millisecond)
		}
		if v, err := f.Get(); err != nil || string(v) != "v" {
			return nil, fmt.Errorf("Get = %q, %v; want \"v\"", v, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGetFuture_ConcurrentGet pins that one Get future may be read from several
// goroutines, as a libfdb_c future may.
func TestGetFuture_ConcurrentGet(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := seedFutureKey(t, db)
	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		f := tr.Get(key)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if v, err := f.Get(); err != nil || string(v) != "v" {
					errs <- errors.New("concurrent Get returned " + string(v))
				}
			}()
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil { // closed and empty when every Get succeeded
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFuture_CancelBeforeReady pins fdb_future_cancel: a future cancelled before
// it is ready reports operation_cancelled (1101); a ready one keeps its value.
func TestFuture_CancelBeforeReady(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	key := seedFutureKey(t, db)
	_, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
		f := tr.Get(key)
		f.Cancel()
		var fe fdb.Error
		if _, err := f.Get(); !errors.As(err, &fe) || fe.Code != 1101 {
			return nil, fmt.Errorf("Get after Cancel = %v, want 1101", err)
		}
		ready := tr.Get(key)
		if v, err := ready.Get(); err != nil || string(v) != "v" {
			return nil, fmt.Errorf("Get = %q, %v", v, err)
		}
		ready.Cancel()
		if v, err := ready.Get(); err != nil || string(v) != "v" {
			return nil, fmt.Errorf("Get after a late Cancel = %q, %v; want the value", v, err)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
