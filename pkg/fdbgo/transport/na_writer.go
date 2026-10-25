package transport

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Design D's writer, D1 (docs/labs/single-network-goroutine/DESIGN.md), off by
// default: FDBGO_NET_DESIGN=1 (D, the C2 delay) or =ng (D-NG, flush when the
// queue is non-empty). The round-3 lab name FDBGO_EXP_NA=1|nodelay selects the same.
// It is a per-connection writer shaped like libfdb_c's
// connectionWriter (FlowTransport.actor.cpp:704-738). Senders append the frame
// to the unsent queue and signal the writer, then wait only for the reply, as
// Peer::send does (:1038-1043). The writer waits the coalescing delay,
// max(MIN, MAX - sinceLastWrite), writes everything queued in one write, and
// blocks until signalled. FDBGO_EXP_NA=nodelay (NG) skips the delay: a
// measured deviation from C++ batching parity (RFC-264/#954).
// readLoop is unchanged.
const (
	designLegacy = "legacy" // today's writeLoop, direct writes and writer turns
	designD      = "d"
	designNG     = "ng"
)

// envDesign is the process-wide writer design; a dial option overrides it.
var envDesign = func() string {
	switch os.Getenv("FDBGO_NET_DESIGN") {
	case "1":
		return designD
	case "ng":
		return designNG
	case "m":
		return designM
	}
	switch os.Getenv("FDBGO_EXP_NA") {
	case "1":
		return designD
	case "nodelay":
		return designNG
	}
	return ""
}()

// NetDesign reports the writer design new connections use by default: "" for
// today's, "d" or "ng" for design D, "m" for D-M.
func NetDesign() string { return envDesign }

// WithLegacyWriter pins today's writer (writeLoop, direct writes, writer turns)
// whatever FDBGO_NET_DESIGN says. Tests of those mechanics use it.
func WithLegacyWriter() DialOption { return func(c *Conn) { c.design = designLegacy } }

// withNetDesign selects design D (designD or designNG) for one connection.
func withNetDesign(mode string) DialOption { return func(c *Conn) { c.design = mode } }

type naWriter struct {
	mu      sync.Mutex
	q       []byte
	nframes int
	delay   bool
	sig     chan struct{}
}

// Lab counters of socket writes and frames over all lab writers
// (FDBGO_NETG_STATS=1 prints them once a second).
var labWrites, labFrames atomic.Int64

var labStatsOnce sync.Once

func startLabStats() {
	if os.Getenv("FDBGO_NETG_STATS") != "1" {
		return
	}
	labStatsOnce.Do(func() {
		go func() {
			for range time.Tick(time.Second) {
				fmt.Fprintf(os.Stderr, "netgstats writes=%d frames=%d\n", labWrites.Load(), labFrames.Load())
			}
		}()
	})
}

func (c *Conn) naSend(token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	LogSend(token, body)
	bufp, frame := encodeFrame(token, body, c.useTLS)
	w := c.na
	w.mu.Lock()
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

// labSend routes a frame to whichever lab writer owns the connection.
func (c *Conn) labSend(token UID, body []byte) error {
	if c.m != nil {
		return c.mSend(token, body)
	}
	if c.na != nil {
		return c.naSend(token, body)
	}
	return c.ngSend(token, body)
}

func (c *Conn) naLoop() {
	defer c.loopWG.Done()
	defer c.recoverLoop("naWriter")
	w := c.na
	var spare []byte
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
		}
		w.mu.Lock()
		buf, n := w.q, w.nframes
		w.q, w.nframes = spare[:0], 0
		w.mu.Unlock()
		if len(buf) > 0 {
			c.lastWrite = time.Now()
			if _, err := c.conn.Write(buf); err != nil {
				c.failConnection(codedConnClosed(err))
				return
			}
			labWrites.Add(1)
			labFrames.Add(int64(n))
		}
		spare = buf
	}
}

// Design D-M (DESIGN.md), FDBGO_NET_DESIGN=m: no writer goroutine. A sender
// that wins the write lock writes its own frame on its goroutine, then drains
// frames other senders queued meanwhile; a sender that loses appends its frame
// and returns. The lock holder re-checks the queue after unlocking, and a
// sender re-tries the lock after appending, so no queued frame is stranded.
// Frames are written whole and in queue order; ctx never aborts a write.
const designM = "m"

type mWriter struct {
	wmu     sync.Mutex // held while writing to the socket
	pmu     sync.Mutex // guards pending
	pending []byte
	npend   int
	spare   []byte
}

func (c *Conn) mSend(token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	LogSend(token, body)
	bufp, frame := encodeFrame(token, body, c.useTLS)
	m := c.m
	if m.wmu.TryLock() {
		err := c.mWrite(frame, 1)
		putFrame(bufp, frame)
		if err != nil {
			m.wmu.Unlock()
			return err
		}
		return c.mDrainAndUnlock()
	}
	m.pmu.Lock()
	m.pending = append(m.pending, frame...)
	m.npend++
	m.pmu.Unlock()
	putFrame(bufp, frame)
	if m.wmu.TryLock() { // the holder may have checked the queue before this append
		return c.mDrainAndUnlock()
	}
	return nil
}

// mDrainAndUnlock writes queued frames until none is left, then releases the
// write lock. Caller holds wmu.
func (c *Conn) mDrainAndUnlock() error {
	m := c.m
	for {
		m.pmu.Lock()
		buf, n := m.pending, m.npend
		m.pending, m.npend = m.spare[:0], 0
		m.pmu.Unlock()
		if n == 0 {
			m.spare = buf
			m.wmu.Unlock()
			m.pmu.Lock()
			empty := m.npend == 0
			m.pmu.Unlock()
			if empty || !m.wmu.TryLock() {
				return nil // nothing left, or a sender that appended holds the lock and drains
			}
			continue
		}
		err := c.mWrite(buf, n)
		m.spare = buf
		if err != nil {
			m.wmu.Unlock()
			return nil // the failure is the connection's; this sender's own frame was written
		}
	}
}

// mWrite writes b whole. A write error fails the connection. Caller holds wmu.
func (c *Conn) mWrite(b []byte, frames int) error {
	c.lastWrite = time.Now()
	if _, err := c.conn.Write(b); err != nil {
		err = codedConnClosed(err)
		c.failConnection(err)
		return err
	}
	labWrites.Add(1)
	labFrames.Add(int64(frames))
	return nil
}
