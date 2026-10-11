package client

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

// failureMonitor tracks per-endpoint health state. When an endpoint
// transitions from failed to alive, all waiters are woken up via the
// recovered channel. This replaces C++'s quorum(ok,1) event-driven pattern.
//
// It also drives read load-balancing exclusion (RFC-115 §1): chooseServer/
// chooseTopTwo skip an endpoint while it is `excluded` (failed AND inside its
// re-admission window), matching C++ loadBalance's IFailureMonitor.failed gate
// (LoadBalance.actor.h:499). Because the pure-Go client is dial-on-demand and has
// NO background connectionKeeper to flip a dead peer healthy (FlowTransport.actor.cpp
// connectionKeeper does that in C++), a permanent exclusion would strand a recovered
// server. So the exclusion is TIME-BOUNDED: after the window elapses the endpoint is
// re-admitted as a probe; a real read then dials it and markAlive (database.go) clears
// it on success, or a re-failure grows the window. The dial IS Go's probe — the
// observable substitute for C++'s background reconnect (FDB-C-dev ACK, RFC-115 §1).
type failureMonitor struct {
	mu        sync.Mutex
	failed    map[string]*failEntry
	recovered chan struct{}
	// epFailed holds endpoints the server reported not found. They never
	// recover (FailureMonitor.actor.cpp:228-230 permanentlyFailed).
	epFailed map[endpointKey]struct{}
	epCount  atomic.Int32 // len(epFailed); lets the read hot path skip the lock
	// C++ NetworkInfo's all-alternatives-failed clock, in nowSeconds.
	oldestAltFailure, newestAltFailure, lastAltSkip float64
}

// allAlternativesFailedDelay knobs (flow/Knobs.cpp:284-290).
const (
	altFailureResetTime      = 5.0
	altFailureMinDelay       = 0.05
	altFailureDelayRatio     = 0.2
	altFailureMaxDelay       = 1.0
	altFailureSlowDelayRatio = 0.04
	altFailureSlowMaxDelay   = 30.0
	altFailureSkipDelay      = 1.0
)

// endpointKey is one request stream at one address, as C++ keys failedEndpoints.
type endpointKey struct {
	addr  string
	token transport.UID
}

// maxFailedEndpoints is C++'s failedEndpoints cap; past it the set is cleared
// (FailureMonitor.actor.cpp:131-134).
const maxFailedEndpoints = 100000

// failEntry is the per-endpoint failure state. Present in `failed` ⇔ isFailed.
type failEntry struct {
	excludedUntil float64 // wall-clock seconds: the LB excludes this endpoint until now > this
	window        float64 // current exclusion window (s); grows on a re-failure past the window
}

// Re-admission window knobs. Anchored to C++ flow knobs but a DELIBERATE local
// divergence in cadence: C++'s connectionKeeper re-dials a dead peer every
// ≤MAX_RECONNECTION_TIME (0.5s) as a cheap background TCP connect, decoupled from
// reads; Go's "probe" is a real read that pays a full dial-timeout on a hung server,
// so re-probing that often would reintroduce the very latency we're removing. We
// therefore exclude for FAILURE_DETECTION_DELAY (4.0s, the timescale at which C++
// considers a peer's health settled) before re-probing, growing by
// RECONNECTION_TIME_GROWTH_RATE (1.2) on a repeated failure, capped so a long-dead
// server is still re-probed periodically. All local-LB (zero wire). RFC-115 §1.
const (
	connFailureInitialWindow = 4.0  // FAILURE_DETECTION_DELAY (flow/Knobs.cpp:309)
	connFailureWindowGrowth  = 1.2  // RECONNECTION_TIME_GROWTH_RATE (flow/Knobs.cpp:114)
	connFailureMaxWindow     = 30.0 // Go choice: bound the re-probe interval for a long-dead server
)

func newFailureMonitor() *failureMonitor {
	return &failureMonitor{
		failed:    make(map[string]*failEntry),
		recovered: make(chan struct{}),
	}
}

// markFailed records that an endpoint is unhealthy. Returns true only on the
// alive→failed transition (the first failure of an episode), so a caller can
// edge-trigger a log without storming on every retry that re-hits a dead peer —
// the symmetric counterpart to markAlive's transition-only signal.
//
// It also (re)arms the LB re-admission window: a fresh failure starts at
// connFailureInitialWindow; a failure that arrives AFTER the previous window already
// expired (i.e. a re-admitted probe failed again) grows the window (capped), while a
// failure WITHIN an active window (a same-episode retry hit) only refreshes the
// deadline — so repeated retry hits in one outage don't inflate the backoff.
func (fm *failureMonitor) markFailed(addr string) (newlyFailed bool) {
	return fm.markFailedAt(addr, nowSeconds())
}

// markFailedAt is markFailed with an injectable clock, so the window-growth/cap
// transitions can be driven deterministically in tests without a real-time wait.
func (fm *failureMonitor) markFailedAt(addr string, now float64) (newlyFailed bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	e, ok := fm.failed[addr]
	if !ok {
		fm.failed[addr] = &failEntry{
			window:        connFailureInitialWindow,
			excludedUntil: now + connFailureInitialWindow,
		}
		return true
	}
	if now >= e.excludedUntil {
		// The window had expired (we'd re-admitted it) and a probe failed again —
		// grow so a persistently-dead server is re-probed less often.
		e.window = math.Min(e.window*connFailureWindowGrowth, connFailureMaxWindow)
	}
	e.excludedUntil = now + e.window
	return false
}

// markAlive records that an endpoint is healthy. If the endpoint was
// previously failed, closes the recovered channel to wake all waiters,
// then creates a fresh channel for the next recovery cycle.
func (fm *failureMonitor) markAlive(addr string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if _, ok := fm.failed[addr]; !ok {
		return // no transition — don't signal
	}
	delete(fm.failed, addr)
	close(fm.recovered)
	fm.recovered = make(chan struct{})
}

// waitForRecovery returns a channel that is closed when any endpoint
// transitions from failed to alive. Each recovery cycle gets a fresh channel.
func (fm *failureMonitor) waitForRecovery() <-chan struct{} {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.recovered
}

// isFailed returns whether an endpoint is currently marked as failed (present in
// the failed set), independent of its re-admission window. Used by tests and the
// recovery-signal logic; the LB uses `excluded` (which also honors the window).
func (fm *failureMonitor) isFailed(addr string) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	_, ok := fm.failed[addr]
	return ok
}

// excluded reports whether the LB should skip this endpoint right now: it is failed
// AND still inside its re-admission window (now < excludedUntil). Past the window a
// failed endpoint is re-admitted (returns false) so a read re-probes it. `now` is
// passed in (not read internally) so selection is testable without a fake clock.
// The LB calls this while holding the QueueModel mutex — lock order is always
// QueueModel.mu → failureMonitor.mu, never the reverse, so there is no deadlock.
func (fm *failureMonitor) excluded(addr string, now float64) bool {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	e, ok := fm.failed[addr]
	return ok && now < e.excludedUntil
}

// excludedUntil returns the endpoint's re-admission deadline (wall-clock seconds),
// or 0 if it is not currently failed. The LB uses it to pick the soonest-eligible
// server when every replica is failed (the "try anyway" probe).
func (fm *failureMonitor) excludedUntil(addr string) float64 {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if e, ok := fm.failed[addr]; ok {
		return e.excludedUntil
	}
	return 0
}

// markEndpointNotFound records that addr no longer has the endpoint token
// (SimpleFailureMonitor::endpointNotFound, FailureMonitor.actor.cpp:117-139).
func (fm *failureMonitor) markEndpointNotFound(addr string, token transport.UID) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if fm.epFailed == nil || len(fm.epFailed) > maxFailedEndpoints {
		fm.epFailed = make(map[endpointKey]struct{})
	}
	fm.epFailed[endpointKey{addr, token}] = struct{}{}
	fm.epCount.Store(int32(len(fm.epFailed)))
}

// endpointFailed reports whether the endpoint was reported not found; C++
// loadBalance skips such an alternative (LoadBalance.actor.h:499) and
// tryGetReply fails it without sending (fdbrpc.h:800-806).
func (fm *failureMonitor) endpointFailed(addr string, token transport.UID) bool {
	if fm == nil || fm.epCount.Load() == 0 {
		return false
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	_, ok := fm.epFailed[endpointKey{addr, token}]
	return ok
}

// liveServers drops replicas whose stream endpoint was reported not found.
// ok is false when none remain: C++ loadBalance then fails with
// all_alternatives_failed (LoadBalance.actor.h:617-634).
func (fm *failureMonitor) liveServers(servers []ServerInfo, endpoint int) (live []ServerInfo, ok bool) {
	if fm == nil || fm.epCount.Load() == 0 {
		return servers, true
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for i, s := range servers {
		if _, failed := fm.epFailed[endpointKey{s.Address, getAdjustedEndpoint(s.Token, endpoint)}]; failed {
			if live == nil {
				live = append(make([]ServerInfo, 0, len(servers)), servers[:i]...)
			}
		} else if live != nil {
			live = append(live, s)
		}
	}
	if live == nil {
		return servers, true
	}
	return live, len(live) > 0
}

// allAlternativesFailedDelay is the wait before all_alternatives_failed: the
// minimum, then growing with the storm's age (LoadBalance.actor.cpp:28-46).
func (fm *failureMonitor) allAlternativesFailedDelay(now float64) time.Duration {
	if fm == nil {
		return time.Duration(altFailureMinDelay * float64(time.Second))
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if now-fm.newestAltFailure > altFailureResetTime {
		fm.oldestAltFailure = now
	}
	d := altFailureMinDelay
	if now-fm.lastAltSkip > altFailureSkipDelay {
		fm.lastAltSkip = now
	} else {
		elapsed := now - fm.oldestAltFailure
		d = math.Max(d, math.Min(elapsed*altFailureDelayRatio, altFailureMaxDelay))
		d = math.Max(d, math.Min(elapsed*altFailureSlowDelayRatio, altFailureSlowMaxDelay))
	}
	fm.newestAltFailure = now
	return time.Duration(d * float64(time.Second))
}
