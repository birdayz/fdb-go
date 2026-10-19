# RFC-262: Port RYW getRangeValue, the storage-read loop under read-your-writes

**Status:** Draft (revision 2)
**Item:** TODO.md, "Wire divergences found by the wire goldens": `getkey_*`, `set_then_range`.
**Spec:** FoundationDB 7.3.77. `fdbclient/ReadYourWrites.actor.cpp` (RYW below),
`fdbclient/NativeAPI.actor.cpp` (NAPI), `fdbclient/include/fdbclient/FDBTypes.h`,
`fdbclient/include/fdbclient/SnapshotCache.h`, `fdbclient/include/fdbclient/WriteMap.h`,
`bindings/c/fdb_c.cpp`. Line numbers are from the checkout at
`~/.local/share/fdb-perf/fdbsrc`. Go line numbers are from commit b73752b7b.

Revision 1 proposed extracting a request-sizing function and keeping the Go loops. Both
reviewers rejected it, and they were right: the Go loops are not ports of
`getRangeValue`, so no sizing function can be dropped into them. This revision replaces
them. The review-resolution index at the end maps every review point to a section.

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
`GetRangeLimits`. A faithful port therefore needs four things the Go code does not
have: the walk itself, a selector-capable NativeAPI `getRange`, a write-map version
that outlives the issuing call, and an iterator over the same segment universe C++
uses. This RFC designs all four and deletes the three loops.

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
  false; else if `resolved == ""`: empty with flags. Re-resolve both selectors.

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
- `getRangeFallback` (:4457-4500) resolves both selectors with NativeAPI `getKey` (both
  started before either is awaited), returns empty if `b >= e`, reads
  `getExactRange([b, e), limits)` and sets `readToBegin` when `b == ""` (and `!more` for
  reverse), `readThroughEnd` when `e == \xff\xff` (and `!more` for forward).
- With `snapshot = false` (only the RYW-disabled path), `getRangeFinished` computes the
  conflict range from the selectors and result (:4534-4590).

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
| Unreadable throws at L7 / K | `unreadableScanCapLocked` reach heuristics (`ryw.go:718`, `:785-787`, `:810`, `:885`, `:910`) | approximates the throw point |
| `minRows` continuation, FGT continuation (A10) | `rangeScanImpl` is `getExactRange`-shaped (`readpath.go:700-960`) | no `minRows`; continues with `FGE(keyAfter(last))` |

The slow path is replaced wholesale. Patching sizing into the existing loops would keep
their merge semantics, their stop points and their byte-key storage interface, all of
which differ from A6.

## Design

### D1. A versioned write map (replaces span capture)

The write-map fields of `rywCache` (`ryw.go:24-54`: `writes`, `sortedKeys`, `cleared`,
`unreadableRanges`, `unreadableKeys`, `bypassUnreadable`) move into a struct reached by
pointer:

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
	pins             int // reads holding this version; guarded by rywCache.mu
}

func (c *rywCache) pinWritesLocked() *rywWrites      // c.w.pins++; return c.w
func (c *rywCache) unpinWrites(w *rywWrites)          // locks; w.pins--
func (c *rywCache) mutableWritesLocked() *rywWrites  // if c.w.pins > 0 { c.w = c.w.clone() }; return c.w
```

Every function that assigns a write-map field calls `mutableWritesLocked` first: `set`,
`clear`, `clearRange`, `atomic`, `addClearedRangeLocked`, `addUnreadableRangeLocked`,
the unreadable-key index updates and `setBypassUnreadable` (the assignment sites are
`ryw.go:238, 252, 266, 284-291, 535, 547, 570, 1633-1683, 2078, 2226, 2236, 2252`).
`reset` (`:171-185`) installs a fresh `rywWrites`. `clone` copies the map (entries by
value, atomic stacks re-sliced as `captureWriteSpanLocked` does today, `ryw.go:983-985`,
because `coalesceOverAtomics` rewrites `stack[top]` in place, `:1752-1753`), and the four
slices. Key, value and parameter bytes are shared; they are never written after insertion
(the slab `allocBytes` only appends, `ryw.go:142-155`; range bounds are replaced, not
edited, `:1661-1683`). `ensureSortedLocked` may build `sortedKeys` on a pinned version;
it is derived data and runs under `c.mu`.

`mergeBatchLocked` writes resolved atomics into the map (`ryw.go:1397, 1410`); it is
deleted (D9), and after that no read mutates a write-map version, as in C++.

Cost: a synchronous read pins and unpins within one call, so a writer clones only when it
runs while another goroutine's read is in flight, or between `StartGetRange` /
`StartGetKey` and their completion. That clone is O(writes). The current design copies
the writes in `[begin, end)` on every fetching read that has any. Phase 1 adds a
benchmark for the bad case (alternating `StartGetRange` and `Set` with 10k pending writes)
and reports it against the merge-base; if it regresses the record-layer benchmarks, a
persistent map (C++'s PTree) is the follow-up, not a different semantics.

### D2. The cursor over a view and C++'s universe

`rywSegCursor` (`ryw_getkey.go:146-192`) and its helpers (`segTypeAtLocked` `:74-133`,
`boundCandidatesLocked` `:280-339`, `mergedBoundsLocked`, `prevBoundaryLocked`,
`isClearedLocked`, `isUnreadableLocked`, `segmentValueLocked` `ryw.go:1120-1130`,
`conflictRangesLocked` `:1528`) take `w *rywWrites` instead of `includeWrites bool` and the
live fields. `w == nil` is `SnapshotCache::iterator` (A2.2). `swapWriteSpanLocked` goes.

The walk's cursor uses `hi = \xff\xff` regardless of `maxReadKey`, so its segments are
C++'s: there is no boundary at `\xff` unless something is cached or written there, and
`getkey_after_set`'s last segment is `[b1\0, \xff\xff)`. Seeking a key `>= \xff\xff` lands
on a sentinel segment `[\xff\xff, \xff\xff\0)` of type `segEmpty` (A1). `prev()` from the
sentinel goes to the last real segment. `next()` on the sentinel is unreachable (A6 stops
at L4/L6 first); it returns `internal_error` (4100) instead of looping. The snapshot cache
accepts entries ending at `\xff\xff` (A8 inserts them for `readThroughEnd`).

New cursor operations, each a port:

| Go | C++ |
|---|---|
| `cur.same(o)` | `it == itEnd`: both valid, equal `begin` (one lock hold, one partition) |
| `cur.known()`, `cur.isKV()` (`segKV` or `segPhantom`), `cur.empty()`, `cur.unreadable()` | `!is_unknown_range()`, `is_kv()`, `is_empty_range()`, `is_unreadable()` |
| `cur.kv() (KeyValue, bool)` | `kv()`; false for a phantom |
| `cur.skipContiguous(bound []byte) []KeyValue` | RYWIterator.cpp:128-133: when the segment is a cache KV under an unmodified write range, land on the last value of the same cache entry `< min(bound, next write boundary)`; return the run |
| `cur.skipContiguousBack(bound []byte) []KeyValue` | RYWIterator.cpp:135-140: first value `>= max(bound, keyAfter(write segment begin))` |

`segPhantom` and `segUnreadable` keep their current meaning (`ryw_getkey.go:53-66`).

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

`mappedRangeLimits` (`mappedrange_selectors.go:17-59`) becomes `rangeLimits`. The existing
`KeySelector` (`mappedrange.go:107-111`) gains `removeOrEqual`, `isFGE`, `isBackward`,
`isDefinitelyGreater/Less` and `keyAfter` with the `\xff\xff` rule. Selector offsets use
`int32` arithmetic that wraps as C++'s `int` does in practice (`2 - read_begin.offset`,
`read_end.offset += n`, `read_begin.offset -= n`); `additionalRows += 1 - end.offset` and
`begin.offset - 1` are computed in `int32` and widened, as C++ does.

```go
type nativeRangeResult struct {
	kvs                         []KeyValue // scan order
	more, readToBegin, readThroughEnd bool
	readThrough                 []byte     // nil when absent
}
type nativeRangeFunc func(ctx context.Context, begin, end KeySelector, limits rangeLimits, reverse bool) (nativeRangeResult, error)
```

### D4. Selector-capable storage dispatch: `nativeGetRange`

`getMappedRangeSelectors` (`mappedrange_selectors.go:113-182`) is already a port of NAPI
`getRange` for FGE selectors. It becomes generic and complete:

```go
func nativeGetRange[T any](tx *Transaction, ctx context.Context, begin, end KeySelector,
	limits rangeLimits, reverse bool, ops rangeScanOps[T]) (nativeRangeResultOf[T], error)
func (tx *Transaction) nativeGetRange(ctx context.Context, begin, end KeySelector,
	limits rangeLimits, reverse bool) (nativeRangeResult, error) // plain KVs, snapshot
```

It implements A10 exactly: `getRangeInternal`'s checks, the `readToBegin` prologue, the
per-shard request selectors, `transformRangeLimits` including the `bytes == 0` branch,
`decrement`, `finished`, `readThrough`, the first-reply shortcut, the FGT/FGE continuation,
the empty-reply fallback and the error arms (wrong shard falls back, all-alternatives and
maybe-delivered retry, reply timeout re-sends, as the mapped port does today). It counts
`getRangeRequests` once per call (`readpath.go:634`).

Wire changes needed under it:
- `rangeRPCOps.build` and `buildGetKeyValuesRequest` (`readpath.go:1226-1246`) take
  `KeySelector`s; byte-key callers pass `FGE(key)`, so their bytes are unchanged.
- `sendRangeRPC` (`readpath.go:991-1007`) takes `rangeLimits` and calls a
  `transformRangeLimits(l, reverse) (limit, limitBytes int32)` port. For `minRows = 1` and
  `bytes != 0` it computes what the current code computes.
- `rangeScanImpl` (`readpath.go:700-960`), which is `getExactRange`, takes `rangeLimits`.
  `remaining` and `bytesLeft` become `limits.rows` and `limits.bytes` with `decrement`, and
  `softByteStop` becomes `limits.hasSatisfiedMinRows() && len(out) > 0`, which is NAPI
  getExactRange's own condition and equals today's for `minRows = 1`. The fallback needs
  this because RYW requests carry `minRows > 1`.
- The fallback resolves selectors with `tx.getKeyImpl` (`readpath.go:148-252`), begin then
  end.

The mapped path keeps its FGE guard (`mappedrange_selectors.go:82-84`) inside its `send`
op; mapped reads go through the same loop and their requests do not change.

### D5. The walk: `rywWalk`

A new file `ryw_walk.go` holds A3-A9 as Go code with the C++ line in a comment at each
step. Because the walk must release `c.mu` across storage I/O and must compute its first
request at issue, it is a state machine whose suspension points are exactly C++'s `wait`s.

```go
type rywWalk struct {
	c        *rywCache
	view     *rywWrites // pinned version; nil = cache only (A2.2)
	gen      uint64     // c.gen at issue; see "Reset" below
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
func (c *rywCache) newGetKeyWalkLocked(view *rywWrites, sel KeySelector, maxKey []byte) *rywWalk // A9; applies offset++ for offset <= 0
func (w *rywWalk) stepLocked()                         // runs A6/A7 to the next await or the end
func (w *rywWalk) run(ctx context.Context, native nativeRangeFunc) error
func (w *rywWalk) rangeResult() rywRangeResult         // A6/A7 epilogue
func (w *rywWalk) keyResult() []byte                   // A9 result mapping
type rywRangeResult struct{ kvs []KeyValue; more, readToBegin, readThroughEnd bool }
```

`stepLocked` builds its cursors (`it`, `itEnd`) fresh on entry by re-resolving `begin` and
`end`, which is what C++ does after every await (P3, U10, U10'). Between awaits the cursors
live in `stepLocked`'s frame; no cursor survives a lock release. When `stepLocked` reaches
a nested getKey (P3, L3, L3'), it creates `sub` and calls `sub.stepLocked()` in the same
critical section, as the nested C++ actor runs to its own first `wait`.

`run` drives the awaits without holding `c.mu`:

```go
for w.pc != pcDone {
	switch w.pc {
	case pcAwaitStorage:
		res, err := native(ctx, w.req.begin, w.req.end, w.req.limits, w.reverse) // U8 / U8'
		c.mu.Lock(); w.onReplyLocked(res, err); w.stepLocked(); c.mu.Unlock()   // U9-U10
	case pcAwaitPreKey, pcAwaitLoopKey:
		err := w.sub.run(ctx, native)
		c.mu.Lock(); w.onKeyLocked(w.sub.keyResult(), err); w.stepLocked(); c.mu.Unlock()
	}
}
```

**`requestCount`** lives in the walk and is incremented once per storage request, in
`stepLocked` when it builds `req` (U7/U7'), together with `additionalRows = 0`, before the
await. Rows consumed from the cache, segments skipped, nested getKeys (which count their
own requests), NativeAPI continuation requests inside one `native` call, and transport
retries do not increment it. `countedGetRange` (`ryw.go:829-832`) is deleted.

**Cache insertion** (`onReplyLocked`): `knownKeyRange(res, req.begin, req.end)` or
`knownKeyRangeBack` (A8) as `rywBoundary` pairs (`ryw_getkey.go:196-231` already models
`ExtStringRef`), materialized, then `c.serverCache.insert(b, e, ascending(res.kvs))`. The
Go insert (`ryw_snapshot_cache.go:81-153`) is already `SnapshotCache::insert`, including
its empty-range early return (`:82-84`); it must accept `\xff\xff` as an end.

**Unreadable**: `stepLocked` returns `accessed_unreadable` (1036) at L7/K when `it` is
`segUnreadable` and nowhere else. `skipUncached*`, `countUncached*` and A3 treat it as a
barrier without error. This replaces `unreadableScanCapLocked` on the range path.

**Reset**: C++ kills in-flight reads through `resetPromise` (RYW:358-364). `rywCache.reset`
increments `c.gen`; `onReplyLocked` drops the reply and fails the walk with the read
incarnation's cause when `w.gen != c.gen`, so a stale reply is never inserted into the new
transaction's cache. Today `cacheServerReply` has this race.

### D6. getKey

`Transaction.GetKey` and `Snapshot.GetKey` (`transaction.go:982-1026`, `:610-631`) split into
issue and run like `issueRange`:

```go
func (tx *Transaction) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey
func (s *Snapshot) StartGetKey(ctx context.Context, sel KeySelector) *PendingKey
```

Issue does the entry checks (2004 on `key > maxReadKey`, not tracked, as today), picks the
route (A2: `rywDisabled` → `tx.getKey` + clamp, unchanged; snapshot with
`snapshotRYWDisableCount > 0` → walk with `view = nil`; else walk with a pinned view),
creates the walk and calls `stepLocked`, and registers in the commit barrier through
`opContextFor(parent, true)` as `issueRange` does (`range_issue.go:44, 80-82`; C++
`reading.add`, RYW:1706). Run drives the walk with `tx.nativeGetRange`, maps the result
(A9), adds the conflict through the pinned view for a non-snapshot read
(`addGetKeyConflictRange`, `transaction.go:1039-1074`, now given the view), and unpins.
The synchronous `GetKey` is `StartGetKey(...).Get()`. The facade's `Transaction.GetKey`
(`fdb/transaction.go:88-98`) calls `StartGetKey` in the call and only waits in the future,
so a `Set` after `tr.GetKey(sel)` is not visible to it.

### D7. Range reads

`rywRangeRead` (`ryw.go:678-691`) becomes `{c, view (pinned or nil), walk, snapshot}`.
`issueRangeInto` builds `begin = FGE(begin)`, `end = FGE(end)`,
`limits = newRangeLimits(limit, byteTarget)` and calls `stepLocked`. If the walk finished,
the read is local (`r.local`, `range_issue.go:78`) and its conflict is computed in the
same critical section with `c.w` (no pin); otherwise the view is pinned. `run` drives the
walk with `tx.nativeGetRange` and returns `rangeResult()`.

Routing (`range_issue.go:68-79`) keeps its three arms. `rangePathSnapshotNoRYW` now
creates a walk with `view = nil` instead of calling `localRange` and `getSnapshotRange`.
`rangePathRYWDisabled` keeps `getRangeReadThrough` until phase 4 (D10).

Conflicts (`range_issue.go:206-223`): `rangeConflictExtent` (`transaction.go:1151`) is
A2.3's `addConflictRange` for FGE selectors, where `readToBegin` and `readThroughEnd` are
always false (A3 sets them only for offsets other than 1, and the nested getKeys that set
them need a non-FGE bound). It stays, filtered through the pinned view instead of
`r.ryw.conflictRanges` / `localConflicts`. Phase 4 replaces it with the general port.

**Result assembly, as a contract.** What the range path returns is defined by A6/A7, not
by a merge:

- Rows come only from walk steps K/K'. A pending write is emitted when the walk reaches
  its segment, so the writes visible are exactly the pinned view's. Storage data enters
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
`keyAfter` advance `:897-907`), the three unreadable reach checks (`:785-787`, `:810-812`,
`:885-887`, `:910-913`), `cachedRangeLocked`'s issue-time answer (`:726-746`), and
`getSnapshotRange`'s fetch and re-walk (`:917-940`). In `getKeyRYW`, the resolve/read loop
(`ryw_getkey.go:567-638`), its backward window (`:601-614`), forward window (`:615-625`) and
empty-window exit (`:626-634`).

### D8. What stays the same

Point reads (`get`, `getSnapshot`), mapped ranges' RYW handling, `materializeCommit`, the
conflict filter `conflictRangesLocked` (now over a view), the snapshot cache structure,
the facade's `GetSliceWithError` and iterator continuation for byte-key ranges
(`fdb/range_result.go:166-201, 271-371`), and the RFC-098 divergence that keeps pending
writes inside SVK ranges.

### D9. Deletions

When the phase that removes the last caller lands (with `rg` output in the PR showing no
caller remains): `getKeyRYW`, `keySelResult`, Go's `resolveKeySelectorFromCache`,
`errGetKeyRYWLoop` (`ryw_getkey.go:369-640`); `rywRangeRead.run`'s body,
`cachedRangeLocked`, `getSnapshotRange`, `replyEdgeKnown`, `rywWriteSpan`,
`captureWriteSpanLocked`, `overlappingRanges`, `swapWriteSpanLocked`, `localRangeInSpan`,
`mergeBatchInSpan`, `rywRequestBytes`, `rywFirstRequestBytes`, `applyRangeByteLimit`,
`localRange`, `localRangeLocked`, `fetchOrCached`, `fetchServer`, `cacheServerResult`,
`cacheServerReply`, `applyLimitAndDirection`, `computeMore`, `cacheWalkBudget`,
`limitReached`, `mergeBatch`, `mergeBatchLocked` (`ryw.go:726-1475` except
`segmentValueLocked`), `unreadableScanCapLocked` (`:2187`), and
`snapshotCache.getRangeKVsLimited`, `skipKnownEmpty`, `knownPast` if nothing else calls
them. Tests that exercise only these helpers go with them; tests that encode a request
sequence C++ does not send are rewritten to the C++-derived trace (for example the second
half of `TestRYWWriteOverlayRequestBytesClampedAndDoubled`,
`ryw_range_cut_unit_test.go:253-300`: under `clearRange(b, e)` C++ sends `[a, b)` then
`[e, z)` with 30000 then 60000 bytes, A4's multi-key barrier, not three requests over `[a, z)`).

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
- RYW-disabled route: A2.1's `readThrough`, calling `nativeGetRange` with `snapshot = false`,
  which then ports `getRangeFinished`'s conflict (NAPI:4558-4590); the backward
  `end.offset > 1` getKey; the forward `maxReadKey` clip. This retires
  `getRangeReadThrough` for range reads.
- Conflicts on the RYW route: `addConflictRange(GetRangeReq<false/true>)` (RYW:245-320)
  with the walk's `readToBegin`/`readThroughEnd`, filtered through the pinned view.
- Facade: pass selectors through; drop `resolveRange`/`resolveSelector`; keep
  `rangecheck.Check` on the raw keys (`fdb/range_result.go:81-88`). Unlimited rows go to the
  client as 0 (→ -1), not `MaxInt32` (`fdb/range_result.go:137-142`): A5 manufactures rows
  only when `!hasRowLimit()`, so `MaxInt32` would send 80000 where C++ sends
  `max(1 + additionalRows, offset)`. The facade keeps its own `MaxInt32` arithmetic for
  `remaining`. Continuations keep the other bound's original selector:
  `FirstGreaterThan(last)` as begin forward, `FirstGreaterOrEqual(last)` as end reverse.

## Phases

Each phase is independently mergeable. "Golden-safe" means `go.golden` changes only in
sections that thereby equal `libfdbc.golden`, and `libfdbc.golden`'s existing sections are
byte-identical (checked by `diff` of the old sections, recorded in the PR).

1. **Infrastructure, no wire change.** D1 (versioned write map; `rywRangeRead` pins instead
   of capturing a span; `swapWriteSpanLocked` becomes a pointer swap), D2 (cursor over a
   view, `\xff\xff` universe, sentinel, skipContiguous), D3, D4 (generic `nativeGetRange`;
   mapped reads move onto it; selector builder; `transformRangeLimits`; `rangeScanImpl` on
   `rangeLimits`). Gate: `go.golden` unchanged; mapped, multishard and RYW suites green;
   the D1 benchmark reported.
2. **Walk + getKey.** D5, D6. Deletes `getKeyRYW` and Go's resolver. Closes the six
   `getkey_*` divergences; `go-divergences.diff` keeps only `set_then_range`.
3. **Range reads.** D7, D9. Closes `set_then_range`; `go-divergences.diff` is empty.
4. **Selector ranges.** D10, with new lab scenarios (Executable spec, S3).

## Executable spec

### S1. Deterministic walk traces (hermetic, fast lane)

`ryw_walk_trace_test.go` drives the walk with a scripted `nativeRangeFunc`. Each case
fixes: the snapshot cache, the pinned write map, the caller's selectors and limits, and
the reply to each request. It asserts, per request in order, `read_begin` and `read_end`
(key, orEqual, offset) and `requestLimit` (`rows`, `minRows`, `bytes`), and the wire
`(limit, limitBytes)` from `transformRangeLimits`; then the rows, `more`, `readToBegin`,
`readThroughEnd`, the cache entries after each insert, and, for non-snapshot reads, the
filtered conflict ranges. Expected values are derived by hand from the cited C++ lines
and the derivation is a comment in the case. A case reachable through the public API also
has a wire-golden twin (S3), which checks the derivation against libfdb_c.

Example derivations (keys unprefixed; unless a case says otherwise, storage is
`{a, b, c, d, e}` and the mode is EXACT, so bytes are -1):

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

The full case list, each forward and reverse unless noted:

1. The seven golden scenarios as traces (A11).
2. Traversal: T1; a pending atomic over an unknown base (UNKNOWN, crossed); a matched
   CompareAndClear phantom (crossed by `skipUncached`, counted by A3's offset walk, skipped by
   K); a dependent write over a cached base (KV).
3. Single-key empties that are not pending clears: a cached absent point read; a one-key
   gap between cached keys; counted from request 2 on.
4. Barriers: T2 (pending multi-key clear and a cached multi-key empty); T3 with an SVK range;
   the 100-segment traversal budget on request 1 (101 alternating keys stop it).
5. getKey: offsets +2, FGT, `lastLessThan`, `lastLessOrEqual`, -2; a selector landing in the
   cache with no request; offsets past the data (`readThroughEnd` → `maxReadKey`,
   `readToBegin` → `""`); a backward selector whose `offset++` lands at offset 1 on a known KV;
   `maxReadKey = \xff\xff` (end selector on the sentinel).
6. Selector offsets on both bounds: begin offset > 1 and < 1 (U5 third branch,
   `countUncachedBack`); end offset > 1 (`countUncached`, `read_end.offset += n`) and < 1;
   the U6/U6' crossed-key clamp; the P3 nested getKey (crossed selectors, returns empty after
   the nested request only) and the L3 nested getKey after rows were emitted; reverse P3.
7. Bytes: unlimited stays -1 on every request; positive bytes doubling only when a request
   consumed no rows (all fetched rows locally cleared); T5; U1 soft stop with rows from the
   cache and zero requests.
8. Saturation and counters: `begin.offset = MaxInt32` (`minRows` and `rows` clamp to
   `MaxInt32`); `requestCount` reaching 21 in one call with bytes 1 and every fetched row
   locally cleared (request bytes 1, 2, 4, …, 65536, then 80000; the walk's recorded
   `clearLimit` is `1 << 20` at `requestCount` 20 and 21); pure-function pins for
   `setRequestLimits` and `clearLimit` at `requestCount` 0, 1, 19, 20, 21, 64.
9. Caches: partial cache without writes (byte-limited: cached rows, more, zero requests;
   EXACT: request for the rest with the remaining rows); a warm second call with zero
   requests; an empty reply that caches a known-empty range; `readThrough` extending the
   known range and the next request starting there; native `readToBegin` and
   `readThroughEnd` reaching A8; a reverse reply inserted ascending; a reply to a selector
   with offset > 1 cached past `read_end.key`.
10. Interleaving: another read's cache insert between this walk's requests (through the
    scripted native's hook) moves the next request; cache-served segments do not change
    `requestCount`.
11. Routing: snapshot with snapshot RYW enabled (sees writes, no conflicts); snapshot with
    it disabled (`view = nil`: `getkey_after_set` returns the storage key `c`, not `b1`);
    RYW disabled (requests carry the caller's selectors and limits; `maxReadKey` clip;
    backward `end.offset > 1` getKey).
12. Issue-time isolation: for `StartGetRange` and `StartGetKey`, every later mutation kind
    (`set`, `clear`, `clearRange`, `atomic`, versionstamp, `setBypassUnreadable`) between
    issue and completion leaves the request sequence, rows, `more` and conflicts equal to
    the run without it; a second read issued before the first's reply sends its own
    request (first requests are computed at issue).
13. Reset during a walk: the late reply is not cached; the read fails with the
    incarnation's cause.

Each new branch has a mutation proof recorded in its PR: the mutation (for example,
dropping the `min(…, clearLimit)` clamp, keeping `read_end = end` when `end.offset < 1`,
not moving the persistent begin, adding `hasSatisfiedMinRows()` to U1', incrementing
`requestCount` after the await), evidence it compiled, and the case that failed.

### S2. Native getRange

`nativeGetRange` cases against the existing multishard harness and the frame-level
simulated transport (`simtransport_test.go`): per-shard selector rewriting forward and
reverse, `readThrough` on a modified final shard, the empty-reply fallback with both
flags, `wrong_shard_server` fallback, FGT continuation on the wire (`OrEqual=true`), the
`bytes == 0` minRows continuation, and mapped requests byte-identical to phase 0.

### S3. Wire goldens and differentials

- Red before, green after: phase 2 turns the six `getkey_*` sections of `go.golden` into
  `libfdbc.golden`'s; phase 3 does `set_then_range`; `go-divergences.diff` is empty after
  phase 3. `libfdbc.golden` is never regenerated to remove a diff.
- New scenarios (phase 2 and 3 for FGE, phase 4 for selectors), each the twin of an S1
  case: `getkey_across_clears` (T1-shaped), `range_across_single_clears` (T1),
  `range_clear_range_barrier` (T2), `range_reverse_across_clears` (T4),
  `range_partial_cache_exact`, `range_unreadable_stop` (T3, rows 2),
  `range_unlimited_large` (>80000 bytes in one call, FGT continuation),
  `range_modes_with_writes` (ITERATOR iterations 1-3, WANT_ALL, EXACT, SMALL, SERIAL, reverse),
  `snapshot_ryw_disabled_getkey`, `ryw_disabled_range_selectors`, and for phase 4
  `range_selector_offsets`, `range_crossed_selectors`, `range_end_last_less_than` (T5).
  Their `libfdbc.golden` sections come from a lab `-update` run
  (`tools/clientbench/scripts/wiregolden.sh`), which the owner runs; existing sections must
  stay byte-identical in that run.
- Result differentials (`pkg/fdbgo/bench`, libfdb_c in process): for every new scenario,
  rows, `more` and the commit's read-conflict ranges equal libfdb_c's, forward and reverse,
  snapshot and not.
- Retained after the diff is empty: all S1 traces, S2, the new golden sections, and the
  existing `ryw_issue_view_*`, RYW adversarial, fuzz and differential suites.

Validation per phase: `just test` (fast lane, including the wire-golden test and S1/S2);
`just test-full` at the phase boundary for the conformance and differential targets.

## Compatibility impact

Requests under RYW become libfdb_c's: getKey reads ask for 1-3 rows instead of 256, RYW
range reads ask for the caller's rows plus counted clears instead of 10000 or
`2 * remaining`, reads cross pending writes, forward multi-request reads continue with
`firstGreaterThan(last)`, and the first request of a byte-limited call uses the caller's
target. Results and conflicts do not change for byte-key reads (S3 differentials check
it). Phase 4 changes selector-range results' conflicts to C++'s single range conflict
instead of two getKey conflicts plus a range conflict, which matches libfdb_c.

## Open items

- **Write-map clone cost.** D1 trades per-read span copies for per-write clones under
  concurrent reads. Phase 1 measures it; a persistent map is the remedy if needed.
- **Multi-shard requests** change from `getExactRange`-shaped to `getRange`-shaped. The lab
  cluster's scenario prefix is one shard, so no golden covers this; S2 and the multishard
  differentials do.
- **Forward/backward U1 asymmetry** is unobservable through the C API (A7); only S1 pins it.
- **Segment partition.** The port assumes the Go cursor's partition equals `RYWIterator`'s,
  including adjacent cache entries splitting an empty run (SnapshotCache.h:231-254). RFC-057's
  property test checks the cursor against Go's materializer, not against C++; the S1 traces
  and S3 twins are the check against libfdb_c.
- **BUGGIFY** (`skipUncached` budget 0) is simulation-only and not ported.

## Review resolution index

| Review point | Resolved in |
|---|---|
| C++ 1: traversal vs counting; crossing KVs; non-clear empties; backward | A4, A7 (U2'), S1 cases 2-4 |
| C++ 2: selector transformations; `end.offset < 1` → FGE; `offset > 1` + counted clears; additionalRows; crossed-key clamp; persistent begin | A6 U3-U6, A7 U3'-U6', D5, S1 case 6 |
| C++ 3: `bytes == 0` vs -1; minRows; saturation; shift on remaining limit; requestCount uncapped | A1, A5, A10, D3, S1 T5 and cases 7-8 |
| C++ 4: backward getKey `offset++`; reverse boundaries; `setRequestLimits(…, read_end.offset, …)` | A7 U7', A9, A11, S1 case 5 |
| C++ 5: internal reads are snapshot; conflicts in the wrapper; requestCount/additionalRows before the await | A2.3, A6 U7-U8, D5 |
| Design contract: full selectors and limits; native conversion separate; byte-key callbacks replaced | D3, D4, D5 |
| Routing: snapshot RYW on/off, RYW disabled, partially cached reads without writes | A2, D6, D7, D10, S1 cases 9 and 11 |
| Streaming iteration vs requestCount; C binding limits | A2, D10, S3 `range_modes_with_writes` |
| Result assembly and cache contracts; readThrough/readToBegin/readThroughEnd; reverse inserts ascending; re-resolution | A6-A8, D5, D7 contract, S1 case 9 |
| Torvalds 1: return type loses minRows; selector-capable dispatch | D3, D4 |
| Torvalds 2: omitted branches; three distinct limits | A4, A5 |
| Torvalds 3: write view, unreadable, cache coverage, re-resolution, remaining limits, more; which stop conditions disappear | D1, D5, D7 |
| Torvalds 4: where requestCount lives; cache hits and retries | D5 |
| Tests: traces, unchanged libfdb_c goldens, retained regressions | S1-S3 |
