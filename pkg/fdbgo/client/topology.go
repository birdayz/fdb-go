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
			db.onCoordinatorSetAdopted()
			db.kickTopology()
		}
		return
	}
	if newInfo.Forward != "" {
		// Path A: a coordinator forwarded us to a new set. Adopt + re-poll now.
		if db.followForward(snap, newInfo.Forward) {
			db.onCoordinatorSetAdopted()
			db.kickTopology()
		}
		return
	}
	db.forwardHops = 0
	// The new coordinators answered with real proxies — now safe to persist a
	// forward we adopted in memory on a previous round (deferred-persist, Path A).
	db.connRecord.persistIfDirty()
	// Installing the proxies IS the handoff when a coordinator-set adoption was
	// pending: the epoch changes here, inside the same act that publishes them.
	db.installProxySet(newInfo)
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

// installProxySet publishes a proxy set, broadcasts the proxy-changed channel,
// and — when this installation completes a pending coordinator-set adoption —
// performs the cluster handoff in the SAME act. Returns true on apply. Split out
// from refreshTopology so the contract can be pinned without faking
// tryAllCoordinators.
//
// THE COHERENCE RULE, and why the handoff cannot be a separate call:
//
//   - DBInfo.Epoch is what a REQUEST binds to. It rides the snapshot the request
//     takes its proxies from, so an epoch can never describe a proxy set the
//     request did not use. That is what closes the commit path's window: the
//     epoch used to be sampled and the proxy selected afterwards, so a handoff
//     landing between the two made the commit's own token stale against the
//     cluster it actually committed to. The publication then failed sameEpoch,
//     the durable floor was never raised, and a lower GRV from the new cluster
//     repopulated the cache underneath a commit the caller had already been told
//     succeeded. On the GRV path refusing is merely conservative; on the commit
//     path it costs read-your-committed-writes, which is the invariant the whole
//     floor mechanism exists to hold.
//   - grvCache's fence word is what a PUBLICATION is judged against.
//
// The two are kept coherent by ORDER: the fences move FIRST, and only then is
// the DBInfo carrying the new epoch published. So DBInfo.Epoch <= fence epoch
// always, and the single reachable disagreement is (new fence, old proxies) — a
// request binding there carries the OLD epoch, talks to the OLD cluster, and is
// refused, which is the correct outcome rather than a conservative
// approximation of one. The reverse order would permit the inverse — a request
// binding the NEW epoch while still holding the old cluster's proxies — and that
// one installs another cluster's version.
//
// C++ reaches the same observable state by a different mechanism: switchConnectionRecord
// clears commitProxies and grvProxies inside the same block as its cluster-state
// reset (NativeAPI.actor.cpp:2196-2197, and again on the published clientInfo at
// :2206-2209), so nothing can be dispatched to the previous cluster at all.
func (db *database) installProxySet(newInfo *DBInfo) bool {
	old := db.dbInfo.Load()
	epoch := int64(0)
	if old != nil {
		epoch = old.Epoch
	}
	if db.clusterSwitchPending.CompareAndSwap(true, false) {
		epoch = db.grvCache.resetForNewCoordinators()
		// C++ resets minAcceptableReadVersion in the same block
		// (switchConnectionRecord, NativeAPI.actor.cpp:2201) and re-derives it
		// from the new cluster's first GRV. Go's floor is the SMALLEST version
		// seen and 0 means unset, so 0 is the faithful reset: carried across, the
		// previous cluster's smallest rejects a perfectly current user-set
		// version from a cluster whose version space sits lower, until this
		// handle's own first GRV happens to lower it.
		db.minAcceptableReadVersion.Store(0)
	}
	newInfo.Epoch = epoch

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

// dbInfoEqual returns true if two DBInfo have identical proxy lists AND the same
// cluster epoch.
//
// The epoch belongs in this comparison, not merely for completeness: a handoff
// onto a coordinator set that happens to publish an identical proxy list would
// otherwise be skipped as "no change", leaving the fence word bumped while the
// published DBInfo.Epoch stayed behind. Every subsequent request would then bind
// an epoch the fence has already passed and every publication would be refused —
// a permanent wedge, reached through the one case where the proxies did not move.
func dbInfoEqual(a, b *DBInfo) bool {
	if a.Epoch != b.Epoch {
		return false
	}
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

// onCoordinatorSetAdopted records that the handle has adopted a coordinator set
// it did not previously have. It does NOT reset the cache yet, and that timing
// is the point: adoption only changes which COORDINATORS we will ask. The
// proxies in db.dbInfo still belong to the previous cluster until a later
// successful refresh installs new ones, so a request dispatched in this window
// still goes to the OLD cluster. Bumping the epoch here would stamp those
// in-flight requests as belonging to the new one, and their replies would
// install the old cluster.s versions into the new epoch — with no second reset
// when the real proxies arrive.
//
// C++ closes the same window from the other side, clearing commitProxies and
// grvProxies in the same block as its cache reset (switchConnectionRecord,
// NativeAPI.actor.cpp:2196-2207) so nothing can be dispatched to the old
// cluster at all. Go reaches the same OBSERVABLE state by moving the epoch
// change to the handoff instead: anything dispatched while the old proxies are
// installed carries the old fences and is refused the moment they change.
func (db *database) onCoordinatorSetAdopted() {
	db.clusterSwitchPending.Store(true)
}

// The handoff itself lives in installProxySet: the instant the handle starts
// talking to the new cluster is the instant its proxies are published, and
// making that one act is what leaves no window between the epoch and the proxies
// it describes.
