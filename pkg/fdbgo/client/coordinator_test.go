package client

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
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
	container, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
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
	// The coordinator compares req.clusterKey with its own cluster key,
	// so we must use the INTERNAL address (not the external socat address).
	_, internalReader, err := container.Exec(ctx, []string{"cat", "/var/fdb/fdb.cluster"})
	if err != nil {
		t.Fatalf("read internal cluster file: %v", err)
	}
	internalBytes, _ := io.ReadAll(internalReader)
	internalStr := string(internalBytes)
	// Docker multiplexer prepends 8-byte binary header per chunk.
	// Find the actual cluster string by looking for the known pattern.
	idx := strings.Index(internalStr, cf.Description)
	if idx >= 0 {
		internalStr = internalStr[idx:]
	}
	internalConnStr := strings.TrimSpace(internalStr)
	t.Logf("internal cluster file: %q (raw len=%d)", internalConnStr, len(internalBytes))

	// Parse internal cluster file for the cluster key
	internalCF, err := ParseClusterString(internalConnStr)
	if err != nil {
		t.Logf("parse internal cluster string: %v, falling back to external", err)
		internalCF = cf
	}

	// The request clusterKey must use the INTERNAL addresses. Store both.
	// Create a ClusterFile with external addresses for TCP, internal for clusterKey.
	connectCF := &ClusterFile{
		Description:  internalCF.Description,
		ID:           internalCF.ID,
		Coordinators: cf.Coordinators, // external (socat) for TCP connection
	}
	// Store the internal connection string for the request's clusterKey field
	internalClusterKey := internalCF.Description + ":" + internalCF.ID + "@"
	for i, a := range internalCF.Coordinators {
		if i > 0 {
			internalClusterKey += ","
		}
		internalClusterKey += a
	}
	t.Logf("internal cluster key: %s", internalClusterKey)
	connectCF.InternalKey = internalClusterKey

	// Create cluster and connect.
	cluster := NewClusterFromConfig(connectCF)
	defer cluster.Close()

	err = cluster.Connect(ctx)
	if err != nil {
		// If Connect fails, let's try to see what happened at a lower level.
		// Connect to coordinator manually and dump the raw response.
		t.Logf("Connect failed: %v", err)
		t.Logf("Attempting raw coordinator exchange for debugging...")
		debugCoordinatorExchange(t, ctx, cf)
		t.FailNow()
	}

	// Validate the result.
	cluster.mu.RLock()
	dbInfo := cluster.dbInfo
	cluster.mu.RUnlock()

	if dbInfo == nil {
		t.Fatal("dbInfo is nil after successful Connect")
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

	// Try GRV — GetReadVersion from the GRV proxy
	t.Log("Attempting GetReadVersion...")
	batcher := NewGRVBatcher(cluster)
	version, err := batcher.GetReadVersion(ctx)
	if err != nil {
		t.Logf("GetReadVersion: %v", err)
	} else {
		t.Logf("GetReadVersion: version=%d", version)
		if version <= 0 {
			t.Errorf("expected positive version, got %d", version)
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
	// Then respond with our own PING to establish the connection fully.
	rawConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var lenBuf0 [4]byte
	if _, err := io.ReadFull(rawConn, lenBuf0[:]); err == nil {
		pktLen0 := binary.LittleEndian.Uint32(lenBuf0[:])
		t.Logf("initial frame: packetLen=%d", pktLen0)
		// Read rest of frame (checksum + payload)
		rest := make([]byte, 8+int(pktLen0))
		io.ReadFull(rawConn, rest)

		// Check if it's a PING
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
	rawConn.SetReadDeadline(time.Time{}) // clear deadline

	// Small delay to let server finish initialization
	time.Sleep(500 * time.Millisecond)

	// Build request.
	replyToken := transport.NewUID()
	body := buildOpenDatabaseCoordRequest(cf, replyToken)
	t.Logf("request body (%d bytes)", len(body))
	t.Logf("reply token: %016x:%016x", replyToken.First, replyToken.Second)

	// Try both the expected token (4) and a bogus token (100) to see
	// if the connection close is token-specific or universal.
	for _, testTokenID := range []int{100, transport.WLTokenClientLeaderRegOpenDatabase} {
		t.Logf("--- Trying token ID %d ---", testTokenID)

		// Reconnect for each attempt.
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
		// Drain PING
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

	addr := cf.Coordinators[0]

	// Ensure cluster is fully configured first.
	exitCode, _, err := container.Exec(ctx, []string{
		"fdbcli", "--exec", "configure new single ssd; status",
	})
	t.Logf("fdbcli exit: %d err: %v", exitCode, err)
	time.Sleep(3 * time.Second)

	// Use separate connections per probe to avoid interference.
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
