package otel

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenFDBBootstrapCancellationAfterConnect(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	acceptErrors := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			acceptErrors <- err
			return
		}
		accepted <- conn
	}()
	path := filepath.Join(t.TempDir(), "blackhole.cluster")
	if err := os.WriteFile(path, []byte("otelbootstrap:cancel283@"+listener.Addr().String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		db, err := OpenFDBContext(ctx, testConfig(), path)
		if db != nil {
			db.Close()
		}
		result <- err
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case err := <-acceptErrors:
		t.Fatal(err)
	case err := <-result:
		t.Fatalf("bootstrap returned before connecting: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("FDB bootstrap never connected to local coordinator")
	}
	defer conn.Close()
	// An accepted, silent TCP coordinator proves bootstrap is active, not merely
	// the adapter's precanceled-context guard. No server response can finish it.
	started := time.Now()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active bootstrap cancellation: %v", err)
		}
		if time.Since(started) > 2*time.Second {
			t.Fatal("bootstrap used its independent timeout instead of caller cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active FDB bootstrap ignored caller cancellation")
	}
}
