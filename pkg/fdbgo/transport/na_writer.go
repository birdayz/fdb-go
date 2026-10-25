package transport

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Lab arms NA and NG (docs/labs/single-network-goroutine), off by default.
// FDBGO_EXP_NA=1 (NA): a per-connection writer shaped like libfdb_c's
// connectionWriter (FlowTransport.actor.cpp:704-738). Senders append the frame
// to the unsent queue and signal the writer, then wait only for the reply, as
// Peer::send does (:1038-1043). The writer waits the coalescing delay,
// max(MIN, MAX - sinceLastWrite), writes everything queued in one write, and
// blocks until signalled. FDBGO_EXP_NA=nodelay (NG) skips the delay: a
// measured deviation from C++ batching parity (RFC-264/#954).
// readLoop is unchanged.
var (
	naMode  = os.Getenv("FDBGO_EXP_NA")
	naOn    = naMode == "1" || naMode == "nodelay"
	naDelay = naMode == "1"
)

type naWriter struct {
	mu      sync.Mutex
	q       []byte
	nframes int
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
	w.q = append(w.q, frame...)
	w.nframes++
	w.mu.Unlock()
	putFrame(bufp, frame)
	select {
	case w.sig <- struct{}{}:
	default:
	}
	return nil
}

// labSend routes a frame to whichever lab writer owns the connection.
func (c *Conn) labSend(token UID, body []byte) error {
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
		if naDelay {
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
