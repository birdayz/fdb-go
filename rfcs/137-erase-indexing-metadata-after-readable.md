# RFC-137 — Erase indexing metadata after an index becomes readable (Java 4.12, RFC-135 §4 R2a)

**Status:** Draft (v3 — v2 + the `SetMarkReadable` enabler surfaced during implementation)
**Item:** RFC-135 §4 **R2 part (a)** — port Java 4.12's post-`markIndexReadable` indexing-metadata
cleanup (commit `0570b368b` #4233). (R2b typed-record range preset is a separate RFC; R2c
`SlidingWindowIndexMaintainer` is N/A — Go lacks that index type; the 4.12 change there is metrics only.)
**Reviewers:** Torvalds + codex + @claude. Record-layer (online-indexer lifecycle), **no Graefe gate**.

---

## 1. Problem (verified; v1 premise corrected)

When an index finishes building, Java's online indexer (`IndexingBase`) marks it readable and then, **in
the same transaction**, calls `IndexingSubspaces.eraseAllIndexingDataButTheLockAndRangeSet(...)` to drop
the per-build bookkeeping.

**v1 of this RFC mis-scoped the gap** (Torvalds NAK). The real situation:

- Go's `MarkIndexReadableOrUniquePending` already calls `clearReadableIndexBuildData`
  (`index_state.go:203`, only on the `READABLE` transition), which already clears the **range set** and
  **all heartbeats** — faithfully matching Java's `FDBRecordStore.clearReadableIndexBuildData`
  (`FDBRecordStore.java:5091`: `IndexingRangeSet…clear()` + `IndexingHeartbeat.clearAllHeartbeats`).
  So there is **no range-set divergence** — Java clears it on `READABLE` too. (v1 wrongly claimed Java
  "preserves the range set"; that's the *method's* local contract, but the full `markIndexReadable`
  flow clears it via `clearReadableIndexBuildData`.)
- What Go genuinely **leaks** is the **scanned-records counter** (`[9, idx, 1]`) and the **type-stamp**
  (`[9, idx, 2]`). In Java these are cleared **only** by the `IndexingBase` erase call
  (`IndexingBase.java:318`) — never by `clearReadableIndexBuildData`. Go has no equivalent, so after
  every online build these two keys persist (one set per index built).

## 2. Investigation (the two Java states matter)

`eraseAllIndexingDataButTheLockAndRangeSet` (`IndexingSubspaces.java:208`) clears `scrubbing` (N/A in Go
— no index scrubbing) + scanned-records (subkey 1) + type-stamp (subkey 2) + **heartbeats** (subkey 7),
preserving the **lock** and the **range set**. Java calls it from `IndexingBase` **unconditionally**
after the mark (inside `markFuture.thenApply`), i.e. for **both** `READABLE` and
`READABLE_UNIQUE_PENDING`.

That heartbeat(7) clear is **not** purely redundant:
- On `READABLE`: `clearReadableIndexBuildData` already cleared heartbeats + range set, so the erase's
  heartbeat clear is redundant; it still clears the leaking scanned(1)+type(2). Net: 1/2/7/range-set all
  gone, lock kept.
- On `READABLE_UNIQUE_PENDING`: `clearReadableIndexBuildData` is **not** called (Go `index_state.go:199`
  and Java both keep build data until violations resolve). So the erase is the **only** thing clearing
  heartbeats here, and it clears scanned(1)+type(2)+heartbeat(7) while **keeping** the range set (still
  needed). Net: 1/2/7 gone, range set + lock kept.

Go's subkey constants match Java: `IndexBuildSpaceKey=9` (`constants.go:35`), scanned=1, type=2
(`index_state.go:375-376`), heartbeat=7 (`indexing_heartbeat.go:16`). Go writes **no** lock subkey at
all (it coordinates via heartbeats), so clearing 1/2/7 cannot touch any lock. The range set lives under
the separate `IndexRangeSpaceKey`, untouched by clearing under `[9, idx]`.

## 3. Fix

1. New `(store *FDBRecordStore) eraseAllIndexingDataButTheLockAndRangeSet(index *Index)` — a faithful 1:1
   port of Java's method: clears, via `fdb.PrefixRange` (Java's `Range.startsWith` semantics — the
   scanned counter and type-stamp are single keys at the exact subspace prefix, which `subspace.Range()`
   would miss, per `index_state.go:442-446`), the three subspaces scanned(1), type(2), heartbeat(7).
   Reuses `indexBuildTypeSubspace` / `heartbeatSubspace`. A comment records the N/A scrubbing step and
   that the range set (separate key) and lock (unwritten) are intentionally untouched.
2. `OnlineIndexer.markReadable` — inside the existing per-index `oi.db.Run` closure, **after**
   `MarkIndexReadableOrUniquePending` returns without error, call
   `store.eraseAllIndexingDataButTheLockAndRangeSet(idx)` in the same transaction (matching Java
   `IndexingBase:318`'s `thenApply`, which runs for both readable and unique-pending). The mark's
   returned `changed` is unaffected.

## 3.1 `SetMarkReadable` enabler (scope grown during impl)

Running the full suite after step 2 surfaced an interaction: 8 existing tests build to completion and
then read `LoadIndexingTypeStamp` / `LoadBuildProgress` and assert their **content** — which the new
erase wipes once the index is readable. The faithful fix (and a real Java-parity gap) is to add Java's
`OnlineIndexer.buildIndex(boolean markReadable)`: a new `OnlineIndexerBuilder.SetMarkReadable(bool)`
(default **true**), gating the `markReadable(ctx)` call in `BuildIndex` and `buildIndexMutual`. With
`false`, the build populates the index but leaves it WRITE_ONLY with its stamp + counter intact (for
inspection or resume) — and, because it never marks readable, never erases. The 8 tests now read the
stamp/counter while WRITE_ONLY (3 that also need a readable index — the vacuum test and two
resume/scan tests — mark readable directly via `store.MarkIndexReadableOrUniquePending`, which clears
the range set + heartbeats but **not** the type-stamp, so the stamp survives for their read/vacuum). No
test assertion was weakened.

## 4. Performance

Three `ClearRange`s per index, once, at end-of-build (cold path). Removes per-build keys that otherwise
persist. No read/write/scan hot-path effect.

## 5. Wire / behaviour impact

Brings Go's post-build store state to Java 4.12 parity: the scanned-records counter and type-stamp are
now erased (they leaked before). Range set + heartbeats were already handled by the existing
`clearReadableIndexBuildData` on `READABLE`; on `READABLE_UNIQUE_PENDING` the new erase now also clears
heartbeats (Java parity) while keeping the range set. **Safe:** erase happens only *after* the index is
readable/unique-pending (build bookkeeping is dead or, for unique-pending, the range set is explicitly
kept). The index entries, state, lock, and (for unique-pending) the range set are untouched. No
record/index-entry persisted bytes change — only transient build metadata Java already discards.

## 6. Test plan

- **FDB integration test** (`online_indexer` suite): build an index to `READABLE`; assert (a) it is
  readable + queryable, (b) the scanned-records key `[9, idx, 1]` and type-stamp key `[9, idx, 2]` are
  **absent** after the build. **Revert-proof:** with the new erase call removed, those two keys remain →
  the test fails, pinning exactly the leak this RFC closes.
- A `READABLE_UNIQUE_PENDING` case (build a unique index over data with a duplicate): assert scanned(1) +
  type(2) + heartbeats absent but the **range set is still present** (kept until violations resolve) —
  pinning the two-state behaviour and that the erase does not wipe the range set.

## 7. Scope

One commit on the RFC-135 branch (PR #336): the new erase method + the `markReadable` call + the FDB
tests. R2b and R3–R8 remain separate.
