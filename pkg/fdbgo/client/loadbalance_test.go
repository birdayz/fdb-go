package client

import (
	"sync"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/transport"
)

func makeServers(addrs ...string) []ServerInfo {
	servers := make([]ServerInfo, len(addrs))
	for i, a := range addrs {
		servers[i] = ServerInfo{Address: a, Token: transport.UID{}}
	}
	return servers
}

func TestQueueModelSingleServer(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500")
	s, idx := qm.chooseServer(servers)
	if idx != 0 || s.Address != "s1:4500" {
		t.Fatalf("expected s1:4500 at 0, got %s at %d", s.Address, idx)
	}
}

func TestQueueModelPicksLeastLoaded(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500", "s3:4500")

	// Simulate s1 having 5 inflight, s2 having 0, s3 having 2.
	qm.mu.Lock()
	qm.getOrCreate("s1:4500").inflight = 5
	qm.getOrCreate("s2:4500").inflight = 0
	qm.getOrCreate("s3:4500").inflight = 2
	qm.mu.Unlock()

	s, idx := qm.chooseServer(servers)
	if s.Address != "s2:4500" {
		t.Fatalf("expected s2:4500 (lowest inflight), got %s at %d", s.Address, idx)
	}
}

func TestQueueModelSkipsFailedServers(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	// Mark s1 as failed with recent failure.
	qm.mu.Lock()
	m := qm.getOrCreate("s1:4500")
	m.failCount = 1
	m.lastFail = time.Now().UnixNano()
	qm.mu.Unlock()

	s, _ := qm.chooseServer(servers)
	if s.Address != "s2:4500" {
		t.Fatalf("expected s2:4500 (s1 in backoff), got %s", s.Address)
	}
}

func TestQueueModelAllFailedPicksEarliestExpiry(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	now := time.Now().UnixNano()
	qm.mu.Lock()
	// s1 failed 100ms ago (failCount=1, backoff=10ms → expired)
	// s2 failed just now (failCount=3, backoff=40ms → not expired)
	m1 := qm.getOrCreate("s1:4500")
	m1.failCount = 1
	m1.lastFail = now - int64(100*time.Millisecond)

	m2 := qm.getOrCreate("s2:4500")
	m2.failCount = 3
	m2.lastFail = now
	qm.mu.Unlock()

	s, _ := qm.chooseServer(servers)
	// s1's backoff (10ms) elapsed 90ms ago → healthy in first pass.
	if s.Address != "s1:4500" {
		t.Fatalf("expected s1:4500 (backoff elapsed), got %s", s.Address)
	}
}

func TestQueueModelLatencyEMA(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	// s1: fast server (100µs latency)
	// s2: slow server (10ms latency)
	for i := 0; i < 20; i++ {
		qm.startRequest("s1:4500")
		qm.endRequest("s1:4500", 100*time.Microsecond, true)
		qm.startRequest("s2:4500")
		qm.endRequest("s2:4500", 10*time.Millisecond, true)
	}

	// Both have 0 inflight, but the EMA difference should make s1 preferred
	// when they have the same inflight count. Actually with 0 inflight both
	// have wait=0, so let's add 1 inflight each.
	qm.startRequest("s1:4500")
	qm.startRequest("s2:4500")

	s, _ := qm.chooseServer(servers)
	if s.Address != "s1:4500" {
		t.Fatalf("expected s1:4500 (lower latency EMA), got %s", s.Address)
	}

	qm.endRequest("s1:4500", 100*time.Microsecond, true)
	qm.endRequest("s2:4500", 10*time.Millisecond, true)
}

func TestQueueModelStartEndRequest(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()

	qm.startRequest("s1:4500")
	qm.startRequest("s1:4500")

	qm.mu.Lock()
	m := qm.servers["s1:4500"]
	if m.inflight != 2 {
		t.Fatalf("expected inflight=2, got %d", m.inflight)
	}
	qm.mu.Unlock()

	qm.endRequest("s1:4500", 1*time.Millisecond, true)

	qm.mu.Lock()
	if m.inflight != 1 {
		t.Fatalf("expected inflight=1 after end, got %d", m.inflight)
	}
	if m.failCount != 0 {
		t.Fatalf("expected failCount=0 on success, got %d", m.failCount)
	}
	qm.mu.Unlock()

	qm.endRequest("s1:4500", 1*time.Millisecond, false)

	qm.mu.Lock()
	if m.failCount != 1 {
		t.Fatalf("expected failCount=1 on failure, got %d", m.failCount)
	}
	qm.mu.Unlock()
}

func TestQueueModelConcurrentSafe(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500", "s3:4500")

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s, _ := qm.chooseServer(servers)
				qm.startRequest(s.Address)
				qm.endRequest(s.Address, time.Millisecond, j%3 != 0)
			}
		}()
	}
	wg.Wait()
}

func TestLoadBalanceOrder(t *testing.T) {
	t.Parallel()
	servers := makeServers("s1:4500", "s2:4500", "s3:4500")

	// chosenIdx=0 → no reorder.
	order := loadBalanceOrder(servers, 0)
	if len(order) != 3 || order[0].Address != "s1:4500" {
		t.Fatalf("expected original order for idx=0, got %v", order)
	}

	// chosenIdx=2 → s3 first, then s1, s2.
	order = loadBalanceOrder(servers, 2)
	if order[0].Address != "s3:4500" || order[1].Address != "s1:4500" || order[2].Address != "s2:4500" {
		t.Fatalf("expected [s3,s1,s2], got [%s,%s,%s]", order[0].Address, order[1].Address, order[2].Address)
	}

	// chosenIdx=1 → s2 first, then s1, s3.
	order = loadBalanceOrder(servers, 1)
	if order[0].Address != "s2:4500" || order[1].Address != "s1:4500" || order[2].Address != "s3:4500" {
		t.Fatalf("expected [s2,s1,s3], got [%s,%s,%s]", order[0].Address, order[1].Address, order[2].Address)
	}
}

func TestLoadBalanceOrderSingle(t *testing.T) {
	t.Parallel()
	servers := makeServers("s1:4500")
	order := loadBalanceOrder(servers, 0)
	if len(order) != 1 || order[0].Address != "s1:4500" {
		t.Fatalf("expected single server unchanged")
	}
}

func TestProxyRoundRobin(t *testing.T) {
	t.Parallel()
	var rr proxyRoundRobin

	// 3 proxies: indices should cycle 0, 1, 2, 0, 1, 2...
	// Note: nextGRV uses Add(1) so first call returns 1%3=1.
	indices := make([]int, 6)
	for i := range indices {
		indices[i] = rr.nextGRV(3)
	}
	// The sequence is: 1, 2, 0, 1, 2, 0 (wrapping)
	expected := []int{1, 2, 0, 1, 2, 0}
	for i, exp := range expected {
		if indices[i] != exp {
			t.Fatalf("index %d: expected %d, got %d", i, exp, indices[i])
		}
	}

	// Commit counter is independent.
	idx := rr.nextCommit(2)
	if idx != 1 {
		t.Fatalf("expected commit idx=1, got %d", idx)
	}
}

func TestQueueModelEndRequestNeverNegativeInflight(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	// End without start — inflight should stay 0, not go negative.
	qm.endRequest("s1:4500", time.Millisecond, true)

	qm.mu.Lock()
	m := qm.servers["s1:4500"]
	if m.inflight != 0 {
		t.Fatalf("expected inflight=0 (not negative), got %d", m.inflight)
	}
	qm.mu.Unlock()
}

func TestQueueModelFailureResetOnSuccess(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()

	// Build up failures.
	qm.startRequest("s1:4500")
	qm.endRequest("s1:4500", time.Millisecond, false)
	qm.startRequest("s1:4500")
	qm.endRequest("s1:4500", time.Millisecond, false)

	qm.mu.Lock()
	m := qm.servers["s1:4500"]
	if m.failCount != 2 {
		t.Fatalf("expected failCount=2, got %d", m.failCount)
	}
	qm.mu.Unlock()

	// Single success should reset.
	qm.startRequest("s1:4500")
	qm.endRequest("s1:4500", time.Millisecond, true)

	qm.mu.Lock()
	if m.failCount != 0 {
		t.Fatalf("expected failCount=0 after success, got %d", m.failCount)
	}
	if m.lastFail != 0 {
		t.Fatalf("expected lastFail=0 after success, got %d", m.lastFail)
	}
	qm.mu.Unlock()
}
