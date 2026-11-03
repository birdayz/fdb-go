package client

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
)

// A refused connect reaches a request as C++'s peer disconnect, request_maybe_delivered
// (FlowTransport.actor.cpp:852-869, 1001-1004); these tests drive it through every dialing path.

const (
	// refusedSSAddr is a storage replica whose port has no listener.
	refusedSSAddr = "10.9.9.9:4500"
	// liveSSAddr is the storage server GetKeyServerLocationsReply_one_range names
	// (cmd/fdb-schema-extract/main.cpp), so a relocation finds it.
	liveSSAddr = "10.1.2.3:4500"
)

var liveSSToken = transport.UID{First: 0x7373737373737373, Second: 0x8484848484848484}

// connectRefused is what net.Dialer returns for a port with no listener.
func connectRefused(addr string) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func TestConnectFailureIsRequestMaybeDelivered(t *testing.T) {
	t.Parallel()
	newDB := func(ctx context.Context, dial transport.DialFunc) *database {
		return &database{ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{}, failMon: newFailureMonitor(), dialFn: dial}
	}

	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		db := newDB(context.Background(), func(_ context.Context, _, addr string) (net.Conn, error) { return nil, connectRefused(addr) })
		_, err := db.getOrDial(context.Background(), refusedSSAddr)
		assertFDBErrorCode(t, err, ErrRequestMaybeDelivered)
		var connectFailed *transport.ConnectFailedError
		if !errors.As(err, &connectFailed) || connectFailed.Addr != refusedSSAddr || !errors.Is(connectFailed.Cause(), syscall.ECONNREFUSED) {
			t.Fatalf("want a ConnectFailedError for %s carrying ECONNREFUSED, got %v", refusedSSAddr, err)
		}
		// The dial error is message-only: probes see the FDB code and nothing else.
		if errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("the raw dial error must not be in the chain: %v", err)
		}
	})

	t.Run("connect_timeout", func(t *testing.T) {
		t.Parallel()
		// The dial's own deadline (db.ctx + DefaultRPCTimeout) is a connect timeout,
		// which C++ also reports as connection_failed (FlowTransport.actor.cpp:852-857).
		db := newDB(context.Background(), func(context.Context, string, string) (net.Conn, error) {
			return nil, fmt.Errorf("dial tcp: %w", context.DeadlineExceeded)
		})
		_, err := db.getOrDial(context.Background(), refusedSSAddr)
		assertFDBErrorCode(t, err, ErrRequestMaybeDelivered)
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a connect timeout must not read as the caller's deadline: %v", err)
		}
	})

	t.Run("database_closed", func(t *testing.T) {
		t.Parallel()
		dbCtx, closeDB := context.WithCancel(context.Background())
		entered := make(chan struct{})
		db := newDB(dbCtx, func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		result := make(chan error, 1)
		go func() { _, err := db.getOrDial(context.Background(), refusedSSAddr); result <- err }()
		<-entered
		closeDB()
		err := <-result
		if !errors.Is(err, context.Canceled) || isMaybeDelivered(err) {
			t.Fatalf("a dial ended by Close is the database's end, not a peer failure: %v", err)
		}
	})
}

// lbRetry is loadBalance's backoff: none until every alternative was tried, then doubling
// (LoadBalance.actor.h:682-686, 721-725; flow/Knobs.cpp:274-276).
func TestLBRetryBackoff(t *testing.T) {
	t.Parallel()
	ms := time.Millisecond
	for _, tc := range []struct {
		n    int
		want []time.Duration
	}{
		{1, []time.Duration{10 * ms, 20 * ms, 40 * ms, 80 * ms}},
		{3, []time.Duration{0, 0, 10 * ms, 20 * ms, 40 * ms}},
	} {
		var r lbRetry
		for i, want := range tc.want {
			if got := r.next(tc.n); got != want {
				t.Fatalf("n=%d attempt %d: delay %v, want %v", tc.n, i+1, got, want)
			}
		}
	}
	var r lbRetry
	var last time.Duration
	for range 20 {
		last = r.next(1)
	}
	if last != loadBalanceMaxBackoff {
		t.Fatalf("backoff after 20 attempts = %v, want the %v cap", last, loadBalanceMaxBackoff)
	}
}

// refusalCluster is a database whose commit and GRV proxy is the identity peer at
// armAddr and whose storage replicas are dialed through dialStorage.
type refusalCluster struct {
	db      *database
	tx      *Transaction
	proxy   *identityPeer
	storage *identityPeer
	// refusals counts refused storage dials; refuseLive is how many dials to
	// liveSSAddr are refused before it accepts.
	refusals   atomic.Int32
	refuseLive int32
	liveDials  atomic.Int32
	locations  atomic.Int32
}

func newRefusalCluster(t *testing.T, ctx context.Context, refuseLive int32, locationReply ...[]byte) *refusalCluster {
	t.Helper()
	c := &refusalCluster{proxy: newIdentityPeer(t), storage: newIdentityPeer(t), refuseLive: refuseLive}
	db := &database{
		ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{},
		failMon: newFailureMonitor(), proxiesChanged: make(chan struct{}),
	}
	var proxyOnce, storageOnce sync.Once
	db.dialFn = func(_ context.Context, _, addr string) (net.Conn, error) {
		switch addr {
		case armAddr:
			var conn net.Conn
			proxyOnce.Do(func() { conn = c.proxy.client })
			if conn == nil {
				return nil, errors.New("test: proxy redialed")
			}
			return conn, nil
		case liveSSAddr:
			if c.liveDials.Add(1) <= c.refuseLive {
				c.refusals.Add(1)
				return nil, connectRefused(addr)
			}
			var conn net.Conn
			storageOnce.Do(func() { conn = c.storage.client })
			if conn == nil {
				return nil, errors.New("test: storage redialed")
			}
			return conn, nil
		default:
			c.refusals.Add(1)
			return nil, connectRefused(addr)
		}
	}
	db.queueModel = newQueueModel()
	db.queueModel.failMon = db.failMon
	db.locCache.maxSize = 16
	proxies := []ProxyInfo{{Address: armAddr, Token: armToken}}
	db.dbInfo.Store(&DBInfo{GRVProxies: proxies, CommitProxies: proxies})
	db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.grvCache.epochNow(), version: 1})
	c.db = db
	c.tx = (&Database{db: db}).CreateTransaction()
	c.tx.SetReadVersion(1)
	t.Cleanup(func() { closeIdentityDatabase(db) })
	c.serveLocations(t, ctx, locationReply...)
	return c
}

// serveLocations repeats the configured reply, defaulting to [a, z) on liveSSAddr.
func (c *refusalCluster) serveLocations(t *testing.T, ctx context.Context, locationReply ...[]byte) {
	t.Helper()
	reply := metricsReplacementLocationsReply(t, [2]string{"a", "z"})
	if len(locationReply) > 0 {
		reply = locationReply[0]
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, ok := c.proxy.recv(ctx)
			if !ok {
				return
			}
			var req types.GetKeyServerLocationsRequest
			if req.UnmarshalFDB(f.body) != nil {
				continue
			}
			c.locations.Add(1)
			_ = transport.WriteFrame(c.proxy.server, replyToken(req.Reply.Token), reply, false)
		}
	}()
	t.Cleanup(func() { <-done })
}

// cache places [a, z) on servers.
func (c *refusalCluster) cache(servers ...ServerInfo) {
	c.db.locCache.insertRanges([]locationEntry{{tenantId: NoTenantID, begin: []byte("a"), end: []byte("z"), servers: servers}})
}

// preferRefused makes the QueueModel rank the refusing replica first, so the
// first attempt deterministically dials it.
func (c *refusalCluster) preferRefused() {
	withServer(c.db.queueModel, liveSSAddr, func(d *queueData) {
		d.smoothOutstanding.total, d.smoothOutstanding.estimate, d.smoothOutstanding.time = 100, 100, nowSeconds()
	})
}

// readOp is one storage read run through its production retry loop.
type readOp struct {
	name  string
	run   func(ctx context.Context, tx *Transaction) error
	reply func(t *testing.T, body []byte) (transport.UID, []byte)
}

func resolvedGetKeyReply(t *testing.T) []byte {
	t.Helper()
	data := replyVector(t, "GetKeyReply_basic")
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		t.Fatal(err)
	}
	sel, err := r.ReadNestedReader(types.GetKeyReplySlotSel)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(data[sel.ObjectPos()+sel.FieldOffset(types.KeySelectorRefSlotOffset):], 0)
	if key, orEqual, offset, _, err := parseGetKeyReply(data); err != nil || string(key) != "resolved_key" || !orEqual || offset != 0 {
		t.Fatalf("resolved GetKeyReply = %q %v %d %v", key, orEqual, offset, err)
	}
	return data
}

func oneRowRangeReply() []byte {
	data := binary.LittleEndian.AppendUint32(nil, 1)
	for _, s := range []string{"k1", "v1"} {
		data = binary.LittleEndian.AppendUint32(data, uint32(len(s)))
		data = append(data, s...)
	}
	return types.MarshalErrorOrValueGetKeyValuesReply(&types.GetKeyValuesReply{Data: data})
}

func rangeRequestReply(t *testing.T, body []byte) (transport.UID, []byte) {
	t.Helper()
	var req types.GetKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatal(err)
	}
	return replyToken(req.Reply.Token), oneRowRangeReply()
}

func mappedRequestReply(t *testing.T, body []byte) (transport.UID, []byte) {
	t.Helper()
	var req types.GetMappedKeyValuesRequest
	if err := req.UnmarshalFDB(body); err != nil {
		t.Fatal(err)
	}
	return replyToken(req.Reply.Token), replyVector(t, "GetMappedKeyValuesReply_getrange")
}

// mappedRows checks the two rows of GetMappedKeyValuesReply_getrange came back.
func mappedRows(r mappedRangeResult, err error) error {
	if err == nil && (len(r.rows) != 2 || string(r.rows[0].Key) != "idx_r") {
		err = fmt.Errorf("mapped rows %v", r.rows)
	}
	return err
}

func storageReadOps() []readOp {
	key := []byte("k")
	return []readOp{
		{"get value", func(ctx context.Context, tx *Transaction) error {
			v, err := tx.getValue(ctx, key)
			if err == nil && string(v) != "ground_truth_value" {
				err = fmt.Errorf("value %q", v)
			}
			return err
		}, func(t *testing.T, body []byte) (transport.UID, []byte) {
			var req types.GetValueRequest
			if err := req.UnmarshalFDB(body); err != nil {
				t.Fatal(err)
			}
			return replyToken(req.Reply.Token), replyVector(t, "GetValueReply_present")
		}},
		{"get key", func(ctx context.Context, tx *Transaction) error {
			k, _, err := tx.getKeyImpl(ctx, 1, key, false, 1)
			if err == nil && string(k) != "resolved_key" {
				err = fmt.Errorf("key %q", k)
			}
			return err
		}, func(t *testing.T, body []byte) (transport.UID, []byte) {
			var req types.GetKeyRequest
			if err := req.UnmarshalFDB(body); err != nil {
				t.Fatal(err)
			}
			return replyToken(req.Reply.Token), resolvedGetKeyReply(t)
		}},
		{"get range", func(ctx context.Context, tx *Transaction) error {
			kvs, _, _, err := tx.getRangeImpl(ctx, key, []byte("l"), newRangeLimits(10, 0), 1, false)
			if err == nil && (len(kvs) != 1 || string(kvs[0].Key) != "k1") {
				err = fmt.Errorf("rows %v", kvs)
			}
			return err
		}, rangeRequestReply},
		{"native range", func(ctx context.Context, tx *Transaction) error {
			r, err := tx.nativeGetRangeAt(ctx, KeySelector{Key: key, Offset: 1}, KeySelector{Key: []byte("l"), Offset: 1}, newRangeLimits(10, 0), 1, false)
			if err == nil && (len(r.kvs) != 1 || string(r.kvs[0].Key) != "k1") {
				err = fmt.Errorf("rows %v", r.kvs)
			}
			return err
		}, rangeRequestReply},
		{"mapped range", func(ctx context.Context, tx *Transaction) error {
			r, err := tx.getMappedRangeSelectors(ctx, mappedSelectorFGE(key), mappedSelectorFGE([]byte("l")), []byte("m"), newMappedRangeLimits(2, ByteLimitUnlimited), 1, false)
			return mappedRows(r, err)
		}, mappedRequestReply},
		{"mapped range exact", func(ctx context.Context, tx *Transaction) error {
			r, err := tx.getMappedRangeSelectorExact(ctx, key, []byte("l"), []byte("m"), newMappedRangeLimits(2, ByteLimitUnlimited), 1, false)
			return mappedRows(r, err)
		}, mappedRequestReply},
		{"watch", func(ctx context.Context, tx *Transaction) error {
			return tx.watchPoll(ctx, nil, key, nil, 1, types.SpanContext{}, nil)
		}, func(t *testing.T, body []byte) (transport.UID, []byte) {
			var req types.WatchValueRequest
			if err := req.UnmarshalFDB(body); err != nil {
				t.Fatal(err)
			}
			return replyToken(req.Reply.Token), replyVector(t, "WatchValueReply_basic")
		}},
	}
}

// runRead runs op and answers the first storage request with its reply.
func runRead(t *testing.T, ctx context.Context, c *refusalCluster, op readOp) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- op.run(ctx, c.tx) }()
	f, ok := c.storage.recv(ctx)
	if !ok {
		t.Fatalf("no request reached the reachable replica; read result: %v", <-result)
	}
	token, reply := op.reply(t, f.body)
	if err := transport.WriteFrame(c.storage.server, token, reply, false); err != nil {
		t.Fatal(err)
	}
	return <-result
}

// A read whose chosen replica refuses connections completes from another replica,
// as loadBalance's retry of request_maybe_delivered does (LoadBalance.actor.h:344-377).
func TestReadRefusedReplicaUsesAnotherReplica(t *testing.T) {
	t.Parallel()
	for _, op := range storageReadOps() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, 0)
			c.cache(ServerInfo{Address: refusedSSAddr, Token: liveSSToken}, ServerInfo{Address: liveSSAddr, Token: liveSSToken})
			c.preferRefused()
			if err := runRead(t, ctx, c, op); err != nil {
				t.Fatalf("read with one refusing replica: %v", err)
			}
			if c.refusals.Load() == 0 {
				t.Fatal("positive control: the refusing replica was never dialed")
			}
			if !c.db.failMon.isFailed(refusedSSAddr) {
				t.Fatal("the refused replica must be marked failed")
			}
		})
	}
}

// A read whose only replica refuses connections retries until the replica accepts,
// as loadBalance does, and surfaces nothing; before, the raw dial error escaped.
func TestReadRetriesUntilRefusingReplicaAccepts(t *testing.T) {
	t.Parallel()
	const refused = 3
	for _, op := range storageReadOps() {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, refused)
			c.cache(ServerInfo{Address: liveSSAddr, Token: liveSSToken})
			start := time.Now()
			if err := runRead(t, ctx, c, op); err != nil {
				t.Fatalf("read whose only replica refused %d connects: %v", refused, err)
			}
			if got := c.refusals.Load(); got != refused {
				t.Fatalf("refused connects = %d, want %d", got, refused)
			}
			// One alternative: loadBalance waits 10, 20 and 40 ms after the refusals (lbRetry).
			if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
				t.Fatalf("read retried %d refusals in %v, want at least the 70ms of backoff", refused, elapsed)
			}
		})
	}
}

// The production loops keep one lbRetry across relocation, charging every refused
// replica rather than one pass (LoadBalance.actor.h:682-686, 721-725).
func TestReadLoopsPaceEveryRefusedReplica(t *testing.T) {
	t.Parallel()
	cases := 0
	for _, op := range storageReadOps() {
		if op.name != "get value" && op.name != "watch" {
			continue
		}
		cases++
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, 2)
			var attempts []int
			var delays []time.Duration
			observe := func(n int, d time.Duration) { attempts = append(attempts, n); delays = append(delays, d) }
			c.db.onReadBackoff.Store(&observe)
			c.cache(
				ServerInfo{Address: refusedSSAddr, Token: liveSSToken},
				ServerInfo{Address: "10.9.9.8:4500", Token: liveSSToken},
				ServerInfo{Address: "10.9.9.7:4500", Token: liveSSToken})
			if err := runRead(t, ctx, c, op); err != nil {
				t.Fatalf("read after refused replicas: %v", err)
			}
			if got := c.refusals.Load(); got != 5 {
				t.Fatalf("refusals = %d, want three initial and two relocated", got)
			}
			want := []time.Duration{0, 0, 10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
			if !slices.Equal(attempts, []int{1, 2, 3, 4, 5}) || !slices.Equal(delays, want) {
				t.Fatalf("attempts=%v delays=%v, want 1..5 and %v", attempts, delays, want)
			}
		})
	}
	if cases != 2 {
		t.Fatalf("retry-loop population=%d, want Get and Watch", cases)
	}
}

// Literal 1030 replies take the same loadBalance path as failed connects
// (LoadBalance.actor.h:344-377), not an unpaced relocation loop.
func TestReadLoopsPaceEncodedMaybeDelivered(t *testing.T) {
	t.Parallel()
	cases := 0
	for _, op := range storageReadOps() {
		if op.name != "get value" && op.name != "watch" {
			continue
		}
		cases++
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, 0)
			var attempts []int
			var delays []time.Duration
			observe := func(n int, d time.Duration) { attempts = append(attempts, n); delays = append(delays, d) }
			c.db.onReadBackoff.Store(&observe)
			c.cache(ServerInfo{Address: liveSSAddr, Token: liveSSToken})
			result := make(chan error, 1)
			go func() { result <- op.run(ctx, c.tx) }()
			for i := range 4 {
				f, ok := c.storage.recv(ctx)
				if !ok {
					t.Fatalf("request %d did not reach storage", i+1)
				}
				token, reply := op.reply(t, f.body)
				if i < 3 {
					reply = (&types.ErrorOrError{ErrorCode: ErrRequestMaybeDelivered}).MarshalFDB()
				}
				if err := transport.WriteFrame(c.storage.server, token, reply, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-result; err != nil {
				t.Fatalf("read after three encoded 1030 replies: %v", err)
			}
			want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
			if !slices.Equal(attempts, []int{1, 2, 3}) || !slices.Equal(delays, want) {
				t.Fatalf("attempts=%v delays=%v, want 1..3 and %v", attempts, delays, want)
			}
		})
	}
	if cases != 2 {
		t.Fatalf("encoded-reply population=%d, want Get and Watch", cases)
	}
}

// Relocation must keep both peers; otherwise it can hide a sender that repeatedly
// reselects the lower-metric 1030 responder.
func retryReplicaLocations(t *testing.T) []byte {
	t.Helper()
	first := locationsReplyVector(t)
	second := slices.Clone(first)
	ep := slot2EndpointReader(t, second)
	ip := ep
	for _, slot := range []int{types.EndpointSlotAddresses, types.NetworkAddressListSlotAddress, types.NetworkAddressSlotIp} {
		var err error
		ip, err = ip.ReadNestedReader(slot)
		if err != nil {
			t.Fatal(err)
		}
	}
	field := ip.ObjectPos() + ip.FieldOffset(types.IPAddressSlotAddr+1)
	target := field + int(binary.LittleEndian.Uint32(second[field:]))
	binary.LittleEndian.PutUint32(second[target:], 0x0a010204)
	binary.LittleEndian.PutUint64(second[ep.ObjectPos()+ep.FieldOffset(types.EndpointSlotToken):], 0x7575757575757575)
	_, server := firstLocation(t, first)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[4:], types.GetKeyServerLocationsReplyFileID)
	b, root := appendObject(b, 0, 9, 8, 4)
	b[root+8] = 2
	b, reply := appendObject(b, root+4, int(types.GetKeyServerLocationsReplyVTable[1]), types.GetKeyServerLocationsReplyVTable[2:]...)
	results := len(b)
	field = reply + int(types.GetKeyServerLocationsReplyVTable[2])
	binary.LittleEndian.PutUint32(b[field:], uint32(results-field))
	b = binary.LittleEndian.AppendUint32(b, 1)
	slot := len(b)
	b = binary.LittleEndian.AppendUint32(b, 0)
	b, pair := appendObject(b, slot, int(types.LocationPairVTable[1]), types.LocationPairVTable[2:]...)
	b, keyRange := appendObject(b, pair+int(types.LocationPairVTable[2]), int(types.KeyRangeRefVTable[1]), types.KeyRangeRefVTable[2:]...)
	b = appendString(b, keyRange+int(types.KeyRangeRefVTable[2]), "a")
	b = appendString(b, keyRange+int(types.KeyRangeRefVTable[3]), "z")
	field = pair + int(types.LocationPairVTable[3])
	binary.LittleEndian.PutUint32(b[field:], uint32(len(b)-field))
	b = binary.LittleEndian.AppendUint32(b, 2)
	slots := len(b)
	b = append(b, make([]byte, 8)...)
	for i, vector := range [][]byte{first, second} {
		for len(b)%8 != 0 {
			b = append(b, 0)
		}
		binary.LittleEndian.PutUint32(b[slots+4*i:], uint32(len(b)+server.ObjectPos()-slots-4*i))
		b = append(b, vector...)
	}
	return b
}

func retryReplicaCluster(t *testing.T, ctx context.Context) (*refusalCluster, *identityPeer, []ServerInfo) {
	t.Helper()
	reply := retryReplicaLocations(t)
	entries, err := parseGetKeyServerLocationsReply(reply)
	if err != nil || len(entries) != 1 || len(entries[0].servers) != 2 {
		t.Fatalf("two-peer location reply = %+v, %v", entries, err)
	}
	servers := entries[0].servers
	if servers[0].Address != liveSSAddr || servers[1].Address != "10.1.2.4:4500" || servers[0].Token == servers[1].Token {
		t.Fatalf("two distinct replica interfaces = %+v", servers)
	}
	c := newRefusalCluster(t, ctx, 0, reply)
	backup := newIdentityPeer(t)
	dial := c.db.dialFn
	var once sync.Once
	c.db.dialFn = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != servers[1].Address {
			return dial(ctx, network, addr)
		}
		var conn net.Conn
		once.Do(func() { conn = backup.client })
		if conn == nil {
			return nil, errors.New("test: backup redialed")
		}
		return conn, nil
	}
	c.cache(servers...)
	return c, backup, servers
}

// These seven affected paths also pin complete values, not only successful errors;
// Watch is an eighth, already-progressing control.
func retryReplicaReadOps() []readOp {
	ops := storageReadOps()
	plain := func(kvs []KeyValue, err error) error {
		if err == nil && (len(kvs) != 1 || string(kvs[0].Key) != "k1" || string(kvs[0].Value) != "v1") {
			return fmt.Errorf("plain rows = %v, want k1=v1", kvs)
		}
		return err
	}
	mapped := func(rows []MappedKeyValue, err error) error {
		if err != nil {
			return err
		}
		if len(rows) != 2 || string(rows[0].Key) != "idx_r" || string(rows[0].Value) != "pfx_r" || rows[0].Kind != MappedResultGetRange ||
			len(rows[0].GetRange.Rows) != 2 || string(rows[0].GetRange.Rows[0].Key) != "pfx_r_1" || string(rows[0].GetRange.Rows[0].Value) != "sub_1" ||
			string(rows[0].GetRange.Rows[1].Key) != "pfx_r_2" || string(rows[0].GetRange.Rows[1].Value) != "sub_2" || rows[0].GetRange.More ||
			string(rows[1].Key) != "idx_e" || string(rows[1].Value) != "pfx_e" || rows[1].Kind != MappedResultGetRange || len(rows[1].GetRange.Rows) != 0 || rows[1].GetRange.More {
			return fmt.Errorf("mapped rows = %+v, want both primary rows and exact secondary values", rows)
		}
		return nil
	}
	for i := range ops {
		switch ops[i].name {
		case "get range":
			ops[i].run = func(ctx context.Context, tx *Transaction) error {
				rows, more, _, err := tx.getRangeImpl(ctx, []byte("k"), []byte("l"), newRangeLimits(10, 0), 1, false)
				if err == nil && more {
					return errors.New("plain range unexpectedly reports more")
				}
				return plain(rows, err)
			}
		case "native range":
			ops[i].run = func(ctx context.Context, tx *Transaction) error {
				r, err := tx.nativeGetRangeAt(ctx, firstGreaterOrEqual([]byte("k")), firstGreaterOrEqual([]byte("l")), newRangeLimits(10, 0), 1, false)
				if err == nil && r.more {
					return errors.New("native range unexpectedly reports more")
				}
				return plain(r.kvs, err)
			}
		case "mapped range":
			ops[i].run = func(ctx context.Context, tx *Transaction) error {
				r, err := tx.getMappedRangeSelectors(ctx, mappedSelectorFGE([]byte("k")), mappedSelectorFGE([]byte("l")), []byte("m"), newMappedRangeLimits(2, ByteLimitUnlimited), 1, false)
				if err == nil && !r.more {
					return errors.New("mapped row limit did not report more")
				}
				return mapped(r.rows, err)
			}
		case "mapped range exact":
			ops[i].run = func(ctx context.Context, tx *Transaction) error {
				r, err := tx.getMappedRangeSelectorExact(ctx, []byte("k"), []byte("l"), []byte("m"), newMappedRangeLimits(2, ByteLimitUnlimited), 1, false)
				if err == nil && !r.more {
					return errors.New("mapped row limit did not report more")
				}
				return mapped(r.rows, err)
			}
		}
	}
	return append(ops, readOp{"mapped generic exact", func(ctx context.Context, tx *Transaction) error {
		rows, more, err := tx.getMappedRangeImpl(ctx, []byte("k"), []byte("l"), []byte("m"), 2, ByteLimitUnlimited, 1, false)
		if err == nil && !more {
			return errors.New("generic mapped row limit did not report more")
		}
		return mapped(rows, err)
	}, mappedRequestReply})
}

func TestReadHedgePacesFailuresNotCanceledLaggards(t *testing.T) {
	t.Parallel()
	cases := 0
	for _, op := range retryReplicaReadOps() {
		if op.name != "get value" && op.name != "get key" && op.name != "native range" {
			continue
		}
		cases++
		for _, mode := range []string{"refused", "reply-failure", "canceled-laggard"} {
			t.Run(op.name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var c *refusalCluster
				var backup *identityPeer
				var servers []ServerInfo
				if mode == "refused" {
					c = newRefusalCluster(t, ctx, 0)
					backup = c.storage
					servers = []ServerInfo{{Address: refusedSSAddr, Token: liveSSToken}, {Address: liveSSAddr, Token: liveSSToken}}
					c.cache(servers...)
				} else {
					c, backup, servers = retryReplicaCluster(t, ctx)
				}
				c.db.hedgeEnabled.Store(true)
				sentinel := c.db.queueModel.startRequest(servers[1].Address)
				withServer(c.db.queueModel, servers[1].Address, func(d *queueData) { d.smoothOutstanding.estimate = sentinel })
				defer func() {
					c.db.queueModel.endRequest(servers[1].Address, sentinel, 0, false)
					if total := totalOf(c.db.queueModel, servers[1].Address); total != 0 {
						t.Errorf("outstanding after sentinel release = %v, want 0", total)
					}
				}()
				if mode == "canceled-laggard" {
					setSecondState(c.db.queueModel, 1, 1)
					withServer(c.db.queueModel, servers[0].Address, func(d *queueData) { d.latency = .1 })
				}
				var attempts []int
				var delays []time.Duration
				observe := func(n int, d time.Duration) { attempts = append(attempts, n); delays = append(delays, d) }
				c.db.onReadBackoff.Store(&observe)
				done := make(chan struct{})
				var readErr error
				go func() { defer close(done); readErr = op.run(ctx, c.tx) }()
				defer func() { cancel(); <-done }()
				if mode != "refused" {
					if _, ok := c.storage.recv(ctx); !ok {
						t.Fatal("primary request missing")
					}
					if mode == "reply-failure" {
						_ = c.storage.server.Close()
					}
				}
				frame, ok := backup.recv(ctx)
				if !ok {
					t.Fatal("backup request missing")
				}
				token, reply := op.reply(t, frame.body)
				if err := transport.WriteFrame(backup.server, token, reply, false); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if readErr != nil {
					t.Fatalf("successful backup: %v", readErr)
				}
				if mode == "canceled-laggard" {
					if len(attempts) != 0 || len(delays) != 0 {
						t.Fatalf("canceled laggard counted as failure: %v %v", attempts, delays)
					}
				} else if !slices.Equal(attempts, []int{1}) || !slices.Equal(delays, []time.Duration{0}) {
					t.Fatalf("one real failure hidden by successful hedge: attempts=%v delays=%v", attempts, delays)
				}
				if a, b := totalOf(c.db.queueModel, servers[0].Address), totalOf(c.db.queueModel, servers[1].Address); a != 0 || b != sentinel {
					t.Fatalf("hedge outstanding = %v,%v, want 0,%v", a, b, sentinel)
				}
				if c.db.failMon.isFailed(servers[0].Address) != (mode != "canceled-laggard") {
					t.Fatalf("wrong primary failure state for %s", mode)
				}
			})
		}
	}
	if cases != 3 {
		t.Fatalf("hedge sender population = %d, want Get/GetKey/range", cases)
	}
}

func TestReadLoopsAdvancePastEncodedMaybeDelivered(t *testing.T) {
	t.Parallel()
	ops := retryReplicaReadOps()
	if len(ops) != 8 {
		t.Fatalf("retry population = %d, want seven affected paths plus Watch", len(ops))
	}
	for _, op := range ops {
		for _, allMaybe := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/all-maybe=%t", op.name, allMaybe), func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				c, backup, servers := retryReplicaCluster(t, ctx)
				c.db.hedgeEnabled.Store(false)
				sentinel := c.db.queueModel.startRequest(servers[1].Address)
				withServer(c.db.queueModel, servers[1].Address, func(d *queueData) { d.smoothOutstanding.estimate = sentinel })
				defer func() {
					c.db.queueModel.endRequest(servers[1].Address, sentinel, 0, false)
					if total := totalOf(c.db.queueModel, servers[1].Address); total != 0 {
						t.Errorf("outstanding after sentinel release = %v, want 0", total)
					}
				}()
				if best, second := c.db.queueModel.chooseTopTwo(servers); best != 0 || second != 1 {
					t.Fatalf("positive selection control = %d,%d, want lower-metric A then busier B", best, second)
				}
				var attempts []int
				var delays []time.Duration
				var pacedAt []time.Time
				observe := func(n int, d time.Duration) {
					attempts = append(attempts, n)
					delays = append(delays, d)
					pacedAt = append(pacedAt, time.Now())
				}
				c.db.onReadBackoff.Store(&observe)
				done := make(chan struct{})
				var readErr error
				go func() { defer close(done); readErr = op.run(ctx, c.tx) }()
				defer func() { cancel(); <-done }()
				count := 2
				if allMaybe {
					count = 4
				}
				var receivedAt []time.Time
				for i := range count {
					var frame identityFrame
					var peer *identityPeer
					select {
					case frame = <-c.storage.frames:
						peer = c.storage
					case frame = <-backup.frames:
						peer = backup
					case <-done:
						t.Fatalf("read completed before request %d: %v", i+1, readErr)
					case <-ctx.Done():
						t.Fatalf("request %d: %v", i+1, ctx.Err())
					}
					receivedAt = append(receivedAt, time.Now())
					if (peer == c.storage) != (i%2 == 0) {
						t.Fatalf("request %d revisited a peer before its alternative; want A,B passes", i+1)
					}
					endpoint := EndpointGetValue
					switch op.name {
					case "get key":
						endpoint = EndpointGetKey
					case "get range", "native range":
						endpoint = EndpointGetKeyValues
					case "mapped range", "mapped range exact", "mapped generic exact":
						endpoint = EndpointGetMappedKeyValues
					case "watch":
						endpoint = EndpointWatchValue
					}
					if want := getAdjustedEndpoint(servers[i%2].Token, endpoint); frame.token != want {
						t.Fatalf("request %d endpoint = %v, want %v", i+1, frame.token, want)
					}
					token, reply := op.reply(t, frame.body)
					if i < count-1 {
						reply = (&types.ErrorOrError{ErrorCode: 1030}).MarshalFDB()
					}
					if err := transport.WriteFrame(peer.server, token, reply, false); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if readErr != nil {
					t.Fatalf("read through healthy busier backup: %v", readErr)
				}
				wantAttempts, wantDelays := []int{1}, []time.Duration{0}
				wantLocations := int32(0)
				if allMaybe {
					wantAttempts, wantDelays = []int{1, 2, 3}, []time.Duration{0, 10 * time.Millisecond, 20 * time.Millisecond}
					wantLocations = 1
				}
				if !slices.Equal(attempts, wantAttempts) || !slices.Equal(delays, wantDelays) {
					t.Fatalf("attempts=%v delays=%v, want %v %v", attempts, delays, wantAttempts, wantDelays)
				}
				for i, delay := range delays {
					if elapsed := receivedAt[i+1].Sub(pacedAt[i]); elapsed < delay {
						t.Fatalf("attempt %d next request after %v, want >= %v", i+1, elapsed, delay)
					}
				}
				if got := c.locations.Load(); got != wantLocations {
					t.Fatalf("relocations = %d, want %d", got, wantLocations)
				}
				if a, b := totalOf(c.db.queueModel, servers[0].Address), totalOf(c.db.queueModel, servers[1].Address); a != 0 || b != sentinel {
					t.Fatalf("outstanding A=%v B=%v, want 0 and preserved %v", a, b, sentinel)
				}
				for _, server := range servers {
					if c.db.failMon.isFailed(server.Address) {
						t.Fatalf("encoded 1030 failed address %s", server.Address)
					}
					withServer(c.db.queueModel, server.Address, func(d *queueData) {
						if d.failedUntil != 0 || d.futureVersionBackoff != futureVersionInitialBackoff {
							t.Errorf("1030 created QueueModel failure state: %+v", d)
						}
					})
				}
				for _, endpoint := range []int{EndpointGetValue, EndpointGetKey, EndpointGetKeyValues, EndpointGetMappedKeyValues, EndpointWatchValue} {
					if live, ok := c.db.failMon.liveServers(servers, endpoint); !ok || len(live) != 2 {
						t.Fatalf("1030 excluded endpoint %d: %+v %t", endpoint, live, ok)
					}
				}
			})
		}
	}
}

// getValue and watch try every replica in one pass; each refused request is one loadBalance
// attempt, so the backoff starts after the pass (LoadBalance.actor.h:301-308, 682-686, 721-725).
func TestReadPacesEachRefusedReplica(t *testing.T) {
	t.Parallel()
	servers := []ServerInfo{
		{Address: refusedSSAddr, Token: liveSSToken},
		{Address: "10.9.9.8:4500", Token: liveSSToken},
		{Address: "10.9.9.7:4500", Token: liveSSToken},
	}
	for _, tc := range []struct {
		name  string
		hedge bool
		send  func(ctx context.Context, tx *Transaction, lb *lbRetry) error
	}{
		{"get value", false, func(ctx context.Context, tx *Transaction, lb *lbRetry) error {
			_, err := tx.sendGetValuePaced(ctx, []byte("k"), servers, readTagSample{}, lb)
			return err
		}},
		{"get value hedged", true, func(ctx context.Context, tx *Transaction, lb *lbRetry) error {
			_, err := tx.sendGetValuePaced(ctx, []byte("k"), servers, readTagSample{}, lb)
			return err
		}},
		{"watch", false, func(ctx context.Context, tx *Transaction, lb *lbRetry) error {
			return tx.sendWatchPaced(ctx, []byte("k"), nil, 1, types.SpanContext{}, servers, readTagSample{}, lb)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, 0)
			c.db.hedgeEnabled.Store(tc.hedge)
			var lb lbRetry
			if err := tc.send(ctx, c.tx, &lb); !isMaybeDelivered(err) {
				t.Fatalf("every replica refused: want request_maybe_delivered, got %v", err)
			}
			if got := c.refusals.Load(); got != int32(len(servers)) {
				t.Fatalf("refused connects = %d, want one per replica", got)
			}
			if lb.attempts != len(servers) || lb.backoff != loadBalanceStartBackoff {
				t.Fatalf("after one pass: %d attempts, backoff %v; want %d and %v", lb.attempts, lb.backoff, len(servers), loadBalanceStartBackoff)
			}
		})
	}
}

// A watch whose every replica refuses keeps retrying (watchValue's loadBalance)
// rather than failing with all_alternatives_failed after a bounded budget.
func TestWatchRefusedEveryReplicaWaits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := newRefusalCluster(t, ctx, 1<<30)
	c.cache(ServerInfo{Address: liveSSAddr, Token: liveSSToken})
	watchCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	err := c.tx.watchPoll(watchCtx, nil, []byte("k"), nil, 1, types.SpanContext{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a watch on an unreachable replica must wait for its context, got %v", err)
	}
	if c.refusals.Load() < 2 {
		t.Fatalf("positive control: %d refused connects", c.refusals.Load())
	}
}

// The send layer reports a refused connect as request_maybe_delivered, never the
// raw dial error, and the next attempt avoids the failed address.
func TestStorageSendRefusedIsMaybeDelivered(t *testing.T) {
	t.Parallel()
	servers := []ServerInfo{{Address: refusedSSAddr, Token: liveSSToken}, {Address: liveSSAddr, Token: liveSSToken}}
	for _, tc := range []struct {
		name string
		send func(ctx context.Context, tx *Transaction) error
	}{
		{"get key", func(ctx context.Context, tx *Transaction) error {
			_, _, _, err := tx.sendGetKey(ctx, 1, []byte("k"), false, 1, servers[:1], readTagSample{}, nil)
			return err
		}},
		{"get range", func(ctx context.Context, tx *Transaction) error {
			_, _, err := sendRangeRPC(tx, ctx, KeySelector{Key: []byte("k")}, KeySelector{Key: []byte("l")}, newRangeLimits(10, 0), 1, false, servers[:1], readTagSample{}, plainRangeRPCOps)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			c := newRefusalCluster(t, ctx, 0)
			c.preferRefused()
			err := tc.send(ctx, c.tx)
			var connectFailed *transport.ConnectFailedError
			if !isMaybeDelivered(err) || !errors.As(err, &connectFailed) || connectFailed.Addr != refusedSSAddr {
				t.Fatalf("send to a refusing replica: want its coded connect failure, got %v", err)
			}
			if !c.db.failMon.isFailed(refusedSSAddr) {
				t.Fatal("the refused replica must be marked failed")
			}
			best, _ := c.db.queueModel.chooseTopTwo(servers)
			if servers[best].Address != liveSSAddr {
				t.Fatalf("the next attempt chose %s, want the reachable replica", servers[best].Address)
			}
		})
	}
}

// A refused commit proxy is commit_unknown_result, as tryCommit makes of AtMostOnce
// request_maybe_delivered (LoadBalance.actor.h:828-830; NativeAPI.actor.cpp:6731-6773).
func TestCommitProxyRefusedIsCommitUnknownResult(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := &database{
		ctx: ctx, connPool: map[string]*transport.Conn{}, dialing: map[string]*dialCall{},
		failMon: newFailureMonitor(), proxiesChanged: make(chan struct{}),
		dialFn: func(_ context.Context, _, addr string) (net.Conn, error) { return nil, connectRefused(addr) },
	}
	proxies := []ProxyInfo{{Address: armAddr, Token: armToken}}
	db.dbInfo.Store(&DBInfo{GRVProxies: proxies, CommitProxies: proxies})
	_, err := (&commitInput{db: db, readVersion: 1, tenantID: NoTenantID}).commitNative(ctx)
	assertFDBErrorCode(t, err, ErrCommitUnknownResult)
	if !db.failMon.isFailed(armAddr) {
		t.Fatal("the refused proxy must be marked failed")
	}
}

// basicLoadBalance skips a failed commit proxy address (LoadBalance.actor.h:779-793);
// with every one failed, Go probes rather than failing the commit.
func TestGetCommitProxySkipsFailedAddress(t *testing.T) {
	t.Parallel()
	db := &database{failMon: newFailureMonitor()}
	proxies := []ProxyInfo{
		{Address: "p:1", Token: transport.UID{First: 1}},
		{Address: "p:2", Token: transport.UID{First: 2}},
	}
	db.dbInfo.Store(&DBInfo{CommitProxies: proxies})
	db.failMon.markFailed("p:1")
	for i := range 4 {
		if p, _, err := db.getCommitProxy(); err != nil || p.Address != "p:2" {
			t.Fatalf("pick %d: %v %v, want the live proxy p:2", i, p, err)
		}
	}
	db.failMon.markFailed("p:2")
	if p, _, err := db.getCommitProxy(); err != nil || p == nil {
		t.Fatalf("every proxy failed: want a probe, got %v %v", p, err)
	}
}
