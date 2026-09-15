package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteCompletionLastOwner(t *testing.T) {
	t.Parallel()
	for i := 0; i < 100; i++ {
		completion := &writeCompletion{done: make(chan error, 1)}
		completion.refs.Store(2)
		var winners atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		for range 2 {
			go func() {
				defer wg.Done()
				if completion.releaseRef() {
					winners.Add(1)
				}
			}()
		}
		wg.Wait()
		if winners.Load() != 1 || completion.refs.Load() != 0 {
			t.Fatalf("ownership winners=%d refs=%d, want 1 and 0", winners.Load(), completion.refs.Load())
		}
	}
}

func testOwnedConn(writeBuffer int) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{ctx: ctx, cancel: cancel, writeCh: make(chan writeReq, writeBuffer)}
}

func TestWriteContextCancellationBeforeEnqueue(t *testing.T) {
	t.Parallel()
	conn := testOwnedConn(0)
	defer conn.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if enqueued, err := conn.SendFrameContext(ctx, UID{First: 1}, []byte("body")); enqueued || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-enqueue cancellation=(%v, %v), want false, context.Canceled", enqueued, err)
	}
	if enqueued, err := conn.SendFrameDeferredContext(ctx, UID{First: 1}, []byte("body")); enqueued || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-enqueue deferred cancellation=(%v, %v), want false, context.Canceled", enqueued, err)
	}
	if conn.hasDirty.Load() {
		t.Fatal("failed deferred enqueue marked connection dirty")
	}
}

func TestWriteContextCancellationAfterEnqueueRetainsWriterOwnership(t *testing.T) {
	t.Parallel()
	conn := testOwnedConn(1)
	defer conn.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	body := []byte("caller-owned")
	type outcome struct {
		enqueued bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		enqueued, err := conn.SendFrameContext(ctx, UID{First: 2}, body)
		done <- outcome{enqueued: enqueued, err: err}
	}()
	request := <-conn.writeCh
	copy(body, []byte("overwritten!!"))
	if bytes.Equal(request.body, body) || string(request.body) != "caller-owned" {
		t.Fatalf("queued body aliases caller: queued=%q caller=%q", request.body, body)
	}
	cancel()
	result := <-done
	if !result.enqueued || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("post-enqueue cancellation=(%v, %v), want true, context.Canceled", result.enqueued, result.err)
	}
	if refs := request.owned.refs.Load(); refs != 1 {
		t.Fatalf("writer ownership refs=%d after caller abandonment, want 1", refs)
	}
	request.owned.complete(nil)
}

func TestWriteContextWriterAcknowledgmentReleasesWaiter(t *testing.T) {
	t.Parallel()
	conn := testOwnedConn(1)
	defer conn.cancel()
	type outcome struct {
		enqueued bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		enqueued, err := conn.SendFrameContext(context.Background(), UID{First: 4}, []byte("written"))
		done <- outcome{enqueued: enqueued, err: err}
	}()
	request := <-conn.writeCh
	request.owned.complete(nil)
	result := <-done
	if !result.enqueued || result.err != nil {
		t.Fatalf("writer acknowledgment=(%v, %v), want true, nil", result.enqueued, result.err)
	}
}

type blockingErrorWriter struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingErrorWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.reached) })
	<-w.release
	return 0, errors.New("forced socket write failure")
}

func TestWriteContextSocketFailureCompletesQueuedRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	network, peer := net.Pipe()
	defer peer.Close()
	connCtx, connCancel := context.WithCancel(context.Background())
	writer := &blockingErrorWriter{reached: make(chan struct{}), release: make(chan struct{})}
	conn := &Conn{
		conn: network, wbuf: bufio.NewWriter(writer), writeCh: make(chan writeReq, 2),
		ctx: connCtx, cancel: connCancel, pending: make(map[UID]chan Response),
	}
	conn.loopWG.Add(1)
	loopDone := make(chan struct{})
	go func() {
		conn.writeLoop()
		close(loopDone)
	}()
	type outcome struct {
		enqueued bool
		err      error
	}
	first, second := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		enqueued, err := conn.SendFrameContext(ctx, UID{First: 3}, []byte("active"))
		first <- outcome{enqueued: enqueued, err: err}
	}()
	select {
	case <-writer.reached:
	case <-ctx.Done():
		t.Fatal("writer did not reach forced socket failure")
	}
	go func() {
		enqueued, err := conn.SendFrameContext(ctx, UID{First: 4}, []byte("queued"))
		second <- outcome{enqueued: enqueued, err: err}
	}()
	for len(conn.writeCh) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("second request was not queued behind blocked writer")
		default:
			runtime.Gosched()
		}
	}
	close(writer.release)
	for name, resultCh := range map[string]<-chan outcome{"active": first, "queued": second} {
		select {
		case result := <-resultCh:
			if !result.enqueued || !errors.Is(result.err, ErrConnClosed) {
				t.Errorf("%s result=(%v, %v), want true, connection closed", name, result.enqueued, result.err)
			}
		case <-ctx.Done():
			t.Fatalf("%s writer remained stranded", name)
		}
	}
	select {
	case <-loopDone:
	case <-ctx.Done():
		t.Fatal("write loop did not terminate after socket failure")
	}
	if enqueued, err := conn.SendFrameContext(ctx, UID{First: 5}, []byte("late")); enqueued || !errors.Is(err, ErrConnClosed) {
		t.Fatalf("post-failure send=(%v, %v), want false, connection closed", enqueued, err)
	}
}

func TestReplyHandleKeepReadyOrCancel(t *testing.T) {
	t.Parallel()
	for _, ready := range []bool{false, true} {
		conn := &Conn{pending: make(map[UID]chan Response)}
		token, replies, handle := conn.PrepareReply()
		if ready {
			conn.pendingMu.Lock()
			conn.pending[token] <- Response{Body: []byte("retained")}
			delete(conn.pending, token)
			conn.pendingMu.Unlock()
		}
		if kept := handle.KeepReadyOrCancel(); kept != ready {
			t.Errorf("ready=%v: kept=%v", ready, kept)
		}
		if ready {
			if response := <-replies; string(response.Body) != "retained" {
				t.Errorf("kept body=%q", response.Body)
			}
		}
		if len(conn.pending) != 0 {
			t.Error("reply token retained after ownership decision")
		}
		handle.Release()
	}
}

func TestReplyHandleTakeReadyOrCancel(t *testing.T) {
	t.Parallel()
	for _, ready := range []bool{false, true} {
		conn := &Conn{pending: make(map[UID]chan Response)}
		token, _, handle := conn.PrepareReply()
		if ready {
			// Same publication critical section as the real connection reader.
			conn.pendingMu.Lock()
			conn.pending[token] <- Response{Body: []byte("retained")}
			delete(conn.pending, token)
			conn.pendingMu.Unlock()
		}
		got, took := handle.TakeReadyOrCancel()
		if took != ready || (ready && string(got.Body) != "retained") {
			t.Errorf("ready=%v: took=%v body=%q", ready, took, got.Body)
		}
		if len(conn.pending) != 0 {
			t.Error("reply token retained after ownership decision")
		}
		handle.Release()
	}
}
