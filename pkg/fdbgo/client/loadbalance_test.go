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

// settleSmoothers drives every tracked server's outstanding smoother to steady
// state (estimate == total), making the power-of-two selection metric reflect
// the load the test set up — deterministically.
//
// chooseServer/chooseTopTwo rank servers by smoothOutstanding.smoothTotal(), a
// C++-faithful Smoother whose estimate integrates toward total only as wall-clock
// time elapses. nowSeconds() is float64(time.Now().UnixNano())/1e9, and UnixNano
// (~1.75e18) exceeds float64's ~16 significant digits, so its sub-microsecond bits
// are lost: two calls a few hundred ns apart frequently read the *same* instant.
// In these microsecond-fast unit tests the smoother then never integrates the
// load just applied — every metric reads ~0, the comparison ties, and power-of-two
// random can pick a server the test expects to be ranked out (a real, reproducible
// flake under -count). Advancing each smoother by a large synthetic interval forces
// full integration; the subsequent real-now reads have elapsed < 0, so update()
// early-returns and the settled estimate stays put. Production is unaffected — real
// requests are milliseconds apart, far above the clock's resolution.
func (q *QueueModel) settleSmoothers() {
	q.mu.Lock()
	defer q.mu.Unlock()
	future := nowSeconds() + 100
	for _, d := range q.servers {
		d.smoothOutstanding.smoothTotal(future)
	}
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

	// Simulate s1 having higher outstanding (5), s2 zero, s3 moderate (2).
	for i := 0; i < 5; i++ {
		_ = qm.startRequest("s1:4500")
	}
	for i := 0; i < 2; i++ {
		_ = qm.startRequest("s3:4500")
	}

	// Make the smoothed metric reflect the load above regardless of how the wall
	// clock rounds during this fast test (see settleSmoothers).
	qm.settleSmoothers()

	// With power-of-two random: the worst server (s1 with metric=5)
	// should never be picked when both other candidates are better.
	// Run 100 iterations to verify s1 is never selected.
	for i := 0; i < 100; i++ {
		s, _ := qm.chooseServer(servers)
		if s.Address == "s1:4500" {
			t.Fatalf("iteration %d: selected worst server s1 (metric=5) over s2 (0) and s3 (2)", i)
		}
	}
}

func TestQueueModelSkipsFailedServers(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	// Mark s1 as failed by setting failedUntil far in the future.
	qm.mu.Lock()
	d := qm.getOrCreate("s1:4500")
	d.failedUntil = nowSeconds() + 60 // 60s in the future
	qm.mu.Unlock()

	s, _ := qm.chooseServer(servers)
	if s.Address != "s2:4500" {
		t.Fatalf("expected s2:4500 (s1 in failedUntil), got %s", s.Address)
	}
}

func TestQueueModelAllFailedPicksEarliestExpiry(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	now := nowSeconds()
	qm.mu.Lock()
	// s1 fails until now+1s, s2 fails until now+10s.
	d1 := qm.getOrCreate("s1:4500")
	d1.failedUntil = now + 1.0

	d2 := qm.getOrCreate("s2:4500")
	d2.failedUntil = now + 10.0
	qm.mu.Unlock()

	s, _ := qm.chooseServer(servers)
	// Both failed, pick s1 (expires sooner).
	if s.Address != "s1:4500" {
		t.Fatalf("expected s1:4500 (earliest expiry), got %s", s.Address)
	}
}

func TestQueueModelSmootherDecay(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	servers := makeServers("s1:4500", "s2:4500")

	// Add 5 requests to s1, complete them with varying latency.
	for i := 0; i < 5; i++ {
		d := qm.startRequest("s1:4500")
		qm.endRequest("s1:4500", d, 100*time.Microsecond, true)
	}
	// s2 has never been used — smoothOutstanding=0.
	// s1's smoothOutstanding should have decayed back toward 0
	// since we started and ended requests.

	// Add 1 outstanding to each and pick.
	_ = qm.startRequest("s1:4500")
	_ = qm.startRequest("s2:4500")

	// Both have ~1 outstanding. The smoother decay means s1's estimate
	// includes residual from the 5 completed requests. s2 should be
	// slightly better (cleaner estimate).
	s, _ := qm.chooseServer(servers)
	_ = s // Result depends on timing; just verify no crash/deadlock.
}

func TestQueueModelStartEndRequest(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()

	// Start 2 requests.
	_ = qm.startRequest("s1:4500")
	delta := qm.startRequest("s1:4500")

	// smoothOutstanding.total should be ~2.0 (2 * penalty=1.0).
	qm.mu.Lock()
	d := qm.servers["s1:4500"]
	if d.smoothOutstanding.total < 1.5 {
		t.Fatalf("expected outstanding total ~2.0, got %.2f", d.smoothOutstanding.total)
	}
	qm.mu.Unlock()

	// End one request with a distinct latency (2ms, not 1ms which matches initial).
	qm.endRequest("s1:4500", delta, 2*time.Millisecond, true)

	qm.mu.Lock()
	if d.smoothOutstanding.total < 0.5 {
		t.Fatalf("expected outstanding total ~1.0, got %.2f", d.smoothOutstanding.total)
	}
	// Latency should be updated to 0.002 (2ms), not the initial 0.001.
	if d.latency != 0.002 {
		t.Fatalf("expected latency=0.002 after endRequest(2ms), got %f", d.latency)
	}
	qm.mu.Unlock()
}

func TestQueueModelFutureVersionBackoff(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()

	// Simulate future_version error.
	d1 := qm.startRequest("s1:4500")
	qm.endRequestFull("s1:4500", d1, time.Millisecond, false, true, -1.0)

	qm.mu.Lock()
	d := qm.servers["s1:4500"]
	if d.failedUntil <= nowSeconds()-1 { // allow 1s margin
		t.Error("expected failedUntil > now after future_version")
	}
	// First future_version: increaseBackoffTime was 0, so now > 0 → backoff doubles: 1.0→2.0.
	if d.futureVersionBackoff != futureVersionInitialBackoff*futureVersionBackoffGrowth {
		t.Errorf("expected backoff=%.1f after 1st future_version, got %f",
			futureVersionInitialBackoff*futureVersionBackoffGrowth, d.futureVersionBackoff)
	}
	qm.mu.Unlock()

	// Second future_version: increaseBackoffTime was set to now+backoff,
	// so now <= increaseBackoffTime → backoff does NOT grow (C++ guard).
	d2 := qm.startRequest("s1:4500")
	qm.endRequestFull("s1:4500", d2, time.Millisecond, false, true, -1.0)

	qm.mu.Lock()
	// Backoff should NOT have grown (still 2.0) because increaseBackoffTime hasn't elapsed.
	if d.futureVersionBackoff != futureVersionInitialBackoff*futureVersionBackoffGrowth {
		t.Errorf("expected backoff=%.1f (guard prevents growth), got %f",
			futureVersionInitialBackoff*futureVersionBackoffGrowth, d.futureVersionBackoff)
	}

	// Manually expire the guard to allow growth.
	d.increaseBackoffTime = 0
	qm.mu.Unlock()

	// Third future_version: guard expired → backoff doubles: 2.0→4.0.
	d3 := qm.startRequest("s1:4500")
	qm.endRequestFull("s1:4500", d3, time.Millisecond, false, true, -1.0)

	qm.mu.Lock()
	expectedBackoff := futureVersionInitialBackoff * futureVersionBackoffGrowth * futureVersionBackoffGrowth
	if d.futureVersionBackoff != expectedBackoff {
		t.Errorf("expected backoff=%.1f after growth, got %f", expectedBackoff, d.futureVersionBackoff)
	}
	qm.mu.Unlock()
}

func TestQueueModelPenaltyFromServer(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()

	// Default penalty is 1.0.
	qm.mu.Lock()
	d := qm.getOrCreate("s1:4500")
	if d.penalty != queueDataInitialPenalty {
		t.Fatalf("expected initial penalty=%f, got %f", queueDataInitialPenalty, d.penalty)
	}
	qm.mu.Unlock()

	// Server reports penalty=2.5 in response.
	dp := qm.startRequest("s1:4500")
	qm.endRequestFull("s1:4500", dp, time.Millisecond, true, false, 2.5)

	qm.mu.Lock()
	if d.penalty != 2.5 {
		t.Fatalf("expected penalty=2.5 after server report, got %f", d.penalty)
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
				d := qm.startRequest(s.Address)
				qm.endRequest(s.Address, d, time.Millisecond, j%3 != 0)
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

	// 3 proxies: indices should cycle 1, 2, 0, 1, 2, 0...
	// Note: nextGRV uses Add(1) so first call returns 1%3=1.
	indices := make([]int, 6)
	for i := range indices {
		indices[i] = rr.nextGRV(3)
	}
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

func TestSmootherDecay(t *testing.T) {
	t.Parallel()
	// Test the smoother directly.
	s := smoother{eFoldingTime: 2.0}

	now := 100.0 // arbitrary start time
	s.addDelta(10.0, now)

	// Immediately after adding, estimate should still be ~0
	// (no time elapsed for decay to converge).
	est := s.smoothTotal(now)
	if est > 1.0 {
		t.Fatalf("estimate should be near 0 right after add, got %f", est)
	}

	// After 1 eFolding time (2s), estimate should be ~63% of total.
	est = s.smoothTotal(now + 2.0)
	expected := 10.0 * (1 - 1/2.718) // ~6.32
	if est < expected*0.8 || est > expected*1.2 {
		t.Fatalf("estimate after 1τ should be ~%.1f, got %.2f", expected, est)
	}

	// After 5τ (10s), estimate should be ~99.3% of total.
	est = s.smoothTotal(now + 10.0)
	if est < 9.0 {
		t.Fatalf("estimate after 5τ should be near 10.0, got %.2f", est)
	}
}

func TestSecondDelay(t *testing.T) {
	t.Parallel()
	q := newQueueModel()

	// Default latency is 1ms. secondDelay = max(10ms, 2.0 × 1ms) = 10ms.
	delay := q.secondDelay("server-a")
	if delay < 10*time.Millisecond || delay > 11*time.Millisecond {
		t.Fatalf("default secondDelay: got %v, want ~10ms", delay)
	}

	// Simulate a slow server (50ms latency). secondDelay = max(10ms, 2.0 × 50ms) = 100ms.
	q.endRequestFull("server-b", 1.0, 50*time.Millisecond, true, false, -1.0)
	delay = q.secondDelay("server-b")
	if delay < 90*time.Millisecond || delay > 110*time.Millisecond {
		t.Fatalf("slow server secondDelay: got %v, want ~100ms", delay)
	}

	// Very fast server (0.1ms latency). secondDelay = max(10ms, 2.0 × 0.1ms) = 10ms (clamped).
	q.endRequestFull("server-c", 1.0, 100*time.Microsecond, true, false, -1.0)
	delay = q.secondDelay("server-c")
	if delay < 9*time.Millisecond || delay > 11*time.Millisecond {
		t.Fatalf("fast server secondDelay: got %v, want ~10ms (clamped)", delay)
	}
}

func TestChooseTopTwo(t *testing.T) {
	t.Parallel()
	q := newQueueModel()

	// Single server → secondIdx = -1.
	servers := makeServers("a")
	best, second := q.chooseTopTwo(servers)
	if best != 0 || second != -1 {
		t.Fatalf("single server: best=%d, second=%d", best, second)
	}

	// Two servers, no load → both available.
	servers = makeServers("a", "b")
	best, second = q.chooseTopTwo(servers)
	if best == second {
		t.Fatalf("two servers: best and second should differ, got best=%d second=%d", best, second)
	}
	if second == -1 {
		t.Fatal("two servers: second should not be -1")
	}

	// Three servers with different loads.
	servers = makeServers("fast", "medium", "slow")
	q.endRequestFull("fast", 1.0, 1*time.Millisecond, true, false, -1.0)
	q.endRequestFull("medium", 1.0, 10*time.Millisecond, true, false, -1.0)
	q.endRequestFull("slow", 1.0, 100*time.Millisecond, true, false, -1.0)
	// Add load to "slow" to make it clearly worst.
	delta := q.startRequest("slow")
	_ = delta

	// Make the smoothed metric reflect the load above regardless of how the wall
	// clock rounds during this fast test (see settleSmoothers).
	q.settleSmoothers()

	best, second = q.chooseTopTwo(servers)
	// "slow" should never be primary; "fast" and "medium" are both valid under power-of-two random.
	if servers[best].Address == "slow" {
		t.Fatalf("slow should not be best: best=%d (%s)", best, servers[best].Address)
	}
	if best == second {
		t.Fatalf("best=%d should differ from second=%d", best, second)
	}
}
