package client

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// C++ getConsistentReadVersion waits for the GRV reply or a proxy change and
// nothing else (NativeAPI.actor.cpp:7252-7259): a slow reply is never re-sent
// and never marks the proxy failed. Its onProxiesChanged future is taken before
// the proxy list is read, so a change cannot fall between the two.

// Literal wire identifiers, so a wrong generated constant cannot confirm itself:
// GetReadVersionRequest and ErrorOr<GetReadVersionReply> (ComposedIdentifier<T, 2>),
// CommitProxyInterface.h:298 and :264.
const (
	grvWaitRequestFileID = 838566
	grvWaitReplyFileID   = 2<<24 | 15709388
)

// grvProxyChangeBound is well under the 5s DefaultRPCTimeout that a GRV wait
// ignoring the proxy change would sit out.
const grvProxyChangeBound = 3 * time.Second

// grvReplyGate is a dialer that counts outgoing GetReadVersionRequests, can park
// the write carrying the next one, and can withhold GetReadVersionReply frames.
type grvReplyGate struct {
	mu        sync.Mutex
	requests  int
	holdLeft  int
	heldTotal int
	held      []grvHeldReply
	parkNext  bool
	parkToken *transport.UID
	err       error

	heldCh     chan struct{}
	parkedCh   chan struct{}
	unpark     chan struct{}
	unparkOnce sync.Once
}

type grvHeldReply struct {
	conn  *grvGateConn
	token transport.UID
	body  []byte
}

func newGRVReplyGate() *grvReplyGate {
	return &grvReplyGate{
		heldCh:   make(chan struct{}, 64),
		parkedCh: make(chan struct{}, 1),
		unpark:   make(chan struct{}),
	}
}

func (g *grvReplyGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	c := &grvGateConn{Conn: raw, gate: g, pr: pr, pw: pw, handshake: transport.ConnectPacketSize}
	go c.readLoop()
	return c, nil
}

// arm resets the request count, withholds the next hold GRV replies and, when
// park is set, parks the write that carries the next GetReadVersionRequest.
func (g *grvReplyGate) arm(hold int, park bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests, g.holdLeft, g.parkNext = 0, hold, park
}

func (g *grvReplyGate) counts() (requests, held int, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests, g.heldTotal, g.err
}

func (g *grvReplyGate) withhold(c *grvGateConn, token transport.UID, body []byte) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.holdLeft == 0 || g.parkToken != nil && *g.parkToken != token {
		return false
	}
	g.holdLeft--
	g.heldTotal++
	g.held = append(g.held, grvHeldReply{conn: c, token: token, body: slices.Clone(body)})
	select {
	case g.heldCh <- struct{}{}:
	default:
	}
	return true
}

// noteRequests returns the channel to park on when this write carries the
// first GetReadVersionRequest after arm(park=true).
func (g *grvReplyGate) noteRequests(tokens []transport.UID) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests += len(tokens)
	if !g.parkNext {
		return nil
	}
	g.parkNext = false
	g.parkToken = &tokens[0]
	g.parkedCh <- struct{}{}
	return g.unpark
}

func (g *grvReplyGate) fail(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err == nil {
		g.err = err
	}
}

func (g *grvReplyGate) openPark() { g.unparkOnce.Do(func() { close(g.unpark) }) }

// takeHeld stops withholding and returns the replies withheld so far.
func (g *grvReplyGate) takeHeld() []grvHeldReply {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := g.held
	g.held, g.holdLeft = nil, 0
	return held
}

// release delivers every withheld reply in arrival order.
func (g *grvReplyGate) release() {
	for _, r := range g.takeHeld() {
		_ = r.conn.forward(r.token, r.body) // a closed connection drops it, like the network
	}
}

// discard loses every withheld reply.
func (g *grvReplyGate) discard() { g.takeHeld() }

type grvGateConn struct {
	net.Conn
	gate  *grvReplyGate
	pr    *io.PipeReader
	pw    *io.PipeWriter
	fwdMu sync.Mutex // orders readLoop forwarding against released replies

	outMu     sync.Mutex
	handshake int
	out       []byte
}

func (c *grvGateConn) Read(b []byte) (int, error) { return c.pr.Read(b) }

func (c *grvGateConn) Close() error {
	c.pr.Close() // unblocks a release racing teardown
	return c.Conn.Close()
}

func (c *grvGateConn) Write(p []byte) (int, error) {
	if tokens := c.requestTokens(p); len(tokens) > 0 {
		if park := c.gate.noteRequests(tokens); park != nil {
			<-park
		}
	}
	return c.Conn.Write(p)
}

// Decode reply tokens before parking the write, so reply order cannot decide
// which RPC is held when another request is sent under the new proxy set.
func (c *grvGateConn) requestTokens(p []byte) []transport.UID {
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.handshake > 0 {
		n := min(c.handshake, len(p))
		c.handshake -= n
		p = p[n:]
	}
	c.out = append(c.out, p...)
	var tokens []transport.UID
	for len(c.out) >= 4 {
		// [packetLen 4][checksum 8][token 16][body]; the body's file ID is at offset 4.
		size := 12 + int(binary.LittleEndian.Uint32(c.out))
		if size < 12+16 || size > 12+100<<20 {
			c.gate.fail(fmt.Errorf("invalid outgoing frame size %d", size))
			c.out = nil
			return tokens
		}
		if len(c.out) < size {
			break
		}
		if body := c.out[28:size]; len(body) >= 8 && binary.LittleEndian.Uint32(body[4:8]) == grvWaitRequestFileID {
			var req types.GetReadVersionRequest
			if err := req.UnmarshalFDB(body); err != nil {
				c.gate.fail(err)
			} else {
				tokens = append(tokens, transport.UID{
					First:  binary.LittleEndian.Uint64(req.Reply.Token[:8]),
					Second: binary.LittleEndian.Uint64(req.Reply.Token[8:]),
				})
			}
		}
		c.out = c.out[size:]
	}
	return tokens
}

func (c *grvGateConn) readLoop() {
	var hello [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(c.Conn, hello[:]); err != nil {
		c.pw.CloseWithError(err)
		return
	}
	c.fwdMu.Lock()
	_, err := c.pw.Write(hello[:])
	c.fwdMu.Unlock()
	if err != nil {
		return
	}
	for {
		token, body, err := transport.ReadFrame(c.Conn, false)
		if err != nil {
			c.pw.CloseWithError(err)
			return
		}
		if len(body) >= 8 && binary.LittleEndian.Uint32(body[4:8]) == grvWaitReplyFileID && c.gate.withhold(c, token, body) {
			continue
		}
		if c.forward(token, body) != nil {
			return
		}
	}
}

func (c *grvGateConn) forward(token transport.UID, body []byte) error {
	c.fwdMu.Lock()
	defer c.fwdMu.Unlock()
	return transport.WriteFrame(c.pw, token, body, false)
}

// newGRVReplyGateDB opens a database through a grvReplyGate and warms it with one
// GRV, returning the GRV proxy addresses. Cleanup unparks and delivers anything
// still withheld before Close, so no wait outlives the test.
func newGRVReplyGateDB(t *testing.T, ctx context.Context) (*Database, *grvReplyGate, []string) {
	t.Helper()
	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}
	g := newGRVReplyGate()
	db := newTestDatabase(t, ctx, sharedClusterFile, g.dial)
	t.Cleanup(func() { db.Close() })
	t.Cleanup(func() {
		g.openPark()
		g.release()
	})
	if _, _, _, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false); err != nil {
		t.Fatalf("warm GRV: %v", err)
	}
	proxies, _ := db.db.getGRVProxies()
	if len(proxies) == 0 {
		t.Fatal("no GRV proxies after a successful GRV")
	}
	addrs := make([]string, len(proxies))
	for i, p := range proxies {
		addrs[i] = p.Address
		if db.db.failMon.isFailed(p.Address) {
			t.Fatalf("%s failed before the test; the failure assertions would be vacuous", p.Address)
		}
	}
	return db, g, addrs
}

func startGatedGRV(db *Database, ctx context.Context) <-chan grvResult {
	done := make(chan grvResult, 1)
	go func() {
		v, locked, at, err := db.db.grvBatchers[grvBatcherDefault].getReadVersion(db.db, ctx, grvPriorityDefault, types.SpanContext{}, nil, false, false)
		done <- grvResult{version: v, locked: locked, instant: at, err: err}
	}()
	return done
}

func awaitGateSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

// Inject one proxy change while the old address stays alive. Holding installMu
// prevents a monitor publication from supplying a second, masking notification.
func publishChangedGRVProxies(t *testing.T, db *Database) {
	t.Helper()
	db.db.installMu.Lock()
	t.Cleanup(db.db.installMu.Unlock)
	cur := db.db.dbInfo.Load()
	next := *cur
	next.GRVProxies = append(slices.Clone(cur.GRVProxies), cur.GRVProxies[0])
	db.db.dbInfo.Store(&next)
	db.db.proxiesChangedMu.Lock()
	close(db.db.proxiesChanged)
	db.db.proxiesChanged = make(chan struct{})
	db.db.proxiesChangedMu.Unlock()
}

// assertGRVProxiesHealthy checks that nothing counted a connection failure or
// marked a GRV proxy failed since failuresBefore.
func assertGRVProxiesHealthy(t *testing.T, db *Database, addrs []string, failuresBefore int64) {
	t.Helper()
	if got := db.db.metrics.Snapshot().ClientConnectionFailures - failuresBefore; got != 0 {
		t.Errorf("GRV_PROXY_FAILED: %d connection failures recorded for a healthy proxy, want 0", got)
	}
	for _, addr := range addrs {
		if db.db.failMon.isFailed(addr) {
			t.Errorf("GRV_PROXY_FAILED: healthy proxy %s is marked failed", addr)
		}
	}
}

func TestGRVHeldReplyOutlivesRPCTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, g, addrs := newGRVReplyGateDB(t, ctx)
	failuresBefore := db.db.metrics.Snapshot().ClientConnectionFailures

	// Withhold every reply, so a re-sent request could not answer the caller either.
	g.arm(1<<30, false)
	done := startGatedGRV(db, ctx)
	awaitGateSignal(t, g.heldCh, "withholding the GRV reply")

	select {
	case r := <-done:
		t.Fatalf("GRV returned while its reply was withheld: version=%d err=%v", r.version, r.err)
	case <-time.After(DefaultRPCTimeout + 2*time.Second):
	}
	requests, held, err := g.counts()
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || held != 1 {
		t.Fatalf("GRV_RESENT: %d GetReadVersionRequests and %d replies past the RPC timeout, want 1 and 1", requests, held)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)

	g.release()
	select {
	case r := <-done:
		if r.err != nil || r.version <= 0 {
			t.Fatalf("GRV after releasing its reply: version=%d err=%v", r.version, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the released reply did not complete the GRV")
	}
	if requests, _, _ := g.counts(); requests != 1 {
		t.Fatalf("GRV_RESENT: %d GetReadVersionRequests in total, want 1", requests)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)
}

func TestGRVLostReplyWaitsForProxyChange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, g, addrs := newGRVReplyGateDB(t, ctx)
	failuresBefore := db.db.metrics.Snapshot().ClientConnectionFailures

	g.arm(1, false)
	done := startGatedGRV(db, ctx)
	awaitGateSignal(t, g.heldCh, "withholding the GRV reply")
	g.discard()

	select {
	case r := <-done:
		t.Fatalf("GRV returned although its only reply was lost: version=%d err=%v", r.version, r.err)
	case <-time.After(DefaultRPCTimeout + 2*time.Second):
	}
	requests, _, err := g.counts()
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("GRV_RESENT: %d GetReadVersionRequests past the RPC timeout without a proxy change, want 1", requests)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)

	changed := time.Now()
	publishChangedGRVProxies(t, db)
	select {
	case r := <-done:
		if r.err != nil || r.version <= 0 {
			t.Fatalf("GRV after the proxy change: version=%d err=%v", r.version, r.err)
		}
	case <-time.After(grvProxyChangeBound):
		t.Fatalf("GRV_CHANGE_IGNORED: GRV still waiting %v after the proxy change", time.Since(changed))
	}
	if requests, _, _ := g.counts(); requests != 2 {
		t.Fatalf("GetReadVersionRequests=%d, want the original plus one after the change", requests)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)
}

func TestGRVProxyChangeWakesReplyWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, g, addrs := newGRVReplyGateDB(t, ctx)
	failuresBefore := db.db.metrics.Snapshot().ClientConnectionFailures

	g.arm(1, false)
	done := startGatedGRV(db, ctx)
	awaitGateSignal(t, g.heldCh, "withholding the GRV reply")

	changed := time.Now()
	publishChangedGRVProxies(t, db)
	select {
	case r := <-done:
		if r.err != nil || r.version <= 0 {
			t.Fatalf("GRV after the proxy change: version=%d err=%v", r.version, r.err)
		}
	case <-time.After(grvProxyChangeBound):
		t.Fatalf("GRV_CHANGE_IGNORED: GRV still waiting %v after the proxy change", time.Since(changed))
	}
	requests, held, err := g.counts()
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || held != 1 {
		t.Fatalf("GetReadVersionRequests=%d withheld=%d, want the original plus one after the change, and 1", requests, held)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)
}

// The change lands while the GRV's first request is still being written: after
// the proxy list was read, before the reply wait began. A wait that samples the
// change signal only after reading the list misses it and waits on a lost reply.
func TestGRVProxyChangeDuringSendIsNotLost(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, g, addrs := newGRVReplyGateDB(t, ctx)
	failuresBefore := db.db.metrics.Snapshot().ClientConnectionFailures

	// The reply to the request written under the old list is lost.
	g.arm(1, true)
	done := startGatedGRV(db, ctx)
	awaitGateSignal(t, g.parkedCh, "parking the GetReadVersionRequest write")

	publishChangedGRVProxies(t, db)
	changed := time.Now()
	g.openPark()
	awaitGateSignal(t, g.heldCh, "withholding the reply to the request sent under the old proxy list")
	g.discard()

	select {
	case r := <-done:
		if r.err != nil || r.version <= 0 {
			t.Fatalf("GRV after the proxy change: version=%d err=%v", r.version, r.err)
		}
	case <-time.After(grvProxyChangeBound):
		t.Fatalf("GRV_CHANGE_LOST: GRV still waiting %v after a proxy change that landed during its send", time.Since(changed))
	}
	requests, held, err := g.counts()
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || held != 1 {
		t.Fatalf("GetReadVersionRequests=%d withheld=%d, want the parked one plus one under the new list, and 1", requests, held)
	}
	assertGRVProxiesHealthy(t, db, addrs, failuresBefore)
}
