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
	"sync"
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
	db.updateMinAcceptable(db.grvCache.epochNow(), 9_000_000)
	if err := db.validateVersion(500); err == nil {
		t.Fatal("precondition: 500 must be rejected against cluster A's floor")
	}

	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})

	if got := db.minAcceptable(); got != 0 {
		t.Fatalf("minAcceptable() = %d after a coordinator handoff, want 0 "+
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

// TestEpoch_InstallIsSerialised pins the read-modify-write. installProxySet
// loads dbInfo, derives the next epoch from it, may bump the fence, and stores:
// atomic.Pointer makes each step atomic and the SEQUENCE not.
//
// A lost update here does not self-correct on the next poll. The losing writer
// publishes DBInfo.Epoch=N after the fence moved to N+1 with
// clusterSwitchPending already consumed, so nothing will ever bump DBInfo.Epoch
// again: every request binds an epoch the fence has passed, every publication is
// refused, and the refresher pins at its 1ms clamp for the life of the handle.
//
// Asserted by holding one installer inside the critical section and requiring
// the second to block — deterministic, where racing two installers would only
// occasionally interleave.
func TestEpoch_InstallIsSerialised(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	hook := func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	db.duringProxyInstall.Store(&hook)
	defer db.duringProxyInstall.Store(nil)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		db.onCoordinatorSetAdopted()
		db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})
	}()
	<-entered // the first installer is mid-publication

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "c:4500"}}})
	}()

	select {
	case <-secondDone:
		t.Fatal("a second installProxySet completed while the first was mid-publication.\n" +
			"The sequence is a read-modify-write on dbInfo: interleaved, the loser " +
			"publishes an epoch the fence has already passed with clusterSwitchPending " +
			"spent, and nothing ever republishes. That is a permanent wedge, not a " +
			"stale read — every request binds a dead epoch and the cache never serves " +
			"again")
	case <-time.After(200 * time.Millisecond):
		// Blocked on installMu, as required.
	}

	close(release)
	<-firstDone
	<-secondDone

	if got, want := db.dbInfo.Load().Epoch, db.grvCache.epochNow(); got != want {
		t.Fatalf("after two concurrent installations the published epoch is %d and the "+
			"fence %d: a lost update left the handle permanently unable to publish",
			got, want)
	}
}

// TestEpoch_CommitEpochIsTheValueProxySelectionReturned closes the sibling of
// the window the epoch-from-the-snapshot fix closed. Reading the fence again
// next to getCommitProxy — rather
// than using the epoch it RETURNED — passes every test that fires the handoff
// completely before proxy selection, because there the two agree.
//
// They disagree exactly during a handoff: the fence moves first, so there is a
// window where it is ahead of the published snapshot. A commit binding there
// uses the PREVIOUS cluster's proxies, so it must carry the previous cluster's
// epoch and be refused — a re-read of the fence would hand it the new epoch, and
// its floor would be credited to a cluster it never talked to.
func TestEpoch_CommitEpochIsTheValueProxySelectionReturned(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	key := []byte("epoch-commit-returned-" + t.Name())

	// Move the fence WITHOUT publishing a new snapshot: the state mid-handoff.
	// The commit below therefore binds the proxies — and the epoch — of the set
	// still published, which is the previous cluster's.
	var fired bool
	hook := func() {
		if fired {
			return
		}
		fired = true
		db.db.grvCache.resetForNewCoordinators()
	}
	db.db.beforeCommitProxySelect.Store(&hook)
	defer db.db.beforeCommitProxySelect.Store(nil)

	boundEpoch := db.db.dbInfo.Load().Epoch

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
		t.Fatal("the seam never ran")
	}
	if db.db.grvCache.epochNow() == boundEpoch {
		t.Fatal("precondition: the fence did not move ahead of the published snapshot")
	}

	if got := tx.commitEpoch.Load(); got != boundEpoch {
		t.Fatalf("commitEpoch = %d, want %d — the epoch getCommitProxy RETURNED.\n"+
			"Re-reading the fence beside the proxy selection instead hands the commit "+
			"the epoch of a cluster whose proxies it did not use. Every test that fires "+
			"the handoff cleanly before proxy selection passes either way, because there "+
			"the two agree; they disagree only inside the handoff, which is the case "+
			"that matters", got, boundEpoch)
	}
	if f := db.db.grvCache.entryFloor(); f >= cv {
		t.Fatalf("floor = %d after a commit at %d that used the PREVIOUS snapshot's "+
			"proxies.\n"+
			"That commit bound the old epoch, so it must be refused — crediting its "+
			"floor to the current epoch attributes a durable fact to a cluster it never "+
			"talked to, which is exactly the confusion the epoch exists to prevent",
			f, cv)
	}
}

// TestEpoch_PublishedEpochIsTheFenceAtPublication is the PLACEMENT-INDEPENDENT
// pin for the order invariant, and it is deliberately anchored to the ACT rather
// than to any seam.
//
// Two pins have now failed on this property, both the same way. A spinning
// observer could not win a two-instruction window. Its replacement bracketed the
// publication with a hook and claimed that "whichever statement moves, one
// observation falls inside the window" — which is false: a bump moved to just
// after the store, still inside the bracket, left both observations consistent
// and the window fully live. Both tested an artifact of where the observer sat.
//
// So this asserts a property of the RESULT, which no placement can dodge: after
// a handoff the published epoch equals the fence. The published value is read
// FROM the fence at publication, so any bump that happens after that read leaves
// the snapshot carrying the older epoch — the fence ends up ahead of the
// snapshot, which is the safe direction, and this equality catches it.
//
// Verified against four mutants: the bump moved to immediately after the store
// (inside the seam bracket, the placement that defeated the previous pin), after
// the trailing hook, after the broadcast, and — the one case an end-state
// equality alone cannot see — a computed epoch combined with a displaced bump,
// caught by the second assertion below.
func TestEpoch_PublishedEpochIsTheFenceAtPublication(t *testing.T) {
	t.Parallel()

	db := &database{proxiesChanged: make(chan struct{})}
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "a:4500"}}})
	if got, want := db.dbInfo.Load().Epoch, db.grvCache.epochNow(); got != want {
		t.Fatalf("precondition: published %d, fence %d", got, want)
	}

	// The SECOND assertion, and load-bearing rather than decorative — the two
	// cover different mutant classes and neither subsumes the other:
	//
	//   - the equality below catches every DISPLACEMENT of the bump while the
	//     derivation stands (after the store, after the trailing hook, after the
	//     broadcast): the published value was read before the bump, so the handle
	//     ends up with the fence ahead of the snapshot.
	//   - this one catches the bump displaced together with a REVERTED derivation
	//     (a computed old+1 rather than a read of the fence). That combination
	//     lands published == fence at the end, so the equality alone is green,
	//     while a snapshot really was published ahead of the fence in between.
	//
	// This one is hook-relative, which is why it is the secondary: it is the
	// weaker of the two and must never be the only pin.
	var fenceOnEntry int64 = -1
	hook := func() {
		if fenceOnEntry < 0 {
			fenceOnEntry = db.grvCache.epochNow()
		}
	}
	db.duringProxyInstall.Store(&hook)
	defer db.duringProxyInstall.Store(nil)

	db.onCoordinatorSetAdopted()
	db.installProxySet(&DBInfo{GRVProxies: []ProxyInfo{{Address: "b:4500"}}})

	published := db.dbInfo.Load().Epoch
	fence := db.grvCache.epochNow()

	if published != fence {
		t.Fatalf("after a coordinator handoff the published epoch is %d and the fence "+
			"%d.\n"+
			"The published value must be READ FROM the fence at publication. Derived "+
			"from anything earlier — the old snapshot, the bump's return value, a fence "+
			"read taken before a bump that has since been displaced — the snapshot "+
			"carries an epoch from another instant. Ahead of the fence it costs a "+
			"commit its durable floor; behind it, as here, the handle publishes an "+
			"epoch the fence has already passed with clusterSwitchPending spent",
			published, fence)
	}
	if fenceOnEntry != fence {
		t.Fatalf("the fence read on entry to the publication was %d but the handle "+
			"settled at %d: the bump did not happen before the value that gets "+
			"published was determined", fenceOnEntry, fence)
	}
	if published == 0 {
		t.Fatal("the handoff did not move the epoch at all")
	}
}
