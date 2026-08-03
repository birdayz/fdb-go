package client

// The cluster epoch lives IN the DBInfo snapshot, beside the proxies it
// describes, and that placement is the whole point: a request loads the proxies
// and the epoch in one atomic read, so it can never bind an epoch belonging to a
// proxy set it did not use.
//
// The window this closes is on the COMMIT path. It used to sample the epoch and
// then select a proxy; a coordinator handoff landing between the two left the
// commit carrying the PREVIOUS cluster's epoch while committing to the NEW one.
// The publication then failed sameEpoch — which costs not only the install (a
// cache miss, harmless) but the DURABLE FLOOR. Without the floor a lower GRV
// from the new cluster repopulates the cache underneath a commit the caller has
// already been told succeeded, and an opted-in read misses its own write. On the
// GRV path refusing is merely conservative; here it costs
// read-your-committed-writes.

import (
	"context"
	"testing"
	"time"
)

// TestEpoch_CommitBindsTheEpochOfTheProxySetItUsed drives the REAL commit path
// against a real cluster with the handoff firing in the exact window — between
// the caller entering Commit and the proxy selection — through the production
// seam that exists for it.
//
// The assertion is the FLOOR, because that is what the window costs. The install
// is separately refused (the handoff moves the generation too, as an
// invalidation would), and that refusal is a benign cache miss; the floor is the
// invariant.
func TestEpoch_CommitBindsTheEpochOfTheProxySetItUsed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("epoch-commit-bind-" + t.Name())

	// The handoff fires once, inside the commit window. Re-publishing the SAME
	// proxy set is what a real handoff onto an unchanged topology looks like, and
	// it is also the case dbInfoEqual has to let through — the epoch moved even
	// though the proxies did not.
	var fired bool
	hook := func() {
		if fired {
			return
		}
		fired = true
		db.db.onCoordinatorSetAdopted()
		db.db.installProxySet(&DBInfo{
			GRVProxies:    db.db.dbInfo.Load().GRVProxies,
			CommitProxies: db.db.dbInfo.Load().CommitProxies,
		})
	}
	db.db.beforeCommitProxySelect.Store(&hook)
	defer db.db.beforeCommitProxySelect.Store(nil)

	epochBefore := db.db.dbInfo.Load().Epoch

	tx := db.CreateTransaction()
	defer tx.Cancel()
	tx.Set(key, []byte("v"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	cv, err := tx.GetCommittedVersion()
	if err != nil {
		t.Fatalf("committed version: %v", err)
	}

	if !fired {
		t.Fatal("the seam never ran: the test proves nothing about the window it exists for")
	}
	info := db.db.dbInfo.Load()
	if info.Epoch == epochBefore {
		t.Fatalf("the handoff did not move the published epoch (still %d).\n"+
			"Re-publishing an identical proxy set under a new epoch must still be "+
			"published, or the fence word moves while DBInfo.Epoch stays behind and "+
			"every later request binds an epoch the fence has already passed",
			epochBefore)
	}

	if f := db.db.grvCache.entryFloor(); f < cv {
		t.Fatalf("floor = %d after a commit at %d that crossed a handoff mid-window.\n"+
			"The commit ran against the CURRENT cluster's proxies, so its version is a "+
			"durable fact about THIS cluster and must floor. Sampling the epoch before "+
			"selecting the proxy leaves the commit carrying the previous cluster's "+
			"epoch, sameEpoch fails, the floor is never raised — and a lower GRV from "+
			"this cluster then repopulates the cache underneath a write the caller was "+
			"told had committed", f, cv)
	}

	// And the consequence the floor exists for, stated directly.
	if v, _, ok := db.db.grvCache.tryCache(grvPriorityDefault); ok && v < cv {
		t.Fatalf("the cache serves version %d below the committed version %d", v, cv)
	}
}

// TestEpoch_ProxyAndEpochComeFromOneLoad is the structural companion: both
// accessors hand back the epoch of the snapshot they read, so there is no
// second load in which the two could disagree. It is asserted by moving the
// epoch underneath a HELD snapshot — the accessor's answer must describe the
// snapshot it returned, not the state of the world afterwards.
func TestEpoch_ProxyAndEpochComeFromOneLoad(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{
		GRVProxies:    []ProxyInfo{{Address: "a:4500"}},
		CommitProxies: []ProxyInfo{{Address: "a:4501"}},
	})

	proxy, commitEpoch, err := db.getCommitProxy()
	if err != nil {
		t.Fatalf("getCommitProxy: %v", err)
	}
	grvProxies, grvEpoch := db.getGRVProxies()
	if len(grvProxies) == 0 {
		t.Fatal("getGRVProxies returned nothing")
	}
	if proxy.Address != "a:4501" || grvProxies[0].Address != "a:4500" {
		t.Fatalf("wrong proxies: commit %q, grv %q", proxy.Address, grvProxies[0].Address)
	}
	if commitEpoch != 0 || grvEpoch != 0 {
		t.Fatalf("epochs = (commit %d, grv %d), want (0, 0)", commitEpoch, grvEpoch)
	}

	// The handle moves to another cluster.
	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{
		GRVProxies:    []ProxyInfo{{Address: "b:4500"}},
		CommitProxies: []ProxyInfo{{Address: "b:4501"}},
	})

	proxy, commitEpoch, err = db.getCommitProxy()
	if err != nil {
		t.Fatalf("getCommitProxy after handoff: %v", err)
	}
	grvProxies, grvEpoch = db.getGRVProxies()
	if proxy.Address != "b:4501" || grvProxies[0].Address != "b:4500" {
		t.Fatalf("stale proxies after handoff: commit %q, grv %q", proxy.Address, grvProxies[0].Address)
	}
	if commitEpoch != 1 || grvEpoch != 1 {
		t.Fatalf("epochs = (commit %d, grv %d), want (1, 1): an accessor must hand back "+
			"the epoch of the snapshot it read, so the proxies and the epoch a request "+
			"binds always describe the same cluster", commitEpoch, grvEpoch)
	}
	if got := db.grvCache.epochNow(); got != 1 {
		t.Fatalf("fence epoch = %d, want 1: the fence and the published DBInfo are two "+
			"views of one fact and installProxySet is what keeps them coherent", got)
	}
}

// TestEpoch_PublishedEpochNeverExceedsTheFence states the ORDER invariant, which
// is what makes the only reachable disagreement the safe one.
//
// Bumped first, the single window is (new fence, old proxies): a request binding
// there carries the OLD epoch, talks to the OLD cluster, and is refused —
// correct, not merely conservative. Reversed, the window becomes (old fence, new
// proxies): a request binds the NEW epoch while still holding the previous
// cluster's proxies, and its reply installs another cluster's version as current.
//
// The final equality is the deterministic pin. The spinning observer is a PROBE
// in the sense this package uses the word: the reversed order's window is a
// couple of instructions wide, so a green run is weak evidence while a red one
// is proof. It is kept for the red case and deliberately not relied on.
func TestEpoch_PublishedEpochNeverExceedsTheFence(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	// Observe the fence from inside the publication, at the instant the new
	// DBInfo is being installed: the entry accessor below runs while the store is
	// still ahead of us in installProxySet only if the fence already moved.
	db.onCoordinatorSetAdopted()
	fenceDuringPublish := int64(-1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nothing to synchronise against without a second seam, so assert the
		// invariant that must hold at EVERY instant instead: the published epoch
		// never exceeds the fence.
		for i := 0; i < 10000; i++ {
			f := db.grvCache.epochNow()
			if info := db.dbInfo.Load(); info != nil && info.Epoch > f {
				fenceDuringPublish = info.Epoch
				return
			}
		}
	}()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})
	<-done

	if fenceDuringPublish >= 0 {
		t.Fatalf("observed a published DBInfo at epoch %d while the fence was still "+
			"behind it.\n"+
			"That is the reversed order, and its window is the harmful one: a request "+
			"binding there carries the NEW epoch while holding the PREVIOUS cluster's "+
			"proxies, so its reply installs another cluster's version as current",
			fenceDuringPublish)
	}
	if got, want := db.dbInfo.Load().Epoch, db.grvCache.epochNow(); got != want {
		t.Fatalf("after the handoff the published epoch is %d and the fence %d: outside "+
			"the handoff instant they must agree, or every publication is refused",
			got, want)
	}
}

// TestEpoch_HandoffResetsMinAcceptableReadVersion covers the database-level fact
// C++ resets in the same block as its cache reset (switchConnectionRecord,
// NativeAPI.actor.cpp:2201, to max() — its "unset"), and that Go was carrying
// across.
//
// Go's floor is the SMALLEST read version this handle has seen and 0 means
// unset. Carried into a cluster whose version space sits LOWER, it rejects
// perfectly current user-set versions with transaction_too_old until this
// handle's own first GRV happens to lower it. That direction — the new cluster
// being lower — is exactly the case the epoch machinery exists for, so it is the
// last one to leave to chance.
func TestEpoch_HandoffResetsMinAcceptableReadVersion(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	// Cluster A's version space is high.
	updateMinAcceptable(&db.minAcceptableReadVersion, 9_000_000)
	if err := db.validateVersion(500); err == nil {
		t.Fatal("precondition: 500 must be rejected against cluster A's floor")
	}

	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})

	if got := db.minAcceptableReadVersion.Load(); got != 0 {
		t.Fatalf("minAcceptableReadVersion = %d after a coordinator handoff, want 0 "+
			"(unset).\n"+
			"It is the smallest version seen on the PREVIOUS cluster. Against a cluster "+
			"whose versions are lower it rejects current versions as transaction_too_old "+
			"until this handle's first GRV happens to lower it — and C++ does not carry "+
			"it either (switchConnectionRecord resets it to max(), then re-derives it "+
			"from the new cluster's first GRV)", got)
	}
	if err := db.validateVersion(500); err != nil {
		t.Fatalf("validateVersion(500) = %v after the handoff, want nil: a version from "+
			"the cluster the handle now serves must not be rejected on the strength of "+
			"the previous cluster's version space", err)
	}
}

// TestEpoch_IdenticalProxySetUnderANewEpochIsStillPublished is the wedge this
// would otherwise walk into. A handoff onto a coordinator set that publishes the
// same proxy list is a real case (the same servers, reached through different
// coordinators). If dbInfoEqual ignored the epoch, that installation would be
// skipped as "no change" — leaving the fence bumped and DBInfo.Epoch behind
// forever, so every later request binds an epoch the fence has passed and every
// publication is refused. Permanently, with no operator escape.
func TestEpoch_IdenticalProxySetUnderANewEpochIsStillPublished(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	same := []ProxyInfo{{Address: "a:4500"}}
	db.installProxySet(&DBInfo{GRVProxies: same, CommitProxies: same})

	db.onCoordinatorSetAdopted()
	if !db.installProxySet(&DBInfo{GRVProxies: same, CommitProxies: same}) {
		t.Fatal("an identical proxy set under a NEW epoch was skipped as unchanged.\n" +
			"The fence has already moved, so DBInfo.Epoch would stay behind it forever: " +
			"every request binds an epoch the fence has passed, every publication is " +
			"refused, and the cache never serves again")
	}
	if got, want := db.dbInfo.Load().Epoch, db.grvCache.epochNow(); got != want {
		t.Fatalf("published epoch %d, fence %d", got, want)
	}

	// And the control: without a pending adoption, an identical set is still a
	// no-op, or every topology poll would broadcast a spurious proxy change and
	// fail every in-flight commit with commit_unknown_result.
	if db.installProxySet(&DBInfo{GRVProxies: same, CommitProxies: same}) {
		t.Fatal("an identical proxy set at the SAME epoch was republished: every " +
			"topology poll would broadcast a proxy change and fail in-flight commits")
	}
}
