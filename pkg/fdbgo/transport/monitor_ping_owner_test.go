package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RFC-269: connectionMonitor owns its PING reply, as C++ connectionMonitor holds
// `state PingRequest pingRequest` (FlowTransport.actor.cpp:665). Every exit
// releases the reply on the monitor's goroutine, before loopWG.Done and before any
// teardown the monitor causes. These tests swap the package putReplyChannel seam,
// so they do not call t.Parallel (see observePool).

// pingOwnerDummy is the pending request that makes the monitor ping.
var pingOwnerDummy = UID{First: 99}

// pingOwnerContext records the PING's reply channel from inside the wait that
// owns it: every wait select evaluates Done before any arm can fire. Recording
// needs pendingMu's read lock (TryRLock, since a release blocked in the seam holds
// pendingMu), so a test that takes pendingMu's write lock must first wait for
// recorded. With panicInWait it then panics once, from that wait.
type pingOwnerContext struct {
	context.Context
	conn        *Conn
	panicInWait bool
	ping        atomic.Pointer[chan Response]
	recorded    chan struct{} // closed once ping is stored
	panicked    atomic.Bool
}

func (p *pingOwnerContext) Done() <-chan struct{} {
	if p.ping.Load() == nil && p.conn.pendingMu.TryRLock() {
		for token, entry := range p.conn.pending {
			if token != pingOwnerDummy {
				ch := entry.ch
				p.ping.Store(&ch)
				close(p.recorded)
			}
		}
		p.conn.pendingMu.RUnlock()
	}
	if p.panicInWait && p.ping.Load() != nil && p.panicked.CompareAndSwap(false, true) {
		panic("injected panic in the monitor's PING wait")
	}
	return p.Context.Done()
}

type pingOwnerFixture struct {
	c           *Conn
	ctx         *pingOwnerContext
	cancel      context.CancelFunc
	monitorDone chan struct{}
	// released receives IsClosed() the first time the PING's reply channel reaches
	// putReplyChannel: Release after a received or taken reply, or a cancel while
	// the token is still registered. It is sent on the releasing goroutine.
	released chan bool
	gate     chan struct{} // non-nil: that release then blocks until openGate
	gateOnce sync.Once
}

func (f *pingOwnerFixture) openGate() {
	if f.gate != nil {
		f.gateOnce.Do(func() { close(f.gate) })
	}
}

// newPingOwnerFixture runs only connectionMonitor, under loopWG. The test is its
// writer (it takes the PING from writeCh) and its reader (it publishes replies).
// No reader means bytesReceived never moves. gated makes the PING reply's release
// block until openGate.
func newPingOwnerFixture(t *testing.T, timeout time.Duration, panicInWait, gated bool) *pingOwnerFixture {
	t.Helper()
	base, cancel := context.WithCancel(context.Background())
	client, peer := net.Pipe()
	c := &Conn{
		conn:                client,
		cancel:              cancel,
		writeCh:             make(chan writeReq, 1),
		pending:             map[UID]pendingEntry{pingOwnerDummy: {ch: make(chan Response, 1)}},
		monitorLoopInterval: time.Millisecond,
		monitorTimeout:      timeout,
	}
	f := &pingOwnerFixture{c: c, cancel: cancel, monitorDone: make(chan struct{}), released: make(chan bool, 1)}
	if gated {
		f.gate = make(chan struct{})
	}
	f.ctx = &pingOwnerContext{Context: base, conn: c, panicInWait: panicInWait, recorded: make(chan struct{})}
	c.ctx = f.ctx

	// Cleanups run last-in first-out: open the gate, join the monitor, and only
	// then restore the seam the monitor calls.
	orig := putReplyChannel
	var fired atomic.Bool
	putReplyChannel = func(ch chan Response) {
		if ping := f.ctx.ping.Load(); ping != nil && ch == *ping && fired.CompareAndSwap(false, true) {
			f.released <- c.IsClosed()
			if f.gate != nil {
				<-f.gate
			}
		}
		orig(ch)
	}
	t.Cleanup(func() { putReplyChannel = orig })

	c.loopWG.Add(1)
	go func() {
		defer close(f.monitorDone)
		c.connectionMonitor()
	}()
	t.Cleanup(func() {
		f.openGate()
		cancel()
		c.loopWG.Wait()
		_ = client.Close()
		_ = peer.Close()
	})
	return f
}

// takePing plays the writer: it returns the monitor's PING reply token.
func (f *pingOwnerFixture) takePing(t *testing.T) UID {
	t.Helper()
	request := admissionAwait(t, f.c.writeCh, "monitor PING")
	f.c.queued.Add(-1)
	if request.token != WellKnownToken(WLTokenPingPacket) {
		t.Fatalf("monitor queued token %+v, want PingRequest", request.token)
	}
	token, ok := extractPingReplyToken(request.body)
	if !ok {
		t.Fatal("monitor PING carries no reply token")
	}
	return token
}

// gateWait is how long a gated release stays blocked while the test watches for
// the join it must hold up. It bounds only how quickly the reverted code is
// caught: on the fixed code the join cannot happen while the release is blocked.
const gateWait = 100 * time.Millisecond

func TestMonitorPingOwner_CloseWaitsForReplyRelease(t *testing.T) {
	f := newPingOwnerFixture(t, time.Minute, false, true)
	token := f.takePing(t)
	if !f.c.HasPendingReply(token) {
		t.Fatal("positive control: the PING's reply token is not registered")
	}

	// Publish the reply as readLoop does: send, then delete, under pendingMu. Wait
	// for the monitor's wait to record the channel first: a write lock held or
	// queued then would fail its TryRLock, and the reply ends that wait.
	admissionAwait(t, f.ctx.recorded, "the monitor's PING wait")
	f.c.pendingMu.Lock()
	f.c.pending[token].ch <- Response{}
	delete(f.c.pending, token)
	f.c.pendingMu.Unlock()
	admissionAwait(t, f.released, "release of the received PING reply")

	closed := make(chan struct{})
	go func() {
		_ = f.c.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while the PING reply's owner was still releasing it")
	case <-time.After(gateWait):
	}
	f.openGate()
	admissionAwait(t, closed, "Close after the PING reply release")
}

func TestMonitorPingOwner_CloseWaitsForOutstandingPing(t *testing.T) {
	f := newPingOwnerFixture(t, time.Minute, false, true)
	token := f.takePing(t)
	admissionAwait(t, f.ctx.recorded, "the monitor's PING wait")

	// No reply: Close's own teardown ends the wait. Whichever arm wins, the owner
	// releases the channel once: Release after the teardown response, or
	// TakeReadyOrCancel taking that response or cancelling before it is published.
	closed := make(chan struct{})
	go func() {
		_ = f.c.Close()
		close(closed)
	}()
	admissionAwait(t, f.released, "release of the outstanding PING reply")
	select {
	case <-closed:
		t.Fatal("Close returned while the outstanding PING's owner was still releasing it")
	case <-time.After(gateWait):
	}
	f.openGate()
	admissionAwait(t, closed, "Close after the PING reply release")
	if f.c.HasPendingReply(token) {
		t.Fatal("Close left the PING's reply registered")
	}
}

func TestMonitorPingOwner_CancellationReleasesOnMonitor(t *testing.T) {
	f := newPingOwnerFixture(t, time.Minute, false, true)
	token := f.takePing(t)
	if !f.c.HasPendingReply(token) {
		t.Fatal("positive control: the PING's reply token is not registered")
	}

	// Cancel without failConnection, so the token stays registered and the owner
	// must cancel it (pooling the channel, which reaches the seam).
	f.cancel()
	admissionAwait(t, f.released, "release of the cancelled PING reply")
	// The blocked cancel holds pendingMu: touch nothing that takes it until open.
	select {
	case <-f.monitorDone:
		t.Fatal("connectionMonitor returned while its PING reply was still being released")
	case <-time.After(gateWait):
	}
	f.openGate()
	admissionAwait(t, f.monitorDone, "connectionMonitor exit after the release")
	if f.c.HasPendingReply(token) {
		t.Fatal("the cancelled PING left its reply registered")
	}
}

// awaitReleaseBeforeTeardown asserts that the PING reply was released while the
// connection was still open, and that the monitor then closed it.
func awaitReleaseBeforeTeardown(t *testing.T, f *pingOwnerFixture, token UID) {
	t.Helper()
	if closedAtRelease := admissionAwait(t, f.released, "release of the PING reply"); closedAtRelease {
		t.Fatal("the PING reply was released after the connection was torn down")
	}
	admissionAwait(t, f.c.Done(), "monitor teardown")
	admissionAwait(t, f.monitorDone, "connectionMonitor exit")
	if f.c.HasPendingReply(token) {
		t.Fatal("the PING left its reply registered")
	}
}

func TestMonitorPingOwner_FrozenPingReleasesBeforeTeardown(t *testing.T) {
	f := newPingOwnerFixture(t, 20*time.Millisecond, false, false)
	awaitReleaseBeforeTeardown(t, f, f.takePing(t))
}

func TestMonitorPingOwner_PanicReleasesBeforeTeardown(t *testing.T) {
	orig := seriousLog
	var logged atomic.Bool
	seriousLog = func(string, ...any) { logged.Store(true) }
	t.Cleanup(func() { seriousLog = orig })

	f := newPingOwnerFixture(t, time.Minute, true, false)
	awaitReleaseBeforeTeardown(t, f, f.takePing(t))
	if !f.ctx.panicked.Load() || !logged.Load() {
		t.Fatal("positive control: the monitor's PING wait did not panic into recoverLoop")
	}
}
