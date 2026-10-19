# RFC-262: Port RYW getRangeValue, the storage-read loop under read-your-writes

**Status:** Accepted (revision 3; FDB C++ maintainer and Torvalds ACK at 7dc5bfc83)
**Item:** TODO.md, "Wire divergences found by the wire goldens": `getkey_*`, `set_then_range`.
**Spec:** FoundationDB 7.3.77. `fdbclient/ReadYourWrites.actor.cpp` (RYW below),
`fdbclient/NativeAPI.actor.cpp` (NAPI), `fdbclient/include/fdbclient/FDBTypes.h`,
`fdbclient/include/fdbclient/SnapshotCache.h`, `fdbclient/include/fdbclient/WriteMap.h`,
`bindings/c/fdb_c.cpp`. Line numbers are from the checkout at
`~/.local/share/fdb-perf/fdbsrc`. Go line numbers are from commit b73752b7b.

Revision 1 proposed extracting a request-sizing function and keeping the Go loops; the
Go loops are not ports of `getRangeValue`, so revision 2 replaced them. Revision 3 keeps
revision 2's algorithm (A3-A9) and traces (T1-T5) and fixes its design: the write map's
structural boundaries (A1, D1b), an independently safe migration and a defined pin owner
(D1, D11, Phases), NativeAPI's `startTransaction`, `resolveKey` and snapshot flag (A10,
D4), resumption by suspension point (D5), and mapped reads left where they are (D4). The
review-resolution index at the end maps every review point of both rounds to a section.

## Problem

When a read-your-writes read needs storage data it has not cached, libfdb_c and the Go
client send different `GetKeyValuesRequest`s. The wire goldens
(`tools/clientbench/wiregolden/go-divergences.diff`) show seven scenarios:

| Scenario | libfdb_c | Go |
|---|---|---|
| `getkey_offset` (`b`, orEqual, +2) | `[b\0, \xff)` limit 2 | limit 256 |
| `getkey_first_greater_than` | limit 1 | limit 256 |
| `getkey_last_less_than`, `getkey_last_less_or_equal` | limit -1 | limit -256 |
| `getkey_negative_offset` (`d`, -2) | limit -3 | limit -256 |
| `getkey_after_set` (pending `b1`) | `[b\0, \xff)` limit 1 | `[b\0, b1)` limit 256 |
| `set_then_range` (iterator mode, pending `b5`) | limit 80000, 4096 bytes | limit 10000, 4096 bytes |

The cause is structural. libfdb_c has one storage-read loop under RYW,
`getRangeValue` / `getRangeValueBack`, used for range reads and, with
`GetRangeLimits(1)`, for `getKey` (RYW:140-160). It walks a merged iterator over the
snapshot cache and the write map, sends a storage read for an uncached stretch, inserts
the reply into the snapshot cache, re-resolves both selectors and continues. The Go
client has three loops, none of which is that loop:

- `getKeyRYW` (`ryw_getkey.go:555-640`) reads one unknown segment at a time with a
  fixed `fillBatch = 256` (`:566`), stops reads at pending writes (`:616`, window ends at
  `res.unknownEnd`), and resolves selectors with a fused variant of
  `resolveKeySelectorFromCache` that also skips to the landing key (`:465-503`).
- `rywRangeRead.run` (`ryw.go:768-915`) reads byte-key windows with an invented row
  heuristic (`fetchLimit`, `:837-843`) and merges each reply with the write map below a
  "knowledge boundary" (`:860-866`, `mergeBatchLocked` `:1297-1475`).
- `getSnapshotRange` (`ryw.go:917-940`) plus the no-writes fast path (`:789-816`)
  answer from a re-walk heuristic (`replyEdgeKnown`, `:944-955`).

All three pass byte keys to a byte-key storage path (`rangeServerFunc`, `ryw.go:764`;
`getKeyRYW`'s callback, `ryw_getkey.go:559`) whose request builder hard-codes
`firstGreaterOrEqual` selectors (`readpath.go:1228-1229`) and has no `minRows`
(`readpath.go:996-1007`). C++ hands NativeAPI full selectors and full
`GetRangeLimits`. A faithful port therefore needs five things the Go code does not
have: the walk itself, a selector-capable NativeAPI `getRange`, a write-map version
that outlives the issuing call, the write map's structural boundaries, and an iterator
over the same segment universe C++ uses. This RFC designs all five and deletes the three
loops.

## The C++ algorithm

This section is normative. The Go design (next section) is a line-for-line port of it.

### A1. Types and the segment universe

**`GetRangeLimits`** (FDBTypes.h:727-757): `rows`, `minRows`, `bytes`, all C `int`.
`ROW_LIMIT_UNLIMITED = BYTE_LIMIT_UNLIMITED = -1`. Constructors set `minRows = 1`.
`hasRowLimit()` is `rows != -1`; `hasByteLimit()` is `bytes != -1` (so `bytes == 0`
*has* a byte limit); `isReached()` is `rows == 0 || (bytes == 0 && minRows == 0)`;
`hasSatisfiedMinRows()` is `hasByteLimit() && minRows == 0` (NAPI:2856-2877).
`decrement(kv)` sets `minRows = max(0, minRows-1)`, `rows--` if limited,
`bytes = max(0, bytes - 8 - |k| - |v|)` if limited (NAPI:2823-2829); the vector form
is the same summed (NAPI:2811-2821). `isValid()` (FDBTypes.h:753-756).

**Selectors** (FDBTypes.h:631-700): `removeOrEqual` replaces `(k, orEqual=true)` by
`(keyAfter(k), false)`; `keyAfter(\xff\xff) = \xff\xff` (FDBTypes.h:581-593).
`isFirstGreaterOrEqual` is `!orEqual && offset == 1`; `isBackward` is
`!orEqual && offset <= 0`; `isDefinitelyGreater`, `isDefinitelyLess` at :667-669.
Offsets are C `int`.

**Segments.** `RYWIterator` zips a `WriteMap::iterator` and a `SnapshotCache::iterator`
and partitions `["", \xff\xff)` into segments at the union of both maps' boundaries
(RYWIterator.cpp:94-126). Segment type is `typeMap[writes.type()*3 + cache.type()]`
(RYWIterator.cpp:26-49): UNKNOWN, EMPTY or KV. A cleared write range is EMPTY, an
independent write is KV, a dependent write (atomic over a storage base) is KV if the
cache knows the base and UNKNOWN otherwise, and an unmodified range takes the cache
type. `is_unreadable()` is the write map's flag (versionstamped writes, SVK ranges);
`type()` and `kv()` *throw* `accessed_unreadable` on an unreadable segment unless the
iterator bypasses it, which only point reads do (RYWIterator.cpp:45-49, 72-90; RYW:98).
`kv()` returns `nullptr` for a matched CompareAndClear (RYWIterator.cpp:82-87). Both
maps hold sentinels: the cache's `["", "")` (degenerate, skipped by `++`/`--`,
SnapshotCache.h:231-254) and `[\xff\xff, \xff\xff\0)` known-empty (SnapshotCache.h:308-313,
260-264); the write map's
`""`, `\xff\xff`, `\xff\xff\0` boundaries (WriteMap.h:129-134). `skip(\xff\xff)` lands on
`[\xff\xff, \xff\xff\0)`, which is EMPTY. Segment boundaries are `ExtStringRef`s,
keys with virtual trailing zeros (SnapshotCache.h:31-121); `isKeyAfter` tests
"this is `keyAfter(that)`".

A `WriteMap::iterator` is a version of the map: "This iterator also represents a
snapshot (will be unaffected by future writes)" (WriteMap.h:158-160). The snapshot
cache is not versioned; every read sees inserts made by every other read.

**Write-map structure.** The write-side segment boundaries are the write map's *nodes*,
not the set of written keys. A node (`WriteMapEntry`, WriteMap.h:69-95) is a key, an
operation stack, and five flags: `following_keys_cleared`, `following_keys_conflict`,
`is_conflict`, `following_keys_unreadable`, `is_unreadable`. A node with a non-empty stack
gives the segment `[k, keyAfter(k))` and then a range segment `[keyAfter(k), next)` unless
the next node is `keyAfter(k)`; a node with an empty stack gives `[k, next)` (iterator
`++`/`--`/`skip`, WriteMap.cpp:312-347; `beginKey`, WriteMap.h:176). Nodes are created and
removed by exactly five mutators plus the constructor's sentinels (WriteMap.h:129-134):

| Mutator | Called from | Structural effect |
|---|---|---|
| `mutate` (WriteMap.cpp:87-158) | `set` (RYW:2353), `atomicOp` (RYW:2295) | inserts or replaces an operation node, copying the surrounding range's flags |
| `clear` (WriteMap.cpp:160-203) | `clear(range)` (RYW:2402), `clear(key)` as `[k, keyAfter(k))` (RYW:2435), when the write adds a conflict | removes interior nodes; inserts begin/end nodes unless the neighbour is already a cleared conflict range, so coalescing depends on order and on conflict flags |
| `clearNoConflict` (WriteMap.cpp:527-596) | the same calls with `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE` | as `clear`, plus a node at every conflict/non-conflict toggle inside the cleared range |
| `addConflictRange` (WriteMap.cpp:244-302) | `addWriteConflictRange` with RYW enabled (RYW:2493-2499) | inserts nodes at both ends and flags interior nodes; no operation. A begin node on an operation keeps its stack (:258), and an end node can land at `keyAfter` of an operation node (:282-286), so the node list changes even where the boundaries do not |
| `addUnmodifiedAndUnreadableRange` (WriteMap.cpp:205-242) | `SetVersionstampedKey` (RYW:2264-2272) | the SVK candidate range: removes interior nodes, inserts unreadable begin/end nodes |

No read mutates the write map. Consequences the Go write map does not reproduce today:

- `addWriteConflictRange(m, n)` splits an UNKNOWN segment (or a cached KV run) at `m` and
  `n`. Whether it adds a node depends on the conflict flags already there: no begin node
  inside an existing conflict range (WriteMap.cpp:253-264), no end node if the segment at
  `n` is already conflicted (:278-288), and every write that adds a conflict sets those
  flags. Adding a boundary at every conflict-range edge would therefore also be wrong.
- `clear` coalesces by order and by flags: `clear(e)` then `clear(keyAfter(e))` leaves a
  node at `keyAfter(e)` (two one-key EMPTY segments), while the opposite order removes it
  (`end_coalesce_clear`, :180-191), unless the second clear adds no conflict
  (`clearNoConflict`, :560-566).

The walk observes these in three places. Each segment costs one iteration of
`skipUncached`'s traversal budget (A4), so conflict nodes in unknown space move where
request 1 ends (T6). An empty run split so that its first segment is a single key adds one
to the count `skipUncached` returns at the barrier, because `b` is not advanced across
empties (RYW:525-532) (T7). A conflict node inside a cached run ends `skipContiguous`'s
batch. Splits deeper inside an empty run are not observable, but D1b reproduces them
anyway, since it is a port, not an approximation.

`skipContiguous(key)` (RYWIterator.cpp:128-133; SnapshotCache.h:276-282) advances a KV
segment that comes from the cache over an unmodified write range to the last cached
value `< min(key, writes.endKey())` inside the same cache entry; `skipContiguousBack`
(RYWIterator.cpp:135-140; SnapshotCache.h:283-290) goes back to the first cached value
`>= max(key, keyAfter(writes.beginKey()))`. Both are no-ops on a KV that comes from the
write map.

### A2. Entry points and routing

`readWithConflictRange` (RYW:396-406) routes every read:

1. RYW disabled: `readThrough` (RYW:176-213), NativeAPI directly with the caller's
   selectors, limits and snapshot flag. `getKey` clamps the result to `maxReadKey`
   (:180-185). A backward range with `end.offset > 1` first resolves `end` with
   `getKey` (:191-199). A forward result is clipped below `maxReadKey` (:203-211).
2. `snapshot && snapshotRywEnabled <= 0`: `read()` over a `SnapshotCache::iterator`
   (:366-376). The cache only; no writes, nothing unreadable, no conflicts.
3. Otherwise: `read()` over a `RYWIterator`, then, only if `!snapshot`,
   `addConflictRange(ryw, req, it.extractWriteMapIterator(), result)` (:378-395).
   The conflict filter uses the iterator's write-map version, the one from issue.

The actor runs synchronously until its first `wait`, so selector resolution, all locally
answerable rows and the first storage request's selectors and limits are computed
against the state at the call.

`getRange` (RYW:1711-1769) rejects keys above `maxReadKey` (2004), returns empty for
`limits.isReached()`, rejects `!isValid()` (2012), applies `removeOrEqual` to both
selectors, returns empty for `begin.offset >= end.offset && begin.key >= end.key`, then
routes. `getKey` (:1695-1709) rejects `key > maxReadKey` (2004) and routes.

The C binding builds the caller's limits (fdb_c.cpp:978-1061): `limit == 0` becomes
`ROW_LIMIT_UNLIMITED`, `target_bytes == 0` becomes `BYTE_LIMIT_UNLIMITED`, EXACT
with both unlimited is error 2210, the mode's byte target applies when the caller gave
none (ITERATOR: 4096, 6144, ... indexed by the caller's `iteration`; EXACT: unlimited;
SMALL 256; MEDIUM 1000; LARGE 4096; SERIAL and WANT_ALL 80000), and
`GetRangeLimits(limit, target_bytes)` reaches RYW with `minRows = 1`. The binding's
`iteration` counts the *caller's* successive `get_range` calls. It is unrelated to
RYW's `requestCount`, which counts storage requests inside *one* `getRangeValue` call
and starts at 0 on every call.

### A3. resolveKeySelectorFromCache (RYW:409-485)

Input: a selector (by reference), an iterator, `maxKey`, and the out-flags
`readToBegin`, `readThroughEnd`, `actualOffset`.

1. `removeOrEqual` (:420). `alreadyExhausted = offset == 1` (:422). `it.skip(key)` (:424).
2. If `offset <= 0 && it.beginKey() == key && key != ""`: `--it` (:426-427).
3. Forward walk while `offset > 1 && !unreadable && !unknown && it.endKey() < maxKey`:
   a KV decrements `offset` (a CompareAndClear phantom is KV and counts); `++it`;
   `key = it.beginKey()` (:433-439).
4. Backward walk while `offset < 1 && !unreadable && !unknown && it.beginKey() != ""`:
   a KV increments `offset`, and on reaching 1 sets `key = it.beginKey()` and stops;
   otherwise `--it; key = it.endKey()` (:440-452).
5. If `!alreadyExhausted`: `actualOffset = offset` (:454-456).
6. If the stop segment is known (not unreadable, not unknown) and `offset < 1`:
   `readToBegin = true`, selector becomes `firstGreaterOrEqual("")` (:458-463).
   If known and `offset > 1`: `readThroughEnd = true`, selector becomes
   `firstGreaterOrEqual(maxKey)` (:465-470).
7. While the segment is readable, EMPTY and `endKey < maxKey`: `++it`,
   `key = it.beginKey()` (:472-476).

It never throws: every type test is guarded by `is_unreadable()` first. It returns a
selector equivalent to the input whose key adjoins `it`: a FGE selector on a known
landing, or the residual offset at an unknown or unreadable segment. It does not skip
empties or phantoms after a KV landing; the walk's iteration does that.

### A4. Traversal, counting and growth are three different limits

| Limit | Value | Applies to |
|---|---|---|
| Traversal budget | `clearLimit + 100` segments (0 under BUGGIFY) | `skipUncached` / `skipUncachedBack` iterations |
| Counted-clear cap | `clearLimit = requestCount ? 1 << min(requestCount, 20) : 0` | the returned empty count, clamped by the caller with `min(…, clearLimit)`; `countUncached*`'s `maxClears` |
| Byte growth | `bytes << min(requestCount, 20)`, capped at `REPLY_BYTE_LIMIT` (80000) | `setRequestLimits`, positive byte limits only |

`requestCount` is not capped; only the shifts are.

**`skipUncached(it, itEnd, iterationLimit)`** (RYW:510-538). Precondition: `it` is a
readable UNKNOWN segment. Locals `b = it.beginKey()` (start of the current run of
empties), `e = it.endKey()`. While `it != itEnd && --iterationLimit >= 0`:
- unreadable or EMPTY: if unreadable, or `e` is not `keyAfter(b)` (the empty run is not
  a single key), back up with `while (unreadable || !unknown) --it` and return the count;
  otherwise count one single-key empty;
- anything else (UNKNOWN, KV, phantom): `b = e`;
- `++it; e = it.endKey()`.

After the loop, back up the same way and return the count. So the stretch crosses
UNKNOWN segments, KV segments (pending writes, cached keys, dependent writes over
known bases, phantoms) and single-key empties; it stops at a multi-key empty, at an
unreadable segment, at `itEnd`, or when the budget runs out, and it always ends on an
UNKNOWN segment. A single-key empty is any one-key EMPTY segment: a pending `clear(k)`,
a cached absent point read (`SnapshotCache::insert(key, absent)`, SnapshotCache.h:329-343),
a one-key gap between two cached keys, or a one-key gap left by a write over a known
range. The count measures what was crossed; the caller clamps it to `clearLimit`
(RYW:702-703). On the first request `clearLimit` is 0: the request crosses pending
writes and clears up to 100 segments away but counts none of them.

**`countUncached(it, maxKey, maxClears)`** (RYW:540-567): returns 0 if `maxClears <= 0`;
otherwise the same single-empty rule walking forward while `e < maxKey`, stopping at
the first multi-key empty or unreadable segment, returning at most `maxClears`. No
traversal budget, no back-up. **`skipUncachedBack`** (RYW:841-871) and
**`countUncachedBack(it, maxClears)`** (RYW:873-894) mirror them: `e` tracks the end of the
empty run, the walk is `--it; b = it.beginKey()`, the back-up is `++it`, and
`countUncachedBack` stops at `b == ""`.

### A5. setRequestLimits (RYW:569-590)

```
minRows = min(max(1 + additionalRows, offset), INT_MAX)                       // :570-571
if hasRowLimit():  rows = min(max(max(1, rows) + additionalRows, offset), INT_MAX) // :572-575
if bytes == 0:                                                                 // :578
    bytes = BYTE_LIMIT_UNLIMITED
    if !hasRowLimit(): rows = min(max(max(1, rows) + additionalRows, offset), INT_MAX)
else if hasByteLimit():                                                        // :585
    bytes = min(int64(bytes) << min(requestCount, 20), REPLY_BYTE_LIMIT)
```

`additionalRows` is `int64`; `offset` and the limits are `int`. The input is the
caller's limits *as decremented so far* in this call (`requestLimit = limits`, :748), so
both the row limit and the byte limit shrink as rows are consumed locally. `bytes == 0`
is reachable: the walk keeps going after `limits.isReached()` while it still needs items
past the end bound (A6 step L5), and then the byte limit has been decremented to 0. That
branch switches to an unlimited byte limit and, when rows are unlimited, manufactures a
row limit from `additionalRows` and `offset`. `bytes == -1` stays -1. With rows unlimited
and bytes positive, `rows` stays -1.

### A6. getRangeValue, forward (RYW:592-813)

State (:599-608): `it` (the caller's iterator), `itEnd` (a copy), `result`,
`additionalRows = 0`, `itemsPastEnd = 0`, `requestCount = 0`, `readToBegin`,
`readThroughEnd`, `actualBeginOffset = begin.offset`, `actualEndOffset = end.offset`.
`begin`, `end` and `limits` are by-value locals that the walk mutates.

**Prologue.**
- P1 (:611-612) Resolve `begin` into `it` and `end` into `itEnd` (A3).
- P2 (:614-619) If `actualBeginOffset >= actualEndOffset && begin.key >= end.key`: empty,
  flags false. Else if `begin` is FGE at `maxKey` or `end` is FGE at `""`: empty with the
  flags.
- P3 (:621-638) If `end` is not FGE and `begin.key > end.key`: **await** a nested
  `read(GetKeyReq(end))` on the same iterator (a full getKey, A9, with its own state and
  its own `requestCount = 0`). Set `readToBegin` if it returned `""`, `readThroughEnd` if
  it returned `maxKey`. If `begin.key >= resolved && !begin.isBackward()`: empty, flags
  false; else if `resolved == ""`: empty with flags. Re-resolve both selectors, then
  enter the loop.

The three awaits resume differently. After P3's getKey, C++ re-resolves both selectors
(:634-637) and enters the loop. After U8's storage read it inserts and re-resolves
(U9-U10) and continues the loop. After L3's getKey it sets `end` and breaks straight to
the epilogue (:667-676): no re-resolution, no loop guard runs again.

**Loop.** Each pass runs the first matching step.
- L1 (:653-655) `result` empty and `actualBeginOffset >= actualEndOffset && begin.key >= end.key`:
  return empty, flags false.
- L2 (:657-659) `end.offset <= 1 && end.key == ""`: return empty with flags.
- L3 (:661-677) `(begin.offset >= end.offset && begin.key >= end.key)` or
  `(begin.offset >= 1 && begin.key >= maxKey)`: break if `end` is FGE or `result` is empty;
  otherwise **await** a nested getKey of `end`, set the flags as in P3,
  `end = firstGreaterOrEqual(resolved)`, break.
- L4 (:679-683) `it` readable, known, and `it.beginKey() > itEnd.beginKey()`: break if
  `end` is FGE, else return empty with flags.
- L5 (:685-686) `limits.isReached() && itemsPastEnd >= 1 - end.offset`: break.
- L6 (:688-690) `it == itEnd` and (`it` readable and known, or `begin.offset > 0 && end` FGE
  `&& end.key == it.beginKey()`): break.
- L7 (:692) dispatch on `it.is_unknown_range()`. **This throws `accessed_unreadable` if
  `it` is unreadable.** Unreadable segments are therefore an error only when the walk
  reaches them here; L4-L6 can stop it first.
- U (unknown, :692-771):
  - U1 (:693-697) `limits.hasByteLimit() && limits.hasSatisfiedMinRows() && result` non-empty
    `&& itemsPastEnd >= 1 - end.offset`: `result.more = true`, break.
  - U2 (:699-703) `ucEnd = it`; `clearLimit` as A4; if `it.beginKey() < itEnd.beginKey()`:
    `singleClears = min(skipUncached(ucEnd, itEnd, clearLimit + 100), clearLimit)`.
  - U3 `read_end` (:705-721):
    - `ucEnd != itEnd`: `firstGreaterOrEqual(ucEnd.endKey())`; if `end.offset < 1`,
      `additionalRows += 1 - end.offset`.
    - else if `end.offset < 1`: `firstGreaterOrEqual(end.key)` (not the unchanged selector),
      `additionalRows += 1 - end.offset`.
    - else `end` unchanged; if `end.offset > 1`:
      `singleClears += countUncached(ucEnd, maxKey, clearLimit - singleClears)` and
      `read_end.offset += singleClears` (the end selector moves forward over counted
      single empties after its unknown segment).
  - U4 (:723) `additionalRows += singleClears`.
  - U5 `read_begin` (:725-741):
    - `begin` FGE: `k = max(it.beginKey(), begin.key)`; **`begin = firstGreaterOrEqual(k)`**
      (the walk's begin moves, and every later step sees it), `read_begin = begin`.
    - `begin.offset > 1`: `firstGreaterOrEqual(begin.key)`, `additionalRows += begin.offset - 1`.
    - else (`offset < 1`): `read_begin = begin`; `n = countUncachedBack(it, clearLimit)`;
      `read_begin.offset -= n`; `additionalRows += n`.
  - U6 (:743-746) If `read_end.key < read_begin.key`: `read_end.key = read_begin.key`
    (offset kept).
  - U7 (:748-757) `requestLimit = limits`; `setRequestLimits(requestLimit, additionalRows,
    2 - read_begin.offset, requestCount)`; `requestCount++`; `additionalRows = 0`. Both
    happen before the await.
  - U8 (:758-759) **await** `tr.getRange(read_begin, read_end, requestLimit,
    Snapshot::True, Reverse::False)`. The internal read is always a snapshot read; conflicts
    belong to the wrapper (A2.3).
  - U9 (:760-765) `range = getKnownKeyRange(reply, read_begin, read_end)` (A8);
    `cache.insert(range, reply)`.
  - U10 (:768-771) Re-resolve `begin` into `it` and `end` into `itEnd`.
- K (KV, :772-794): `start = it.kv()`; if `nullptr` (phantom) `++it` and continue.
  `it.skipContiguous(end FGE ? end.key : maxKey)`; `maxCount` values from `start` to the
  new position; decrement `limits` per value while `!limits.isReached()`;
  `itemsPastEnd += maxCount - count`; append the counted values; `++it`.
- E (empty, :795-796): `++it`.

**Epilogue** (:799-811). `result.more |= limits.isReached()`. If `end` is FGE, drop
values `>= end.key`, and if any were dropped set `more = false`.
`result.readToBegin = readToBegin`; `result.readThroughEnd = !more && readThroughEnd`.

### A7. getRangeValueBack (RYW:896-1123): the differences

`end` resolves into `it`, `begin` into `itEnd` (:914-916). P2 is the same. P3 resolves
`begin` with a nested getKey when `begin` is not FGE and `begin.key > end.key`; it
returns empty if `resolved >= end.key && end.offset <= 1`, or empty-with-flags if
`resolved == maxKey` (:925-941). The loop:

- L1 as forward. L2' (:961-963) `!begin.isBackward() && begin.key >= maxKey`: empty with flags.
- L3' (:965-981) `(begin.offset >= end.offset && begin.key >= end.key)` or
  `(end.offset <= 1 && end.key == "")`: break if `begin` FGE or `result` empty; else
  nested getKey of `begin`, `begin = firstGreaterOrEqual(resolved)`, break.
- L4' (:983-988) `itemsPastBegin >= begin.offset - 1` and `it` readable, known and
  `it.beginKey() < itEnd.beginKey()`: break if `begin` FGE, else empty with flags.
- L5' (:990-991) `limits.isReached() && itemsPastBegin >= begin.offset - 1`: break.
- L6' (:993-997) If `end` is FGE and `end.key == it.beginKey()`: break if
  `itemsPastBegin >= begin.offset - 1 && it == itEnd`, else `--it`.
- U' (:999-1081):
  - U1' (:1000-1003) `limits.hasByteLimit() && result` non-empty `&& itemsPastBegin >=
    begin.offset - 1`: `more = true`, break. **No `hasSatisfiedMinRows()` term.** From the C
    API the two are equivalent (a non-empty result has decremented `minRows` from 1 to 0);
    the port keeps the asymmetry verbatim.
  - U2' `skipUncachedBack` guarded by `it.beginKey() > itEnd.beginKey()` (:1005-1009).
  - U3' `read_begin` (:1011-1026): `ucEnd != itEnd` gives `firstGreaterOrEqual(ucEnd.beginKey())`
    and `additionalRows += begin.offset - 1` if `begin.offset > 1`; else `begin.offset > 1`
    gives `firstGreaterOrEqual(begin.key)` and the same increment; else `begin` unchanged,
    and if `begin.offset < 1`, `singleClears += countUncachedBack(ucEnd, clearLimit -
    singleClears)` and `read_begin.offset -= singleClears`.
  - U4' `additionalRows += singleClears` (:1028).
  - U5' `read_end` (:1030-1045): `end` FGE: `k = min(it.endKey(), end.key)`,
    **`end = firstGreaterOrEqual(k)`**, `read_end = end`; `end.offset < 1`:
    `firstGreaterOrEqual(end.key)`, `additionalRows += 1 - end.offset`; else `end` unchanged,
    `n = countUncached(it, maxKey, clearLimit)`, `read_end.offset += n`, `additionalRows += n`.
  - U6' (:1047-1050) If `read_begin.key > read_end.key`: `read_begin.key = read_end.key`.
  - U7' (:1052-1061) `setRequestLimits(requestLimit, additionalRows, read_end.offset,
    requestCount)` (the offset argument is `read_end.offset`, not `2 - …`);
    `requestCount++`; `additionalRows = 0`.
  - U8' (:1062-1063) **await** `tr.getRange(read_begin, read_end, requestLimit,
    Snapshot::True, Reverse::True)`.
  - U9' (:1064-1075) `range = getKnownKeyRangeBack(...)`; the reply is reversed to
    ascending order and inserted.
  - U10' (:1078-1081) Re-resolve `end` into `it`, `begin` into `itEnd`.
- K'/E' (:1082-1108): a KV with non-null `kv()` takes `skipContiguousBack(begin FGE ?
  begin.key : "")`, consumes values from the high end down, `itemsPastBegin += maxCount -
  count`, appends in descending order. Then, for every non-unknown segment including
  EMPTY and phantoms: if `it == itEnd` break, else `--it`.

Epilogue (:1111-1120): `more |= isReached()`; if `begin` FGE, drop values `< begin.key`
and clear `more` if any were dropped; `readToBegin = !more && readToBegin`;
`readThroughEnd = readThroughEnd`. The flag asymmetry against A6 is intentional in C++.

### A8. Known ranges and cache insertion

`getKnownKeyRange(data, begin, end)` (RYW:487-508), forward:
```
beginKey = begin.offset <= 1 ? begin.key : \xff\xff
endKey   = !data.more && end.offset >= 1 ? end.key : ""
if data.readToBegin: beginKey = ""
if data.readThroughEnd: endKey = \xff\xff
if data non-empty:
    beginKey = min(beginKey, data[0].key)
    endKey = data.readThrough present ? max(endKey, readThrough)
           : (!data.more && last.key < endKey ? endKey : keyAfter(last.key))
if beginKey >= endKey: empty range
```
`getKnownKeyRangeBack` (:816-839) mirrors it: `beginKey = !more && begin.offset <= 1 ?
begin.key : \xff\xff`, `endKey = end.offset >= 1 ? end.key : ""`; with data,
`beginKey = readThrough present ? min(readThrough, beginKey) : (!more && last > beginKey ?
beginKey : last)` where `last` is the smallest key, and `endKey = data[0].key < endKey ?
endKey : keyAfter(data[0].key)`.

`SnapshotCache::insert(range, values)` (SnapshotCache.h:345-382) ignores an empty range,
trims the range against cache entries already straddling either end, erases entries
strictly inside, and inserts one entry. Values are ascending; a reverse reply is reversed
first (RYW:1068-1072). The NativeAPI result supplies `more`, `readToBegin`,
`readThroughEnd` and `readThrough` (A10), and the selectors are the request's, so a reply
to a selector with offset above 1 or below 1 is known over the keys it actually covered,
not over the selector keys.

### A9. getKey (RYW:140-160, 230-243)

`read(GetKeyReq)`:
- `offset > 0`: `getRangeValue(sel, firstGreaterOrEqual(maxReadKey), GetRangeLimits(1))`;
  `readToBegin` gives `""`; `readThroughEnd` or no rows gives `maxReadKey`; else
  `result[0].key`.
- `offset <= 0`: **`sel.offset++`** (:151), then `getRangeValueBack(firstGreaterOrEqual(""),
  sel, GetRangeLimits(1))`; `readThroughEnd` gives `maxReadKey`; `readToBegin` or no rows
  gives `""`; else `result[0].key`.

`lastLessThan(c) = (c, false, 0)` becomes `(c, false, 1) = firstGreaterOrEqual(c)`;
`lastLessOrEqual(c) = (c, true, 0)` becomes `firstGreaterThan(c)`, which A3 turns into
`firstGreaterOrEqual(c\0)`; `(d, false, -2)` becomes `(d, false, -1)`, so the backward walk
reads `[FGE(""), FGE(d))` with `additionalRows += 2` (U5', end.offset < 1).
`GetRangeLimits(1)` has `bytes = -1`, so getKey requests never carry a byte limit.

The conflict range (:230-243, filtered by `updateConflictMap` :322-351 through the read's
own write-map version) uses the *original* selector: for `offset <= 0`,
`[result, orEqual ? keyAfter(key) : key)`; otherwise `[orEqual ? keyAfter(key) : key,
keyAfter(result))`.

### A10. NativeAPI getRange under RYW (NAPI:4596-4893, 4222-4240, 4457-4500, 5801-5848)

`tr.getRange(begin, end, limits, snapshot, reverse)` reaches `getRangeInternal`
(:5801-5848): returns empty if `limits.isReached()`, errors if `!isValid()`, removes
`orEqual` from both selectors, returns empty when inverted, then `getRange`:

- If `begin.key == "" && begin.offset < 1`: `readToBegin = true`, `begin = FGE("")` (:4623-4626).
- Loop (:4631-4889): stop when `end.key == ""` and (`end.offset < 1` or FGE). Locate
  `reverse ? end.key : begin.key` with `isBackward` of `end - 1` or `begin` (:4638-4639).
  The request's begin becomes `FGE(shard.begin)` when reverse and
  `(begin-1).isDefinitelyLess(shard.begin)` (unless `begin` is FGE at `shard.begin`); its
  end becomes `FGE(shard.end)` when forward and `end.isDefinitelyGreater(shard.end)`.
  Either sets `modifiedSelectors` (:4651-4679).
- `transformRangeLimits` (:4222-4240): if `bytes != 0`, `limit = hasRowLimit ? min(80000,
  rows) : 80000`, negated for reverse, and `limitBytes = hasByteLimit ? min(80000, bytes) :
  80000`; if `bytes == 0`, `limitBytes = 80000` and `limit = ±minRows`.
- After a reply: `limits.decrement(rows)`. The reverse lastLessOrEqual fix (:4756-4759).
  `finished = isReached() || (!modifiedSelectors && !more) || hasSatisfiedMinRows()`;
  `readThrough = modifiedSelectors && !more` (:4761-4762). A finished first reply is
  returned directly with `more = modifiedSelectors || isReached() || rep.more`, and
  `readThrough` sets `more = true` and `readThrough = reverse ? shard.begin : shard.end`
  (:4765-4808). Later replies append and finish the same way (:4810-4826).
- Not finished: `!rep.more` with no rows falls back to `getRangeFallback` with the original
  selectors and limits (:4828-4845); `!rep.more` with rows continues at the shard
  boundary; `rep.more` continues at `firstGreaterThan(last)` forward or
  `firstGreaterOrEqual(last)` reverse (:4846-4851). So a forward continuation request
  inside one call carries `orEqual = true` on the wire.
- `wrong_shard_server` falls back; `all_alternatives_failed` invalidates, delays and retries
  (:4858-4885).
- Before any location lookup, `getRange` awaits `trState->startTransaction()` (the GRV)
  and calls `validateVersion(readVersion())` (:4617-4618; `DatabaseContext::validateVersion`,
  NAPI:511-524: version 0 is `client_invalid_operation`, a switchable cluster rejects a
  version below `minAcceptableReadVersion` with `transaction_too_old`). Every request of
  the call carries that one read version. The checks in `getRangeInternal` come first, so
  an empty or inverted request acquires no read version; a RYW read that never reaches
  `tr.getRange` acquires none either.
- `getRangeFallback` (:4457-4500) resolves both selectors with `resolveKey` (:4447-4455):
  a FGE selector resolves to its key and a FGT selector to `keyAfter(key)` with no
  request; anything else calls NativeAPI `getKey`. `fb` and `fe` are both created before
  either is awaited (:4465-4468), so both `GetKeyRequest`s are in flight together, begin's
  first when its location is cached. Then it returns empty if `b >= e`, reads
  `getExactRange([b, e), limits)` and sets `readToBegin` when `b == ""` (and `!more` for
  reverse), `readThroughEnd` when `e == \xff\xff` (and `!more` for forward).
- With `snapshot = false` (only the RYW-disabled path), `getRangeInternal` registers a
  conflict promise (:5841-5844) and `getRangeFinished` fills it from the selectors and
  result (:4558-4590). With `snapshot = true` it adds none. The RYW-disabled backward
  pre-read `tr.getKey(read.end, snapshot)` (RYW:191-199) likewise adds a getKey conflict
  only when `snapshot` is false (NAPI:5778-5787, 5761-5776).

`minRows` matters here: a reply cut by its byte limit before `minRows` rows leaves
`bytes == 0, minRows > 0`, which is neither reached nor satisfied, so the next request
uses the `bytes == 0` branch: `limit = ±minRows`, `limitBytes = 80000`.

### A11. The seven goldens, derived

- `getkey_offset`: `(b, true, 2)` → A3 → `(b\0, false, 2)`, unknown. U3: `ucEnd == itEnd`,
  `read_end = FGE(\xff)`. U5: offset > 1 → `FGE(b\0)`, `additionalRows = 1`. A5 with
  `offset = 2 - 1 = 1`: `minRows = 2`, `rows = max(1 + 1, 1) = 2`, bytes -1. A10: limit 2,
  limitBytes 80000.
- `getkey_first_greater_than`: `FGE(b\0)`, rows 1.
- `getkey_last_less_than` / `_or_equal`: A9 gives `FGE(c)` / `FGE(c\0)`; U5' takes the FGE
  branch, `k = min(it.endKey(), end.key)`, so `[FGE(""), FGE(c))` reverse, rows 1 → -1.
- `getkey_negative_offset`: `[FGE(""), FGE(d))`, `additionalRows = 2`, A5 with
  `offset = read_end.offset = 1`: `rows = minRows = 3` → -3.
- `getkey_after_set` (pending `b1`): segments `["", b1)` U, `[b1, b1\0)` KV,
  `[b1\0, \xff\xff)` U. `begin = FGE(b\0)` lands in the first; `itEnd` is the last. U2 with
  `clearLimit = 0`: `skipUncached` crosses the KV, reaches `itEnd`, returns 0. U3:
  `read_end = end = FGE(\xff)`. Rows 1. The reply (say `c`) is cached as `[b\0, c\0)`; A3
  then resolves `begin` across `[b\0, b1)` EMPTY to `b1`, and K returns `b1`.
- `set_then_range`: rows unlimited (the Go facade passes `MaxInt32`, which saturates to the
  same wire value, A5), bytes 4096, pending `b5` crossed exactly as above: `[a, d)`,
  limit 80000, limitBytes 4096.

## The current Go code, against the algorithm

| C++ | Go today | Why it cannot stay |
|---|---|---|
| One walk, cache insert, re-resolve (A6-A7) | three loops: `getKeyRYW`, `rywRangeRead.run` slow and fast paths, `getSnapshotRange` (`ryw_getkey.go:555-640`, `ryw.go:768-940`) | they merge storage replies with writes instead of caching and re-walking, so their batches and stop points differ |
| Request selectors and limits from A5-A7 | `fillBatch = 256` (`ryw_getkey.go:566`); `fetchLimit` (`ryw.go:837-843`); `rywRequestBytes` (`:1038-1050`) | invented; no `minRows`, no `additionalRows`, no selector offsets |
| `skipUncached` crosses writes | window ends at the unknown segment (`ryw_getkey.go:616`); `mergeBatchLocked` boundary (`ryw.go:860-866`) | stops at pending writes |
| `requestCount` per RYW request, before the await (RYW:750) | `countedGetRange` counts storage callback calls (`ryw.go:829-832`) | correct only by accident of call structure |
| Selector + `GetRangeLimits` to NativeAPI | `rangeServerFunc(begin, end []byte, limit, byteTarget, reverse)` (`ryw.go:764`); FGE-only builder (`readpath.go:1228-1229`) | cannot send `read_begin.offset != 1`, `read_end.offset > 1`, `orEqual` continuations or `minRows` |
| `getKnownKeyRange` with `more`, `readThrough`, flags (A8) | `cacheServerReply` (`ryw.go:1196-1232`) from byte bounds | wrong for selector requests; getKey's callback drops `readThrough` (`ryw_getkey.go:559`) |
| A3 exact | fused resolver that also skips to the landing (`ryw_getkey.go:465-503`), clips at `hi = maxReadKey` and special-cases off-end (`:396-417`) | the walk needs A3's transformed selector and flags, and C++'s segment universe |
| Write-map version from issue | `captureWriteSpanLocked(begin, end)` (`ryw.go:970-992`) | covers only `[begin, end)`; the walk reaches beyond (end resolution skips empties past `end`; selector offsets walk anywhere) |
| Write-side boundaries are WriteMap nodes (A1) | boundaries from write keys, merged cleared ranges and SVK ranges (`ryw_getkey.go:290-320`; cleared ranges coalesce on overlap or adjacency, `ryw.go:1642-1683`); `AddWriteConflictRange` only appends to `tx.writeConflicts` (`transaction.go:3264-3289`) | misses conflict nodes and order-dependent clear nodes, so traversal budgets and single-key tests differ |
| `tr.getRange` captures one read version after `startTransaction` (NAPI:4617-4618) | each send re-reads `tx.readVersion` (`readpath.go:1019-1021`, `:279-281`) | a request's version is not the operation's |
| Unreadable throws at L7 / K | `unreadableScanCapLocked` reach heuristics (`ryw.go:718`, `:785-787`, `:810`, `:885`, `:910`) | approximates the throw point |
| `minRows` continuation, FGT continuation (A10) | `rangeScanImpl` is `getExactRange`-shaped (`readpath.go:700-960`) | no `minRows`; continues with `FGE(keyAfter(last))` |

The slow path is replaced wholesale. Patching sizing into the existing loops would keep
their merge semantics, their stop points and their byte-key storage interface, all of
which differ from A6.

## Design

### D1. A versioned write map

The write-map fields of `rywCache` (`ryw.go:24-54`: `writes`, `sortedKeys`, `cleared`,
`unreadableRanges`, `unreadableKeys`, `bypassUnreadable`) and the node index of D1b move
into a struct reached by pointer, `c.w`:

```go
// rywWrites is one version of the write map. A pinned version is immutable:
// it is the C++ WriteMap::iterator snapshot (WriteMap.h:158-160).
type rywWrites struct {
	writes           map[string]rywEntry
	sortedKeys       []string
	cleared          []rywRange
	unreadableRanges []rywRange
	unreadableKeys   []string
	bypassUnreadable bool
	nodes            rywNodes // D1b
	pins             int      // reads holding this version; guarded by rywCache.mu
}

func (c *rywCache) pinWritesLocked() *rywWrites     // c.w.pins++; return c.w
func (c *rywCache) unpinWritesLocked(w *rywWrites)   // w.pins--; panics below zero
func (c *rywCache) mutableWritesLocked() *rywWrites // if c.w.pins > 0 { c.w = c.w.clone() }; return c.w
```

Rules:

- **One live version, no swaps.** `c.w` is always the live version. Nothing installs another
  version in `c.w`, even temporarily: `swapWriteSpanLocked` (`ryw.go:1006-1013`) and its
  callers `localRangeInSpan`, `mergeBatchInSpan` and `conflictRanges` (`:750-760`,
  `:1017-1033`) are replaced in phase 2 by helpers that take the version as a parameter
  (`localRangeLocked(w, …)`, `mergeBatchLocked(w, …)`, `conflictRangesLocked(w, …)`). So
  `mutableWritesLocked` can only ever clone or mutate the live version.
- **Every mutation clones first if pinned.** Every function that assigns a write-map field
  calls `mutableWritesLocked` first: `set`, `clear`, `clearRange`, `atomic`,
  `addClearedRangeLocked`, `addUnreadableRangeLocked`, the unreadable-key index updates,
  `setBypassUnreadable` and D1b's `addConflictRange` (the current assignment sites are
  `ryw.go:238, 252, 266, 284-291, 535, 547, 570, 1633-1683, 2078, 2226, 2236, 2252`). A unit
  test enumerates the mutators and checks each one clones a pinned version.
- **No read mutates the live or a pinned version.** The only read-side writer today is
  `mergeBatchLocked`, which stores resolved atomics (`ryw.go:1397, 1410`). Until phase 3
  deletes it, it runs only on the private span copy the old range loop already makes
  (`captureWriteSpanLocked`, `ryw.go:970-992`, which returns a fresh unpinned `*rywWrites`
  that is never `c.w`); it asserts `w != c.w && w.pins == 0`, and the test build turns a
  violation into a failure. `ensureSortedLocked` and D1b's lazy node build may fill derived
  indexes on a pinned version under `c.mu`; they do not change what the version means.
- **Clone.** `clone` copies the map (entries by value, atomic stacks re-sliced as
  `captureWriteSpanLocked` does, `ryw.go:983-985`, because `coalesceOverAtomics` rewrites
  `stack[top]` in place, `:1752-1753`), the slices, and the node index. Key, value and
  parameter bytes are shared; they are never written after insertion (the slab
  `allocBytes` only appends, `ryw.go:142-155`; range bounds are replaced, not edited,
  `:1661-1683`).
- `reset` (`:171-185`) installs a fresh version and increments `c.gen` (D5).

Cost: a synchronous read pins and unpins within one call, so a writer clones only while
another read is in flight. That clone is O(writes). The current design copies the writes in
`[begin, end)` on every fetching read that has any. Phase 2 adds a benchmark for the bad
case (alternating `StartGetRange` and `Set` with 10k pending writes) and reports it against
the merge-base; a persistent map (C++'s PTree) is the remedy if it regresses the
record-layer benchmarks.

### D1b. The write map's nodes

`rywWrites.nodes` ports `WriteMap`'s node set without operation stacks:

```go
type rywNode struct {
	key []byte
	op  bool // the C++ stack is non-empty
	followingCleared, followingConflict, isConflict, followingUnreadable, isUnreadable bool
}
type rywNodes struct {
	sorted []rywNode       // by key; holds the sentinels "", \xff\xff, \xff\xff\0 (WriteMap.h:129-134)
	log    []rywNodeOp     // structural ops not yet applied (lazy build, below)
}

// Ports of the five mutators' effect on nodes and flags.
func (n *rywNodes) mutate(key []byte, addConflict, versionstamp bool)  // WriteMap.cpp:87-158
func (n *rywNodes) clear(begin, end []byte, addConflict bool)          // :160-203, or :527-596 when !addConflict
func (n *rywNodes) addConflictRange(begin, end []byte)                 // :244-302
func (n *rywNodes) addUnmodifiedAndUnreadableRange(begin, end []byte)  // :205-242
```

A `nodeIter` ports `WriteMap::iterator` over `sorted` (`skip`, `++`, `--`, `beginKey`,
`endKey`, `keyAtBegin`, `is_cleared_range`, `is_conflict_range`, `is_unreadable`,
`is_operation`; WriteMap.cpp:305-347, WriteMap.h:150-200), and the mutator ports are written
against it line for line, including `insert_begin`, `insert_end`, `end_coalesce_clear` and
`clearNoConflict`'s toggle nodes. They are the only writers of `sorted`.

Call sites. The `addConflict` argument is C++'s `addWriteConflict`
(`!options.getAndResetWriteConflictDisabled()`, RYW:2220, 2328, 2358, 2407):

- `stateSet` / `stateClear` / `stateClearRange` (`transaction.go:1324-1327, 1362-1365,
  1391-1394`) and the atomic path (`:1531-1535`): `addWriteConflictForKeyLocked` and
  `addWriteConflictLocked` (`:2884-2924`) return whether they recorded a conflict, and that
  value goes to `tx.ryw.set/clear/clearRange/atomic`. The Go-only `writeConflictsDisabled`
  (`:261-265`) therefore reads as `false`, like `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE`.
  `SetVersionstampedKey` passes `false` and calls `addUnmodifiedAndUnreadableRange` first, as
  RYW:2264-2272 does.
- `stateAddWriteConflictRange` (`:3264-3289`), after its clamps and when RYW is enabled,
  calls the new `tx.ryw.addConflictRange(begin, end)`, a mutator under D1's rules. With
  RYW disabled it does not, as RYW:2493-2496. It keeps appending to `tx.writeConflicts`,
  which is how Go ships write conflicts.
- Bounds are the same clamped bounds the stores receive (`getMaxClearKeySize`, RYW:2389-2399).

Lazy build. Write-only transactions must not pay O(n) per out-of-order write
(`sortedKeys` is lazy for the same reason, `ryw.go:28-29, 45-46`). Until the first walk needs
nodes, mutators append a `rywNodeOp` to `log`. The first walk replays the log in order; a run
of consecutive `mutate` ops is merged in one sorted pass, which is exact because `mutate`
only copies the flags of the range containing the key and no `mutate` changes another
node's flags; repeated keys and keys already present combine order-independently (the first
op copies the range flags, later ones only OR in `addConflict` and `versionstamp`). After the build, mutators apply directly, at the
same O(n) slice cost `sortedKeys` already has.

Roles. Nodes are authoritative for write-side *boundaries*; the stores stay authoritative
for values, atomic stacks and segment *types* (`segTypeAtLocked`, `ryw_getkey.go:74-133`),
which RFC-058 already matches to C++. A property test replays random sequences of all
mutators, with and without conflicts and with RYW-disabled interleavings, and checks at
every node-derived segment that `op` agrees with a `writes` entry, `followingCleared` with
`isClearedLocked`, and the unreadable flags with the unreadable checks. The one exception is
RFC-098's deliberate divergence: Go keeps pending writes inside an SVK range where C++
removes their nodes. Such keys are inside an unreadable segment, which every walk treats as
a barrier or an error at its edge (A4, A6 L7), so boundaries inside it are never observed;
the test excludes them by that argument, and S1 case 4 checks the edge.

### D2. The cursor over a version and C++'s universe

`rywSegCursor` (`ryw_getkey.go:146-192`) and its helpers (`segTypeAtLocked` `:74-133`,
`boundCandidatesLocked` `:280-339`, `mergedBoundsLocked`, `prevBoundaryLocked`,
`isClearedLocked`, `isUnreadableLocked`, `segmentValueLocked` `ryw.go:1120-1130`,
`conflictRangesLocked` `:1528`) take `w *rywWrites` instead of `includeWrites bool` and the
live fields. `w == nil` is `SnapshotCache::iterator` (A2.2).

Write-side boundary candidates come from `w.nodes` (each node's key, and `keyAfter(key)` for
an operation node unless the next node is that key), replacing the write-key, cleared and
SVK sources of `boundCandidatesLocked` (`ryw_getkey.go:290-320`). The window argument
there carries over: nodes are one sorted list of boundaries. A private span copy (D1) has no
node index; until phase 3 deletes the old range loop, a cursor over such a copy keeps the
current sources, so the old loop's requests do not change before phase 3.

The walk's cursor uses `hi = \xff\xff` regardless of `maxReadKey`, so its segments are
C++'s: no boundary at `\xff` unless something is cached or written there, and
`getkey_after_set`'s last segment is `[b1\0, \xff\xff)`. Seeking a key `>= \xff\xff` lands
on the sentinel `[\xff\xff, \xff\xff\0)`, typed `segEmpty` (A1). `prev()` from the sentinel goes
to the last real segment. `next()` on the sentinel is unreachable (A6 stops at L4/L6
first); it returns `internal_error` (4100) instead of looping. The snapshot cache accepts
entries ending at `\xff\xff` (A8 inserts them for `readThroughEnd`).

New cursor operations, each a port:

| Go | C++ |
|---|---|
| `cur.same(o)` | `it == itEnd`: both valid, equal `begin` (one lock hold, one partition) |
| `cur.known()`, `cur.isKV()` (`segKV` or `segPhantom`), `cur.empty()`, `cur.unreadable()` | `!is_unknown_range()`, `is_kv()`, `is_empty_range()`, `is_unreadable()` |
| `cur.kv() (KeyValue, bool)` | `kv()`; false for a phantom |
| `cur.skipContiguous(bound []byte) []KeyValue` | RYWIterator.cpp:128-133: on a cache KV under an unmodified write segment, land on the last value of the same cache entry `< min(bound, the write segment's end node)`; return the run |
| `cur.skipContiguousBack(bound []byte) []KeyValue` | RYWIterator.cpp:135-140: first value `>= max(bound, keyAfter(write segment begin))` |

### D3. Limits, selectors, native results

```go
// rangeLimits is GetRangeLimits (FDBTypes.h:727-757); int32 fields, -1 = unlimited.
type rangeLimits struct{ rows, minRows, bytes int32 }

func newRangeLimits(rows, bytes int) rangeLimits // rows 0 → -1 (fdb_c.cpp:991-992); minRows 1
func (l rangeLimits) isReached() bool            // NAPI:2856
func (l rangeLimits) hasRowLimit() bool
func (l rangeLimits) hasByteLimit() bool
func (l rangeLimits) hasSatisfiedMinRows() bool  // NAPI:2875
func (l rangeLimits) isValid() bool              // FDBTypes.h:753
func (l *rangeLimits) decrement(kv KeyValue)     // NAPI:2823-2829

// setRequestLimits is RYW:569-590. Arithmetic in int64, results clamped to MaxInt32.
func setRequestLimits(l *rangeLimits, additionalRows int64, offset int32, requestCount int)
```

The existing `KeySelector` (`mappedrange.go:107-111`) gains `removeOrEqual`, `isFGE`,
`isFGT`, `isBackward`, `isDefinitelyGreater/Less` and `keyAfter` with the `\xff\xff` rule.
Selector offsets use `int32` arithmetic that wraps as C++'s `int` does in practice
(`2 - read_begin.offset`, `read_end.offset += n`, `read_begin.offset -= n`);
`additionalRows += 1 - end.offset` and `begin.offset - 1` are computed in `int32` and
widened, as C++ does. `mappedRangeLimits` (`mappedrange_selectors.go:17-59`) is left alone
(D4).

```go
type nativeRangeResult struct {
	kvs                               []KeyValue // scan order
	more, readToBegin, readThroughEnd bool
	readThrough                       []byte     // nil when absent
	conflict                          *[2][]byte // set only when snapshot == false (A10)
}
type nativeRangeFunc func(ctx context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error)
```

### D4. Selector-capable storage dispatch: `nativeGetRange`

```go
func (tx *Transaction) nativeGetRange(ctx context.Context, begin, end KeySelector,
	limits rangeLimits, snapshot, reverse bool) (nativeRangeResult, error)
```

It ports A10 for plain key-values, in this order:

1. `getRangeInternal`'s checks (NAPI:5810-5834): `isReached()` → empty; `!isValid()` → 2012;
   `removeOrEqual` both; inverted → empty. No read version is taken for these.
2. **Read version.** `rv, err := tx.readVersionForOperation(ctx, 0)`
   (`transaction.go:759-777`), once. It is Go's `startTransaction` (GRV if none yet) plus
   `validateVersion` (`validateExplicitVersion`, `read_version_state.go:183`, for a
   user-set version; C++'s check at NAPI:511-524). `rv` is threaded to every request of
   the call, including the fallback's getKeys and exact range, instead of each send
   re-reading `tx.readVersion` (`readpath.go:1019-1021, 279-281`). The walk never calls it,
   so a read answered locally takes no read version, as in C++, where such a read never
   reaches `tr.getRange`.
3. The loop of A10 (NAPI:4623-4893): the `readToBegin` prologue, location lookup, per-shard
   request selectors, `transformRangeLimits` with the `bytes == 0` branch, `decrement`,
   `finished`, `readThrough`, the first-reply shortcut, the FGT forward / FGE reverse
   continuation, and the error arms (wrong shard falls back; all-alternatives and
   maybe-delivered invalidate and retry; reply timeout re-sends). It counts
   `getRangeRequests` once per call (`readpath.go:634`).
4. **Fallback** (NAPI:4457-4500) through a `resolveKey` port (NAPI:4447-4455):

   ```go
   // startResolveKey: FGE → its key, FGT → keyAfter(key), both ready with no request;
   // otherwise a goroutine runs getKeyImpl at version rv. parked closes when that
   // goroutine first blocks on the network (location lookup or GetKeyRequest), or finishes.
   func (tx *Transaction) startResolveKey(ctx context.Context, sel KeySelector, rv int64) (fut *keyFuture, parked <-chan struct{})
   ```

   The fallback starts `begin`'s resolution, waits for its `parked`, starts `end`'s, then
   awaits begin and end. That is the flow order: `resolveKey(begin)` runs until its first
   `wait` before `resolveKey(end)` is created (NAPI:4465-4466), and both are outstanding
   before either is awaited (:4468-4469). Then `b >= e` → empty, else the exact range
   (`rangeScanImpl`) on `[b, e)` with the original limits, and the `readToBegin` /
   `readThroughEnd` rules.
5. **Snapshot.** With `snapshot == false` (only the RYW-disabled route, D10) it fills
   `conflict` by porting `getRangeFinished` (NAPI:4558-4590); with `true` it computes none.
   RYW routes always pass `true` (A2.3, RYW:759, 1063).

Shared plumbing changes, none of which changes an existing caller's requests (phase 1;
request-field equality within the C `int` limit domain, S2):
- `rangeRPCOps.build` and `buildGetKeyValuesRequest` (`readpath.go:1226-1246`) take
  `KeySelector`s; byte-key callers pass `FGE(key)`.
- `sendRangeRPC` (`readpath.go:991-1007`) takes `rangeLimits` and the read version and
  calls a `transformRangeLimits(l, reverse) (limit, limitBytes int32)` port. Existing callers
  pass `minRows = 1` and `bytes != 0`, where it computes what the current code computes.
- `rangeScanImpl` (`readpath.go:700-960`), NAPI `getExactRange`, takes `rangeLimits`:
  `remaining` and `bytesLeft` become `limits.rows` and `limits.bytes` with `decrement`, and
  `softByteStop` becomes `limits.hasSatisfiedMinRows() && len(out) > 0`, getExactRange's
  own condition, equal to today's for `minRows = 1`. The fallback needs it because RYW
  requests carry `minRows > 1`.
- `sendGetKey` and `getKeyImpl` (`readpath.go:148-372`) take the read version.

**Mapped reads stay on their current implementation.** `getMappedRangeSelectors`,
`getMappedRangeSelectorExact`, `sendGetMappedSelectors` and `mappedRangeLimits`
(`mappedrange_selectors.go:17-290`) are not moved onto `nativeGetRange`. They reach
`sendRangeRPC` through the new signature with FGE selectors and
`rangeLimits{rows, 1, bytes}` built from `mappedRangeLimits.requestLimits()`, so their
requests do not change (phase 1 gate). A positive skip resolves through `getKeyForRange`
(`mappedrange_skip.go`), which acquires the transaction's cached read version per step as
before; mapped reads with a skip are not single-capture until the mapped sender is ported. Their forward continuation is
`FGE(keyAfter(last))` (`mappedrange_selectors.go:179`) where NativeAPI sends
`firstGreaterThan(last)` (NAPI:4850); that divergence predates this RFC and is recorded in
TODO.md for a follow-up that ports the mapped sender and changes those bytes on purpose.

### D5. The walk: `rywWalk`

A new file `ryw_walk.go` holds A3-A9 as Go code with the C++ line in a comment at each
step. Because the walk releases `c.mu` across storage I/O and computes its first request at
issue, it is a state machine whose suspension points are exactly C++'s `wait`s.

```go
type rywWalk struct {
	c        *rywCache
	view     *rywWrites // the owner's pinned version, or nil = cache only (A2.2); borrowed
	gen      uint64     // c.gen at issue
	maxKey   []byte
	reverse  bool
	getKey   bool       // this walk is read(GetKeyReq) (A9)
	begin, end KeySelector
	limits   rangeLimits
	result   []KeyValue
	more     bool
	additionalRows int64
	itemsPast      int32 // itemsPastEnd / itemsPastBegin
	requestCount   int   // RYW:604 / :906
	readToBegin, readThroughEnd        bool
	actualBeginOffset, actualEndOffset int32
	pc   walkPC          // pcAwaitStorage, pcAwaitPreKey, pcAwaitLoopKey, pcDone
	req  walkRequest     // pcAwaitStorage: read_begin, read_end, requestLimit
	sub  *rywWalk        // pcAwait*Key: the nested getKey (its own requestCount = 0)
	err  error
}
type walkRequest struct{ begin, end KeySelector; limits rangeLimits }

func (c *rywCache) newRangeWalkLocked(view *rywWrites, begin, end KeySelector, limits rangeLimits, reverse bool, maxKey []byte) *rywWalk
func (c *rywCache) newGetKeyWalkLocked(view *rywWrites, sel KeySelector, maxKey []byte) *rywWalk // A9; offset++ for offset <= 0

func (w *rywWalk) startLocked()                        // P1-P3, then the loop
func (w *rywWalk) resumeStorageLocked(r nativeRangeResult, err error)
func (w *rywWalk) resumePreKeyLocked(key []byte, err error)
func (w *rywWalk) resumeLoopKey(key []byte, err error)
func (w *rywWalk) run(ctx context.Context, native nativeRangeFunc) error
func (w *rywWalk) rangeResult() rywRangeResult         // A6/A7 epilogue
func (w *rywWalk) keyResult() []byte                   // A9 result mapping
type rywRangeResult struct{ kvs []KeyValue; more, readToBegin, readThroughEnd bool }
```

Resumption follows the suspension point (A6, after P3):

| Await | Resume |
|---|---|
| P3 / P3' prologue getKey | set the flags, run the early returns (RYW:623-632, 927-936), re-resolve both selectors into new cursors (:634-637, 938-940), run the loop |
| U8 / U8' storage read | if `w.gen != c.gen` fail with the read incarnation's cause; else insert (U9/U9'), re-resolve both selectors (U10/U10'), continue the loop |
| L3 / L3' loop getKey | set the flags and `end = FGE(resolved)` (forward, RYW:671-675) or `begin = FGE(resolved)` (backward, :975-979), then the epilogue. No re-resolution and no loop guard runs again. |

Cursors exist only inside one critical section: the loop gets them from the re-resolution
that precedes it, and none survives a lock release. When the loop or prologue reaches a
nested getKey, it creates `sub` on the same `view` and calls `sub.startLocked()` in the same
critical section, as the nested C++ actor runs to its own first `wait`.

`run` drives the awaits without holding `c.mu`:

```go
for w.pc != pcDone {
	switch w.pc {
	case pcAwaitStorage:
		res, err := native(ctx, w.req.begin, w.req.end, w.req.limits, w.reverse) // U8 / U8'
		c.mu.Lock(); w.resumeStorageLocked(res, err); c.mu.Unlock()
	case pcAwaitPreKey:
		err := w.sub.run(ctx, native)
		c.mu.Lock(); w.resumePreKeyLocked(w.sub.keyResult(), err); c.mu.Unlock()
	case pcAwaitLoopKey:
		err := w.sub.run(ctx, native)
		w.resumeLoopKey(w.sub.keyResult(), err) // epilogue only; touches no shared state
	}
}
```

**`requestCount`** lives in the walk and is incremented once per storage request, when the
loop builds `req` (U7/U7'), together with `additionalRows = 0`, before the await. Rows
consumed from the cache, segments skipped, nested getKeys (which count their own),
NativeAPI continuation requests inside one `native` call, and transport retries do not
change it. `countedGetRange` (`ryw.go:829-832`) is deleted.

**Cache insertion** (`resumeStorageLocked`): `knownKeyRange(res, req.begin, req.end)` or
`knownKeyRangeBack` (A8) as `rywBoundary` pairs (`ryw_getkey.go:196-231` already models
`ExtStringRef`), materialized, then `c.serverCache.insert(b, e, ascending(res.kvs))`. The
Go insert (`ryw_snapshot_cache.go:81-153`) is already `SnapshotCache::insert`, including
its empty-range early return (`:82-84`); it must accept `\xff\xff` as an end.

**Unreadable**: the loop returns `accessed_unreadable` (1036) at L7/K when `it` is
`segUnreadable` and nowhere else. `skipUncached*`, `countUncached*` and A3 treat it as a
barrier without error. This replaces `unreadableScanCapLocked` on the range path.

**Reset**: C++ kills in-flight reads through `resetPromise` (RYW:358-364). The `gen` check
keeps a reply from a reset transaction out of the new transaction's cache; today
`cacheServerReply` has this race.

### D6. getKey

`Transaction.GetKey` and `Snapshot.GetKey` (`transaction.go:982-1026`, `:610-631`) split into
issue and run like `issueRange`:

```go
func (tx *Transaction) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey
func (s *Snapshot) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey
```

Issue does the entry checks (2004 on `key > maxReadKey`, not tracked, as today), picks the
route (A2: `rywDisabled` → `tx.getKey` + clamp, unchanged; snapshot with
`snapshotRYWDisableCount > 0` → walk with `view = nil`; else walk over `c.w`), runs
`startLocked`, pins per D11, and registers in the commit barrier through
`opContextFor(parent, true)` as `issueRange` does (`range_issue.go:44, 80-82`; C++
`reading.add`, RYW:1706). Run drives the walk with `tx.nativeGetRange(…, snapshot = true, …)`,
maps the result (A9), adds the conflict through the issue version for a non-snapshot read
(`addGetKeyConflictRange`, `transaction.go:1039-1074`, now given the version), and
completes per D11. `PendingKey` mirrors `PendingRange` (`range_issue.go:258-329`): a
background goroutine started at issue, a CAS claim, a closed done channel. The synchronous
`GetKey` is `StartGetKey(...).Get()`. The facade's `Transaction.GetKey`
(`fdb/transaction.go:88-98`) calls `StartGetKey` in the call and waits in the future, so a
`Set` after `tr.GetKey(sel)` is not visible to it.

### D7. Range reads

`rywRangeRead` (`ryw.go:678-691`) becomes `{c, view, walk, snapshot}`. `issueRangeInto`
builds `begin = FGE(begin)`, `end = FGE(end)`, `limits = newRangeLimits(limit, byteTarget)`,
creates the walk over `c.w` (or `nil` for the snapshot-cache route) and runs `startLocked`.
If the walk finished, the read is local (`r.local`, `range_issue.go:78`) and its conflict is
computed in the same critical section; otherwise D11 pins. `run` drives the walk with
`tx.nativeGetRange(…, snapshot = true, …)` and returns `rangeResult()`.

Routing (`range_issue.go:68-79`) keeps its three arms. `rangePathSnapshotNoRYW` creates a
walk with `view = nil` instead of calling `localRange` and `getSnapshotRange`.
`rangePathRYWDisabled` keeps `getRangeReadThrough` until phase 4 (D10).

Conflicts (`range_issue.go:206-223`): `rangeConflictExtent` (`transaction.go:1151`) is
A2.3's `addConflictRange` for FGE selectors, where `readToBegin` and `readThroughEnd` are
always false (A3 sets them only for offsets other than 1, and the nested getKeys that set
them need a non-FGE bound). It stays, filtered through the issue version instead of
`r.ryw.conflictRanges` / `localConflicts`. Phase 4 replaces it with the general port.

**Result assembly, as a contract.** What the range path returns is defined by A6/A7, not
by a merge:

- Rows come only from walk steps K/K'. A pending write is emitted when the walk reaches
  its segment, so the writes visible are exactly the issue version's. Storage data enters
  only through the snapshot cache, so it is never mixed with writes in a batch, and
  nothing is emitted past the first UNKNOWN segment the walk stops at. This subsumes the
  old knowledge boundary (`ryw.go:860-866`).
- `more` is the epilogue's: the U1 soft byte stop, `isReached()`, and the FGE trim.
- The caller's remaining row and byte limits are the walk's `limits`, decremented per row;
  every request is sized from them (A5).
- Selector offsets move only at P1, U5/U5' (persistent begin/end), U10 and L3.

Loops and stop conditions that disappear: `run`'s no-writes fast path (`ryw.go:789-816`,
including the `replyEdgeKnown` re-walk and `limitStop`), the slow loop (`:820-914`: the
`fetchLimit` heuristic `:837-843`, `fetchOrCached`, the `localRangeInSpan` early return
`:849-853`, the merge boundary `:860-866`, the take/byte cut `:867-879`, the
`!serverMore` exit `:881-890`, the soft byte stop `:892-894`, the zero-row guard and
`keyAfter` advance `:897-907`), the unreadable reach checks (`:785-787`, `:810-812`,
`:885-887`, `:910-913`), `cachedRangeLocked`'s issue-time answer (`:726-746`), and
`getSnapshotRange`'s fetch and re-walk (`:917-940`). In `getKeyRYW`, the resolve/read loop
(`ryw_getkey.go:567-638`), its backward window (`:601-614`), forward window (`:615-625`) and
empty-window exit (`:626-634`).

### D8. What stays the same

Point reads (`get`, `getSnapshot`), mapped ranges (D4), `materializeCommit`, the conflict
filter `conflictRangesLocked` (now over a version), the snapshot cache structure, the
facade's `GetSliceWithError` and iterator continuation for byte-key ranges
(`fdb/range_result.go:166-201, 271-371`), and the RFC-098 divergence that keeps pending
writes inside SVK ranges.

### D9. Deletions

In phase 3, with `rg` output in the PR showing no caller remains: `getKeyRYW`,
`keySelResult`, Go's `resolveKeySelectorFromCache`, `errGetKeyRYWLoop`
(`ryw_getkey.go:369-640`, phase 2); `rywRangeRead.run`'s body, `cachedRangeLocked`,
`getSnapshotRange`, `replyEdgeKnown`, `rywWriteSpan`, `captureWriteSpanLocked`,
`overlappingRanges`, `swapWriteSpanLocked` (already gone in phase 2), `localRangeInSpan`,
`mergeBatchInSpan`, `rywRequestBytes`, `rywFirstRequestBytes`, `applyRangeByteLimit`,
`localRange`, `localRangeLocked`, `fetchOrCached`, `fetchServer`, `cacheServerResult`,
`cacheServerReply`, `applyLimitAndDirection`, `computeMore`, `cacheWalkBudget`,
`limitReached`, `mergeBatch`, `mergeBatchLocked` (`ryw.go:726-1475` except
`segmentValueLocked`), `unreadableScanCapLocked` (`:2187`), the cursor's legacy boundary
sources (D2), and `snapshotCache.getRangeKVsLimited`, `skipKnownEmpty`, `knownPast` if
nothing else calls them. Tests that exercise only these helpers go with them; tests that
encode a request sequence C++ does not send are rewritten to the C++-derived trace (for
example the second half of `TestRYWWriteOverlayRequestBytesClampedAndDoubled`,
`ryw_range_cut_unit_test.go:253-300`: under `clearRange(b, e)` C++ sends `[a, b)` then
`[e, z)` with 30000 then 60000 bytes, A4's multi-key barrier, not three requests over
`[a, z)`).

### D10. Selector ranges, RYW-disabled routing, facade limits (phase 4)

Today the facade resolves a selector range with two `GetKey` calls and reads the resolved
byte range (`fdb/range_result.go:81-95, 414-466`). That sends different requests, adds two
getKey conflicts instead of one range conflict, and never reaches P3, L3, U3's
`end.offset < 1` branch or A5's `bytes == 0` branch. Phase 4:

- Client API: `StartGetRangeSelectors(ctx, begin, end KeySelector, limit, byteTarget int,
  reverse bool)` and the snapshot form; the byte-key API wraps FGE selectors. Entry checks
  in RYW:1736-1764 order: cancel/deferred error, 2004 on either key `> maxReadKey`, empty on
  `isReached()`, 2012 on `!isValid()`, `removeOrEqual`, empty when inverted.
- RYW and snapshot-cache routes: the walk with both selectors as given.
- RYW-disabled route: A2.1's `readThrough` (RYW:186-213) with the **caller's snapshot
  flag**. A backward read with `end.offset > 1` first resolves `end` with NativeAPI getKey
  under the same flag; a non-snapshot pre-read adds its getKey conflict
  (`getKeyAndConflictRange`, NAPI:5761-5787), a snapshot one adds none. Then
  `nativeGetRange(…, snapshot, …)`, which adds the `getRangeFinished` conflict only when
  `snapshot` is false (NAPI:4558-4590, 5841-5844); then the forward `maxReadKey` clip
  (RYW:203-211). This retires `getRangeReadThrough` for range reads.
- Conflicts on the RYW route: `addConflictRange(GetRangeReq<false/true>)` (RYW:245-320)
  with the walk's `readToBegin`/`readThroughEnd`, filtered through the issue version.
- Facade: pass selectors through; drop `resolveRange`/`resolveSelector`; keep
  `rangecheck.Check` on the raw keys (`fdb/range_result.go:81-88`). Unlimited rows go to the
  client as 0 (→ -1), not `MaxInt32` (`fdb/range_result.go:137-142`): A5 manufactures rows
  only when `!hasRowLimit()`, so `MaxInt32` would send 80000 where C++ sends
  `max(1 + additionalRows, offset)` (T5). The facade keeps its own `MaxInt32` arithmetic for
  `remaining`. Continuations keep the other bound's original selector:
  `FirstGreaterThan(last)` as begin forward, `FirstGreaterOrEqual(last)` as end reverse.

### D11. Pin ownership

Exactly one object owns a pin: the read (`rangeRead`, `range_issue.go:14-31`, or the new
`keyRead` behind `PendingKey`). Everything else borrows.

- **Acquire.** In issue, under `c.mu`, after `startLocked`: if the walk is not done,
  `r.view = c.pinWritesLocked()`; the walk already references the same `c.w`, and no
  mutation can intervene because the lock is held from walk creation to pin. A read that
  finished at issue, a `finished` read (`range_issue.go:144-148`), the snapshot-cache route
  and the RYW-disabled route never pin.
- **Borrow.** The walk and every nested `sub` walk hold the pointer without pinning; their
  lifetime is inside the owner's `run`.
- **Release.** Once, in the owner's `complete()` (`range_issue.go:226-229`), which `run`
  defers (`:156`) outside the panic backstop (`:159-161`) and which runs exactly once because
  `run` does: `PendingRange`/`PendingKey` claim it by CAS (`:279-282`, `:321-323`) and the
  synchronous API calls `run` once. Order inside `complete`: the read's conflicts were added
  in `read` (`:198-200`); then `c.unpinWritesLocked(r.view)`, `r.view = nil`; then
  `leaveIssuedRead` (commit barrier); then the context release. A commit therefore never
  waits on a pin, and it never builds its request before the read's conflict exists.
- **Abandoned futures.** `startRange` (`:307-326`) and `StartGetKey` start the background
  goroutine at issue, and the read progresses whether or not `Get` is called; the facade's
  `Future.Cancel` completes the future without stopping the operation
  (`fdb/future.go:49-53`). Starting the goroutine does not guarantee the read finishes: with
  a background context and no transaction timeout it can wait indefinitely. The pin follows
  the operation's lifetime and is released when the operation completes, fails or is
  cancelled; it never outlives the read.
- **Cancellation and errors**, including during a nested getKey: the native call or `sub.run`
  returns the error, the walk ends, `run` returns, `complete` releases. A sub walk has
  nothing to release.
- **Reset.** `reset` replaces `c.w` and increments `c.gen`. The owner releases the version
  it pinned (its stored pointer), which is no longer live; the old version is garbage once
  its pins reach zero. A late reply is dropped by the `gen` check (D5).
- **Writers.** `mutableWritesLocked` reads `c.w.pins` under `c.mu`; `unpinWritesLocked` runs
  under `c.mu`; they are serialized.

## Phases

Each phase is independently mergeable. "Golden-safe" means `go.golden` changes only in
sections that thereby equal `libfdbc.golden`, and `libfdbc.golden`'s existing sections are
byte-identical (checked by `diff` of the old sections, recorded in the PR).

1. **Storage plumbing, no wire change.** D3's `rangeLimits` and selector helpers; D4's shared
   plumbing (selector builder, `transformRangeLimits`, `rangeScanImpl` on `rangeLimits`,
   the operation's read version threaded through `sendRangeRPC`, `sendGetKey` and
   `getKeyImpl`). Nothing reads through a new path yet. Gate: S2's request-transcript gate
   (plain, mapped, fallback, multishard) and the existing suites named there; `go.golden`
   unchanged.
2. **Versioned write map, nodes, walk, getKey.** D1, D1b, D2, D5, D6, D11, and
   `nativeGetRange` with `resolveKey` (D4). The old range loop keeps its private span copies
   (now passed as parameters, D1) and the legacy cursor boundaries (D2), so its requests do
   not change. Deletes `getKeyRYW`, Go's resolver and `swapWriteSpanLocked`. Closes the six
   `getkey_*` divergences; `go-divergences.diff` keeps only `set_then_range`. Reports the D1
   benchmark and the D1b build benchmark.
3. **Range reads.** D7, D9. Closes `set_then_range`; `go-divergences.diff` is empty.
4. **Selector ranges and RYW-disabled routing.** D10, with new lab scenarios (S3).

## Executable spec

### S1. Deterministic walk traces (hermetic, fast lane)

`ryw_walk_trace_test.go` drives the walk with a scripted `nativeRangeFunc`. Each case
fixes: the snapshot cache, the issue version of the write map (built through the public
mutators, so nodes come from D1b), the caller's selectors and limits, and the reply to each
request. It asserts, per request in order, `read_begin` and `read_end` (key, orEqual,
offset) and `requestLimit` (`rows`, `minRows`, `bytes`), and the wire `(limit, limitBytes)`
from `transformRangeLimits`; then the rows, `more`, `readToBegin`, `readThroughEnd`, the
cache entries after each insert, and, for non-snapshot reads, the filtered conflict ranges.
Expected values are derived by hand from the cited C++ lines and the derivation is a comment
in the case. A case reachable through the public API also has a wire-golden twin (S3), which
checks the derivation against libfdb_c.

Example derivations (keys unprefixed; unless a case says otherwise, storage is
`{a, b, c, d, e}`, the mode is EXACT, so bytes are -1, and writes add conflicts):

- **T1, first request crosses writes and clears, counts none.** Writes `clear(b)`,
  `clear(c)`, `set(c1)`; `[a, z)` rows 2. Request 1: `skipUncached` crosses
  `["", b)` U, `[b, b\0)` E (1), `[b\0, c)` U, `[c, c\0)` E (2), `[c\0, c1)` U, `[c1, c1\0)` KV
  and stops at `itEnd = [c1\0, \xff\xff)`; `min(2, 0) = 0`. `[FGE(a), FGE(z))`, rows 2,
  minRows 1. Reply `a, b`, more → cache `[a, b\0)`. The walk emits `a`, passes `b` (cleared),
  stops at `[b\0, c)` U. Request 2: `clearLimit = 2`, crossed single empties: `[c, c\0)` = 1;
  `begin` moved to `FGE(b\0)` (U5); `additionalRows = 1`; rows `max(1 + 1, 1) = 2`, minRows 2.
  Reply `c, d` → `begin` resolves over `[b\0, c)` E and `[c, c\0)` E (cleared) and
  `[c\0, c1)` E to `c1`; K emits `c1`; L5 breaks. Result `[a, c1]`, more = true.
- **T2, multi-key barrier and remaining rows.** `clearRange(b, c)`; storage `{a, b, b5, c, d}`;
  `[a, z)` rows 5. Request 1 stops at the barrier: `[FGE(a), FGE(b))` rows 5. Reply `a`, !more
  → cache `[a, b)`. Request 2: `[FGE(c), FGE(z))`, rows 4 (the caller's limit after `a`).
  Reply `c, d`, !more. L6 breaks at `itEnd` (`end.key == it.beginKey()`); more = false.
  Result `[a, c, d]`.
- **T3, unreadable barrier.** `SetVersionstampedValue(m)`; storage `{a, b, n}`; `[a, z)`.
  Rows 2: request `[FGE(a), FGE(m))`, rows 2; result `[a, b]`, more, no error. Rows 3: same
  request; reply `a, b` !more; the walk reaches `[m, m\0)` at L7: error 1036 after one request.
- **T4, reverse with clears.** `clear(x)`, `clear(y)`; storage `{v, w, x, y}`; reverse `[a, z)`
  rows 2. Request 1: `skipUncachedBack` crosses both clears, counts `min(2, 0) = 0`:
  `[FGE(a), FGE(z))`, rows 2 → limit -2. Reply `y, x`, more → known `[x, z)`. The walk descends
  through cleared `y`, `x` to `["", x)` U; U5' sets `end = FGE(x)`. Request 2:
  `[FGE(a), FGE(x))`, rows 2. Reply `w, v` → result `[w, v]`, more.
- **T5, bytes == 0 and minRows.** Writes `clear(b)`; storage `{a, b, c}`; forward
  `[FGE(a), lastLessThan(m))` (end offset 0), rows unlimited, bytes 1. Request 1:
  `read_end = FGE(m)`, `additionalRows = 1`: rows -1, minRows 2, bytes 1 → wire
  `limit 80000, limitBytes 1`. Inside `nativeGetRange` the reply is one row; `bytes` hits 0
  with `minRows = 1`, so the continuation is `[FGT(a), FGE(m))` with
  `limit = minRows = 1, limitBytes = 80000` (A10). The reply `a, b` is cached as
  `[a, b\0)`. The walk consumes `a` (bytes 0, minRows 0: reached), passes cleared `b`, and at
  `[b\0, \xff\xff)` U, which is `itEnd`, still needs one item past the end (L5, L6 and U1
  fail): request 2 is `[FGE(b\0), FGE(m))` through A5's `bytes == 0` branch: bytes -1,
  rows `max(max(1, -1) + 1, 1) = 2`, minRows 2 → wire `limit 2, limitBytes 80000`. With
  caller rows `MaxInt32` instead of unlimited, request 2 sends `limit 80000`: the D10
  reason.
- **T6, conflict-only nodes exhaust the traversal budget.** No pending mutations;
  `AddWriteConflictRange([c_i, c_i\0))` for `i = 00..59`; `[a, z)` rows 5. The write map has
  empty-stack nodes at every `c_i` and `c_i\0` (A1), so the cache-empty space splits into
  `seg0 = ["", c00)`, `seg(2i+1) = [c_i, c_i\0)`, `seg(2i+2) = [c_i\0, c_(i+1))`, all UNKNOWN.
  `skipUncached` with `clearLimit + 100 = 100` runs its body 100 times, from `seg0` to
  `seg100 = [c49\0, c50)`, then stops; `seg100` is unknown, so `ucEnd = seg100 != itEnd` and
  request 1 is `[FGE(a), FGE(c50))`, rows 5. Without the nodes it would be `[FGE(a), FGE(z))`.
- **T6b, conflict flags decide whether a range adds nodes.** As T6 with 100 ranges
  `i = 00..99`, after first adding `AddWriteConflictRange(c00, c50)`. The enclosed ranges
  `i < 50` add no node: their begin is inside a conflict range (WriteMap.cpp:253) and the
  segment at their end is conflicted (:278). `c50` keeps a node (the big range's end node,
  now flagged). Segments: `seg0 = ["", c00)`, `seg1 = [c00, c50)`, then for `i >= 50`
  `[c_i, c_i\0)` and `[c_i\0, c_(i+1))`, so `seg100 = [c99, c99\0)` and `itEnd = [c99\0, \xff\xff)`.
  The budget runs out on `seg100`: request 1 is `[FGE(a), FGE(c99\0))`. A boundary at every
  conflict-range edge would give `[FGE(a), FGE(c50))`; no conflict nodes would give
  `[FGE(a), FGE(z))`.
- **T7, clear order decides the counted clears.** Storage `{a, b, d, f}`; writes
  `clearRange(c, c5)`, then `clear(e)`, then `clear(e\0)`; `[a, z)` rows 5. The second
  single clear finds `[e\0, …)` unmodified, so it keeps a node at `e\0`
  (WriteMap.cpp:168-196): `[e, e\0)` and `[e\0, e\0\0)` are separate EMPTY segments.
  Request 1 stops at the `[c, c5)` barrier: `[FGE(a), FGE(c))`, rows 5. Reply `a, b`, !more;
  the walk emits both and stops at `[c5, e)` U. Request 2 (`clearLimit = 2`): `skipUncached`
  counts `[e, e\0)` as a single empty, then finds `[e\0, e\0\0)` extends the run past one key,
  backs up to `[c5, e)` and returns 1. `read_end = FGE(e)`, `additionalRows = 1`:
  `[FGE(c5), FGE(e))`, rows `max(3 + 1, 1) = 4`, minRows 2. With the two single clears in
  the opposite order, `end_coalesce_clear` (:180-191) removes the node, the run is the one
  segment `[e, e\0\0)`, the count is 0, and request 2 has rows 3, minRows 1. With the opposite
  order and `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE` on the second clear, `clearNoConflict`'s
  `end_coalesce_clear` requires equal conflict flags (:565-566), the node stays, and request 2
  has rows 4 again. Go today merges all three into `[e, e\0\0)`.

The full case list, each forward and reverse unless noted:

1. The seven golden scenarios as traces (A11).
2. Traversal: T1; a pending atomic over an unknown base (UNKNOWN, crossed); a matched
   CompareAndClear phantom (crossed by `skipUncached`, counted by A3's offset walk, skipped by
   K); a dependent write over a cached base (KV).
3. Single-key empties that are not pending clears: a cached absent point read; a one-key
   gap between cached keys; counted from request 2 on.
4. Barriers: T2 (pending multi-key clear and a cached multi-key empty); T3 with an SVK range
   and with pending writes kept inside it (RFC-098), checking the barrier's edge; T6;
   101 alternating cached keys and unknown gaps stopping request 1 by budget.
5. Write-map structure (D1b): T6b; T7's three variants; `set(k)` then `clear(k)` inside a
   cleared range (empty-stack node at `k`); `AddWriteConflictRange` across an existing clear
   and across operation nodes (interior flags only, no new boundary); writes without
   conflicts (`NEXT_WRITE_NO_WRITE_CONFLICT_RANGE` and Go's `writeConflictsDisabled`)
   followed by conflict ranges over them; a conflict range splitting a cached KV run, which
   ends `skipContiguous` (K's batch) and so changes `itemsPastEnd` for an end offset below 1
   (phase 4). Plus the D1b property test and a lazy-build test: the same history built
   eagerly and by log replay gives identical nodes.
6. getKey: offsets +2, FGT, `lastLessThan`, `lastLessOrEqual`, -2; a selector landing in the
   cache with no request; offsets past the data (`readThroughEnd` → `maxReadKey`,
   `readToBegin` → `""`); a backward selector whose `offset++` lands at offset 1 on a known KV;
   `maxReadKey = \xff\xff` (end selector on the sentinel); T6 for getKey (`FGT(a)` with 60
   conflict ranges: request `[FGE(a\0), FGE(c50))`, rows 1).
7. Selector offsets on both bounds: begin offset > 1 and < 1 (U5 third branch,
   `countUncachedBack`); end offset > 1 (`countUncached`, `read_end.offset += n`) and < 1;
   the U6/U6' crossed-key clamp; the P3 nested getKey (crossed selectors, returns empty after
   the nested request only); the L3 nested getKey after rows were emitted, asserting that no
   re-resolution or loop guard runs after it (a cache insert by another read between the
   nested request and the resume must not change the result); reverse P3 and L3'.
8. Bytes: unlimited stays -1 on every request; positive bytes doubling only when a request
   consumed no rows (all fetched rows locally cleared); T5; U1 soft stop with rows from the
   cache and zero requests.
9. Saturation and counters: `begin.offset = MaxInt32` (`minRows` and `rows` clamp to
   `MaxInt32`); `requestCount` reaching 21 in one call with bytes 1 and every fetched row
   locally cleared (request bytes 1, 2, 4, …, 65536, then 80000; the walk's recorded
   `clearLimit` is `1 << 20` at `requestCount` 20 and 21); pure-function pins for
   `setRequestLimits` and `clearLimit` at `requestCount` 0, 1, 19, 20, 21, 64.
10. Caches: partial cache without writes (byte-limited: cached rows, more, zero requests;
    EXACT: request for the rest with the remaining rows); a warm second call with zero
    requests; an empty reply that caches a known-empty range; `readThrough` extending the
    known range and the next request starting there; native `readToBegin` and
    `readThroughEnd` reaching A8; a reverse reply inserted ascending; a reply to a selector
    with offset > 1 cached past `read_end.key`.
11. Interleaving: another read's cache insert between this walk's requests (through the
    scripted native's hook) moves the next request; cache-served segments do not change
    `requestCount`.
12. Routing: snapshot with snapshot RYW enabled (sees writes, no conflicts); snapshot with
    it disabled (`view = nil`: `getkey_after_set` returns the storage key `c`, not `b1`);
    RYW disabled (requests carry the caller's selectors and limits; `maxReadKey` clip;
    backward `end.offset > 1` getKey), snapshot and not.
13. Issue-time isolation: for `StartGetRange` and `StartGetKey`, every later mutation kind
    (`set`, `clear`, `clearRange`, `atomic`, versionstamp, `setBypassUnreadable`,
    `AddWriteConflictRange`) between issue and completion leaves the request sequence, rows,
    `more` and conflicts equal to the run without it; for `AddWriteConflictRange`, 60 later
    conflict ranges inside the second request's stretch do not split it (T6 shape). A second
    read issued before the first's reply sends its own request (first requests are computed
    at issue).
14. Read version (through the client and the simulated transport): a fresh transaction whose
    first read is answered locally (`Set(k)` then `GetKey(FGE(k))`; a range fully covered by
    pending writes and clears) sends no `GetReadVersionRequest`; the first read that needs
    storage sends exactly one, and every request of that call carries its version; with
    `SetReadVersion(v)` below `minAcceptableReadVersion`, a read that needs storage fails with
    `transaction_too_old` (1007) and sends no `GetKeyValuesRequest`, and a locally answered
    read with the same version succeeds and sends nothing.

Each new branch has a mutation proof recorded in its PR: the mutation (for example,
dropping the `min(…, clearLimit)` clamp, keeping `read_end = end` when `end.offset < 1`,
not moving the persistent begin, adding `hasSatisfiedMinRows()` to U1', incrementing
`requestCount` after the await, re-resolving after L3, ignoring conflict-only nodes,
coalescing adjacent clears), evidence it compiled, and the case that failed.

### S2. Native getRange and the phase 1 gate

**Request-transcript gate (phase 1).** The phase-1 PR's first commit adds
`request_transcript_test.go` and a test-only copy of the pre-change `sendRangeRPC`,
`buildGetKeyValuesRequest`, `rangeScanImpl`, getKey and mapped senders
(`request_transcript_legacy_test.go`). The test runs each case twice on the same container
and shard layout, once through the copy and once through the changed code, and compares:

- **Requests**, recorded at the client's send path (`wire_recorder_test.go`): every frame
  the read sends, in order, of every kind a read may send (storage reads, location
  lookups, `GetReadVersion`). Requests are decoded and compared field by field without the
  reply token, span context and debug ID, with versions numbered in order of appearance
  and body lengths kept. This is request-field equality, not literal byte identity. The
  recorder fails closed: an undecodable frame, a request kind it does not know, a
  malformed length and a trailing partial frame are each a gate failure. A body of a known
  kind must re-encode to the bytes sent, since the generated decoder reads an
  out-of-bounds field as absent rather than failing. Its only
  exclusions are named: frames to the ping token and `VoidReply` bodies.
- **Results**, in full: every row's key and value, `more`, `readThrough`, a getKey's key
  and fallback flag, and every mapped row with its secondary rows.
- **Faults**: an injected `wrong_shard_server` or reply timeout is delivered to the reply
  token of a recorded storage request, never to a location or read-version reply, and the
  case fails unless exactly one injection happened in each arm.

Every case runs with the read version preset and with a fresh transaction, so each arm's
own `GetReadVersion` is in the compared transcript. Equivalence is claimed only within the
C `int` domain. `newRangeLimits` clamps a Go limit above `MaxInt32`: the first request is
unchanged (its wire limits are capped at `REPLY_BYTE_LIMIT`), but a scan whose total passes
`MaxInt32` rows or bytes now stops where the old Go-int budget continued, so such totals are
excluded. Cases: plain range reads forward and reverse, row
limits 1/5/unlimited, byte targets unlimited/256/4096/80000, on `multiShardEnv`
(`multishard_test.go`, both shard-size regimes) and on one shard; `wrong_shard_server` and
reply-timeout injection through the proxy transport, which drives the exact-range
relocation; selector getKey across shards; mapped ranges through
`getMappedRangeSelectors`, positive mapped skips through
`getMappedRangeWithByteTargetAndSkip` and the `getMappedRangeSelectorExact` fallback after
an injected `wrong_shard_server`, and the `rangeScanImpl` mapped path; RYW-disabled
ranges. Negative controls on the single-shard cluster prove the gate fails on an extra
`GetValue`, an extra `GetReadVersion`, an undecodable frame, a known kind with a malformed
field, an unexpected request kind, a malformed length and a trailing partial frame; they
feed a socketless parser, so no real write interleaves with them. The copy stays until phase 4 changes the
RYW-disabled route. The existing suites run too: the wire-golden test, `mappedrange_e2e_test`,
`mapped_skip_regression_test`, `libfdbc/mappedrange_multishard_differential_test`,
`range_shard_pair_differential_test`, `range_division_differential_test`,
`resolve_fallback_test`, `multishard_test`.

**`nativeGetRange` (phase 2).** Against `multiShardEnv` and the simulated transport:
per-shard selector rewriting forward and reverse; `readThrough` on a modified final shard;
FGT continuation on the wire (`OrEqual=true`); the `bytes == 0` minRows continuation; one
read version per call; the empty-reply fallback with both flags. Fallback scheduling, with
replies held by the simulated transport: two non-trivial selectors with cached locations
send `GetKeyRequest(begin)` then `GetKeyRequest(end)` before either reply is released; an
uncached begin location sends its location request and then end's first request before
begin's location reply is released; FGE or FGT bounds send no `GetKeyRequest` (one
non-trivial bound sends exactly one); then the exact-range requests.

### S3. Wire goldens and differentials

- Red before, green after: phase 2 turns the six `getkey_*` sections of `go.golden` into
  `libfdbc.golden`'s; phase 3 does `set_then_range`; `go-divergences.diff` is empty after
  phase 3. `libfdbc.golden` is never regenerated to remove a diff.
- New scenarios, each the twin of an S1 case. Phase 2: `getkey_across_clears` (T1-shaped),
  `getkey_across_write_conflict_ranges` (T6), `snapshot_ryw_disabled_getkey`. Phase 3:
  `range_across_single_clears` (T1), `range_clear_range_barrier` (T2),
  `range_reverse_across_clears` (T4), `range_across_write_conflict_ranges` (T6),
  `range_enclosed_write_conflict_ranges` (T6b), `range_adjacent_clears` and `range_adjacent_clears_reversed` (T7),
  `range_adjacent_clears_no_conflict` (T7 with `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE`),
  `range_partial_cache_exact`, `range_unreadable_stop` (T3, rows 2), `range_unlimited_large`
  (>80000 bytes in one call, FGT continuation), `range_modes_with_writes` (ITERATOR
  iterations 1-3, WANT_ALL, EXACT, SMALL, SERIAL, reverse). Phase 4:
  `ryw_disabled_range_selectors` (snapshot and not), `range_selector_offsets`,
  `range_crossed_selectors`, `range_end_last_less_than` (T5). Their `libfdbc.golden` sections
  come from a lab `-update` run (`tools/clientbench/scripts/wiregolden.sh`), which the owner
  runs; existing sections must stay byte-identical in that run.
- Result differentials (`pkg/fdbgo/bench`, libfdb_c in process): for every new scenario,
  rows, `more` and the commit's read-conflict ranges equal libfdb_c's, forward and reverse,
  snapshot and not. Phase 4 adds RYW disabled with `Snapshot()` ranges, forward and backward
  with `end.offset > 1`: the commit carries no read conflict from them, while the
  non-snapshot run carries the range conflict and the pre-read's getKey conflict.
- Retained after the diff is empty: all S1 traces, S2, S4, the new golden sections, and the
  existing `ryw_issue_view_*`, RYW adversarial, fuzz and differential suites.

### S4. Pin lifecycle and version immutability (run with `-race`)

A test seam counts pin and unpin per version; a deep, deterministic dump (map entries with
atomic stacks and their bytes, slices, flags, and the nodes after a lazy build, so the
representation of pending node ops does not affect it) fingerprints a version.

1. Two reads issued back to back with no write between share `c.w` (`pins == 2`); a `Set`
   then clones once; both reads finish with their issue results; pins return to 0.
2. A pinned version holding an atomic stack; a writer folds a same-type atomic
   (`coalesceOverAtomics` rewriting the top); the pinned fingerprint is unchanged.
3. Phase 2 bridge: the old range loop resolves atomics in `mergeBatchLocked` on its span
   while a getKey walk pins the live version: the pinned fingerprint is unchanged, the live
   version is unchanged, and passing `c.w` or a pinned version to `mergeBatchLocked` fails.
4. Reset while a walk awaits storage, then the reply arrives: nothing is inserted into the
   new cache, the read fails with the incarnation's cause, the old version's pin is released
   once. `Transaction.Reset` cancels the read's incarnation and waits for the read, so the
   `gen` check is reached only when the cache resets between a request and its reply; the
   test drives that directly, and `Transaction.Reset` separately.
5. Context cancellation while a nested getKey awaits storage (S1 case 7 shape): the read
   fails, the parent's pin is released once, the sub walk never pinned.
6. Stress: goroutines issue `StartGetRange`/`StartGetKey`, call or skip `Get`, cancel
   futures, and write concurrently; after quiescence every observed version has
   `pins == 0`, each pin was released exactly once, and each version's fingerprint at
   release equals its fingerprint at pin.
7. Panics: an `afterRangeReply` hook that panics; the read fails through the backstop and
   the pin is released once.
8. Every mutator clones a pinned version (D1's enumeration test).
9. Concurrent and repeated `Get` on one issued read (several goroutines, then again after
   completion): the operation runs once, every caller gets the same result, and the pin is
   released once.

Lifecycle tests run with explicit deadlines and join every worker before checking pins;
deterministic cases come first, and the stress case supplements them.

Validation per phase: `just test` (fast lane, including the wire-golden test, S1, S2's
hermetic parts and S4); `just test-full` at the phase boundary for the multishard,
conformance and differential targets.

## Compatibility impact

Requests under RYW become libfdb_c's: getKey reads ask for 1-3 rows instead of 256, RYW
range reads ask for the caller's rows plus counted clears instead of 10000 or
`2 * remaining`, reads cross pending writes, request ends respect the write map's structural
boundaries, forward multi-request reads continue with `firstGreaterThan(last)`, and the first
request of a byte-limited call uses the caller's target. Results and conflicts do not change
for byte-key reads (S3 differentials check it). Mapped reads do not change (D4). Phase 4
changes selector-range conflicts to C++'s single range conflict instead of two getKey
conflicts plus a range conflict, which matches libfdb_c.

## Open items

- **Write-map clone cost.** D1 trades per-read span copies for per-write clones under
  concurrent reads; phase 2 measures it. A persistent map is the remedy if needed.
- **Mapped forward continuation** is `FGE(keyAfter(last))`, not `firstGreaterThan(last)`
  (D4); recorded in TODO.md, outside this RFC.
- **Multi-shard requests** change from `getExactRange`-shaped to `getRange`-shaped. The lab
  cluster's scenario prefix is one shard, so no golden covers this; S2 and the multishard
  differentials do.
- **Fallback order with uncached locations.** Go's `parked` signal reproduces flow's
  run-to-first-wait order; when both locations are uncached, which location request reaches
  the wire first is decided by the transport in both clients. S2 asserts population and
  overlap there, not a total order.
- **Forward/backward U1 asymmetry** is unobservable through the C API (A7); only S1 pins it.
- **Segment partition.** The cache side is checked against Go's materializer (RFC-057), the
  write side by D1b's port; libfdb_c exposes neither, so the S1 traces and their S3 twins
  (T6, T7 included) are the check against libfdb_c.
- **BUGGIFY** (`skipUncached` budget 0) is simulation-only and not ported.

## Review resolution index

Revision 3 (review of 8ba8c70c5):

| Review point | Resolved in |
|---|---|
| Torvalds 1: phase 1 shares pinned versions while `mergeBatchLocked` still mutates; swaps install historical versions | D1 (no swaps, explicit version parameters; `mergeBatchLocked` only on private spans with an assertion), Phases 1-2, S4.3 |
| Torvalds 2: pin ownership for abandoned futures, cancellation, panic, repeated Get, nested walks, commit barrier | D11, S4.1-S4.8 |
| Torvalds 3: fallback getKeys must both be in flight | A10, D4 step 4, S2 fallback scheduling |
| Torvalds: phase 1 "no wire change" proven beyond the goldens | Phases 1, S2 request-transcript gate |
| C++ 1: conflict-only and other structural WriteMap boundaries | A1 write-map structure (all five mutators audited), D1b, D2, T6, T6b, T7, S1 cases 4-5 and 13, S3 twins |
| C++ 2: `resolveKey` (FGE, FGT immediate; getKey otherwise; both started) | A10, D4 step 4, S2 |
| C++ 3: mapped FGT continuation vs FGE-only sender | D4 (mapped left on its implementation; divergence recorded), Open items |
| C++ 4: RYW-disabled must keep the caller's snapshot flag | A10, D3 `conflict`, D4 step 5, D10, S1 case 12, S3 phase 4 differential |
| C++ 5: `startTransaction` and `validateVersion` before lookup; owner and captured version; no GRV for local answers | A10, D4 step 2, Phases 1, S1 case 14 |
| C++ 6: resume by suspension point; L3 breaks to the epilogue | A6 (after P3), D5 resumption table, S1 case 7 |

Revision 2 (review of b73752b7b):

| Review point | Resolved in |
|---|---|
| C++ 1: traversal vs counting; crossing KVs; non-clear empties; backward | A4, A7 (U2'), S1 cases 2-4 |
| C++ 2: selector transformations; `end.offset < 1` → FGE; `offset > 1` + counted clears; additionalRows; crossed-key clamp; persistent begin | A6 U3-U6, A7 U3'-U6', D5, S1 case 7 |
| C++ 3: `bytes == 0` vs -1; minRows; saturation; shift on remaining limit; requestCount uncapped | A1, A5, A10, D3, S1 T5 and cases 8-9 |
| C++ 4: backward getKey `offset++`; reverse boundaries; `setRequestLimits(…, read_end.offset, …)` | A7 U7', A9, A11, S1 case 6 |
| C++ 5: internal reads are snapshot; conflicts in the wrapper; requestCount/additionalRows before the await | A2.3, A6 U7-U8, D5 |
| Design contract: full selectors and limits; native conversion separate; byte-key callbacks replaced | D3, D4, D5 |
| Routing: snapshot RYW on/off, RYW disabled, partially cached reads without writes | A2, D6, D7, D10, S1 cases 10 and 12 |
| Streaming iteration vs requestCount; C binding limits | A2, D10, S3 `range_modes_with_writes` |
| Result assembly and cache contracts; readThrough/readToBegin/readThroughEnd; reverse inserts ascending; re-resolution | A6-A8, D5, D7 contract, S1 case 10 |
| Torvalds 1: return type loses minRows; selector-capable dispatch | D3, D4 |
| Torvalds 2: omitted branches; three distinct limits | A4, A5 |
| Torvalds 3: write view, unreadable, cache coverage, re-resolution, remaining limits, more; which stop conditions disappear | D1, D5, D7 |
| Torvalds 4: where requestCount lives; cache hits and retries | D5 |
| Tests: traces, unchanged libfdb_c goldens, retained regressions | S1-S3 |
