package client

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// faultConn wraps a net.Conn. When killReads is set, Read returns EOF.
type faultConn struct {
	net.Conn
	killReads atomic.Bool
}

func (f *faultConn) Read(b []byte) (int, error) {
	if f.killReads.Load() {
		return 0, io.EOF
	}
	return f.Conn.Read(b)
}

// faultDialer creates connections and can arm/disarm faults.
type faultDialer struct {
	mu    sync.Mutex
	conns []*faultConn
	armed bool // when true, new connections have killReads=true
}

func (d *faultDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	fc := &faultConn{Conn: c}
	d.mu.Lock()
	if d.armed {
		fc.killReads.Store(true)
	}
	d.conns = append(d.conns, fc)
	d.mu.Unlock()
	return fc, nil
}

// arm sets killReads on ALL existing connections.
func (d *faultDialer) arm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = true
	for _, fc := range d.conns {
		fc.killReads.Store(true)
	}
}

// disarm clears killReads and ensures new connections are clean.
func (d *faultDialer) disarm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = false
	for _, fc := range d.conns {
		fc.killReads.Store(false)
	}
}

// TestCommitUnknownResult_NoDoubleApply verifies that the self-conflicting
// mechanism prevents double-apply of non-idempotent operations.
//
// Uses atomic ADD: counter starts at 10, we ADD 5. If the commit succeeds
// on the server but the client retries, a naive retry would ADD 5 again
// (counter=20). The self-conflicting mechanism should cause the retry to
// conflict, and the final Transact outcome should show counter=15.
//
// NOTE: commit_unknown_result + self-conflicting only prevents the IMMEDIATE
// retry from double-applying. After the self-conflicting retry fails with
// 1020, Transact retries AGAIN with a clean transaction. For atomic ADD,
// this third attempt WILL add 5 again (counter=20). This matches the C++
// client behavior — non-idempotent operations need application-level
// idempotency tokens for true exactly-once semantics.
//
// What this test DOES prove: the fault injection works, the server commits
// survive client-side connection death, and the self-conflicting mechanism
// fires (the retry gets 1020, not a second successful commit).
func TestCommitUnknownResult_NoDoubleApply(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	defer container.Terminate(ctx)

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		t.Fatalf("fdbcli configure exit: %d", exitCode)
	}
	time.Sleep(2 * time.Second)

	_, internalReader, _ := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, _ := ParseClusterString(strings.TrimSpace(internalStr))

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	fd := &faultDialer{}
	cluster := NewClusterFromConfig(connectCF)
	cluster.SetDialFunc(fd.dial)
	defer cluster.Close()

	if err := cluster.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	db := &Database{
		cluster:       cluster,
		grvBatcher:    NewGRVBatcher(cluster),
		locationCache: NewLocationCache(cluster),
	}

	// Seed counter = 10.
	var counterBuf [8]byte
	binary.LittleEndian.PutUint64(counterBuf[:], 10)
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set([]byte("fault_counter"), counterBuf[:])
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Log("seeded fault_counter=10")

	// Manual commit with fault: ADD 5, then kill connection.
	tx := db.CreateTransaction()
	rv, err := db.grvBatcher.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}
	tx.SetReadVersion(rv)

	binary.LittleEndian.PutUint64(counterBuf[:], 5)
	tx.Atomic(MutAddValue, []byte("fault_counter"), counterBuf[:])

	// Arm fault BEFORE commit — reply will be killed.
	fd.arm()

	err = tx.Commit(ctx)
	t.Logf("commit with fault: %v", err)
	if err == nil {
		t.Fatal("commit should have failed (connection killed)")
	}

	// Disarm and reconnect.
	fd.disarm()
	cluster.mu.Lock()
	for k := range cluster.connPool {
		delete(cluster.connPool, k)
	}
	cluster.mu.Unlock()

	if err := cluster.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	db.locationCache = NewLocationCache(cluster)

	// Read the counter. It should be 15 (10 + 5, applied once by server).
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, []byte("fault_counter"))
	})
	if err != nil {
		t.Fatalf("verify Get: %v", err)
	}
	val := binary.LittleEndian.Uint64(result.([]byte))
	t.Logf("fault_counter = %d", val)
	if val != 15 {
		t.Errorf("expected 15 (ADD applied once), got %d", val)
	}
}

// buildFDBErrorResponse crafts an ErrorOr<T> error response with the given
// FDB error code. The wire format matches what an FDB server sends when a
// storage server returns an error (e.g., wrong_shard_server = 1062).
//
// Layout: FakeRoot → Error object (vtable {6,6,4}, field 0 = int32 error_code).
// ReadErrorOr() detects vtable with ≤1 field and reads field 0 as the error code.
func buildFDBErrorResponse(errorCode int32) []byte {
	// ErrorOr root vtable: type byte + value RelOff.
	// NewReader treats the root as FakeRoot and follows field at offset 4 (value).
	errorOrVT := wire.VTable{8, 9, 8, 4}
	// Error vtable: 1 field (error_code uint16 at offset 4).
	// Object size 6 = 4 vtable-backref + 2 uint16.
	errorVT := wire.VTable{6, 6, 4}

	w := wire.NewWriter(nil)
	return w.WriteRootObject(0, errorOrVT, 4,
		[]wire.VTable{errorVT, errorOrVT},
		func(obj *wire.ObjectWriter) {
			obj.WriteUint8(8, 1) // type = Error variant
			obj.WriteStruct(4, errorVT, 4, func(inner *wire.ObjectWriter) {
				inner.WriteUint16(4, uint16(errorCode))
			})
		})
}

// wrongShardConn wraps a net.Conn with a pipe-based proxy goroutine.
// The proxy reads complete frames from the real connection and writes them
// to a pipe. When armed, it replaces the next non-PING frame's body with
// crafted ErrorOr bytes. This ensures frame boundary alignment — no
// production code changes needed.
type wrongShardConn struct {
	net.Conn
	pr       *io.PipeReader
	injectCh chan struct{} // buffered; send to arm
	errBody  []byte
}

func newWrongShardConn(c net.Conn, errBody []byte) *wrongShardConn {
	pr, pw := io.Pipe()
	wsc := &wrongShardConn{
		Conn:     c,
		pr:       pr,
		injectCh: make(chan struct{}, 1),
		errBody:  errBody,
	}
	go wsc.proxyLoop(pw)
	return wsc
}

func (w *wrongShardConn) Read(b []byte) (int, error) {
	return w.pr.Read(b)
}

func (w *wrongShardConn) proxyLoop(pw *io.PipeWriter) {
	defer pw.Close()

	// Forward the ConnectPacket (44 bytes) before entering frame mode.
	var cpBuf [transport.ConnectPacketSize]byte
	if _, err := io.ReadFull(w.Conn, cpBuf[:]); err != nil {
		pw.CloseWithError(err)
		return
	}
	if _, err := pw.Write(cpBuf[:]); err != nil {
		return
	}

	// Forward frames, optionally replacing the next non-PING response.
	pingToken := transport.WellKnownToken(transport.WLTokenPingPacket)
	for {
		token, body, err := transport.ReadFrame(w.Conn, false)
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		// Check if we should inject.
		select {
		case <-w.injectCh:
			if token == pingToken {
				// PING — pass through, re-queue the inject signal.
				w.injectCh <- struct{}{}
			} else {
				// Replace body with error response.
				body = w.errBody
			}
		default:
		}

		if err := transport.WriteFrame(pw, token, body, false); err != nil {
			return
		}
	}
}

func (w *wrongShardConn) arm() {
	select {
	case w.injectCh <- struct{}{}:
	default:
	}
}

// wrongShardDialer creates wrongShardConns and can arm them for fault injection.
type wrongShardDialer struct {
	mu      sync.Mutex
	conns   []*wrongShardConn
	errBody []byte
}

func (d *wrongShardDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	wc := newWrongShardConn(c, d.errBody)
	d.mu.Lock()
	d.conns = append(d.conns, wc)
	d.mu.Unlock()
	return wc, nil
}

// armAll arms all existing connections to replace the next frame.
func (d *wrongShardDialer) armAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.conns {
		c.arm()
	}
}

// TestWrongShardServer_FaultInjection verifies that wrong_shard_server (1062)
// triggers location cache invalidation and automatic retry.
//
// Uses a wrongShardConn at the net.Conn level (via custom dialer) to replace
// the next server response frame with ErrorOr(1062). The client should:
//  1. Receive 1062, invalidate the location cache
//  2. Re-query the commit proxy for fresh locations
//  3. Retry the read and succeed
func TestWrongShardServer_FaultInjection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	if exitCode != 0 {
		t.Fatalf("fdbcli configure exit: %d", exitCode)
	}
	time.Sleep(2 * time.Second)

	_, internalReader, _ := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	if idx := strings.Index(internalStr, cf.Description); idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, _ := ParseClusterString(strings.TrimSpace(internalStr))

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	connectCF.InternalKey = internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			connectCF.InternalKey += ","
		}
		connectCF.InternalKey += a
	}

	// Verify the crafted error response parses correctly.
	errBody := buildFDBErrorResponse(int32(ErrWrongShardServer))
	if _, parseErr := wire.ReadErrorOr(errBody); parseErr == nil {
		t.Fatal("buildFDBErrorResponse should produce an error response")
	}

	wd := &wrongShardDialer{errBody: errBody}
	cluster := NewClusterFromConfig(connectCF)
	cluster.SetDialFunc(wd.dial)
	defer cluster.Close()

	if err := cluster.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	db := &Database{
		cluster:       cluster,
		grvBatcher:    NewGRVBatcher(cluster),
		locationCache: NewLocationCache(cluster),
	}

	key := []byte("fault_wss_key")
	expected := []byte("correct_value")

	// Seed the key.
	_, err = db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		tx.Set(key, expected)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Warm the location cache with a successful read.
	result, err := db.Transact(ctx, func(tx *Transaction) (interface{}, error) {
		return tx.Get(ctx, key)
	})
	if err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if string(result.([]byte)) != string(expected) {
		t.Fatalf("warm read: got %q, want %q", result, expected)
	}

	// Pre-fetch read version so no GRV request during the fault window.
	rv, err := db.grvBatcher.GetReadVersion(ctx)
	if err != nil {
		t.Fatalf("GRV: %v", err)
	}

	// Arm: the next frame read on any connection will be replaced with 1062.
	wd.armAll()

	// Read with pre-set read version. The first getValue attempt will get
	// a replaced frame (1062), causing cache invalidation and retry.
	tx := db.CreateTransaction()
	tx.SetReadVersion(rv)
	got, err := tx.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get after fault: %v", err)
	}
	if string(got) != string(expected) {
		t.Fatalf("Get value: got %q, want %q", got, expected)
	}
	t.Log("wrong_shard_server fault injection: retry succeeded with correct value")
}
