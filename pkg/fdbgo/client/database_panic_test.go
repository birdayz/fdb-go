package client

import (
	"context"
	"net"
	"testing"

	"fdb.dev/pkg/fdbgo/transport"
)

func panicDialFn(context.Context, string, string) (net.Conn, error) {
	panic("boom in dial")
}

// TestDialAndPool_RecoverUnpoisonsSingleflight pins RFC-110 Class B (codex P2): a
// panic in transport.Dial must not abort the host. The dialAndPool backstop must
// wake waiters with an error AND remove the singleflight entry, so the next
// caller re-dials. Without it, call.done would never close (waiters hang on a
// never-closed channel) and db.dialing[addr] would stay forever (no fresh dial).
func TestDialAndPool_RecoverUnpoisonsSingleflight(t *testing.T) {
	t.Parallel()
	const addr = "127.0.0.1:4500"
	db := &database{
		ctx:      context.Background(),
		dialing:  make(map[string]*dialCall),
		connPool: make(map[string]*transport.Conn),
		dialFn:   panicDialFn,
	}
	call := &dialCall{done: make(chan struct{})}
	db.dialing[addr] = call

	db.dialAndPool(addr, call) // must return, not crash the process

	select {
	case <-call.done:
	default:
		t.Fatal("call.done not closed — every waiter on this dial would hang forever")
	}
	if call.err == nil {
		t.Fatal("call.err nil — a panicked dial would look like a successful one")
	}
	db.connMu.Lock()
	_, poisoned := db.dialing[addr]
	db.connMu.Unlock()
	if poisoned {
		t.Fatal("db.dialing entry left behind — poisoned singleflight, no caller ever re-dials")
	}
	if got := db.metrics.Snapshot().RecoveredPanics; got != 1 {
		t.Fatalf("RecoveredPanics = %d, want 1", got)
	}
}

// TestTryOneCoordinator_PanicReturnsError pins RFC-110 Class B (codex P3): the
// single-coordinator fast path calls tryOneCoordinator directly on the caller's
// goroutine, so a panic in its dial path must surface as a returned error, never
// a host crash.
func TestTryOneCoordinator_PanicReturnsError(t *testing.T) {
	t.Parallel()
	db := &database{
		ctx:      context.Background(),
		dialing:  make(map[string]*dialCall),
		connPool: make(map[string]*transport.Conn),
		dialFn:   panicDialFn,
	}
	snap := &ClusterFile{Coordinators: []string{"127.0.0.1:4500"}}
	if _, err := db.tryOneCoordinator(context.Background(), snap, "127.0.0.1:4500"); err == nil {
		t.Fatal("tryOneCoordinator returned nil err for a panicking dial — the fast path would crash the host")
	}
}
