package transport

import (
	"context"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"fdb.dev/pkg/fdbgo/internal/sendqueue"
)

// Enqueue-only per-connection writer (RFC-282), shaped like libfdb_c's
// connectionWriter (FlowTransport.actor.cpp:704-738). Senders append the frame
// to the connection's unsent buffer and signal the writer on an empty->non-empty
// transition, then wait only for the reply, as Peer::send does (:1038-1043).
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

// WithAdaptiveLinger configures Nagle-style adaptive linger on the D-NG writer:
// if the last socket write was at least d ago, the writer flushes immediately;
// otherwise it yields (runtime.Gosched, no timer) until d has elapsed since the
// last write. Passing 0 disables linger; the last linger/coalesce option wins.
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
	nframes int
	drained chan struct{} // lazily allocated only when len(q) >= naHighWater
	delay   bool
	// adaptive > 0 is D-NG with Nagle-style linger: if the last write is older
	// than adaptive, write at once; otherwise yield (runtime.Gosched, no timer)
	// until adaptive has passed since the last write, then write.
	adaptive time.Duration
	sig      chan struct{}
}

// naWrite is the D-NG writer's send entry: it preserves cold-dial permit order,
// then appends the frame to the connection's queue without waiting for the
// socket write (FlowTransport.actor.cpp:1038-1043).
func (c *Conn) naWrite(ctx context.Context, token UID, body []byte, flush bool, permit *sendqueue.Permit) (bool, error) {
	if err := ctx.Err(); err != nil {
		permit.Release()
		return false, err
	}
	admission, err := c.beginSend(ctx, permit)
	if err != nil {
		return false, err
	}
	defer admission.permit.Release()
	if admission.held {
		if order := c.peerSendOrder.Load(); order != nil {
			select {
			case <-order.Drained():
			default:
				NotifySuspend(ctx) // waits behind the cold cohort, as enqueueHeld does
			}
			select {
			case <-order.Drained():
			case <-ctx.Done():
				return false, ctx.Err()
			case <-c.ctx.Done():
				return false, errConnClosed
			}
		}
	}
	if c.ctx.Err() != nil {
		return false, errConnClosed
	}
	if flush {
		return true, nil
	}
	if err := c.naSend(ctx, token, body); err != nil {
		return false, err
	}
	return true, nil
}

func (c *Conn) naWaitHighWater(ctx context.Context, w *naWriter) error {
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
			return ctx.Err()
		case <-c.ctx.Done():
			return errConnClosed
		}
		w.mu.Lock()
		if err := ctx.Err(); err != nil {
			w.mu.Unlock()
			return err
		}
		if c.ctx.Err() != nil {
			w.mu.Unlock()
			return errConnClosed
		}
	}
	return nil
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
		w.mu.Lock()
		if c.ctx.Err() != nil {
			w.mu.Unlock()
			return errConnClosed
		}
		if len(w.q) >= naHighWater {
			if err := c.naWaitHighWater(ctx, w); err != nil {
				return err
			}
		}
		LogSend(token, body)
		wake := len(w.q) == 0 // dataToSend fires only on the first unsent frame (Peer::send)
		w.q = append(w.q, frame...)
		w.nframes++
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
	w.mu.Lock()
	if c.ctx.Err() != nil {
		w.mu.Unlock()
		putFrame(bufp, frame)
		return errConnClosed
	}
	if len(w.q) >= naHighWater {
		if err := c.naWaitHighWater(ctx, w); err != nil {
			putFrame(bufp, frame)
			return err
		}
	}
	LogSend(token, body)
	wake := len(w.q) == 0 // dataToSend fires only on the first unsent frame (Peer::send)
	w.q = append(w.q, frame...)
	w.nframes++
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
	if c.ctx.Err() != nil {
		return false
	}
	w := c.na
	w.mu.Lock()
	if len(w.q) >= naHighWater || c.ctx.Err() != nil {
		w.mu.Unlock()
		return false
	}
	LogSend(token, body)
	n := frameSize(body, c.useTLS)
	off := len(w.q)
	wake := off == 0
	if cap(w.q)-off < n {
		w.q = slices.Grow(w.q, n)
	}
	w.q = w.q[:off+n]
	encodeFrameInto(w.q[off:off+n], token, body, c.useTLS)
	w.nframes++
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
			for deadline := c.lastWrite.Add(w.adaptive); time.Now().Before(deadline); {
				if c.ctx.Err() != nil {
					return
				}
				runtime.Gosched()
			}
		}
		w.mu.Lock()
		buf, drained := w.q, w.drained
		w.q, w.nframes, w.drained = spare[:0], 0, nil
		w.mu.Unlock()
		if drained != nil {
			close(drained)
		}
		if len(buf) > 0 {
			c.lastWrite = time.Now()
			if _, err := c.conn.Write(buf); err != nil {
				c.failConnection(codedConnClosed(err))
				return
			}
		}
		spare = buf
	}
}
