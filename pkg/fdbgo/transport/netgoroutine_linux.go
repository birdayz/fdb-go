//go:build linux

package transport

// EXPERIMENT (docs/labs/single-network-goroutine): FDBGO_NETWORK_GOROUTINE=1 runs
// all plaintext connection I/O of the process on one goroutine, like flow's
// Net2::run (flow/Net2.actor.cpp:1506): one epoll set holds every socket, an
// eventfd (reactor.wake) and a timerfd (connectionWriter's coalescing delay).
// The goroutine parks in the Go netpoller on the epoll fd itself, so it is an
// ordinary goroutine, not a blocked thread. Off by default; benchmark use only.
// Unlike readLoop it does not contain panics: a panic here ends the process.

import (
	"encoding/binary"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/zeebo/xxh3"
)

// "1": the loop owns reads and writes. "writer": variant (a), the loop owns
// writes only and each connection keeps its readLoop.
var (
	netgMode    = os.Getenv("FDBGO_NETWORK_GOROUTINE")
	netgEnabled = netgMode == "1" || netgMode == "writer"
	netgWriter  = netgMode == "writer"
)

const (
	ngOpFrame = iota
	ngOpRegister
	ngOpClose
)

type ngSub struct {
	op    uint8
	st    *ngConn
	bufp  *[]byte
	frame []byte
}

// ngConn is a connection's state; every field but c and fd is loop-owned.
type ngConn struct {
	c         *Conn
	fd        int
	out       []byte
	flushAt   int64 // monotonic ns; 0 = nothing scheduled
	lastWrite int64
	wantOut   bool // EPOLLOUT armed: the socket refused part of out
	readable  bool // the loop reads this socket (not in writer mode)
	loop      *netLoop
	nframes   int // frames in out
	outOff    int // written prefix of out (FDBGO_NETG_OUTFIX)
	in        []byte
	closed    bool
}

type netLoop struct {
	epfd, efd, tfd int
	rc             syscall.RawConn

	mu       sync.Mutex
	subs     []ngSub
	nsubs    atomic.Int32
	sleeping atomic.Bool

	conns     map[int32]*ngConn
	scheduled []*ngConn
	timerAt   int64
	events    [128]syscall.EpollEvent
	nevents   int

	// Counters for the write-up.
	Wakes, Iterations, Writes, Reads atomic.Int64
}

// Round-2 levers, each off by default (docs/labs/single-network-goroutine):
// FDBGO_NETG_LOOPS=K shards connections over K loops (round-robin);
// FDBGO_NETG_RBUF=bytes sets the per-connection read buffer;
// FDBGO_NETG_FLUSH_FRAMES=N writes a connection as soon as N frames are pending;
// FDBGO_NETG_OUTFIX=1 keeps the out buffer's capacity across writes.
var (
	netgLoops       = envInt("FDBGO_NETG_LOOPS", 1)
	netgRbuf        = envInt("FDBGO_NETG_RBUF", readBufferSize)
	netgFlushFrames = envInt("FDBGO_NETG_FLUSH_FRAMES", 0)
	netgOutFix      = os.Getenv("FDBGO_NETG_OUTFIX") == "1"
	// FDBGO_NETG_ADAPT=K skips the coalescing delay while the connection has
	// fewer than K replies outstanding; FDBGO_NETG_LOCKTHREAD=1 wires each loop to its thread.
	netgAdapt      = envInt("FDBGO_NETG_ADAPT", 0)
	netgLockThread = os.Getenv("FDBGO_NETG_LOCKTHREAD") == "1"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

var (
	netgOnce  sync.Once
	netgLoopv []*netLoop
	netgErr   error
	netgNext  atomic.Uint32
)

func getNetLoop() (*netLoop, error) {
	netgOnce.Do(func() {
		for range netgLoops {
			l, err := startNetLoop()
			if err != nil {
				netgErr = err
				return
			}
			netgLoopv = append(netgLoopv, l)
		}
	})
	if netgErr != nil {
		return nil, netgErr
	}
	return netgLoopv[int(netgNext.Add(1)-1)%len(netgLoopv)], nil
}

func startNetLoop() (*netLoop, error) {
	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	efdR, _, e := syscall.Syscall(syscall.SYS_EVENTFD2, 0, syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != 0 {
		return nil, e
	}
	tfdR, _, e := syscall.Syscall(syscall.SYS_TIMERFD_CREATE, 1 /* CLOCK_MONOTONIC */, syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != 0 {
		return nil, e
	}
	l := &netLoop{epfd: epfd, efd: int(efdR), tfd: int(tfdR), conns: map[int32]*ngConn{}}
	for _, fd := range []int{l.efd, l.tfd} {
		ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
		if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
			return nil, err
		}
	}
	if err := syscall.SetNonblock(epfd, true); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(epfd), "netg-epoll")
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	l.rc = rc
	go l.run(f)
	return l, nil
}

func (l *netLoop) submit(s ngSub) {
	l.mu.Lock()
	l.subs = append(l.subs, s)
	l.nsubs.Add(1)
	l.mu.Unlock()
	if l.sleeping.CompareAndSwap(true, false) {
		l.Wakes.Add(1)
		var one [8]byte
		one[0] = 1
		_, _ = syscall.Write(l.efd, one[:])
	}
}

// netgRegister moves a handshaken plaintext TCP connection onto the loop. The
// loop keeps a dup of the socket and the net.Conn is closed, so the runtime's
// netpoller no longer watches it.
func netgRegister(c *Conn) bool {
	if !netgEnabled || c.useTLS {
		return false
	}
	tc, ok := c.conn.(*net.TCPConn)
	if !ok {
		return false
	}
	l, err := getNetLoop()
	if err != nil {
		return false
	}
	rc, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	dup := -1
	if rc.Control(func(fd uintptr) { dup, err = syscall.Dup(int(fd)) }) != nil || err != nil {
		return false
	}
	syscall.CloseOnExec(dup)
	if syscall.SetNonblock(dup, true) != nil {
		syscall.Close(dup)
		return false
	}
	st := &ngConn{c: c, fd: dup, lastWrite: monoNow(), readable: !netgWriter, loop: l}
	if st.readable {
		_ = tc.Close() // the dup keeps the socket open
		st.in = make([]byte, 0, netgRbuf)
	}
	c.ng = st
	c.labw = true
	startLabStats()
	l.submit(ngSub{op: ngOpRegister, st: st})
	return true
}

// ngSend encodes on the caller and hands the frame to the loop; it never waits
// for the write. A frame sent after teardown is dropped, and its reply waiter
// is failed by failAllPending.
func (c *Conn) ngSend(token UID, body []byte) error {
	if c.ctx.Err() != nil {
		return errConnClosed
	}
	LogSend(token, body)
	bufp, frame := encodeFrame(token, body, false)
	c.ng.loop.submit(ngSub{op: ngOpFrame, st: c.ng, bufp: bufp, frame: frame})
	return nil
}

func (c *Conn) ngClose() { c.ng.loop.submit(ngSub{op: ngOpClose, st: c.ng}) }

func (l *netLoop) run(f *os.File) {
	if netgLockThread {
		runtime.LockOSThread()
	}
	var subs []ngSub
	pollFn := func(uintptr) bool {
		n, err := syscall.EpollWait(l.epfd, l.events[:], 0)
		if err != nil && err != syscall.EINTR {
			panic(err)
		}
		l.nevents = max(n, 0)
		return n > 0
	}
	for {
		l.Iterations.Add(1)
		// 1. Hand-offs from callers.
		if l.nsubs.Load() != 0 {
			l.mu.Lock()
			subs, l.subs = l.subs, subs[:0]
			l.nsubs.Store(0)
			l.mu.Unlock()
			now := monoNow()
			for i := range subs {
				l.apply(&subs[i], now)
				subs[i] = ngSub{}
			}
		}
		// 2. Writes whose coalescing delay has passed.
		l.flushDue(monoNow())
		// 3. Sleep unless more work arrived; the eventfd wakes us.
		l.sleeping.Store(true)
		if l.nsubs.Load() != 0 {
			l.sleeping.Store(false)
			n, _ := syscall.EpollWait(l.epfd, l.events[:], 0)
			l.nevents = max(n, 0)
		} else {
			if err := l.rc.Read(pollFn); err != nil {
				panic(err)
			}
			l.sleeping.Store(false)
		}
		// 4. Socket readiness.
		for i := 0; i < l.nevents; i++ {
			ev := l.events[i]
			switch int(ev.Fd) {
			case l.efd, l.tfd:
				var b [8]byte
				_, _ = syscall.Read(int(ev.Fd), b[:])
				if int(ev.Fd) == l.tfd {
					l.timerAt = 0
				}
				continue
			}
			st := l.conns[ev.Fd]
			if st == nil || st.closed {
				continue
			}
			if ev.Events&syscall.EPOLLOUT != 0 && st.wantOut {
				l.write(st, monoNow())
			}
			if ev.Events&(syscall.EPOLLIN|syscall.EPOLLERR|syscall.EPOLLHUP) != 0 && !st.closed {
				if st.readable {
					l.readConn(st)
				} else if ev.Events&(syscall.EPOLLERR|syscall.EPOLLHUP) != 0 {
					// readLoop sees the failure; stop level-triggered HUP reports.
					_ = syscall.EpollCtl(l.epfd, syscall.EPOLL_CTL_DEL, st.fd, nil)
					st.closed = true
				}
			}
		}
	}
}

func (l *netLoop) apply(s *ngSub, now int64) {
	st := s.st
	switch s.op {
	case ngOpRegister:
		l.conns[int32(st.fd)] = st
		ev := syscall.EpollEvent{Events: st.inEvents(), Fd: int32(st.fd)}
		if err := syscall.EpollCtl(l.epfd, syscall.EPOLL_CTL_ADD, st.fd, &ev); err != nil {
			l.fail(st, err)
		}
	case ngOpFrame:
		if !st.closed {
			st.out = append(st.out, s.frame...)
			st.nframes++
			if netgFlushFrames > 0 && st.nframes >= netgFlushFrames && st.flushAt > now && !st.wantOut {
				st.flushAt = now // enough frames for one packet: write without the rest of the delay
			}
			if st.flushAt == 0 && !st.wantOut {
				c := st.c
				d := time.Duration(0)
				if (c.minCoalesce > 0 || c.maxCoalesce > 0) && (netgAdapt == 0 || c.pendingCount() >= netgAdapt) {
					d = coalesceDelay(c.minCoalesce, c.maxCoalesce, time.Duration(now-st.lastWrite), mathrand.Float64())
				}
				st.flushAt = now + int64(d)
				if st.flushAt == 0 {
					st.flushAt = 1
				}
				l.scheduled = append(l.scheduled, st)
			}
		}
		putFrame(s.bufp, s.frame)
	case ngOpClose:
		l.teardown(st)
	}
}

func (l *netLoop) flushDue(now int64) {
	next := int64(0)
	kept := l.scheduled[:0]
	for _, st := range l.scheduled {
		if st.closed || st.flushAt == 0 {
			continue
		}
		if st.flushAt <= now {
			st.flushAt = 0
			l.write(st, now)
			continue
		}
		kept = append(kept, st)
		if next == 0 || st.flushAt < next {
			next = st.flushAt
		}
	}
	clear(l.scheduled[len(kept):])
	l.scheduled = kept
	if next != 0 && (l.timerAt == 0 || next < l.timerAt) {
		l.armTimer(next - now)
		l.timerAt = next
	}
}

type itimerspec struct{ interval, value syscall.Timespec }

func (l *netLoop) armTimer(d int64) {
	if d < 1000 {
		d = 1000
	}
	spec := itimerspec{value: syscall.NsecToTimespec(d)}
	_, _, _ = syscall.Syscall6(syscall.SYS_TIMERFD_SETTIME, uintptr(l.tfd), 0, uintptr(unsafe.Pointer(&spec)), 0, 0, 0)
}

func (l *netLoop) write(st *ngConn, now int64) {
	if st.nframes > 0 {
		labWrites.Add(1)
		labFrames.Add(int64(st.nframes))
	}
	st.nframes = 0
	for len(st.out) > st.outOff {
		n, err := syscall.Write(st.fd, st.out[st.outOff:])
		l.Writes.Add(1)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN {
			if !st.wantOut {
				st.wantOut = true
				ev := syscall.EpollEvent{Events: st.inEvents() | syscall.EPOLLOUT, Fd: int32(st.fd)}
				_ = syscall.EpollCtl(l.epfd, syscall.EPOLL_CTL_MOD, st.fd, &ev)
			}
			return
		}
		if err != nil {
			l.fail(st, codedConnClosed(err))
			return
		}
		st.lastWrite = now
		if netgOutFix {
			st.outOff += n
		} else {
			st.out = st.out[n:]
		}
	}
	st.out, st.outOff = st.out[:0], 0
	if st.wantOut {
		st.wantOut = false
		ev := syscall.EpollEvent{Events: st.inEvents(), Fd: int32(st.fd)}
		_ = syscall.EpollCtl(l.epfd, syscall.EPOLL_CTL_MOD, st.fd, &ev)
	}
}

func (st *ngConn) inEvents() uint32 {
	if st.readable {
		return syscall.EPOLLIN | syscall.EPOLLRDHUP
	}
	return 0
}

var errNetgEOF = errors.New("netgoroutine: peer closed")

func (l *netLoop) readConn(st *ngConn) {
	for {
		if cap(st.in)-len(st.in) < 4096 {
			grown := make([]byte, len(st.in), 2*cap(st.in)+4096)
			copy(grown, st.in)
			st.in = grown
		}
		free := st.in[len(st.in):cap(st.in)]
		n, err := syscall.Read(st.fd, free)
		l.Reads.Add(1)
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			l.fail(st, err)
			return
		}
		if n == 0 {
			l.fail(st, errNetgEOF)
			return
		}
		st.c.bytesReceived.Add(int64(n))
		st.in = st.in[:len(st.in)+n]
		if !l.parse(st) {
			return
		}
		if n < len(free) {
			break
		}
	}
}

// parse dispatches every whole frame in st.in, as readLoop does per frame.
func (l *netLoop) parse(st *ngConn) bool {
	c := st.c
	buf := st.in
	pingToken := WellKnownToken(WLTokenPingPacket)
	for len(buf) >= packetLenWidth+checksumWidth {
		payloadLen := int(binary.LittleEndian.Uint32(buf))
		if payloadLen < minPayloadSize || payloadLen > maxPayloadSize {
			l.fail(st, fmt.Errorf("bad payload length %d", payloadLen))
			return false
		}
		total := packetLenWidth + checksumWidth + payloadLen
		if len(buf) < total {
			break
		}
		want := binary.LittleEndian.Uint64(buf[packetLenWidth:])
		payload := make([]byte, payloadLen)
		copy(payload, buf[packetLenWidth+checksumWidth:total])
		buf = buf[total:]
		if got := xxh3.Hash(payload); got != want {
			l.fail(st, fmt.Errorf("checksum mismatch: got %x, want %x", got, want))
			return false
		}
		token := UID{First: binary.LittleEndian.Uint64(payload), Second: binary.LittleEndian.Uint64(payload[8:])}
		body := payload[16:]
		LogRecv(token, body)
		switch token {
		case pingToken:
			c.handlePing(body)
			continue
		case endpointNotFoundToken:
			c.handleEndpointNotFound(body)
			continue
		}
		c.pendingMu.Lock()
		if entry, ok := c.pending[token]; ok {
			entry.ch <- Response{Body: body, RecvAt: time.Now()}
			delete(c.pending, token)
		}
		c.pendingMu.Unlock()
	}
	rest := copy(st.in, buf)
	st.in = st.in[:rest]
	return true
}

func (l *netLoop) fail(st *ngConn, err error) {
	l.teardown(st)
	st.c.failConnection(err)
}

func (l *netLoop) teardown(st *ngConn) {
	if st.fd < 0 {
		return
	}
	st.closed = true
	delete(l.conns, int32(st.fd))
	_ = syscall.EpollCtl(l.epfd, syscall.EPOLL_CTL_DEL, st.fd, nil)
	_ = syscall.Close(st.fd)
	st.fd = -1
	st.out, st.in = nil, nil
	st.c.loopWG.Done()
}

func (c *Conn) pendingCount() int {
	c.pendingMu.RLock()
	n := len(c.pending)
	c.pendingMu.RUnlock()
	return n
}
