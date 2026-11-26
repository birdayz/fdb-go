package transport

import (
	"context"
	"fmt"
	mathrand "math/rand/v2"
	"sync"
	"time"
)

// errChanPool pools chan error for SendFrame/Flush synchronization.
var errChanPool = sync.Pool{New: func() any { return make(chan error, 1) }}

// writeReq is a frame queued for the write loop.
type writeReq struct {
	owned *writeCompletion // cancellable read request; separate from legacy errCh
	token UID
	body  []byte
	errCh chan<- error // nil = fire-and-forget (deferred writes)
}

// Send sends a request and returns a channel that will receive the response.
// The destToken identifies the remote endpoint (e.g., a StorageServer's getValue endpoint).
// The replyToken is a fresh token for routing the response back.
func (c *Conn) Send(destToken UID, body []byte) (replyToken UID, replyCh <-chan Response, err error) {
	return c.sendInternal(destToken, body)
}

// sendInternal is Send returning the bidirectional reply channel so internal
// callers (SendAndWait) can return it to the pool. Send exposes only the
// receive-only view.
func (c *Conn) sendInternal(destToken UID, body []byte) (replyToken UID, replyCh chan Response, err error) {
	token := c.nextReplyToken()
	ch := getReplyChannel()
	c.putPending(token, pendingEntry{ch: ch, dest: destToken})
	transferred := false
	defer func() {
		if !transferred {
			c.cancelPending(token, ch)
		}
	}()

	if err = c.SendFrame(destToken, body); err != nil {
		return UID{}, nil, fmt.Errorf("write frame: %w", err)
	}

	transferred = true
	return token, ch, nil
}

// SendFrame queues a raw frame on the connection's writer (returning once appended
// on D-NG, or after write/tail handoff on WithLegacyWriter()).
func (c *Conn) SendFrame(destToken UID, body []byte) error {
	if c.na != nil {
		_, err := c.naWriteFull(c.ctx, destToken, body, false, true, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	if c.debugFrames {
		fmt.Fprintf(c.debugWriter, "[send %s] token=%016x:%016x bodyLen=%d\n", c.conn.RemoteAddr(),
			destToken.First, destToken.Second, len(body))
	}
	LogSend(destToken, body)
	if admission.ordinary() {
		if direct, err := c.tryWriteDirect(destToken, body); direct {
			return err
		}
		if took, err := c.takeTurn(c.ctx, destToken, body); took {
			return err
		}
	}
	errCh := errChanPool.Get().(chan error)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{token: destToken, body: body, errCh: errCh}, nil, admission); !ok {
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Wait for writeLoop to write+flush, OR bail if the connection is torn down
	// (Close/monitor/read-error cancels ctx). Without the ctx.Done arm a sender
	// whose frame is still queued when writeLoop exits would block on errCh
	// forever. On the ctx.Done path errCh is deliberately NOT returned to the
	// pool: writeLoop may still hold a reference and send to it, which would
	// surface as a stale buffered value on the next pool user.
	select {
	case err := <-errCh:
		errChanPool.Put(errCh)
		return err
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// SendFrameDeferred queues a raw frame without waiting for a socket write.
func (c *Conn) SendFrameDeferred(destToken UID, body []byte) error {
	if c.na != nil {
		_, err := c.naWrite(c.ctx, destToken, body, false, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	LogSend(destToken, body)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{token: destToken, body: body}, nil, admission); !ok {
		return errConnClosed
	}
	return nil
}

// Flush checks cold-dial admission and liveness on D-NG, or waits for a flush
// marker acknowledgment when WithLegacyWriter() is active.
func (c *Conn) Flush() error {
	if c.na != nil {
		_, err := c.naWrite(c.ctx, UID{}, nil, true, nil)
		return err
	}
	admission, err := c.beginSend(c.ctx, nil)
	if err != nil {
		return err
	}
	defer admission.permit.Release()
	if admission.ordinary() && c.queued.Load() == 0 && c.ctx.Err() == nil {
		return nil
	}
	errCh := errChanPool.Get().(chan error)
	if ok, _ := c.enqueueAdmitted(c.ctx, writeReq{errCh: errCh}, nil, admission); !ok { // empty token+body = flush-only request
		errChanPool.Put(errCh)
		return errConnClosed
	}
	// Bail on connection teardown rather than block forever on errCh (see SendFrame).
	// errCh is not pooled on the ctx.Done path (stale-value hazard).
	select {
	case err := <-errCh:
		errChanPool.Put(errCh)
		return err
	case <-c.ctx.Done():
		return errConnClosed
	}
}

// coalesceWait is connectionWriter's
// delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - sinceWrite))
// (FlowTransport.actor.cpp:708-710; DELAY_JITTER_OFFSET/RANGE 0.9/0.2,
// flow/Knobs.cpp:54-55). failConnection closes the timer, which ends the wait.
func (c *Conn) coalesceWait(sinceWrite time.Duration) {
	if c.coalesce == nil {
		return
	}
	if c.coalesce.Reset(coalesceDelay(c.minCoalesce, c.maxCoalesce, sinceWrite, mathrand.Float64())) == nil {
		if c.na != nil {
			if h := c.na.hooks.Load(); h != nil && h.onCoalesceWait != nil {
				h.onCoalesceWait()
			}
		}
		_ = c.coalesce.Wait()
	}
}

// stampedWriter is wbuf's socket: connectionWriter stamps lastWriteTime just
// before each connection write (FlowTransport.actor.cpp:716-718), including a
// batch that spills the buffer.
type stampedWriter struct{ c *Conn }

func (w stampedWriter) Write(p []byte) (int, error) {
	w.c.lastWrite = time.Now()
	return w.c.conn.Write(p)
}

// CoalesceDelay reports the connection's write delay bounds and whether it waits.
func (c *Conn) CoalesceDelay() (minDelay, maxDelay time.Duration, waits bool) {
	return c.minCoalesce, c.maxCoalesce, c.coalesce != nil
}

// coalesceDelay is the jittered wait for a uniform jitter in [0, 1).
func coalesceDelay(minDelay, maxDelay, sinceWrite time.Duration, jitter float64) time.Duration {
	d := max(minDelay, maxDelay-sinceWrite)
	return time.Duration(float64(d) * (0.9 + 0.2*jitter))
}

// writeLoop is the dedicated write goroutine. It reads frames from writeCh,
// writes them to the buffered writer, and flushes. After the first frame it
// waits the coalescing delay, then drains all other queued frames before
// flushing — so N concurrent SendFrame/SendFrameDeferred calls share one
// flush, as connectionWriter writes everything unsent after its delay
// (FlowTransport.actor.cpp:704-732). It holds writeMu from before the delay
// to the flush, so neither a direct writer nor a sender's turn interleaves a batch.
func (c *Conn) writeLoop() {
	defer c.loopWG.Done()
	if c.coalesce != nil {
		defer c.coalesce.Close()
	}
	var owned []*writeCompletion
	var inBatch int64
	defer func() {
		c.queued.Add(-inBatch)
		c.finishOwnedWrites(owned)
	}()
	defer c.failTailMarkers()
	defer c.recoverLoop("writeLoop")

	// Collect errCh channels that need notification after flush.
	var errChans []chan<- error

	for {
		// The finite cold prefix precedes held frames, including any held
		// admission head not yet in its channel, then ordinary warm traffic.
		req, first, ready := c.nextWrite(true)
		if !ready {
			return
		}
		if first {
			inBatch = 1
			if req.owned != nil {
				owned = append(owned, req.owned)
			}
		}
		if !c.lockForBatch(req, first) {
			inBatch = 0
			clear(owned)
			owned = owned[:0]
			continue // a sender's turn now owns this request and its completion
		}
		if first && c.tail == nil {
			// The rest of a partial write goes on without a new delay, as
			// connectionWriter keeps writing until nothing is unsent
			// (FlowTransport.actor.cpp:714-731).
			c.coalesceWait(time.Since(c.lastWrite))
		}
		if c.ctx.Err() != nil {
			c.writeMu.Unlock()
			return // torn down while waiting: write nothing
		}
		var writeErr error
		// A direct write's unwritten end precedes every frame queued after it.
		if c.tail != nil {
			inBatch++
			_, writeErr = c.wbuf.Write(c.tail)
			putFrame(c.tailBuf, c.tailFrame) // wbuf has copied or written it
			c.tail, c.tailFrame, c.tailBuf = nil, nil, nil
			// Flush markers a turn left behind the tail complete with this batch.
			for _, m := range c.takeTailMarkers() {
				inBatch++
				if m.owned != nil {
					owned = append(owned, m.owned)
				}
				if m.errCh != nil {
					errChans = append(errChans, m.errCh)
				}
			}
		}
		// Process first frame.
		if first {
			if writeErr == nil && (req.token != (UID{}) || req.body != nil) {
				writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
			}
			if req.errCh != nil {
				errChans = append(errChans, req.errCh)
			}
		}
		if inBatch == 0 { // an outbox wake, or an already-written tail's stale kick
			c.writeMu.Unlock()
			continue
		}

		// Do not wait here for an unbuilt prefix member or held head: flush
		// this available batch before the next blocking priority read.
		for writeErr == nil {
			var available bool
			req, _, available = c.nextWrite(false)
			if !available {
				break
			}
			inBatch++
			if req.owned != nil {
				owned = append(owned, req.owned)
			}
			if req.token != (UID{}) || req.body != nil {
				writeErr = WriteFrame(c.wbuf, req.token, req.body, c.useTLS)
			}
			if req.errCh != nil {
				errChans = append(errChans, req.errCh)
			}
		}

		writeErr = c.flushWriteBatchLocked(writeErr, inBatch)
		inBatch = 0
		notifyWriteBatch(errChans, owned, writeErr)
		errChans = errChans[:0]
		owned = owned[:0]
		if writeErr != nil {
			// The socket writer is terminal after any frame/flush failure; the
			// deferred ownership finalizer drains requests outside this batch.
			return
		}
	}
}

func (c *Conn) flushWriteBatchLocked(writeErr error, inBatch int64) error {
	// Flush all accumulated frames.
	if writeErr == nil {
		writeErr = c.wbuf.Flush()
	}
	if writeErr != nil {
		// Fail under writeMu so no direct write can follow a torn frame.
		writeErr = codedConnClosed(writeErr)
		c.failConnection(writeErr)
	}
	c.queued.Add(-inBatch)
	c.writeMu.Unlock()
	return writeErr
}

func notifyWriteBatch(errChans []chan<- error, owned []*writeCompletion, writeErr error) {
	// Notify all waiting senders. A write error means the frame may or may
	// not have reached the peer before the socket died — code it as the
	// request_maybe_delivered teardown (C++ has no synchronous send errors
	// at all; a failed send there surfaces only as the disconnect signal →
	// request_maybe_delivered, fdbrpc.h:794-799).
	for _, ch := range errChans {
		ch <- writeErr
	}
	for i, completion := range owned {
		owned[i] = nil // recovery must not complete an already-released owner
		completion.complete(writeErr)
	}
}

// SendAndWait sends a request and blocks until the response arrives.
func (c *Conn) SendAndWait(ctx context.Context, destToken UID, body []byte) ([]byte, error) {
	replyToken, replyCh, err := c.sendInternal(destToken, body)
	if err != nil {
		return nil, err
	}

	select {
	case resp := <-replyCh:
		// Received: readLoop deleted the token before delivering, so no further
		// send can race — return the channel to the pool.
		c.putReplyCh(replyCh)
		return resp.Body, resp.Err
	case <-ctx.Done():
		// Timed out: remove the still-pending token; pool the channel only if we
		// beat readLoop's delivery (cancelPending), else leave it to GC.
		c.cancelPending(replyToken, replyCh)
		return nil, ctx.Err()
	case <-c.ctx.Done():
		// Connection teardown: failAllPending owns the pending entry and may be
		// delivering an error to replyCh concurrently — leave the channel to GC.
		return nil, errConnClosed
	}
}
