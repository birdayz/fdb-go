package client

import (
	"sync/atomic"
	"time"
)

// GRV cache knobs — matching C++ CLIENT_KNOBS defaults.
const (
	maxVersionCacheLag = 100 * time.Millisecond // MAX_VERSION_CACHE_LAG = 0.1s
	maxProxyContactLag = 200 * time.Millisecond // MAX_PROXY_CONTACT_LAG = 0.2s
	grvCacheRKCooldown = 60 * time.Second       // GRV_CACHE_RK_COOLDOWN = 60s
)

// grvCache holds the cached read version state.
// C++: DatabaseContext fields cachedReadVersion, lastGrvTime,
//
//	lastProxyRequestTime, lastRkBatchThrottleTime, lastRkDefaultThrottleTime.
//
// C++ does NOT explicitly invalidate this cache on proxy change — it relies
// on natural expiry via MAX_VERSION_CACHE_LAG (100ms). We match this behavior.
type grvCacheTestHooks struct {
	// now is the clock freshness is judged against. A seam, not a knob: the
	// freshness window is 100ms, so a test that compares against the real
	// wall clock is one GC pause away from failing on a CORRECT
	// implementation. nil means time.Now.
	now func() time.Time
}

type grvCache struct {
	hooks *grvCacheTestHooks

	// entry is the cached version plus its two instants, published as ONE
	// value. The (version, anchor) half is a PAIR and must never be observed
	// torn; freshness rides along because it is written by the same events,
	// not because it moves with them (see publish for the two rules).
	//
	// Two independently-published atomics cannot express this: a reader that
	// loads the version before a writer's store and the instant after it
	// observes a pair that never existed. When the instant is the newer half,
	// the anchor claims a later MVCC-window start than the version really
	// has, the RFC-198 budget under-counts the age, and the page it admits
	// dies on 1007. C++ WRITES both fields inside one critical section
	// (updateCachedReadVersionShared, NativeAPI.actor.cpp:348-359, under
	// MutexHolder) but READS them through two separate acquisitions —
	// getCachedReadVersion() then getLastGrvTime(), :7511-7512 — so its reader
	// can observe a torn pair. It survives that because it uses lastGrvTime only
	// for FRESHNESS and never derives an anchor from it: a torn instant costs it
	// at most a spurious cache miss. Go's anchor accessor is the extra consumer
	// that turns the same tear into a wrong MVCC budget, so Go's single
	// publication is strictly stronger than C++'s — and has to be.
	//
	// nil means empty. Both instants are stored as time.Time so their
	// MONOTONIC readings survive: round-tripping through UnixNano drops them,
	// and a backward wall-clock step then corrupts every anchor derived from
	// the one and every freshness verdict derived from the other.
	entry atomic.Pointer[grvCacheEntry]

	// publications counts successful entry publications. It exists because the
	// cache's central safety property — that everything tryCache consults
	// arrives in ONE publication — is otherwise observable only by catching a
	// window a few nanoseconds wide, i.e. by luck. A test that can only
	// probabilistically catch a regression is not a regression net; counting
	// publications turns the property into something assertable exactly.
	publications atomic.Int64

	// fences is BOTH fence counters in ONE word: the cluster epoch in the high
	// 32 bits, the invalidation generation in the low 32.
	//
	// The GENERATION counts invalidations. A reply may install its VERSION only
	// against the generation it was validated under; once the generation has
	// moved it may still contribute its ratekeeper cooldown — a fact about the
	// cluster, not about the version that carried it — but never its version.
	// Without it the load-validate-CAS loop can be turned against itself: a
	// reply correctly rejected as stale against version N loses its CAS to an
	// invalidation, re-reads the version-0 sentinel, no longer looks stale
	// against 0, and installs the very version it was just rejected for —
	// resurrecting a PRE-invalidation version inside the freshness window.
	//
	// The EPOCH counts CLUSTER changes and is a strictly coarser fence. Both
	// halves move when the handle moves to another cluster; only the generation
	// moves on an invalidation. Two fences because install and floor need
	// DIFFERENT scopes: a commit blocked by an invalidation must still raise the
	// floor — same cluster, so its version genuinely proves lower ones stale —
	// while a commit from a PREVIOUS CLUSTER must raise nothing, because across
	// clusters a version comparison is meaningless (the two number spaces are
	// unrelated, so "9000000 > 500" says only which cluster minted it).
	//
	// ONE WORD, not two atomics, because the two predicates must be derived from
	// the SAME observation. With separate counters a publisher that tokened
	// before a coordinator switch can observe the state BETWEEN the two bumps
	// and conclude mayInstall=true (generation not yet bumped) AND
	// sameEpoch=false (epoch already bumped) — a combination that describes no
	// reachable state of the cache, and under which mergeReply installs a
	// previous-cluster version over a valid current-epoch entry while declining
	// to stamp it, leaving the cache holding an unservable epoch-0 entry. Packed,
	// mayInstall is `obs == tok` and therefore IMPLIES sameEpoch: the skew is
	// unrepresentable rather than merely unlikely.
	fences atomic.Uint64

	lastProxyContact atomic.Int64 // UnixNano
	// Like C++'s opt-in GRV cache, this cache does not carry lock state;
	// Transaction.readVersionForOperation checks locks only on fresh-GRV replies.
}

// grvCacheEntry is the cache's published state. Immutable once published —
// writers build a new one rather than mutating.
//
// anchor and freshness are DIFFERENT instants with different rules (see
// publish). anchor belongs to version and answers the MVCC-budget question;
// freshness is monotonic and answers the cache-staleness question. They ride
// one entry so the (version, anchor) pair can never be observed torn, not
// because they move together — they usually do not.
type grvCacheEntry struct {
	// epoch is the cluster epoch this entry was published under. Carried IN the
	// entry so a reader validates it in the SAME atomic load that produced the
	// entry: comparing a separately-loaded epoch against a separately-loaded
	// entry has a window where the two disagree, and that window is served.
	//
	// The comparison is conservative in the safe direction, which is why no
	// atomicity between the epoch counter and this entry is needed: a reader
	// that sees a NEW epoch counter beside an OLD entry refuses (correct), and
	// one that sees an old counter beside an old entry serves an entry that
	// was still current when it loaded it (also correct).
	epoch int64

	version   int64
	anchor    time.Time
	freshness time.Time

	// floor is the highest version this client has observed as COMMITTED. No
	// version below it is ever served, whenever it arrives and whoever
	// publishes it: a durable commit proves every lower version predates it,
	// so serving one is a stale read by construction.
	//
	// It is an ORDER invariant, which is why it needs no generation — the same
	// reason the version comparison in a stale-reply rejection needs none.
	// Evictions cannot do this job: an eviction is point-in-time, and a
	// publisher dispatched before the commit can land arbitrarily later and
	// repopulate what was evicted. The floor refuses it on arrival instead.
	//
	// MONOTONIC, and deliberately NOT reset by invalidate(): invalidation is
	// about freshness (what is recent enough to serve), the floor is about
	// order (what is old enough to be wrong). Clearing it would let the next
	// low publication become servable again.
	//
	// It cannot wedge the cache permanently: a client whose GRVs lag its last
	// commit simply pays real GRVs until the cluster's versions catch up,
	// which is the correct behaviour and not the cache-hit regression it can
	// look like in a hit-rate test.
	floor int64

	// rkDefault and rkBatch are the ratekeeper cooldown stamps tryCache
	// consults alongside freshness. They live HERE, not in their own atomics,
	// because serving is decided by freshness AND cooldown together: with two
	// separately-published values a reader can catch a renewed freshness
	// before this reply's cooldown lands and be served, bypassing the very
	// throttling the reply was delivering. One publication makes that window
	// impossible by construction rather than by statement order, which is a
	// property the next edit to applyGRVReply cannot silently break.
	//
	// Zero means never throttled at that priority.
	rkDefault time.Time
	rkBatch   time.Time
}

// publish records a successful GRV/commit observation: version v, obtained by
// a request dispatched at at.
//
// It maintains TWO instants with OPPOSITE rules, which is why they cannot be
// one field:
//
//   - anchor is when THIS VERSION's MVCC window opened. For a given version
//     that is a fixed fact, so a later observation of the same version must
//     not move it: the window did not reopen, and claiming it did over-grants
//     the RFC-198 budget until a page dies on 1007. It travels with its
//     version — a higher version keeps ITS OWN pre-dispatch time even when
//     that is earlier than the previous entry's, which C++ explicitly expects
//     ("it's possible that we get a newer version with an older time",
//     NativeAPI.actor.cpp:374-376). Correct for that version, and conservative.
//
//   - freshness is when the cache was last CONFIRMED with the cluster. Every
//     accepted reply confirms it, including one returning the version we
//     already hold: a quiet cluster returns the same version repeatedly, and
//     that is a freshly-confirmed cache, not a stale one. Monotonic, exactly
//     as C++ keeps lastGrvTime (`if (t > lastGrvTime) lastGrvTime = t;`,
//     :378-380).
//
// Collapsing them onto the anchor's rule starves freshness: equal-version
// refreshes get discarded, a read-only workload stops getting cache hits once
// the first observation ages past maxVersionCacheLag no matter how many GRVs
// succeed, and the background refresher — which paces off the same instant —
// clamps to its 1ms floor and hammers the proxy forever.
//
// Version monotonicity is C++'s (`if (v >= cachedReadVersion)`, :367): a lower
// version is dropped whole, freshness included.
//
// All three fields ride ONE immutable entry behind ONE atomic pointer, so a
// reader can never observe a version paired with another version's anchor.
func (c *grvCache) publish(tok cacheToken, at time.Time, v int64) {
	c.publishReplyAt(tok, false, at, v, time.Time{}, false, false)
}

// publishReplyAt records a reply as ONE atomic publication: the version and
// its anchor, the renewed freshness, and any ratekeeper cooldown the reply
// carried. Doing it in one CAS is what makes it impossible for a reader to see
// a renewed entry before its cooldown — see grvCacheEntry.rkDefault.
//
// gen is the cache generation current when this reply's request was
// DISPATCHED, and it is a REQUIRED parameter on every publication entry point
// in this file — deliberately. There is no overload that loads it here,
// because "load the generation at publication time" is precisely the bug: by
// then an invalidation may have happened while the request was in flight, and
// a version obtained before it would install as though it were obtained after.
// Requiring the parameter means a future publisher cannot compile without
// deciding when it captured, which a convention in a comment cannot promise.
//
// replyTime stamps the cooldowns (C++ uses the reply instant for these, not
// the request instant: lastRkBatchThrottleTime/lastRkDefaultThrottleTime =
// replyTime, NativeAPI.actor.cpp:7411-7416). Cooldowns only ever advance, and
// they SURVIVE a version that is rejected as stale — a throttle signal is
// about the cluster, not about the version that happened to carry it.
//
// It returns whether the publication belonged to the CURRENT epoch. That is the
// same predicate mergeReply gates the floor and the cooldowns on, handed back
// rather than recomputed by the caller: a second comparison against a fence that
// may have moved in between is a second answer, and the two would drift.
func (c *grvCache) publishReplyAt(tok cacheToken, durable bool, at time.Time, v int64, replyTime time.Time, rkDefault, rkBatch bool) (sameEpoch bool) {
	for {
		cur := c.entry.Load()
		// Re-read per iteration, and as ONE word: an invalidation that lands
		// between the load and the CAS fails the CAS, and the retry must see the
		// new generation rather than re-deciding staleness against the sentinel
		// the invalidation installed. Both fence predicates come from this single
		// Load, so no publication can ever mix a pre-switch generation with a
		// post-switch epoch.
		obs := c.fences.Load()
		sameEpoch = fenceEpoch(obs) == tok.epoch()
		next := mergeReply(cur, obs, tok, durable, at, v, replyTime, rkDefault, rkBatch)
		if cur != nil && next == *cur {
			return // nothing moved
		}
		if cur == nil && next == (grvCacheEntry{}) {
			// An empty cache and a publication that contributed nothing — a
			// stale-epoch straggler is the case that gets here. Installing a zero
			// entry where there was none is not a publication: it changes no
			// observable (every accessor treats it as empty) but it advances the
			// counter, and that counter is the exact-accounting proof that
			// everything tryCache consults arrives in ONE publication. A count that
			// is only approximately right cannot carry that proof.
			return
		}
		if c.entry.CompareAndSwap(cur, &next) {
			c.publications.Add(1)
			return
		}
	}
}

// mergeReply computes the entry a reply produces from the current one. Pure,
// so the decision can be asserted directly instead of by racing the loop that
// calls it — the version-resurrection bug lived in the loop's RE-evaluation,
// and a test that cannot pin the decision can only catch it by luck.
//
// obs is the fence word observed at publication; tok is the one the publisher
// captured at dispatch. BOTH predicates are derived here from that single pair,
// so a caller cannot supply a combination the cache never held:
//
//   - mayInstallVersion (obs == tok) is false when either fence moved after this
//     reply's request was dispatched.
//   - sameEpoch compares only the high halves.
//
// mayInstallVersion therefore IMPLIES sameEpoch. That implication is the whole
// reason the fences share a word: as two counters, a publisher could observe
// mayInstall=true beside sameEpoch=false in the gap between a switch's two
// bumps, and install a previous-cluster version over a valid current-epoch entry
// — while the stamp below, correctly refusing to promote a stale-epoch
// publication, left the result unservable at epoch 0.
//
// durable says the version is one the caller has already observed as
// COMMITTED — ground truth about the cluster rather than a point-in-time
// read. Only a durable version may retire an older cached one when its
// install is blocked; a GRV reply carries no such promise, and retiring on
// one would evict a perfectly good cache entry every time a straggler with a
// higher version landed after an invalidation.
func mergeReply(cur *grvCacheEntry, obs uint64, tok cacheToken, durable bool, at time.Time, v int64, replyTime time.Time, rkDefault, rkBatch bool) grvCacheEntry {
	obsEpoch := fenceEpoch(obs)
	mayInstallVersion := obs == tok.f
	sameEpoch := obsEpoch == tok.epoch()
	// A previous-epoch entry is not a MERGE BASE. Its version, floor and
	// cooldowns belong to another cluster, so inheriting any of them — or
	// comparing this version against it for staleness — is the same category
	// error as letting it be served. Only when this publication itself belongs
	// to the current epoch: a stale-epoch reply must leave cur untouched.
	if sameEpoch && cur != nil && cur.epoch != obsEpoch {
		cur = nil
	}

	next := grvCacheEntry{version: v, anchor: at, freshness: at}

	// The floor is carried forward by every publication and raised by a
	// durable one. Raised even when the install below is REFUSED: a commit
	// that may not publish its version has still proved every lower version
	// stale, and that proof is what the floor records.
	floor := int64(0)
	if cur != nil {
		floor = cur.floor
	}
	// GATED ON THE GENERATION, like the install. The order-claim/time-claim
	// split — a durable version may raise the floor without the generation,
	// because it is a version comparison rather than a claim about when —
	// holds WITHIN one cluster and is meaningless across a switch: two
	// clusters' versions are unrelated numbers, so "9000000 > 500" says
	// nothing about order and everything about which cluster minted them.
	//
	// A stale-generation reply may therefore touch NOTHING: not the version,
	// not the floor, not the cooldowns (which describe another cluster's
	// ratekeeper). Leaving the durable arm ungated let a late commit from the
	// previous cluster raise this one's floor above every version it will ever
	// mint — the permanent wedge the epoch exists to prevent, reached through
	// the one arm the epoch did not cover.
	if durable && sameEpoch && v > floor {
		floor = v
	}
	if !mayInstallVersion {
		// The version is void: it predates an invalidation whose whole purpose
		// is that the next transaction not see it. Keep whatever is cached now
		// (nothing, if the sentinel is in place) and merge cooldowns below.
		// THE ASYMMETRY: a blocked reply may not INSTALL its version, but a
		// DURABLE one still raises the FLOOR above. Installing needs the
		// generation because the anchor belongs to a pre-invalidation dispatch
		// and would date the entry as something it is not. The floor needs no
		// generation, because it is a version comparison — order, not time.
		//
		// Refusing the install protects InvalidateGRVCache; the floor protects
		// read-your-committed-writes. Both are required, and only the first
		// needs the generation.
		next = grvCacheEntry{}
		if cur != nil {
			next = *cur
		}
	} else if v < floor {
		// Below the last durable commit: this version is stale by
		// construction, whoever published it and however current its
		// generation. Nothing to install; cooldowns still merge below.
		//
		// This is what an eviction could not do. Clearing the entry when the
		// commit landed left every publisher already in flight free to
		// repopulate with a low version the moment it arrived, and a
		// generation check cannot refuse those — they were dispatched after
		// the invalidation and are perfectly current. The floor refuses them
		// on arrival instead, which is why it subsumes the eviction entirely.
		next = grvCacheEntry{}
		if cur != nil {
			next = *cur
		}
	} else if cur != nil {
		next.rkDefault, next.rkBatch = cur.rkDefault, cur.rkBatch
		if v < cur.version {
			// Stale reply: the version is dropped whole (C++ :367), but a
			// throttle signal it carried still counts.
			next = *cur
		} else {
			if v == cur.version {
				// Same version: keep the earliest evidence of when its window
				// opened; freshness still advances below.
				if cur.anchor.Before(next.anchor) {
					next.anchor = cur.anchor
				}
			}
			if cur.freshness.After(next.freshness) {
				next.freshness = cur.freshness // monotonic (C++ :378-380)
			}
		}
	}
	// Cooldowns are cluster-scoped too: a ratekeeper stamp from the previous
	// cluster describes THAT cluster's ratekeeper and says nothing about this
	// one. Within an epoch they still merge from a generation-blocked reply —
	// an invalidation does not change which ratekeeper is throttling us.
	if sameEpoch && rkDefault && replyTime.After(next.rkDefault) {
		next.rkDefault = replyTime
	}
	if sameEpoch && rkBatch && replyTime.After(next.rkBatch) {
		next.rkBatch = replyTime
	}
	// Applied on every path, including the two that refused the install: the
	// floor is what those refusals have to LEAVE BEHIND, or the next publisher
	// re-opens the hole they just closed.
	next.floor = floor
	// Stamp from the OBSERVED fence when this publication contributed content,
	// and only then. On the stale-epoch path every field above was copied from
	// cur, so re-stamping would PROMOTE a previous-cluster entry into the
	// current epoch and make it servable — the stale reply changing nothing
	// except the one field that decides whether the old content counts.
	//
	// The epoch-0 sentinel bug this looks like it should fix lives in
	// invalidate(), not here: that path builds a fresh entry from a struct
	// literal and must carry cur's epoch across explicitly.
	if sameEpoch {
		next.epoch = obsEpoch
	}
	return next
}

// tryCache returns the cached version if it's fresh enough and not throttled.
// priority determines which ratekeeper throttle to check: BATCH checks lastRkBatch,
// DEFAULT checks lastRkDefault, and SYSTEM_IMMEDIATE is never throttled (it bypasses
// ratekeeper, so it is always "cooled down" and serves a fresh cache — #16).
// Matches C++ DatabaseContext::getConsistentReadVersion throttle checks. The
// opt-in gate (USE_GRV_CACHE) is enforced by the CALLER (getReadVersion);
// reaching here already implies the transaction opted in (RFC-104).
func (c *grvCache) tryCache(priority uint32) (int64, time.Time, bool) {
	// ONE load, through the shared accessor: version and instant come from the
	// same immutable entry, so the pair handed back is always one that actually
	// occurred, and the previous-cluster guard is the SAME guard every other
	// accessor applies rather than a copy of it that can drift. An entry from a
	// previous cluster is not merely stale here — its version is meaningless.
	e := c.currentEntry()
	if e == nil || e.version == 0 {
		return 0, time.Time{}, false
	}
	// Never below the last version this client committed. Checked HERE and not
	// only at publication because the floor must hold against an entry that
	// was already installed when the commit landed, not merely against later
	// publications — the two together are what make "no stale read after a
	// commit returns" an invariant rather than a race.
	if e.version < e.floor {
		return 0, time.Time{}, false
	}

	nowT := c.clock()
	// Staleness is judged on FRESHNESS — when the cluster last confirmed this
	// entry — not on the version anchor. An equal-version reply renews
	// freshness without moving the anchor, and on a quiet cluster that reply
	// is the steady state: judging staleness on the anchor would retire a
	// cache the cluster just confirmed.
	if nowT.Sub(e.freshness) > maxVersionCacheLag {
		return 0, time.Time{}, false // stale
	}

	// Check ratekeeper throttle cooldown for the requesting priority, matching C++
	// rkThrottlingCooledDown (NativeAPI.actor.cpp:7484-7499): BATCH → lastRkBatchThrottleTime,
	// DEFAULT → lastRkDefaultThrottleTime, and IMMEDIATE → always cooled down (:7485-7486 —
	// IMMEDIATE bypasses ratekeeper), so an opted-in IMMEDIATE read is served from the fresh
	// cache like any other priority (#16).
	// Read from the SAME entry as the freshness above: the cooldown a reply
	// carried is published with the freshness it renewed, so there is no state
	// in which this check sees the old cooldown beside the new freshness.
	var lastThrottle time.Time
	switch priority {
	case grvPriorityBatch:
		lastThrottle = e.rkBatch
	case grvPrioritySystemImmediate:
		// IMMEDIATE is never ratekeeper-throttled → always cooled down
	default:
		lastThrottle = e.rkDefault
	}
	if !lastThrottle.IsZero() && nowT.Sub(lastThrottle) < grvCacheRKCooldown {
		return 0, time.Time{}, false // throttled — must contact proxy
	}

	// The instant is when this CACHED version was obtained, not now. Serving a
	// version up to maxVersionCacheLag old while reporting "now" as its anchor
	// would hand a budget the full window when most of it is already spent.
	// Returned straight from the entry, so it keeps its monotonic reading.
	return e.version, e.anchor, true
}

// update advances the cached version + freshness clock from a successful
// commit, matching C++ updateCachedReadVersion at the commit site
// (NativeAPI.actor.cpp:6657, t=now()). Monotonic on both: version only accepts
// >= current (the CAS loop), lastTime only advances (updateTime's guard).
// Population is UNCONDITIONAL — it runs for every committing transaction
// regardless of USE_GRV_CACHE (RFC-104); only cache READS are opt-in,
// so a default transaction's commit can warm the cache for a later opted-in
// reader. The cached path does not check lock state, matching C++ (RFC-104).
func (c *grvCache) update(tok cacheToken, at time.Time, v int64) {
	// durable=true: a returned Commit is ground truth about the cluster, so
	// this version may retire an older cached one even when its own install is
	// blocked by the generation.
	c.publishReplyAt(tok, true, at, v, time.Time{}, false, false)
}

// updateFromGRV updates the cache from a real GRV reply: version + freshness,
// monotonic on both (CAS for version, updateTime for lastTime). Unconditional —
// runs for every GRV reply regardless of USE_GRV_CACHE (C++ extractReadVersion,
// :7409, is not gated on the option). Lock checks belong to fresh-GRV consumption,
// not cache reads, matching C++ (RFC-104).
func (c *grvCache) updateFromGRV(tok cacheToken, t time.Time, v int64) {
	c.publish(tok, t, v)
}

// clock returns the freshness clock: the injected seam when set, otherwise
// time.Now. Freshness decisions are the only thing it governs — the instants
// the cache STORES always come from the caller, so an injected clock can never
// fabricate an anchor.
func (c *grvCache) testHooks() *grvCacheTestHooks {
	if c.hooks == nil {
		c.hooks = &grvCacheTestHooks{}
	}
	return c.hooks
}

func (c *grvCache) clock() time.Time {
	if c.hooks != nil && c.hooks.now != nil {
		return c.hooks.now()
	}
	return time.Now()
}

// currentEntry is the cached entry when it belongs to the epoch the cache
// serves under, and nil otherwise. EVERY accessor below goes through it, so
// they answer uniformly: an entry from a previous cluster is inert in all of
// them, not merely in the ones someone remembered to guard. The uniformity is
// load-bearing rather than tidy — an unguarded freshnessInstant paces the
// background refresher off a previous cluster's confirmation instant and
// suppresses the refresh that would repopulate the cache under the new epoch.
func (c *grvCache) currentEntry() *grvCacheEntry {
	e := c.entry.Load()
	if e == nil || e.epoch != c.epochNow() {
		return nil
	}
	return e
}

// freshnessInstant is when the cluster last CONFIRMED the cached entry, or the
// zero time when the cache is empty. This is what the background refresher
// paces off and what tryCache judges staleness on — never the version anchor,
// which equal-version replies deliberately leave untouched. Pacing off the
// anchor collapses the refresh interval to its floor on a quiet cluster, where
// every reply carries the version already held.
func (c *grvCache) freshnessInstant() time.Time {
	if e := c.currentEntry(); e != nil {
		return e.freshness
	}
	return time.Time{}
}

// cachedVersion is the cached version, or 0 when the cache is empty.
func (c *grvCache) cachedVersion() int64 {
	if e := c.currentEntry(); e != nil {
		return e.version
	}
	return 0
}

// anchorInstant is the cached version's MVCC-window anchor, or the zero time
// when the cache is empty. This is the budget-facing instant — the one that
// must never move for a version already held.
func (c *grvCache) anchorInstant() time.Time {
	if e := c.currentEntry(); e != nil {
		return e.anchor
	}
	return time.Time{}
}

// markThrottled records a ratekeeper cooldown at the given instant without
// touching the cached version. Test-facing counterpart to the reply path,
// which publishes the cooldown together with the version it arrived with.
func (c *grvCache) markThrottled(tok cacheToken, at time.Time, rkDefault, rkBatch bool) {
	c.publishReplyAt(tok, false, time.Time{}, 0, at, rkDefault, rkBatch)
}

// throttleStamp is the recorded cooldown for a priority, or the zero time. A
// cooldown describes the epoch's ratekeeper, so a previous-epoch stamp reads as
// zero — the same rule mergeReply applies when it declines to merge one.
func (c *grvCache) throttleStamp(priority uint32) time.Time {
	e := c.currentEntry()
	if e == nil {
		return time.Time{}
	}
	if priority == grvPriorityBatch {
		return e.rkBatch
	}
	return e.rkDefault
}

// entryFloor is the committed-version floor, or 0 when the cache is empty.
func (c *grvCache) entryFloor() int64 {
	// The EFFECTIVE floor in the current epoch. A previous-epoch entry may
	// still be present — the switch deliberately does not clear it — but its
	// floor is inert: it is neither served nor used as a merge base.
	if e := c.currentEntry(); e != nil {
		return e.floor
	}
	return 0
}
