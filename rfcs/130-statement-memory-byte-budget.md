# RFC-130 — Statement-wide memory byte budget (RFC-106b)

**Status:** Implemented (PR #328 — Graefe + Torvalds + codex + @claude ACK; 1M stress no-regression)
**Item:** prod-readiness-audit-2026-06-19.md **P1** — "No Full Statement-Wide Memory Byte Budget Yet"
(the `RFC-106b` follow-up deferred from RFC-106a).
**Reviewers:** Graefe (executor/Cascades alignment) + Torvalds (code quality) + codex + @claude.
Query-engine change → Graefe ACK required before merge.

> **This is a Go-side extension, not a parity port.** Java byte-bounds bytes *read from FDB*
> (`ByteScanLimiter`, `FDBRecordStore.java:1049`) but byte-bounds *buffering* **nowhere** — `asList()` is
> unbounded (`RecordCursor.java:285`), in-memory sort is row-bounded (`MemorySortAdapter.java:90`), the
> PK-distinct seen-set is unbounded (`RecordQueryUnorderedPrimaryKeyDistinctPlan.java:100`). So this budget
> is a new Go capability; wire compat is untouched (read-side; opt-in; `0` = today's behaviour).

---

## 1. Problem (verified)

Cardinality-growing buffers in `pkg/recordlayer/query/executor/` are bounded by **row count**, not **bytes**.
`MaterializationLimit` (default **100,000 rows**, `pkg/recordlayer/scan_properties.go:165/168`) bounds the
shared `CollectAllBounded` (`pkg/recordlayer/query/executor/executor.go:2908`) — but (a) it's rows not bytes,
so 100k *large* rows still OOMs, and (b) several buffers accumulate **across** `CollectAllBounded` calls or
don't use it at all (see §3). RFC-106a's `maxResultBytes` caps *egress* bytes only
(`pkg/relational/core/embedded/cascades_generator.go:1063-1072`), not internal buffers.

## 2. Design — the budget lives in a Go `ExecuteState`, charged through accounted buffers

### 2.1 `ExecuteState` (the Java-faithful home — Graefe NAK fix)

Java keeps the mutable, statement-scoped counters in `ExecuteState` (`ExecuteState.java:44-47`), held **by
reference** inside the *immutable* `ExecuteProperties` (`:69-70`); `clearSkipAndLimit` (`:240-245`) preserves
it for free because it only zeroes skip/rowLimit. Go's `ExecuteProperties` is a value struct with **no**
such state object. Introduce the analog (in `pkg/recordlayer`):

```go
type ExecuteState struct { memUsed, memLimit int64 }       // memLimit<=0 == unlimited
func (s *ExecuteState) ChargeMemory(n int64) error {        // nil receiver == no budget == no-op
    if s == nil || s.memLimit <= 0 { return nil }
    s.memUsed += n
    if s.memUsed > s.memLimit { return &MemoryLimitExceededError{Used: s.memUsed, Limit: s.memLimit} }
    return nil
}
```

`ExecuteProperties.State *ExecuteState` — a pointer, so every value-copy of `ExecuteProperties` (the `WithX`
helpers, `ClearSkipAndLimit`, per-operator `innerProps`) **shares one counter**, and none of them reset it:
statement-wide survival is **structural**, exactly as Java's `ExecuteState`. The state is **always minted**
once per statement (in the `ExecutePlan`/`paginatingRows` setup) — **never nil** — with `memLimit<=0`
(unlimited) when the option is unset, and a fresh statement gets a fresh state. **The "no budget" case is
`memLimit<=0`, NOT a nil state** (Torvalds: a nil-`st` no-op would make a missed accumulation site an
*invisible* bypass; an always-present state makes a missed site a *charge*, not a silent skip). (This
`ExecuteState` is also the correct future home for the scan/byte/time *counters* Go presently tracks
per-cursor — out of scope here, noted for the divergence ledger.)

### 2.2 Concurrency invariant (Torvalds NAK fix)

The executor is **single-threaded per statement** — grep confirms **zero** `go ` / goroutine launches in
`pkg/recordlayer/query/executor/`. `ChargeMemory` therefore needs no mutex/atomic. This invariant is
**load-bearing** and pinned: a `package_invariant_test.go` greps the executor package for goroutine launches
and fails if one appears (so a future parallel-union forces a revisit to atomic counters, instead of a
silent data race).

### 2.3 Emergent accounting via accounted buffers (Torvalds/Graefe "no-bypass" fix)

A grep-lint is theatre. Instead, two generic accounted containers in the executor package, and **every**
cardinality-growing buffer uses one — accounting is then a property of the type, not a reviewer's vigilance
(CLAUDE.md principle #10):

```go
type boundedBuffer[T] struct { items []T; st *ExecuteState }   // Append charges sizeof(item) before keeping it
type boundedSet[K]    struct { m map[K]struct{}; st *ExecuteState } // Add charges on a NEW key only
```

`boundedBuffer.Append` and `boundedSet.Add` call `st.ChargeMemory(estimate)` and propagate the error. They
also subsume the existing `MaterializationLimit` row check, so a buffer can't exist without both bounds.

**No silent bypass (Torvalds):** the constructors (`newBoundedBuffer`/`newBoundedSet`, and `TempTable`'s)
take `*ExecuteState` **non-optionally** — combined with §2.1's always-present state, a buffer literally
cannot be constructed without the accountant, so a missed wiring is a compile/test failure, not a nil-no-op.
A `charge_coverage_test.go` drives one row through each of the eight buffer paths and asserts the shared
`ExecuteState.memUsed` advanced — pinning that every path actually charges.

### 2.4 Complete buffer survey (the §3 the reviewers required)

Every cardinality-growing buffer, charged at the point of accumulation:

| Site | file:line | container |
|------|-----------|-----------|
| `CollectAllBounded` (union branch, NLJ inner, DML target sets, recursive-CTE per level, DFS) | `executor.go:2908` | `boundedBuffer` |
| Buffered-UNION cross-branch `all` slice | `executor.go:1405` | `boundedBuffer` |
| NLJ hash-index map | `streaming_cursors.go:656` | `boundedSet`/`boundedBuffer` |
| In-memory sort buffers (×2) | `streaming_cursors.go:452, :548` | `boundedBuffer` |
| `distinct` seen-set | `executor.go:1110` | `boundedSet` |
| Recursive-CTE per-level working set + `seen` | `TempTable.Add` + `boundedSet` | charged **once** at `TempTable.Add` (the recursive sub-plan's `TempTableInsertPlan` top); the level-union drains use a **non-charging** row-capped collect (`collectAllRowCapped`) to avoid double-counting the same shared records — see §2.7 |
| Recursive-DFS `*results` (cross-traversal) + `seen` | `executor.go:2765/2796` | `boundedBuffer`+`boundedSet` |
| `executeLoadByKeys` full-record slice (`FromList`) | `executor_new_plans.go:230` | `boundedBuffer` (Graefe: bounded by a plan-literal key count, but each element is a whole stored record → bytes can grow) |
| `TempTable.list` — the recursive-CTE `insertTable`/`scanTable` working set **and** `TempTableInsertPlan` (`evaluation_context.go:133`, `Add` at `:145`; recursive-CTE use at `executor.go:2584-2585`) | charge in `TempTable.Add` (Torvalds: the actual cross-level per-level working set) |
| DML `results` echo slice — one (often full `FromStoredRecord`) `QueryResult` per *mutated* row | DELETE/INSERT/UPDATE in `executor.go` | INSERT/UPDATE: **build-all-then-save**, charging actual `proto.Size` before any write; DELETE: **not** charged (echo = already-charged target) — see §2.7 |

Verified **streaming → no buffer, no charge**: aggregate (one group key + running aggregates,
`streaming_cursors.go:46`), intersection (per-key match-list O(children)), IN-join/IN-union (lazy cursor
slices, not row buffers). **Bounded-by-cardinality, charged anyway for bytes**: `scalar_subquery.go:51`
(`rows` drain capped at >1 → error, so already byte-bounded — listed for completeness, folded into
`boundedBuffer`). The cross-level recursive totals are covered because each `Append`/`Add` charges the
*shared* `ExecuteState`, so 1000 levels × big rows trips the budget even though each level's row count is
individually under `MaterializationLimit`.

`TempTable` (the recursive-CTE working set + `TempTableInsertPlan`) carries an `*ExecuteState` and charges in
`Add`; its pre-existing `sync.Mutex` is defensive (the §2.2 zero-goroutine invariant makes it currently
moot, and charging under that lock is correct regardless — if the executor ever goes concurrent, the pinned
invariant test fires and `ChargeMemory` moves to `atomic`).

### 2.5 Byte estimate (Torvalds layering + nil-safety fix)

`estimateRowBytes` lives in package `embedded` and the executor **cannot** import the relational layer
(`query_result.go:18`), so `estimateQueryResultBytes(QueryResult) int64` is written **fresh** in the executor
package. It must handle every `QueryResult` shape without panic:
- stored row: `Record != nil` → `Record.Record.Size()` (proto wire size) + `len(PrimaryKey-encoded)`;
- computed row: `Record == nil`, `Datum` is a map/value → sum approximate value sizes; `Datum == nil` → a
  small constant. Approximate by design (a ceiling signal, not exact heap).

### 2.6 Option + error

`OptMaxStatementMemoryBytes` (`pkg/relational/api/options.go`), default `0` = unlimited.
`MemoryLimitExceededError` → SQLSTATE **`54F01`** (`ErrCodeExecutionLimitReached`) — same resource-limit
family as scan/byte/time (no memory-specific SQLSTATE in Java either; Graefe ACK'd reuse).

### 2.7 Implementation refinements (impl-review findings, PR #328)

Two charge-once/safety subtleties the impl gauntlet surfaced (the two primary reviewers
missed both; the `/code-review` finder + codex caught them):

1. **No double-charge in recursive CTE** (`/code-review`). The recursive sub-plan's top is a
   `TempTableInsertPlan`, which charges each row in `TempTable.Add`; the level-union then
   drained that *same* cursor through `CollectAllBounded`, charging the shared record again →
   the budget tripped at ~half its true value. `TempTable.Add` is the **sole owner**
   (`memUsed` is monotonic and survives the per-level `Clear`, so the cumulative charge =
   true peak residency). The initial/recursive level drains use `collectAllRowCapped` (row
   cap, **no** byte charge); the DFS join drains keep charging (plain plans, no
   `TempTableInsert`). Pinned by `TestFDB_RFC130_RecursiveCTE_NoDoubleCharge`.

2. **DML echo charged by actual built-record size, before any write** (codex, 3 rounds). The
   DELETE/INSERT/UPDATE result echo (one record per mutated row) was first charged *after*
   `SaveRecord` staged a write — and `runInTx` does **not** roll back on a statement error, so
   a mid-loop `54F01` could persist a **partial mutation**. The fixes converged on:
   - **DELETE** — the echo *is* the already-charged target row, so it is **not** re-charged
     (re-charging double-counts the same shared `QueryResult`).
   - **INSERT/UPDATE** — the echo is genuinely new memory. Charging an *estimate over the
     source/target* up front (an intermediate fix) under-counts a **growing** DML (INSERT …
     VALUES with a large literal whose datum is a wrapped value → `scalarValueBytes` flat 8;
     UPDATE SET to a large value — the new bytes aren't in the old target), letting it bypass
     the cap. Final design: **build-all-then-save** — `executeInsert`/`executeUpdate` build
     **every** record in phase 1, charging its **actual `proto.Size`**, then save all in
     phase 2. Charging the real built record accounts the true echo bytes; doing all charging
     before the first `SaveRecord` keeps the mutation **all-or-nothing** (a budget breach *or*
     a build/transform error returns with zero writes). Wire-equivalent — same records, same
     order; the built messages *are* the echo (no extra residency). The charge is the full
     stored-row estimate — `proto.Size(record)` **plus** the packed primary-key tuple bytes
     (`len(PK.Pack())`) the echo's `FDBStoredRecord` holds separately — matching
     `estimateQueryResultBytes` exactly (UPDATE uses the target's PK; INSERT derives it from
     the built record via the target type's primary-key expression).

   Pinned by `TestFDB_RFC130_DeleteEchoNotDoubleCharged`, `…_UpdateEchoChargedNoPartial`
   (growing case: tiny rows → 600B value trips on the large new records, zero mutated —
   revert-proven against charging the original record), `…_InsertEchoChargedNoPartial`.

## 3. Executable spec (tests)

1. **Byte budget trips before the row-count limit:** N wide rows whose bytes exceed the budget but N <
   `MaterializationLimit` → `54F01`. Revert-proven (no charge → rows returned).
2. **Statement-wide accumulation across operators:** a query whose plan buffers in **two** distinct sites
   (e.g. buffered-union of two NLJ-inner materializations) trips when the **sum** exceeds the budget, not
   per-site — proves the shared `ExecuteState`.
3. **Cross-level recursive accumulation:** a recursive CTE where no single level exceeds the budget but the
   accumulated `allResults` does → `54F01` (the bug the row-count limit misses).
4. **Default unlimited:** option unset → large buffers succeed exactly as today (regression guard).
5. **Estimate sanity:** `estimateQueryResultBytes` within a sane factor for a stored record and a computed
   row; no nil-panic on `Record==nil`/`Datum==nil`.
6. **Single-threaded invariant** (§2.2): the goroutine-grep test.

## 4. Wire/behaviour impact

**None by default** (opt-in; `0` = current behaviour). No persisted bytes, no plan shape. When set, a
previously-OOM-risking query fails fast with `54F01` — strictly safer.

## 5. Scope

One coherent PR: `ExecuteState` + the two accounted containers + the estimate + threading the option +
charging the seven sites + tests. Migrating Go's per-cursor scan/byte counting into `ExecuteState` (to fully
match Java) is a **separate** divergence-ledger item, not this RFC.
