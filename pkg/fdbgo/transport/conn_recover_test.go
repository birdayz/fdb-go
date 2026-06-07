package transport

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
)

// TestRecoverLoop_ContainsPanicAndFailsConnection pins P0.2: a panic in a
// long-lived connection goroutine must be contained — not crash the host, since a
// request-goroutine recover cannot catch a panic raised on another goroutine's
// stack — and must fail the connection so callers retry/reroute, mirroring
// libfdb_c's connection_failed on a bad reply. See TODO-production.md P0.2.
func TestRecoverLoop_ContainsPanicAndFailsConnection(t *testing.T) {
	t.Parallel()
	var logged string
	orig := seriousLogf
	seriousLogf = func(format string, args ...any) { logged += fmt.Sprintf(format, args...) }
	t.Cleanup(func() { seriousLogf = orig })

	cli, srv := net.Pipe()
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Conn{ctx: ctx, cancel: cancel, conn: cli, pending: make(map[UID]chan Response)}
	pending := make(chan Response, 1)
	c.pending[UID{First: 1}] = pending

	// Reaching the assertions at all proves containment: without recoverLoop, this
	// closure's panic would propagate and crash the test goroutine.
	func() {
		defer c.recoverLoop("testLoop")
		panic("boom")
	}()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("recoverLoop did not fail the connection: ctx not cancelled after a contained panic")
	}
	select {
	case resp := <-pending:
		if resp.Err == nil {
			t.Fatal("pending reply must carry the failure error after connection teardown")
		}
	default:
		t.Fatal("pending reply was not failed on connection teardown")
	}
	if !strings.Contains(logged, "testLoop") || !strings.Contains(logged, "boom") {
		t.Errorf("recovered panic must be logged SERIOUS with loop + cause; got %q", logged)
	}
}
