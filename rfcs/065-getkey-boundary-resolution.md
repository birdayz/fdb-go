# RFC-065: getKey backward-selector resolution at the keyspace boundary

**Status:** Implemented
**Item:** RFC-010 C3 (fresh differential axes). getKey resolution is the wire hard line; the
existing getKey differentials (RFC-055/056/057/058) cover the keyspace INTERIOR and clamp
off-prefix results, masking the EDGES. A boundary probe found a real divergence.

## Problem (a real bug, found differentially)

A **backward** key selector (`lastLessThan`/`lastLessOrEqual`, offset ≤ 0) whose key is at or past
`maxReadKey` (`\xff` without system-key access) wrongly resolved to `maxReadKey` itself, instead
of the greatest key **strictly less than** `maxReadKey`. Probe vs libfdb_c (pinned read version):

| selector | go (before) | cgo |
|---|---|---|
| `lastLessThan(\xff)` | `\xff` ✗ | the last user key (e.g. `bench_range_0099`) |
| `lastLessOrEqual(\xff)` | `\xff` ✗ | the last user key |
| `firstGreaterOrEqual(\xff)` | `\xff` | `\xff` (correct — nothing ≥ \xff) |
| `firstGreaterThan(\xff\xff)` | 2004 | 2004 |

`\xff` is not `< \xff`, so go's result violated the `lastLessThan` contract outright. Two Go apps
sharing a cluster with Java would disagree on "the greatest key" — a silent wire divergence.

## Root cause

`resolveKeySelectorFromCache` (`pkg/fdbgo/client/ryw_getkey.go`, the port of C++
`ReadYourWrites.actor.cpp:409`) short-circuited **every** off-end seek to `readThroughEnd`:

```go
cur.seek(key)
if cur.offEnd() {                  // key >= maxKey
    return readThroughEnd(maxKey)  // WRONG for backward selectors
}
```

C++ does **not** short-circuit here: `it.skip(key)` **clamps** the iterator to the last segment,
and `readThroughEnd` is only set **after** the offset walk, and only for `offset > 1` (a forward
overshoot). A backward selector at `maxKey` resolves backward over the last segment to the
greatest present key. Go's unconditional shortcut returned `maxKey` for the backward case.

## Fix

Make the off-end branch direction-aware (`pkg/fdbgo/client/ryw_getkey.go`):
- **Forward** (offset > 0): keep `readThroughEnd` → `maxKey` (correct — nothing past the end).
- **Backward** (offset ≤ 0): reposition onto the last segment `[prevBoundary(maxKey), maxKey)` and
  resolve backward (anchoring the FGE-form key at `maxKey`, so the backward server-read window is
  `[unknownBegin, maxKey)`), mirroring C++'s `it.skip()` clamp. Empty DB → `allKeysBegin`.

Only the RYW path had the bug; the rywDisabled path delegates selector resolution to the server
(correct). The fix is one direction-aware branch; no other path changes.

## Performance

The backward off-end case now does a bounded reverse server read (limit-1-style, like any backward
getKey) instead of returning a sentinel — one extra round-trip ONLY for a selector that resolves at
the keyspace end, which is rare. No change to the common interior path.

## Test plan

`TestDifferential_GetKeyBoundary` (`pkg/fdbgo/bench/`) — both clients pin the SAME read version
(deterministic snapshot; transient stale-pin → retry with a fresh version), and compare the
resolved key / error code for: `lastLessThan`/`lastLessOrEqual(maxReadKey)` (the bug — asserted
`< maxReadKey`), `lastLessThan(empty)` → `allKeysBegin`, `firstGreaterOrEqual`/`Than(maxReadKey)`,
`firstGreater*(allKeysEnd)` / past-max → 2004, and large ±offsets walking off each end. Teeth:
re-introducing the unconditional shortcut reddens `LLT_maxReadKey` and `LLE_maxReadKey` (go=`\xff`
vs cgo=last-key). Existing getKey RYW + conflict differentials stay green (no regression).
