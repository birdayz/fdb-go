package client

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
	tcfdb "github.com/birdayz/fdb-record-layer-go/pkg/testcontainers/foundationdb"
)

// TestFDBServerLogs starts an FDB testcontainer, reads its logs, sends a raw
// OpenDatabaseCoordRequest frame to well-known token 4, then reads logs again
// to see if the server emits anything in response to our connection attempt.
func TestFDBServerLogs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Start FDB testcontainer.
	container, err := tcfdb.Run(ctx, "", tcfdb.WithVersion("7.3.75"))
	if err != nil {
		t.Fatalf("start FDB container: %v", err)
	}
	defer container.Terminate(ctx) //nolint:errcheck

	// --- Phase 1: read startup logs ---
	t.Log("=== Phase 1: container startup logs ===")
	printContainerLogs(t, ctx, container, "startup")

	// --- Phase 2: exec trace file listing inside the container ---
	t.Log("=== Phase 2: FDB trace files inside container ===")
	execAndLog(t, ctx, container, []string{
		"/bin/sh", "-c",
		`ls /var/fdb/logs/ 2>/dev/null && cat /var/fdb/logs/*.xml 2>/dev/null || ` +
			`ls /var/log/foundationdb/ 2>/dev/null && cat /var/log/foundationdb/*.xml 2>/dev/null || ` +
			`echo 'no trace files found'`,
	})

	// --- Phase 3: connect and send a frame to well-known token 4 ---
	t.Log("=== Phase 3: sending OpenDatabaseCoordRequest to token 4 ===")

	connStr, err := container.ClusterFile(ctx)
	if err != nil {
		t.Fatalf("get cluster file: %v", err)
	}
	t.Logf("cluster connection string: %s", connStr)

	cf, err := ParseClusterString(connStr)
	if err != nil {
		t.Fatalf("parse cluster string: %v", err)
	}
	t.Logf("coordinators: %v", cf.Coordinators)

	if len(cf.Coordinators) == 0 {
		t.Fatal("no coordinators in cluster file")
	}

	// Configure the database and verify with C binding.
	exitCode, _, err := container.Exec(ctx, []string{
		"fdbcli", "--exec", "configure new single ssd",
	})
	t.Logf("fdbcli configure exit: %d err: %v", exitCode, err)

	// Use the C binding to verify coordinator works through socat proxy.
	fdb.MustAPIVersion(720)
	tmpFile, _ := os.CreateTemp("", "fdb-cluster-*.cluster")
	tmpFile.WriteString(connStr)
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())
	cdb, cErr := fdb.OpenDatabase(tmpFile.Name())
	if cErr != nil {
		t.Logf("C binding OpenDatabase: %v", cErr)
	} else {
		_, txErr := cdb.Transact(func(tx fdb.Transaction) (any, error) {
			tx.Set(fdb.Key("_go_test"), []byte("ping"))
			return nil, nil
		})
		t.Logf("C binding transact: %v", txErr)
	}

	time.Sleep(2 * time.Second)

	addr := cf.Coordinators[0]
	t.Logf("dialing coordinator at %s", addr)

	// Dial using our transport (full handshake).
	conn, err := transport.Dial(ctx, addr, false)
	if err != nil {
		t.Logf("transport.Dial failed: %v", err)
		t.Log("(dial failure itself is interesting — check logs below)")
	} else {
		t.Logf("connected, peer protocol version: %#x", conn.PeerProtocolVersion())

		// Allocate reply token before building the request body so we can embed it.
		replyToken, replyCh, cancelReply := conn.PrepareReply()
		defer cancelReply()
		t.Logf("reply token allocated: %016x:%016x", replyToken.First, replyToken.Second)

		// Build the OpenDatabaseCoordRequest body.
		body := buildOpenDatabaseCoordRequest(cf, replyToken)
		t.Logf("built request body: %d bytes", len(body))

		// Send to well-known token 4 (WLTokenClientLeaderRegOpenDatabase).
		destToken := transport.WellKnownToken(transport.WLTokenClientLeaderRegOpenDatabase)
		t.Logf("sending to dest token: %016x:%016x", destToken.First, destToken.Second)

		if sendErr := conn.SendFrame(destToken, body); sendErr != nil {
			t.Logf("SendFrame error: %v", sendErr)
		} else {
			t.Log("frame sent successfully, waiting for reply (5s timeout)...")

			waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
			defer waitCancel()

			select {
			case resp := <-replyCh:
				if resp.Err != nil {
					t.Logf("reply error: %v", resp.Err)
				} else {
					t.Logf("received reply: %d bytes", len(resp.Body))
					if len(resp.Body) > 0 {
						t.Logf("reply body (hex, first 128 bytes): %x", truncate(resp.Body, 128))
					}
				}
			case <-waitCtx.Done():
				t.Logf("no reply within 5s: %v", waitCtx.Err())
			}
		}

		conn.Close() //nolint:errcheck

		// Give the server a moment to write error traces (FDB has write-buffered trace files).
		time.Sleep(3 * time.Second)
	}

	// --- Phase 4: read container logs again after our connection attempt ---
	t.Log("=== Phase 4: container logs after connection attempt ===")
	printContainerLogs(t, ctx, container, "post-connection")

	// --- Phase 5: grep trace files for endpoint/connection/error events ---
	t.Log("=== Phase 5: FDB trace events after connection (filtered) ===")
	execAndLog(t, ctx, container, []string{
		"/bin/sh", "-c",
		`grep -i 'Endpoint\|Undeliver\|Connection\|IncomingConn\|PacketError\|deserializ\|close\|error_code\|OpenDatabase\|endpoint_not\|checksum' /var/fdb/logs/*.xml 2>/dev/null | tail -50 || echo 'no matches'`,
	})

	// Also dump the LAST 30 lines of the trace file to catch anything we missed.
	t.Log("=== Phase 5b: last 30 trace events ===")
	execAndLog(t, ctx, container, []string{
		"/bin/sh", "-c",
		`tail -30 /var/fdb/logs/*.xml 2>/dev/null || echo 'no trace files'`,
	})
}

// printContainerLogs reads the container's combined stdout/stderr stream and
// logs the last 100 lines to the test output.
func printContainerLogs(t *testing.T, ctx context.Context, container *tcfdb.Container, label string) {
	t.Helper()

	logRC, err := container.Logs(ctx)
	if err != nil {
		t.Logf("[%s] container.Logs error: %v", label, err)
		return
	}
	defer logRC.Close() //nolint:errcheck

	all, err := io.ReadAll(logRC)
	if err != nil {
		t.Logf("[%s] read logs error: %v", label, err)
		return
	}

	lines := splitLines(string(all))

	const maxLines = 100
	start := 0
	if len(lines) > maxLines {
		start = len(lines) - maxLines
		t.Logf("[%s] (showing last %d of %d lines)", label, maxLines, len(lines))
	}

	for i, line := range lines[start:] {
		t.Logf("[%s] %4d: %s", label, start+i+1, line)
	}
}

// execAndLog runs a command in the container and logs its output.
func execAndLog(t *testing.T, ctx context.Context, container *tcfdb.Container, cmd []string) {
	t.Helper()

	exitCode, output, err := container.Exec(ctx, cmd)
	if err != nil {
		t.Logf("exec %v: error: %v", cmd, err)
		return
	}

	t.Logf("exec exit code: %d", exitCode)

	if output != nil {
		sc := bufio.NewScanner(output)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			t.Logf("  exec[%d]: %s", lineNo, sc.Text())
		}
		if scanErr := sc.Err(); scanErr != nil {
			t.Logf("  exec scan error: %v", scanErr)
		}
	}
}

// splitLines splits a string into non-empty lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		// Docker log multiplexer prepends 8-byte header per chunk; strip non-printable prefixes.
		line = strings.TrimRight(line, "\r")
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// truncate returns at most n bytes of b.
func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[:n]
}
