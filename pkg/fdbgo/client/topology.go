package client

import (
	"time"
)

const (
	topologySteadyInterval = 5 * time.Second        // background poll when idle
	topologyRapidInterval  = 200 * time.Millisecond // fast retry after a kick
	topologyRapidBurst     = 10                     // rapid refreshes before reverting
	// maxForwardHops bounds a pathological coordinator-forward cycle (RFC-111 §5).
	// C++ has no hop bound (actor fair-scheduling paces it); a Go tight loop needs
	// one. Reset on every successful non-forward connect, so a legitimate long
	// rotation chain still progresses.
	maxForwardHops = 10
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
// On success, atomically swaps dbInfo if proxies changed. It also follows a
// coordinator-set rotation: a forwarded connection string (Path A) or, when every
// coordinator is unreachable, an externally-rewritten cluster file (Path B) — so a
// `coordinators auto`/`change` no longer strands the client (RFC-111).
func (db *database) refreshTopology() {
	snap := db.connRecord.get()
	newInfo, err := db.tryAllCoordinators(db.ctx, snap)
	if err != nil {
		// Path B: all coordinators unreachable — adopt a rotated set from the file.
		if db.connRecord.adoptStoredIfChanged() {
			db.kickTopology()
		}
		return
	}
	if newInfo.Forward != "" {
		// Path A: a coordinator forwarded us to a new set. Adopt + re-poll now.
		if db.followForward(snap, newInfo.Forward) {
			db.kickTopology()
		}
		return
	}
	db.forwardHops = 0
	// The new coordinators answered with real proxies — now safe to persist a
	// forward we adopted in memory on a previous round (deferred-persist, Path A).
	db.connRecord.persistIfDirty()
	db.applyDBInfo(newInfo)
}

// followForward adopts a forwarded connection string (RFC-111 Path A). Returns
// true when a new, distinct coordinator set was adopted (the caller should re-poll
// immediately). It refuses (returns false) an unparseable or zero-coordinator
// forward (port of C++ ASSERT getNumberOfCoordinators() > 0,
// MonitorLeader.actor.cpp:946 — a soft reject, never a panic) and a degenerate
// self-forward, and stops following once forwardHops exceeds maxForwardHops to
// bound a pathological A->B->A cycle (Go-only divergence; C++ relies on actor
// fair-scheduling). forwardHops is written only by the single active follow path
// (bootstrap, then exclusively this monitor goroutine), so no atomic is needed.
func (db *database) followForward(old *ClusterFile, fwd string) bool {
	newCF, err := ParseClusterString(fwd)
	if err != nil || len(newCF.Coordinators) == 0 {
		db.logger.Warn("fdbgo: ignoring invalid coordinator forward", "forward", fwd, "error", err)
		return false
	}
	if newCF.String() == old.String() {
		return false // degenerate self-forward — not a real change
	}
	if db.forwardHops >= maxForwardHops {
		db.logger.Warn("fdbgo: coordinator forward chain exceeded bound; backing off",
			"hops", db.forwardHops, "forward", fwd)
		return false
	}
	db.forwardHops++
	db.connRecord.setInMemory(newCF)    // persisted by persistIfDirty after we connect to the new set
	db.metrics.countCoordinatorChange() // RFC-114: a coordinator-set rotation was followed
	db.logger.Info("fdbgo: followed coordinator forward", "from", old.String(), "to", newCF.String())
	return true
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
	db.recordConnFailure(addr)
}

// recordConnFailure marks an endpoint failed and makes the failure observable
// (RFC-114). It is the SINGLE observability sink for endpoint failures: the COUNTER
// ticks on every event (the rate signal, like logRetryEvent's counter), but the Warn
// is edge-triggered on the alive→failed transition so a flapping or down peer hit by
// the ~18 retry arms doesn't melt the log (the storm-hygiene rule logRetryEvent
// follows; one Warn per failure episode, re-armed by markAlive). Every failure path
// routes here — handleConnError (after pool eviction) and the GRV proxy-timeout path
// (sendGRVRequest) — so none is invisible.
func (db *database) recordConnFailure(addr string) {
	newlyFailed := db.failMon.markFailed(addr)
	db.metrics.countConnectionFailure()
	if newlyFailed && db.logger != nil {
		db.logger.Warn("fdbgo: connection to server failed", "address", addr)
	}
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
