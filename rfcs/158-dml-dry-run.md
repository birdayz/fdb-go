# RFC-158: Wire DML `OPTIONS (DRY RUN)` through to the dry-run store primitives

**Status:** Draft
**Gate:** Graefe (executor execution-semantics) + Torvalds + codex + @claude
**RFC review:** Graefe ACK — "faithful to Java's `setDryRun → plan-branch` architecture, correct
layer, correct existence-check parity." Two impl conditions (folded into Fix + Test plan):
(G1) the DELETE echo must keep the `if deleted` filter — only would-be-deleted PKs echo (Java's
`.filter(isDeleted -> isDeleted)`); (G2) the UPDATE/INSERT echo must be built from the record
`DryRunSaveRecord` returns (`FromStoredRecord(stored)`), never a real save.
Torvalds ACK (v2) — verified the per-statement channel (no sticky / never-fires hazard) and that
DML plans are never cached (`PlanCacheSkip`, no cache-collision back door). One added test (T1):
DRY RUN inside an explicit `BeginTx`/`COMMIT` (`respectActiveTx`) stages nothing across COMMIT.

## Problem

`<INSERT|UPDATE|DELETE> ... OPTIONS (DRY RUN)` is **rejected** today
(`cascades_generator.go:693`, `ErrCodeUnsupportedQuery` "DRY RUN is not supported"). That reject is
a fail-closed stopgap for a former **data-loss** bug — the option used to be silently ignored and
the statement ran the *real* mutation (`DELETE ... OPTIONS (DRY RUN)` wiped the matching rows, the
exact opposite of intent).

Java **honors** DRY RUN: `AstNormalizer.visitQueryOptions` sets `Options.Name.DRY_RUN`, threaded at
execution into `ExecuteProperties.setDryRun` (`QueryPlan.java:435`), and the data-modification plans
(`RecordQueryAbstractDataModificationPlan` + Insert/Update/Delete) branch to
`dryRunSaveRecordAsync` / `dryRunDeleteRecordAsync` and return the would-be-affected rows **without
committing**. Conformance principle: a Java-supported feature unimplemented in Go → **port it** (not
a Go extension).

## Investigation

Go already has every piece except the wiring:

- **Option**: `OptDryRun` exists (`api/options.go:80`, default false `:151`).
- **Primitives**: `DryRunSaveRecord(record, existenceCheck RecordExistenceCheck)` (`store_api.go:233`,
  "performs all save validation … without actually writing", matches `dryRunSaveRecordAsync`) and
  `DryRunDeleteRecord(pk) (bool, error)` (`store_api.go:353`, matches `dryRunDeleteRecordAsync`).
  Crucially `DryRunSaveRecord` takes the **same** `RecordExistenceCheck` as `SaveRecordWithOptions`,
  so each mutation site branches with identical validation semantics.
- **Threading channel**: the executor already passes `props recordlayer.ExecuteProperties` into
  `executeDelete`/`executeInsert`/`executeUpdate`. `ExecuteProperties` (`scan_properties.go:131`)
  has **no `DryRun` field yet** — the one structural gap.
- **Build site**: `paginatingRows.executeProps()` (`cascades_generator.go:1408`) constructs the
  per-page `ExecuteProperties` from `DefaultExecuteProperties()` and **connection-scoped**
  `r.conn.Options()` — those are JDBC-style SET options (scan limits, `MAX_ROWS`), NOT the SQL
  statement's `OPTIONS` clause. The statement-scoped values (`maxRows`, `maxResultBytes`,
  `execState`) are instead carried as **`paginatingRows` fields** set once at construction
  (`cascades_generator.go:1032-1055`). DRY RUN is statement-scoped, so it must ride that channel —
  **not** `c.Options()` (Torvalds NAK, below).
- **Mutation sites** (the `dryRunSave/Delete` branch points):
  - DELETE — `executor.go:2202` `store.DeleteRecord(pk)`
  - INSERT — `executor.go:2311` `store.SaveRecordWithOptions(msg, RecordExistenceCheckErrorIfExists)`
  - UPDATE — `executor.go:2474` `store.SaveRecordWithOptions(msg, …ErrorIfNotExistsOrTypeChanged)`
- **Option not yet parsed into `api.Options`**: today the SQL `OPTIONS` clause is only inspected
  via `dmlHasDryRunOption(dmlOpts)` for the reject; it is not lowered into the `api.Options` that
  reach `executeProps()`. So step 1 below must lower it (Java's `visitQueryOptions`).

## Fix

> **Torvalds NAK (RFC v1) — corrected here.** v1 proposed lowering the option into `api.Options` /
> `c.Options()`. That channel is **connection-scoped**: routing DRY RUN through it either makes the
> flag *sticky* on a pooled connection (the next plain `DELETE` silently no-ops) or leaves
> `executeProps()` reading `c.Options()` with `props.DryRun==false` so the branch never fires →
> **the real mutation runs on a DRY RUN statement** — the exact data-loss regression the reject
> exists to stop. The corrected design carries DRY RUN as a **per-statement field**, mirroring how
> `maxRows`/`execState` are already threaded. The connection is never touched.

1. **`ExecuteProperties.DryRun bool`** — add the field + a `WithDryRun` setter, threaded through
   `DefaultExecuteProperties()` (mirrors Java `ExecuteProperties.setDryRun`).
2. **Carry DRY RUN per-statement, not on the connection.** `planDML` detects it via the typed-tree
   helper `dmlHasDryRunOption(dml)`, which **walks the whole DML subtree** — not just
   `insertStatement.queryOptions`. This is required because the grammar attaches the trailing
   `OPTIONS` clause to different nodes per spelling: a VALUES insert puts it on
   `insertStatement.queryOptions`, but an `INSERT … SELECT … OPTIONS (DRY RUN)` is consumed by the
   inner SELECT's `queryTerm.queryOptions` (`#simpleTable`), leaving the outer one nil — so a
   statement-level-only check would MISS it and **commit** (codex P1, the resurrected data-loss).
   The subtree walk matches Java's `AstNormalizer`, which visits every `queryOptions` node and
   accumulates them into one statement-level `Options` (`DRY_RUN` at `AstNormalizer.java:281`);
   over-detection only ever previews, so the walk fails safe. Instead of *rejecting*, store the bool
   on the planned-statement result it returns; propagate it into a new `paginatingRows.dryRun` field
   at construction (`cascades_generator.go:1032`, alongside `maxRows`/`execState`).
3. **Thread it** — in `executeProps()` set `props = props.WithDryRun(r.dryRun)` (read the
   `paginatingRows` field, **never** `c.Options()`).
4. **Branch the three mutation sites** on `props.DryRun`:
   - DELETE → `store.DryRunDeleteRecord(pk)`; **keep the `if deleted` filter** so only
     would-be-deleted PKs echo (G1, Java's `.filter(isDeleted -> isDeleted)`); stage **no** write.
   - INSERT → `store.DryRunSaveRecord(msg, RecordExistenceCheckErrorIfExists)`; build the echo from
     the returned stored record (G2), not a real save; an existing-PK breach still raises `23505`
     exactly as the real path (Java validates in dry-run too).
   - UPDATE → `store.DryRunSaveRecord(msg, …ErrorIfNotExistsOrTypeChanged)`; echo from the returned
     stored record (G2).
   The no-partial-mutation accounting (all charging before the loop) is unchanged.
5. **Remove the `:693` reject** (step 2 now *sets the flag* where the reject used to fire). Once
   removed, `EXPLAIN <DML> OPTIONS (DRY RUN)` renders the plan regardless of routing (EXPLAIN never
   invokes the executor, so the flag is inert there). Torvalds (RFC v1) correctly flagged that v1's
   "EXPLAIN already bypasses the reject" was *unproven* — so this is pinned by an explicit EXPLAIN
   test (below), not asserted.

## Performance

DRY RUN is a strictly-no-write path: it runs the same plan + validation but calls the dry-run
primitives instead of staging FDB writes, so it is never slower than the real mutation (it skips the
write itself). No effect on non-DRY-RUN statements (the branch is a single bool check already in the
hot struct).

## Conformance note (codex review) — dry-run validation scope is Java-faithful

codex flagged that the dry-run preview misses (a) a secondary-UNIQUE conflict and (b) an
intra-statement duplicate PK. Reading Java settled both as **Java-faithful, not divergences**:
`FDBRecordStore.saveTypedRecord(isDryRun=true)` early-returns at `FDBRecordStore.java:578`, *before*
`serializeAndSaveRecord` (staging) and `updateSecondaryIndexes` (line 594). So Java's dry-run also
validates only the PK existence check against pre-statement state and skips secondary-index validation
+ intra-statement staging. Go's `DryRunSaveRecord` matches exactly. Detecting either would make Go's
preview **stricter than Java** — a conformance divergence (Go rejecting a DRY RUN Java previews as
success), which the conformance principle forbids. The Java-faithful boundary is pinned by
`TestFDB_DmlDryRun_MatchesJavaLightweightValidation` and documented at `DryRunSaveRecord`.

## Test plan (FDB integration, red→green)

- `DELETE ... OPTIONS (DRY RUN)` returns N, table contents UNCHANGED (asserted via a follow-up SELECT
  in a fresh txn).
- `UPDATE ... OPTIONS (DRY RUN)` returns N, values UNCHANGED.
- `INSERT ... OPTIONS (DRY RUN)` returns 1, the row is ABSENT afterward.
- `INSERT` of an existing PK under DRY RUN still raises `23505` (validation parity with the real path).
- **(G1) DELETE echo filter** — `DELETE WHERE pk IN (existing, absent) OPTIONS (DRY RUN)` returns
  only the count of *existing* rows; only would-be-deleted PKs echo.
- **(Torvalds, no-sticky regression)** — on ONE connection: a DRY RUN `DELETE` followed by a plain
  `DELETE` (no OPTIONS) — the plain one MUST actually mutate (proves the flag is per-statement, not
  connection-sticky). This is the data-loss-regression sentinel; revert-proof.
- **(Torvalds, EXPLAIN)** — `EXPLAIN <DML> OPTIONS (DRY RUN)` with a live DB renders a plan, no
  reject, no mutation (proven, not asserted).
- **(codex P1, DATA-LOSS)** `INSERT … SELECT … OPTIONS (DRY RUN)` previews with NO mutation (the
  OPTIONS is on the inner SELECT, not the statement) — revert-proven: a statement-level-only check
  commits the rows (count grows). A control `INSERT … SELECT` without DRY RUN still inserts.
- Flip `dryrun_option_rejected_probe_test.go` (currently pins the reject) to the new behavior.
- Full `just test` green.
