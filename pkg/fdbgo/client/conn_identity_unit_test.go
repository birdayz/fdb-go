package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// identityPeer is the server end of one dialed connection: it answers PINGs, so the
// connection monitor keeps it, and reports every other frame.
type identityPeer struct {
	client, server net.Conn
	frames         chan identityFrame
}

type identityFrame struct {
	token transport.UID
	body  []byte
}

func newIdentityPeer(t *testing.T) *identityPeer {
	t.Helper()
	client, server := net.Pipe()
	p := &identityPeer{client: client, server: server, frames: make(chan identityFrame, 16)}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		if _, err := transport.ReadConnectPacket(server); err != nil {
			return
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		if _, err := server.Write(packet.Marshal()); err != nil {
			return
		}
		ping := transport.WellKnownToken(transport.WLTokenPingPacket)
		for {
			token, body, err := transport.ReadFrame(server, false)
			if err != nil {
				return
			}
			if token == ping {
				if reply, ok := pingReplyToken(body); ok {
					_ = transport.WriteFrame(server, reply, (&types.VoidReply{}).MarshalFDB(), false)
				}
				continue
			}
			select {
			case p.frames <- identityFrame{token: token, body: body}:
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		client.Close()
		server.Close()
		<-done
	})
	return p
}

// pingReplyToken reads the reply token of a PingRequest: field 0 is a nested
// ReplyPromise whose field 0 is the inline UID.
func pingReplyToken(body []byte) (transport.UID, bool) {
	r, err := wire.NewReader(body)
	if err != nil {
		return transport.UID{}, false
	}
	promise, err := r.ReadNestedReader(0)
	if err != nil || !promise.FieldPresent(0) {
		return transport.UID{}, false
	}
	off, obj := promise.FieldOffset(0), promise.ObjectBytes()
	if off+16 > len(obj) {
		return transport.UID{}, false
	}
	return transport.UID{First: binary.LittleEndian.Uint64(obj[off:]), Second: binary.LittleEndian.Uint64(obj[off+8:])}, true
}

// recv returns the next frame; it may run off the test goroutine.
func (p *identityPeer) recv(ctx context.Context) (identityFrame, bool) {
	select {
	case f := <-p.frames:
		return f, true
	case <-ctx.Done():
		return identityFrame{}, false
	}
}

// The fake peer answers the connection monitor's PING, so a test's connection
// cannot be torn down by the monitor's timer.
func TestIdentityPeerAnswersPing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, _ := identityDatabase(ctx, newIdentityPeer(t))
	defer closeIdentityDatabase(db)
	c, err := db.getOrDial(ctx, armAddr)
	if err != nil {
		t.Fatal(err)
	}
	ping := transport.WellKnownToken(transport.WLTokenPingPacket)
	token, replies, handle := c.PrepareReplyTo(ping)
	defer handle.Release()
	if err := c.SendFrame(ping, transport.BuildPingRequest(token)); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-replies:
		if resp.Err != nil {
			t.Fatalf("ping reply: %v", resp.Err)
		}
	case <-ctx.Done():
		t.Fatal("the peer did not answer the PING")
	}
}

// identityDatabase dials the peers in order, one per dial, and reports each dial.
func identityDatabase(ctx context.Context, peers ...*identityPeer) (*database, <-chan int) {
	dials := make(chan int, len(peers)+4)
	var mu sync.Mutex
	n := 0
	db := &database{
		ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{},
		failMon: newFailureMonitor(), proxiesChanged: make(chan struct{}),
		dialFn: func(context.Context, string, string) (net.Conn, error) {
			mu.Lock()
			i := n
			n++
			mu.Unlock()
			dials <- i
			if i >= len(peers) {
				return nil, errors.New("test: no peer left to dial")
			}
			return peers[i].client, nil
		},
	}
	return db, dials
}

func closeIdentityDatabase(db *database) {
	db.connMu.Lock()
	defer db.connMu.Unlock()
	for _, conn := range db.connPool {
		conn.Close()
	}
}

func pooledConn(db *database, addr string) *transport.Conn {
	db.connMu.Lock()
	defer db.connMu.Unlock()
	return db.connPool[addr]
}

func replyToken(tok [16]byte) transport.UID {
	return transport.UID{First: binary.LittleEndian.Uint64(tok[:8]), Second: binary.LittleEndian.Uint64(tok[8:])}
}

// A location query's late report about dead connection A must not close B, on
// which a commit waits: the commit gets B's not_committed (1020), not 1021.
func TestLateConnErrorKeepsReplacementConn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const addr = "proxy:4500"
	proxyToken := transport.UID{First: 0x1111, Second: 0x2222}
	peerA, peerB, peerC := newIdentityPeer(t), newIdentityPeer(t), newIdentityPeer(t)
	db, dials := identityDatabase(ctx, peerA, peerB, peerC)
	defer closeIdentityDatabase(db)
	db.locCache.maxSize = 16
	db.dbInfo.Store(&DBInfo{CommitProxies: []ProxyInfo{{Address: addr, Token: proxyToken}}})

	commitCtx, cancelCommit := context.WithCancel(ctx)
	defer cancelCommit()
	commitErr := make(chan error, 1)
	var commitReply transport.UID
	var connA *transport.Conn
	captured := make(chan struct{}) // the hook has taken the commit frame from B
	var once sync.Once
	hook := func() {
		once.Do(func() {
			defer close(captured)
			connA = pooledConn(db, addr)
			if _, ok := peerA.recv(ctx); !ok {
				t.Error("no location request on A")
				return
			}
			peerA.server.Close() // A dies with the location reply outstanding
			select {
			case <-connA.Done():
			case <-ctx.Done():
				t.Error("connection A did not fail")
				return
			}
			go func() {
				input := &commitInput{db: db, readVersion: 1, tenantID: NoTenantID}
				_, err := input.commitNative(commitCtx)
				commitErr <- err
			}()
			f, ok := peerB.recv(ctx)
			if !ok {
				t.Error("no commit on B")
				return
			}
			var req types.CommitTransactionRequest
			if err := req.UnmarshalFDB(f.body); err != nil {
				t.Errorf("decode commit request: %v", err)
				return
			}
			commitReply = replyToken(req.Reply.Token)
		})
	}
	db.afterLocationSend.Store(&hook)

	locCtx, cancelLoc := context.WithCancel(ctx)
	locDone := make(chan struct{})
	go func() {
		defer close(locDone)
		_, _ = db.locCache.queryLocations(db, locCtx, NoTenantID, func(tok transport.UID) []byte {
			return buildGetKeyServerLocationsRequest([]byte("k"), false, NoTenantID, types.SpanContext{}, tok)
		}, nil)
	}()
	defer func() { cancelLoc(); <-locDone }()

	// The location query retries after handling A's failure: on B when B survived,
	// on a third connection when the handler closed B.
	select {
	case <-captured:
	case <-ctx.Done():
		t.Fatal("the location query did not reach its seam")
	}
	for _, want := range []int{0, 1} {
		select {
		case got := <-dials:
			if got != want {
				t.Fatalf("dial %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for dial %d", want)
		}
	}
	select {
	case <-peerB.frames:
	case n := <-dials:
		t.Logf("the location query redialed (dial %d): the late handler closed B", n)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the location retry")
	}
	if t.Failed() {
		return
	}

	_ = transport.WriteFrame(peerB.server, commitReply, (&types.ErrorOrError{ErrorCode: ErrNotCommitted}).MarshalFDB(), false)
	var err error
	select {
	case err = <-commitErr:
	case <-ctx.Done():
		t.Fatal("commit did not finish")
	}
	var fdbErr *wire.FDBError
	if !errors.As(err, &fdbErr) || fdbErr.Code != ErrNotCommitted {
		t.Fatalf("commit on the replacement connection = %v, want the proxy's not_committed (1020)", err)
	}
	connB := pooledConn(db, addr)
	if connB == nil || connB == connA || connB.IsClosed() {
		t.Fatalf("pool for %s = %p (closed=%v), want the open replacement", addr, connB, connB != nil && connB.IsClosed())
	}
	if db.failMon.isFailed(addr) {
		t.Fatal("a superseded failure report marked the reachable address failed")
	}
	if got := db.metrics.Snapshot().ClientConnectionFailures; got != 1 {
		t.Fatalf("ClientConnectionFailures = %d, want 1 (the failure of A)", got)
	}
}

// warnCounter counts the records a database logs.
type warnCounter struct {
	mu sync.Mutex
	n  int
}

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) Handle(context.Context, slog.Record) error {
	w.mu.Lock()
	w.n++
	w.mu.Unlock()
	return nil
}
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }
func (w *warnCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// handleConnError evicts the pooled connection only when it failed or is closed; a
// report the pool has outlived changes only the failure counter.
func TestHandleConnErrorIdentity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel) // parallel subtests outlive this function
	const addr = "peer:4500"
	setup := func(t *testing.T, peers int) (*database, *warnCounter) {
		t.Helper()
		ps := make([]*identityPeer, peers)
		for i := range ps {
			ps[i] = newIdentityPeer(t)
		}
		db, _ := identityDatabase(ctx, ps...)
		t.Cleanup(func() { closeIdentityDatabase(db) })
		w := &warnCounter{}
		db.logger = slog.New(w)
		return db, w
	}
	dial := func(t *testing.T, db *database) *transport.Conn {
		t.Helper()
		c, err := db.getOrDial(ctx, addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return c
	}
	check := func(t *testing.T, db *database, w *warnCounter, wantPooled *transport.Conn, wantFailed bool, wantCount int64, wantWarns int) {
		t.Helper()
		if got := pooledConn(db, addr); got != wantPooled {
			t.Fatalf("pooled = %p, want %p", got, wantPooled)
		}
		if got := db.failMon.isFailed(addr); got != wantFailed {
			t.Fatalf("isFailed = %v, want %v", got, wantFailed)
		}
		if got := db.metrics.Snapshot().ClientConnectionFailures; got != wantCount {
			t.Fatalf("ClientConnectionFailures = %d, want %d", got, wantCount)
		}
		if w.count() != wantWarns {
			t.Fatalf("warnings = %d, want %d", w.count(), wantWarns)
		}
	}

	t.Run("failed connection is pooled", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 1)
		a := dial(t, db)
		db.handleConnError(addr, a)
		requireIdentityConnClosed(t, ctx, a)
		check(t, db, w, nil, true, 1, 1)
	})
	t.Run("pool replaced it with an open connection", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 2)
		a := dial(t, db)
		a.Close()
		b := dial(t, db)
		db.handleConnError(addr, a)
		if b.IsClosed() {
			t.Fatal("a report about A closed its replacement B")
		}
		check(t, db, w, b, false, 1, 0)
	})
	t.Run("dial failure after a later dial succeeded", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 1)
		b := dial(t, db)
		db.handleDialError(ctx, addr)
		if b.IsClosed() {
			t.Fatal("a dial failure closed the connection a later dial pooled")
		}
		check(t, db, w, b, false, 1, 0)
	})
	t.Run("pooled connection is closed too", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 1)
		b := dial(t, db)
		b.Close()
		db.handleConnError(addr, nil)
		check(t, db, w, nil, true, 1, 1)
	})
	t.Run("nothing pooled", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 0)
		db.handleConnError(addr, nil)
		check(t, db, w, nil, true, 1, 1)
	})
	t.Run("failure then redial", func(t *testing.T) {
		t.Parallel()
		db, w := setup(t, 2)
		a := dial(t, db)
		db.handleConnError(addr, a)
		b := dial(t, db)
		if db.failMon.isFailed(addr) || b.IsClosed() {
			t.Fatal("a successful redial must leave the address alive")
		}
		db.handleConnError(addr, a) // a second report about A
		check(t, db, w, b, false, 2, 1)
	})
}

// A redial attempted just before the failure mark must not be overtaken by it; the
// seam's redial can complete first only if the mark runs outside connMu.
func TestHandleConnErrorMarkCannotOvertakeRedial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const addr = "peer:4500"
	db, _ := identityDatabase(ctx, newIdentityPeer(t), newIdentityPeer(t))
	defer closeIdentityDatabase(db)
	a, err := db.getOrDial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	redialed := make(chan error, 1)
	redial := func() {
		_, err := db.getOrDial(ctx, addr)
		redialed <- err
	}
	hook := func() {
		if db.connMu.TryLock() {
			db.connMu.Unlock()
			redial()
			return
		}
		go redial()
	}
	db.beforeConnFailureMark.Store(&hook)
	db.handleConnError(addr, a)
	db.beforeConnFailureMark.Store(nil)
	if err := <-redialed; err != nil {
		t.Fatalf("redial: %v", err)
	}
	b := pooledConn(db, addr)
	if b == nil || b == a || b.IsClosed() {
		t.Fatalf("pooled = %p, want the redialed connection", b)
	}
	if db.failMon.isFailed(addr) {
		t.Fatal("the failure mark overtook the redial: an open pooled connection's address is marked failed")
	}
}

// Fallback accounting must cover dial/send failures as well as successful replies.
func TestFallbackSendFailureBudget(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"dial", "send"} {
		for _, cancelled := range []bool{false, true} {
			name := stage
			if cancelled {
				name += "-cancelled"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				opCtx, stop := context.WithCancel(ctx)
				defer stop()
				var peers []*identityPeer
				var writing *writeSignalConn
				if stage == "send" {
					peer, signal := newStallPeer(t)
					peers, writing = []*identityPeer{peer}, signal
				}
				db, tx := armDatabase(ctx, peers...)
				defer closeIdentityDatabase(db)
				if stage == "send" {
					if _, err := db.getOrDial(ctx, armAddr); err != nil {
						t.Fatal(err)
					}
				} else if cancelled {
					stop()
				}
				done := make(chan error, 1)
				go func() {
					_, err := tx.sendGetValueToServer(opCtx, []byte("k"), ServerInfo{Address: armAddr, Token: armToken}, 1, false, NoTenantID, readTagSample{})
					done <- err
				}()
				if writing != nil {
					select {
					case <-writing.writing:
					case <-ctx.Done():
						t.Fatal("fallback did not enter the frame write")
					}
					if cancelled {
						stop()
					} else {
						peers[0].server.Close()
					}
				}
				select {
				case err := <-done:
					if err == nil || (cancelled && !errors.Is(err, context.Canceled)) {
						t.Fatalf("fallback failure = %v, cancelled=%v", err, cancelled)
					}
				case <-ctx.Done():
					t.Fatal("fallback did not return its failure")
				}
				accrued := 1
				if cancelled {
					accrued = 0
				}
				checkSecondModel(t, db.queueModel, 0, accrued, false)
				db.queueModel.mu.Lock()
				outstanding := db.queueModel.getOrCreate(armAddr).smoothOutstanding.total
				db.queueModel.mu.Unlock()
				if outstanding != 0 {
					t.Fatalf("fallback failure leaked outstanding delta %v", outstanding)
				}
			})
		}
	}
}

// connArm runs one RPC through its production path against peer:4500. An eager
// arm's send returns once its frame is queued, so it never fails on a blocked write.
type connArm struct {
	name  string
	eager bool
	run   func(context.Context, *database, *Transaction)
}

const armAddr = "peer:4500"

var armToken = transport.UID{First: 0x77, Second: 0x88}

// connArms lists every path that reports a connection failure for an RPC it sent.
func connArms() []connArm {
	servers := []ServerInfo{{Address: armAddr, Token: armToken}}
	key, end := []byte("k"), []byte("l")
	arms := []connArm{
		{"commit", true, func(ctx context.Context, db *database, _ *Transaction) {
			input := &commitInput{db: db, readVersion: 1, tenantID: NoTenantID}
			_, _ = input.commitNative(ctx)
		}},
		{"grv", false, func(ctx context.Context, db *database, _ *Transaction) {
			_, _, _, _, _, _, _, _ = (&grvBatcher{}).sendGRVRequest(db, ctx, 0, 1, types.SpanContext{}, nil)
		}},
		{"location", false, func(ctx context.Context, db *database, _ *Transaction) {
			_, _ = db.locCache.queryLocations(db, ctx, NoTenantID, func(tok transport.UID) []byte {
				return buildGetKeyServerLocationsRequest(key, false, NoTenantID, types.SpanContext{}, tok)
			}, nil)
		}},
		{"get value from one server", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendGetValueToServer(ctx, key, servers[0], 1, false, NoTenantID, readTagSample{})
		}},
		{"get value", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendGetValue(ctx, key, servers, readTagSample{})
		}},
		{"get key", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _, _, _ = tx.sendGetKey(ctx, 1, key, false, 1, servers, readTagSample{}, nil)
		}},
		{"get range", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _, _ = sendRangeRPC(tx, ctx, KeySelector{Key: key}, KeySelector{Key: end}, newRangeLimits(10, 0), 1, false, servers, readTagSample{}, plainRangeRPCOps)
		}},
		{"watch", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_ = tx.sendWatch(ctx, key, nil, 1, types.SpanContext{}, servers, readTagSample{})
		}},
		{"pipelined get", true, func(ctx context.Context, _ *database, tx *Transaction) {
			if _, p, err := tx.GetPipelined(ctx, key); err == nil && p != nil {
				_, _ = p.Resolve()
			}
		}},
	}
	for _, arm := range metricsArms() {
		arms = append(arms, arm.connArm())
	}
	return arms
}

// armDatabase is a database whose proxies and storage server are armAddr, with
// the shard of every user key cached there.
func armDatabase(ctx context.Context, peers ...*identityPeer) (*database, *Transaction) {
	db, _ := identityDatabase(ctx, peers...)
	db.queueModel = newQueueModel()
	db.locCache.maxSize = 16
	db.locCache.insertRanges([]locationEntry{{tenantId: NoTenantID, begin: []byte{}, end: []byte{0xff}, servers: []ServerInfo{{Address: armAddr, Token: armToken}}}})
	proxies := []ProxyInfo{{Address: armAddr, Token: armToken}}
	db.dbInfo.Store(&DBInfo{GRVProxies: proxies, CommitProxies: proxies})
	// A known version floor lets the pipelined read use version 1 without a GRV.
	db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.grvCache.epochNow(), version: 1})
	tx := (&Database{db: db}).CreateTransaction()
	tx.SetReadVersion(1)
	return db, tx
}

// replaceAndKill gives the pool a replacement for a, then kills a's peer, and
// waits until the arm has handled the failure: it returned or sent again.
func replaceAndKill(t *testing.T, ctx context.Context, db *database, a *transport.Conn, kill func(), done <-chan struct{}, next ...*identityPeer) *transport.Conn {
	t.Helper()
	db.connMu.Lock()
	if db.connPool[armAddr] == a {
		delete(db.connPool, armAddr)
	}
	// A send still blocked in A's first write keeps A's dial joinable; drop it so
	// the next dial is a new connection.
	if call := db.dialing[armAddr]; call != nil && call.conn == a {
		delete(db.dialing, armAddr)
	}
	db.connMu.Unlock()
	b, err := db.getOrDial(ctx, armAddr)
	if err != nil || b == a {
		t.Fatalf("replacement dial: %v", err)
	}
	kill()
	select {
	case <-done:
	case <-next[0].frames:
	case <-next[1].frames:
	case <-ctx.Done():
		t.Fatal("the arm neither returned nor sent again")
	}
	return b
}

// requireReplacementKept checks that the arm reported A exactly once and that
// the report left the replacement B alone.
func requireReplacementKept(t *testing.T, db *database, b *transport.Conn) {
	t.Helper()
	if got := db.metrics.Snapshot().ClientConnectionFailures; got != 1 {
		t.Fatalf("ClientConnectionFailures = %d, want 1: the arm must report A's failure", got)
	}
	if b.IsClosed() || pooledConn(db, armAddr) != b {
		t.Fatal("the report about A closed its replacement B")
	}
	if db.failMon.isFailed(armAddr) {
		t.Fatal("the report about A marked the reachable address failed")
	}
}

// Once A holds an arm's request, the pool gets B and A dies; the arm's reply-failure
// report must leave B pooled, open and its address alive.
func TestLateReplyFailureEveryArm(t *testing.T) {
	t.Parallel()
	for _, arm := range connArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peerA, peerB, peerC := newIdentityPeer(t), newIdentityPeer(t), newIdentityPeer(t)
			db, tx := armDatabase(ctx, peerA, peerB, peerC)
			defer closeIdentityDatabase(db)
			armCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				arm.run(armCtx, db, tx)
			}()
			defer func() { stop(); <-done }()
			if _, ok := peerA.recv(ctx); !ok {
				t.Fatal("no request on A")
			}
			b := replaceAndKill(t, ctx, db, pooledConn(db, armAddr), func() { peerA.server.Close() }, done, peerB, peerC)
			requireReplacementKept(t, db, b)
		})
	}
}

// writeSignalConn reports when the client starts writing its first frame (its
// second write: the connect packet is one).
type writeSignalConn struct {
	net.Conn
	writes  atomic.Int32
	writing chan struct{}
}

func (c *writeSignalConn) Write(p []byte) (int, error) {
	if c.writes.Add(1) == 2 {
		close(c.writing)
	}
	return c.Conn.Write(p)
}

// newStallPeer completes the handshake and then reads nothing, so the client's
// first frame write blocks until the peer is killed.
func newStallPeer(t *testing.T) (*identityPeer, *writeSignalConn) {
	t.Helper()
	client, server := net.Pipe()
	w := &writeSignalConn{Conn: client, writing: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := transport.ReadConnectPacket(server); err != nil {
			return
		}
		packet := transport.ConnectPacket{ProtocolVersion: transport.ProtocolVersion73, ConnectionID: 262}
		_, _ = server.Write(packet.Marshal())
	}()
	t.Cleanup(func() { client.Close(); server.Close(); <-done })
	return &identityPeer{client: w, server: server}, w
}

// Each waiting send's frame blocks on A; the pool then gets B and A dies under the
// write. The arm's send-failure report must leave B alone.
func TestLateSendFailureEveryArm(t *testing.T) {
	t.Parallel()
	for _, arm := range connArms() {
		if arm.eager {
			continue // fails only on a connection closed before it sends; see RFC-271
		}
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peerA, writing := newStallPeer(t)
			peerB, peerC := newIdentityPeer(t), newIdentityPeer(t)
			db, tx := armDatabase(ctx, peerA, peerB, peerC)
			defer closeIdentityDatabase(db)
			a, err := db.getOrDial(ctx, armAddr)
			if err != nil {
				t.Fatal(err)
			}
			armCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				arm.run(armCtx, db, tx)
			}()
			defer func() { stop(); <-done }()
			select {
			case <-writing.writing:
			case <-ctx.Done():
				t.Fatal("no frame write on A")
			}
			b := replaceAndKill(t, ctx, db, a, func() { peerA.server.Close() }, done, peerB, peerC)
			requireReplacementKept(t, db, b)
		})
	}
}

// The hedged read path reports a failure against the connection that request
// was sent on, for the winner and for every request it ends as a loser.
func TestHedgeResultCarriesConn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	connA, connB := new(transport.Conn), new(transport.Conn)
	failed := func(addr string, conn *transport.Conn) sendFunc {
		return func() inFlightRPC {
			ch := make(chan transport.Response, 1)
			ch <- transport.Response{Err: transport.ErrConnClosed}
			return inFlightRPC{replyCh: ch, replyHandle: &transport.ReplyHandle{}, addr: addr, conn: conn, start: time.Now()}
		}
	}
	silent := func(addr string, conn *transport.Conn) sendFunc {
		return func() inFlightRPC {
			return inFlightRPC{replyCh: make(chan transport.Response), replyHandle: &transport.ReplyHandle{}, addr: addr, conn: conn, start: time.Now()}
		}
	}

	res := sendFrameWithHedge(ctx, nil, time.Hour, failed("a", connA), nil, time.Minute)
	if !res.connErr || res.addr != "a" || res.conn != connA {
		t.Fatalf("single request: addr %q conn %p connErr %v, want a/%p/true", res.addr, res.conn, res.connErr, connA)
	}

	// The primary fails before the hedge delay; its failure is ended as a loser.
	res = sendFrameWithHedge(ctx, nil, time.Hour, failed("a", connA), failed("b", connB), time.Minute)
	if res.addr != "b" || res.conn != connB || len(res.others) != 1 || res.others[0].addr != "a" || res.others[0].conn != connA {
		t.Fatalf("failed primary: winner %q/%p others %+v", res.addr, res.conn, res.others)
	}

	// Both requests outstanding when the reply timeout fires.
	res = sendFrameWithHedge(ctx, budgetModel(1), 0, silent("a", connA), silent("b", connB), time.Millisecond)
	if len(res.others) != 2 || res.others[0].conn != connA || res.others[1].conn != connB {
		t.Fatalf("timeout: others %+v, want a/%p and b/%p", res.others, connA, connB)
	}
	res = sendFrameWithHedge(ctx, nil, time.Hour, silent("a", connA), nil, time.Millisecond)
	if res.conn != connA {
		t.Fatalf("timeout of a single request: conn %p, want %p", res.conn, connA)
	}
}

// endOther reports a loser's transport failure against the loser's connection,
// so it cannot close the pool's replacement.
func TestEndOtherReportsLoserConn(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const addr = "peer:4500"
	db, _ := identityDatabase(ctx, newIdentityPeer(t), newIdentityPeer(t))
	defer closeIdentityDatabase(db)
	db.queueModel = newQueueModel()
	a, err := db.getOrDial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b, err := db.getOrDial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	db.endOther(rpcAccount{addr: addr, conn: a, err: transport.ErrConnClosed, start: time.Now()})
	if b.IsClosed() || pooledConn(db, addr) != b || db.failMon.isFailed(addr) {
		t.Fatal("a hedge loser's failure on A closed or failed its replacement B")
	}
	db.endOther(rpcAccount{addr: addr, conn: b, err: transport.ErrConnClosed, start: time.Now()})
	requireIdentityConnClosed(t, ctx, b)
	if !db.failMon.isFailed(addr) {
		t.Fatal("a failure on the pooled connection must evict it and fail the address")
	}
}

// closeGateConn blocks Close until released, as a TLS close_notify to a peer that
// does not read can.
type closeGateConn struct {
	net.Conn
	once                   sync.Once
	closing, release, done chan struct{}
	err                    error
}

func (c *closeGateConn) Close() error {
	c.once.Do(func() {
		close(c.closing)
		<-c.release
		c.err = c.Conn.Close()
		close(c.done)
	})
	return c.err
}

// failMetricsWriteConn holds only the chosen metrics frame, leaving monitor
// PINGs usable until the test releases a deterministic transport failure.
type failMetricsWriteConn struct {
	net.Conn
	token         transport.UID
	writing, fail chan struct{}
}

func (c *failMetricsWriteConn) Write(p []byte) (int, error) {
	r := bytes.NewReader(p)
	for r.Len() > 0 {
		token, _, err := transport.ReadFrame(r, false)
		if err != nil {
			break // the connect packet is not a frame
		}
		if token == c.token {
			close(c.writing)
			<-c.fail
			return 0, errors.New("test: metrics frame write failed")
		}
	}
	return c.Conn.Write(p)
}

func requireIdentityConnClosed(t *testing.T, ctx context.Context, conn *transport.Conn) {
	t.Helper()
	select {
	case <-conn.Done():
	case <-ctx.Done():
		t.Fatal("the failed connection was left open")
	}
	done := make(chan struct{})
	go func() {
		conn.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the failed connection's transport loops did not stop")
	}
}

// Failure reporting publishes eviction before database-owned teardown, so a
// slow close cannot stall the reporter or another connection lookup.
func TestHandleConnErrorClosesOutsideConnMu(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	gate := &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	peer.client = gate
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	db, _ := identityDatabase(ctx, peer)
	a, err := db.getOrDial(ctx, armAddr)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a completed dial's cohort undrained, as a sender paused before its flush.
	call := &dialCall{done: make(chan struct{}), conn: a}
	permit, _ := call.sends.Reserve()
	defer permit.Release()
	call.sends.Seal()
	close(call.done)
	db.connMu.Lock()
	db.dialing[armAddr] = call
	db.connMu.Unlock()
	hook := func() {
		if db.dialing[armAddr] == call {
			t.Error("the failed connection's completed dial cohort was still published before Close")
		}
	}
	db.beforeConnFailureMark.Store(&hook)
	done := make(chan struct{})
	go func() {
		defer close(done)
		db.handleConnError(armAddr, a)
	}()
	select {
	case <-gate.closing:
	case <-ctx.Done():
		t.Fatal("handleConnError did not close the failed connection")
	}
	locked := make(chan struct{})
	go func() {
		db.connMu.Lock()
		_ = len(db.connPool)
		db.connMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(3 * time.Second):
		release()
		t.Fatal("connMu held while the failed connection closes")
	}
	if pooledConn(db, armAddr) != nil || !db.failMon.isFailed(armAddr) {
		t.Fatal("the failed connection must leave the pool and its address be marked failed before the close")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("failure reporting waited for the blocked connection close")
	}
	select {
	case <-gate.done:
		t.Fatal("the socket closed before its close gate was released")
	default:
	}
	release()
	select {
	case <-gate.done:
	case <-ctx.Done():
		t.Fatal("the socket did not close after its close gate was released")
	}
	requireIdentityConnClosed(t, ctx, a)
}

// Database shutdown detaches the entire pool before a socket close can block,
// so cancelled workers can still acquire connMu while Close joins teardown.
func TestDatabaseCloseClosesOutsideConnMu(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dbCtx, cancelDB := context.WithCancel(ctx)
	defer cancelDB()
	peer, other := newIdentityPeer(t), newIdentityPeer(t)
	gate := &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	peer.client = gate
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	db, _ := identityDatabase(dbCtx, peer, other)
	db.cancel = cancelDB
	publicDB := &Database{db: db}
	defer func() {
		release()
		publicDB.Close()
	}()
	first, err := db.getOrDial(ctx, armAddr)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.getOrDial(ctx, "other:4500")
	if err != nil {
		t.Fatal(err)
	}
	if first == second || pooledConn(db, armAddr) != first || pooledConn(db, "other:4500") != second {
		t.Fatal("the shutdown regression requires two distinct pooled connections")
	}
	done := make(chan error, 1)
	go func() { done <- publicDB.Close() }()
	select {
	case <-gate.closing:
	case <-ctx.Done():
		t.Fatal("Database.Close did not enter the blocked socket close")
	}
	if !errors.Is(dbCtx.Err(), context.Canceled) {
		t.Fatal("Database.Close did not cancel the database lifetime before closing sockets")
	}
	pooled := make(chan int, 1)
	go func() {
		db.connMu.Lock()
		n := len(db.connPool)
		db.connMu.Unlock()
		pooled <- n
	}()
	select {
	case n := <-pooled:
		if n != 0 {
			t.Fatalf("Database.Close left %d connections pooled while socket teardown was blocked", n)
		}
	case <-time.After(time.Second):
		t.Fatal("Database.Close held connMu while socket teardown was blocked")
	}
	select {
	case err := <-done:
		t.Fatalf("Database.Close returned before its blocked socket closed: %v", err)
	default:
	}
	select {
	case <-gate.done:
		t.Fatal("the socket closed before its close gate was released")
	default:
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Database.Close: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Database.Close did not finish after its close gate was released")
	}
	select {
	case <-gate.done:
	default:
		t.Fatal("Database.Close returned before the actual socket close completed")
	}
	requireIdentityConnClosed(t, ctx, first)
	requireIdentityConnClosed(t, ctx, second)
}

// Draining the pool does not consume a completed dial cohort. A database-close
// check must reject that still-open connection even while another close stalls.
func TestDatabaseCloseRejectsCompletedDialCohort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dbCtx, cancelDB := context.WithCancel(ctx)
	defer cancelDB()
	peers := [2]*identityPeer{newIdentityPeer(t), newIdentityPeer(t)}
	var gates [2]*closeGateConn
	for i, peer := range peers {
		gates[i] = &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
		peer.client = gates[i]
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			for _, gate := range gates {
				close(gate.release)
			}
		})
	}
	db, _ := identityDatabase(dbCtx, peers[0], peers[1])
	db.cancel = cancelDB
	publicDB := &Database{db: db}
	defer func() {
		release()
		publicDB.Close()
	}()
	addresses := [2]string{"close-a:4500", "close-b:4500"}
	var conns [2]*transport.Conn
	var calls [2]*dialCall
	for i, addr := range addresses {
		conn, err := db.getOrDial(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = conn
		call := &dialCall{done: make(chan struct{}), conn: conn}
		permit, _ := call.sends.Reserve()
		defer permit.Release()
		call.sends.Seal()
		close(call.done)
		calls[i] = call
		db.connMu.Lock()
		db.dialing[addr] = call
		db.connMu.Unlock()
	}
	done := make(chan error, 1)
	go func() { done <- publicDB.Close() }()
	var live int
	select {
	case <-gates[0].closing:
		live = 1
	case <-gates[1].closing:
		live = 0
	case <-ctx.Done():
		t.Fatal("Database.Close did not enter either blocked socket close")
	}
	if !errors.Is(dbCtx.Err(), context.Canceled) || conns[live].IsClosed() {
		t.Fatal("the database must be cancelled while the other connection remains open")
	}
	select {
	case <-gates[live].closing:
		t.Fatal("the second connection started closing before the first close gate was released")
	default:
	}
	db.connMu.Lock()
	cohortPending := db.dialing[addresses[live]] == calls[live] && calls[live].sends.Pending()
	poolEmpty := len(db.connPool) == 0
	db.connMu.Unlock()
	if !cohortPending || !poolEmpty {
		t.Fatal("the regression requires an undrained completed cohort outside the drained pool")
	}
	callerCtx, stopCaller := context.WithCancel(ctx)
	defer stopCaller()
	conn, err := db.getOrDial(callerCtx, addresses[live])
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("ordinary lookup after database close = (%p, %v), want (nil, context.Canceled)", conn, err)
	}
	conn, permit, err := db.getOrDialForSend(callerCtx, addresses[live])
	if permit != nil {
		permit.Release()
	}
	if conn != nil || !errors.Is(err, context.Canceled) {
		t.Errorf("send lookup after database close = (%p, %v), want (nil, context.Canceled)", conn, err)
	}
	if err := callerCtx.Err(); err != nil {
		t.Fatalf("database close also cancelled the independent caller: %v", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Database.Close: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Database.Close did not finish after both close gates were released")
	}
	for i, conn := range conns {
		select {
		case <-gates[i].done:
		default:
			t.Fatalf("connection %d's socket close did not complete", i)
		}
		requireIdentityConnClosed(t, ctx, conn)
	}
}

type metricsArm struct {
	name       string
	endpoint   int
	reply      string
	send       func(context.Context, *Transaction, []ServerInfo) error
	operation  func(context.Context, *Transaction, []byte, []byte) error
	replyToken func(*testing.T, []byte) transport.UID
}

func (arm metricsArm) connArm() connArm {
	return connArm{arm.name, false, func(ctx context.Context, _ *database, tx *Transaction) {
		_ = arm.send(ctx, tx, []ServerInfo{{Address: armAddr, Token: armToken}})
	}}
}

func metricsArms() []metricsArm {
	return []metricsArm{
		{
			name: "wait metrics", endpoint: EndpointWaitMetrics, reply: "StorageMetrics_basic",
			operation: func(ctx context.Context, tx *Transaction, begin, end []byte) error {
				_, err := tx.GetEstimatedRangeSizeBytes(ctx, begin, end)
				return err
			},
			send: func(ctx context.Context, tx *Transaction, servers []ServerInfo) error {
				_, err := tx.sendWaitMetrics(ctx, []byte("k"), []byte("l"), 1, servers)
				return err
			},
			replyToken: func(t *testing.T, body []byte) transport.UID {
				t.Helper()
				var req types.WaitMetricsRequest
				if err := req.UnmarshalFDB(body); err != nil {
					t.Fatal(err)
				}
				return replyToken(req.Reply.Token)
			},
		},
		{
			name: "split range", endpoint: EndpointGetRangeSplitPoints, reply: "SplitRangeReply_two_points",
			operation: func(ctx context.Context, tx *Transaction, begin, end []byte) error {
				_, err := tx.GetRangeSplitPoints(ctx, begin, end, 1000)
				return err
			},
			send: func(ctx context.Context, tx *Transaction, servers []ServerInfo) error {
				_, err := tx.sendSplitRange(ctx, []byte("k"), []byte("l"), 1000, servers)
				return err
			},
			replyToken: func(t *testing.T, body []byte) transport.UID {
				t.Helper()
				var req types.SplitRangeRequest
				if err := req.UnmarshalFDB(body); err != nil {
					t.Fatal(err)
				}
				return replyToken(req.Reply.Token)
			},
		},
	}
}

// startArm runs arm until the test ends.
func startArm(t *testing.T, ctx context.Context, arm connArm, db *database, tx *Transaction) {
	t.Helper()
	armCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		arm.run(armCtx, db, tx)
	}()
	t.Cleanup(func() { stop(); <-done })
}

// noFrame fails if peer receives a frame within d.
func noFrame(t *testing.T, peer *identityPeer, d time.Duration, why string) {
	t.Helper()
	select {
	case f := <-peer.frames:
		t.Fatalf("%s: a request reached the peer (token %v)", why, f.token)
	case <-time.After(d):
	}
}

// A losing shard's transport teardown belongs to the database, not the public
// metrics operation: a blocked close cannot hide a sibling error or cancellation.
func TestMetricsFanoutDoesNotWaitForConnectionClose(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancelled=%t", arm.name, cancelled), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				healthy, losing := newIdentityPeer(t), newIdentityPeer(t)
				write := &failMetricsWriteConn{
					Conn: losing.client, token: getAdjustedEndpoint(armToken, arm.endpoint),
					writing: make(chan struct{}), fail: make(chan struct{}),
				}
				gate := &closeGateConn{Conn: write, closing: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
				losing.client = gate
				var failOnce, releaseOnce sync.Once
				failWrite := func() { failOnce.Do(func() { close(write.fail) }) }
				release := func() { releaseOnce.Do(func() { close(gate.release) }) }
				db, tx := armDatabase(ctx, healthy, losing)
				tx.tenantId = 42
				db.altFailureDelayOverride = 30 * time.Second
				const losingAddr = "losing:4500"
				var losingConn *transport.Conn
				opCtx, stop := context.WithCancel(ctx)
				var done chan struct{}
				defer func() {
					failWrite()
					release()
					stop()
					if done != nil {
						<-done
					}
					if losingConn != nil {
						losingConn.Close()
					}
					closeIdentityDatabase(db)
				}()
				healthyConn, err := db.getOrDial(ctx, armAddr)
				if err != nil {
					t.Fatal(err)
				}
				losingConn, err = db.getOrDial(ctx, losingAddr)
				if err != nil {
					t.Fatal(err)
				}
				db.locCache.entries = []locationEntry{
					{tenantId: 42, begin: []byte("a"), end: []byte("m"), servers: []ServerInfo{{Address: armAddr, Token: armToken}}},
					{tenantId: 42, begin: []byte("m"), end: []byte("z"), servers: []ServerInfo{{Address: losingAddr, Token: armToken}}},
				}
				reporting := make(chan struct{})
				var reportOnce sync.Once
				hook := func() {
					if db.connPool[losingAddr] == nil {
						reportOnce.Do(func() { close(reporting) })
					}
				}
				db.beforeConnFailureMark.Store(&hook)
				done = make(chan struct{})
				go func() {
					defer close(done)
					err = arm.operation(opCtx, tx, []byte("a"), []byte("z"))
				}()
				frame, ok := healthy.recv(ctx)
				if !ok || frame.token != getAdjustedEndpoint(armToken, arm.endpoint) {
					t.Fatal("the healthy shard did not receive its metrics request")
				}
				reply := arm.replyToken(t, frame.body)
				select {
				case <-write.writing:
				case <-ctx.Done():
					t.Fatal("the losing shard did not enter its frame write")
				}
				failWrite()
				select {
				case <-gate.closing:
				case <-ctx.Done():
					t.Fatal("the failed connection did not enter its blocked close")
				}
				select {
				case <-reporting:
				case <-ctx.Done():
					t.Fatal("the losing shard did not report its connection failure")
				}
				if pooledConn(db, losingAddr) != nil || !db.failMon.isFailed(losingAddr) {
					t.Fatal("failure reporting did not publish eviction and failed state")
				}
				if !healthyConn.HasPendingReply(reply) {
					t.Fatal("the healthy shard was not still waiting at the close gate")
				}
				select {
				case <-gate.done:
					t.Fatal("the failed socket closed before the terminal/cancellation gate")
				default:
				}
				if cancelled {
					stop()
				} else if err := transport.WriteFrame(healthy.server, reply, (&types.ErrorOrError{ErrorCode: uint16(ErrTenantNotFound)}).MarshalFDB(), false); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("public metrics operation waited for its losing shard's blocked close")
				}
				if cancelled {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("operation cancellation = %v, want context.Canceled", err)
					}
				} else {
					assertFDBErrorCode(t, err, ErrTenantNotFound)
				}
				select {
				case <-gate.done:
					t.Fatal("the operation completed only after its failed socket closed")
				default:
				}
				if healthyConn.IsClosed() || healthyConn.HasPendingReply(reply) {
					t.Fatal("completion must release the healthy reply waiter, not its connection")
				}
				release()
				select {
				case <-gate.done:
				case <-ctx.Done():
					t.Fatal("the failed socket did not close after its gate was released")
				}
				requireIdentityConnClosed(t, ctx, losingConn)
			})
		}
	}
}

// Recovery of the request's replica, not another address, ends the wait
// well before the 30 s storm delay, as loadBalance waits on okFuture.
func TestMetricsRequestWakesOnRecovery(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			var dials atomic.Int32
			dial := db.dialFn
			db.dialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return nil, errors.New("test: replica down")
				}
				return dial(ctx, network, addr)
			}
			db.altFailureDelayOverride = 30 * time.Second
			parked := make(chan struct{})
			var parks atomic.Int32
			hook := func() {
				if parks.Add(1) == 1 {
					// Keep the replica excluded past the test, so no probe dials it.
					db.failMon.mu.Lock()
					db.failMon.failed[armAddr].excludedUntil = nowSeconds() + 600
					db.failMon.mu.Unlock()
					close(parked)
				}
			}
			db.beforeAltFailureWait.Store(&hook)
			startArm(t, ctx, arm.connArm(), db, tx)
			select {
			case <-parked:
			case <-ctx.Done():
				t.Fatal("the request did not wait for its failed replica")
			}
			db.failMon.markFailed("other:4500")
			db.failMon.markAlive("other:4500")
			noFrame(t, peer, time.Second, "another address recovered")
			if n := parks.Load(); n != 1 {
				t.Fatalf("another address's recovery restarted the wait (%d waits)", n)
			}
			db.failMon.markAlive(armAddr)
			waitCtx, waitStop := context.WithTimeout(ctx, 5*time.Second)
			defer waitStop()
			if _, ok := peer.recv(waitCtx); !ok {
				t.Fatal("the request still waits after its replica recovered")
			}
		})
	}
}

// Expiry permits a probe, not recovery: another address becoming live cannot
// end the original failure-delay wait while this replica's probe is still stuck.
func TestMetricsRecoveryIgnoresUnrelatedAfterExclusionExpires(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dbCtx, closeDB := context.WithCancel(ctx)
	defer closeDB()
	db, _ := identityDatabase(dbCtx)
	db.altFailureDelayOverride = 30 * time.Second
	db.failMon.markFailed(armAddr)
	entered := make(chan struct{})
	db.dialFn = func(ctx context.Context, _, addr string) (net.Conn, error) {
		if addr != armAddr {
			return nil, fmt.Errorf("unexpected probe address %q", addr)
		}
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	beforeWait := func() {
		db.failMon.mu.Lock()
		db.failMon.failed[armAddr].excludedUntil = nowSeconds() - 1
		db.failMon.mu.Unlock()
	}
	db.beforeAltFailureWait.Store(&beforeWait)
	opCtx, stop := context.WithCancel(ctx)
	checks := make(chan bool, 2)
	release := make(chan struct{})
	afterCheck := func(live bool) {
		select {
		case checks <- live:
		case <-opCtx.Done():
			return
		}
		if !live {
			select {
			case <-release:
			case <-opCtx.Done():
			}
		}
	}
	db.afterAltRecoveryCheck.Store(&afterCheck)
	servers := []ServerInfo{{Address: armAddr, Token: armToken}}
	recovered := db.failMon.waitForRecovery()
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		err = db.awaitAlternative(opCtx, dbCtx.Done(), servers, recovered, db.altFailureDelay())
	}()
	var call *dialCall
	defer func() {
		stop()
		closeDB()
		<-done
		if call != nil {
			<-call.done
		}
		closeIdentityDatabase(db)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the expired replica's probe did not start")
	}
	if !db.failMon.isFailed(armAddr) || db.failMon.excluded(armAddr, nowSeconds()) {
		t.Fatal("the replica must remain failed after its exclusion deadline expires")
	}
	db.connMu.Lock()
	call = db.dialing[armAddr]
	db.connMu.Unlock()
	if call == nil {
		t.Fatal("the stalled probe has no shared dial")
	}
	db.failMon.markFailed("other:4500")
	db.failMon.markAlive("other:4500")
	select {
	case live := <-checks:
		if live {
			t.Fatal("unrelated recovery treated the expired replica as live")
		}
	case <-done:
		t.Fatalf("unrelated recovery ended the failure-delay wait: %v", err)
	case <-ctx.Done():
		t.Fatal("the failure-delay wait did not consume unrelated recovery")
	}
	select {
	case <-done:
		t.Fatalf("the original failure-delay wait ended before its replica recovered: %v", err)
	default:
	}
	if !db.failMon.isFailed(armAddr) {
		t.Fatal("unrelated recovery cleared this replica's failed state")
	}
	db.failMon.markAlive(armAddr)
	close(release)
	select {
	case live := <-checks:
		if !live {
			t.Fatal("the wait did not recognize its own replica's recovery")
		}
	case <-ctx.Done():
		t.Fatal("the wait did not consume its own replica's recovery")
	}
	select {
	case <-done:
		if err != nil {
			t.Fatalf("own-replica recovery = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("own-replica recovery did not end the wait with a stalled probe")
	}
	select {
	case <-call.done:
		t.Fatal("ending the wait cancelled the database-owned shared dial")
	default:
	}
}

// A recovered replica is usable while another replica's shared probe dial is stalled;
// ending the recovery wait must abandon only the probe's waiter, not the shared dial.
func TestMetricsRecoveryDoesNotWaitForOtherProbe(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	db, tx := armDatabase(ctx, peer)
	db.altFailureDelayOverride = 30 * time.Second
	const blocked = "blocked:4500"
	db.failMon.markFailed(blocked)
	db.failMon.markFailed(armAddr)
	entered := make(chan struct{})
	dial := db.dialFn
	db.dialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == blocked {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dial(ctx, network, addr)
	}
	// Both replicas are excluded when the request decides to wait. Only the blocked
	// replica becomes probe-eligible before awaitAlternative arms its probe timer.
	hook := func() {
		db.failMon.mu.Lock()
		db.failMon.failed[blocked].excludedUntil = nowSeconds() - 1
		db.failMon.failed[armAddr].excludedUntil = nowSeconds() + 600
		db.failMon.mu.Unlock()
	}
	db.beforeAltFailureWait.Store(&hook)
	opCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tx.sendWaitMetrics(opCtx, []byte("k"), []byte("l"), 1,
			[]ServerInfo{{Address: blocked, Token: armToken}, {Address: armAddr, Token: armToken}})
	}()
	defer func() {
		stop()
		cancel()
		<-done
		closeIdentityDatabase(db)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the excluded-replica probe did not begin its dial")
	}
	db.connMu.Lock()
	call := db.dialing[blocked]
	db.connMu.Unlock()
	if call == nil {
		t.Fatal("the stalled probe has no shared dial")
	}
	defer func() { cancel(); <-call.done }()
	// Keep the blocked replica excluded after recovery, so only armAddr can send.
	db.failMon.markFailed(blocked)
	db.failMon.markAlive(armAddr)
	waitCtx, waitStop := context.WithTimeout(ctx, time.Second)
	defer waitStop()
	f, ok := peer.recv(waitCtx)
	if !ok || f.token != getAdjustedEndpoint(armToken, EndpointWaitMetrics) {
		t.Fatal("recovery did not send to the available replica while the other probe dial was stalled")
	}
	select {
	case <-call.done:
		t.Fatal("abandoning the probe cancelled the database-owned shared dial")
	default:
	}
}

// A replica whose metrics endpoint was reported not found, before the request or
// while it waits for the reply, gets no further request; the call reports 1006.
func TestMetricsRequestSkipsEndpointNotFound(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, during := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/during=%t", arm.name, during), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				peer := newIdentityPeer(t)
				db, tx := armDatabase(ctx, peer)
				defer closeIdentityDatabase(db)
				token := getAdjustedEndpoint(armToken, arm.endpoint)
				if !during {
					db.failMon.markEndpointNotFound(armAddr, token)
				}
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = arm.send(ctx, tx, []ServerInfo{{Address: armAddr, Token: armToken}})
				}()
				if during {
					f, ok := peer.recv(ctx)
					if !ok || f.token != token {
						t.Fatalf("first request: %v, %v", f.token, ok)
					}
					// The server no longer has the stream (FlowTransport ENDPOINT_NOT_FOUND).
					if err := transport.WriteFrame(peer.server, transport.WellKnownToken(transport.WLTokenEndpointNotFound), endpointNotFoundBody(token), false); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal("the request did not end")
				}
				assertFDBErrorCode(t, err, ErrAllAlternativesFailed)
				noFrame(t, peer, 0, "after not found")
			})
		}
	}
}

// endpointNotFoundBody is FlowTransport's SerializeSource<UID> body for tok: a FakeRoot
// table, UID inline at field 0, file identifier 15597147.
func endpointNotFoundBody(tok transport.UID) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[0:], 16)
	binary.LittleEndian.PutUint32(b[4:], 15597147)
	binary.LittleEndian.PutUint16(b[8:], 6)
	binary.LittleEndian.PutUint16(b[10:], 24)
	binary.LittleEndian.PutUint16(b[12:], 8)
	binary.LittleEndian.PutUint32(b[16:], 8)
	binary.LittleEndian.PutUint64(b[24:], tok.First)
	binary.LittleEndian.PutUint64(b[32:], tok.Second)
	return b
}

// An in-band broken_promise is not a response: the request marks that endpoint not
// found and goes on to the next replica in the same call, as loadBalance does.
func TestMetricsRequestBrokenPromiseTriesNextReplica(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			second := transport.UID{First: 0x99, Second: 0xaa}
			servers := []ServerInfo{{Address: armAddr, Token: armToken}, {Address: armAddr, Token: second}}
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				err = arm.send(ctx, tx, servers)
			}()
			// The first replica is chosen at random; the second request must go to the other.
			var first transport.UID
			for i, body := range [][]byte{(&types.ErrorOrError{ErrorCode: 1100}).MarshalFDB(), replyVector(t, arm.reply)} {
				f, ok := peer.recv(ctx)
				if !ok {
					t.Fatalf("request %d missing", i)
				}
				if i == 0 {
					first = f.token
				} else if f.token == first {
					t.Fatalf("request 1 went to the broken replica again (%v)", f.token)
				}
				if err := transport.WriteFrame(peer.server, arm.replyToken(t, f.body), body, false); err != nil {
					t.Fatal(err)
				}
			}
			<-done
			if err != nil {
				t.Fatalf("second replica's reply: %v", err)
			}
			if !db.failMon.endpointFailed(armAddr, first) {
				t.Fatal("broken_promise did not mark the endpoint not found")
			}
		})
	}
}

// A replica the failure monitor excludes is skipped, not dialed, while another is
// available, as loadBalance skips failed alternatives (LoadBalance.actor.h:595-614).
func TestMetricsRequestSkipsExcludedReplica(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			const dead = "dead:4500"
			db.failMon.markFailed(dead)
			var deadDials atomic.Int32
			dial := db.dialFn
			db.dialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if addr == dead {
					deadDials.Add(1)
					return nil, errors.New("test: unreachable")
				}
				return dial(ctx, network, addr)
			}
			servers := []ServerInfo{{Address: dead, Token: armToken}, {Address: armAddr, Token: armToken}}
			armCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = arm.send(armCtx, tx, servers)
			}()
			defer func() { stop(); <-done }()
			// Two attempts, whatever the random start: the first is answered
			// request_maybe_delivered, so the next attempt moves on in the list.
			for i := range 2 {
				f, ok := peer.recv(ctx)
				if !ok {
					t.Fatalf("request %d did not reach the available replica", i)
				}
				if i == 0 {
					if err := transport.WriteFrame(peer.server, arm.replyToken(t, f.body), (&types.ErrorOrError{ErrorCode: 1030}).MarshalFDB(), false); err != nil {
						t.Fatal(err)
					}
				}
			}
			if n := deadDials.Load(); n != 0 {
				t.Fatalf("the excluded replica was dialed %d times", n)
			}
		})
	}
}

// A metrics request waits for a slow replica's reply, as loadBalance does: no reply
// timeout re-sends it.
func TestMetricsRequestWaitsForSlowReply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	db, tx := armDatabase(ctx, peer)
	defer closeIdentityDatabase(db)
	reply := replyVector(t, "StorageMetrics_basic")
	want, err := parseWaitMetricsReply(reply)
	if err != nil {
		t.Fatal(err)
	}
	var got int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err = tx.sendWaitMetrics(ctx, []byte("k"), []byte("l"), 1, []ServerInfo{{Address: armAddr, Token: armToken}})
	}()
	f, ok := peer.recv(ctx)
	if !ok {
		t.Fatal("no request")
	}
	noFrame(t, peer, DefaultRPCTimeout+time.Second, "while the reply is slow")
	if err := transport.WriteFrame(peer.server, metricsArms()[0].replyToken(t, f.body), reply, false); err != nil {
		t.Fatal(err)
	}
	<-done
	if err != nil || got != want {
		t.Fatalf("got %d, %v; want %d", got, err, want)
	}
}

// Probing after exclusion expires lets the replica's recovery end the 30 s wait
// without other traffic, as connectionKeeper does.
func TestMetricsRequestProbesExcludedReplica(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			peer := newIdentityPeer(t)
			db, tx := armDatabase(ctx, peer)
			defer closeIdentityDatabase(db)
			var dials atomic.Int32
			dial := db.dialFn
			db.dialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if dials.Add(1) == 1 {
					return nil, errors.New("test: replica down")
				}
				return dial(ctx, network, addr)
			}
			db.altFailureDelayOverride = 30 * time.Second
			var parks atomic.Int32
			hook := func() { parks.Add(1) }
			db.beforeAltFailureWait.Store(&hook)
			startArm(t, ctx, arm.connArm(), db, tx)
			waitCtx, stop := context.WithTimeout(ctx, connFailureInitialWindow*time.Second+5*time.Second)
			defer stop()
			if _, ok := peer.recv(waitCtx); !ok {
				t.Fatal("no request after the replica's exclusion window ended")
			}
			if n := parks.Load(); n != 1 {
				t.Fatalf("%d waits, want the one the probe ended", n)
			}
		})
	}
}

// Once every replica was tried, each further request waits a backoff that starts
// at 10 ms and doubles per request (LoadBalance.actor.h:301-308, :721-725).
func TestMetricsRequestBacksOffPerRequest(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	db, tx := armDatabase(ctx, peer)
	defer closeIdentityDatabase(db)
	servers := []ServerInfo{{Address: armAddr, Token: armToken}, {Address: armAddr, Token: transport.UID{First: 0x99, Second: 0xaa}}}
	armCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = tx.sendWaitMetrics(armCtx, []byte("k"), []byte("l"), 1, servers)
	}()
	defer func() { stop(); <-done }()
	// Each request is answered request_maybe_delivered, so the next one follows.
	var at []time.Time
	for range 6 {
		f, ok := peer.recv(ctx)
		if !ok {
			t.Fatal("request missing")
		}
		at = append(at, time.Now())
		if err := transport.WriteFrame(peer.server, metricsArms()[0].replyToken(t, f.body), (&types.ErrorOrError{ErrorCode: 1030}).MarshalFDB(), false); err != nil {
			t.Fatal(err)
		}
	}
	for i := 2; i < len(at); i++ {
		if gap, want := at[i].Sub(at[i-1]), loadBalanceStartBackoff<<(i-2); gap < want {
			t.Errorf("request %d followed request %d after %v, want at least %v", i, i-1, gap, want)
		}
	}
}

// A location with no servers reports all_alternatives_failed, so the caller re-locates.
func TestMetricsRequestWithoutServers(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			db, tx := armDatabase(ctx, newIdentityPeer(t))
			defer closeIdentityDatabase(db)
			assertFDBErrorCode(t, arm.send(ctx, tx, nil), ErrAllAlternativesFailed)
		})
	}
}

// metricsWaitContext observes a wait without cancelling the request. For backoff,
// the post-reply Err check arms the observation of the following timer select.
type metricsWaitContext struct {
	context.Context
	waiting  chan struct{}
	once     sync.Once
	observe  atomic.Bool
	armOnErr atomic.Bool
}

func (ctx *metricsWaitContext) Done() <-chan struct{} {
	if ctx.observe.Load() {
		ctx.once.Do(func() { close(ctx.waiting) })
	}
	return ctx.Context.Done()
}

func (ctx *metricsWaitContext) Err() error {
	if ctx.armOnErr.Swap(false) {
		ctx.observe.Store(true)
	}
	return ctx.Context.Err()
}

// Both dead-end location shapes wait before callers can re-locate; bypassing the
// delay must fail even if the sender still returns all_alternatives_failed.
func TestMetricsRequestAllFailedDelay(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, empty := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/empty=%t", arm.name, empty), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				peer := newIdentityPeer(t)
				db, tx := armDatabase(ctx, peer)
				defer closeIdentityDatabase(db)
				db.altFailureDelayOverride = 30 * time.Second
				var servers []ServerInfo
				if !empty {
					servers = []ServerInfo{{Address: armAddr, Token: armToken}, {Address: armAddr, Token: transport.UID{First: 0x99, Second: 0xaa}}}
					for _, server := range servers {
						db.failMon.markEndpointNotFound(server.Address, getAdjustedEndpoint(server.Token, arm.endpoint))
					}
				}
				opCtx, stop := context.WithCancel(ctx)
				waitCtx := &metricsWaitContext{Context: opCtx, waiting: make(chan struct{})}
				waitCtx.observe.Store(true)
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = arm.send(waitCtx, tx, servers)
				}()
				defer func() { stop(); <-done }()
				select {
				case <-waitCtx.waiting:
				case <-done:
					t.Fatalf("sender bypassed the all-failed delay: %v", err)
				case <-ctx.Done():
					t.Fatal("sender did not enter the all-failed delay")
				}
				select {
				case <-done:
					t.Fatalf("sender ended during the long all-failed delay: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				stop()
				select {
				case <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("all-failed delay cancellation = %v, want context.Canceled", err)
					}
				case <-ctx.Done():
					t.Fatal("all-failed delay ignored request cancellation")
				}
				noFrame(t, peer, 0, "with no usable endpoint")
			})
		}
	}
}

// Database close also interrupts the dead-end delay for empty locations and
// exhausted endpoint lists, without relying on request cancellation or sockets.
func TestMetricsRequestDatabaseCloseInterruptsAllFailedDelay(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, empty := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/empty=%t", arm.name, empty), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				dbCtx, closeDB := context.WithCancel(ctx)
				defer closeDB()
				peer := newIdentityPeer(t)
				db, tx := armDatabase(dbCtx, peer)
				defer closeIdentityDatabase(db)
				db.altFailureDelayOverride = 30 * time.Second
				var servers []ServerInfo
				if !empty {
					servers = []ServerInfo{{Address: armAddr, Token: armToken}, {Address: armAddr, Token: transport.UID{First: 0x99, Second: 0xaa}}}
					for _, server := range servers {
						db.failMon.markEndpointNotFound(server.Address, getAdjustedEndpoint(server.Token, arm.endpoint))
					}
				}
				opCtx, stop := context.WithCancel(ctx)
				waitCtx := &metricsWaitContext{Context: opCtx, waiting: make(chan struct{})}
				waitCtx.observe.Store(true)
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = arm.send(waitCtx, tx, servers)
				}()
				defer func() { stop(); <-done }()
				select {
				case <-waitCtx.waiting:
				case <-done:
					t.Fatalf("request returned before entering the all-failed delay: %v", err)
				case <-ctx.Done():
					t.Fatal("request did not enter the all-failed delay")
				}
				closeDB()
				select {
				case <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("database-close result = %v, want context.Canceled", err)
					}
				case <-time.After(500 * time.Millisecond):
					t.Fatal("all-failed delay did not return promptly after database close")
				}
				if err := opCtx.Err(); err != nil {
					t.Fatalf("database close also cancelled the independent request: %v", err)
				}
				noFrame(t, peer, 0, "with no usable endpoint")
			})
		}
	}
}

// Closing the database interrupts both loadBalance waits independently of the
// request's lifetime, including an already-armed backoff timer.
func TestMetricsRequestDatabaseCloseInterruptsWait(t *testing.T) {
	t.Parallel()
	for _, arm := range metricsArms() {
		for _, backoff := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/backoff=%t", arm.name, backoff), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				dbCtx, closeDB := context.WithCancel(ctx)
				defer closeDB()
				peer := newIdentityPeer(t)
				db, tx := armDatabase(dbCtx, peer)
				defer closeIdentityDatabase(db)
				servers := []ServerInfo{{Address: armAddr, Token: armToken}}
				opCtx, stop := context.WithCancel(ctx)
				waitCtx := &metricsWaitContext{Context: opCtx, waiting: make(chan struct{})}
				if !backoff {
					db.altFailureDelayOverride = 30 * time.Second
					db.failMon.markFailed(armAddr)
					db.failMon.mu.Lock()
					db.failMon.failed[armAddr].excludedUntil = nowSeconds() + 600
					db.failMon.mu.Unlock()
					hook := func() { waitCtx.observe.Store(true) }
					db.beforeAltFailureWait.Store(&hook)
				}
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = arm.send(waitCtx, tx, servers)
				}()
				defer func() { stop(); <-done }()
				if backoff {
					for i := range 8 {
						f, ok := peer.recv(ctx)
						if !ok {
							t.Fatalf("request %d missing", i)
						}
						if i == 7 {
							// The successful frame write has already checked Err; the
							// next check follows this retryable reply, before backoff.
							waitCtx.armOnErr.Store(true)
						}
						if err := transport.WriteFrame(peer.server, arm.replyToken(t, f.body), (&types.ErrorOrError{ErrorCode: 1030}).MarshalFDB(), false); err != nil {
							t.Fatal(err)
						}
					}
				}
				select {
				case <-waitCtx.waiting:
				case <-done:
					t.Fatalf("request returned before entering its wait: %v", err)
				case <-ctx.Done():
					t.Fatal("request did not enter its wait")
				}
				closeDB()
				select {
				case <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("database-close result = %v, want context.Canceled", err)
					}
				case <-time.After(500 * time.Millisecond):
					t.Fatal("request did not return promptly after database close")
				}
				if err := opCtx.Err(); err != nil {
					t.Fatalf("database close also cancelled the independent request: %v", err)
				}
				noFrame(t, peer, 0, "after database close")
			})
		}
	}
}
