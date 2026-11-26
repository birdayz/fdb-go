package client

import "fdb.dev/internal/fdbwire"

// invalidate clears the cached version.
// invalidate drops the cached VERSION.
//
// THE GUARANTEE: no version obtained before this call is served afterwards.
// That is the whole point of the API — a caller invalidates because an
// external write happened, so a version that predates the call would answer
// with data the caller knows is superseded.
//
// WHAT ENFORCES IT: the generation counter, not the sentinel. Clearing the
// entry alone is not enough, because a reply already inside publishReply can
// lose its CAS to this one, reload the sentinel, and install a version this
// call was meant to retire — the sentinel makes the reply look FRESHER than
// it is (version 0 makes any version look like an advance). Bumping the
// generation FIRST, before the sentinel is installed, closes that: a reply
// that captured the old generation may from then on contribute only its
// cooldown. The order matters — installing the sentinel first would leave a
// window where a reply reloads the sentinel while still reading the old
// generation, which is the same bug with extra steps.
//
// The ratekeeper cooldowns are deliberately PRESERVED: a throttle signal is
// about the cluster, not about the version that carried it, and forgetting it
// here would let the next un-throttled reply serve a cache the ratekeeper is
// still suppressing.
func (c *grvCache) invalidate() {
	c.bumpFences(false)
	for {
		cur := c.entry.Load()
		if cur == nil {
			return
		}
		// The floor SURVIVES, like the cooldowns: invalidation is about
		// freshness (what is recent enough to serve), the floor is about order
		// (what is old enough to be wrong). Clearing it would let the next low
		// publication become servable again and re-open the stale read a
		// committed version had already ruled out.
		// The sentinel inherits cur's EPOCH along with the floor and cooldowns.
		// Without it the sentinel reads as epoch 0 while the cache is at epoch N,
		// and the next publication discards it as a previous-epoch entry —
		// losing the committed floor and the cooldowns an invalidation is
		// explicitly meant to preserve.
		next := grvCacheEntry{epoch: cur.epoch, rkDefault: cur.rkDefault, rkBatch: cur.rkBatch, floor: cur.floor}
		if c.entry.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// minAcceptableStamp is the seen-version floor together with the cluster epoch
// it was observed in. One value, for the reason grvCacheEntry carries its epoch:
// a floor is a claim about a VERSION SPACE, and a reader must decide whether the
// claim applies to the cluster it is serving from the SAME load that produced the
// version. Zero version means unset.
type minAcceptableStamp struct {
	epoch   int64
	version int64
}

// minAcceptable is the seen-version floor for the epoch currently being served,
// or 0 when there is none. A stamp from a previous cluster reads as unset, which
// is the fail-open direction and, more importantly, the direction that RE-ARMS
// the bootstrap GRV in ensureReadVersion — the repair that re-derives the floor
// from the cluster now being served.
func (db *database) minAcceptable() int64 {
	s := db.minAcceptableReadVersion.Load()
	if s == nil || s.epoch != db.grvCache.epochNow() {
		return 0
	}
	return s.version
}

// updateMinAcceptable lowers this epoch's observed floor (C++ std::min; 0 is unset).
// Later GRVs cannot raise this storage/commit guard; it is not an MVCC boundary.
//
// THE EPOCH CHECK IS ON THE STORE ITSELF, and that placement is the point. The
// caller already knows whether the reply's epoch was current — but it learned
// that a few instructions earlier, and a handoff in between would let a reply
// from the previous cluster write that cluster's version space into this one's
// floor. Re-reading the epoch inside the loop means the decision is made where
// the write happens: replyEpoch is compared against the epoch observed at the
// CAS, so a handoff anywhere before it refuses the write instead of racing it.
//
// Refusing costs nothing, which is why the check can be this strict: a refused
// floor reads as unset, and unset re-arms the bootstrap GRV that establishes the
// floor properly from the current cluster.
func (db *database) updateMinAcceptable(replyEpoch, v int64) {
	if v <= 0 {
		return
	}
	// THE OBSERVATION IS TAKEN FIRST, before the epoch is checked, and the CAS is
	// pinned to it. That order is the whole safety argument, and reversing it —
	// checking the epoch and only then loading what to replace — reopens the
	// defect through the scheduler rather than through the code:
	//
	//	this goroutine reads the epoch and passes the check, then loses the
	//	processor; a handoff resets the stamp and a GRV on the new cluster installs
	//	a legitimate floor; this goroutine resumes, loads THAT fresh stamp, and
	//	CASes it away — replacing a valid current-epoch floor with an inert
	//	previous-epoch one. The guard the current cluster established is gone and
	//	the read fails open.
	//
	// Loading first makes the window unreachable. Anything that lands in it
	// changes the stamp pointer — the handoff's own empty-stamp publication is
	// enough on its own — so the CAS fails and the retry re-observes and refuses
	// on the epoch. Both the load and the check live inside the loop, so every
	// attempt CASes against the same observation it validated.
	for {
		cur := db.minAcceptableReadVersion.Load()
		if db.updateMinAcceptableOnce(cur, replyEpoch, v) {
			return
		}
	}
}

// updateMinAcceptableOnce is one attempt against the observation `cur`. It
// returns true when the attempt is settled — installed, or refused for good —
// and false when the caller must re-observe and try again.
//
// The observation is a PARAMETER so the dangerous interleaving is expressible
// without a seam: a test can take the stamp a stale goroutine would have seen,
// let a handoff and a fresh install happen, and then present that stale
// observation here. The alternative — loading inside — makes the property
// testable only by racing the scheduler, which is how it went unnoticed.
func (db *database) updateMinAcceptableOnce(cur *minAcceptableStamp, replyEpoch, v int64) bool {
	curEpoch := db.grvCache.epochNow()
	if replyEpoch != curEpoch {
		return true // a previous cluster's version space; not this cluster's floor
	}
	if cur != nil && cur.epoch == curEpoch && cur.version != 0 && v >= cur.version {
		return true // already hold a smaller-or-equal floor for this epoch
	}
	return db.minAcceptableReadVersion.CompareAndSwap(cur, &minAcceptableStamp{epoch: curEpoch, version: v})
}

// validateVersion retains storage/commit floor and absurd-future fail-fast checks.
// It neither validates explicit-pin getters nor models server snapshot retention.
func (db *database) validateVersion(version int64) error {
	// Read through the stamp: a floor observed under a previous cluster reads as
	// unset here, because its version space says nothing about this one.
	min := db.minAcceptable()
	if min > 0 && version < min {
		return &wire.FDBError{Code: ErrTransactionTooOld}
	}
	if version > 1_000_000_000_000_000 {
		return &wire.FDBError{Code: ErrFutureVersion}
	}
	return nil
}

// resetForNewCoordinators discards every CLUSTER-SCOPED fact in the cache after
// the handle adopts a coordinator set it did not previously have — a followed
// coordinator forward, or a cluster file another process rewrote.
//
// Neither adoption path validates cluster identity (nor does the C++ client
// they are ported from: MonitorLeader.actor.cpp adopts a forwarded connection
// string and a rewritten cluster file without comparing cluster ids, and
// ClientDBInfo.clusterId is never diffed anywhere in the C++ client), so a
// handle can end up bound to an unrelated version space. Everything the cache
// holds is then meaningless: the version and its anchor, the freshness, the
// ratekeeper cooldowns — those describe another cluster's ratekeeper — and
// above all the committed-version FLOOR.
//
// The floor is the dangerous one. It is a high-water mark with no expiry and no
// operator escape (invalidate() preserves it by design, correctly, because
// within one cluster a committed version stays committed). Carried into a
// cluster whose versions are lower, it refuses every reply permanently: the
// cache never serves again and the background refresher sits at its 1ms clamp.
// That is a liveness failure with no recovery short of reopening the handle.
//
// Bumping the generation rather than only clearing the entry is what refuses
// LATE replies — a GRV or commit dispatched to the previous cluster that lands
// after the switch carries the old generation and can no longer install. Same
// dispatch-capture pattern as the invalidation, one level up, and reusing that
// counter rather than adding a second one keeps a single answer to "may this
// reply install?".
//
// C++ resets the analogous state on its explicit cluster-switch path
// (switchConnectionRecord, NativeAPI.actor.cpp:2191-2213, "Reset state from
// former cluster": proxies, location cache, minAcceptableReadVersion,
// ssVersionVectorCache). It notably does NOT clear cachedReadVersion or
// lastGrvTime there, which leaves its own GRV cache stale across a switch —
// this is deliberately stricter, because Go's floor makes the consequence
// permanent rather than bounded by the 100ms freshness window.
//
// db.minAcceptableReadVersion is reset too, but by installProxySet rather than
// here: it is a database field, not a cache field, exactly as it is in C++. It
// genuinely needs the reset. Go's is the SMALLEST read version this handle has
// seen and 0 means unset, so carrying the previous cluster's value into a
// cluster whose version space sits LOWER rejects perfectly current user-set
// versions with transaction_too_old until this handle's own first GRV happens to
// lower it. That direction — the new cluster being lower — is the whole reason
// the epoch exists, so it is precisely the case not to leave to chance.
// It returns the epoch it installed, for callers that want to assert on the
// bump. installProxySet deliberately does NOT use it: it re-reads the fence at
// publication instead, so the epoch it publishes is a fact about the fence at
// that instant rather than about when this bump happened. That is what makes the
// coherence rule hold however the two are arranged — see database.installProxySet.
func (c *grvCache) resetForNewCoordinators() int64 {
	// Bumping the fences is the WHOLE reset. The entry is deliberately NOT
	// cleared: every entry records the epoch it was published under, so one
	// from the previous cluster is already unservable and unusable as a merge
	// base, and it is overwritten by the next publication.
	//
	// Clearing it would introduce a race it cannot win — a publisher that
	// landed under the NEW epoch between the fence bump and the clear would
	// have its perfectly valid version and floor erased by a reset that was
	// never about it.
	epoch := fenceEpoch(c.bumpFences(true))

	// lastProxyContact is the one cluster-scoped fact NOT carried on the entry,
	// so nothing about the new epoch makes it inert — it is a bare instant and
	// would keep describing when the PREVIOUS cluster's proxies were last
	// reached. The refresher subtracts it from MAX_PROXY_CONTACT_LAG, so a
	// carried-over instant grants a contact budget that was never earned against
	// the cluster now being served, delaying the very refresh that repopulates
	// the cache under the new epoch. Zeroing it means the next refresh is due
	// immediately, which is exactly right after a cluster change.
	//
	// The publication path gates its stores on the epoch, so this cannot be
	// re-polluted by a straggler; this clears what was already there.
	c.lastProxyContact.Store(0)
	return epoch
}

// bumpFences advances the generation, and the epoch too when newCluster.
// Returns the fence word it installed.
//
// A CAS loop rather than an Add, deliberately: an Add on a packed word carries
// between the halves, so the low half's wrap at 2^32 would silently increment
// the cluster epoch and make every live entry unservable. Recomputing both
// halves under a CAS keeps them independent and removes the carry question
// entirely. Both callers are rare (an explicit InvalidateGRVCache, a
// coordinator-set handoff), so the loop costs nothing that matters.
func (c *grvCache) bumpFences(newCluster bool) uint64 {
	for {
		cur := c.fences.Load()
		epoch := fenceEpoch(cur)
		if newCluster {
			epoch++
		}
		next := packFences(epoch, fenceGen(cur)+1)
		if c.fences.CompareAndSwap(cur, next) {
			return next
		}
	}
}

// fenceGenMask is the low half of the packed fence word — the generation.
const fenceGenMask = uint64(1)<<32 - 1

// packFences builds a fence word from an epoch and a generation. The generation
// is masked to its own half, so it wraps within itself and can never carry into
// the epoch.
func packFences(epoch, gen int64) uint64 {
	return uint64(epoch)<<32 | uint64(gen)&fenceGenMask
}

func fenceEpoch(f uint64) int64 { return int64(f >> 32) }
func fenceGen(f uint64) int64   { return int64(f & fenceGenMask) }

// cacheToken is the fence word a publisher captures AT DISPATCH and must present
// to publish anything.
//
// One value rather than two parameters so the API gate stays a single required
// argument — a publisher cannot compile without deciding when it captured — and
// one WORD rather than two fields so the epoch and the generation it presents
// are necessarily from the same instant.
type cacheToken struct {
	f uint64
}

func (t cacheToken) epoch() int64 { return fenceEpoch(t.f) }
func (t cacheToken) gen() int64   { return fenceGen(t.f) }

// withEpoch rebinds a token to the epoch of the proxy set the attempt ACTUALLY
// used, keeping the generation it captured at dispatch. The two are captured at
// different instants by necessity — the generation covers the whole round trip
// from dispatch, while the epoch belongs to the proxy set selected later,
// possibly on a retry — and this is the only sanctioned way to combine them.
//
// A rebound token whose epoch is older than the observed fence fails `obs ==
// tok.f` and is refused: the request talked to the previous cluster, so refusing
// it is the correct outcome, not a conservative approximation of one.
//
// THE PREMISE this rests on: every epoch bump also bumps the generation
// (bumpFences). Rebinding replaces one half of a word whose halves are otherwise
// only ever set together, so it could in principle synthesise a pair the fence
// never held — and `obs == tok.f` would then accept it. It cannot, because a
// fence word at epoch E only ever exists with the generation that arrived
// alongside E or later: matching the whole word means matching a pair that
// really occurred. An epoch bump that left the generation alone would break
// exactly this, which is why it is pinned rather than assumed.
func (t cacheToken) withEpoch(epoch int64) cacheToken {
	return cacheToken{f: packFences(epoch, t.gen())}
}

// token captures the current fences in ONE load. Call at DISPATCH, never at
// publication.
func (c *grvCache) token() cacheToken {
	return cacheToken{f: c.fences.Load()}
}

// epochNow is the cluster epoch the cache currently serves under. Every entry
// accessor validates against it, so an entry minted by a previous cluster is
// inert rather than merely stale.
func (c *grvCache) epochNow() int64 { return fenceEpoch(c.fences.Load()) }
