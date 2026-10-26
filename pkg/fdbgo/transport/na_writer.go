package transport

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// Enqueue-only per-connection writer (RFC-282, FlowTransport.actor.cpp:704-738, 1038-1043).
const (
	designLegacy = "legacy" // legacy writeLoop, direct writes and writer turns
	designD      = "d"
	designNG     = "ng"
)

// envDesign is the process-wide writer design; a dial option overrides it.
// Defaults to designNG (D-NG), with FDBGO_NET_DESIGN=legacy|0 for the legacy writer.
var envDesign = func() string {
	switch os.Getenv("FDBGO_NET_DESIGN") {
	case "0", designLegacy:
		return ""
	case "1", designD:
		return designD
	default:
		return designNG
	}
}()

// NetDesign reports the writer design new connections use by default: "" for
// the legacy writer, "d" for timer-coalesced design D, or "ng" for D-NG.
func NetDesign() string { return envDesign }

// WithLegacyWriter pins the legacy writer (writeLoop, direct writes, writer turns)
// whatever FDBGO_NET_DESIGN says. Tests of those mechanics use it.
func WithLegacyWriter() DialOption { return func(c *Conn) { c.design = designLegacy } }

// withNetDesign selects design D (designD or designNG) for one connection.
func withNetDesign(mode string) DialOption { return func(c *Conn) { c.design = mode } }

// WithAdaptiveLinger configures Nagle-style adaptive linger on the D-NG writer
// (0 disables; the last linger/coalesce option wins; RFC-282).
func WithAdaptiveLinger(d time.Duration) DialOption {
	return func(c *Conn) {
		c.adaptiveLinger = max(d, 0)
		c.minCoalesce, c.maxCoalesce = 0, 0
		c.coalesceSet = true
	}
}

// AdaptiveLinger reports the connection's active adaptive linger window (0 when
// disabled or when using the legacy writer).
func (c *Conn) AdaptiveLinger() time.Duration {
	if c.na == nil {
		return 0
	}
	return c.na.adaptive
}

const naHighWater = 16 << 20 // 16 MiB high-water backpressure bound when a peer socket stalls

type naWriter struct {
	mu      sync.Mutex
	q       []byte
	drained chan struct{} // lazily allocated only when len(q) >= naHighWater
	delay   bool
	// adaptive > 0 enables Nagle-style linger on D-NG (RFC-282).
	adaptive        time.Duration
	now             func() time.Time // optional clock for tests; nil uses time.Now
	onAdaptiveYield func()           // optional observer for tests
	onCoalesceWait  func()           // optional observer for tests
	beforeLock      func()           // optional test seam before acquiring w.mu in naSend
	sig             chan struct{}
}

func (w *naWriter) nowTime() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (c *Conn) naReserveOrder(permit *sendqueue.Permit) (p *sendqueue.Permit, hPermit *sendqueue.Permit) {
	if permit != nil {
		return permit, nil
	}
	if !c.coldOrderActive.Load() {
		return nil, nil
	}
	if c.beforeControlLock != nil {
		c.beforeControlLock()
	}
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if order := c.peerSendOrder.Load(); order != nil {
		if p, _, reserved := order.TryReserve(); reserved {
			return p, nil
		}
		if order.Pending() || c.heldWaiters.Pending() {
			hp, _ := c.heldWaiters.Reserve()
			return nil, hp
		}
	} else if c.heldWaiters.Pending() {
		hp, _ := c.heldWaiters.Reserve()
		return nil, hp
	}
	c.coldOrderActive.Store(false)
	return nil, nil
}

// naWrite preserves cold-dial and held-cohort FIFO order, then queues the frame
// without waiting for the socket write (FlowTransport.actor.cpp:1038-1043).
func (c *Conn) naWrite(ctx context.Context, token UID, body []byte, flush bool, permit *sendqueue.Permit) (bool, error) {
	return c.naWriteFull(ctx, token, body, flush, false, permit)
}

func (c *Conn) naWriteFull(ctx context.Context, token UID, body []byte, flush, debug bool, permit *sendqueue.Permit) (bool, error) {
	if err := ctx.Err(); err != nil {
		permit.Release()
		return false, c.enqueueError(ctx)
	}
	p, hPermit := c.naReserveOrder(permit)
	defer p.Release()
	defer hPermit.Release()
	if p != nil {
		select {
		case <-p.Ready():
		default:
			NotifySuspend(ctx)
		}
		select {
		case <-p.Ready():
		case <-ctx.Done():
			return false, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return false, errConnClosed
		}
	} else if hPermit != nil {
		select {
		case <-hPermit.Ready():
		default:
			NotifySuspend(ctx)
		}
		select {
		case <-hPermit.Ready():
		case <-ctx.Done():
			return false, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return false, errConnClosed
		}
		if c.prefixDone != nil {
			select {
			case <-c.prefixDone:
			default:
				NotifySuspend(ctx) // waits behind the cold cohort, as enqueueHeld does
			}
			select {
			case <-c.prefixDone:
			case <-ctx.Done():
				return false, c.enqueueError(ctx)
			case <-c.ctx.Done():
				return false, errConnClosed
			}
		}
	}
	if c.ctx.Err() != nil {
		return false, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if flush {
		return true, nil
	}
	if debug && c.debugFrames {
		fmt.Fprintf(c.debugWriter, "[send %s] token=%016x:%016x bodyLen=%d\n", c.conn.RemoteAddr(),
			token.First, token.Second, len(body))
	}
	if err := c.naSend(ctx, token, body); err != nil {
		return false, err
	}
	return true, nil
}

// naWaitHighWater reserves a FIFO place on c.sendWaiters before unlocking w.mu
// or firing NotifySuspend, so high-water suspension preserves sender wire order.
func (c *Conn) naWaitHighWater(ctx context.Context, w *naWriter) (*sendqueue.Permit, error) {
	permit, _ := c.sendWaiters.Reserve()
	ok := false
	defer func() {
		if !ok {
			permit.Release()
		}
	}()
	w.mu.Unlock()
	select {
	case <-permit.Ready():
	default:
		NotifySuspend(ctx)
	}
	select {
	case <-permit.Ready():
	case <-ctx.Done():
		return nil, c.enqueueError(ctx)
	case <-c.ctx.Done():
		return nil, errConnClosed
	}
	w.mu.Lock()
	if c.ctx.Err() != nil {
		w.mu.Unlock()
		return nil, errConnClosed
	}
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		return nil, err
	}
	for len(w.q) >= naHighWater {
		ch := w.drained
		if ch == nil {
			ch = make(chan struct{})
			w.drained = ch
		}
		w.mu.Unlock()
		NotifySuspend(ctx)
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, c.enqueueError(ctx)
		case <-c.ctx.Done():
			return nil, errConnClosed
		}
		w.mu.Lock()
		if c.ctx.Err() != nil {
			w.mu.Unlock()
			return nil, errConnClosed
		}
		if err := ctx.Err(); err != nil {
			w.mu.Unlock()
			return nil, err
		}
	}
	ok = true
	return permit, nil
}

func (c *Conn) naSend(ctx context.Context, token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	w := c.na
	if len(body) <= 4096 {
		var stackBuf [4096 + packetLenWidth + checksumWidth + 16]byte
		n := frameSize(body, c.useTLS)
		frame := stackBuf[:n]
		encodeFrameInto(frame, token, body, c.useTLS)
		if w.beforeLock != nil {
			w.beforeLock()
		}
		w.mu.Lock()
		if c.ctx.Err() != nil {
			w.mu.Unlock()
			return errConnClosed
		}
		if err := ctx.Err(); err != nil {
			w.mu.Unlock()
			return err
		}
		var permit *sendqueue.Permit
		if len(w.q) >= naHighWater || c.sendWaiters.Pending() {
			var err error
			permit, err = c.naWaitHighWater(ctx, w)
			if err != nil {
				return err
			}
		}
		if globalWireLog.Load() != nil {
			if permit == nil {
				permit, _ = c.sendWaiters.Reserve()
			}
			defer permit.Release()
			w.mu.Unlock()
			LogSend(token, body)
			w.mu.Lock()
			if c.ctx.Err() != nil {
				w.mu.Unlock()
				return errConnClosed
			}
			if err := ctx.Err(); err != nil {
				w.mu.Unlock()
				return err
			}
		}
		wake := len(w.q) == 0 // dataToSend fires only on the first unsent frame (Peer::send)
		w.q = append(w.q, frame...)
		permit.Release()
		w.mu.Unlock()
		if wake {
			select {
			case w.sig <- struct{}{}:
			default:
			}
		}
		return nil
	}
	bufp, frame := encodeFrame(token, body, c.useTLS)
	if w.beforeLock != nil {
		w.beforeLock()
	}
	w.mu.Lock()
	if c.ctx.Err() != nil {
		w.mu.Unlock()
		putFrame(bufp, frame)
		return errConnClosed
	}
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		putFrame(bufp, frame)
		return err
	}
	var permit *sendqueue.Permit
	if len(w.q) >= naHighWater || c.sendWaiters.Pending() {
		var err error
		permit, err = c.naWaitHighWater(ctx, w)
		if err != nil {
			putFrame(bufp, frame)
			return err
		}
	}
	if globalWireLog.Load() != nil {
		if permit == nil {
			permit, _ = c.sendWaiters.Reserve()
		}
		returned := false
		defer func() {
			permit.Release()
			if !returned {
				putFrame(bufp, frame)
			}
		}()
		w.mu.Unlock()
		LogSend(token, body)
		w.mu.Lock()
		if c.ctx.Err() != nil {
			w.mu.Unlock()
			return errConnClosed
		}
		if err := ctx.Err(); err != nil {
			w.mu.Unlock()
			return err
		}
		returned = true
	}
	wake := len(w.q) == 0 // dataToSend fires only on the first unsent frame (Peer::send)
	w.q = append(w.q, frame...)
	permit.Release()
	w.mu.Unlock()
	putFrame(bufp, frame)
	if wake {
		select {
		case w.sig <- struct{}{}:
		default:
		}
	}
	return nil
}

// naTrySend appends a fire-and-forget frame without blocking; drops if closed or at high-water.
func (c *Conn) naTrySend(token UID, body []byte) bool {
	if c.ctx.Err() != nil || c.sendWaiters.Pending() {
		return false
	}
	w := c.na
	w.mu.Lock()
	if len(w.q) >= naHighWater || c.sendWaiters.Pending() || c.ctx.Err() != nil {
		w.mu.Unlock()
		return false
	}
	if globalWireLog.Load() != nil {
		permit, _ := c.sendWaiters.Reserve()
		defer permit.Release()
		w.mu.Unlock()
		LogSend(token, body)
		w.mu.Lock()
		if len(w.q) >= naHighWater || c.ctx.Err() != nil {
			w.mu.Unlock()
			return false
		}
	}
	n := frameSize(body, c.useTLS)
	off := len(w.q)
	wake := off == 0
	if cap(w.q)-off < n {
		w.q = slices.Grow(w.q, n)
	}
	w.q = w.q[:off+n]
	encodeFrameInto(w.q[off:off+n], token, body, c.useTLS)
	w.mu.Unlock()
	if wake {
		select {
		case w.sig <- struct{}{}:
		default:
		}
	}
	return true
}

func (c *Conn) naLoop() {
	defer c.loopWG.Done()
	defer c.recoverLoop("naWriter")
	defer func() {
		if c.coalesce != nil {
			_ = c.coalesce.Close()
		}
	}()
	w := c.na
	spare := make([]byte, 0, 64*1024)
	for {
		select {
		case <-w.sig:
		case <-c.ctx.Done():
			return
		}
		if w.delay {
			c.coalesceWait(time.Since(c.lastWrite))
			if c.ctx.Err() != nil {
				return
			}
		} else if w.adaptive > 0 {
			for deadline := c.lastWrite.Add(w.adaptive); w.nowTime().Before(deadline); {
				if c.ctx.Err() != nil {
					return
				}
				if w.onAdaptiveYield != nil {
					w.onAdaptiveYield()
				}
				runtime.Gosched()
			}
		}
		w.mu.Lock()
		buf, drained := w.q, w.drained
		w.q, w.drained = spare[:0], nil
		w.mu.Unlock()
		if drained != nil {
			close(drained)
		}
		if len(buf) > 0 {
			c.lastWrite = w.nowTime()
			if _, err := c.conn.Write(buf); err != nil {
				c.failConnection(codedConnClosed(err))
				return
			}
		}
		spare = buf
	}
}
