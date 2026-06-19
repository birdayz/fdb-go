# RFC-125 — ConflictRange workload: a two-directional read-conflict-range oracle on key-selector getRange

**Status:** Draft
**Item:** TODO.md C3 ("Ride their test designs"), increment 6 — the **ConflictRange** workload
(RFC-119 §7 named gap: "concurrent read/write race-detection scenario"). Builds on the RFC-121
Get/GetRange read-conflict clamp + RYW filter; this is the cross-engine validation of that work
across the full key-selector offset/onEqual/reverse/limit space.
**Spec:** `fdbserver/workloads/ConflictRange.actor.cpp` @ 7.3.75.
**Conflict-range generation cited:** `fdbclient/ReadYourWrites.actor.cpp` @ 7.3.75
(`addConflictRange(GetRangeReq<false>)` :246-281, `addConflictRange(GetRangeReq<true>)` :285-319,
`addConflictRange(GetKeyReq)` :230-243, `updateConflictMap` :334-351).
**Test-only — expected zero production/wire impact** (see §6 for the one exception: an under-conflict
finding would be a real serializability bug in Go, fixed in this increment). Differential-vs-libfdb_c
gates N/A (this rides FDB's own oracle, not the byte differential).

---

## 1. Problem — the unprobed dimension

RFC-119 §7's coverage audit put ConflictRange's gap at "concurrent read/write race-detection
scenario." The existing coverage pins the *write* side and the *single-key* read side:
`atomic_conflictrange_test.go` (per-op **write**-conflict-range vector), `getkey_conflict_unit_test.go`
/ `getrange_conflict_unit_test.go` (RFC-121's **read**-conflict clamp, unit-level), and
`retry_adversarial_test.go` (self-conflicting predicate on commit_unknown). **None** drives a
concurrent writer against a key-**selector** getRange and checks the resolver's verdict against ground
truth.

FDB's ConflictRange workload is exactly that oracle. It is a precise, **two-directional** test of the
read-conflict range generated for a `getRange(beginSelector, endSelector, limit, reverse)`:

- **No under-conflicts (the serializability teeth).** If a reader at version `v` did *not* conflict
  with a writer that committed after `v`, then re-reading the same query at a fresh version must return
  the *same* result. A differing result with no conflict = a **missed conflict** = a serializability
  violation. **This direction is a hard bug for any client, regardless of how it generates conflicts.**
- **No over-conflicts beyond documented conservative cases (the precision teeth).** If the reader *did*
  conflict, the result *should* have changed — unless the conflict falls into one of a small set of
  documented limit/offset/boundary cases where the conflict range is conservatively wider than the
  visible result (C++ enumerates them, `ConflictRange.actor.cpp:273-305`).

This is the dimension RFC-121 fixed mechanically (clamp the range conflict to returned data; filter
through the RYW write-map) but never validated against a live concurrent writer across the selector
space. The workload is that validation.

## 2. The C++ design (cited, `ConflictRange.actor.cpp` @ 7.3.75)

Single client (`clientId == 0`, `:67`) loops for `testDuration`:

1. **Reset** (`:104-141`). Clear `[%010d(0), %010d(maxKeySpace))`; for each `i ∈ [0,maxKeySpace)`,
   with p≈0.5 `set(%010d(i), randomUID)` (recorded in `insertedSet`). Set a **sentinel** at
   `%010d(maxKeySpace)` (`:98,134`) — "one key after the end of the tested range; if a result includes
   it we may have drifted into system key-space and cannot evaluate" (`:96-97`).
2. **Pick a non-empty query** (`:154-174`). Random `myKeyA`, `myKeyB ∈ [0,maxKeySpace)`, random
   `onEqualA/B`, random `offsetA/B ∈ [-maxOffset, maxOffset]`, random `limit ∈ [1, maxKeySpace)`,
   random `reverse`. Run on `tr1`; retry (fresh `tr1`) until it returns ≥1 row; save `originalResults`.
3. **Pin two transactions to one version** (`:184-190`). `readVersion = tr2.getReadVersion()`;
   `tr3.setVersion(readVersion)`. So `tr3` reads as-of `readVersion` (before `tr2` commits).
4. **The concurrent writer** (`:192-221`). On `tr2`, `randomInt(min,max)` ops, *either* all sets to
   empty slots (`randomSets`) *or* all clears of filled slots (alternating each loop, `:101`). Commit
   `tr2`.
5. **The pinned reader** (`:223-243`). On `tr3` (pinned, sees pre-`tr2` data): a dummy
   `clear(%010d(maxKeySpace+1))` so `tr3` is **not read-only** (a read-only txn never conflicts), then
   the **same** selector-getRange (this generates `tr3`'s read-conflict range), then `commit()`. Catch
   `not_committed (1020)` → `foundConflict = true`; rethrow anything else.
6. **The oracle.**
   - **`foundConflict`** (`:250-343`): re-read the query on `tr4` (fresh version, sees `tr2`).
     `withConflicts++`. If `res.size() != originalResults.size()` → results changed → conflict
     justified, OK. If sizes equal: compare element-wise; any difference → justified, OK. **If all
     elements equal** → the conflict didn't change the visible result; this is an error **unless** one
     of the documented conservative cases applies (`:273-305`):
     - `originalResults.size() == limit && ((offsetB ≤ 0 && !reverse) || (offsetA > 1 && reverse))`
       — hit the limit but the end-side offset reaches into the range, so a change *could* matter even
       though here it didn't (`:273-278`).
     - `largestResult >= sentinelKey` — results reach into server keyspace; an unresolved offset
       there can't affect results (`:286-290`).
     - `smallestResult == firstElement && offsetA < 0` — results include the first element and the
       begin offset is negative; an unresolved offset can't affect results (`:292-298`).
     - `(myKeyA > myKeyB || (myKeyA==myKeyB && onEqualA && !onEqualB)) && size == limit` — begin>end,
       so the change only affects the end selector, but the limit masks it (`:300-305`).
     None apply → `SevError "Conflict returned, however results are the same"` (`:317`).
   - **`!foundConflict`** (`:344-393`): re-read on `tr4`. If sizes differ → `SevError "No conflict
     returned, however result sizes do not match"` (`:376`). If sizes equal but any element differs
     (and not both `\xff`-prefixed) → `SevError "No conflict returned, however results do not match"`
     (`:357`). **This is the under-conflict (missed-conflict) error.**

**How the conflict range is generated (the spec the oracle validates).** For a forward selector
getRange, `addConflictRange(GetRangeReq<false>)` (`ReadYourWrites.actor.cpp:246-281`) computes
`[rangeBegin, rangeEnd)` from the **selector base keys** (`.getKey()`), then extends by the returned
data and the `more`/`readToBegin`/`readThroughEnd` flags:

```
rangeBegin = begin.getKey()            // base key of the begin selector
rangeEnd   = (end.offset>0 && more) ? begin.getKey() : end.getKey()
if readToBegin && begin.offset<=0:  rangeBegin = allKeys.begin
if readThroughEnd && end.offset>0:  rangeEnd   = getMaxReadKey()
if result.size():
    if begin.offset<=0:  rangeBegin = min(rangeBegin, result[0].key)
    if rangeEnd <= result.back().key:  rangeEnd = keyAfter(result.back().key)
```

(`GetRangeReq<true>` = reverse is the mirror, :285-319.) The documented exception cases above are
exactly the conditions under which this range is conservatively wider than the visible-result delta.

## 3. Go's architecture — and why this is a genuine oracle, not a foregone pass

**Go does not resolve a selector range server-side.** C++ ships both `KeySelector`s in one
`GetKeyValuesRequest`; the storage server resolves them atomically and the client adds the single
combined `[rangeBegin, rangeEnd)` above. **Go's facade** (`fdb/range_result.go:resolveRange` →
`resolveSelector`) resolves each non-trivial selector with a separate `GetKey` round-trip, then issues
`getRange` over the **resolved** `[begin, end)` (whose request always carries trivial
`firstGreaterOrEqual` selectors, `readpath.go:935-936`). So Go's read-conflict coverage for a selector
getRange is the **union of three** independently-generated conflicts:

| Source | Conflict added | Cited port |
|--------|----------------|------------|
| `GetKey(beginSel)` (non-trivial) | resolution span around the begin selector | `addGetKeyConflictRange`, `transaction.go:973` ← C++ `addConflictRange(GetKeyReq)` :230 |
| `GetKey(endSel)` (non-trivial) | resolution span around the end selector | same |
| `getRange(resolvedBegin, resolvedEnd)` | `[resolvedBegin, resolvedEnd)` clamped to returned data + RYW-filtered | `rangeConflictExtent`, `transaction.go:1059` (RFC-121) |

Two facts make the union's correctness **non-obvious** — i.e. worth an empirical oracle:

1. **Trivial-selector fast paths skip `GetKey`** (`resolveSelector`, `range_result.go:299-322`):
   `firstGreaterOrEqual(k)` (offset 1, !orEqual) and `firstGreaterThan(k)` (offset 1, orEqual) resolve
   client-side with **no `GetKey` and therefore no getKey conflict** — only the getRange conflict on
   the resolved key covers them. With `offset ∈ [-maxOffset,maxOffset]` uniform, `offset==1` lands
   ~9% of selectors on this path; the other ~91% take the `GetKey` path. The workload exercises both.
2. **The union vs the combined range.** Heuristically the union ⊇ C++'s combined `[rangeBegin,
   rangeEnd)` (the getKey resolution-span conflicts cover the offset-spill zones C++ folds into
   `rangeBegin/rangeEnd`), so Go is expected to be **safe (over-, never under-conflict)** — consistent
   with RFC-121's finding ("SAFE over-conflicts — Go aborted where C/Java committed, never the
   reverse"). But ⊇-by-inspection is not proof across the full offset/orEqual/reverse/limit/`more`
   cross-product. **The workload is the proof.** If it surfaces an under-conflict, Go's selector-range
   conflict composition has a real hole (see §6).

## 4. Proposed Go change (test-only) — `pkg/fdbgo/client/conflictrange_workload_test.go`

A faithful port of the **non-RYW** variant (`testReadYourWrites = false` — the default and the core
read-conflict-range oracle). The RYW variant is a noted follow-up (§7).

### 4.1 Isolation: guard keys (the one faithful deviation, forced by `t.Parallel`)

C++ owns the whole simulated cluster, so a selector that resolves past the data via a negative/positive
offset hits `allKeys.begin` / system keyspace (handled by `readToBegin`/`readThroughEnd` + the
sentinel). Our testcontainer is **shared across parallel tests**, so an unbounded resolution could
drift into a neighbor subspace and read another test's keys — both a correctness hazard and a
flake source (CLAUDE.md: no flakes). The port keeps every selector resolution inside one unique
prefix by planting **always-present guard keys** at both ends:

- Data keys: `prefix + %010d(i)`, `i ∈ [0, maxKeySpace)`, each present with p≈0.5.
- **Floor guards:** `prefix + %010d(i)`, `i ∈ [-maxOffset, -1]` (`%010d` renders negatives as
  `-00000000N`, which sort *before* `0000000000`), always present.
- **Ceiling guards + sentinel:** `prefix + %010d(i)`, `i ∈ [maxKeySpace, maxKeySpace+maxOffset]`,
  always present; `i == maxKeySpace` is the sentinel.

Selector base keys are in `[0, maxKeySpace)` and offsets in `[-maxOffset, maxOffset]`, so **every**
resolution lands within `[prefix+%010d(-maxOffset), prefix+%010d(maxKeySpace+maxOffset)] ⊂ prefix` —
never `readToBegin`/`readThroughEnd` to the global boundary, never into a neighbor. The guards are
**never written by `tr2`** (which only touches `[0,maxKeySpace)`), so they appear identically in every
read and are inert to the oracle. The two-sided sentinel guard (`smallest <= floorSentinel` /
`largest >= ceilSentinel`) generalizes C++'s single top sentinel: a result touching the guard band is
"drifted to the boundary, can't evaluate" → skip (mirrors `:286-290`).

### 4.2 The dance (per iteration, single goroutine — the workload is single-client)

Built on raw `db.CreateTransaction()` (not `db.Transact`) so the tr1/tr2/tr3/tr4 choreography,
`GetReadVersion`/`SetReadVersion` pin, explicit `Commit`, and the `not_committed` catch are exact.
Each iteration:

1. `reset()` — clear the data band, set ~50% data keys + all guards + sentinel, commit; record
   `insertedSet`.
2. Pick a random selector query; run on `tr1` (fresh `tr1` per retry) until non-empty → `original`.
   Skip the iteration if `original` touches the guard band (two-sided sentinel).
3. `rv := tr2.GetReadVersion(); tr3.SetReadVersion(rv)`.
4. `tr2`: random sets-to-empty *or* clears-of-filled (alternating `randomSets`); commit.
5. `tr3`: dummy `Clear(prefix+%010d(maxKeySpace+1))`; the same selector-getRange; `Commit`. Catch
   `not_committed` → `foundConflict`.
6. Oracle (§4.3).

Selector resolution in the test mirrors `resolveSelector` **exactly** (including the two trivial
fast paths) so it drives the real production conflict-generation code (`tx.GetKey` + `tx.GetRange`)
on the same paths the facade uses. A `t.Parallel` client test cannot import the `fdb` facade (import
cycle: `fdb` imports `client`), so the resolver is a small test helper cross-referenced to
`range_result.go` to prevent drift.

### 4.3 The oracle — under-conflict fatal, over-conflict safe-but-counted

The asymmetry is deliberate and matches the established RFC-121 stance (under = bug, over = safe):

- **`!foundConflict && resultsChanged` → `t.Fatalf` (under-conflict).** The serializability teeth.
  Robust to Go's conflict-generation architecture: it asserts only "if you didn't conflict, your read
  was still valid." `resultsChanged` = size differs, or any element differs ignoring pairs where both
  keys are `\xff`-prefixed (`:355-356`). Revert-proven by temporarily widening the read-version pin or
  narrowing a conflict range.
- **`foundConflict`** → `withConflicts++`. If `resultsChanged`, the conflict was clearly justified. If
  **not** changed: evaluate the four documented C++ exceptions (§2). If an exception applies, it's an
  expected conservative conflict. If **none** applies, this is a pure-precision **over-conflict** —
  **safe for Go** (aborted where it could have committed; never a correctness defect, per RFC-121).
  The port **logs** it with full selector detail and **counts** it (`overConflictUnexplained`) but does
  **not** fail: Go's getKey-then-range union is *architecturally* wider than C++'s combined resolution,
  so asserting C++-precision parity would assert a property Go deliberately does not have (and RFC-121
  explicitly accepted). A surprising count is a signal to investigate precision, not a red test.
- **Anti-vacuity:** assert `withConflicts > 0 && withoutConflicts > 0` — both detection directions
  were actually exercised (not trivially always/never conflicting). Plus a per-run floor on iterations
  that produced a non-empty `original`.

### 4.4 Determinism / flake-freedom

Seeded RNG → reproducible. **No timing assertions** — the conflict verdict is decided by read-version
ordering (the resolver), not the clock; `tr3`'s pin guarantees the race outcome is a pure function of
the conflict range vs `tr2`'s writes. Fixed iteration count (sized to clear the anti-vacuity floor with
headroom, reported in the PR), bounded keyspace, guard-key isolation. Retryable errors from any txn go
through `OnError` and retry the iteration (faithful to the workload's outer catch, `:394-404`); a
non-`not_committed`, non-context error surfacing is a real failure (`t.Fatalf`).

## 5. Executable spec (what the test proves)

1. **Serializability (hard):** across the full random selector/offset/onEqual/reverse/limit space,
   *every* non-conflicting pinned read returns a result identical to a fresh re-read — i.e. Go's
   selector-range read-conflict composition has **no under-conflict** (no missed conflict).
2. **Both directions exercised:** `withConflicts > 0 && withoutConflicts > 0`.
3. **Precision characterization (soft):** the count of unexplained over-conflicts (conflicts whose
   visible result didn't change and no documented exception applies) is reported; expected low/zero,
   logged for inspection, non-fatal.

## 6. Wire-compat impact

**None expected — test-only.** No production code path changes.

**The one exception, stated up front:** if spec #1 fails (an under-conflict surfaces), Go's
selector-range conflict composition is missing a conflict that libfdb_c/Java would generate — a real
serializability bug. Per the skill (C++ is the spec) and CLAUDE.md (fix bugs as you find them, DFS),
that is **fixed in this increment**: root-cause against the C++ `addConflictRange` overloads, fix the
Go conflict generation to cover the gap, and pin it with both the workload and a focused deterministic
regression. Such a fix touches read-conflict-range bytes in the commit request (resolver-visible, not
persisted) → it would carry full FDB-C-dev design review as its own commit within the PR.

## 7. Follow-ups

- **RYW variant** (`testReadYourWrites = true`, `ConflictRange.actor.cpp:110-117, 176-182, 226-234,
  253-258`): same oracle but with same-transaction clears interacting with the read-conflict
  generation (the RYW write-map filter, `updateConflictMap` :334-351). A second sub-increment; Go's
  RYW conflict-filter path has separate unit coverage (RFC-121 D2), so the non-RYW core lands first.
- FuzzApiCorrectness (property-based multi-txn) gap — its own increment (the last C3 item).
