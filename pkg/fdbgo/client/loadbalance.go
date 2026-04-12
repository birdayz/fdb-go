package client

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// QueueModel tracks estimated outstanding request depth per storage server.
// Used by sendGetValue/sendGetKey/sendGetRange to pick the least-loaded
// server from a shard's replica set.
//
// Matches C++ QueueModel in fdbclient/QueueModel.h:
// - Smoother-based continuous exponential decay (eFoldingTime=2s)
// - Server penalty from LoadBalancedReply
// - failedUntil with exponential backoff for future_version errors
//
// Selection metric: smoothOutstanding.smoothTotal() (lower = better).
// C++ also uses latency for speculative second requests (secondDelay),
// which we don't implement yet.
type QueueModel struct {
	mu      sync.Mutex
	servers map[string]*queueData
}

// queueData matches C++ QueueData. One per storage server endpoint.
type queueData struct {
	smoothOutstanding smoother // eFoldingTime=2.0s — tracks weighted outstanding requests
	latency           float64  // last measured client-side latency in seconds. Init: 0.001
	penalty           float64  // per-request cost multiplier from server. Init: 1.0. Always >= 1.0
	failedUntil       float64  // wall time (Unix seconds): ignore server until now > failedUntil

	// future_version exponential backoff (C++ QueueData fields)
	futureVersionBackoff float64 // current backoff duration in seconds. Init: 1.0
	increaseBackoffTime  float64 // wall time: only grow backoff after this time. Init: 0
}

// smoother implements C++ Smoother from fdbrpc/Smoother.h.
// Continuous exponential decay toward a target total.
type smoother struct {
	eFoldingTime float64 // time constant τ (seconds)
	total        float64 // exact running sum (set by addDelta)
	estimate     float64 // smoothed value (lazy, updated on read/write)
	time         float64 // Unix seconds of last update
}

// C++ CLIENT_KNOBS defaults for QueueModel.
const (
	queueModelSmoothingAmount = 2.0   // QUEUE_MODEL_SMOOTHING_AMOUNT (eFoldingTime)
	queueDataInitialLatency   = 0.001 // 1ms initial latency estimate
	queueDataInitialPenalty   = 1.0   // default penalty (no overload)

	// future_version backoff knobs
	futureVersionInitialBackoff = 1.0 // INITIAL_BACKOFF
	futureVersionMaxBackoff     = 8.0 // FUTURE_VERSION_MAX_BACKOFF (growth capped here)
	futureVersionBackoffGrowth  = 2.0 // BACKOFF_GROWTH_RATE

	// Load balance knobs
	loadBalanceMaxBadOptions = 1    // LOAD_BALANCE_MAX_BAD_OPTIONS
	loadBalancePenaltyIsBad  = true // LOAD_BALANCE_PENALTY_IS_BAD
	penaltyBadThreshold      = 1.001
)

// Backoff constants for completely failed servers.
const (
	serverBackoffStart = 10 * time.Millisecond // LOAD_BALANCE_START_BACKOFF
	serverBackoffMax   = 5 * time.Second       // LOAD_BALANCE_MAX_BACKOFF
	serverBackoffRate  = 2.0                   // LOAD_BALANCE_BACKOFF_RATE
)

func newQueueModel() *QueueModel {
	return &QueueModel{
		servers: make(map[string]*queueData),
	}
}

// getOrCreate returns the queueData for addr, creating if absent. Caller must hold mu.
func (q *QueueModel) getOrCreate(addr string) *queueData {
	d, ok := q.servers[addr]
	if !ok {
		d = &queueData{
			smoothOutstanding: smoother{
				eFoldingTime: queueModelSmoothingAmount,
			},
			latency:              queueDataInitialLatency,
			penalty:              queueDataInitialPenalty,
			futureVersionBackoff: futureVersionInitialBackoff,
		}
		q.servers[addr] = d
	}
	return d
}

// chooseServer picks the server with the lowest smoothOutstanding from the
// given replica set. Matches C++ LoadBalance.actor.h server selection:
// - Skip hard-failed servers (failedUntil not elapsed)
// - Count penalty > 1.001 as "bad" when LOAD_BALANCE_PENALTY_IS_BAD
// - Select by min smoothOutstanding.smoothTotal()
func (q *QueueModel) chooseServer(servers []ServerInfo) (ServerInfo, int) {
	if len(servers) == 1 {
		return servers[0], 0
	}

	now := nowSeconds()

	q.mu.Lock()
	defer q.mu.Unlock()

	bestIdx := -1
	bestMetric := math.MaxFloat64
	badServers := 0

	for i, s := range servers {
		d := q.getOrCreate(s.Address)

		// Skip servers in failedUntil backoff.
		if now <= d.failedUntil {
			badServers++
			if badServers > loadBalanceMaxBadOptions+1 {
				break // C++: stop iterating after too many bad
			}
			continue
		}

		// Penalty > threshold counts as "bad" but still considered.
		if loadBalancePenaltyIsBad && d.penalty > penaltyBadThreshold {
			badServers++
		}

		metric := d.smoothOutstanding.smoothTotal(now)
		if bestIdx < 0 || metric < bestMetric {
			bestIdx = i
			bestMetric = metric
		}
	}

	if bestIdx >= 0 {
		return servers[bestIdx], bestIdx
	}

	// All servers failed — pick the one whose failedUntil expires soonest.
	var bestExpiry float64 = math.MaxFloat64
	for i, s := range servers {
		d := q.getOrCreate(s.Address)
		if d.failedUntil < bestExpiry {
			bestExpiry = d.failedUntil
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		bestIdx = 0
	}
	return servers[bestIdx], bestIdx
}

// startRequest increments smoothOutstanding by the server's current penalty.
// Returns the delta that must be passed back to endRequest.
// Matches C++ QueueModel::addRequest().
func (q *QueueModel) startRequest(addr string) float64 {
	now := nowSeconds()
	q.mu.Lock()
	d := q.getOrCreate(addr)
	delta := d.penalty
	d.smoothOutstanding.addDelta(delta, now)
	q.mu.Unlock()
	return delta
}

// endRequest decrements smoothOutstanding and updates latency/penalty/backoff.
// Matches C++ QueueModel::endRequest().
func (q *QueueModel) endRequest(addr string, latency time.Duration, success bool) {
	q.endRequestFull(addr, latency, success, false, -1.0)
}

// endRequestFull is the full-featured version matching C++ signature.
// futureVersion=true for error codes 1009/1037.
// penalty > 0 updates the server's penalty from the reply.
func (q *QueueModel) endRequestFull(addr string, latency time.Duration, success bool, futureVersion bool, penalty float64) {
	now := nowSeconds()
	lat := latency.Seconds()

	q.mu.Lock()
	d := q.getOrCreate(addr)

	// Remove the penalty added at startRequest. Use current penalty as delta
	// (C++ stores delta from addRequest; we approximate with current penalty).
	d.smoothOutstanding.addDelta(-d.penalty, now)

	if success {
		d.latency = lat
	} else {
		// Error: take worst case latency.
		if lat > d.latency {
			d.latency = lat
		}
	}

	if futureVersion {
		if now > d.increaseBackoffTime {
			d.futureVersionBackoff = math.Min(d.futureVersionBackoff*futureVersionBackoffGrowth, futureVersionMaxBackoff)
			d.increaseBackoffTime = now + d.futureVersionBackoff
		}
		d.failedUntil = now + d.futureVersionBackoff
	} else if success {
		d.futureVersionBackoff = futureVersionInitialBackoff
		d.increaseBackoffTime = 0
	}

	if penalty > 0 {
		d.penalty = penalty
	}

	q.mu.Unlock()
}

// nowSeconds returns the current time as Unix seconds (float64).
func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// --- Smoother implementation (C++ fdbrpc/Smoother.h) ---

// smoothTotal returns the current smoothed estimate, updating the decay.
// Matches C++ Smoother::smoothTotal().
func (s *smoother) smoothTotal(now float64) float64 {
	s.update(now)
	return s.estimate
}

// addDelta adds a delta to the total, updating the decay first.
// Matches C++ Smoother::addDelta().
func (s *smoother) addDelta(delta float64, now float64) {
	s.update(now)
	s.total += delta
}

// update applies the continuous exponential decay.
// estimate += (total - estimate) * (1 - exp(-elapsed / eFoldingTime))
func (s *smoother) update(now float64) {
	if s.time == 0 {
		s.time = now
		return
	}
	elapsed := now - s.time
	if elapsed <= 0 {
		return
	}
	s.time = now
	s.estimate += (s.total - s.estimate) * (1.0 - math.Exp(-elapsed/s.eFoldingTime))
}

// reset sets the smoother to a specific value immediately.
func (s *smoother) reset(value float64) {
	s.time = 0
	s.total = value
	s.estimate = value
}

// loadBalanceOrder returns servers reordered with the chosen server first,
// followed by the remaining servers in their original order.
// Does not allocate when chosenIdx == 0 (the common case for single-server shards).
func loadBalanceOrder(servers []ServerInfo, chosenIdx int) []ServerInfo {
	if chosenIdx == 0 || len(servers) <= 1 {
		return servers
	}
	out := make([]ServerInfo, 0, len(servers))
	out = append(out, servers[chosenIdx])
	out = append(out, servers[:chosenIdx]...)
	out = append(out, servers[chosenIdx+1:]...)
	return out
}

// proxyRoundRobin provides simple round-robin selection for GRV and commit proxies.
// Separate counters for GRV and commit proxies. Must be safe for concurrent use.
type proxyRoundRobin struct {
	grvCounter    atomic.Uint64
	commitCounter atomic.Uint64
}

func (rr *proxyRoundRobin) nextGRV(n int) int {
	return int(rr.grvCounter.Add(1) % uint64(n))
}

func (rr *proxyRoundRobin) nextCommit(n int) int {
	return int(rr.commitCounter.Add(1) % uint64(n))
}
