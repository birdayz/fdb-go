package client

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"time"

	"fdb.dev/pkg/fdbgo/transport"
)

const (
	// coordinatorReconnectionDelay is CLIENT_KNOBS->COORDINATOR_RECONNECTION_DELAY
	// (ClientKnobs.cpp:52): the pause after a sweep in which every coordinator failed.
	coordinatorReconnectionDelay = time.Second
	// coordinatorLongPollCeiling bounds one long poll. A coordinator answers an
	// unchanged poll within CLIENT_REGISTER_INTERVAL (600s, jittered;
	// ServerKnobs.cpp:782, Coordination.actor.cpp:245-249); past the ceiling the
	// poll is re-sent, never counted as a failure.
	coordinatorLongPollCeiling = 15 * time.Minute
	// maxForwardHops bounds a pathological coordinator-forward cycle (RFC-111 §5).
	// C++ has no hop bound (actor fair-scheduling paces it); a Go tight loop needs
	// one. Reset on every successful non-forward connect, so a legitimate long
	// rotation chain still progresses.
	maxForwardHops = 10
)

// coordPollFunc sends one OpenDatabaseCoordRequest carrying known to addr.
type coordPollFunc func(ctx context.Context, snap *ClusterFile, addr string, known transport.UID) (*DBInfo, error)

// topologyMonitor follows ClientDBInfo the way C++ monitorProxies does
// (MonitorLeader.actor.cpp:984-1005): one long-poll generation per coordinator
// set, restarted when the set changes.
func (db *database) topologyMonitor() {
	defer db.wg.Done()
	// RFC-110: a panic in a generation must not abort the host; pace the restart
	// like a failed sweep so a deterministic bug re-fires at most once a second.
	pb := &panicBackstop{name: "topologyMonitor", db: db}
	var known transport.UID
	if cur := db.dbInfo.Load(); cur != nil {
		known = cur.ID
	}
	first := db.bootstrapCoordinator
	for db.ctx.Err() == nil {
		pause := func() { db.sleep(coordinatorReconnectionDelay) }
		if pb.run(func() { db.monitorProxiesOneGeneration(db.pollCoordinator, pause, first, &known) }) {
			pause()
		}
		first = ""
	}
}

// monitorProxiesOneGeneration ports C++ monitorProxiesOneGeneration
// (MonitorLeader.actor.cpp:840-982). It polls one coordinator at a time with the
// id of the last ClientDBInfo received, so the coordinator answers only when the
// info changes; a failure moves to the next coordinator, and a sweep in which all
// failed calls pause. It returns when the coordinator set changes. first, when in
// the set, is polled first: C++ keeps polling the coordinator that last answered
// (:966-967), and bootstrap is that answer here.
func (db *database) monitorProxiesOneGeneration(poll coordPollFunc, pause func(), first string, known *transport.UID) {
	snap := db.connRecord.get()
	if snap == nil || len(snap.Coordinators) == 0 {
		pause()
		return
	}
	addrs := slices.Clone(snap.Coordinators)
	rand.Shuffle(len(addrs), func(i, j int) { addrs[i], addrs[j] = addrs[j], addrs[i] })
	if i := slices.Index(addrs, first); i > 0 {
		addrs[0], addrs[i] = addrs[i], addrs[0]
	}
	index, successIndex := 0, 0
	allConnectionsFailed := false
	for db.ctx.Err() == nil {
		// Path B (:888-900): every coordinator of this set failed, so another
		// process may have rotated the set and rewritten the cluster file.
		if allConnectionsFailed && db.connRecord.adoptStoredIfChanged() {
			db.onCoordinatorSetAdopted()
			return
		}
		info, err := poll(db.ctx, snap, addrs[index], *known)
		if errors.Is(err, errLongPollExpired) {
			continue
		}
		if err == nil && info.Forward != "" {
			// Path A (:940-948): a coordinator forwarded us to a new set.
			if db.followForward(snap, info.Forward) {
				db.onCoordinatorSetAdopted()
				return
			}
			err = errors.New("coordinator forward could not be followed")
		}
		if err == nil && info.ID == (transport.UID{}) {
			// The id is echoed as knownClientInfoID; zero would never long-poll.
			err = errors.New("coordinator reply without a ClientDBInfo id")
		}
		if err != nil {
			// :969-980: next coordinator; pause once the sweep wraps around.
			index = (index + 1) % len(addrs)
			if index == successIndex {
				allConnectionsFailed = true
				pause()
			}
			continue
		}
		db.forwardHops = 0
		// The new coordinators answered with real proxies — now safe to persist a
		// forward adopted in memory on a previous round (deferred-persist, Path A).
		db.connRecord.persistIfDirty()
		// Installing the proxies IS the handoff when a coordinator-set adoption was
		// pending: the epoch changes here, inside the same act that publishes them.
		db.installProxySet(info)
		// C++ sets clientInfo unconditionally (:966), so the next poll waits on
		// this id even when the proxies did not change.
		*known = info.ID
		successIndex = index
		allConnectionsFailed = false
	}
}

// pollCoordinator is the production coordPollFunc. A non-zero known makes it a
// long poll, which C++ waits on with no client timeout (tryGetReply, :920-926);
// a dead coordinator surfaces as a connection error via the connection monitor.
func (db *database) pollCoordinator(ctx context.Context, snap *ClusterFile, addr string, known transport.UID) (*DBInfo, error) {
	conn, permit, err := db.getOrDialForSend(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer permit.Release()
	timeout := coordinatorLongPollCeiling
	if known == (transport.UID{}) {
		timeout = CoordinatorTimeout
	}
	return db.openDatabaseCoord(ctx, conn, snap, known, timeout, permit)
}

// sleep waits d or until the database closes.
func (db *database) sleep(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-db.ctx.Done():
	}
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

// installProxySet completes pending coordinator handoffs and publishes proxies under installMu.
// Coherence rule: DBInfo.Epoch must not exceed the fence epoch, or commits can lose their durable floor.
func (db *database) installProxySet(newInfo *DBInfo) bool {
	// The whole load-derive-bump-store sequence, not each step: see installMu.
	// Two concurrent installers that interleave here publish an epoch the fence
	// has already passed, with clusterSwitchPending spent, and nothing ever
	// republishes — a permanent wedge rather than a stale read.
	db.installMu.Lock()
	defer db.installMu.Unlock()

	old := db.dbInfo.Load()
	if db.clusterSwitchPending.CompareAndSwap(true, false) {
		// The returned epoch is deliberately DISCARDED. Using it would make the
		// published value a fact about when the bump happened; reading the fence
		// below makes it a fact about what the fence holds at publication, which
		// is the property that has to be true.
		db.grvCache.resetForNewCoordinators()
		// C++ resets minAcceptableReadVersion in the same block
		// (switchConnectionRecord, NativeAPI.actor.cpp:2201) and re-derives it
		// from the new cluster's first GRV. Go's floor is the SMALLEST version
		// seen and 0 means unset, so an empty stamp is the faithful reset:
		// carried across, the previous cluster's smallest rejects a perfectly
		// current user-set version from a cluster whose version space sits lower.
		//
		// Publishing it AT THE NEW EPOCH is belt-and-braces rather than the
		// mechanism — a previous epoch's stamp already reads as unset, because
		// every reader validates the stamp's epoch in the load that produced it.
		// This makes the reset explicit rather than relying on that alone.
		db.minAcceptableReadVersion.Store(&minAcceptableStamp{epoch: db.grvCache.epochNow()})
	}

	// Derive the snapshot epoch from the fence: publishing ahead of it would
	// reject a successful commit's durable floor as belonging to another epoch.
	newInfo.Epoch = db.grvCache.epochNow()

	if old != nil && dbInfoEqual(old, newInfo) {
		return false
	}
	// The test seam observes publication while installMu still excludes other installers.
	db.observeProxyInstall()
	db.dbInfo.Store(newInfo)

	// Broadcast proxy change to in-flight commits and GRVs. Close the old channel
	// to wake all waiters, create a fresh one for the next change.
	db.proxiesChangedMu.Lock()
	close(db.proxiesChanged)
	db.proxiesChanged = make(chan struct{})
	db.proxiesChangedMu.Unlock()
	return true
}

// waitProxiesChanged returns a channel that is closed when the proxy list
// changes. Each change creates a fresh channel for commit and GRV waiters
// (C++ onProxiesChanged).
func (db *database) waitProxiesChanged() <-chan struct{} {
	db.proxiesChangedMu.Lock()
	defer db.proxiesChangedMu.Unlock()
	return db.proxiesChanged
}

// handleConnError reports that failed (nil for a dial failure), the connection an
// RPC to addr used, has failed; with an open replacement pooled it only counts, as
// C++ reports only the connection it holds (FlowTransport.actor.cpp:924-1001).
func (db *database) handleConnError(addr string, failed *transport.Conn) {
	newlyFailed := false
	db.connMu.Lock()
	c := db.connPool[addr]
	if c == nil || c == failed || c.IsClosed() {
		if c != nil {
			c.Close()
			delete(db.connPool, addr)
		}
		if hook := db.beforeConnFailureMark.Load(); hook != nil {
			(*hook)()
		}
		// Under connMu, as dialAndPool's markAlive is: a redial cannot land between.
		newlyFailed = db.failMon.markFailed(addr)
	}
	db.connMu.Unlock()
	db.reportConnFailure(addr, newlyFailed)
}

// reportConnFailure makes an endpoint failure observable (RFC-114): the counter
// ticks on every report, the Warn only on the alive-to-failed transition.
func (db *database) reportConnFailure(addr string, newlyFailed bool) {
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
// install the old cluster's versions into the new epoch — with no second reset
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

// observeProxyInstall runs the duringProxyInstall seam when one is installed.
// Nil in production, so this is a load and a branch on the topology path.
//
// The call site is INSIDE installMu, which is not reentrant: a hook that
// calls back into installProxySet deadlocks the calling goroutine against
// itself. Hooks may observe and block; they may not install.
func (db *database) observeProxyInstall() {
	if hook := db.duringProxyInstall.Load(); hook != nil {
		(*hook)()
	}
}

// The handoff itself lives in installProxySet: the instant the handle starts
// talking to the new cluster is the instant its proxies are published, and
// making that one act is what leaves no window between the epoch and the proxies
// it describes.
