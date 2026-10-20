package transport

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if conn.queued.Load() != 0 {
		t.Fatal("failed deferred enqueue left a frame counted as queued")
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

type writePanicContext struct {
	context.Context
	observe func()
}

func (c *writePanicContext) Value(key any) any {
	value := c.Context.Value(key)
	if _, ok := value.(*SuspendSignal); ok {
		c.observe()
	}
	return value
}

// Exclusive: the final completion census must precede another pool acquisition.
func TestWriteContextNotificationPanicRetainsWriterOwner(t *testing.T) {
	conn := testOwnedConn(1)
	defer conn.cancel()
	ctx, signal := WithSuspendSignal(context.Background())
	defer signal.Fire()
	var request writeReq
	var before int32
	ctx = &writePanicContext{Context: ctx, observe: func() {
		request = <-conn.writeCh
		before = request.owned.refs.Load()
		panic(replyPanicMarker)
	}}
	requireReplyPanic(t, func() {
		_, _ = conn.SendFrameContext(ctx, UID{First: 262}, []byte("owned before suspension"))
	})
	if before != 2 || request.token.First != 262 || string(request.body) != "owned before suspension" {
		t.Fatalf("panic did not follow actual frame transfer: refs=%d token=%d body=%q", before, request.token.First, request.body)
	}
	if refs := request.owned.refs.Load(); refs != 1 {
		t.Fatalf("caller panic retained %d completion owners, want only the writer", refs)
	}
	admissionFinish(conn, request)
	if refs := request.owned.refs.Load(); refs != 0 || conn.queued.Load() != 0 {
		t.Fatalf("writer completion retained refs=%d queued=%d", refs, conn.queued.Load())
	}
	if conn.IsClosed() {
		t.Fatal("local notification panic closed the shared connection")
	}
	if ok, err := conn.SendFrameDeferredContext(context.Background(), UID{First: 263}, []byte("reused")); !ok || err != nil {
		t.Fatal(ok, err)
	}
	reused := <-conn.writeCh
	if reused.token.First != 263 || string(reused.body) != "reused" {
		t.Fatalf("same-connection reuse frame = %d/%q", reused.token.First, reused.body)
	}
	admissionFinish(conn, reused)
}

type writeAdmissionPanicContext struct {
	context.Context
	conn       *Conn
	completion *writeCompletion
	reader     bool
	refs       int32
	queued     int64
	empty      bool
}

func (c *writeAdmissionPanicContext) observe() {
	c.refs = c.completion.refs.Load()
	c.queued = c.conn.queued.Load()
	c.empty = len(c.conn.writeCh)+len(c.conn.prefixCh)+len(c.conn.heldCh) == 0
	panic(replyPanicMarker)
}

func (c *writeAdmissionPanicContext) Done() <-chan struct{} {
	if !c.reader {
		c.observe()
	}
	return c.Context.Done()
}

func (c *writeAdmissionPanicContext) Err() error {
	if c.reader {
		c.observe()
	}
	return c.Context.Err()
}

// Exclusive: failed admission returns the completion to the shared pool.
func TestWriteContextAdmissionPanicReleasesBothOwners(t *testing.T) {
	for _, reader := range []bool{false, true} {
		t.Run(fmt.Sprintf("reader=%t", reader), func(t *testing.T) {
			conn := testOwnedConn(1)
			defer conn.cancel()
			completion := newWriteCompletion()
			ctx := &writeAdmissionPanicContext{Context: context.Background(), conn: conn, completion: completion, reader: reader}
			var caller context.Context = ctx
			var wantQueued int64
			if reader {
				conn.ctx, caller, wantQueued = ctx, context.Background(), 1
			}
			requireReplyPanic(t, func() {
				_, _ = conn.writeAdmittedContext(caller, UID{First: 262}, []byte("not admitted"), peerAdmission{}, completion)
			})
			if ctx.refs != 2 || !ctx.empty || ctx.queued != wantQueued {
				t.Fatalf("panic boundary: refs=%d empty=%t queued=%d, want 2/true/%d", ctx.refs, ctx.empty, ctx.queued, wantQueued)
			}
			if refs := completion.refs.Load(); refs != 0 || conn.queued.Load() != 0 || len(completion.done) != 0 {
				t.Fatalf("failed admission retained refs=%d queued=%d replies=%d", refs, conn.queued.Load(), len(completion.done))
			}
			if !conn.writeAdmission.TryLock() {
				t.Fatal("admission panic retained its reader lock")
			}
			conn.writeAdmission.Unlock()
		})
	}
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
		ctx: connCtx, cancel: connCancel, pending: make(map[UID]pendingEntry),
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
		conn := &Conn{pending: make(map[UID]pendingEntry)}
		token, replies, handle := conn.PrepareReply()
		if ready {
			conn.pendingMu.Lock()
			conn.pending[token].ch <- Response{Body: []byte("retained")}
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
		conn := &Conn{pending: make(map[UID]pendingEntry)}
		token, _, handle := conn.PrepareReply()
		if ready {
			// Same publication critical section as the real connection reader.
			conn.pendingMu.Lock()
			conn.pending[token].ch <- Response{Body: []byte("retained")}
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
