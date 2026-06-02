# RFC-056: GetKey read-your-writes resolution (faithful resolveKeySelectorFromCache port)

**Status:** Reviewed (FDB-C-dev ACK, Torvalds ACK) — ready to implement
**Item:** RFC-010 C2-followup (RFC-056 audit), sub-item (1). Extends RFC-055.

## Problem

`Transaction.GetKey(selector)` resolves the key selector against **storage only**
(`readpath.go` `getKey` → `sendGetKey` loop), never merging the transaction's
pending writes. This is a real divergence from libfdb_c: in C++,
`ReadYourWritesTransaction::getKey` resolves the selector against the **merged**
read-your-writes view (pending writes + cleared ranges + snapshot cache) via
`resolveKeySelectorFromCache`, so a pending `Set`/`Clear`/atomic shifts where a
selector lands.

Example divergence (uncommitted txn): storage has `{a, c}`. Pending `Set(b)`.
`GetKey(firstGreaterThan(a))` → libfdb_c returns `b` (sees the pending write); Go
returns `c` (storage only). RFC-055's differential deliberately **excludes** this
path (`differential_ryw_test.go:132-139`) precisely because Go is known-wrong here.

A merged-`GetRange`-then-index-by-offset **shortcut was tried and verified WRONG**
on `{key, orEqual, offset>1}` (recorded in TODO + RFC-055): it does not reproduce
FDB's `removeOrEqual` normalization or the per-segment offset stepping. The only
correct fix is a faithful port of the segment-walk algorithm. "C++ is the spec."

## Investigation (C++ reference, /tmp/fdbsrc)

`getKey` is **implemented as a 1-row range read** (ReadYourWrites.actor.cpp:140-160):

```cpp
read(GetKeyReq read, Iter* it):
  if (read.key.offset > 0):
     result = getRangeValue(read.key, firstGreaterOrEqual(getMaxReadKey()), limit=1, it)
     if result.readToBegin: return allKeys.begin
     if result.readThroughEnd || result.empty: return getMaxReadKey()
     return result[0].key
  else:                       # offset <= 0
     read.key.offset++
     result = getRangeValueBack(firstGreaterOrEqual(allKeys.begin), read.key, limit=1, it)
     if result.readThroughEnd: return getMaxReadKey()
     if result.readToBegin || result.empty: return allKeys.begin
     return result[0].key
```

The heart is **`resolveKeySelectorFromCache`** (line 409), which walks a
`RYWIterator` — a merged segment iterator over `SnapshotCache::iterator` +
`WriteMap::iterator`:

- The keyspace `[allKeys.begin, allKeys.end)` is partitioned into half-open
  `[begin, end)` **segments**, each of type **KV** (one resolved key), **EMPTY_RANGE**
  (known to contain no keys), or **UNKNOWN_RANGE** (must read server). A separate
  `is_unreadable()` flag marks versionstamp writes (reading throws
  `accessed_unreadable`).
- Merge: `beginKey() = max(writes.begin, cache.begin)`, `endKey() = min(writes.end,
  cache.end)` (segment = intersection of the two sub-iterators' current segments).
  `type()` = a cross-product `typeMap[writes.type*3 + cache.type]`: CLEARED→EMPTY,
  INDEPENDENT_WRITE→KV, DEPENDENT_WRITE(atomic)→KV if cache known else UNKNOWN,
  UNMODIFIED→passthrough cache type.
- `resolveKeySelectorFromCache(key, it, maxKey, &readToBegin, &readThroughEnd,
  &actualOffset)`:
  1. `key.removeOrEqual()` — if `orEqual`, `key = keyAfter(key); orEqual = false`
     (FDBTypes.h). Normalizes to non-orEqual before the walk.
  2. `it.skip(key.getKey())`; if `offset<=0 && it.beginKey()==key && key!=allKeys.begin`
     then `--it`.
  3. Walk toward `firstGreaterOrEqual` form over **known** segments: while
     `offset>1 && !unreadable && !unknown && endKey<maxKey`: if `is_kv` then `--offset`;
     `++it`. Symmetric loop for `offset<1` going backward (`++offset` on each KV,
     `--it`), breaking when `offset==1`.
  4. Terminal clamps on fully-known data: `offset<1` → `readToBegin=true, key=allKeys.begin`;
     `offset>1` → `readThroughEnd=true, key=maxKey`.
  5. Skip known **empty** ranges forward (`while !unreadable && is_empty_range &&
     endKey<maxKey: ++it`), then the resolved key = current `it.beginKey()`.
  - If the walk **stops on an unknown/unreadable segment**, `key` is left as an
    equivalent `firstGreaterOrEqual` selector adjoining that segment — NOT yet resolved.

`getRangeValue` (line 593) drives the **server-read-then-remerge loop**: resolve both
ends from cache; if the walk lands on an unknown range (line 692), issue a server
`getRange` over the unknown tail (`skipUncached` bounds it, purely a batching
optimization — each server read strictly shrinks the unknown set, so the loop always
makes progress even if `skipUncached` is omitted), `cache.insert(...)` the result, and
**re-run `resolveKeySelectorFromCache`** (lines 768-771) — loop until the selector
resolves within known data or hits a terminal `readToBegin/readThroughEnd`.

**Read-conflict range** (`addConflictRange(GetKeyReq)`, lines 230-243) — getKey adds a
**RANGE** conflict from the selector base to the resolved key, NOT a single key:

```cpp
if (read.key.offset <= 0)   // backward
  readRange = [result, orEqual ? keyAfter(key) : key)
else                        // forward
  readRange = [orEqual ? keyAfter(key) : key, keyAfter(result))
```

Go's current `GetKey` adds only `addReadConflictForKey(selectorKey)`
(`transaction.go:573`) — a **single key**, which is a real existing divergence: a
concurrent write anywhere between the selector base and the resolved key must conflict,
and today it does not. This RFC fixes it as part of the port.

**Unreadable (versionstamp) keys.** The walk loops are guarded by `!it.is_unreadable()`
(lines 437, 444, 462, 469, 476): C++ stops at an unreadable segment and ultimately
throws `accessed_unreadable` on read. Go has no unreadable state — #234 established the
codebase-wide approximation: a pending-versionstamp key reads as **absent**
(`ryw.go` `resolveAtomics`→`unresolved`, excluded from Get/GetRange). The segment
iterator MUST treat an unreadable-versionstamp key as **absent** (neither a KV to step
over nor a stop), so `GetKey` agrees with `Get`/`GetRange` over the same key. This is
the same deferred throw-vs-absent divergence tracked under RFC-056 — documented, not
papered.

## Existing Go primitives (reuse)

- `snapshotCache.getKey(key) → (val, known)` and `getRangeKVs(begin,end) → (kvs,
  fullyKnown)` (`ryw_snapshot_cache.go:154,114`): the **known/unknown oracle**
  `resolveKeySelectorFromCache` needs. `cacheEntry{begin,end,kvs}` = a known range;
  gaps = unknown.
- `writes` map + `sortedKeys` binary search; `cleared []rywRange` +
  `isClearedLocked` (sorted, non-overlapping, coalesced); `resolveAtomics` +
  `isUnresolvedVersionstamp` (post-#234) — for per-key value + unreadable resolution.
- `tx.getKey(ctx, key, orEqual, offset)` storage primitive + `tx.GetRange` for the
  unknown-tail server reads.

## Design

Add, in `pkg/fdbgo/client`:

1. **`removeOrEqual` on the selector** (a small helper on the wire `KeySelectorRef`
   or a local `(key, orEqual, offset)` triple): `if orEqual { key = keyAfter(key);
   orEqual = false }`. `keyAfter(k) = append(k, 0x00)`.

2. **`rywSegmentIterator`** over `(writes+sortedKeys, cleared, snapshotCache)` — a
   steppable cursor yielding `segment{begin, endExcl []byte, typ segType, unreadable
   bool, kv *KeyValue}` where `segType ∈ {segKV, segEmpty, segUnknown}`. It mirrors
   `RYWIterator`: zip the write-map view (a key is KV/independent, an atomic is
   KV-if-cache-known-else-unknown, a versionstamp is unreadable, a cleared span is
   EMPTY, untouched is passthrough-cache) against the snapshot cache (known KV runs vs
   unknown gaps), with `beginKey=max`, `endKey=min`, and `skip(key)`/`next()`/`prev()`.
   `mergeBatch` is **not reusable** — it is a one-shot flat `[]KeyValue` producer with
   no steppable cursor; the iterator is new but built from the same structures.
   Represent the exclusive end of a single-key segment as `key+\x00` (the ExtStringRef
   "afterKey" trick) to avoid ambiguity.

3. **`resolveKeySelectorFromCache`** — direct port of lines 409-485 over
   `rywSegmentIterator`, returning `(resolvedKey []byte, readToBegin, readThroughEnd
   bool, actualOffset int)`.

4. **`getKeyRYW(ctx, sel)`** — port of `read(GetKeyReq)` (140-160) + the
   `getRangeValue` unknown-range loop, specialized to limit 1: forward/backward
   branch, resolve from cache, and on land-on-unknown do a bounded server `GetRange`
   over the tail → `snapshotCache.insert` → re-resolve → loop. Terminal clamps map to
   `allKeys.begin` / `getMaxReadKey()`.

5. **Read-conflict range** — port `addConflictRange(GetKeyReq)` (230-243) exactly:
   add the **base↔resolved RANGE** (offset-sign branch + orEqual `keyAfter`), replacing
   the current single-key `addReadConflictForKey`. This corrects an existing Go
   divergence; pinned by a differential conflict test (a concurrent write inside the
   range must make the txn conflict in both clients).

6. **Wire into `Transaction.GetKey`** (replace the storage-only call when RYW is
   enabled). **`Snapshot.GetKey`** is *also* storage-only today and therefore equally
   wrong (it resolves against neither the snapshot cache nor — correctly — the writes);
   the existing `transaction.go:581` "Snapshot.GetKey is correct" comment is FALSE and
   is deleted. Match C++ `readWithConflictRangeSnapshot` (SnapshotCache::iterator — no
   write map, no conflict range): the `rywSegmentIterator` takes an `includeWrites bool`,
   and snapshot `GetKey` runs the SAME resolution with `includeWrites=false` (snapshot
   cache only, no pending writes, no conflict range). This is one flag on the iterator,
   not a separate port — so it ships in this RFC, not deferred.

## Performance

GetKey with pending writes that already cover the resolution does **zero** server
round-trips (resolved from cache) — strictly faster than today's unconditional
storage `getKey`. The unknown-tail path issues the same server read it does today,
plus cache reuse for repeated selectors. Limit-1 specialization avoids the byte-limit
/ `itemsPastEnd` machinery of the general `getRangeValue`.

## Test plan

- **`TestGetKeyRYW_*`** unit tests over `rywCache` (no FDB): pending Set fills a gap
  (offset skips it), pending Clear removes a storage key (selector shifts past it),
  `{orEqual, offset>1}` (the case the shortcut got wrong), readToBegin/readThroughEnd
  off both ends, a pending atomic key, an unreadable versionstamp key (skipped/absent),
  and the unknown-range server-remerge loop (mock server returning partial ranges).
- **`TestDifferential_GetKeyRYW`** (`pkg/fdbgo/bench/differential_ryw_test.go`): the
  RFC-055 dual-client harness, now COMPARING GetKey over pending writes — remove the
  exclusion at `:132-139`. Seed identical storage, identical pending ops, shared read
  version, compare resolved keys byte-for-byte for all four selector kinds + offsets,
  clamped to the per-test prefix (selectors that walk off-prefix into the shared
  keyspace are not compared, per the RFC-055 clamp rule).
- **Conflict-range differential:** after a `GetKey`, commit a concurrent write inside
  the base↔resolved range from the *other* client and assert BOTH clients' txns
  conflict (`not_committed 1020`) identically — pins the range-conflict port and the
  fixed single-key→range divergence.
- **`TestSnapshotGetKeyRYW_*`:** snapshot `GetKey` resolves against the snapshot cache
  but NOT pending writes (a pending `Set` must NOT shift a snapshot selector), and adds
  no conflict range.
- **Land the held `FuzzRYWRead` GetKey axis** — fuzz selector resolution over random
  pending-write sequences vs libfdb_c; a manual burst produces 0 mismatches.
- **Teeth:** a deliberately broken resolution (ignore a pending Clear) makes a case
  fail.
- Revert-proof each fix: the bug-pinning tests fail on the storage-only path.
