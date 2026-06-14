package client

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

// TestGetOrDialConn_DialOutsideConnMu pins that getOrDialConn does NOT hold the
// connection-pool lock across the dial. Holding connMu across transport.Dial is a
// deadlock amplifier: one stalled dial (an unreachable proxy whose deadline timer
// is slow to fire under heavy load) freezes EVERY connection acquisition — pooled
// lookups and dials to healthy proxies alike — wedging the whole client. That was
// the root cause of an intermittent GRV hang under container contention.
//
// With the bug (Lock held across Dial), the connMu.Lock() probe below blocks for
// the full duration of the stalled dial and the test times out. With the fix,
// connMu is free while the dial is in flight.
func TestGetOrDialConn_DialOutsideConnMu(t *testing.T) {
	t.Parallel()

	dialing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	db := &database{
		connPool: make(map[string]*transport.Conn),
		dialing:  make(map[string]*dialCall),
		dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
			once.Do(func() { close(dialing) })
			<-release // stall the dial while we probe connMu
			return nil, errors.New("test: dial aborted")
		},
	}

	go func() { _, _, _ = db.getOrDialConn(context.Background(), "stalled:1") }()
	<-dialing // a dial is now in flight

	locked := make(chan struct{})
	go func() {
		db.connMu.Lock()
		_ = len(db.connPool) // touch the connMu-protected state, then release
		db.connMu.Unlock()
		close(locked)
	}()

	select {
	case <-locked:
		// connMu was free during the dial — the fix holds.
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("connMu held across transport.Dial — a stalled dial wedges all connection acquisition")
	}
	close(release)
}

// TestGetOrDialConn_CoalescesConcurrentDials pins the singleflight: a burst of
// concurrent misses to the SAME cold address must run ONE dial, not O(requests).
// Without coalescing, a startup/reconnect burst to one proxy opens a redundant
// socket + TCP/TLS/ConnectPacket handshake per goroutine — defeating the pool and
// risking FD exhaustion under the very load this guards. Revert-proven: against the
// pre-coalescing code all callers dial (calls == N) and this fails.
func TestGetOrDialConn_CoalescesConcurrentDials(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	db := &database{
		connPool: make(map[string]*transport.Conn),
		dialing:  make(map[string]*dialCall),
		dialFn: func(_ context.Context, _, _ string) (net.Conn, error) {
			calls.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release // hold the dial in flight so the others must coalesce
			return nil, errors.New("test: dial done")
		},
	}

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup

	// First caller owns the dial; it registers db.dialing[addr] before dialing.
	wg.Add(1)
	go func() { defer wg.Done(); _, _, errs[0] = db.getOrDialConn(context.Background(), "cold:1") }()
	<-entered // dial in flight; db.dialing["cold:1"] is set

	// The rest target the SAME address → must coalesce onto the in-flight dial.
	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _, errs[i] = db.getOrDialConn(context.Background(), "cold:1") }(i)
	}

	// Give the coalescing callers time to reach the wait. If any had dialed instead
	// of coalescing, calls would exceed 1.
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		close(release)
		t.Fatalf("a same-address burst must run exactly 1 dial, got %d (coalescing broken)", got)
	}

	close(release)
	wg.Wait()
	for i, e := range errs {
		if e == nil {
			t.Fatalf("caller %d got nil error from a failed dial (should have seen the shared dial error)", i)
		}
	}
}
