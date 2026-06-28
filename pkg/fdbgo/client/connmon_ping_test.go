package client

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// TestConnectionMonitor_PingReplyArrives verifies that the FDB server
// actually responds to our outbound PingRequest. This confirms the wire
// format produced by buildPingRequest is valid for the server (not just
// our own extractPingReplyToken).
//
// Strategy: get a live connection, send a PING via PrepareReply, wait
// for the reply channel to deliver.
func TestConnectionMonitor_PingReplyArrives(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Find a live connection from the pool.
	db.db.connMu.RLock()
	var conn *transport.Conn
	for _, c := range db.db.connPool {
		if !c.IsClosed() {
			conn = c
			break
		}
	}
	db.db.connMu.RUnlock()

	if conn == nil {
		t.Fatal("no live connection in pool")
	}

	// Send a PING and wait for the reply.
	replyToken, replyCh, replyHandle := conn.PrepareReply()
	defer replyHandle.Release()
	pingEP := transport.WellKnownToken(transport.WLTokenPingPacket)
	body := transport.BuildPingRequest(replyToken)
	if err := conn.SendFrame(pingEP, body); err != nil {
		replyHandle.Cancel()
		t.Fatalf("SendFrame: %v", err)
	}

	// Wait up to 5s for the server to respond.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case resp := <-replyCh:
		if resp.Err != nil {
			t.Fatalf("PING reply error: %v", resp.Err)
		}
		t.Logf("PING reply arrived: %d bytes", len(resp.Body))
	case <-timer.C:
		replyHandle.Cancel()
		t.Fatal("PING reply did not arrive within 5s — server may not understand our PingRequest format")
	}
}
