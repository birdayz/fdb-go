package client

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
	"github.com/zeebo/xxh3"
)

// TestCoordinatorBootstrap connects to a real FDB testcontainer,
// sends OpenDatabaseCoordRequest, and validates the response.
func TestCoordinatorBootstrap(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start FDB testcontainer with version matching our wire protocol (7.3.75).
	container, err := tcfdb.Run(ctx, "")
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	defer container.Terminate(ctx)

	// Get connection string.
	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}
	t.Logf("cluster connection string: %s", connStr)

	// Parse cluster connection string.
	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster string: %v", err)
	}
	t.Logf("coordinators: %v", cf.Coordinators)

	// Configure the cluster.
	exitCode, _, err := container.Exec(ctx, []string{
		"fdbcli", "--exec", "configure new single ssd; status",
	})
	t.Logf("fdbcli configure+status exit: %d, err: %v", exitCode, err)
	time.Sleep(2 * time.Second)

	// Read the INTERNAL cluster file from the container.
	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		t.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	idx := strings.Index(internalStr, cf.Description)
	if idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalConnStr := strings.TrimSpace(internalStr)
	t.Logf("internal cluster file: %q (raw len=%d)", internalConnStr, len(internalBytes))

	internalCF, err := ParseClusterString(internalConnStr)
	if err != nil {
		t.Logf("parse internal cluster string: %v, falling back to external", err)
		internalCF = cf
	}

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	internalClusterKey := internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			internalClusterKey += ","
		}
		internalClusterKey += a
	}
	t.Logf("internal cluster key: %s", internalClusterKey)
	connectCF.InternalKey = internalClusterKey

	// Create database and connect.
	db, err := openDatabaseFromConfig(ctx, connectCF, nil)
	if err != nil {
		// If bootstrap fails, try raw coordinator exchange for debugging.
		t.Logf("openDatabaseFromConfig failed: %v", err)
		t.Logf("Attempting raw coordinator exchange for debugging...")
		debugCoordinatorExchange(t, ctx, cf)
		t.FailNow()
	}
	defer db.Close()

	// Validate the result.
	dbInfo := db.db.dbInfo.Load()
	if dbInfo == nil {
		t.Fatal("dbInfo is nil after successful bootstrap")
	}

	t.Logf("GRV proxies: %d", len(dbInfo.GRVProxies))
	for i, p := range dbInfo.GRVProxies {
		t.Logf("  GRV proxy %d: addr=%s token=%x:%x", i, p.Address, p.Token.First, p.Token.Second)
	}

	t.Logf("Commit proxies: %d", len(dbInfo.CommitProxies))
	for i, p := range dbInfo.CommitProxies {
		t.Logf("  Commit proxy %d: addr=%s token=%x:%x", i, p.Address, p.Token.First, p.Token.Second)
	}

	if len(dbInfo.GRVProxies) == 0 {
		t.Error("expected at least 1 GRV proxy")
	}
	if len(dbInfo.CommitProxies) == 0 {
		t.Error("expected at least 1 commit proxy")
	}

	// Try location lookup
	t.Log("Attempting GetKeyServerLocations...")
	servers, locErr := db.db.locCache.locate(db.db, ctx, []byte("test_key"))
	if locErr != nil {
		t.Logf("Locate: %v", locErr)
	} else {
		t.Logf("Locate: %d servers", len(servers))
		for i, s := range servers {
			t.Logf("  server %d: %s token=%x:%x", i, s.Address, s.Token.First, s.Token.Second)
		}
	}

	// Write a test key via C binding
	fdb.MustAPIVersion(720)
	tmpFile, _ := os.CreateTemp("", "fdb-*.cluster")
	tmpFile.WriteString(connStr)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())
	cdb, cErr := fdb.OpenDatabase(tmpFile.Name())
	if cErr != nil {
		t.Logf("C binding: %v", cErr)
	} else {
		_, txErr := cdb.Transact(func(tx fdb.Transaction) (any, error) {
			tx.Set(fdb.Key("test_key"), []byte("hello_from_go"))
			return nil, nil
		})
		t.Logf("C binding write: %v", txErr)
	}

	// Try GRV — GetReadVersion from the GRV proxy
	t.Log("Attempting GetReadVersion...")
	version, err := db.db.grvBatcher.getReadVersion(db.db, ctx)
	if err != nil {
		t.Logf("GetReadVersion: %v", err)
	} else {
		t.Logf("GetReadVersion: version=%d", version)
		if version <= 0 {
			t.Errorf("expected positive version, got %d", version)
		}
	}

	// Try GetValue — read the key we wrote via C binding
	if version > 0 && len(dbInfo.GRVProxies) > 0 {
		t.Log("Attempting GetValue for 'test_key'...")
		tx := db.CreateTransaction()
		tx.readVersion = version
		tx.hasReadVersion = true

		val, err := tx.getValue(ctx, []byte("test_key"))
		if err != nil {
			t.Logf("GetValue: %v", err)
		} else {
			t.Logf("GetValue: key=test_key value=%q", string(val))
			if string(val) != "hello_from_go" {
				t.Errorf("expected 'hello_from_go', got %q", string(val))
			}
		}
	}

	// Test WRITE path: Go client writes, C binding reads back.
	if len(dbInfo.CommitProxies) > 0 && cErr == nil {
		t.Log("Attempting Go client write...")

		// Write via Go client.
		writeTx := db.CreateTransaction()
		rv, err := db.db.grvBatcher.getReadVersion(db.db, ctx)
		if err != nil {
			t.Fatalf("GRV for write: %v", err)
		}
		writeTx.readVersion = rv
		writeTx.hasReadVersion = true
		writeTx.Set([]byte("go_native_key"), []byte("written_by_go"))
		err = writeTx.Commit(ctx)
		if err != nil {
			t.Fatalf("Go client commit: %v", err)
		}
		committedVer, _ := writeTx.GetCommittedVersion()
		t.Logf("Go client committed at version %d", committedVer)

		// Read back via C binding.
		cVal, readErr := cdb.Transact(func(tx fdb.Transaction) (any, error) {
			return tx.Get(fdb.Key("go_native_key")).Get()
		})
		if readErr != nil {
			t.Fatalf("C binding read: %v", readErr)
		}
		got := string(cVal.([]byte))
		t.Logf("C binding read: go_native_key=%q", got)
		if got != "written_by_go" {
			t.Errorf("expected 'written_by_go', got %q", got)
		}

		// Also verify via Go client read.
		readTx := db.CreateTransaction()
		rv2, _ := db.db.grvBatcher.getReadVersion(db.db, ctx)
		readTx.readVersion = rv2
		readTx.hasReadVersion = true
		val2, err := readTx.getValue(ctx, []byte("go_native_key"))
		if err != nil {
			t.Fatalf("Go read-back: %v", err)
		}
		t.Logf("Go read-back: go_native_key=%q", string(val2))
		if string(val2) != "written_by_go" {
			t.Errorf("Go read-back: expected 'written_by_go', got %q", string(val2))
		}

		// Test Transact API — the standard way to use FDB.
		t.Log("Testing Transact API...")
		_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte("transact_key"), []byte("transact_value"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("Transact write: %v", err)
		}
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			val, err := tx.Get(ctx, []byte("transact_key"))
			return string(val), err
		})
		if err != nil {
			t.Fatalf("Transact read: %v", err)
		}
		if result.(string) != "transact_value" {
			t.Errorf("Transact: expected 'transact_value', got %q", result)
		}
		t.Logf("Transact API: write+read OK, value=%q", result)

		// Test MVCC conflict detection: two transactions reading+writing same key.
		t.Log("Testing MVCC conflict detection...")
		tx1 := db.CreateTransaction()
		tx2 := db.CreateTransaction()
		// Both get the same read version.
		sharedRV, _ := db.db.grvBatcher.getReadVersion(db.db, ctx)
		tx1.readVersion = sharedRV
		tx1.hasReadVersion = true
		tx2.readVersion = sharedRV
		tx2.hasReadVersion = true
		// Both read the same key (adds read conflict range).
		_, _ = tx1.Get(ctx, []byte("conflict_key"))
		_, _ = tx2.Get(ctx, []byte("conflict_key"))
		// Both write the same key.
		tx1.Set([]byte("conflict_key"), []byte("from_tx1"))
		tx2.Set([]byte("conflict_key"), []byte("from_tx2"))
		// First commit should succeed.
		err = tx1.Commit(ctx)
		if err != nil {
			t.Fatalf("tx1 commit: %v", err)
		}
		t.Logf("tx1 committed at version %d", tx1.committedVersion)
		// Second commit should get not_committed (1020).
		err = tx2.Commit(ctx)
		if err == nil {
			t.Error("tx2 should have conflicted but succeeded")
		} else {
			t.Logf("tx2 conflict (expected): %v", err)
			var fdbErr *wire.FDBError
			if errors.As(err, &fdbErr) && fdbErr.Code == ErrNotCommitted {
				t.Log("MVCC conflict detection working!")
			} else {
				t.Logf("unexpected error type: %T %v", err, err)
			}
		}
	}
}

// TestGetRange tests the range read path with a dedicated container.
func TestGetRange(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "")
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

	// Configure cluster.
	exitCode, _, _ := container.Exec(ctx, []string{"fdbcli", "--exec", "configure new single ssd"})
	t.Logf("fdbcli configure exit: %d", exitCode)
	time.Sleep(2 * time.Second)

	// Read internal cluster file.
	_, internalReader, _ := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := strings.TrimSpace(string(internalBytes))
	idx := strings.Index(internalStr, cf.Description)
	if idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalCF, _ := ParseClusterString(strings.TrimSpace(internalStr))

	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators,
	}
	internalClusterKey := internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			internalClusterKey += ","
		}
		internalClusterKey += a
	}
	connectCF.InternalKey = internalClusterKey

	db, err := openDatabaseFromConfig(ctx, connectCF, nil)
	if err != nil {
		t.Fatalf("openDatabaseFromConfig: %v", err)
	}
	defer db.Close()

	// Enable debug tracing on ALL connections.
	db.db.connMu.RLock()
	for _, conn := range db.db.connPool {
		conn.SetDebug(true)
	}
	db.db.connMu.RUnlock()

	dbInfo := db.db.dbInfo.Load()
	t.Logf("Connected! GRV proxies=%d commit proxies=%d", len(dbInfo.GRVProxies), len(dbInfo.CommitProxies))
	if len(dbInfo.GRVProxies) > 0 {
		t.Logf("GRV proxy: %s", dbInfo.GRVProxies[0].Address)
	}
	if len(dbInfo.CommitProxies) > 0 {
		t.Logf("Commit proxy: %s", dbInfo.CommitProxies[0].Address)
	}

	// Write keys via C binding (avoids Go commit server crashes for now).
	fdb.MustAPIVersion(720)
	tmpFile, _ := os.CreateTemp("", "fdb-*.cluster")
	tmpFile.WriteString(connStr)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())
	cdb, cErr := fdb.OpenDatabase(tmpFile.Name())
	if cErr != nil {
		t.Fatalf("C binding open: %v", cErr)
	}
	_, err = cdb.Transact(func(tx fdb.Transaction) (any, error) {
		tx.Set(fdb.Key("range_a"), []byte("value_a"))
		tx.Set(fdb.Key("range_b"), []byte("value_b"))
		tx.Set(fdb.Key("range_c"), []byte("value_c"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("C binding write: %v", err)
	}
	t.Log("wrote 3 keys via C binding")
	time.Sleep(1 * time.Second) // ensure version advances past the commit

	// Range read via Go client.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte("range_"), []byte("range_~"), 100)
		return []any{kvs, more}, err
	})
	if err != nil {
		t.Fatalf("range read: %v", err)
	}

	kvs := result.([]any)[0].([]KeyValue)
	more := result.([]any)[1].(bool)
	t.Logf("GetRange: %d keys, more=%v", len(kvs), more)
	for _, kv := range kvs {
		t.Logf("  %s = %s", kv.Key, kv.Value)
	}

	if len(kvs) != 3 {
		t.Errorf("expected 3 keys, got %d", len(kvs))
	}
	if more {
		t.Error("expected more=false")
	}
	expected := map[string]string{"range_a": "value_a", "range_b": "value_b", "range_c": "value_c"}
	for _, kv := range kvs {
		if exp, ok := expected[string(kv.Key)]; ok {
			if string(kv.Value) != exp {
				t.Errorf("%s: got %q, want %q", kv.Key, kv.Value, exp)
			}
		}
	}
}

// debugCoordinatorExchange does a raw TCP exchange to see exact bytes.
func debugCoordinatorExchange(t *testing.T, ctx context.Context, cf *ClusterFile) {
	t.Helper()

	addr := cf.Coordinators[0]
	t.Logf("raw TCP exchange with %s", addr)

	// Raw TCP connect.
	var d net.Dialer
	rawConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Logf("dial: %v", err)
		return
	}
	defer rawConn.Close()

	// Send ConnectPacket.
	connID := uint64(0x1234567890ABCDEF)
	if err := transport.WriteConnectPacket(rawConn, rawConn.LocalAddr(), connID); err != nil {
		t.Logf("write connect packet: %v", err)
		return
	}
	t.Logf("sent ConnectPacket")

	// Read peer's ConnectPacket.
	peerPkt, err := transport.ReadConnectPacket(rawConn)
	if err != nil {
		t.Logf("read connect packet: %v", err)
		return
	}
	t.Logf("peer ConnectPacket: version=%#016x (with flag), stripped=%#016x",
		peerPkt.ProtocolVersion,
		peerPkt.ProtocolVersion&^transport.ObjectSerializerFlag)
	t.Logf("peer has ObjectSerializer: %v", peerPkt.HasObjectSerializerFlag())

	// Read the initial PING from the server (connection keepalive init).
	rawConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var lenBuf0 [4]byte
	if _, err := io.ReadFull(rawConn, lenBuf0[:]); err == nil {
		pktLen0 := binary.LittleEndian.Uint32(lenBuf0[:])
		t.Logf("initial frame: packetLen=%d", pktLen0)
		rest := make([]byte, 8+int(pktLen0))
		io.ReadFull(rawConn, rest)

		if int(pktLen0) >= 16 {
			tok1 := binary.LittleEndian.Uint64(rest[8:])
			tok2 := binary.LittleEndian.Uint64(rest[16:])
			if tok1 == 0xFFFFFFFFFFFFFFFF && tok2 == 1 {
				t.Logf("initial frame is PING (as expected)")
			} else {
				t.Logf("initial frame token: %016x:%016x", tok1, tok2)
			}
		}
	} else {
		t.Logf("no initial frame: %v", err)
	}
	rawConn.SetReadDeadline(time.Time{})

	time.Sleep(500 * time.Millisecond)

	replyToken := transport.NewUID()
	body := buildOpenDatabaseCoordRequest(cf, replyToken)
	t.Logf("request body (%d bytes)", len(body))
	t.Logf("reply token: %016x:%016x", replyToken.First, replyToken.Second)

	for _, testTokenID := range []int{100, transport.WLTokenClientLeaderRegOpenDatabase} {
		t.Logf("--- Trying token ID %d ---", testTokenID)

		rawConn.Close()
		rawConn, err = d.DialContext(ctx, "tcp", addr)
		if err != nil {
			t.Logf("redial: %v", err)
			continue
		}
		transport.WriteConnectPacket(rawConn, rawConn.LocalAddr(), connID+uint64(testTokenID))
		if _, err := transport.ReadConnectPacket(rawConn); err != nil {
			t.Logf("rehandshake: %v", err)
			continue
		}
		rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		pingBuf := make([]byte, 256)
		rawConn.Read(pingBuf)
		rawConn.SetReadDeadline(time.Time{})
		time.Sleep(200 * time.Millisecond)

		replyToken = transport.NewUID()
		body = buildOpenDatabaseCoordRequest(cf, replyToken)

		destToken := transport.WellKnownToken(testTokenID)
		payloadLen2 := 16 + len(body)
		frame2 := make([]byte, 4+8+payloadLen2)
		binary.LittleEndian.PutUint32(frame2[0:], uint32(payloadLen2))
		binary.LittleEndian.PutUint64(frame2[12:], destToken.First)
		binary.LittleEndian.PutUint64(frame2[20:], destToken.Second)
		copy(frame2[28:], body)
		checksum2 := xxh3.Hash(frame2[12 : 12+payloadLen2])
		binary.LittleEndian.PutUint64(frame2[4:], checksum2)
		rawConn.Write(frame2)

		rawConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var lenBuf2 [4]byte
		_, err = io.ReadFull(rawConn, lenBuf2[:])
		if err != nil {
			t.Logf("token %d: %v after sending", testTokenID, err)
		} else {
			pktLen2 := binary.LittleEndian.Uint32(lenBuf2[:])
			t.Logf("token %d: got response frame packetLen=%d", testTokenID, pktLen2)
		}
	}
}

// TestProbeWellKnownTokens probes different well-known token IDs on a single
// TCP connection to find which endpoints the coordinator has registered.
func TestProbeWellKnownTokens_WIP(t *testing.T) {
	t.Skip("WIP: debugging coordinator bootstrap")
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcfdb.Run(ctx, "")
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

	addr := cf.Coordinators[0]

	exitCode, _, err := container.Exec(ctx, []string{
		"fdbcli", "--exec", "configure new single ssd; status",
	})
	t.Logf("fdbcli exit: %d err: %v", exitCode, err)
	time.Sleep(3 * time.Second)

	for tokenID := 2; tokenID <= 15; tokenID++ {
		func(tid int) {
			var d net.Dialer
			rawConn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				t.Logf("token %d: dial failed: %v", tid, err)
				return
			}
			defer rawConn.Close()

			connID := uint64(0x1234567890ABCDEF + uint64(tid))
			transport.WriteConnectPacket(rawConn, rawConn.LocalAddr(), connID)
			if _, err := transport.ReadConnectPacket(rawConn); err != nil {
				t.Logf("token %d: handshake failed: %v", tid, err)
				return
			}

			replyToken := transport.NewUID()
			body := buildOpenDatabaseCoordRequest(cf, replyToken)

			destToken := transport.WellKnownToken(tid)
			payloadLen := 16 + len(body)
			frame := make([]byte, 4+8+payloadLen)
			binary.LittleEndian.PutUint32(frame[0:], uint32(payloadLen))
			binary.LittleEndian.PutUint64(frame[12:], destToken.First)
			binary.LittleEndian.PutUint64(frame[20:], destToken.Second)
			copy(frame[28:], body)
			checksum := xxh3.Hash(frame[12 : 12+payloadLen])
			binary.LittleEndian.PutUint64(frame[4:], checksum)

			rawConn.Write(frame)

			rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			resp := make([]byte, 8192)
			n, err := rawConn.Read(resp)
			if err != nil {
				t.Logf("token %d: no response: %v", tid, err)
				return
			}

			if n >= 28 {
				respFirst := binary.LittleEndian.Uint64(resp[12:])
				respSecond := binary.LittleEndian.Uint64(resp[20:])

				if respFirst == 0xFFFFFFFFFFFFFFFF && respSecond == 1 {
					t.Logf("token %d: PING (endpoint not found)", tid)
				} else if respFirst == replyToken.First && respSecond == replyToken.Second {
					t.Logf("token %d: *** REPLY *** (%d body bytes)", tid, n-28)
					if n-28 <= 200 {
						t.Logf("  body: %s", hex.EncodeToString(resp[28:n]))
					}
				} else {
					t.Logf("token %d: resp token %016x:%016x (%d bytes)", tid, respFirst, respSecond, n)
				}
			} else {
				t.Logf("token %d: short (%d bytes)", tid, n)
			}
		}(tokenID)
	}
}

// Ensure xxh3 is used.
var _ = xxh3.Hash
