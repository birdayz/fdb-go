package client

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

type failedReadVersionWindow struct{ armed chan struct{} }

func (w *failedReadVersionWindow) arm(time.Duration) bool { close(w.armed); return true }
func (*failedReadVersionWindow) stop()                    {}

func failedReadVersionTransaction(t *testing.T, failure string, native bool) *Transaction {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	window := &failedReadVersionWindow{armed: make(chan struct{})}
	batcher := &grvBatcher{priority: grvPriorityDefault, inFlight: 1, batchTime: time.Hour, window: window}
	db := &database{timeouts: newTimeoutQueue()}
	db.grvBatchers[grvBatcherDefault] = batcher
	tx := &Transaction{db: db}
	t.Cleanup(tx.Cancel)
	producer, stopProducer := context.WithCancel(ctx)
	t.Cleanup(stopProducer)
	done := make(chan error, 1)
	if native {
		lease := tx.enterState()
		lease.release()
	}
	go func() {
		var err error
		if native {
			request := readVersionRequest{db: db}
			_, _, err = tx.readVersionState.cachedOrFetchReadVersion(producer, &request, 0, nil)
		} else {
			_, err = tx.GetReadVersion(producer)
		}
		done <- err
	}()
	select {
	case <-window.armed:
	case <-ctx.Done():
		t.Fatal("GRV producer was not admitted")
	}
	want := ErrDatabaseLocked
	switch failure {
	case "locked":
		batcher.mu.Lock()
		requests := append([]grvRequest(nil), batcher.pending...)
		batcher.mu.Unlock()
		if len(requests) != 1 {
			t.Fatalf("GRV requests=%d", len(requests))
		}
		requests[0].reply <- grvResult{err: &wire.FDBError{Code: ErrDatabaseLocked}}
	case "timeout":
		tx.SetTimeout(1)
		want = ErrTransactionTimedOut
	case "caller_cancel":
		stopProducer()
	default:
		t.Fatal("unknown GRV failure")
	}
	select {
	case err := <-done:
		if failure == "caller_cancel" {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("producer=%v want caller cancellation", err)
			}
		} else if code, ok := fdbErrorCode(err); !ok || code != want {
			t.Fatalf("producer=%v want%d", err, want)
		}
	case <-ctx.Done():
		t.Fatal("GRV failure was not delivered")
	}
	tx.readVersionMu.Lock()
	admitted, pending, cached := tx.readVersionAdmitted, tx.pendingReadVersion, tx.hasReadVersion
	tx.readVersionMu.Unlock()
	if !admitted || pending != nil || cached {
		t.Fatalf("failed future state: admitted=%v pending=%v cached=%v", admitted, pending, cached)
	}
	return tx
}

func TestFailedReadVersionFutureRejectsSetter(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"locked", "timeout", "caller_cancel"} {
		for _, pin := range []int64{0, 123} {
			t.Run(fmt.Sprintf("%s/%d", failure, pin), func(t *testing.T) {
				tx := failedReadVersionTransaction(t, failure, false)
				tx.SetReadVersion(pin)
				_, err := tx.GetReadVersion(context.Background())
				if code, ok := fdbErrorCode(err); !ok || code != 2010 {
					t.Fatalf("setter after failed GRV=%v want2010", err)
				}
			})
		}
	}
}

func TestFailedReadVersionFutureResetAndRetryClearAdmission(t *testing.T) {
	t.Parallel()
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("Reset=%v", reset), func(t *testing.T) {
			tx := failedReadVersionTransaction(t, "locked", false)
			if reset {
				tx.Reset()
			} else if err := tx.OnError(context.Background(), &wire.FDBError{Code: 1007}); err != nil {
				t.Fatal(err)
			}
			tx.SetReadVersion(123)
			if rv, err := tx.GetReadVersion(context.Background()); err != nil || rv != 123 || tx.readVersionAdmitted {
				t.Fatalf("new incarnation=%d/%v admitted=%v", rv, err, tx.readVersionAdmitted)
			}
		})
	}
}

func TestRetainedNativeFailedReadVersionRejectsSetter(t *testing.T) {
	t.Parallel()
	tx := failedReadVersionTransaction(t, "locked", true)
	tx.SetReadVersion(0)
	if _, err := tx.GetReadVersion(context.Background()); admissionErrorCodeForFailedGRV(err) != 2010 {
		t.Fatalf("retained-native failed GRV setter=%v want2010", err)
	}
}

func admissionErrorCodeForFailedGRV(err error) int {
	if err == nil {
		return 0
	}
	if code, ok := fdbErrorCode(err); ok {
		return code
	}
	return -1
}

func TestReadVersionEntryFailureDoesNotAdmitFuture(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"cancel", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			tx := &Transaction{}
			t.Cleanup(tx.Cancel)
			if failure == "cancel" {
				tx.Cancel()
			} else {
				tx.SetTimeout(1)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				select {
				case <-tx.readLife.ctx.Done():
				case <-ctx.Done():
					t.Fatal("transaction timeout was not delivered")
				}
			}
			if _, err := tx.GetReadVersion(context.Background()); err == nil {
				t.Fatal("entry failure was not observed")
			}
			if tx.readVersionAdmitted || tx.pendingReadVersion != nil {
				t.Fatal("entry failure admitted a GRV future")
			}
			tx.SetReadVersion(0)
			if _, err := tx.GetReadVersion(context.Background()); admissionErrorCodeForFailedGRV(err) != 2011 {
				t.Fatalf("setter after entry failure=%v want2011", err)
			}
		})
	}
}
