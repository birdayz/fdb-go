package client

import (
	"context"
	"encoding/binary"
	"errors"
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

// identityPeer is the server end of one dialed connection: it completes the
// handshake and reports every frame other than a PING.
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

// recv returns the next frame; it may run off the test goroutine.
func (p *identityPeer) recv(ctx context.Context) (identityFrame, bool) {
	select {
	case f := <-p.frames:
		return f, true
	case <-ctx.Done():
		return identityFrame{}, false
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

// A late failure report for a connection the pool has already replaced must not
// close the replacement. A location query on connection A sees A die; before its
// handler runs, a commit dials B and waits for B's reply. The handler then runs
// for A. If it closed whatever the pool held, the commit would lose B and report
// commit_unknown_result (1021); the replacement must instead deliver the proxy's
// answer (not_committed, 1020), and B's dial must still count as the address
// being alive. C++ connectionKeeper closes and reports only its own connection
// (FlowTransport.actor.cpp:924-1001).
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
	var once sync.Once
	hook := func() {
		once.Do(func() {
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

// handleConnError acts on the pooled connection only when it is the failed one,
// or is itself closed; a failure report the pool has outlived changes nothing
// but the failure counter.
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

// A redial that lands just before a failure report marks the address failed
// must not be overtaken by that mark: the report decides and marks under connMu,
// the lock the dial pools and marks alive under. The seam attempts the redial at
// that instant; it can only complete first if the mark runs outside connMu.
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

// A failure report racing the redial that replaces the failed connection never
// leaves an open pooled connection with its address marked failed.
func TestHandleConnErrorRacesRedial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const addr = "peer:4500"
	for i := 0; i < 50; i++ {
		db, _ := identityDatabase(ctx, newIdentityPeer(t), newIdentityPeer(t))
		a, err := db.getOrDial(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		a.Close()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); db.handleConnError(addr, a) }()
		go func() { defer wg.Done(); _, _ = db.getOrDial(ctx, addr) }()
		wg.Wait()
		if b := pooledConn(db, addr); b != nil && !b.IsClosed() && db.failMon.isFailed(addr) {
			t.Fatalf("iteration %d: open pooled connection with its address marked failed", i)
		}
		closeIdentityDatabase(db)
	}
}

// connArm runs one RPC through its production path against peer:4500.
type connArm struct {
	name string
	run  func(context.Context, *database, *Transaction)
}

const armAddr = "peer:4500"

var armToken = transport.UID{First: 0x77, Second: 0x88}

// connArms lists every path that reports a connection failure for an RPC it sent.
func connArms() []connArm {
	servers := []ServerInfo{{Address: armAddr, Token: armToken}}
	key, end := []byte("k"), []byte("l")
	return []connArm{
		{"commit", func(ctx context.Context, db *database, _ *Transaction) {
			input := &commitInput{db: db, readVersion: 1, tenantID: NoTenantID}
			_, _ = input.commitNative(ctx)
		}},
		{"grv", func(ctx context.Context, db *database, _ *Transaction) {
			_, _, _, _, _, _, _, _ = (&grvBatcher{}).sendGRVRequest(db, ctx, 0, 1, types.SpanContext{}, nil)
		}},
		{"location", func(ctx context.Context, db *database, _ *Transaction) {
			_, _ = db.locCache.queryLocations(db, ctx, NoTenantID, func(tok transport.UID) []byte {
				return buildGetKeyServerLocationsRequest(key, false, NoTenantID, types.SpanContext{}, tok)
			}, nil)
		}},
		{"wait metrics", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendWaitMetrics(ctx, key, end, 1, servers)
		}},
		{"split range", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendSplitRange(ctx, key, end, 1000, servers)
		}},
		{"get value from one server", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendGetValueToServer(ctx, key, servers[0], 1, false, NoTenantID, readTagSample{})
		}},
		{"get value", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _ = tx.sendGetValue(ctx, key, servers, readTagSample{})
		}},
		{"get key", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _, _, _ = tx.sendGetKey(ctx, 1, key, false, 1, servers, readTagSample{}, nil)
		}},
		{"get range", func(ctx context.Context, _ *database, tx *Transaction) {
			_, _, _ = sendRangeRPC(tx, ctx, KeySelector{Key: key}, KeySelector{Key: end}, newRangeLimits(10, 0), 1, false, servers, readTagSample{}, plainRangeRPCOps)
		}},
		{"watch", func(ctx context.Context, _ *database, tx *Transaction) {
			_ = tx.sendWatch(ctx, key, nil, 1, types.SpanContext{}, servers, readTagSample{})
		}},
		{"pipelined get", func(ctx context.Context, _ *database, tx *Transaction) {
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
	db.locCache.insertSorted([]locationEntry{{tenantId: NoTenantID, begin: []byte{}, end: []byte{0xff}, servers: []ServerInfo{{Address: armAddr, Token: armToken}}}})
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

// Every reply-failure arm reports the connection its request went out on. Each
// arm runs its production path; once A holds the request, the pool is given a
// replacement B, and A dies. The arm's report about A must leave B pooled, open
// and its address alive.
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

// The send-failure arms report late too: a send waits for its write, and the
// connection can die while it waits. Each arm's first frame blocks on A; the pool
// is given a replacement B, and A dies under the blocked write. The arm's report
// about A must leave B pooled, open and its address alive.
func TestLateSendFailureEveryArm(t *testing.T) {
	t.Parallel()
	for _, arm := range connArms() {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			peerA, writing := newStallPeer(t)
			peerB, peerC := newIdentityPeer(t), newIdentityPeer(t)
			db, tx := armDatabase(ctx, peerA, peerB, peerC)
			defer closeIdentityDatabase(db)
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
			b := replaceAndKill(t, ctx, db, pooledConn(db, armAddr), func() { peerA.server.Close() }, done, peerB, peerC)
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

	res := sendFrameWithHedge(ctx, time.Hour, failed("a", connA), nil, time.Minute)
	if !res.connErr || res.addr != "a" || res.conn != connA {
		t.Fatalf("single request: addr %q conn %p connErr %v, want a/%p/true", res.addr, res.conn, res.connErr, connA)
	}

	// The primary fails before the hedge delay; its failure is ended as a loser.
	res = sendFrameWithHedge(ctx, time.Hour, failed("a", connA), failed("b", connB), time.Minute)
	if res.addr != "b" || res.conn != connB || len(res.others) != 1 || res.others[0].addr != "a" || res.others[0].conn != connA {
		t.Fatalf("failed primary: winner %q/%p others %+v", res.addr, res.conn, res.others)
	}

	// Both requests outstanding when the reply timeout fires.
	res = sendFrameWithHedge(ctx, 0, silent("a", connA), silent("b", connB), time.Millisecond)
	if len(res.others) != 2 || res.others[0].conn != connA || res.others[1].conn != connB {
		t.Fatalf("timeout: others %+v, want a/%p and b/%p", res.others, connA, connB)
	}
	res = sendFrameWithHedge(ctx, time.Hour, silent("a", connA), nil, time.Millisecond)
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
