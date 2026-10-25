package client

import (
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
		if !a.IsClosed() {
			t.Fatal("the failed connection was left open")
		}
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
	return []connArm{
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
		{"wait metrics", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendWaitMetrics(ctx, key, end, 1, servers)
		}},
		{"split range", false, func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendSplitRange(ctx, key, end, 1000, servers)
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
	if !b.IsClosed() || !db.failMon.isFailed(addr) {
		t.Fatal("a failure on the pooled connection must evict it and fail the address")
	}
}

// closeGateConn blocks Close until released, as a TLS close_notify to a peer that
// does not read can.
type closeGateConn struct {
	net.Conn
	once             sync.Once
	closing, release chan struct{}
}

func (c *closeGateConn) Close() error {
	c.once.Do(func() { close(c.closing) })
	<-c.release
	return c.Conn.Close()
}

// handleConnError closes the failed connection after releasing connMu, so a slow
// close cannot stall every connection lookup.
func TestHandleConnErrorClosesOutsideConnMu(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := newIdentityPeer(t)
	gate := &closeGateConn{Conn: peer.client, closing: make(chan struct{}), release: make(chan struct{})}
	peer.client = gate
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	db, _ := identityDatabase(ctx, peer)
	a, err := db.getOrDial(ctx, armAddr)
	if err != nil {
		t.Fatal(err)
	}
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
	release()
	<-done
	if !a.IsClosed() {
		t.Fatal("the failed connection was left open")
	}
}

// metricsArms are the connArms that send a storage-metrics request.
func metricsArms() []connArm {
	var arms []connArm
	for _, arm := range connArms() {
		if arm.name == "wait metrics" || arm.name == "split range" {
			arms = append(arms, arm)
		}
	}
	return arms
}

// metricsEndpoints maps each metrics arm to its interface stream.
var metricsEndpoints = map[string]int{"wait metrics": EndpointWaitMetrics, "split range": EndpointGetRangeSplitPoints}

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

// A metrics request whose only replica is down waits for that replica, as
// loadBalance waits on okFuture: another address's recovery does not end the wait,
// the replica's does, well before the 30 s storm delay.
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
			// A long storm: allAlternativesFailedDelay is its 30 s slow maximum.
			now := nowSeconds()
			db.failMon.mu.Lock()
			db.failMon.oldestAltFailure, db.failMon.newestAltFailure, db.failMon.lastAltSkip = now-1000, now, now
			db.failMon.mu.Unlock()
			parked := make(chan struct{})
			var parks atomic.Int32
			hook := func() {
				if parks.Add(1) == 1 {
					close(parked)
				}
			}
			db.beforeAltFailureWait.Store(&hook)
			startArm(t, ctx, arm, db, tx)
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
				token := getAdjustedEndpoint(armToken, metricsEndpoints[arm.name])
				if !during {
					db.failMon.markEndpointNotFound(armAddr, token)
				}
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					err = metricsArmError(ctx, arm.name, tx)
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

// metricsArmError sends the named metrics request to armAddr and returns its error.
func metricsArmError(ctx context.Context, name string, tx *Transaction) error {
	servers := []ServerInfo{{Address: armAddr, Token: armToken}}
	if name == "wait metrics" {
		_, err := tx.sendWaitMetrics(ctx, []byte("k"), []byte("l"), 1, servers)
		return err
	}
	_, err := tx.sendSplitRange(ctx, []byte("k"), []byte("l"), 1000, servers)
	return err
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

// metricsReplyToken decodes a metrics request's reply token.
func metricsReplyToken(t *testing.T, name string, body []byte) transport.UID {
	t.Helper()
	if name == "wait metrics" {
		var req types.WaitMetricsRequest
		if err := req.UnmarshalFDB(body); err != nil {
			t.Fatal(err)
		}
		return replyToken(req.Reply.Token)
	}
	var req types.SplitRangeRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatal(err)
	}
	return replyToken(req.Reply.Token)
}

// An in-band broken_promise is not a response: the request marks that endpoint not
// found and goes on to the next replica in the same call, as loadBalance does.
func TestMetricsRequestBrokenPromiseTriesNextReplica(t *testing.T) {
	t.Parallel()
	replies := map[string]string{"wait metrics": "StorageMetrics_basic", "split range": "SplitRangeReply_two_points"}
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
				if arm.name == "wait metrics" {
					_, err = tx.sendWaitMetrics(ctx, []byte("k"), []byte("l"), 1, servers)
				} else {
					_, err = tx.sendSplitRange(ctx, []byte("k"), []byte("l"), 1000, servers)
				}
			}()
			// The first replica is chosen at random; the second request must go to the other.
			var first transport.UID
			for i, body := range [][]byte{(&types.ErrorOrError{ErrorCode: 1100}).MarshalFDB(), replyVector(t, replies[arm.name])} {
				f, ok := peer.recv(ctx)
				if !ok {
					t.Fatalf("request %d missing", i)
				}
				if i == 0 {
					first = f.token
				} else if f.token == first {
					t.Fatalf("request 1 went to the broken replica again (%v)", f.token)
				}
				if err := transport.WriteFrame(peer.server, metricsReplyToken(t, arm.name, f.body), body, false); err != nil {
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
				if arm.name == "wait metrics" {
					_, _ = tx.sendWaitMetrics(armCtx, []byte("k"), []byte("l"), 1, servers)
				} else {
					_, _ = tx.sendSplitRange(armCtx, []byte("k"), []byte("l"), 1000, servers)
				}
			}()
			defer func() { stop(); <-done }()
			if _, ok := peer.recv(ctx); !ok {
				t.Fatal("no request reached the available replica")
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
	if err := transport.WriteFrame(peer.server, metricsReplyToken(t, "wait metrics", f.body), reply, false); err != nil {
		t.Fatal(err)
	}
	<-done
	if err != nil || got != want {
		t.Fatalf("got %d, %v; want %d", got, err, want)
	}
}

// A request whose only replica is excluded probes it when its exclusion window
// ends, as connectionKeeper does, so a recovered replica ends the 30 s wait
// without other traffic.
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
			now := nowSeconds()
			db.failMon.mu.Lock()
			db.failMon.oldestAltFailure, db.failMon.newestAltFailure, db.failMon.lastAltSkip = now-1000, now, now
			db.failMon.mu.Unlock()
			var parks atomic.Int32
			hook := func() { parks.Add(1) }
			db.beforeAltFailureWait.Store(&hook)
			startArm(t, ctx, arm, db, tx)
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
		if err := transport.WriteFrame(peer.server, metricsReplyToken(t, "wait metrics", f.body), (&types.ErrorOrError{ErrorCode: 1030}).MarshalFDB(), false); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, tx := armDatabase(ctx, newIdentityPeer(t))
	defer closeIdentityDatabase(db)
	_, err := tx.sendWaitMetrics(ctx, []byte("k"), []byte("l"), 1, nil)
	assertFDBErrorCode(t, err, ErrAllAlternativesFailed)
	_, err = tx.sendSplitRange(ctx, []byte("k"), []byte("l"), 1000, nil)
	assertFDBErrorCode(t, err, ErrAllAlternativesFailed)
}
