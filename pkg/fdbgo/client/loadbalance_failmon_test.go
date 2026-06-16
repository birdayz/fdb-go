package client

import "testing"

// TestQueueModelExcludesConnDeadServer is the RFC-115 §1 HARD GATE, asserted with no
// clock: while ≥1 live replica exists, a connection-dead server (marked in the failure
// monitor) is NEVER returned by chooseServer. This is a pure selection assertion — the
// dead server's absence from the candidate set — not a latency measurement.
// Revert-proof: drop the `q.failMon.excluded` skip in chooseServer and the dead server
// gets selected ~1/3 of the time, reddening this loop.
func TestQueueModelExcludesConnDeadServer(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	qm.failMon = newFailureMonitor()
	qm.failMon.markFailed("dead:4500") // excluded for connFailureInitialWindow (≫ test runtime)

	servers := makeServers("dead:4500", "live1:4500", "live2:4500")
	for i := 0; i < 300; i++ {
		s, _ := qm.chooseServer(servers)
		if s.Address == "dead:4500" {
			t.Fatalf("iteration %d: chooseServer returned the connection-dead server", i)
		}
	}
}

// TestChooseTopTwoExcludesConnDeadServer: a dead server is never the hedge PRIMARY nor
// the hedge TARGET while live replicas exist (C++ loadBalance excludes it from both via
// the IFailureMonitor gate, LoadBalance.actor.h:499). Clock-free selection assertion.
func TestChooseTopTwoExcludesConnDeadServer(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	qm.failMon = newFailureMonitor()
	qm.failMon.markFailed("dead:4500")

	servers := makeServers("dead:4500", "live1:4500", "live2:4500")
	for i := 0; i < 300; i++ {
		best, second := qm.chooseTopTwo(servers)
		if servers[best].Address == "dead:4500" {
			t.Fatalf("iteration %d: dead server chosen as hedge primary", i)
		}
		if second >= 0 && servers[second].Address == "dead:4500" {
			t.Fatalf("iteration %d: dead server chosen as hedge target", i)
		}
	}
}

// TestQueueModelAllConnDeadProbesSoonest: when every replica is connection-dead, the LB
// must NOT deadlock or return an invalid index — it falls back to "try anyway" (the dial
// is Go's probe), unlike C++'s quorum(ok,1) block. Both chooseServer and chooseTopTwo
// return a valid in-range index.
func TestQueueModelAllConnDeadProbesSoonest(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	qm.failMon = newFailureMonitor()
	servers := makeServers("d1:4500", "d2:4500", "d3:4500")
	for _, s := range servers {
		qm.failMon.markFailed(s.Address)
	}

	s, idx := qm.chooseServer(servers)
	if idx < 0 || idx >= len(servers) || s.Address == "" {
		t.Fatalf("all-dead chooseServer returned invalid (idx=%d, addr=%q)", idx, s.Address)
	}
	best, second := qm.chooseTopTwo(servers)
	if best < 0 || best >= len(servers) {
		t.Fatalf("all-dead chooseTopTwo returned invalid primary %d", best)
	}
	if second >= len(servers) {
		t.Fatalf("all-dead chooseTopTwo returned out-of-range hedge %d", second)
	}
}

// TestQueueModelReadmitsRecoveredServer: after markAlive clears a dead server, it is a
// candidate again. Made deterministic by putting the only other replica into QueueModel
// backoff, so the recovered server is the SOLE eligible candidate and must be returned.
func TestQueueModelReadmitsRecoveredServer(t *testing.T) {
	t.Parallel()
	qm := newQueueModel()
	qm.failMon = newFailureMonitor()
	servers := makeServers("s1:4500", "s2:4500")

	qm.failMon.markFailed("s1:4500")
	// While excluded, s1 is never chosen (s2 is the only candidate).
	if s, _ := qm.chooseServer(servers); s.Address != "s2:4500" {
		t.Fatalf("excluded s1 should not be chosen, got %s", s.Address)
	}

	// Recover s1, and push s2 into QueueModel future_version backoff so s1 is the
	// SOLE eligible candidate — proving s1 was re-admitted, not just randomly picked.
	qm.failMon.markAlive("s1:4500")
	qm.mu.Lock()
	qm.getOrCreate("s2:4500").failedUntil = nowSeconds() + 60
	qm.mu.Unlock()

	if s, _ := qm.chooseServer(servers); s.Address != "s1:4500" {
		t.Fatalf("recovered s1 should be the sole eligible candidate, got %s", s.Address)
	}
}
