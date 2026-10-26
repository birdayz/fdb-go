package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RFC-269: connectionMonitor owns its PING reply. These tests swap putReplyChannel,
// so they do not call t.Parallel (see observePool).

// pingOwnerDummy is the pending request that makes the monitor ping.
var pingOwnerDummy = UID{First: 99}

// pingOwnerContext records the PING's reply channel in the monitor's wait, whose
// select evaluates Done before any arm can fire.
type pingOwnerContext struct {
	context.Context
	conn        *Conn
	panicInWait bool
	hold        chan struct{} // non-nil: the recording Done blocks until closed
	ping        atomic.Pointer[chan Response]
	recorded    chan struct{} // closed once ping is stored
	panicked    atomic.Bool
}

func (p *pingOwnerContext) Done() <-chan struct{} {
	// TryLock per shard: a release blocked in the seam may hold its shard lock.
	if p.ping.Load() == nil {
		recordedNow := false
		for i := range p.conn.pendingShards {
			s := &p.conn.pendingShards[i]
			if !s.mu.TryLock() {
				continue
			}
			for token, entry := range s.m {
				if token != pingOwnerDummy {
					ch := entry.ch
					p.ping.Store(&ch)
					close(p.recorded)
					recordedNow = true
				}
			}
			s.mu.Unlock()
		}
		if recordedNow && p.hold != nil {
			<-p.hold
		}
	}
	if p.panicInWait && p.ping.Load() != nil && p.panicked.CompareAndSwap(false, true) {
		panic("injected panic in the monitor's PING wait")
	}
	return p.Context.Done()
}

type pingOwnerOptions struct {
	timeout     time.Duration
	panicInWait bool
	gated       bool // the PING reply's release blocks until openGate
	holdWait    bool // the monitor's wait blocks after recording until releaseWait
}

type pingOwnerFixture struct {
	c           *Conn
	ctx         *pingOwnerContext
	cancel      context.CancelFunc
	monitorDone chan struct{}
	released    chan bool // IsClosed() at the PING reply's first putReplyChannel
	gate        chan struct{}
	gateOnce    sync.Once
	holdOnce    sync.Once
}

func (f *pingOwnerFixture) openGate() {
	if f.gate != nil {
		f.gateOnce.Do(func() { close(f.gate) })
	}
}

func (f *pingOwnerFixture) releaseWait() {
	if f.ctx.hold != nil {
		f.holdOnce.Do(func() { close(f.ctx.hold) })
	}
}

// newPingOwnerFixture runs only connectionMonitor, under loopWG; the test plays
// writer and reader, so bytesReceived never moves.
func newPingOwnerFixture(t *testing.T, opts pingOwnerOptions) *pingOwnerFixture {
	t.Helper()
	base, cancel := context.WithCancel(context.Background())
	client, peer := net.Pipe()
	c := &Conn{
		conn:                client,
		cancel:              cancel,
		writeCh:             make(chan writeReq, 1),
		monitorLoopInterval: time.Millisecond,
		monitorTimeout:      opts.timeout,
	}
	c.putPending(pingOwnerDummy, pendingEntry{ch: make(chan Response, 1)})
	f := &pingOwnerFixture{c: c, cancel: cancel, monitorDone: make(chan struct{}), released: make(chan bool, 1)}
	if opts.gated {
		f.gate = make(chan struct{})
	}
	f.ctx = &pingOwnerContext{Context: base, conn: c, panicInWait: opts.panicInWait, recorded: make(chan struct{})}
	if opts.holdWait {
		f.ctx.hold = make(chan struct{})
	}
	c.ctx = f.ctx

	// Cleanups run last-in first-out: the seam is restored after the monitor joins.
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
		f.releaseWait()
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

// gateWait only bounds how fast the reverted code is caught; the fixed code cannot
// join while the gated release is blocked.
const gateWait = 100 * time.Millisecond

// requireCloseWaitsForGate runs Close and requires that it returns only after the
// gated release does.
func requireCloseWaitsForGate(t *testing.T, f *pingOwnerFixture, token UID) {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		_ = f.c.Close()
		close(closed)
	}()
	if f.ctx.hold != nil {
		// Close's teardown publishes and deregisters before the wait resumes.
		deadline := time.Now().Add(3 * time.Second)
		for f.c.HasPendingReply(token) {
			if time.Now().After(deadline) {
				t.Fatal("Close did not deregister the outstanding PING")
			}
			time.Sleep(time.Millisecond)
		}
		f.releaseWait()
	}
	admissionAwait(t, f.released, "release of the PING reply")
	select {
	case <-closed:
		t.Fatal("Close returned while the PING reply's owner was still releasing it")
	case <-time.After(gateWait):
	}
	f.openGate()
	admissionAwait(t, closed, "Close after the PING reply release")
	if f.c.HasPendingReply(token) {
		t.Fatal("Close left the PING's reply registered")
	}
}

func TestMonitorPingOwner_CloseWaitsForReplyRelease(t *testing.T) {
	f := newPingOwnerFixture(t, pingOwnerOptions{timeout: time.Minute, gated: true})
	token := f.takePing(t)
	if !f.c.HasPendingReply(token) {
		t.Fatal("positive control: the PING's reply token is not registered")
	}
	// A shard lock held before recording would skip that shard's TryLock.
	admissionAwait(t, f.ctx.recorded, "the monitor's PING wait")
	// Publish the reply as readLoop does: send, then delete, under the shard lock.
	f.c.deliverPending(token, Response{})
	requireCloseWaitsForGate(t, f, token)
}

// The wait resumes only after Close's teardown published, so either arm releases
// via Release without taking the shard lock, and Close can only be waiting on the join.
func TestMonitorPingOwner_CloseWaitsForOutstandingPing(t *testing.T) {
	f := newPingOwnerFixture(t, pingOwnerOptions{timeout: time.Minute, gated: true, holdWait: true})
	token := f.takePing(t)
	admissionAwait(t, f.ctx.recorded, "the monitor's PING wait")
	requireCloseWaitsForGate(t, f, token)
}

func TestMonitorPingOwner_CancellationReleasesOnMonitor(t *testing.T) {
	f := newPingOwnerFixture(t, pingOwnerOptions{timeout: time.Minute, gated: true})
	token := f.takePing(t)
	if !f.c.HasPendingReply(token) {
		t.Fatal("positive control: the PING's reply token is not registered")
	}
	// No failConnection: the token stays registered, so the owner's cancel pools it.
	f.cancel()
	admissionAwait(t, f.released, "release of the cancelled PING reply")
	// The blocked cancel holds its shard lock: touch nothing that takes it until open.
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

// awaitReleaseBeforeTeardown requires the release while the connection was open,
// then the monitor's teardown.
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
	f := newPingOwnerFixture(t, pingOwnerOptions{timeout: 20 * time.Millisecond})
	awaitReleaseBeforeTeardown(t, f, f.takePing(t))
}

func TestMonitorPingOwner_PanicReleasesBeforeTeardown(t *testing.T) {
	orig := seriousLog
	var logged atomic.Bool
	seriousLog = func(string, ...any) { logged.Store(true) }
	t.Cleanup(func() { seriousLog = orig })

	f := newPingOwnerFixture(t, pingOwnerOptions{timeout: time.Minute, panicInWait: true})
	awaitReleaseBeforeTeardown(t, f, f.takePing(t))
	if !f.ctx.panicked.Load() || !logged.Load() {
		t.Fatal("positive control: the monitor's PING wait did not panic into recoverLoop")
	}
}

// releasePing pools the channel whether or not the reply was published before the
// release: a plain Cancel leaves a published-but-unreceived channel to GC.
func TestReleasePingPoolsPublishedReply(t *testing.T) {
	for _, published := range []bool{false, true} {
		name := "registered"
		if published {
			name = "published"
		}
		t.Run(name, func(t *testing.T) {
			c := newTestConn()
			token, _, h := c.PrepareReply()
			ch := h.ch
			if published { // failAllPending: send, then delete, under the shard lock
				c.deliverPending(token, Response{Err: errConnClosed})
			}
			pooled := observePool(t)
			releasePing(h, false)
			if len(*pooled) != 1 || (*pooled)[0] != ch {
				t.Fatalf("releasePing pooled %d channels, want the PING's once", len(*pooled))
			}
			if n := c.pendingCount(); n != 0 {
				t.Fatalf("releasePing left %d registrations", n)
			}
		})
	}
}
