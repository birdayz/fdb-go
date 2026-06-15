package client

import (
	"time"
)

const (
	topologySteadyInterval = 5 * time.Second        // background poll when idle
	topologyRapidInterval  = 200 * time.Millisecond // fast retry after a kick
	topologyRapidBurst     = 10                     // rapid refreshes before reverting
)

// topologyMonitor periodically refreshes the cluster topology from coordinators.
// After an RPC-failure kick, switches to rapid polling for fast recovery before
// reverting to steady-state interval. The C++ client uses long-poll to the
// cluster controller; we approximate with kick-triggered bursts since we talk
// directly to coordinators.
func (db *database) topologyMonitor() {
	defer db.wg.Done()
	// RFC-110: a panic in refreshTopology (a wire-decode invariant, a nil-deref)
	// must not abort the host — libfdb_c's monitorProxies never lets a round
	// failure take down the network thread. The backstop recovers + counts +
	// rate-limited-logs; on a recovered panic we drop out of rapid-poll to the
	// steady interval so a deterministic bug re-fires at ≤1/steady, not every
	// 200ms (the monitorProxies post-failed-sweep COORDINATOR_RECONNECTION_DELAY
	// analog).
	pb := &panicBackstop{name: "topologyMonitor", db: db}
	ticker := time.NewTicker(topologySteadyInterval)
	defer ticker.Stop()
	rapidLeft := 0
	for {
		select {
		case <-ticker.C:
			if pb.run(db.refreshTopology) {
				rapidLeft = 0
				ticker.Reset(topologySteadyInterval)
				continue
			}
			if rapidLeft > 0 {
				rapidLeft--
				if rapidLeft == 0 {
					ticker.Reset(topologySteadyInterval)
				}
			}
		case <-db.topologyKick:
			if pb.run(db.refreshTopology) {
				rapidLeft = 0
				ticker.Reset(topologySteadyInterval)
				continue
			}
			rapidLeft = topologyRapidBurst
			ticker.Reset(topologyRapidInterval)
		case <-db.ctx.Done():
			return
		}
	}
}

// kickTopology triggers an immediate topology refresh. Non-blocking.
// Called when an RPC to a proxy fails (connection error, broken_promise, etc.).
func (db *database) kickTopology() {
	select {
	case db.topologyKick <- struct{}{}:
	default:
	}
}

// refreshTopology races all coordinators in parallel to fetch fresh ClientDBInfo.
// On success, atomically swaps dbInfo if proxies changed.
func (db *database) refreshTopology() {
	newInfo, err := db.tryAllCoordinators(db.ctx)
	if err != nil {
		return
	}
	db.applyDBInfo(newInfo)
}

// applyDBInfo replaces dbInfo and broadcasts the proxy-changed channel
// when newInfo differs from the current value. Returns true on apply.
// Split out from refreshTopology so the apply/broadcast contract can be
// pinned without faking tryAllCoordinators.
func (db *database) applyDBInfo(newInfo *DBInfo) bool {
	old := db.dbInfo.Load()
	if old != nil && dbInfoEqual(old, newInfo) {
		return false
	}
	db.dbInfo.Store(newInfo)

	// Broadcast proxy change to in-flight commits. Close the old channel
	// to wake all waiters, create a fresh one for the next change.
	db.proxiesChangedMu.Lock()
	close(db.proxiesChanged)
	db.proxiesChanged = make(chan struct{})
	db.proxiesChangedMu.Unlock()
	return true
}

// waitProxiesChanged returns a channel that is closed when the proxy list
// changes. Each change creates a fresh channel. Used by commit to detect
// mid-commit proxy changes (C++ onProxiesChanged).
func (db *database) waitProxiesChanged() <-chan struct{} {
	db.proxiesChangedMu.Lock()
	defer db.proxiesChangedMu.Unlock()
	return db.proxiesChanged
}

// handleConnError evicts a dead connection from the pool and marks the
// endpoint as failed so the failure monitor can wake backoff sleeps on recovery.
func (db *database) handleConnError(addr string) {
	db.connMu.Lock()
	if c, ok := db.connPool[addr]; ok {
		c.Close()
		delete(db.connPool, addr)
	}
	db.connMu.Unlock()
	db.failMon.markFailed(addr)
}

// dbInfoEqual returns true if two DBInfo have identical proxy lists.
func dbInfoEqual(a, b *DBInfo) bool {
	if len(a.GRVProxies) != len(b.GRVProxies) || len(a.CommitProxies) != len(b.CommitProxies) {
		return false
	}
	for i := range a.GRVProxies {
		if a.GRVProxies[i].Address != b.GRVProxies[i].Address ||
			a.GRVProxies[i].Token != b.GRVProxies[i].Token {
			return false
		}
	}
	for i := range a.CommitProxies {
		if a.CommitProxies[i].Address != b.CommitProxies[i].Address ||
			a.CommitProxies[i].Token != b.CommitProxies[i].Token {
			return false
		}
	}
	return true
}
