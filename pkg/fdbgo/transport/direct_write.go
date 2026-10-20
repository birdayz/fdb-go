package transport

// tryEnqueue is enqueue for fire-and-forget frames that a full queue drops.
func (c *Conn) tryEnqueue(req writeReq) bool {
	if order := c.peerSendOrder.Load(); order != nil {
		reserved, accepted, held := c.reserveColdControl(order, req)
		if reserved {
			return accepted
		}
		if held {
			return c.tryEnqueueHeld(req)
		}
	}
	if c.sendWaiters.Pending() {
		return false
	}
	c.writeAdmission.RLock()
	defer c.writeAdmission.RUnlock()
	if c.writeClosed || c.ctx.Err() != nil || c.sendWaiters.Pending() {
		return false
	}
	c.queued.Add(1)
	select {
	case c.writeCh <- req:
		return true
	default:
		c.queued.Add(-1)
		return false
	}
}

// tryWriteDirect writes the frame on the caller's goroutine when writeLoop is idle,
// saving two handoffs; it never blocks, and writeLoop finishes what a full socket refuses.
// A coalescing connection never writes directly: every frame waits in writeLoop.
func (c *Conn) tryWriteDirect(token UID, body []byte) (bool, error) {
	if c.raw == nil || c.coalesce != nil || c.queued.Load() != 0 || !c.writeMu.TryLock() {
		return false, nil
	}
	defer c.writeMu.Unlock()
	// A frame may have been queued between the Load and the TryLock. writeLoop
	// keeps the lock until its batch is flushed, so zero here means a quiet socket.
	if c.queued.Load() != 0 || c.ctx.Err() != nil {
		return false, nil
	}
	bufp, frame := encodeFrame(token, body, c.useTLS)
	n, err := c.raw.write(frame)
	if err != nil {
		// As in writeLoop: maybe delivered, and failed under writeMu so that no
		// frame follows a torn one.
		putFrame(bufp, frame)
		err = codedConnClosed(err)
		c.failConnection(err)
		return true, err
	}
	if n == len(frame) {
		putFrame(bufp, frame)
		return true, nil
	}
	c.leaveTail(bufp, frame, n)
	return true, nil
}

// leaveTail has writeLoop write frame[n:] before any later frame, then pool
// bufp. Call under writeMu.
func (c *Conn) leaveTail(bufp *[]byte, frame []byte, n int) {
	c.tail, c.tailFrame, c.tailBuf = frame[n:], frame, bufp
	c.queued.Add(1)
	c.tails.Add(1)
	select {
	case c.tailKick <- struct{}{}:
	default:
	}
}
