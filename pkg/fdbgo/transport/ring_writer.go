package transport

import (
	"encoding/binary"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/zeebo/xxh3"
)

// Lab writer queues for D-NG (DESIGN.md "Options"), off by default:
//
//   - FDBGO_NET_DESIGN=ngy, D-NG + yield: naLoop calls runtime.Gosched once
//     after each write before it waits for the next signal.
//   - FDBGO_NET_DESIGN=ngc, D-NG-chan: senders send the encoded frame on a
//     buffered channel; the writer receives one, drains len(ch) more and makes
//     one write.
//   - FDBGO_NET_DESIGN=ngr, D-NG-ring: a lock-free MPSC byte ring. A sender
//     claims a slot and a byte span with one CAS, encodes the frame in place and
//     marks its slot ready; the writer writes consecutive ready slots with one
//     writev and is woken only when it sleeps. (A first version committed in
//     reservation order, with committers waiting on their predecessor; it
//     collapsed at 256 workers, DESIGN.md round 6.)
const (
	designNGYield = "ngy"
	designNGChan  = "ngc"
	designNGRing  = "ngr"
	// FDBGO_NET_DESIGN=nga: D-NG with adaptive linger of FDBGO_NETG_ADAPTIVE_US
	// microseconds (default 20); see naWriter.adaptive.
	designNGAdaptive = "nga"
)

var envAdaptive = func() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("FDBGO_NETG_ADAPTIVE_US")); err == nil && v > 0 {
		return time.Duration(v) * time.Microsecond
	}
	return 20 * time.Microsecond
}()

// ---- D-NG-chan ----

type chanFrame struct {
	bufp  *[]byte
	frame []byte
}

type chanWriter struct{ ch chan chanFrame }

func (c *Conn) chanSend(token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	LogSend(token, body)
	bufp, frame := encodeFrame(token, body, c.useTLS)
	select {
	case c.cw.ch <- chanFrame{bufp, frame}:
		return nil
	case <-c.ctx.Done():
		putFrame(bufp, frame)
		return errConnClosed
	}
}

func (c *Conn) chanLoop() {
	defer c.loopWG.Done()
	defer c.recoverLoop("chanWriter")
	var buf []byte
	for {
		var f chanFrame
		select {
		case f = <-c.cw.ch:
		case <-c.ctx.Done():
			return
		}
		buf = append(buf[:0], f.frame...)
		putFrame(f.bufp, f.frame)
		n := 1
		for k := len(c.cw.ch); k > 0; k-- {
			f = <-c.cw.ch
			buf = append(buf, f.frame...)
			putFrame(f.bufp, f.frame)
			n++
		}
		c.lastWrite = time.Now()
		if _, err := c.conn.Write(buf); err != nil {
			c.failConnection(codedConnClosed(err))
			return
		}
		labWrites.Add(1)
		labFrames.Add(int64(n))
	}
}

// ---- D-NG-ring ----
//
// The reserve word packs a 16-bit slot sequence (high bits) and a 48-bit byte
// position, so one CAS claims a slot and a byte span together, in one order.
// A sender encodes in place and marks its slot ready; it never waits for an
// earlier sender. The writer advances over consecutive ready slots and writes
// their spans with one writev. A frame larger than ringChunk, or one that does
// not fit in the free bytes, takes a slot with no ring bytes and carries its
// own pooled buffer (ext), which the writer writes in slot order.

const (
	ringSize  = 1 << 20 // bytes; power of two
	ringMask  = ringSize - 1
	ringSlots = 4096 // power of two, < 1<<16
	ringChunk = ringSize / 4
	posBits   = 48
	posMask   = uint64(1)<<posBits - 1
)

type ringSlot struct {
	ready atomic.Uint32 // seq16+1 once filled; 0 when free
	n     uint32        // ring bytes
	ext   []byte        // frame outside the ring, or nil
	extp  *[]byte
}

type ringWriter struct {
	buf      []byte
	slots    [ringSlots]ringSlot
	reserve  atomic.Uint64 // seq16<<48 | pos
	wpos     atomic.Uint64 // bytes before it are written
	wseq     atomic.Uint32 // slots before it are written (mod 1<<16)
	sleeping atomic.Bool
	sig      chan struct{}
}

func frameSize(body []byte, tls bool) int {
	n := packetLenWidth + 16 + len(body)
	if !tls {
		n += checksumWidth
	}
	return n
}

// encodeFrameInto writes the frame into dst, len(dst) == frameSize.
func encodeFrameInto(dst []byte, token UID, body []byte, tls bool) {
	payloadLen := 16 + len(body)
	binary.LittleEndian.PutUint32(dst, uint32(payloadLen))
	p := packetLenWidth
	if !tls {
		p += checksumWidth
	}
	binary.LittleEndian.PutUint64(dst[p:], token.First)
	binary.LittleEndian.PutUint64(dst[p+8:], token.Second)
	copy(dst[p+16:], body)
	if !tls {
		binary.LittleEndian.PutUint64(dst[packetLenWidth:], xxh3.Hash(dst[p:p+payloadLen]))
	}
}

func (r *ringWriter) ringCopy(pos uint64, b []byte) {
	off := int(pos & ringMask)
	k := copy(r.buf[off:], b)
	copy(r.buf, b[k:])
}

func (c *Conn) ringSend(token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	LogSend(token, body)
	r := c.rw
	n := frameSize(body, c.useTLS)
	spins := 0
	for {
		cur := r.reserve.Load()
		seq, pos := uint16(cur>>posBits), cur&posMask
		if seq-uint16(r.wseq.Load()) >= ringSlots { // every slot in flight: back off
			if c.ctx.Err() != nil {
				return errConnClosed
			}
			if spins++; spins < 64 {
				runtime.Gosched()
			} else {
				time.Sleep(20 * time.Microsecond)
			}
			continue
		}
		inRing := n <= ringChunk && pos+uint64(n)-r.wpos.Load() <= ringSize
		adv := uint64(0)
		if inRing {
			adv = uint64(n)
		}
		next := uint64(seq+1)<<posBits | (pos+adv)&posMask
		if !r.reserve.CompareAndSwap(cur, next) {
			continue
		}
		sl := &r.slots[seq&(ringSlots-1)]
		if inRing {
			off := int(pos & ringMask)
			if off+n <= ringSize {
				encodeFrameInto(r.buf[off:off+n], token, body, c.useTLS) // in place
			} else {
				bufp, frame := encodeFrame(token, body, c.useTLS) // wraps: copy in two pieces
				r.ringCopy(pos, frame)
				putFrame(bufp, frame)
			}
			sl.n, sl.ext, sl.extp = uint32(n), nil, nil
		} else {
			sl.extp, sl.ext = encodeFrame(token, body, c.useTLS)
			sl.n = 0
		}
		sl.ready.Store(uint32(seq) + 1)
		if r.sleeping.Load() && r.sleeping.CompareAndSwap(true, false) {
			select {
			case r.sig <- struct{}{}:
			default:
			}
		}
		return nil
	}
}

// appendSpan appends ring bytes [from, to) (sequence positions) to bufs.
func (r *ringWriter) appendSpan(bufs net.Buffers, from, to uint64) net.Buffers {
	if from == to {
		return bufs
	}
	a, b := int(from&ringMask), int(to&ringMask)
	if a < b {
		return append(bufs, r.buf[a:b])
	}
	bufs = append(bufs, r.buf[a:])
	if b > 0 {
		bufs = append(bufs, r.buf[:b])
	}
	return bufs
}

func (c *Conn) ringLoop() {
	defer c.loopWG.Done()
	defer c.recoverLoop("ringWriter")
	r := c.rw
	var bufs, iov net.Buffers
	var exts []*[]byte
	for {
		seq := uint16(r.wseq.Load())
		start := r.wpos.Load()
		pos := start
		span := start
		bufs, exts = bufs[:0], exts[:0]
		k := 0
		for ; k < 1024; k++ {
			sl := &r.slots[(seq+uint16(k))&(ringSlots-1)]
			if sl.ready.Load() != uint32(seq+uint16(k))+1 {
				break
			}
			if sl.ext != nil {
				bufs = r.appendSpan(bufs, span, pos)
				bufs = append(bufs, sl.ext)
				exts = append(exts, sl.extp)
				span = pos
			}
			pos += uint64(sl.n)
		}
		if k == 0 {
			r.sleeping.Store(true)
			if r.slots[seq&(ringSlots-1)].ready.Load() == uint32(seq)+1 {
				r.sleeping.Store(false)
				continue
			}
			select {
			case <-r.sig:
			case <-c.ctx.Done():
				return
			}
			continue
		}
		bufs = r.appendSpan(bufs, span, pos)
		c.lastWrite = time.Now()
		iov = append(iov[:0], bufs...) // WriteTo consumes its receiver
		if _, err := iov.WriteTo(c.conn); err != nil {
			c.failConnection(codedConnClosed(err))
			return
		}
		for i := 0; i < k; i++ {
			sl := &r.slots[(seq+uint16(i))&(ringSlots-1)]
			if sl.ext != nil {
				putFrame(sl.extp, sl.ext)
				sl.ext, sl.extp = nil, nil
			}
			sl.ready.Store(0)
		}
		r.wpos.Store(pos)
		r.wseq.Store(uint32(seq + uint16(k)))
		labWrites.Add(1)
		labFrames.Add(int64(k))
	}
}
