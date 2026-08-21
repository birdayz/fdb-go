package client

import (
	"context"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/fdbgo/internal/diag"
	"fdb.dev/pkg/fdbgo/transport"
	"fdb.dev/pkg/fdbgo/wire"
	"fdb.dev/pkg/fdbgo/wire/types"
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
type grvCache struct {
	// now is the clock freshness is judged against. A seam, not a knob: the
	// freshness window is 100ms, so a test that compares against the real
	// wall clock is one GC pause away from failing on a CORRECT
	// implementation. nil means time.Now.
	now func() time.Time

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
	// No `locked` rides the cache: the cache is now opt-in (USE_GRV_CACHE,
	// default off, RFC-104), so every DEFAULT transaction takes the fresh-GRV
	// path and hits the real locked check at the consumption site
	// (transaction.go ensureReadVersion). C++ fail-opens its cached path by
	// contract (NativeAPI.actor.cpp:7514-7516 returns the version with zero
	// lock inspection; lockAware appears only on the fresh fall-through, :7425),
	// so an opted-in Go transaction does too. The RFC-096 lastLocked ride-along
	// existed ONLY to compensate for the previous always-on cache and is gone.
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
// reader. No lock state: the cached path fail-opens (RFC-104; the RFC-096
// commit-must-not-extend-freshness divergence is reverted now that the cache is
// no longer always-on + enforcement-carrying).
func (c *grvCache) update(tok cacheToken, at time.Time, v int64) {
	// durable=true: a returned Commit is ground truth about the cluster, so
	// this version may retire an older cached one even when its own install is
	// blocked by the generation.
	c.publishReplyAt(tok, true, at, v, time.Time{}, false, false)
}

// updateFromGRV updates the cache from a real GRV reply: version + freshness,
// monotonic on both (CAS for version, updateTime for lastTime). Unconditional —
// runs for every GRV reply regardless of USE_GRV_CACHE (C++ extractReadVersion,
// :7409, is not gated on the option). No lock state: the cached path fail-opens
// (RFC-104; the RFC-096 lastLocked ride-along is removed — the per-transaction
// locked check now lives only on the fresh-GRV consumption path).
func (c *grvCache) updateFromGRV(tok cacheToken, t time.Time, v int64) {
	c.publish(tok, t, v)
}

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

// updateMinAcceptable lowers the floor toward the SMALLEST read version this
// client has seen IN THIS EPOCH. C++ DatabaseContext::minAcceptableReadVersion is
// a std::min over GRV reply versions (NativeAPI.actor.cpp:3871,:7287), inited to
// max(). Go treats version 0 as "unset": the first version sets the floor and
// later (necessarily ≥) versions leave it at the smallest. This is what
// validateVersion compares against — keeping it the smallest-seen (NOT a rising
// max) means only a genuinely-ancient pinned version is rejected, never a recent
// one that merely predates a later GRV. (Previously this ratcheted UPWARD, so
// RFC-104's fresh-GRV default raced it past pinned versions libfdb_c accepted —
// the root of the spurious 1007s in the snapshot/conflict/getKey differentials.)
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

// validateVersion checks a user-set read version against the client's
// seen-version floor. A version below minAcceptableReadVersion — the SMALLEST
// read version this client has seen (std::min; see updateMinAcceptable) — is
// transaction_too_old. Because the floor is the smallest-seen and NOT a rising
// max (the RFC-104 fix), this rejects only a genuinely-ancient pinned version
// (below anything the client has observed), never a recent one that merely
// predates a later GRV — so a libfdb_c-current version pinned across both
// clients is honored, and the storage server remains the authority on the 5s
// MVCC window. (C++ gates the same throw on `switchable` at NativeAPI.actor.cpp:518;
// the Go client is non-switchable and keeps it as a client-side fail-fast for
// ancient versions — outcome-equivalent, since the server rejects exactly those.)
//
// The absurd-future check is a Go-only client-side guard (real FDB versions are
// ~10^7/s; even 100 years is ~3×10^16): a wildly-out-of-range version would
// block the storage server indefinitely, and our RPC timeout races the server's
// own future_version, so detecting it client-side is more reliable.
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

// grvBatcherIndex maps GRV priority bits to a batcher array index.
// C++ uses separate batchers for BATCH, DEFAULT, and SYSTEM_IMMEDIATE.
const (
	grvBatcherBatch           = 0
	grvBatcherDefault         = 1
	grvBatcherSystemImmediate = 2
)

// grvBatcherIndex returns the array index for the given priority flags.
func grvBatcherIndex(flags uint32) int {
	switch flags & grvPriorityMask {
	case grvPriorityBatch:
		return grvBatcherBatch
	case grvPrioritySystemImmediate:
		return grvBatcherSystemImmediate
	default:
		return grvBatcherDefault
	}
}

// grvBatcher batches concurrent GetReadVersion calls for a single priority.
// C++: DatabaseContext::VersionBatcher + readVersionBatcher actor.
//
// Each priority level (BATCH, DEFAULT, SYSTEM_IMMEDIATE) has its own batcher,
// so requests at different priorities never mix — matching C++ behavior.
//
// Methods receive *database as argument — no stored back-pointer.
type grvBatcher struct {
	mu        sync.Mutex
	pending   []grvRequest
	batchTime time.Duration
	timer     *time.Timer
	priority  uint32 // fixed priority bits for this batcher

	refreshOnce sync.Once
	// refresherStarted is a deterministic test seam (RFC-104): true once the
	// background refresher has been launched (on the first opted-in request —
	// hit OR miss, matching C++ which starts the updater inside the opt-in gate
	// before the freshness check). Lets a test assert "the refresher never
	// started for a cache-off process" without a flaky goroutine count.
	refresherStarted atomic.Bool

	// lastPanicLog rate-limits recoverFlush's diagnostic log across overlapping
	// flushes (UnixNano; atomic — a new batch can flush while an earlier flush's
	// RPC is still in flight). RFC-110.
	lastPanicLog atomic.Int64
}

type grvRequest struct {
	reply chan grvResult
	flags uint32 // GRV Flags from the requesting transaction
	// spanContext is the requesting transaction's span. flush() folds the batch's
	// span contexts into the readVersionBatcher span (batchGRVSpanContext) to stamp
	// the GetReadVersionRequest — 1:1 with C++ addLink (NativeAPI.actor.cpp:7345).
	spanContext types.SpanContext
	// tags is the requesting transaction's tag set. flush() folds the batch's
	// tags into a per-tag occurrence count, matching C++ readVersionBatcher's
	// TransactionTagMap<uint32_t> accumulation (NativeAPI.actor.cpp:7347-7349):
	// the count is how many transactions in this batch carried the tag, which is
	// what the ratekeeper needs to attribute GRV load.
	tags []string
}

// aggregateBatchTags folds per-transaction tag sets into the per-tag occurrence
// counts the GRV request carries. Entries are sorted by tag: C++ accumulates
// into an unordered_map and the proxy re-reads it into one, so element order is
// not part of the wire contract — sorting only makes Go's output deterministic.
func aggregateBatchTags(batch []grvRequest) []types.TransactionTagCount {
	var counts map[string]uint32
	for _, r := range batch {
		for _, t := range r.tags {
			if counts == nil {
				counts = make(map[string]uint32)
			}
			counts[t]++
		}
	}
	if len(counts) == 0 {
		return nil
	}
	names := make([]string, 0, len(counts))
	for t := range counts {
		names = append(names, t)
	}
	sort.Strings(names)
	out := make([]types.TransactionTagCount, 0, len(names))
	for _, t := range names {
		out = append(out, types.TransactionTagCount{Tag: []byte(t), Count: counts[t]})
	}
	return out
}

type grvResult struct {
	version int64
	locked  bool // database-locked flag from the GRV reply (RFC-096)
	// instant is when this version's MVCC window opened, as nearly as the
	// client can know it: the PRE-RPC request time, never the reply time. It
	// is a lower bound — the proxy assigned the version at or after we asked —
	// and erring early is the safe direction, because a budget built on it
	// then expires sooner than FDB's window rather than later.
	instant time.Time
	err     error
}

// getReadVersion returns a read version, using the cache if fresh. The
// second return is the database-locked flag from the reply (or from the
// cache entry) — the per-transaction lock check happens at the consumption
// site (the C++ extractReadVersion analog), NOT here: one batched reply
// fans out to transactions with different lock-awareness.
func (b *grvBatcher) getReadVersion(db *database, ctx context.Context, flags uint32, span types.SpanContext, tags []string, useGrvCache, skipGrvCache bool) (int64, bool, time.Time, error) {
	// Fast path: serve from cache ONLY when the transaction opted in
	// (USE_GRV_CACHE, default off — RFC-104). C++ gate NativeAPI.actor.cpp:7503-7517:
	//   (debugChance || useGrvCache) && rkThrottlingCooledDown(cx, priority)
	// The gate does NOT special-case SYSTEM_IMMEDIATE: rkThrottlingCooledDown(IMMEDIATE)
	// returns true (:7485-7486 — IMMEDIATE bypasses ratekeeper), so an opted-in IMMEDIATE
	// transaction is served from the cache exactly like DEFAULT (#16 — IMMEDIATE priority is
	// a ratekeeper concern, orthogonal to cache freshness; the app opted into USE_GRV_CACHE).
	// The cached path FAIL-OPENS on locked (returns locked=false): C++ does not
	// lock-check the cached path (:7514-7516 returns the version with zero lock
	// inspection; lockAware appears only on the fresh fall-through, :7425). The
	// real locked check is at the consumption site (ensureReadVersion), which
	// every DEFAULT (cache-off) transaction reaches.
	//
	// NOTE (separate documented divergence): the C++ gate skips the WHOLE cache block under
	// active ratekeeper throttling (rkThrottlingCooledDown false for DEFAULT/BATCH); Go's gate
	// omits that at-gate condition, so under throttle Go starts the refresher where C++ would
	// not. tryCache still rechecks the DEFAULT/BATCH throttle on the serve path, so this only
	// affects WHEN the background updater launches, never correctness. IMMEDIATE is unaffected —
	// it is never throttled.
	if useGrvCache && !skipGrvCache {
		// Start the background refresher on the FIRST opted-in request, BEFORE the
		// freshness check — matching C++ getReadVersion (NativeAPI.actor.cpp:7507-7509),
		// which launches backgroundGrvUpdater inside the opt-in gate regardless of
		// whether the cached version is usable ("Upon our first request to use cached
		// RVs, start the background updater"). Starting it only on a cache HIT (the
		// prior Go behavior) left a cold/stale cache un-warmed: every opted-in read with
		// lag > MAX_VERSION_CACHE_LAG fell through to a real GRV and the cache never
		// caught up, defeating the opt-in entirely for sparse workloads.
		//
		// EXCEPT for SYSTEM_IMMEDIATE: Go's refresher is PER-BATCHER and issues its periodic
		// GRVs at b.priority, so starting the IMMEDIATE batcher's refresher would emit a long-lived stream
		// of ratekeeper-bypassing PRIORITY_SYSTEM_IMMEDIATE GRVs from a single opted-in read — a real
		// throttling bug. C++ instead runs ONE cx-level updater at DEFAULT priority (backgroundGrvUpdater,
		// NativeAPI.actor.cpp:1283 — its refreshTransaction uses a default-priority getReadVersion), so
		// IMMEDIATE never drives ratekeeper-bypassing background traffic. IMMEDIATE still SERVES the shared
		// cache below when it is warm (the #16 fix); the cache is kept warm by DEFAULT/BATCH refreshers and
		// every commit/GRV reply, and a pure-IMMEDIATE read-only workload simply falls through to a real
		// GRV once it ages out — never a background IMMEDIATE flood.
		if b.priority != grvPrioritySystemImmediate {
			b.refreshOnce.Do(func() {
				// Reserve the refresher's wg slot under the close barrier. If Close() has begun,
				// registerBackgroundGoroutine returns false and we skip: starting a background goroutine +
				// db.wg.Add(1) now would race Close's db.wg.Wait() at a zero counter (the topology monitor has
				// Done'd) → "WaitGroup misuse: Add concurrently with Wait" panic. refreshOnce still marks this
				// done (no retry) and tryCache below misses → the caller falls through to a real GRV, the
				// right behavior during shutdown.
				if !db.registerBackgroundGoroutine() {
					return
				}
				b.refresherStarted.Store(true)
				go b.backgroundRefresher(db) // does its own db.wg.Done() on exit
			})
		}
		if v, at, ok := db.grvCache.tryCache(b.priority); ok {
			db.metrics.countGRVCacheHit()
			return v, false, at, nil
		}
	}

	// Slow path: batch request to proxy.
	req := grvRequest{reply: make(chan grvResult, 1), flags: flags, spanContext: span, tags: tags}

	b.mu.Lock()
	b.pending = append(b.pending, req)
	if len(b.pending) == 1 {
		b.timer = time.AfterFunc(b.batchTime, func() { b.flush(db) })
	}
	b.mu.Unlock()

	select {
	case result := <-req.reply:
		return result.version, result.locked, result.instant, result.err
	case <-ctx.Done():
		return 0, false, time.Time{}, ctx.Err()
	}
}

// flush sends the batched GRV request and updates the cache.
//
// Lock is held only to pop the pending slice; the RPC executes without
// holding mu, so new requests can queue (and start a new timer) while
// the RPC is in flight.
func (b *grvBatcher) flush(db *database) {
	// Pop the pending batch under a closure-scoped lock (RFC-110: a panic in any
	// b.mu-holding region must unwind the lock, else later GRV requests blocking
	// on b.mu.Lock() deadlock).
	batch := func() []grvRequest {
		b.mu.Lock()
		defer b.mu.Unlock()
		p := b.pending
		b.pending = nil
		return p
	}()

	if len(batch) == 0 {
		return
	}

	// RFC-110: once the batch is popped, b.pending no longer references
	// it — a panic anywhere below (sendGRVRequest decode, applyGRVReply, the
	// adaptive-window math) would orphan it, and every waiter blocked on its
	// req.reply with a non-canceling ctx would hang forever. recoverFlush is the
	// fail-the-batch backstop: it delivers an error to every popped waiter and
	// keeps the process alive (the libfdb_c analog: a failed proxy GRV future
	// resolves with error, not a hung future). This is a one-shot per-batch
	// callback (not a standing loop) — there is nothing to re-arm, and the next
	// queued request arms a fresh timer; the log is rate-limited, the metric
	// counts every occurrence.
	defer b.recoverFlush(db, batch)

	// Bound the GRV request. C++ cancels the actor when callers drop
	// the future. Our equivalent: context with timeout. If all callers
	// have given up (their ctx expired), this ensures the batcher
	// goroutine doesn't hang forever.
	batchCtx, batchCancel := context.WithTimeout(db.ctx, CoordinatorTimeout)
	defer batchCancel()

	// Each batcher has a fixed priority. OR all option flags (bits 0-23)
	// from requests in this batch. Collect the per-tx span contexts so the
	// GetReadVersionRequest carries the readVersionBatcher span (C++
	// NativeAPI.actor.cpp:7345 addLink + :7385 getConsistentReadVersion child).
	var optionBits uint32
	spans := make([]types.SpanContext, len(batch))
	for i, r := range batch {
		optionBits |= r.flags &^ grvPriorityMask
		spans[i] = r.spanContext
	}
	flags := b.priority | optionBits

	// The generation is captured with the request time, BEFORE dispatch: a
	// reply may install its version only if no invalidation happened while it
	// was in flight. Capturing it at reply-processing time instead would leave
	// the whole round trip as a window in which InvalidateGRVCache is defeated
	// by a reply it was meant to retire.
	tok := db.grvCache.token()
	requestTime := time.Now()
	version, locked, rkDefault, rkBatch, tagThrottleInfoEntries, _, attemptEpoch, err := b.sendGRVRequest(db, batchCtx, flags, uint32(len(batch)), batchGRVSpanContext(spans), aggregateBatchTags(batch))
	elapsed := time.Since(requestTime)

	if err == nil {
		// Unconditional, even when locked: C++ updates the shared cache
		// BEFORE the per-transaction locked throw (NativeAPI.actor.cpp:7409
		// precedes :7425). `locked` is returned to waiters below but no longer
		// rides the cache (RFC-104).
		b.applyGRVReply(db, tok.withEpoch(attemptEpoch), requestTime, version, rkDefault, rkBatch, tagThrottleInfoEntries)
		// C++ counts per-transaction in extractReadVersion (:7428-7440) — one
		// batched reply serves len(batch) transactions. Cache hits never reach
		// here (C++ parity: its cached path returns before the counters); the
		// background refresher has no waiters and adds nothing. Two accepted
		// edge divergences: waiters whose ctx expired mid-batch are still
		// counted (C++ cancels abandoned futures before its counter), and
		// under a LOCKED database non-lock-aware waiters are counted here
		// while C++'s database_locked throw (:7425-7426) precedes its
		// counters — fixing either would need per-waiter counting at the
		// consumption site, machinery a counter doesn't justify. RFC-097.
		db.metrics.countGRVBatchCompleted(b.priority, len(batch))
		// RFC-114: GRV round-trip latency (C++ GRVLatencies, NativeAPI.actor.cpp:7417).
		// Divergence (documented in RFC-114), on TWO axes: (1) count — Go samples
		// once per GRV BATCH, so Count is proxy round-trips, whereas C++ samples
		// per-transaction in extractReadVersion (Count == read-versions-completed);
		// (2) semantics — Go measures only the proxy RPC round-trip, whereas C++'s
		// latency = replyTime − startTime also folds in the per-transaction
		// batch-window queueing. Go's is the cleaner RPC-latency SLI. Cache hits
		// never reach here (they return before the flush — C++ parity, as above).
		db.metrics.observeGRVLatency(elapsed)
	}

	// Adaptive batch window (closure-scoped lock: a panic here unwinds b.mu).
	func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.batchTime = time.Duration(0.1*float64(elapsed)/2 + 0.9*float64(b.batchTime))
		if b.batchTime < 100*time.Microsecond {
			b.batchTime = 100 * time.Microsecond
		}
		if b.batchTime > 5*time.Millisecond { // C++ GRV_BATCH_TIMEOUT = 5ms
			b.batchTime = 5 * time.Millisecond
		}
	}()

	result := grvResult{version: version, locked: locked, instant: requestTime, err: err}
	for _, req := range batch {
		req.reply <- result
	}
}

// recoverFlush is flush's deferred RFC-110 backstop. On a recovered panic it
// FAILS THE BATCH — delivers an error to every popped waiter so none hangs —
// counts it, and rate-limited-logs. The batch reply channels are
// cap-1 and unsent on the panic path (the panic precedes the normal delivery
// loop), so the sends never block even for a waiter that already took its
// ctx.Done() branch. On the normal path recover() is nil → no-op. flushes are
// independent one-shots (not a standing loop), so this is fail-the-batch at
// request rate, not backoff-bounded.
func (b *grvBatcher) recoverFlush(db *database, batch []grvRequest) {
	r := recover()
	if r == nil {
		return
	}
	if db != nil {
		db.metrics.countRecoveredPanic(1)
	}
	b.logFlushPanic(r)
	result := grvResult{err: fmt.Errorf("fdbgo: panic in GRV flush: %v", r)}
	for _, req := range batch {
		req.reply <- result
	}
}

// logFlushPanic rate-limits recoverFlush's ERROR log to one per panicLogInterval
// (atomic CAS so overlapping flushes don't all log). The metric counts every
// panic; the log is the human breadcrumb, not the storm signal.
func (b *grvBatcher) logFlushPanic(r any) {
	now := time.Now().UnixNano()
	last := b.lastPanicLog.Load()
	if now-last < int64(panicLogInterval) || !b.lastPanicLog.CompareAndSwap(last, now) {
		return
	}
	diag.Recovered("fdbgo: recovered panic in client goroutine",
		"goroutine", "grvFlush",
		"err", fmt.Sprintf("%v", r),
		"stack", string(debug.Stack()),
	)
}

// grvRefreshMin is the floor for backgroundRefresher's adaptive delay.
// Matches C++ backgroundGrvUpdater's std::max(0.001, ...) clamp.
const grvRefreshMin = 1 * time.Millisecond

// nextGRVRefreshDelay computes how long backgroundRefresher should sleep
// before the next GRV proxy contact. Pure-function port of C++
// NativeAPI.actor.cpp:backgroundGrvUpdater's wait expression.
//
// proxyBudget = MAX_PROXY_CONTACT_LAG - (now - lastProxy)
// cacheBudget = (MAX_VERSION_CACHE_LAG - grvDelay) - (now - lastTime)
// next = max(grvRefreshMin, min(proxyBudget, cacheBudget))
//
// `grvDelay` is the EMA of recent GRV RPC latency — subtracted from the
// cache budget so the refresh completes before the cache becomes stale.
func nextGRVRefreshDelay(now, lastProxy, lastTime time.Time, grvDelay time.Duration) time.Duration {
	proxyBudget := maxProxyContactLag - now.Sub(lastProxy)
	cacheBudget := (maxVersionCacheLag - grvDelay) - now.Sub(lastTime)
	wait := proxyBudget
	if cacheBudget < wait {
		wait = cacheBudget
	}
	if wait < grvRefreshMin {
		wait = grvRefreshMin
	}
	return wait
}

// backgroundRefresher proactively keeps the cache fresh.
//
// Matches C++ NativeAPI.actor.cpp:backgroundGrvUpdater. Each iteration:
//
//	wait( max(0.001,
//	          min(MAX_PROXY_CONTACT_LAG - (now - lastProxyTime),
//	              (MAX_VERSION_CACHE_LAG - grvDelay) - (now - lastTime))) )
//	... issue GRV ...
//	grvDelay = (grvDelay + (now - curTime)) / 2  // EMA of RPC latency
//
// `grvDelay` is the EMA of GRV RPC latency, used as lead time so the
// refresh completes BEFORE the cache would expire. `lastProxyTime` is
// the last successful proxy contact (kept warm). `lastTime` is the
// timestamp of the cached read version.
//
// Pre-2026-04-25 this was a fixed `maxVersionCacheLag/2` ticker — 2× the
// RPC rate of C++ under low latency (when the cache budget is large).
// Now matches C++: refresh just-in-time, scaled by observed latency.
func (b *grvBatcher) backgroundRefresher(db *database) {
	defer db.wg.Done()

	// RFC-110: a panic in the refresh (sendGRVRequest decode, applyGRVReply)
	// must not abort the host — C++'s backgroundGrvUpdater catches every error,
	// backs off, and loops, leaving the stale cached version usable. The backstop
	// recovers + counts + rate-limited-logs; on a recovered panic the next wait
	// is the backoff (≤1s) instead of the just-in-time refresh delay, so a
	// deterministic bug re-fires at ≤1/s (the C++ Backoff analog) rather than
	// hot-spinning at grvRefreshMin (1ms).
	pb := &panicBackstop{name: "backgroundRefresher", db: db}

	// EMA of GRV RPC latency. C++ initializes to 0.001s (1ms).
	grvDelay := grvRefreshMin

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		wait := nextGRVRefreshDelay(
			time.Now(),
			time.Unix(0, db.grvCache.lastProxyContact.Load()),
			db.grvCache.freshnessInstant(),
			grvDelay,
		)
		if bo := pb.backoff(); bo > wait {
			wait = bo // a recovered panic last iteration: back off, don't hot-spin
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)

		select {
		case <-timer.C:
			pb.run(func() {
				// Captured before dispatch, like the foreground path: a
				// background refresh must not resurrect a version an
				// invalidation retired while its RPC was in flight.
				tok := db.grvCache.token()
				requestTime := time.Now()
				refreshCtx, refreshCancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
				defer refreshCancel() // RFC-110: release the GRV timer even if sendGRVRequest panics
				// Refresh at this batcher's own priority, not always DEFAULT.
				// The BATCH batcher must refresh BATCH ratekeeper state
				// (lastRkBatch); the DEFAULT batcher refreshes DEFAULT
				// (lastRkDefault). SYSTEM_IMMEDIATE never reaches here because
				// its tryCache always returns false (refreshOnce never fires).
				// No tx waiters on a background refresh, so the batcher span has no
				// links: batchGRVSpanContext(nil) = {traceID 0, random spanID,
				// unsampled} — the no-sampled-link case, matching a C++ updater GRV.
				version, _, rkDefault, rkBatch, tagThrottleInfoEntries, _, attemptEpoch, err := b.sendGRVRequest(db, refreshCtx, b.priority, 1, batchGRVSpanContext(nil), nil)
				if err == nil {
					// The refresher ignores the reply's `locked` flag — equivalent
					// to C++'s background updater, whose non-lock-aware txn THROWS
					// 1038 on a locked DB after the cache update (:7409 precedes
					// :7425) and is caught by its own onError loop. Nothing surfaces
					// to users from a background refresh, and the cached path
					// fail-opens anyway (RFC-104).
					b.applyGRVReply(db, tok.withEpoch(attemptEpoch), requestTime, version, rkDefault, rkBatch, tagThrottleInfoEntries)
					// EMA update: grvDelay = (grvDelay + measured_latency) / 2.
					grvDelay = (grvDelay + time.Since(requestTime)) / 2
				}
			})
		case <-db.ctx.Done():
			return
		}
	}
}

// applyGRVReply updates all database state from a successful GRV response:
// version cache, proxy contact time, minAcceptableReadVersion, ratekeeper
// throttle state, and tag throttle info.
// Called from both flush() (batched request) and backgroundRefresher().
func (b *grvBatcher) applyGRVReply(db *database, tok cacheToken, requestTime time.Time, version int64, rkDefault, rkBatch bool, tagThrottleInfoEntries []types.TransactionTagThrottle) {
	// ONE publication for everything tryCache consults. Serving is decided by
	// freshness AND cooldown together, so publishing them separately — in
	// either order — leaves a state a concurrent reader can catch. Renewing
	// freshness first is the harmful one: an opted-in transaction is served
	// from an entry this reply just refreshed while the throttling the same
	// reply carried has not landed, and it bypasses the ratekeeper.
	//
	// C++ writes them as separate statements in the opposite order
	// (updateCachedReadVersion at NativeAPI.actor.cpp:7409, then the
	// lastRk*ThrottleTime stores at :7411-7416) and is safe regardless: no
	// wait() separates them, so the actor cannot yield and no other actor sees
	// the intermediate state. Go has real threads and no such guarantee, so
	// matching C++ here means reproducing which states an observer can SEE.
	// Publishing atomically is stronger than ordering the stores, and unlike an
	// ordering it cannot be undone by an innocent-looking edit to this
	// function.
	replyTime := time.Now()
	sameEpoch := db.grvCache.publishReplyAt(tok, false, requestTime, version, replyTime, rkDefault, rkBatch)
	// The REQUEST instant, not the reply instant: C++ sets
	// lastProxyRequestTime = trState->startTime (NativeAPI.actor.cpp:7408) while
	// the reply instant is reserved for the ratekeeper cooldowns (:7411-7416).
	// The refresher subtracts this from MAX_PROXY_CONTACT_LAG, so dating it at the
	// reply understates how long the proxy has gone uncontacted by a whole round
	// trip and lets the contact budget run over.
	//
	// Gated on the epoch, like the cooldowns: "when we last reached the proxies"
	// is a fact about a CLUSTER, and a reply from the previous one says nothing
	// about this one — it would suppress the very refresh that repopulates the
	// cache under the new epoch. The predicate comes back from the publication
	// rather than being recomputed here, so there is exactly one answer.
	//
	// It stays a separate atomic rather than joining the entry: nothing tryCache
	// consults reads it, so it is not part of the serve decision that has to be
	// observed as one value. It only paces the background refresher.
	//
	// The minimum-acceptable floor is gated on the SAME predicate, and for the
	// same reason with sharper consequences: it is a claim about a VERSION SPACE,
	// and a straggler from the previous cluster writes that cluster's numbers into
	// this one's floor. validateVersion then rejects every user-set version below
	// the previous cluster's space with transaction_too_old. The pollution also
	// closes the path that would fix it FASTEST: the bootstrap GRV in
	// ensureReadVersion fires only while the floor reads 0, and the handoff resets
	// it to 0 precisely to arm that. A handle that only ever uses SetReadVersion
	// takes no other GRV, so for it the straggler's write is not a delay — it is
	// permanent.
	//
	// So the floor does NOT rely on this sameEpoch verdict, which is decided in
	// publishReplyAt a few instructions earlier and can be overtaken by a handoff
	// in between. updateMinAcceptable re-reads the epoch AT ITS OWN CAS and stamps
	// what it writes, so a straggler is refused at the write rather than racing
	// it, and a value written just before a handoff goes inert on its own. The
	// sameEpoch gate here is the outer of two checks, not the only one.
	//
	// Gated on sameEpoch and NOT on mayInstall: an invalidation does not change
	// whose version space a reply came from, so a generation-blocked reply from
	// THIS cluster still describes this cluster's numbers. Same asymmetry the
	// floor and the cooldowns are built on — and this is one more arm the epoch
	// gate had not covered.
	//
	// C++ does not gate this at all: its minAcceptableReadVersion is an
	// unconditional std::min at NativeAPI.actor.cpp:7287 and :3871, and
	// switchConnectionRecord is equally exposed to a reply in flight across its
	// own switch. Go is stricter than the spec here, deliberately, because Go's
	// bootstrap-GRV repair is what a polluted floor suppresses.
	//
	// THE REMAINING RESIDUAL, and it is only the contact instant: it is a bare
	// atomic with no stamp, so a handoff between the verdict above and this store
	// can still leave a previous cluster's instant behind. That costs at most one
	// delayed refresh — the refresher re-derives it on its next contact and the
	// lag is bounded by MAX_PROXY_CONTACT_LAG — and it can wedge nothing, because
	// nothing gates a repair on it. Stamping it too, or moving it inside the
	// publication's CAS, is real work for a bounded, self-healing skew; the floor
	// got the treatment because the floor could wedge a handle and this cannot.
	if sameEpoch {
		db.grvCache.lastProxyContact.Store(requestTime.UnixNano())
	}
	// Outside the gate above ON PURPOSE — it carries its own, applied at the CAS.
	db.updateMinAcceptable(tok.epoch(), version)

	if len(tagThrottleInfoEntries) > 0 {
		parsed := parseTagThrottleInfo(tagThrottleInfoEntries)
		if parsed != nil {
			priority := grvPriorityToPriority(b.priority)
			db.tagThrottles.replace(priority, parsed)
		}
	}
}

// Load balance knobs — match C++ FLOW_KNOBS.
const (
	loadBalanceStartBackoff = 10 * time.Millisecond // LOAD_BALANCE_START_BACKOFF
	loadBalanceMaxBackoff   = 5 * time.Second       // LOAD_BALANCE_MAX_BACKOFF
	loadBalanceBackoffRate  = 2.0                   // LOAD_BALANCE_BACKOFF_RATE
)

// inBandMaybeDeliveredGRV counts GRV replies that carried an IN-BAND
// maybeDelivered error and therefore took the next-alternative arm below.
//
// It exists because that arm is otherwise unobservable from a test. The arm
// consumes the reply and continues; the retry then succeeds, so the read
// version, the connection pool and the failure monitor all end in exactly the
// state they would have reached had the arm never run. A test asserting on end
// state therefore passes with the arm removed -- measured, on three separate
// attempts at writing one. This counter is what lets a test prove the arm RAN
// rather than that the run happened to end tidily.
var inBandMaybeDeliveredGRV atomic.Int64

// sendGRVRequest cycles all GRV proxies, matching C++ basicLoadBalance
// with AtMostOnce::False. On broken_promise (transport error), tries next
// proxy. On FDB application error, propagates immediately. If all proxies
// fail, applies exponential backoff and retries — loops until success or
// db.ctx cancellation (matching C++ infinite loop + quorum(ok,1) wait).
func (b *grvBatcher) sendGRVRequest(db *database, ctx context.Context, flags uint32, txnCount uint32, span types.SpanContext, tags []types.TransactionTagCount) (version int64, locked bool, rkDefaultThrottled, rkBatchThrottled bool, tagThrottleInfo []types.TransactionTagThrottle, proxyTagThrottledDuration float64, attemptEpoch int64, err error) {
	var backoff time.Duration

	for {
		// Re-read the proxy list each cycle — topology may have refreshed — and
		// take the epoch FROM THAT SAME LOAD. A retry can cross a cluster
		// handoff, and a token minted once at dispatch would then describe a
		// cluster this attempt never talked to. There is no epoch-before-proxies
		// ordering to reason about any more: the epoch rides the snapshot, so it
		// is by construction the epoch of the proxies this attempt uses.
		var proxies []ProxyInfo
		proxies, attemptEpoch = db.getGRVProxies()
		if len(proxies) == 0 {
			db.kickTopology()
			if backoff == 0 {
				backoff = loadBalanceStartBackoff
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
				continue
			case <-db.failMon.waitForRecovery():
				timer.Stop()
				backoff = 0
				continue
			case <-db.waitProxiesChanged():
				timer.Stop()
				backoff = 0
				continue
			case <-ctx.Done():
				timer.Stop()
				return 0, false, false, false, nil, 0, attemptEpoch, ctx.Err()
			}
		}

		// Start from a round-robin offset to distribute load across proxies.
		startIdx := db.proxyRR.nextGRV(len(proxies))
		for i := 0; i < len(proxies); i++ {
			proxy := proxies[(startIdx+i)%len(proxies)]
			conn, err := db.getOrDial(ctx, proxy.Address)
			if err != nil {
				db.handleDialError(ctx, proxy.Address)
				continue
			}

			replyToken, replyCh, replyHandle := conn.PrepareReply()
			body := buildGetReadVersionRequest(replyToken, flags, txnCount, span, tags)

			if err := conn.SendFrame(proxy.Token, body); err != nil {
				replyHandle.Cancel()
				replyHandle.Release()
				db.handleConnError(proxy.Address)
				continue
			}

			resp, rpcErr := waitReply(replyCh, ctx, DefaultRPCTimeout)
			if rpcErr != nil {
				replyHandle.Cancel()
				replyHandle.Release()
				if ctx.Err() != nil {
					return 0, false, false, false, nil, 0, attemptEpoch, ctx.Err()
				}
				// RFC-114: a GRV proxy that stops replying (TCP may stay open) is a
				// real endpoint failure — route through the observability sink so it
				// counts + Warns like any other conn failure, not a silent markFailed.
				db.recordConnFailure(proxy.Address)
				continue
			}
			replyHandle.Release()
			if resp.Err != nil {
				db.handleConnError(proxy.Address)
				continue
			}
			// ALIVE on frame receipt, before classifying the reply. The failure
			// monitor is keyed by ADDRESS, and a well-formed frame proves the
			// address is reachable whatever the reply says.
			//
			// This briefly sat below the disposition check, on the reasoning that a
			// broken_promise is the proxy dying. That was wrong in a way worth
			// recording: an address already marked failed by the GRV-timeout arm
			// above would then never be cleared by an in-band 1100 -- the continue
			// skips past here -- so a healthy co-located role stays excluded and
			// recovery waiters are never signalled. It also bought nothing: the
			// alive->failed churn that produced a backoff spin came from the arm
			// calling handleConnError, which is gone; markAlive is transition-gated
			// and cannot close the shared recovered channel unless something first
			// marked the address failed.
			db.failMon.markAlive(proxy.Address)
			v, lk, rkD, rkB, tti, ptd, perr := parseGetReadVersionReply(resp.Body)
			// An IN-BAND maybeDelivered error is not this proxy's answer, it is
			// the proxy going away while answering: a dying GrvProxy replies
			// broken_promise (1100) inside the ErrorOr rather than dropping the
			// connection, so it lands here and never in the resp.Err arm above.
			//
			// C++ basicLoadBalance treats 1100 and 1030 as one class and, at
			// AtMostOnce::False, moves to the next alternative
			// (LoadBalance.actor.h:823-830). GRV is AtMostOnce::False
			// (NativeAPI.actor.cpp:7258, in getConsistentReadVersion at :7231). Returning perr raw was the divergence:
			// 1100 is retryable under NONE of the three Go predicates, so a GRV
			// issued while a proxy was dying failed TERMINALLY where C++ just
			// asks the next proxy. Exhausting them all falls through to the
			// backoff below, which is C++'s behaviour too.
			if dispositionForReplyError(perr, false) == replyTryNextAlternative {
				inBandMaybeDeliveredGRV.Add(1)
				// NOTHING is touched here -- not the failure monitor, not the
				// connection pool. basicLoadBalance does not either: its
				// broken_promise arm just moves to the next alternative.
				//
				// An earlier version called handleConnError, which was actively
				// harmful. The connection is demonstrably HEALTHY -- a well-formed
				// reply just arrived on it -- and connPool is keyed by ADDRESS, so
				// closing it for a dying GrvProxy co-located with a CommitProxy
				// pushes an unrelated in-flight COMMIT into resp.Err and
				// manufactures a commit_unknown_result. It also re-armed the
				// one-Warn-per-episode latch every cycle and, via markAlive closing
				// the shared recovered channel, let all three grvBatchers zero each
				// other's exhaustion backoff -- a fast spin. And it bought nothing:
				// this loop never consults the excluded set, so the marking steered
				// no traffic.
				continue
			}
			return v, lk, rkD, rkB, tti, ptd, attemptEpoch, perr
		}

		// All proxies exhausted — backoff with recovery/topology wakeup.
		db.kickTopology()
		if backoff == 0 {
			backoff = loadBalanceStartBackoff
		} else {
			backoff = time.Duration(math.Min(float64(backoff)*loadBalanceBackoffRate, float64(loadBalanceMaxBackoff)))
		}

		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-db.failMon.waitForRecovery():
			timer.Stop()
			backoff = 0
		case <-db.waitProxiesChanged():
			timer.Stop()
			backoff = 0
		case <-ctx.Done():
			timer.Stop()
			return 0, false, false, false, nil, 0, attemptEpoch, ctx.Err()
		}
	}
}

// grvPriorityToPriority converts GRV wire priority bits to TransactionPriority.
func grvPriorityToPriority(flags uint32) TransactionPriority {
	switch flags & grvPriorityMask {
	case grvPriorityBatch:
		return PriorityBatch
	case grvPrioritySystemImmediate:
		return PrioritySystemImmediate
	default:
		return PriorityDefault
	}
}

func buildGetReadVersionRequest(replyToken transport.UID, flags uint32, txnCount uint32, span types.SpanContext, tags []types.TransactionTagCount) []byte {
	req := types.GetReadVersionRequest{
		TransactionCount: txnCount,
		Flags:            flags,
		MaxVersion:       InvalidVersion,
		SpanContext:      span, // C++ GetReadVersionRequest req(span.context, …) (:7245)
		Reply:            types.ReplyPromise{Token: wire.UIDFromParts(replyToken.First, replyToken.Second)},
		// Tags carries the batch's per-tag occurrence counts. There is no
		// tag-specific GRV flag — flags encode priority only, and the throttle
		// decision is made per priority against cx->throttledTags client-side
		// (NativeAPI.actor.cpp:7538-7568).
		Tags: tags,
	}
	return req.MarshalFDB()
}

// parseGetReadVersionReply parses the ErrorOr-wrapped GRV response.
// Returns (version, locked, rkDefaultThrottled, rkBatchThrottled,
// tagThrottleInfo, proxyTagThrottledDuration, error). `locked` is the
// database-locked flag the proxy reports unconditionally
// (GrvProxyServer.actor.cpp:673); enforcement is client-side, per
// transaction (RFC-096).
func parseGetReadVersionReply(data []byte) (int64, bool, bool, bool, []types.TransactionTagThrottle, float64, error) {
	var r wire.Reader
	if err := wire.ReadErrorOrInto(data, &r); err != nil {
		return 0, false, false, false, nil, 0, fmt.Errorf("GRV: %w", err)
	}
	var reply types.GetReadVersionReply
	reply.UnmarshalFromReader(&r)
	return reply.Version, reply.Locked, reply.RkDefaultThrottled, reply.RkBatchThrottled, reply.TagThrottleInfo, reply.ProxyTagThrottledDuration, nil
}

// clock returns the freshness clock: the injected seam when set, otherwise
// time.Now. Freshness decisions are the only thing it governs — the instants
// the cache STORES always come from the caller, so an injected clock can never
// fabricate an anchor.
func (c *grvCache) clock() time.Time {
	if c.now != nil {
		return c.now()
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
