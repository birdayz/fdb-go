package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// ============================================================================
// installProxySet — apply + broadcast contract.
// ============================================================================

func TestApplyDBInfo_FirstApplyBroadcasts(t *testing.T) {
	t.Parallel()
	db := &database{proxiesChanged: make(chan struct{})}
	ch := db.waitProxiesChanged()

	info := &DBInfo{
		GRVProxies:    []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}}},
		CommitProxies: []ProxyInfo{{Address: "10.0.0.2:4500", Token: transport.UID{First: 2}}},
	}
	if !db.installProxySet(info) {
		t.Fatal("first installProxySet should report change=true")
	}
	if got := db.dbInfo.Load(); got != info {
		t.Errorf("dbInfo not stored: got %v, want %v", got, info)
	}
	select {
	case <-ch:
		// ok — broadcast fired
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first apply must close the proxiesChanged channel")
	}
	// A fresh channel must replace the closed one.
	if next := db.waitProxiesChanged(); next == ch {
		t.Error("broadcast must replace the channel, but old chan was returned")
	}
}

func TestApplyDBInfo_NoChangeNoBroadcast(t *testing.T) {
	t.Parallel()
	db := &database{proxiesChanged: make(chan struct{})}
	info := &DBInfo{GRVProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}}}}
	db.dbInfo.Store(info)

	ch := db.waitProxiesChanged()
	// Apply an equal (but distinct pointer) DBInfo — must NOT broadcast.
	dup := &DBInfo{GRVProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}}}}
	if db.installProxySet(dup) {
		t.Error("equal DBInfo must report change=false")
	}
	select {
	case <-ch:
		t.Error("no-change apply must NOT close the broadcast channel")
	default:
		// ok
	}
	// dbInfo pointer must still be the original — apply skips Store on no-change.
	if got := db.dbInfo.Load(); got != info {
		t.Errorf("no-change apply must NOT swap dbInfo: got %v, want %v", got, info)
	}
}

func TestApplyDBInfo_RealChangeBroadcasts(t *testing.T) {
	t.Parallel()
	db := &database{proxiesChanged: make(chan struct{})}
	old := &DBInfo{GRVProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}}}}
	db.dbInfo.Store(old)
	ch := db.waitProxiesChanged()

	updated := &DBInfo{GRVProxies: []ProxyInfo{{Address: "10.0.0.99:4500", Token: transport.UID{First: 99}}}}
	if !db.installProxySet(updated) {
		t.Error("real change must report change=true")
	}
	select {
	case <-ch:
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("real change must broadcast")
	}
	if db.dbInfo.Load() != updated {
		t.Error("dbInfo pointer must swap to updated on real change")
	}
}

// ============================================================================
// waitProxiesChanged — channel identity until next broadcast.
// ============================================================================

func TestWaitProxiesChanged_StableUntilBroadcast(t *testing.T) {
	t.Parallel()
	db := &database{proxiesChanged: make(chan struct{})}
	a := db.waitProxiesChanged()
	b := db.waitProxiesChanged()
	if a != b {
		t.Error("repeated waitProxiesChanged must return the same channel until broadcast")
	}
}

// ============================================================================
// handleConnError — pool eviction + failure marking.
// ============================================================================

func TestHandleConnError_MarksEndpointFailedWithEmptyPool(t *testing.T) {
	t.Parallel()
	db := &database{
		connPool: make(map[string]*transport.Conn),
		failMon:  newFailureMonitor(),
	}
	const addr = "10.0.0.42:4500"
	if db.failMon.isFailed(addr) {
		t.Fatal("setup: addr should not be marked failed yet")
	}
	db.handleConnError(addr)
	if !db.failMon.isFailed(addr) {
		t.Error("handleConnError must mark the endpoint as failed even when pool is empty")
	}
}

func TestHandleConnError_PoolUnchangedWhenAddrAbsent(t *testing.T) {
	t.Parallel()
	db := &database{
		connPool: make(map[string]*transport.Conn),
		failMon:  newFailureMonitor(),
	}
	db.handleConnError("not-in-pool:4500")
	if len(db.connPool) != 0 {
		t.Errorf("pool size should remain 0, got %d", len(db.connPool))
	}
}

// ============================================================================
// topologyMonitor — graceful shutdown via ctx.Done().
// ============================================================================

func TestTopologyMonitor_ShutsDownOnCtxDone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	db := &database{
		ctx:            ctx,
		cancel:         cancel,
		proxiesChanged: make(chan struct{}),
		failMon:        newFailureMonitor(),
		connPool:       make(map[string]*transport.Conn),
		connRecord:     newConnRecord(&ClusterFile{Coordinators: nil}, "", nil),
	}
	// With Coordinators: nil the monitor has nothing to poll and only waits
	// out the reconnection delay, which must yield to ctx.
	db.dbInfo.Store(&DBInfo{})

	db.wg.Add(1)
	doneTopology := make(chan struct{})
	go func() {
		db.topologyMonitor()
		close(doneTopology)
	}()

	cancel()
	select {
	case <-doneTopology:
		// ok — exited promptly
	case <-time.After(2 * time.Second):
		t.Fatal("topologyMonitor did not shut down within 2s of ctx cancel")
	}
	db.wg.Wait()
}

// ============================================================================
// Concurrent waitProxiesChanged + installProxySet — no panic, all waiters wake.
// ============================================================================

func TestApplyDBInfo_WakesAllConcurrentWaiters(t *testing.T) {
	t.Parallel()
	db := &database{proxiesChanged: make(chan struct{})}
	const N = 20
	var wg sync.WaitGroup
	var ready sync.WaitGroup
	wg.Add(N)
	ready.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ch := db.waitProxiesChanged()
			ready.Done() // signal: this goroutine has captured its channel snapshot
			<-ch         // block until installProxySet broadcasts
		}()
	}
	// Wait for every waiter to confirm it has captured its channel — replaces
	// the previous "sleep 50ms and hope" pattern (race on slow CI).
	ready.Wait()

	info := &DBInfo{GRVProxies: []ProxyInfo{{Address: "10.0.0.1:4500", Token: transport.UID{First: 1}}}}
	if !db.installProxySet(info) {
		t.Fatal("installProxySet should report change=true on first apply")
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		// ok — all 20 waiters woke
	case <-time.After(2 * time.Second):
		t.Fatal("installProxySet did not wake all concurrent waitProxiesChanged subscribers")
	}
}
