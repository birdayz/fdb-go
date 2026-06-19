# RFC-121: Get/GetRange read-conflict — RYW-filter + extent-clamp (vs libfdb_c)

**Status:** Draft

**Item:** Two confirmed conflict-range divergences from libfdb_c 7.3.75, found by the quality-grind
conflict-range audit (2026-06-19). Both make a Go transaction **over-conflict** relative to a C/Java
transaction doing the identical operations — a real, observable serializability-outcome divergence
(different commit/abort under the same concurrent write), though always in the SAFE direction (Go
never under-conflicts, never loses serializability).

## Problem

The pure-Go client adds read-conflict ranges **eagerly at the API call site** (a NativeAPI-style
layer), where libfdb_c RYW derives them from the coalesced `readConflicts` RangeMap + the WriteMap
*at commit*, filtered through `updateConflictMap`. Two consequences:

### D1 — GetRange does not clamp the read-conflict to the data actually returned

- **Go:** `getRangeDir` (`transaction.go:1094-1096`) adds the **full requested `[begin, end)`**
  read-conflict eagerly, *before* the read (`:1102`), and never adjusts it — forward and reverse,
  RYW-enabled and `rywDisabled`.
- **C++:** clamps to what was read. RYW: `addConflictRange(GetRangeReq<false>)`
  (`ReadYourWrites.actor.cpp:257`, `:271-274` — `rangeEnd = keyAfter(lastReturnedKey)` when `more`;
  reverse `:295`). RYW-disabled: NativeAPI `getRangeResult`→conflictRange
  (`NativeAPI.actor.cpp:4576-4579`; reverse begin clamp `:4564-4567`).
- **Scenario:** `GetRange([a, z), limit=10)` returns 10 rows ending at `j` with `more=true`. C++ adds
  read-conflict `[a, j\x00)`; Go adds `[a, z)`. A concurrent txn commits an insert at `m` (`j < m <
  z`). **Go aborts with not_committed (1020); C++ commits.** Higher impact (limited scans are common;
  the spurious window is the whole unread `[j, z)` tail).

### D2 — Get / GetRange do not skip the read-conflict for a read served by a local independent write

- **Go:** `Get` (`transaction.go:671-673`) and `GetRange` (`:1094-1096`) call
  `addReadConflictForKey`/`addReadConflict` **unconditionally**, never consulting the RYW write map.
  The `updateConflictMap` filter (`conflictRangesLocked`, `ryw.go:975`) is wired **only into GetKey**
  (`addGetKeyConflictRange`, `transaction.go:1001`) — RFC-058 closed GetKey but left Get/GetRange.
- **C++:** `updateConflictMap` (`ReadYourWrites.actor.cpp:328`, `:342`) inserts a read conflict only
  for `is_unmodified_range() || (is_operation() && !is_independent())`. A plain `Set` is
  `INDEPENDENT_WRITE` (`WriteMap.h:171-174`), so a Get/GetRange served from a local Set adds **no
  read conflict**.
- **Scenario:** `Set(K, v); Get(K); commit`. C++ ships only a **write** conflict on K; Go ships
  **read + write**. A concurrent txn commits a write to K in between. **Go aborts (1020); C++
  commits.**

The audit also verified NO divergence on: Get single-key range, GetKey base↔resolved range
(RFC-058), Set/Clear/atomic/ClearRange write conflicts + NEXT_WRITE_NO_WRITE_CONFLICT_RANGE,
snapshot reads (no conflict), explicit AddRead/WriteConflictKey, and coalescing (Go sends
uncoalesced but the resolver decides by key-space membership — a wire-shape difference only, never
a serializability divergence).

## Proposed fix

Route Get/GetRange read-conflict generation through the **same RYW-overlay path GetKey already
uses** (`conflictRangesLocked`/`updateConflictMap`), derived from the post-read result:
- **D2:** for Get(key) and each GetRange segment, add the read conflict only when the value came from
  storage (unmodified range or dependent op), skipping independent-write segments — mirroring
  `addConflictRange(GetValueReq/GetRangeReq)`.
- **D1:** clamp the GetRange conflict end to `keyAfter(lastReturnedKey)` (forward) / the returned
  begin (reverse) when `more` or a limit truncated the read — mirroring the C++ clamp.

This means generating the read-conflict **after** the read (knowing the extent + which segments were
local), not the eager pre-read `[begin, end)`.

## Risk — why this is a dedicated RFC, not an inline grind fix

The current behavior is a SAFE over-conflict (extra spurious not_committed retries; Go never under-
conflicts). A **botched fix that under-conflicts loses serializability — a real data-integrity bug,
strictly worse than the status quo.** So this requires: (a) exact RYW-segment semantics ported 1:1,
(b) a differential proving both D1/D2 red→green AND a comprehensive conflict-outcome differential
proving no new under-conflict across op mixes, (c) the full fdb-client-review gauntlet (FDB-C-dev
validates the segment-filter semantics). Out of scope for a quick fix.

## Executable spec (the differential, in `pkg/fdbgo/bench`)

1. **D1:** seed `[a..z]`; both clients `GetRange([a,z), limit=N)`; capture the returned extent;
   concurrent committed insert in the unread tail; assert go-commit-outcome == cgo-commit-outcome
   (currently Go aborts, cgo commits → RED).
2. **D2:** both clients `Set(K); Get(K)`; concurrent committed write to K; assert
   go-outcome == cgo-outcome (currently Go aborts, cgo commits → RED).
3. A `FuzzDifferential_ConflictOutcome` over random op mixes + a concurrent writer, asserting
   identical commit/abort — the guard against introducing an under-conflict.
