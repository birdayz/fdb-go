# RFC-262: Size RYW storage reads as getRangeValue does

**Status:** Draft
**Item:** TODO.md, "Wire divergences found by the wire goldens": `getkey_*`, `set_then_range`.
**Spec:** FoundationDB 7.3.77, `fdbclient/ReadYourWrites.actor.cpp`.

## Problem

When a read-your-writes read needs storage data it has not cached, libfdb_c and the Go
client send different `GetKeyValuesRequest`s. The wire goldens show it in seven
scenarios:

| Scenario | libfdb_c | Go |
|---|---|---|
| `getkey_offset` (`b`, orEqual, +2) | limit 2 | limit 256 |
| `getkey_first_greater_than` | limit 1 | limit 256 |
| `getkey_last_less_than`, `getkey_last_less_or_equal` | limit -1 (reverse) | limit -256 |
| `getkey_negative_offset` (-2) | limit -3 | limit -256 |
| `getkey_after_set` (pending write `b1`) | `[b\x00, \xff)`, limit 1 | `[b\x00, b1)`, limit 256 |
| `set_then_range` (iterator mode, pending write) | 80000 rows, 4096 bytes | 10000 rows, 4096 bytes |

The data returned is the same, but Go asks storage for up to 256 times the rows it needs,
stops reads at pending writes that C++ reads across, and sizes RYW range reads with an
invented heuristic (`fetchLimit` 10000, or twice the remaining rows with a floor of 256,
`ryw.go` slow path). Both paths have one C++ source: `RYWImpl::getRangeValue` and
`getRangeValueBack`, which `read(GetKeyReq)` also uses with `GetRangeLimits(1)`
(`ReadYourWrites.actor.cpp:140-160`).

## C++ algorithm (forward; backward mirrors it)

`getRangeValue(begin, end, limits)` (`:592-800`) walks the RYW iterator from the resolved
begin. At an unknown (uncached) range it issues one storage read, then re-resolves both
selectors against the grown cache and continues:

1. `clearLimit = requestCount ? 1 << min(requestCount, 20) : 0`.
2. `ucEnd` starts at the unknown range; `skipUncached(ucEnd, itEnd, clearLimit + 100)`
   (`:513-537`) extends it over known single-key empty ranges (pending single-key
   clears) separating unknown ranges, counting them as `singleClears`, capped at
   `clearLimit`. On the first request the cap is 0.
3. `read_end`: the end of that uncached stretch, `firstGreaterOrEqual(ucEnd.endKey())`,
   when it stops before `itEnd`; otherwise the end selector itself (forward offsets
   above 1 also count further single clears with `countUncached`, `:548-566`). An end
   offset below 1 adds `1 - end.offset` to `additionalRows`.
4. `read_begin`: a first-greater-or-equal begin starts at the later of the selector key
   and the unknown range's start; an offset above 1 becomes
   `firstGreaterOrEqual(key)` with `begin.offset - 1` added to `additionalRows`; an
   offset below 1 subtracts the single clears just before it (`countUncachedBack`).
5. `setRequestLimits(requestLimit, additionalRows, 2 - read_begin.offset, requestCount)`
   (`:569-590`): `minRows = max(1 + additionalRows, offset)`; with a row limit,
   `rows = max(max(1, rows) + additionalRows, offset)`; with no byte limit the byte limit
   becomes unlimited; with one, it doubles per request, capped at `REPLY_BYTE_LIMIT`.
6. Send `getRange(read_begin, read_end, requestLimit, snapshot)`, insert the result into
   the snapshot cache, reset `additionalRows`, `requestCount++`.

`getKey` is `getRangeValue(sel, firstGreaterOrEqual(maxReadKey), GetRangeLimits(1))`
forward and `getRangeValueBack(firstGreaterOrEqual(allKeys.begin), sel, GetRangeLimits(1))`
backward (`:140-160`), which is where limits 1, 2, -1 and -3 come from.

## Design

Port the request computation of both loops once, as a function of the RYW segment
cursor, the current selectors, the caller's limits, `additionalRows` and
`requestCount`, returning `(readBegin, readEnd, rows, bytes)`. Both Go callers use it:

- `getKeyRYW` replaces its fixed `fillBatch` read with the ported request, with
  `GetRangeLimits(1)` and the selector as C++ passes them.
- The RYW range slow path (`ryw.go`) replaces the `fetchLimit` heuristic with the ported
  request and the same per-request state.

The segment cursor (`newSegCursor`) and `resolveKeySelectorFromCache` are already ports
(RFC-056); `skipUncached`, `skipUncachedBack`, `countUncached` and `countUncachedBack`
become cursor methods. The ported loops replace the current stop conditions only where
C++'s differ; result assembly and the snapshot cache are unchanged.

## Executable spec

- The wire goldens: the seven scenarios above match libfdb_c and leave
  `go-divergences.diff` empty. New scenarios for the cases the goldens do not yet reach:
  a selector across pending single-key clears (second request with `singleClears`), a
  reverse range with a byte limit over several requests, an exact-mode limit smaller
  than the stretch.
- Unit tests of the request function against hand-computed C++ values for each branch
  of steps 2-5, forward and backward, including `requestCount` 0, 1 and 21 (the cap).
- The existing RYW read tests and the `pkg/fdbgo/bench` RYW differentials (results must
  not change).

## Wire-compatibility impact

Request limits and ranges change to libfdb_c's; results do not. Fewer rows per storage
read for selectors (1-3 instead of 256), more for RYW iterator reads (80000 rows under
the same byte limit).
