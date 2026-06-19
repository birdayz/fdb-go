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
local), not the eager pre-read `[begin, end)`. (C++ adds the conflict only in the read's success
branch — `ReadYourWrites.actor.cpp:388`, `if (!snapshot) addConflictRange(...)` after
`when(result = wait(read(...)))`; a *failed* read adds no conflict. The current Go eager-add even
conflicts on a failed read — a second, latent divergence the move also closes.)

## Verified C++ derivation (the exact clamp)

Both clients normalize a plain `GetRange(begin, end)` to selectors `firstGreaterOrEqual(begin)` /
`firstGreaterOrEqual(end)` — **both offset `+1`**. Substituting `begin.offset=1`, `end.offset=1`
into the two C++ clamp sites gives an **identical** rule on the RYW path
(`ReadYourWrites.actor.cpp:245-319`, `addConflictRange(GetRangeReq<false>/<true>)`) and the
RYW-disabled native path (`NativeAPI.actor.cpp:4558-4587`, `getRange` → `conflictRange`):

| read | conflict `[rangeBegin, rangeEnd)` |
|------|-----------------------------------|
| **forward**, `more`, non-empty | `[begin, keyAfter(lastReturnedKey))` |
| **forward**, `!more` or empty  | `[begin, end)` |
| **reverse**, `more`, non-empty | `[firstReturnedKey, end)` |
| **reverse**, `!more` or empty  | `[begin, end)` |

`lastReturnedKey`/`firstReturnedKey` are **`kvs[len(kvs)-1].Key` in both directions** — the Go
range cursor returns forward results ascending (so `kvs[last]` is the highest) and reverse results
descending (so `kvs[last]` is the lowest, matching `result.end()[-1].key`; `readpath.go:781`). The
`readToBegin`/`readThroughEnd` arms (`:263-266`/`:302-305`, `:4562-4584`) and the cross-offset
guards (`:4570`, `:4582`) are inert for offset-`+1`-on-both plain ranges, so they're omitted.

**`more` ⇒ non-empty for a row-limited read** (C++ `output.more = (data.size()==limit)`,
`readpath.go:754`; `limit==0` is unlimited ⇒ `more=false` at exhaustion). So the only way an
**empty** read carries `more=true` is a byte-ceiling cut before the first row — which Go's
row-limited `GetRange` cannot produce — and the table's `!more`/empty rows are what actually fire.

**Phantom protection is preserved.** An empty or fully-drained (`!more`) read keeps the full
`[begin, end)` conflict, so a concurrent insert *anywhere* in the requested range still trips 1020
— the read "saw" the whole extent. Only a `more=true` (limit-truncated) read narrows the conflict
to the prefix/suffix actually scanned, because the unread tail/head was genuinely not observed. The
clamp **never** widens to less than what was read, so it cannot introduce an under-conflict (the
data-integrity hazard §Risk warns about).

## Go change sites (3)

1. `Transaction.Get` (`transaction.go:671-673`) and `Transaction.GetPipelined`
   (`transaction.go:700-702`) — the facade `Get` uses `GetPipelined` with an `inner.Get` fallback
   (`fdb/transaction.go:55,73`), so **both** must route the single-key conflict through the RYW
   filter (D2). New helper `addReadConflictForKeyRYW(key)`: `rywDisabled` → full single-key conflict
   (no write map; the read went straight to storage — matches native `getValue`); else
   `conflictRangesLocked(key, keyAfter(key))` → add the filtered sub-ranges (mirrors GetKey's
   `addGetKeyConflictRange`, `transaction.go:1001`).
2. `getRangeDir` (`transaction.go:1082-1104`) — move conflict generation **after** the read;
   compute `[cBegin, cEnd)` per the table; `rywDisabled` → `addReadConflict(cBegin, cEnd)` (clamp
   only, no filter — D1); else `conflictRangesLocked(cBegin, cEnd)` → add filtered sub-ranges (clamp
   **and** filter — D1+D2). Preserve the existing `begin<=end` + non-special-key guards.

## Risk — why this is a dedicated RFC, not an inline grind fix

The current behavior is a SAFE over-conflict (extra spurious not_committed retries; Go never under-
conflicts). A **botched fix that under-conflicts loses serializability — a real data-integrity bug,
strictly worse than the status quo.** So this requires: (a) exact RYW-segment semantics ported 1:1,
(b) a differential proving both D1/D2 red→green AND a comprehensive conflict-outcome differential
proving no new under-conflict across op mixes, (c) the full fdb-client-review gauntlet (FDB-C-dev
validates the segment-filter semantics). Out of scope for a quick fix.

## Executable spec (the differential, in `pkg/fdbgo/bench`)

The D1 and D2 "record-the-gap" probes **already exist** in
`differential_getrange_conflict_test.go` (`TestDifferential_GetRangeConflictClamp_RFC121`,
`TestDifferential_ReadOwnWriteConflict_RFC121`) — each pins the current over-conflict (`go aborts,
cgo commits`) and is annotated to **flip to `goOut.conflicted == cOut.conflicted` (agreement) when
the fix lands**. The fix turns them red (the stale-probe `t.Errorf` fires), then the flip turns
them green. That is the red→green proof.

1. **D1:** seed `k00..k19`, pin both txns to the setup commit version; `GetRange([k00,kzz),
   limit=10)` reads `k00..k09` (`more`); concurrent committed write to `k15` (unread tail); assert
   go-commit-outcome == cgo-commit-outcome (both COMMIT after the clamp).
2. **D2:** `Set(K); Get(K)`; concurrent committed write to K; assert go-outcome == cgo-outcome
   (both COMMIT after the filter).
3. **New — `FuzzDifferential_ConflictOutcome`:** random op mix (Set/Clear/Get/GetRange fwd+rev with
   varied limits, including empty-range and full-drain reads) on txn A + a concurrent committed
   writer at a random key, run identically through both clients; assert **identical commit/abort**.
   This is the guard against introducing an under-conflict (a go-commits-where-cgo-aborts mismatch
   is a serializability regression, the worst outcome) and against breaking phantom protection on
   `!more`/empty reads.
4. **Go-side unit tests:** assert `tx.readConflicts` after a clamped read equals the table above
   (forward `more` → `[begin, keyAfter(lastKey))`; `!more`/empty → `[begin,end)`; reverse `more` →
   `[firstKey, end)`), and that a read served by a local independent `Set` adds no read-conflict —
   revert-proven.
