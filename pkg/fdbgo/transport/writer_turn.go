package transport

import "time"

// maxTurnDelay bounds the delay a sender waits out on its own goroutine. The
// wait does not observe the sender's context, so a longer configured delay
// leaves every batch to writeLoop.
const maxTurnDelay = time.Millisecond

// takeTurn is connectionWriter's turn taken on the sender's goroutine. When the
// connection waits before writing and is idle (nothing queued, no batch in
// progress), the sender waits the delay itself and then writes its frame,
// followed by every frame sent during the wait, in one socket write
// (FlowTransport.actor.cpp:704-732). This saves the two goroutine handoffs
// through writeLoop without changing the bytes, their order, or the delay.
//
// Frames sent during the wait reach the turn through writeLoop (lockForBatch),
// in queue order; the turn never reads writeCh itself, so it cannot write a
// frame ahead of one queued earlier. A frame writeLoop has not handed over
// when the wait ends goes in the next batch.
//
// The batch goes out in one non-blocking write, so a full socket never holds
// the sender past the wait: as for a direct write, writeLoop writes whatever
// the socket refused before any later frame. A connection without non-blocking
// writes (TLS, or not TCP) leaves every batch to writeLoop.
//
// takeTurn reports false, having done nothing, when the connection is busy or
// cannot take turns. Otherwise it returns once the batch is written or handed
// to writeLoop, or the connection fails.
func (c *Conn) takeTurn(token UID, body []byte) (bool, error) {
	if c.raw == nil || c.coalesce == nil || c.maxCoalesce > c.turnDelayLimit || c.queued.Load() != 0 {
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
	if c.ctx.Err() == nil {
		writeErr = c.writeTurn(token, body, batch)
	}
	c.queued.Add(-int64(1 + len(batch)))
	c.writeMu.Unlock()
	for _, req := range batch {
		if req.errCh != nil {
			req.errCh <- writeErr
		}
		if req.owned != nil {
			req.owned.complete(writeErr)
		}
	}
	return true, writeErr
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
	if first && c.turnCollecting {
		c.turnFrames = append(c.turnFrames, req)
		// Hand over every frame already queued too, in one wakeup. writeLoop
		// is writeCh's only reader, so queue order holds.
	drain:
		for {
			select {
			case next := <-c.writeCh:
				c.turnFrames = append(c.turnFrames, next)
			default:
				break drain
			}
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
