# RFC-058: Op-type-preserving write-map (phantom slots + exact conflict filtering)

**Status:** Implemented — both sub-edges shipped + differential-proven vs libfdb_c. (a) getKey
phantom slots via `segPhantom` (count-in-walk + skip-at-landing — the RFC-056 "counted" guess was
disproved by the differential; getKey is a limit-1 range read) + a fold-path nil bug the same
differential caught. (b) `updateConflictMap` filtering via `conflictRangesLocked`, with a
commit-order conflict differential that fails without the fix (go over-conflicts on INDEPENDENT
writes + cleared ranges) and passes with it.
**Item:** RFC-056 continuation (2). Follows the merged getKey-RYW core (#235, RFC-056) and
the lazy iterator (#236, RFC-057). Closes BOTH deferred sub-edges of RFC-056 item (2).

**Key invariant (the load-bearing fact, verified by FDB-C-dev + Torvalds):** C++
`isDependent()` (`WriteMap.cpp:49-55`) reads `singletonOperation` — the **bottom** of the
operation stack, fixed at first write and NEVER mutated by `coalesceOver` (`WriteMap.cpp:480`
only `poppush`/`push`es the TOP). So a flat `dependent` bool, set once at entry birth and
*carried unchanged* through every later fold, faithfully reproduces `isDependent()`. This is a
correct compression of the OperationStack, not a hack — provided every birth site sets it
right (the table below) and folds never overwrite it.

## Problem

The `rywCache` (`pkg/fdbgo/client/ryw.go`) models pending writes as `{value, hasAtomics,
atomics}` and **eagerly folds** a resolved atomic into a plain entry. That fold is correct
for VALUES — the RFC-055 Get/GetRange differential proves byte-identity with libfdb_c — but
it **erases the per-key operation TYPE** that C++'s `WriteMap` preserves. Two consumers need
that type, and both currently diverge from C++:

1. **Phantom-slot handling in getKey.** A `CompareAndClear` that *matches* clears the key's
   value but is still an `is_kv` segment in C++. **The original RFC-056 hypothesis — "the
   phantom is a counted slot the offset walk lands on" — was only HALF right, and the
   differential (newly enabled for pending CAC) proved it.** C++ getKey is a **limit-1 range
   read** (`read(GetKeyReq)` = `getRangeValue`/`getRangeValueBack(limit=1)`,
   `ReadYourWrites.actor.cpp:141-159`), so a phantom is **COUNTED** in the offset walk
   (`resolveKeySelectorFromCache` counts `it.is_kv()`) **but SKIPPED at the landing** (the range
   iteration returns the first `kv()`-non-null key — `RYWIterator::kv()` returns `nullptr` for a
   matched CAC, `RYWIterator.cpp:86-89`). So `getKey(FGT(a))` over `{a, b-phantom, c}` returns
   **c** (skip the phantom), but `getKey(offset+2 from a)` **counts** b and lands on c too. Go's
   previous model (matched CAC → `segEmpty`, moved to the `cleared` list) **never counted** the
   phantom — correct for offset-1 selectors but **wrong for offset>1** (it under-counted). Pending
   CAC was *excluded* from the getKey differential, so this was untested; enabling it surfaced the
   real (and subtler) behavior.

2. **Over-broad read-conflict on getKey.** C++ `updateConflictMap`
   (`ReadYourWrites.actor.cpp:335`) records a read-conflict for a getKey-resolution segment
   **only** when it required a DB read — i.e. UNMODIFIED ranges and DEPENDENT writes — and
   SKIPS INDEPENDENT writes and CLEARED ranges (no DB read happened there). Go keeps the
   **full** base↔resolved range (`transaction.go addGetKeyConflictRange`), over-conflicting on
   locally-satisfied segments. That is *safe* (extra spurious retries, never a missed
   conflict) but it is a behavioral divergence from C++: a Go getKey conflicts where a C++
   getKey would not. A naive `!hasAtomics` filter was tried on #235 and was **UNSAFE** (codex):
   a Get-folded *dependent* atomic looked independent post-fold, so the filter dropped a
   *required* conflict. The safe fix needs the op-type preserved, not reconstructed.

Root cause shared by both: **the fold throws away whether a key is INDEPENDENT or DEPENDENT,
and whether a CAC-cleared key is still a slot.** This RFC restores that information.

## Investigation (C++ reference, FoundationDB 7.3.x at /tmp/fdbsrc)

### The two faces of a write-map segment: `type()` vs `is_independent()`

`WriteMap::iterator` (`WriteMap.h:152-205`) classifies every key into one of four
`SEGMENT_TYPE`s — `UNMODIFIED_RANGE`, `CLEARED_RANGE`, `INDEPENDENT_WRITE`, `DEPENDENT_WRITE`
— and exposes TWO orthogonal predicates that the two consumers use:

- **`type()`** (`WriteMap.cpp:305-310`): at an operation key, `stack.isDependent() ?
  DEPENDENT_WRITE : INDEPENDENT_WRITE`. Drives the RYWIterator `typeMap` → `is_kv` for the
  **offset walk** (sub-edge 1).
- **`is_independent()`** (`WriteMap.h:171-174`): `following_keys_cleared ||
  !stack.isDependent()`. Drives **`updateConflictMap`** (sub-edge 2).

`isDependent()` (`WriteMap.cpp:49-55`) is true iff the **bottom** of the operation stack
(`singletonOperation`) is NOT one of `SetValue`, `ClearRange`, `SetVersionstampedValue`,
`SetVersionstampedKey`. So: a plain `Set` → independent; a standalone atomic (Add/CAC/…) →
dependent; a versionstamp → independent (but unreadable).

### An atomic over a locally-cleared range is INDEPENDENT (not a divergence)

When an atomic (`is_dependent`) hits a key inside a cleared range, `mutate`
(`WriteMap.cpp:102-111`, and the same-key arm `:142-144`) pushes a synthetic
`SetValue(<no value>)` at the bottom and coalesces the atomic on top. The bottom is now
`SetValue` ⇒ `isDependent()` is false ⇒ **INDEPENDENT_WRITE**, with `following_keys_cleared`
preserved. This is *exactly* Go's eager-fold-over-empty (`atomic()` `!exists &&
isClearedLocked` arm), which already matches C++ for values. **No change needed here.**

### A matched `CompareAndClear` is a phantom: COUNTED in the walk, SKIPPED at the landing

`coalesce` (`WriteMap.cpp:374-384`): `CompareAndClear` over a `SetValue` base returns the
existing value if it did NOT match, else `RYWMutation(Optional<ValueRef>(), SetValue)` — a
`SetValue` whose value is **not present**. The segment TYPE is unchanged: `INDEPENDENT_WRITE`
(folded over a Set) or `DEPENDENT_WRITE` (standalone), and `typeMap` (`RYWIterator.cpp:25-49`)
maps both to `KV` over a known cache, so `it.is_kv()` is **true**.

The decisive part is **how getKey uses this**. getKey is NOT a bare `resolveKeySelectorFrom
Cache` — `read(GetKeyReq)` (`ReadYourWrites.actor.cpp:141-159`) is a **limit-1 range read**:
`getRangeValue(sel, FGE(maxKey), limit=1)` for offset>0, else
`getRangeValueBack(FGE(begin), sel, limit=1)`. So resolution is two phases:
1. **`resolveKeySelectorFromCache`** transforms the selector toward FGE form, counting
   `it.is_kv()` for the offset (`:437-456`: `if (it.is_kv()) --key.offset`) — a phantom IS
   counted here.
2. **The range iteration** from the resolved position returns the first `kv()`-non-null key.
   `RYWIterator::kv()` (`:74-92`) returns `nullptr` for a matched CAC ("Key is now deleted") —
   so the phantom is **SKIPPED at the landing**; getKey returns the adjacent present key.

Net: a phantom **counts toward the offset but cannot be the result**. Go's old model (matched
CAC → `segEmpty`/`cleared`) never counted it — right for offset-1, wrong for offset>1. **Only
`CompareAndClear` produces a no-value result; every other `do*` atomic yields a present
(possibly empty) value** — so phantom slots come *only* from CAC.

**Bonus pre-existing bug, found by the same differential:** the eager-fold path (`atomic()`)
did not normalize an empty atomic result `nil → []byte{}` the way `resolveAtomics` does
(`ryw.go`). So `Max(d,"")` over a cleared base resolved to a `nil` value, which a *subsequent*
`CompareAndClear` (`doCompareAndClear(nil,…)`) misread as **absent** and cleared — turning a
present-empty key into a phantom and diverging from libfdb_c (which keeps it present). Fixed at
both fold sites; the failing fuzz input is pinned as a corpus seed + a deterministic case.

### `updateConflictMap` and the getKey conflict range

`addConflictRange(GetKeyReq)` (`ReadYourWrites.actor.cpp:230-243`) computes exactly the range
Go already computes in `addGetKeyConflictRange` (offset≤0 → `[result, orEqual?keyAfter(key):
key)`; else `[orEqual?keyAfter(key):key, keyAfter(result))`), then walks the **write-map**
over it and inserts a read-conflict per segment **iff** `is_unmodified_range() ||
(is_operation() && !is_independent())` (`:342`). I.e. UNMODIFIED + DEPENDENT conflict;
INDEPENDENT + CLEARED do not. This is a pure function of the write-map (independent of the
snapshot cache).

## Design

Preserve op-type without abandoning the (correct, proven) eager-value-fold. Add two fields to
`rywEntry` that capture exactly the C++ predicates:

```go
type rywEntry struct {
    value      []byte         // resolved value when present
    absent     bool           // phantom: resolved value is "no value" (a matched CAC). The
                              //   key is still an is_kv slot for getKey; getRange/Get skip it.
    dependent  bool           // DEPENDENT_WRITE: a standalone atomic resolved against a DB
                              //   base. Survives caching/folding. Drives conflict filtering.
    hasAtomics bool           // unresolved atomic chain (base unknown, or versionstamp)
    atomics    []rywMutation
}
```

Mapping to C++ predicates (the load-bearing equivalence):

| Go entry state | C++ segment | getKey segType | `is_independent` (conflict) |
|---|---|---|---|
| plain `Set` (`!hasAtomics`, !absent, !dependent) | INDEPENDENT_WRITE | `segKV` (counted, lands) | independent → no conflict |
| atomic folded over Set/cleared (`!hasAtomics`, !dependent) | INDEPENDENT_WRITE | `segKV` | independent → no conflict |
| **CAC folded over Set/cleared → cleared** (`!hasAtomics`, **absent**, !dependent) | INDEPENDENT_WRITE, no-value | **`segPhantom`** (counted, skipped) | independent → no conflict |
| standalone atomic, base unknown (`hasAtomics`, non-vstamp) | DEPENDENT_WRITE | `segUnknown`→`segKV` after read | dependent → conflict |
| standalone atomic resolved present (cached: `!hasAtomics`, **dependent**) | DEPENDENT_WRITE | `segKV` | dependent → conflict |
| **standalone CAC resolved cleared** (cached: `!hasAtomics`, **absent+dependent**) | DEPENDENT_WRITE, no-value | **`segPhantom`** | dependent → conflict |
| versionstamp (`hasAtomics`, vstamp) | INDEPENDENT_WRITE + unreadable | `segEmpty` (#234 approx — out of scope) | independent → no conflict |

The unifying rules that make both consumers correct:

- **A key touched by an atomic is NEVER moved to the `cleared` list.** A matched CAC keeps an
  entry with `absent=true`; only an explicit `Clear`/`ClearRange` creates a `cleared` range.
  (Matches C++: CAC produces an *operation* segment, not a cleared range.) This is what lets the
  conflict walk still see a standalone matched CAC as a DEPENDENT operation.
- **getKey is a limit-1 range read → a phantom is `segPhantom`: COUNTED in the offset walk
  (like `segKV`) but SKIPPED at the landing (like `segEmpty`), in the resolution direction.**
  `resolveKeySelectorFromCache` counts `segKV`+`segPhantom`; the directional skip-to-present
  (the range iteration's `kv()`-non-null scan) skips `segEmpty`+`segPhantom`. The old model
  (matched CAC → `segEmpty`) under-counted for offset>1; a naive `segKV` would wrongly land on
  the phantom. `segPhantom` is exactly C++.
- **getRange/Get resolved value is separate:** an `absent`/phantom entry is skipped from getRange
  output and reads absent in Get — *byte-for-byte the same getRange/Get result as today* (those
  keys were already absent). Only getKey's offset COUNT changes (for offset>1 selectors).

### Entry birth & transition sites — EXACT `(absent, dependent)` at each (`ryw.go`)

Every place an entry is created or rewritten, with its resulting flags and the C++ it mirrors.
"Bottom wins, and folds never change an existing entry's `dependent`" is the one rule.

| # | Site (current line) | Condition | Result entry | C++ basis |
|---|---|---|---|---|
| A | `set()` :110 | any Set (incl. over a phantom) | `{value, absent:false, dependent:false, !hasAtomics}` | SetValue **replaces** stack → INDEPENDENT (`WriteMap.cpp:125-137`, `:120`) |
| B | `atomic()` fold-over-existing :184 | `exists && !hasAtomics` | applyAtomic→(val,clr); `{value:val, absent:clr, dependent:`**`entry.dependent`**`, !hasAtomics}` (clr→value:nil) | coalesce on top; **bottom unchanged** → keep existing dependence (`WriteMap.cpp:480`) |
| C | `atomic()` over-cleared :196 | `!exists && isClearedLocked` | applyAtomic(op,nil,…)→(val,clr); `{value:val, absent:clr, dependent:false, !hasAtomics}` | synthetic `SetValue(empty)` pushed at bottom → INDEPENDENT (`WriteMap.cpp:102-111`) |
| D | `atomic()` standalone :205 | else (not over Set, not cleared) | `{hasAtomics:true, atomics:+op, value:nil}` (dependent unused while hasAtomics) | bare op stack → DEPENDENT or, if vstamp, INDEPENDENT+unreadable (`WriteMap.cpp:117`) |
| E | resolve+cache (`get()`/`mergeBatch()`) | hasAtomics, base **known**, non-vstamp | cleared→`{absent:true, dependent:true, !hasAtomics}`; present→`{value, absent:false, dependent:true, !hasAtomics}` | a hasAtomics entry is only reachable via D (standalone) ⇒ bottom is the atomic ⇒ **DEPENDENT** |
| — | resolve, vstamp/unknown | hasAtomics, vstamp **or** base unknown | **left intact** (no cache); reads absent (#234) / `segUnknown` | unreadable / not-yet-read |

Transitions on an existing **phantom** (`absent:true`) entry, all enumerated:
- **Set** → site A: `{value, absent:false, dependent:false}` (independent present). ✓
- **atomic** → site B: `applyAtomic(op, nil, param)`; recomputes `absent` from `clr` (e.g. `Add`
  over nil→operand→present `absent:false`; another `CAC`→`absent:true`), **keeps existing
  `dependent`** (a standalone-CAC phantom stays dependent; a folded-CAC phantom stays
  independent). ✓
- **Clear / ClearRange** → `clear()` :131 / `clearRange()` :147: **delete the entry from
  `writes`, add to `cleared`** (becomes a CLEARED range — `segEmpty`, no conflict). ✓ (matches
  C++ `clear` setting `following_keys_cleared` and removing the operation).

A phantom is NEVER left in both `writes` and `cleared`: a matched CAC keeps it ONLY in `writes`
(`absent:true`); an explicit Clear/ClearRange removes it from `writes` and puts the range in
`cleared`. So `segTypeAtLocked`/`conflictRangesLocked`/`mergeBatch` (all of which check `writes`
before `cleared`) see exactly one classification per key.

Independence helper (the C++ `is_independent` for an operation key, given the above invariant
that a `dependent:true` entry is never cleared-based — see "Consistency" below):
```go
func (e *rywEntry) isDependentLocked() bool {
    if e.hasAtomics { // unresolved chain: vstamp anywhere ⇒ unreadable ⇒ INDEPENDENT; else DEPENDENT
        for _, m := range e.atomics { if isUnresolvedVersionstamp(m.typ) { return false } }
        return true
    }
    return e.dependent
}
```

**Consistency (why the `dependent` bool alone is sufficient for conflict, addressing the
`following_keys_cleared` arm):** C++ `is_independent() = following_keys_cleared ||
!isDependent()`. In Go an operation key that *sits on a cleared base* is born at site C with
`dependent:false` (the synthetic-SetValue fold), and a `dependent:true` entry is ONLY born at
site D/E (standalone, which the `!exists && isClearedLocked` guard at :196 excludes from cleared
bases) — so **no `dependent:true` entry is ever cleared-based**. Thus `!isDependentLocked()`
already subsumes the `following_keys_cleared` arm for operation keys; the conflict walk handles
*pure* cleared ranges (no operation key) separately via the `cleared` list.

### Concrete edits

- `ryw.go`:
  - `atomic()`: site B writes `absent:clr` and preserves `entry.dependent` (no more
    `delete`+`addClearedRange` on a CAC match); site C stores the phantom `{absent:true,
    dependent:false}` instead of a no-op when `clr`; site D unchanged.
  - `get()` :223-269: for a `!hasAtomics` entry, `if entry.absent { return nil (absent) }` before
    treating `value` as present-empty; for a `hasAtomics` entry that resolves cleared, cache as
    site E `{absent:true, dependent:true}` (not `delete`+`addClearedRange`); resolves present →
    `{value, dependent:true}`. Versionstamp/unknown unchanged.
  - `mergeBatch()` :551-618: at the TOP of the per-write-key loop body, `if entry.absent { mark
    atomicCleared[k]=true; continue }` — this skips the phantom from `writeKVs` AND shadows any
    server-present value at `k` (closing Torvalds' regression: a pre-existing `absent` plain
    entry must NOT fall through to the `:608` else and be emitted as a phantom KV). The
    hasAtomics-resolves-cleared arm caches as site E and still marks `atomicCleared`.
  - `rywEntry` gains `absent bool`, `dependent bool`; `isDependentLocked()` as above.
- `ryw_getkey.go`:
  - `segTypeAtLocked()`: a `!hasAtomics` entry → `segPhantom` if `absent`, else `segKV`; a
    `hasAtomics` entry over a **known** base → `segPhantom` if it resolves cleared, else `segKV`;
    `segUnknown` if base unknown; `segEmpty` ONLY for the vstamp `unresolved` arm (#234). Segment
    TYPE, never resolved value.
  - `resolveKeySelectorFromCache()`: count `segKV`+`segPhantom` in both offset walks; replace the
    forward-only empty-skip with a **directional** skip-to-present (forward for offset>0, backward
    for offset≤0) over `segEmpty`+`segPhantom`, with direction-correct `readToBegin`/`readThroughEnd`
    terminals. Pass `backward` from `getKeyRYW`. (This reproduces C++ getKey = limit-1 range read:
    count `is_kv`, then return the first `kv()`-non-null key in direction.)
- `transaction.go` `addGetKeyConflictRange()`: replace the single full-range `addReadConflict`
  with a walk over `rywCache.conflictRangesLocked(begin, end)`, which partitions `[begin, end)`
  by write-key + cleared boundaries and classifies each sub-segment exactly as `updateConflictMap`
  does — **add** a read-conflict for (i) UNMODIFIED gaps (no write key, not cleared) and (ii) a
  write key with `isDependentLocked()` (single-key `[k, keyAfter(k))`); **skip** (iii) INDEPENDENT
  write keys and (iv) CLEARED sub-ranges. Adjacent conflict sub-ranges are coalesced. The walk is
  pure write-map (no snapshot cache) — exactly `updateConflictMap` iterating `WriteMap::iterator`
  (`ReadYourWrites.actor.cpp:338-350`). `Snapshot.GetKey` still adds no conflict.

## Performance

Two `bool` fields per entry (no extra allocation). `segTypeAtLocked` is unchanged in cost (it
already resolved atomics over the cached base; the classification just changed). The conflict
walk is `O(boundaries in [base, resolved])` — bounded by the write/cleared boundaries the
selector actually crosses, the same order as the getKey resolution itself. getKey hot path
(RFC-057 lazy cursor) is untouched; the directional skip is the same forward empty-skip it
already had, extended to phantoms and made direction-aware. No wire-format change.

## Test plan (the proof — this is the point of the item)

- **Sub-edge 1 — phantom slot, PROVEN against libfdb_c (done).** RE-ENABLED pending-CAC (and all
  atomics) in `runGetKeyRYWDifferential` / `FuzzDifferential_GetKeyRYW`
  (`pkg/fdbgo/bench/differential_getkey_ryw_test.go`, was excluded via `filterWriteShaping`, now
  deleted). Deterministic cases cover CAC over a committed value, over a pending Set (site B),
  over a cleared base (site C), a no-match control, and two adjacent phantoms — across the full
  selector matrix (FGE/FGT/LLE/LLT + offset±2). **The differential immediately disproved the
  RFC-056 hypothesis** (`FGT(a)` over `{a, b-phantom, c}` returns `c` in libfdb_c, not `b`),
  driving the correct `segPhantom` model (count-in-walk + skip-at-landing). It also surfaced the
  pre-existing fold-path `nil`-normalization bug (pinned as a corpus seed + the
  `max_empty_then_cac_stays_present` case). A 92k-exec fuzz burst runs clean (0 mismatches).
- **Sub-edge 2 — conflict filtering, deterministic commit-order differential (done).**
  `TestDifferential_GetKeyConflict` (`pkg/fdbgo/bench/`, modeled on `interop_test.go:478`): on
  EACH client — `A.GetKey(selector)` (pins A's read version + registers the read-conflict),
  `A.Set(sentinel)`, commit a separate txn B that writes probe-K (commit version > A's read
  version), then commit A — A fails `not_committed(1020)` iff K ∈ A's read-conflict range. Assert
  go-A's outcome == cgo-A's == the expected C++ outcome, for K on: an UNMODIFIED gap (both
  conflict), an INDEPENDENT-write key (neither), a CLEARED key (neither), a DEPENDENT-atomic key
  (both conflict — proves no UNDER-conflict, the codex #235 safety concern), and outside the span
  (neither). **Teeth verified:** with the old full-range conflict, the INDEPENDENT and CLEARED
  cases FAIL (go-A conflicts where cgo-A does not); with `conflictRangesLocked` they pass. The Go
  client does not expose `\xff\xff/transaction/read_conflict_range/`, so the commit-order race is
  the proof — fully deterministic because B commits strictly between A's read version and A's
  commit.
- **Value-preservation net (must stay green UNCHANGED):** the RFC-055 Get/GetRange differential
  + the RFC-056 getKey differential + the RFC-057 equivalence property-test. If any getRange/Get
  value changed, these fail — they ARE the spec that the fold stays value-correct.
- **Fuzz:** `FuzzDifferential_GetKeyRYW` burst (≥100k execs, 0 mismatches) with CAC shapes live.

### Out of scope (explicitly)

Versionstamp/unreadable getKey semantics (C++ `is_unreadable` halts the offset walk; Go's #234
folds versionstamp→absent and skips). That is a distinct axis (item 3 / versionstamp-offset)
and is NOT touched here — `segTypeAtLocked` keeps the versionstamp `segEmpty` arm.
