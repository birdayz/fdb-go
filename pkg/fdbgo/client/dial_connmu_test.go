package client

import (
	"context"
	"errors"
	"net"
	"sync"
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
		dialFn: func(ctx context.Context, _, _ string) (net.Conn, error) {
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
