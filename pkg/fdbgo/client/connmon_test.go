package client

import (
	"context"
	"testing"
	"time"
)

// TestConnectionMonitor_BytesReceived verifies that the bytesReceived counter
// is incremented by real FDB traffic, proving the connection monitor's
// dead-connection detection mechanism works.
func TestConnectionMonitor_BytesReceived(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Get a connection from the pool.
	info := db.GetDBInfo()
	if info == nil || len(info.GRVProxies) == 0 {
		t.Skip("no proxy connections available")
	}

	// Find a connection in the pool.
	db.db.connMu.RLock()
	var totalBytes int64
	for addr, conn := range db.db.connPool {
		b := conn.BytesReceived()
		if b > 0 {
			t.Logf("conn %s: %d bytes received", addr, b)
			totalBytes += b
		}
	}
	db.db.connMu.RUnlock()

	// After bootstrap + initial GRV, we should have received some bytes.
	if totalBytes == 0 {
		t.Error("expected bytesReceived > 0 after bootstrap")
	}

	// Do a transaction to generate more traffic.
	key := []byte(t.Name() + "_key")
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(key, []byte("v"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Transact: %v", err)
	}

	// Verify bytes increased.
	db.db.connMu.RLock()
	var newTotal int64
	for _, conn := range db.db.connPool {
		newTotal += conn.BytesReceived()
	}
	db.db.connMu.RUnlock()

	if newTotal <= totalBytes {
		t.Errorf("bytesReceived did not increase after transaction: before=%d, after=%d", totalBytes, newTotal)
	}
	t.Logf("bytesReceived: before=%d, after=%d (delta=%d)", totalBytes, newTotal, newTotal-totalBytes)
}

// TestConnectionMonitor_SurvivesIdleConnection verifies that idle connections
// (no pending requests) are NOT killed by the connection monitor. The monitor
// only activates when there are pending requests.
func TestConnectionMonitor_SurvivesIdleConnection(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	db := openTestDB(t, ctx)
	defer db.Close()

	// Do an initial transaction to warm up connections.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(t.Name()+"_warmup"), []byte("v"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("warmup: %v", err)
	}

	// Wait 5 seconds — longer than a full PING cycle (750ms + 750ms + 2s = 3.5s).
	// If the monitor incorrectly kills idle connections, the next tx will fail.
	time.Sleep(5 * time.Second)

	// Verify the connection is still alive by doing another transaction.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		return tx.Get(ctx, []byte(t.Name()+"_warmup"))
	})
	if err != nil {
		t.Fatalf("post-idle transaction failed (connection monitor may have killed idle connection): %v", err)
	}
}
