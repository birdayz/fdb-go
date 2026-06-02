# RFC-057: Lazy merged-segment iterator for getKey-RYW resolution

**Status:** Reviewed (FDB-C-dev ACK w/ requirements incorporated; Torvalds NAK→re-review). Step 0 benchmark DONE → proceed.
**Item:** RFC-056 continuation (1). Follows the merged getKey-RYW core (#235, RFC-056).

## Problem

`getKeyRYW` resolves a key selector by walking a merged view of the keyspace (pending
write-map ∪ snapshot cache). The current backing, `buildSegmentsLocked`
(`pkg/fdbgo/client/ryw_getkey.go`), **materializes the ENTIRE merged-segment partition**
of `[allKeysBegin, maxKey)` — it collects every boundary (each write key + successor,
each cleared-range bound, each snapshot-cache entry bound, each cache key + successor),
sorts + dedupes them, and builds a `[]rywSegment` — **on every resolution attempt**.

Cost is **O(writes + cacheKeys)** per attempt, regardless of how far the selector walk
actually travels. libfdb_c's `RYWIterator` is **lazy**: it computes each segment on
demand by zipping the WriteMap and SnapshotCache sub-iterators, so a `getKey` costs only
its walk distance (typically O(|offset|) segments + the empty-skip), independent of the
total cache size.

Concrete consequence: a transaction that has read a large range (big snapshot cache)
then does `getKey` in a loop pays O(cacheKeys) **per call** — quadratic-ish in a hot
loop.

**This is a HYPOTHESIS gated on measurement, not an assumed win.** Per review (Torvalds):
*benchmark first*. We do NOT refactor on speculation — Step 1 of the plan is a benchmark
that proves the materializer is a real bottleneck for a realistic getKey workload; if it
isn't material, this RFC is closed as "measured, materializer adequate" and we don't add
the lazy-iterator complexity.

**Explicitly out of scope as justification: RFC-056 item (2)** (the go-vs-cgo
`transaction_too_old(1007)` asymmetry). Making go faster so it trips the 5s timer less
often is *moving the cliff*, not fixing it — item (2) is correctly handled by the
differential's retry (1007 is a legitimately retryable error) and is a perf-not-
correctness characteristic tracked separately. A latency win here MAY incidentally reduce
go's 1007-drift, but that is **not** a reason to do this refactor and is not claimed as
one.

This is a strictly **behavior-preserving refactor**: resolution results must stay
byte-identical (the RFC-056 differential + unit tests are the regression net). It is NOT
a semantics change, and touches ONLY the segment backing, never the resolution logic.

## Investigation (C++ reference, FoundationDB 7.3.x at /tmp/fdbsrc)

`RYWIterator` (RYWIterator.h/.cpp) zips two sub-iterators — `SnapshotCache::iterator` and
`WriteMap::iterator` — each of which partitions the full keyspace into contiguous
segments:
- **Merged segment** = the INTERSECTION of the two sub-iterators' current segments:
  `beginKey() = max(writes.begin, cache.begin)`, `endKey() = min(writes.end, cache.end)`
  (half-open `[begin, end)`).
- **`++`** advances whichever sub-iterator(s) own the current `endKey()` (the min end);
  the other stays put. **`--`** mirrors on `beginKey()` (the max begin). So neither
  sub-iterator overshoots; the merge boundary is always the finer of the two.
- **`skip(key)`** skips BOTH sub-iterators to the segment containing `key`, then
  recomputes the begin/end comparison flags.
- **`type()`** = `typeMap[writes.type()*3 + cache.type()]` (a 4×3 cross-product):
  CLEARED→EMPTY, INDEPENDENT_WRITE→KV, DEPENDENT_WRITE→KV if cache known else UNKNOWN,
  UNMODIFIED→passthrough cache type. (Go folds versionstamp→absent per #234.)
- KV segment: `beginKey() = key`, `endKey() = keyAfter(key)`.

`resolveKeySelectorFromCache` (the offset walk + empty-skip) uses ONLY: `skip`, `++`,
`--`, `beginKey`, `endKey`, `is_kv`/`is_empty_range`/`is_unknown_range`. Nothing requires
the full materialized partition — exactly the steppable-cursor interface.

## Design

Replace the materialized `[]rywSegment` with a **lazy `rywSegCursor`** built from two
sub-views over the existing structures (no new state, no precompute):

- **`writeView`** — a cursor over the write-map's partition: sorted write keys
  (`sortedKeys`) + cleared ranges (`cleared`). Given a probe `p`, its current segment is:
  a single-key `[k, keyAfter(k))` if `p` is a write key; a `[cb, ce)` CLEARED segment if
  `p` is in a cleared range; else an UNMODIFIED gap `[prevBoundary, nextBoundary)` found
  by binary search over `sortedKeys` + `cleared`. When `includeWrites=false` (snapshot),
  the whole space is one UNMODIFIED segment (write map bypassed).
- **`cacheView`** — a cursor over `snapshotCache.entries`: within a known entry, each
  `kv.Key` is a KV segment and the gaps are EMPTY; outside any entry is UNKNOWN. Found by
  binary search over `entries` (and the entry's `kvs`).
- **`rywSegCursor`** — merges them: `beginKey = max`, `endKey = min`, `typ` via the
  cross-product + the `valued`/versionstamp folding already in `segTypeAtLocked`.
  **`next`/`prev` are implemented as a single MERGED-boundary `skip`, NOT two independent
  per-view bumps** (binding, per FDB-C-dev + Torvalds): `next()` = `skip(endKey)` — both
  views land on the segment *containing* `endKey`, so the side that already ended there
  advances and the side that didn't stays, exactly reproducing C++ `operator++`'s
  `end_key_cmp==0` tie (advance both) vs `!=0` (advance one). `prev()` = compute the
  merged predecessor boundary `B = max(writeView.boundaryBelow(beginKey),
  cacheView.boundaryBelow(beginKey))` (each view's largest boundary strictly < beginKey,
  where boundaries INCLUDE the per-key successor `keyAfter(k)` that the materializer
  emits — the adjacent-key `k`/`k\x00` case), then `skip(B)`. Driving both views from one
  merged boundary (not independent per-view searches) prevents the two views desyncing —
  the failure mode the materialized array structurally cannot have.

`resolveKeySelectorFromCache` is rewritten to walk the cursor (skip/next/prev/begin/end/
typ) instead of indexing `segs`. `getKeyRYW`'s unknown-remerge loop is unchanged (it
already only consumes the resolve result). `buildSegmentsLocked` is deleted.

Cost: **O(walk distance × log(writes + cacheKeys))** per resolution — the log factor from
per-step binary searches; bounded by the selector offset + the empty/unknown skips, NOT
the cache size.

## Performance

The win is exactly the avoided O(cacheKeys) materialization (Step 0: 57 ms / 39 MB at
N=100k → expected flat in N after the fix). No regression for tiny caches (the
materialization was already cheap there); large wins for big-cache + getKey-loop
workloads. (Item-2's 1007-drift is NOT a justification — see Problem.)

## Plan (ordered — STEP 0 is a go/no-go gate)

**Step 0 — BENCHMARK FIRST (decides whether to proceed). DONE → PROCEED.**
`BenchmarkGetKeyRYW_CacheSize` (`ryw_getkey_test.go`) measures one cache-only `getKeyRYW`
(offset-1 probe at the cache midpoint) against a snapshot cache pre-loaded with N entries:

| N | ns/op | B/op | allocs/op |
|---|---|---|---|
| 1 | 1,154 | 856 | 11 |
| 100 | 37,261 | 29,736 | 115 |
| 1,000 | 441,093 | 288,488 | 1,019 |
| 10,000 | 5,535,221 (5.5 ms) | 3.5 MB | 10,027 |
| 100,000 | 56,989,918 (57 ms) | 39 MB | 100,037 |

Strictly **linear in cache size** (10× N → 10× time, bytes, allocs) — the predicted O(N)
materialization, and far worse than "negligible": a single getKey on a 100k-entry cache
is **57 ms / 39 MB**, and it runs *inside* the remerge loop. ~87 such calls exhaust FDB's
5 s transaction budget — which is also the mechanism behind item (2)'s 1007 drift. Verdict:
the win is large and real → **proceed**. (Numbers recorded; re-benchmark after the fix to
confirm cost goes flat in N.)

Then:

- **Behavior-preserving (the load-bearing check):** the entire RFC-056 suite stays green
  UNCHANGED — `ryw_getkey_test.go` unit tests + `TestDifferential_GetKeyRYW` vs libfdb_c +
  the corpus seeds + a `-test.fuzz` burst (≥100k execs, 0 mismatches). If resolution
  changed, these fail — they ARE the spec.
- **Equivalence property-test (oracle = the RETAINED materializer):** keep `buildSegments`
  compiled into the TEST binary as the oracle; over random (writes, cleared, cache) states
  + random probe keys, assert the lazy cursor's full skip/next/prev walk yields the
  IDENTICAL segment sequence (begin/end/type). **Must seed successor-collision + shared-
  boundary cases** (write key == cache key; adjacent keys `k`/`k\x00`; a write-key boundary
  coinciding with a cache-entry boundary — the `cmp==0` tie). This is the load-bearing
  faithfulness check (the differential only covers *walked* shapes, not arbitrary
  skip/prev landings).
- **Re-benchmark:** confirm the lazy iterator actually delivers the Step-0 win (cost now
  flat in N), and that small-cache cost did not regress.
- **Teeth:** a deliberately wrong cursor step (e.g. `next` skipping a KV) makes the
  equivalence property-test + a resolution case fail.
