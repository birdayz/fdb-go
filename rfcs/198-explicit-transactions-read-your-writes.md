# RFC-198: An explicit transaction's reads belong to that transaction

Status: **proposed**, revision 1 — awaiting joint-review ACK before implementation.
Closes: TODO "driver: NO read-your-writes inside an explicit transaction — SELECT
auto-commits" (TODO.md:6616), the Tier-2 production gate B2.

## The defect, measured

`pkg/relational/sqldriver/tx_select_isolation_probe_test.go` pins today's
behaviour and its assertion is the defect stated as a fact:

```go
// DIVERGENCE: SELECT runs in a fresh tx → sees the pre-update value, not 777.
if v != 100 { t.Errorf(...) }
```

Inside `BeginTx`, `UPDATE t SET v = 777 WHERE id = 1` followed by
`SELECT v FROM t WHERE id = 1` returns **100**.

One line produces it. `cascades_generator.go:1753-1756`:

```go
runTx := c.sess.DB.Run
if r.respectActiveTx {
    runTx = c.runInTx
}
```

and `respectActiveTx` is set from `p.IsUpdate()` (`:1254`). `runInTx`
(`connection.go:197-202`) hands `fn` the open transaction's
`*FDBRecordContext` when `activeTx != nil` and otherwise opens a fresh one.
So DML joins the user's transaction and SELECT does not. Every other layer —
the store builder, the executor, the cursors — is already indifferent to which
context it is handed.

## The real failure is not the missing read-your-writes

Read-your-writes is the *visible* symptom. The shipping hazard is a **lost
update with no error**, and the mechanism is worth stating precisely because
the obvious explanation ("the read adds no conflict range") is not the whole
mechanism.

FDB takes a read version lazily, at the first read
(`pkg/fdbgo/client/transaction.go:662`, `ensureReadVersion`, called from every
read entry). A `BeginTx` alone reads nothing (`connection.go:501-516` creates
the transaction handle and sets one option), so the explicit transaction has
no read version until its first *statement* reads. Now:

| step | what happens |
|---|---|
| T1 `BeginTx` | handle created, **no read version** |
| T1 `SELECT v` → 100 | runs in a *separate* auto-commit transaction, at its own read version. Nothing is recorded on T1. |
| T2 `UPDATE v = 500` | commits |
| T1 `UPDATE v = 101` | **now** T1 takes its read version — *after* T2 committed. Its scan reads 500, writes 101. No conflict range covers anything T2 wrote before T1's read version. |
| T1 `COMMIT` | succeeds |

T2's write is gone and neither session saw 1020/40001. The transaction did
not merely fail to detect a conflict — the read and the write were never in
the same transaction, so there was **no serializable schedule to violate**.
This is why the fix cannot be "add a conflict range": the read has to move
into the transaction.

`BeginTx` makes this a contract violation, not just a surprise. It accepts
`sql.LevelSerializable` and says so (`connection.go:490-497`):

```go
case sql.LevelDefault, sql.LevelSerializable:
    // FDB is always serializable — this is fine.
```

The comment is true of FDB and false of this driver. A connection that
advertises serializable and permits lost updates is worse than one that
advertises read-committed.

## What Java does

Java's `EmbeddedRelationalConnection` holds exactly one `Transaction` field
(`:99`), assigned in four places only (`:118`, `:212`, `:237`, `:393`). With
`setAutoCommit(false)`, `canCommit()` is false (`:179-182`), and every
statement's entry call `conn.ensureTransactionActive()`
(`AbstractEmbeddedStatement.java:75`) short-circuits without replacing it:

```java
// EmbeddedRelationalConnection.java:477-487
if (inActiveTransaction()) {
    if (canCommit()) { rollbackInternal(); return ensureTransactionActive(); }
    return false;                       // autoCommit==false: REUSE the transaction
}
```

The SELECT's store is then built on that transaction's context:
`AbstractEmbeddedStatement.java:90` passes `conn.getTransaction()` into the
execution context; `QueryPlan.java:426` calls `recordLayerSchema.loadStore()`;
`RecordLayerSchema.java:98-108` caches one store per transaction and drops it
on termination; `BackingRecordStore.java:235` is the join point:

```java
.setContext(txn.unwrap(FDBRecordContext.class))
```

**Isolation is SERIALIZABLE, and it is not a default nobody chose.**
`EmbeddedRelationalConnection.java:125` initialises `transactionIsolation =
Connection.TRANSACTION_SERIALIZABLE`; `:510-516` maps that to
`IsolationLevel.SERIALIZABLE`; `:521` puts it on the `ExecuteProperties` that
every statement in the transaction inherits; `ExecuteProperties.java:402`
independently defaults the same way; and `:334-336` declares SERIALIZABLE the
*only* supported level. The level reaches the wire at
`KeyValueCursorBase.java:358`:

```java
transaction = context.readTransaction(scanProperties.getExecuteProperties().getIsolationLevel().isSnapshot());
```

with `FDBRecordContext.java:659-666` returning `ensureActive()` — the plain
transaction, which adds read conflict ranges — for the non-snapshot case.

So in Java, a SELECT inside an explicit transaction reads the transaction's
own uncommitted writes **and** contributes read conflict ranges that make the
commit fail on a concurrent writer. Both halves. Go has neither.

## What Go already has

The reason this is a driver-layer change and not an executor rewrite:

- `ExecuteProperties.IsolationLevel` exists and is a 1:1 port of Java's enum
  (`scan_properties.go:50-92, 131-133`), and `DefaultExecuteProperties()`
  already returns `SerializableIsolation` (`:233-244`) — the same default Java
  has.
- Every leaf read already honours it: `key_value_cursor.go:885-886`,
  `index_scan.go:706-707`, `record_key_cursor.go:238-239`,
  `bitmap_value_index_maintainer.go:509-510`, `store.go:453-457`. The
  `isSnapshot()` switch is already ported at every site Java has one.
- The executor's entry point (`executor.ExecutePlan(ctx, plan, store, evalCtx,
  continuation, props)`, called at `cascades_generator.go:1802`) takes the
  store it is given. It has no opinion about where the store's context came
  from.
- The pure-Go client is a full read-your-writes implementation ported from
  C++ `ReadYourWrites.actor.cpp` (`pkg/fdbgo/client/ryw.go`,
  `readpath.go`) — merged range reads, cleared ranges, atomic resolution —
  so an in-transaction scan will see the same transaction's staged mutations
  without any record-layer help.
- Record versions are already read-your-writes inside a transaction:
  `LoadRecordVersion` consults the context's local version cache before
  reading FDB (`store_version.go:142-149`).
- **In-transaction serializable reads already happen today** — for DML. An
  in-tx `UPDATE` routes through `runInTx`, builds its props from
  `DefaultExecuteProperties()` (`cascades_generator.go:1627`), and scans at
  serializable isolation on the user's transaction. SELECT is the *only*
  statement kind routed away from the transaction.

**The executor plumbing this needs is zero.** The change is transaction
routing in `pkg/relational/core/embedded`, plus one anchor field.

## Decision

**1. A SELECT executed while an explicit transaction is open runs on that
transaction's record context.** `fetchPage`'s `runTx` selection stops keying
on `IsUpdate()`. Read-your-writes and read-conflict ranges both follow from
this one routing change, because the layers below are already correct.

**2. In-transaction reads are SERIALIZABLE, not snapshot.**

The booking names "snapshot vs serializable" as the open question. It is not
open:

- Java is serializable, deliberately and at four independent sites
  (`EmbeddedRelationalConnection.java:125, 511-512, 521`;
  `ExecuteProperties.java:402`), and admits no other level (`:334-336`).
- `BeginTx` already promises serializable (`connection.go:490-497`). Snapshot
  reads would deliver read-your-writes while leaving the lost update in place
  — it would close the cosmetic half of the defect and ship the dangerous
  half, under a serializable label.
- Snapshot is not even the cheaper option in code: `DefaultExecuteProperties`
  is *already* serializable, so choosing snapshot means adding a deliberate
  downgrade.

"Spurious conflicts" is the stated fear, and it should be sized rather than
assumed. Three facts bound it. (a) The catalog carve-out that motivates the
fear is about *catalog* reads and it stays — see Decision 8. (b) The user's
own table reads conflicting with a concurrent writer of those rows is not
spurious; it is the serialization the connection promises, and the whole
point of B2. (c) The conflict-range surface is not new machinery: in-tx DML
already emits exactly these ranges over exactly these keys today.

**3. The result set is pinned to the transaction, and a dead transaction is
loud.** `runInTx` resolves `c.activeTx` at call time and **silently falls back
to `DB.Run` when it is nil** (`connection.go:197-202`). `Commit`/`Rollback`
set `activeTx = nil` (`connection.go:183, 189`). A `paginatingRows` outlives
the `Execute` that made it — pages are fetched from `Next` (`:1451-1494`),
after `QueryContext` has returned — so a SELECT whose transaction ends
mid-iteration would resume **reading in a fresh auto-commit transaction**,
silently, having just been made transactional. That is a worse bug than the
one being fixed.

`database/sql` closes the common door: `Tx.Commit` and `Tx.rollback` call
`tx.cancel()` and then take `tx.closemu.Lock()` before `txi.Commit()`
(`$GOROOT/src/database/sql/sql.go:2303-2309, 2337-2341`), so every `Rows` from
that `Tx` is force-closed first. The doors it does not close are real:
`execTransactionStatement`'s raw `COMMIT`/`ROLLBACK` (`connection.go:619-645`,
which mutates `activeTx` behind `database/sql`'s back), `ResetSession`
(`:532-547`), `Close` (`:451-459`), and any non-`database/sql` frontend
holding `driver.Rows` directly.

So: `paginatingRows` captures the `*embeddedTx` at `Execute` time and uses
that context for every page. `embeddedTx` gains a terminal flag set by
`Commit`/`Rollback`; a page fetched against a terminated transaction returns
`ErrCodeTransactionInactive` (25F01). An asserted boundary, never a silent
fallback.

**4. `respectActiveTx` splits into the two independent facts it currently
conflates.** The field is read at two sites with two different meanings:

- `fetchPage:1754` — "route through the explicit transaction" (a *transaction*
  question).
- `pageRowBudget:1606` — `if r.respectActiveTx { return 0 }`, i.e. "DML must
  never have its scan bounded by the returned-row cap" (a *statement-kind*
  question).

Redefining the one field to mean "in an explicit transaction" would silently
unbound the page scan of every in-tx SELECT that sets `MAX_ROWS` — a
performance regression introduced by a correctness fix, invisible to any test
that only checks rows. Two fields: `tx *embeddedTx` (routing) and `isUpdate
bool` (scan-bounding policy).

**5. The read time budget is anchored at the transaction, not at the page.**

`executeProps()` clamps each page to `txPageTimeLimit` = 4s
(`cascades_generator.go:1183-1186, 1641-1647`) and the counter it is checked
against is minted fresh per page — `DefaultExecuteProperties()` calls
`NewScanLimiterState()`, which anchors at `time.Now()`
(`scan_limiter_state.go:90-97`). Today that is harmless for SELECT because a
page *is* a transaction. Once pages share one transaction, N pages get N × 4s
against a 5-second wall.

Java anchors the same limiter at the transaction:
`CursorLimitManager.java:92-94` constructs `TimeScanLimiter` from
`context.getTransactionCreateTime()` (`FDBRecordContext.java:187, 676`). Go's
own comment at `scan_limiter_state.go:91-94` already *claims* that anchor; the
code does not implement it once pages share a context. So this is a parity
fix with a citation, not a Go invention.

Two deliberate deviations from a literal port, both because Go's cap is
**always on** while Java's is opt-in and unlimited by default
(`Options.java:294` sets `EXECUTION_TIME_LIMIT` to 0 = `UNLIMITED_TIME`,
`ExecuteProperties.java:45`):

- **Anchor at the transaction's first statement execution, not at
  `CreateWritableTransaction`.** FDB's 5-second window is measured from the
  *read version*, and the read version is taken lazily at the first read
  (`client/transaction.go:662`). A create-time anchor under an always-on cap
  would fail a query FDB would have served, on a transaction that idled after
  `BeginTx`. Every statement's first act is opening a store, which reads the
  store header, so "first statement execution against this transaction" is
  within one store-open of the GRV. Java can use the looser anchor precisely
  because its limiter is off by default.
- **The record and byte counters move WITH the time anchor.** Review
  established what the draft left open: Java shares one `ExecuteState`
  across every statement in a transaction — `Builder(ExecuteProperties)`
  carries the state reference (`ExecuteProperties.java:415`), `build()`
  keeps it when present (`:598-600`), `QueryPlan.java:432` derives every
  statement from `connection.getExecuteProperties().toBuilder()`, and
  `newExecuteProperties()` runs once per transaction
  (`EmbeddedRelationalConnection.java:394`). So the scanned-records and
  scanned-bytes counters are TRANSACTION-scoped in Java, and they become
  transaction-scoped here, in the same shared-state shape. The verification
  plan pins it: two statements in one transaction whose combined scanned
  records cross the limit must trip it on the second statement, where
  per-page counters would stay green.

**6. Budget exhaustion pre-empts with the SQLSTATE FDB would have produced.**
When the remaining transaction budget is gone, the next page fails with
`ErrCodeSerializationFailure` (40001) and a message naming the 5-second
window. That is the same code `translateFDBCode` already assigns to FDB's own
`transaction_too_old` (1007) (`connection.go:670-682`), so an application's
retry logic is uniform whether the driver pre-empts or FDB does. It is not
54F01: 54F01 means "raise your limits", and no limit the caller can raise
makes an FDB transaction live longer than 5 seconds.

Pre-emption is also required to keep the liveness tripwire honest. Without a
pre-page check, an exhausted budget produces a page with zero rows and an
unadvanced continuation, which trips
`"query cannot progress under the configured per-page resource limits"`
(`cascades_generator.go:1837-1840`) — a 54F01 telling the user to raise limits
that are not the problem.

**7. The paginating path gets the FDB error translation the first page already
has.** `QueryContext`/`ExecContext` wrap `plan.Execute`'s error in
`translateFDBError` (`connection.go:376, 413, 433`), so a conflict on page 1
becomes 40001. Pages ≥ 2 surface through `paginatingRows.Next` →
`fetchPage` → `translateExecErrorCtx` → `translateExecError`, which has no FDB
lane and returns the error unchanged (`cascades_generator.go:1874-1963`, final
`return err`). Today that is nearly unreachable for SELECT; after this change
it is the normal way an in-tx conflict is observed. `translateExecError` gains
the `translateFDBError` tail, and `translateFDBCode` gains `1025
transaction_cancelled` → 25F01 (the code a rolled-back context returns,
`client/transaction.go:607-615`; it is absent from the switch today).

**8. Planning-time reads stay outside the transaction, deliberately.** Two
existing carve-outs are kept, and both are planning-time, not execution-time:

- `cachedLoadSchema` reads the catalog in a separate auto-commit transaction
  when `activeTx != nil` (`connection.go:212-237`), so catalog reads do not
  conflict with concurrent DDL. Java's equivalent scoping is per-transaction
  (`RecordContextTransaction.java:88-96` keeps the bound schema template in
  the context session), so this is a genuine divergence — but it is the
  pre-existing one that already governs in-tx DML, it is documented at the
  site, and this RFC neither widens nor narrows it. Note that DDL itself
  auto-commits regardless (`ddl.go:886-894` uses `DB.Run`, never `runInTx`),
  so binding catalog reads to the user transaction would not make DDL
  transactional anyway.
- `fetchTableStatistics` reads record counts via `DB.RunRead` with snapshot
  gets (`cascades_generator.go:1993-2005`). Cost-model statistics are not part
  of the transaction's read set; conflicting on a per-type record count would
  make every explicit transaction conflict with every insert in the database.

## Rejected alternatives

**Snapshot reads for in-transaction SELECT.** Delivers read-your-writes,
leaves the lost update. It is the option that makes the probe test's first
assertion flip while the actual production hazard ships. Java is serializable
at four sites and supports no other level; `BeginTx` already advertises
serializable. Rejected in Decision 2.

**Expose the choice as `SET TRANSACTION ISOLATION LEVEL`.** The grammar
already parses it (`RelationalParser.g4:619-632`, `READ COMMITTED |
SERIALIZABLE`) and `administrationStatement` has no Go handler, so it fails
0A000 today. Adding a snapshot escape hatch would be a Go-only widening of the
isolation surface in the same change that fixes an isolation bug — and Java's
own `supportsTransactionIsolationLevel` (`:334-336`) accepts SERIALIZABLE
only. If the level is ever wired, it belongs in its own item with its own
conformance decision about Java's `setTransactionIsolation` (which stores the
field but only rebuilds `ExecuteProperties` at the next transaction start,
`:345-348` vs `:394` — a quirk Go should decide about deliberately, not
inherit accidentally).

**Buffer DML writes in the driver and replay them over SELECT results.** A
second, driver-local read-your-writes implementation, layered on top of the
one the client already ports from C++, correct only for the shapes someone
remembers to handle (index entries, cleared ranges, atomic ops, split
records). The transaction already does this. Rejected.

**Materialize the whole in-tx result set inside the transaction, then serve it
after.** Bounds every in-tx SELECT by memory instead of by the transaction,
and breaks the RFC-130 statement memory budget's meaning. It also *hides* the
5-second limit rather than reporting it, which is the failure mode the FAQ
explicitly tells users to design around (`fdb-record-layer/docs/sphinx/source/
FAQ.md:173-180`).

**Let pages ≥ 2 of an in-tx SELECT open their own transactions (today's
behaviour, kept deliberately).** This is exactly the defect, applied to page 2
onward: the tail of a result set would be read outside the transaction that
was supposed to contain it. It also makes an in-tx result set non-repeatable
against itself. Rejected in Decision 3.

**Set an FDB transaction timeout on the explicit transaction at `BeginTx`.**
Would give a hard stop for free (1031 → 53F00, already mapped). But the
timeout is anchored at transaction creation, so it would kill a transaction
that idled after `BeginTx` before reading anything — which FDB itself would
have served, because the read version had not been taken yet. Java sets no
default either (`RecordLayerTransactionManager.java:62-70`;
`Options.Name.TRANSACTION_TIMEOUT` has no default entry;
`FDBDatabaseFactory.DEFAULT_TR_TIMEOUT_MILLIS = -1`).

## Consequences to expect

**The 5-second wall becomes visible to applications, and that is the point.**
Today an in-tx SELECT over a large table paginates across as many
transactions as it needs and always "works". After this change it is bounded
by the transaction it belongs to, and a scan that cannot finish in the
remaining budget fails 40001. Java behaves the same way and is *less* helpful
about it: `ExceptionUtil.java:59-85` has no lane for
`FDBStoreTransactionIsTooOldException`, so Java surfaces the raw 1007 as
`ErrorCode.UNKNOWN`. Go pre-empting with 40001 and a message naming the window
is a read-side improvement over Java with no wire implication.

The application-facing remedy is the one the record layer documents:
decompose the work, or bound the statement so it stops before the wall
(`MAX_ROWS`, `EXECUTION_SCANNED_ROWS_LIMIT`, `EXECUTION_TIME_LIMIT` — all
already wired through `executeProps`). Java's equivalent surface is
`Continuation.Reason.TRANSACTION_LIMIT_REACHED`
(`RecordLayerResultSet.java:121-135`). Go has no cross-request continuation
surface at all (`cascades_generator.go:1214-1217` rejects a caller-supplied
`CONTINUATION` loudly), so an in-tx SELECT that outgrows its transaction has
no resume path — the application must re-run it in a new transaction. That is
a real UX cost of doing this correctly, and it is the honest one.

**Serialization failures will start appearing where none appeared before.**
Any read-modify-write across two concurrent explicit transactions on
overlapping rows now fails one of them with 40001 instead of silently losing a
write. Applications with no retry loop will see new errors. There is no
version of "correct" that avoids this.

**Read-your-writes changes results, including through indexes.** An in-tx
`INSERT` followed by an in-tx `SELECT` that plans to an index scan must return
the new row, which requires the client's RYW merge over a *range* read, not
just a point get. An in-tx `DELETE` followed by a `SELECT` exercises the
cleared-range merge. These are the RYW paths most likely to have a gap, and
they are pinned explicitly below rather than assumed from "the client ports
C++".

**Auto-commit behaviour is unchanged.** With no explicit transaction,
`runInTx` still falls through to `DB.Run` and every page is its own
transaction, exactly as today.

**A caller deadline still does not reach the FDB read futures on this path.**
The cursor loop honours the statement context (`ExecutePlan(r.ctx, ...)`), so
cancellation is observed between reads; the in-flight read future is bounded
by the transaction's own context, which for an explicit transaction is
`context.Background()` (`database.go:575-580`), the pre-existing gap
documented at `connection.go:471-481`. This RFC does not change it, and does
not widen it either — in-tx DML already reads on that context.

## What this does not do

- It does not make DDL transactional. `runDDL` auto-commits in its own
  transaction even inside `BeginTx` (`ddl.go:886-894`). Java runs DDL in the
  connection transaction. Separate item, separate blast radius.
- It does not add a cross-request continuation surface, and it does not add a
  SQL isolation-level surface.
- It does not change auto-commit's cross-transaction pagination, which is a
  Go-only extension (Java hands the caller a continuation and stops). Real,
  pre-existing, out of scope.
- It does not touch the wire. No key encoding, record/index format,
  continuation encoding, or committed byte changes. The change is *which
  transaction* reads run in.

## Verification plan

The acceptance bar is the pinned probe flipping, plus the dimensions it cannot
express.

1. **`tx_select_isolation_probe_test.go` flips.** `no_read_your_writes_in_
   explicit_tx` becomes `read_your_writes_in_explicit_tx` asserting **777**,
   and the file header comment stating the divergence is deleted, not amended.
   `dml_still_atomic_on_commit` and `dml_undone_on_rollback` must stay green
   unchanged — they are the controls proving the write side was not disturbed.
2. **The lost update becomes a 40001**, as the exact shape from the table
   above: T1 `BeginTx` → `SELECT v` → T2 (separate conn) `UPDATE` and commit →
   T1 `UPDATE` → T1 `COMMIT` must fail with 40001. Mutation direction:
   forcing `IsolationLevelSnapshot` on the in-tx read path must make this test
   go green-wrong (commit succeeds, write lost) while test 1 still passes —
   that is the pin that makes the snapshot-vs-serializable decision a *tested*
   decision rather than a prose one.
2. **Read-your-writes through an index, not just a scan.** In-tx `INSERT` then
   an in-tx `SELECT` with an `EXPLAIN` assertion that the plan is an index
   scan, returning the uncommitted row. A full-scan plan would pass this test
   while the index RYW path is broken, so the plan shape is part of the
   assertion (the "test that cannot express the defect is not coverage" rule).
3. **Read-your-writes over a cleared range.** In-tx `DELETE` then in-tx
   `SELECT` returns nothing, under both a record scan and an index scan.
5. **The result set cannot outlive its transaction.** Open an in-tx SELECT
   large enough to need a second page, issue a raw `COMMIT` via `ExecContext`
   on the same connection (the door `database/sql`'s `closemu` barrier does
   not cover), then iterate: must be 25F01, never rows read from a fresh
   transaction. Mutation direction: restoring the `runInTx` nil-fallback for
   the pinned transaction must redden this and *only* this.
   A `ResetSession` variant covers the pooled-connection door.
6. **Whole-transaction time budget.** An in-tx SELECT whose scan needs more
   than the remaining budget fails 40001 naming the 5-second window — not
   54F01, and not the liveness tripwire's "query cannot progress" message
   (assert on the code *and* that the message is not the tripwire's).
   Mutation direction: reverting the anchor to per-page must make this test
   observe FDB's raw 1007 instead, which is the pre-emption's whole reason to
   exist. A second case pins that the anchor is the first *statement*, not
   `BeginTx`: `BeginTx`, sleep past the budget, then a small SELECT — must
   succeed.
7. **MAX_ROWS still bounds an in-tx SELECT's page scan.** The regression
   Decision 4 exists to prevent: assert the page cursor's returned-row limit
   is set for an in-tx SELECT with `MAX_ROWS`, and that an in-tx DML's is not.
   A rows-only test cannot see this, so it asserts on the bound, not the
   output.
8. **FDB error translation on pages ≥ 2.** A conflict raised on a later page
   surfaces as 40001, and a page fetched on a cancelled context as 25F01 —
   pinning Decision 7's `translateExecError` tail and the new 1025 lane.
9. **Auto-commit is untouched.** The existing auto-commit SELECT suite
   (including the pagination and `resource_limits_test.go` cases) stays green
   with no expectation edits. `resource_limits_test.go:41,78` asserts the 4s
   page floor and must still hold when no explicit transaction is open.
10. **1M stress before and after** (CLAUDE.md's workflow). The auto-commit
    path should be unmoved; a difference there means the routing split leaked.

## What I could not establish by reading, and must be measured at implementation

Stated here so it is not discovered as a surprise, and so a reviewer can
attack the right things.

1. **The actual skew between "first statement execution against the
   transaction" and the GRV instant.** The argument is that opening a store
   reads the store header immediately, so the two are within one round trip.
   That is a claim about a measured interval, and it decides whether 4s is the
   right whole-transaction budget or whether it should be lower. Measure it
   against a real cluster and record the number; if the skew is material, the
   anchor moves to the client's read-version instant and that becomes a
   `pkg/fdbgo` change with its own review gate.
3. **Whether any in-tx read path bypasses `ExecuteProperties.IsolationLevel`.**
   I enumerated the `isSnapshot()` sites and they cover the leaf cursors, but
   an unconditional `Snapshot()` call reached from a SELECT plan would be a
   silent hole. `range_set.go:296`, `bunched_map.go:158,340`,
   `index_state.go:305`, `database.go:980` and `aggregate_function.go:470` all
   take snapshots unconditionally; each is index-maintenance, store-state, or
   ranked-set internals rather than a query leaf, and each is paired with an
   explicit conflict key where Java pairs one — but "reached from a SELECT
   plan" is a reachability claim, and reachability claims get a test, not a
   reading. A negative-result test enumerating the query leaves and asserting
   each consults the isolation level is the pin.
4. **The conflict-range volume of a realistic in-tx SELECT.** Decision 2 argues
   the new conflict surface is bounded and legitimate. The *rate* at which
   real workloads will now see 40001 is an empirical question the 1M stress
   run will answer for one workload and no more.
