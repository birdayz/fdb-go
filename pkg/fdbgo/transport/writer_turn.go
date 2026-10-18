package transport

import "time"

// maxTurnDelay bounds the wait a sender makes on its own goroutine, jitter
// included. The wait does not observe the sender's context, so a connection
// whose longest wait exceeds it leaves every batch to writeLoop.
const maxTurnDelay = time.Millisecond

// longestCoalesceWait is the most coalesceDelay can return: the larger bound,
// jittered up by 10%.
func longestCoalesceWait(minDelay, maxDelay time.Duration) time.Duration {
	return time.Duration(float64(max(minDelay, maxDelay)) * 1.1)
}

// takeTurn is connectionWriter's turn taken on the sender's goroutine. When the
// connection waits before writing and is idle (nothing queued, no batch in
// progress), the sender waits the delay itself and then writes its frame,
// followed by the frames sent during the wait, in one socket write
// (FlowTransport.actor.cpp:704-732). This saves the two goroutine handoffs
// through writeLoop without changing the bytes or their order.
//
// Frames sent during the wait reach the turn through writeLoop (lockForBatch),
// in queue order; the turn never reads writeCh itself, so it cannot write a
// frame ahead of one queued earlier. writeLoop plays the part of libfdb_c's
// network thread, which serializes a request from another thread only when it
// runs that request's task: a frame queued but not yet handed over when the
// wait ends goes in the next batch, after the next delay.
//
// The batch goes out in one non-blocking write, so a full socket never holds
// the sender past the wait: as for a direct write, writeLoop writes whatever
// the socket refused before any later frame. A connection without non-blocking
// writes (TLS, or not TCP) leaves every batch to writeLoop.
//
// A flush marker handed to the turn promises that every earlier byte is
// written, so if the socket refused part of the batch the marker goes back to
// writeLoop, which writes the rest before acknowledging it.
//
// takeTurn reports false, having done nothing, when the connection is busy or
// cannot take turns. Otherwise it returns once the batch is written or handed
// to writeLoop, or the connection fails.
func (c *Conn) takeTurn(token UID, body []byte) (bool, error) {
	if c.raw == nil || c.coalesce == nil || c.queued.Load() != 0 ||
		longestCoalesceWait(c.minCoalesce, c.maxCoalesce) > c.turnDelayLimit {
		return false, nil
	}
	c.turnMu.Lock()
	if !c.writeMu.TryLock() {
		c.turnMu.Unlock()
		return false, nil
	}
	// A frame may have been queued between the Load and the TryLock; writeLoop
	// holds writeMu until its batch is flushed, so zero here means an idle writer.
	if c.queued.Load() != 0 || c.ctx.Err() != nil {
		c.writeMu.Unlock()
		c.turnMu.Unlock()
		return false, nil
	}
	c.queued.Add(1) // Flush waits for this frame as for any queued one
	c.turns.Add(1)
	c.turnCollecting = true
	c.turnMu.Unlock()

	c.coalesceWait(time.Since(c.lastWrite))

	c.turnMu.Lock()
	batch := c.turnFrames
	c.turnFrames = nil
	c.turnCollecting = false
	c.turnMu.Unlock()

	writeErr := errConnClosed // torn down while waiting: write nothing
	var markers []writeReq
	if c.ctx.Err() == nil {
		writeErr = c.writeTurn(token, body, batch)
		if writeErr == nil && c.tail != nil {
			batch, markers = splitFlushMarkers(batch)
		}
	}
	c.queued.Add(-int64(1 + len(batch))) // returned markers stay counted
	c.writeMu.Unlock()
	for _, req := range batch {
		completeWrite(req, writeErr)
	}
	for _, req := range markers {
		c.returnMarker(req)
	}
	return true, writeErr
}

func completeWrite(req writeReq, err error) {
	if req.errCh != nil {
		req.errCh <- err
	}
	if req.owned != nil {
		req.owned.complete(err)
	}
}

// splitFlushMarkers separates the flush markers (no token, no body) from the frames.
func splitFlushMarkers(batch []writeReq) (frames, markers []writeReq) {
	for _, req := range batch {
		if req.token == (UID{}) && req.body == nil {
			markers = append(markers, req)
		} else {
			frames = append(frames, req)
		}
	}
	return frames, markers
}

// returnMarker queues a flush marker behind a turn's unwritten tail. writeLoop
// writes a pending tail before any frame, so it acknowledges the marker only
// once every earlier byte is written; a torn-down connection fails it.
func (c *Conn) returnMarker(req writeReq) {
	c.writeAdmission.RLock()
	defer c.writeAdmission.RUnlock()
	if !c.writeClosed {
		select {
		case c.writeCh <- req:
			return
		case <-c.ctx.Done():
		}
	}
	c.queued.Add(-1)
	completeWrite(req, errConnClosed)
}

// writeTurn makes the turn's one non-blocking write. Call under writeMu.
func (c *Conn) writeTurn(token UID, body []byte, batch []writeReq) error {
	bufp, frame := encodeFrame(token, body, c.useTLS)
	for _, req := range batch {
		if req.token != (UID{}) || req.body != nil {
			next, f := encodeFrame(req.token, req.body, c.useTLS)
			frame = append(frame, f...)
			putFrame(next, f)
		}
	}
	c.lastWrite = time.Now() // connectionWriter stamps every write (FlowTransport.actor.cpp:716)
	n, err := c.raw.write(frame)
	if err != nil {
		// As in writeLoop: maybe delivered, and failed under writeMu.
		putFrame(bufp, frame)
		err = codedConnClosed(err)
		c.failConnection(err)
		return err
	}
	if n == len(frame) {
		putFrame(bufp, frame)
		return nil
	}
	c.leaveTail(bufp, frame, n)
	return nil
}

// lockForBatch takes writeMu for writeLoop's next batch, whose first frame is
// req. If a sender's turn is still waiting out the delay, req joins that turn
// instead and lockForBatch reports false without taking writeMu. A turn that
// has ended its wait is writing: as in connectionWriter, a frame queued after
// the write began waits for the next delay, here behind writeMu.
func (c *Conn) lockForBatch(req writeReq, first bool) bool {
	if c.coalesce == nil {
		c.writeMu.Lock()
		return true
	}
	c.turnMu.Lock()
	if !first && c.turnCollecting {
		// A turn starts only with nothing queued, so no tail is pending: the
		// kick is for a tail an earlier batch already wrote.
		c.turnMu.Unlock()
		return false
	}
	if first && c.turnCollecting {
		c.turnFrames = append(c.turnFrames, req)
		// Hand over the frames already queued too, in one wakeup. writeLoop
		// is writeCh's only reader, so queue order holds; taking only what was
		// queued at this point keeps producers from prolonging the handoff.
		for range len(c.writeCh) {
			c.turnFrames = append(c.turnFrames, <-c.writeCh)
		}
		c.turnMu.Unlock()
		return false
	}
	locked := c.writeMu.TryLock()
	c.turnMu.Unlock()
	if !locked {
		c.writeMu.Lock()
	}
	return true
}

// withTurnDelayLimit overrides maxTurnDelay. Test-only: lets a turn wait out
// the hour-long delays the tests end by hand.
func withTurnDelayLimit(d time.Duration) DialOption {
	return func(c *Conn) { c.turnDelayLimit = d }
}
