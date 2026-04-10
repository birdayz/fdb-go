package client

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// QueueModel tracks estimated queue depth per storage server address.
// Used by sendGetValue/sendGetKey/sendGetRange to pick the least-loaded
// server from a shard's replica set, instead of always hitting servers[0].
//
// Matches C++ QueueModel in fdbclient/QueueModel.h. Simplified: we track
// in-flight count and latency EMA, estimate wait as inflight * latencyEMA,
// and skip servers in exponential backoff after failures.
type QueueModel struct {
	mu      sync.Mutex
	servers map[string]*serverMetrics
}

type serverMetrics struct {
	inflight   int32   // currently in-flight requests
	latencyEMA float64 // exponential moving average of response time (microseconds)
	lastFail   int64   // UnixNano of last failure (0 = no recent failure)
	failCount  int     // consecutive failure count (0 = healthy)
}

// EMA smoothing constants — match C++ QueueModel defaults.
const (
	latencyEMASmoothingFactor = 0.1   // C++ QUEUE_MODEL_SMOOTHING_AMOUNT
	defaultLatencyEMA         = 500.0 // 500µs — initial estimate for unknown servers
)

// Backoff constants for failed servers — match C++ LOAD_BALANCE knobs.
const (
	serverBackoffStart = 10 * time.Millisecond // LOAD_BALANCE_START_BACKOFF
	serverBackoffMax   = 5 * time.Second       // LOAD_BALANCE_MAX_BACKOFF
	serverBackoffRate  = 2.0                   // LOAD_BALANCE_BACKOFF_RATE
)

func newQueueModel() *QueueModel {
	return &QueueModel{
		servers: make(map[string]*serverMetrics),
	}
}

// getOrCreate returns the metrics for addr, creating if absent. Caller must hold mu.
func (q *QueueModel) getOrCreate(addr string) *serverMetrics {
	m, ok := q.servers[addr]
	if !ok {
		m = &serverMetrics{latencyEMA: defaultLatencyEMA}
		q.servers[addr] = m
	}
	return m
}

// chooseServer picks the server with the lowest estimated wait time from the
// given replica set. Returns the chosen server and its index in the slice.
//
// Estimated wait = inflight * latencyEMA. Servers in failure backoff are
// skipped unless ALL servers are in backoff (then pick the one with the
// shortest remaining backoff).
func (q *QueueModel) chooseServer(servers []ServerInfo) (ServerInfo, int) {
	if len(servers) == 1 {
		return servers[0], 0
	}

	now := time.Now().UnixNano()

	q.mu.Lock()
	defer q.mu.Unlock()

	bestIdx := -1
	bestWait := math.MaxFloat64

	// First pass: among healthy (non-backoff) servers, pick lowest wait.
	for i, s := range servers {
		m := q.getOrCreate(s.Address)
		if m.failCount > 0 && !q.backoffElapsed(m, now) {
			continue // in backoff
		}
		wait := float64(m.inflight) * m.latencyEMA
		if bestIdx < 0 || wait < bestWait {
			bestIdx = i
			bestWait = wait
		}
	}

	if bestIdx >= 0 {
		return servers[bestIdx], bestIdx
	}

	// All servers in backoff — pick the one whose backoff expires soonest.
	var bestExpiry int64 = math.MaxInt64
	for i, s := range servers {
		m := q.getOrCreate(s.Address)
		expiry := m.lastFail + q.backoffDuration(m.failCount).Nanoseconds()
		if expiry < bestExpiry {
			bestExpiry = expiry
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		bestIdx = 0 // shouldn't happen, but be safe
	}
	return servers[bestIdx], bestIdx
}

// backoffElapsed returns true if the server's backoff period has elapsed.
func (q *QueueModel) backoffElapsed(m *serverMetrics, nowNano int64) bool {
	if m.failCount == 0 || m.lastFail == 0 {
		return true
	}
	return nowNano-m.lastFail >= q.backoffDuration(m.failCount).Nanoseconds()
}

// backoffDuration computes exponential backoff for the given consecutive failure count.
func (q *QueueModel) backoffDuration(failCount int) time.Duration {
	d := float64(serverBackoffStart) * math.Pow(serverBackoffRate, float64(failCount-1))
	if d > float64(serverBackoffMax) {
		d = float64(serverBackoffMax)
	}
	return time.Duration(d)
}

// startRequest increments the in-flight counter for addr.
func (q *QueueModel) startRequest(addr string) {
	q.mu.Lock()
	m := q.getOrCreate(addr)
	m.inflight++
	q.mu.Unlock()
}

// endRequest decrements the in-flight counter and updates latency EMA.
// On success, resets fail state. On failure, increments consecutive fail count.
func (q *QueueModel) endRequest(addr string, latency time.Duration, success bool) {
	q.mu.Lock()
	m := q.getOrCreate(addr)
	if m.inflight > 0 {
		m.inflight--
	}
	if success {
		// Update EMA: new = α * sample + (1-α) * old
		sample := float64(latency.Microseconds())
		if sample < 1 {
			sample = 1 // floor at 1µs to avoid zero-weight
		}
		m.latencyEMA = latencyEMASmoothingFactor*sample + (1-latencyEMASmoothingFactor)*m.latencyEMA
		m.failCount = 0
		m.lastFail = 0
	} else {
		m.failCount++
		m.lastFail = time.Now().UnixNano()
	}
	q.mu.Unlock()
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
// Separate counters for GRV and commit proxies.
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
