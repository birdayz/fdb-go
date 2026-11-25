package transport

import (
	"context"
	"testing"
	"time"
)

func TestSuspendSignalReplyReadiness(t *testing.T) {
	t.Parallel()
	NotifySuspend(context.Background())
	for _, ready := range []bool{false, true} {
		ctx, signal := WithSuspendSignal(context.Background())
		other, untouched := WithSuspendSignal(context.Background())
		h := &ReplyHandle{ch: make(chan Response, 1)}
		if ready {
			h.ch <- Response{}
		}
		derived, cancel := context.WithCancel(ctx)
		h.NotifyWait(derived)
		select {
		case <-signal.Done():
			if ready {
				t.Error("already-ready reply fired suspension")
			}
		default:
			if !ready {
				t.Error("reply wait did not fire suspension")
			}
		}
		select {
		case <-untouched.Done():
			t.Error("another context's signal fired")
		default:
		}
		NotifySuspend(other)
		signal.Fire()
		untouched.Fire()
		cancel()
	}
}

func TestSuspendSignalFullWriteQueue(t *testing.T) {
	t.Parallel()
	conn := testOwnedConn(1)
	defer conn.cancel()
	if !conn.tryEnqueue(writeReq{body: []byte("first")}) {
		t.Fatal("failed to fill queue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx, signal := WithSuspendSignal(ctx)
	defer signal.Fire()
	done := make(chan error, 1)
	go func() {
		_, err := conn.SendFrameDeferredContext(ctx, UID{First: 262}, []byte("second"))
		done <- err
	}()
	select {
	case <-signal.Done():
	case <-ctx.Done():
		t.Fatal("full queue did not signal before enqueue blocked")
	}
	if got := <-conn.writeCh; string(got.body) != "first" {
		t.Fatalf("queue order changed: %q", got.body)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := <-conn.writeCh; string(got.body) != "second" {
		t.Fatalf("queued frame = %q, want second", got.body)
	}
}
