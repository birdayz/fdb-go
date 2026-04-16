package client

import (
	"math"
	"math/rand/v2"
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

	// Speculative second request knobs (C++ CLIENT_KNOBS)
	backupRequestDelay = 0.01 // BACKUP_REQUEST_DELAY (10ms minimum hedge delay)
	secondMultiplier   = 2.0  // MODEL_SECOND_MULTIPLIER (latency × this = hedge delay)
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

// chooseServer picks a server from the replica set using the "power of two
// random choices" algorithm, matching C++ basicLoadBalance with
// LOAD_BALANCE_USE_BEST_OF_TWO_RANDOM:
// 1. Pick two random candidate indices from available (non-failed) servers
// 2. Select the one with lower smoothOutstanding metric
// This distributes load better than deterministic min (which causes all
// clients to converge on the same server under uniform load).
func (q *QueueModel) chooseServer(servers []ServerInfo) (ServerInfo, int) {
	if len(servers) == 1 {
		return servers[0], 0
	}

	now := nowSeconds()

	q.mu.Lock()
	defer q.mu.Unlock()

	// Build list of available (non-failed) server indices.
	available := make([]int, 0, len(servers))
	for i, s := range servers {
		d := q.getOrCreate(s.Address)
		if now > d.failedUntil {
			available = append(available, i)
		}
	}

	if len(available) == 0 {
		// All servers failed — pick the one whose failedUntil expires soonest.
		bestIdx := 0
		bestExpiry := math.MaxFloat64
		for i, s := range servers {
			d := q.getOrCreate(s.Address)
			if d.failedUntil < bestExpiry {
				bestExpiry = d.failedUntil
				bestIdx = i
			}
		}
		return servers[bestIdx], bestIdx
	}

	if len(available) == 1 {
		return servers[available[0]], available[0]
	}

	// Best of two random choices: pick two distinct random indices,
	// select the one with lower metric.
	a := rand.IntN(len(available))
	b := rand.IntN(len(available) - 1)
	if b >= a {
		b++ // ensure b != a
	}
	idxA, idxB := available[a], available[b]
	metricA := q.getOrCreate(servers[idxA].Address).smoothOutstanding.smoothTotal(now)
	metricB := q.getOrCreate(servers[idxB].Address).smoothOutstanding.smoothTotal(now)

	if metricA <= metricB {
		return servers[idxA], idxA
	}
	return servers[idxB], idxB
}

// secondDelay returns the hedge delay for speculative second requests.
// The delay is max(BACKUP_REQUEST_DELAY, secondMultiplier × latency) for the
// best server. This balances between not sending too early (wasting work) and
// not waiting too long (defeating the purpose of hedging).
// Matches C++ LoadBalance.actor.h secondDelay computation.
func (q *QueueModel) secondDelay(addr string) time.Duration {
	q.mu.Lock()
	d := q.getOrCreate(addr)
	lat := d.latency
	q.mu.Unlock()

	delay := secondMultiplier * lat
	if delay < backupRequestDelay {
		delay = backupRequestDelay
	}
	return time.Duration(delay * float64(time.Second))
}

// chooseTopTwo picks a primary and backup server for hedged reads.
// Primary: power-of-two random (same as chooseServer) for load distribution.
// Backup: best remaining server by metric (deterministic) for fast hedge.
// Returns the indices. If only one server, secondIdx = -1.
func (q *QueueModel) chooseTopTwo(servers []ServerInfo) (bestIdx, secondIdx int) {
	if len(servers) <= 1 {
		return 0, -1
	}

	now := nowSeconds()
	q.mu.Lock()
	defer q.mu.Unlock()

	type ranked struct {
		idx    int
		metric float64
	}
	var candidates []ranked

	for i, s := range servers {
		d := q.getOrCreate(s.Address)
		if now <= d.failedUntil {
			continue
		}
		candidates = append(candidates, ranked{idx: i, metric: d.smoothOutstanding.smoothTotal(now)})
	}

	if len(candidates) == 0 {
		return 0, -1
	}
	if len(candidates) == 1 {
		return candidates[0].idx, -1
	}

	// Primary: power-of-two random selection for load distribution.
	a := rand.IntN(len(candidates))
	b := rand.IntN(len(candidates) - 1)
	if b >= a {
		b++
	}
	primary := a
	if candidates[b].metric < candidates[a].metric {
		primary = b
	}

	// Backup: best remaining candidate by metric (deterministic for fast hedge).
	backupCandIdx := -1
	backupMetric := math.MaxFloat64
	for i := range candidates {
		if i == primary {
			continue
		}
		if candidates[i].metric < backupMetric {
			backupMetric = candidates[i].metric
			backupCandIdx = i
		}
	}
	if backupCandIdx < 0 {
		return candidates[primary].idx, -1
	}
	return candidates[primary].idx, candidates[backupCandIdx].idx
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
// delta is the value returned by startRequest (penalty at request time).
// Matches C++ QueueModel::endRequest().
func (q *QueueModel) endRequest(addr string, delta float64, latency time.Duration, success bool) {
	q.endRequestFull(addr, delta, latency, success, false, -1.0)
}

// endRequestFull is the full-featured version matching C++ signature.
// delta is the value returned by startRequest (must match to keep smoothOutstanding balanced).
// futureVersion=true for error codes 1009/1037.
// penalty > 0 updates the server's penalty from the reply.
func (q *QueueModel) endRequestFull(addr string, delta float64, latency time.Duration, success bool, futureVersion bool, penalty float64) {
	now := nowSeconds()
	lat := latency.Seconds()

	q.mu.Lock()
	d := q.getOrCreate(addr)

	// Remove exactly the delta added at startRequest time. C++ passes the
	// delta through the ModelHolder; we pass it explicitly.
	d.smoothOutstanding.addDelta(-delta, now)

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
