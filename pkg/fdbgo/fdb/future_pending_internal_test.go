package fdb

import (
	"errors"
	"testing"
	"time"
)

// blockingRead is a pipelined read whose reply never comes; its waits end only
// when aborted.
type blockingRead struct{ waits chan struct{} }

func (b *blockingRead) ResolveOrAbort(abort <-chan struct{}) ([]byte, error, bool) {
	b.waits <- struct{}{}
	<-abort
	return nil, nil, true
}

// TestPendingFutureCancelWakesWaiter pins fdb_future_cancel waking a goroutine
// already blocked on the future, with operation_cancelled (1101).
func TestPendingFutureCancelWakesWaiter(t *testing.T) {
	t.Parallel()
	read := &blockingRead{waits: make(chan struct{}, 2)}
	f := &pendingFutureByteSlice{pending: read}
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := f.Get()
			done <- err
		}()
	}
	<-read.waits
	f.Cancel()
	for range 2 {
		select {
		case err := <-done:
			var fe Error
			if !errors.As(err, &fe) || fe.Code != 1101 {
				t.Fatalf("Get after Cancel = %v, want 1101", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Cancel did not wake a blocked Get")
		}
	}
	if !f.IsReady() {
		t.Fatal("cancelled future not ready")
	}
}
