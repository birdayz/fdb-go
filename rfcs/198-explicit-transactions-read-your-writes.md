# RFC-198: An explicit transaction's reads belong to that transaction

Status: **proposed**, revision 2 — awaiting joint-review ACK before implementation.
Closes: TODO "driver: NO read-your-writes inside an explicit transaction — SELECT
auto-commits" (`TODO.md:6846`), the Tier-2 production gate B2.

Revision 1 was **NAK'd by both reviewers**. The mechanism — Decision 1, routing
in-transaction SELECTs through the active transaction — was verified sound by
both and is unchanged. Everything around it moved.

### What changed from revision 1

1. **Decision 6 is rewritten.** Revision 1 said an exhausted budget pre-empts
   with `40001`, full stop. That contradicts merged RFC-203, and it is not what
   Java does. Java stops the cursor **cleanly** with
   `TRANSACTION_LIMIT_REACHED` and leaves the transaction open. `40001` is now a
   **named interim** with a stated retirement condition (Decision 6).
2. **RFC-203 is amended and cross-cited** (its §4.2.1/§4.2.2, its G12/G12a/G12b,
   its G13). The two documents no longer contradict each other, and neither may
   be read alone on in-transaction pagination.
3. **Continuation tokens minted inside a transaction are transaction-bound**
   (Decision 9). Revision 1 left the door open for `BEGIN; SELECT MAX_ROWS;
   COMMIT; EXECUTE CONTINUATION` to resume at a different read version.
4. **Every out-of-transaction read is enumerated with an explicit routing
   decision** (Decision 8), including six sites that already join the
   transaction and refute revision 1's carve-out rationale.
5. **The catalog carve-out is resolved, not inherited** (Decision 8b): the
   catalog binds to the transaction, Java's shape.
6. **The acceptance plan is rebuilt on SimFDB + `dst.Env`** (Verification). The
   wall-clock `time.Sleep` is gone; `dst` has no sleep to replace it with.
7. **`ScanLimiterState` is named as the transaction-scoped object**, and
   `ExecuteState` is named as the one that stays statement-scoped (Decision 5).
8. **`embeddedTx.Commit` gets error translation and a `1021` lane** with a
   decided SQLSTATE (Decision 7).
9. **Every citation was re-verified at this head.** Revision 1's were
   substantially wrong — see "Citations re-verified" at the end, which records
   what moved and what was refuted outright.

---

## The defect, measured

`pkg/relational/sqldriver/tx_select_isolation_probe_test.go` pins today's
behaviour and its assertion is the defect stated as a fact. In
`TestFDB_TxSelectIsolationProbe` (`:26`), subtest
`no_read_your_writes_in_explicit_tx` (`:46`), at `:59-60`:

```go
// DIVERGENCE: SELECT runs in a fresh tx → sees the pre-update value, not 777.
if v != 100 {
```

Inside `BeginTx`, `UPDATE t SET v = 777 WHERE id = 1` followed by
`SELECT v FROM t WHERE id = 1` returns **100**.

One line produces it. `cascades_generator.go:1759-1762`:

```go
runTx := c.sess.DB.Run
if r.respectActiveTx {
    runTx = c.runInTx
}
```

and `respectActiveTx` is set from `p.IsUpdate()` (`:1255`). `runInTx`
(`connection.go:197-202`) hands `fn` the open transaction's
`*FDBRecordContext` when `activeTx != nil` and otherwise opens a fresh one.
So DML joins the user's transaction and SELECT does not.

## The real failure is not the missing read-your-writes

Read-your-writes is the *visible* symptom. The shipping hazard is a **lost
update with no error**, and the mechanism is worth stating precisely because
the obvious explanation ("the read adds no conflict range") is not the whole
mechanism.

FDB takes a read version lazily, at the first read
(`pkg/fdbgo/client/transaction.go:662`, `ensureReadVersion`, called from every
read entry). A `BeginTx` alone reads nothing — `beginTransaction`
(`connection.go:501-516`) calls `CreateWritableTransaction()` (`:507`), sets
exactly one option (`:511`, `SetReadSystemKeys`), and stores the handle
(`:514`) — so the explicit transaction has no read version until its first
*statement* reads. Now:

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
(`:98-99`), assigned in four places only (`:118`, `:212`, `:237`, `:393`) — I
grepped for others and there are none. With `setAutoCommit(false)`,
`canCommit()` is false (`:179-182`: `return !usingAnExternalTransaction &&
this.autoCommit`), and every statement's entry call
`conn.ensureTransactionActive()` (`AbstractEmbeddedStatement.java:75`)
short-circuits without replacing it (`:476-495`, branch `:477-488`):

```java
if (inActiveTransaction()) {
    if (canCommit()) { rollbackInternal(); return ensureTransactionActive(); }
    return false;                       // autoCommit==false: REUSE the transaction
}
```

The SELECT's store is then built on that transaction's context:
`AbstractEmbeddedStatement.java:90` passes `conn.getTransaction()` into the
execution context; `query/QueryPlan.java:426` calls
`recordLayerSchema.loadStore()`; `recordlayer/RecordLayerSchema.java:98-108`
caches one store per transaction and drops it on termination via
`conn.addCloseListener(() -> currentStore = null)`; `BackingRecordStore.java:235`
is the join point:

```java
.setContext(txn.unwrap(FDBRecordContext.class))
```

(Two files are named `RecordLayerSchema.java`; the one with `loadStore` is
`recordlayer/`, not `recordlayer/metadata/`.)

**Isolation is SERIALIZABLE, and it is not a default nobody chose.**
`EmbeddedRelationalConnection.java:125` initialises `transactionIsolation =
Connection.TRANSACTION_SERIALIZABLE`; `:510-516` maps that to
`IsolationLevel.SERIALIZABLE`; `:521` puts it on the `ExecuteProperties` that
every statement in the transaction inherits; and `ExecuteProperties.java:402`
independently defaults the same way. The level reaches the wire at
`KeyValueCursorBase.java:358`:

```java
transaction = context.readTransaction(scanProperties.getExecuteProperties().getIsolationLevel().isSnapshot());
```

with `FDBRecordContext.java:659-666` returning `ensureActive()` (`:546`) — the
plain transaction, which adds read conflict ranges — for the non-snapshot case.

So in Java, a SELECT inside an explicit transaction reads the transaction's
own uncommitted writes **and** contributes read conflict ranges that make the
commit fail on a concurrent writer. Both halves. Go has neither.

**What Java does NOT have: any retry.** There is no `FDBDatabaseRunner` and no
`runAsync` anywhere under `fdb-relational-core/.../recordlayer/` — I grepped and
the hit count is zero. `RecordLayerTransactionManager.java:58-60` is
`public void commit(Transaction txn) { txn.commit(); }`, one shot.
`RecordContextTransaction.java:64-73` catches
`FDBStoreTransactionConflictException` → `ErrorCode.SERIALIZATION_FAILURE` and
otherwise defers to `ExceptionUtil`. This matters twice below (Decisions 6
and 7), so it is established once here.

## What Go already has

The reason this is a driver-layer change and not an executor rewrite:

- `ExecuteProperties.IsolationLevel` exists and is a 1:1 port of Java's enum,
  and `DefaultExecutePropertiesIn` (`scan_properties.go:245`) returns
  `SerializableIsolation` (`:247`) — the same default Java has.
  `DefaultExecuteProperties()` (`:234`) is `DefaultExecutePropertiesIn(nil)`.
- Every leaf read already honours it: `key_value_cursor.go`, `index_scan.go`,
  `record_key_cursor.go`, `bitmap_value_index_maintainer.go`, `store.go`. The
  `isSnapshot()` switch is ported at every site Java has one. (The
  *reachability* of a bypass is Open Question 2, not a reading.)
- The executor's entry point (`executor.ExecutePlan(r.ctx, r.plan, store,
  evalCtx, r.continuation, mainProps)`, `cascades_generator.go:1808`) takes the
  store it is given. It has no opinion about where the store's context came
  from.
- The pure-Go client is a full read-your-writes implementation ported from
  C++ `ReadYourWrites.actor.cpp` (`pkg/fdbgo/client/ryw.go`, `readpath.go`) —
  merged range reads, cleared ranges, atomic resolution — so an
  in-transaction scan will see the same transaction's staged mutations without
  any record-layer help.
- Record versions are already read-your-writes inside a transaction:
  `LoadRecordVersion` consults the context's local version cache before
  reading FDB (`store_version.go:142-149`).
- **In-transaction serializable reads already happen today** — for DML, and for
  the six system-table paths in Decision 8. SELECT is the *only* ordinary
  statement kind routed away from the transaction.

Revision 1 ended this section with "**The executor plumbing this needs is
zero.**" That sentence is **deleted**. It was an invitation to stop looking, and
it is contradicted two decisions later: `fetchPage` rebuilds the whole store on
every page (`cascades_generator.go:1768-1772`), which is free when a page is a
transaction and is a per-page header read against a scarce budget once pages
share one (Decision 10). The change is small; "zero" is a claim, and it is
false.

## Decision

### 1. A SELECT executed while an explicit transaction is open runs on that transaction's record context

`fetchPage`'s `runTx` selection stops keying on `IsUpdate()`. Read-your-writes
and read-conflict ranges both follow from this one routing change, because the
layers below are already correct.

**This REMOVES automatic retry from in-transaction SELECTs, and that is a
consequence, not a side effect.** `DB.Run` (`database.go:230`) dispatches
through `runTransactCtx` → `TransactCtx`, the client's retry loop, which retries
the retryable codes (1020, 1007, 1021) transparently. `runInTx` calls
`fn(c.activeTx.rctx)` **directly** (`connection.go:198-200`) — no loop, no
`onError`. So today an in-tx SELECT that hits a conflict is silently retried and
the application never learns; after this change the error reaches the
application.

That is the correct semantics — a retry inside an explicit transaction would
have to re-run *the whole transaction*, which the driver cannot do because it
does not own the statements the application has not issued yet — but it belongs
in the blast radius, stated plainly: **the application re-runs the transaction.**
It ties directly to Decision 7's ruling that an explicit transaction does not
auto-retry.

### 2. In-transaction reads are SERIALIZABLE, not snapshot

The booking names "snapshot vs serializable" as the open question. It is not
open:

- Java is serializable, deliberately and at three independent sites
  (`EmbeddedRelationalConnection.java:125`, `:510-516`, `:521`;
  `ExecuteProperties.java:402`).
- `BeginTx` already promises serializable (`connection.go:490-497`). Snapshot
  reads would deliver read-your-writes while leaving the lost update in place
  — it would close the cosmetic half of the defect and ship the dangerous
  half, under a serializable label.
- Snapshot is not even the cheaper option in code: `DefaultExecutePropertiesIn`
  is *already* serializable, so choosing snapshot means adding a deliberate
  downgrade.

"Spurious conflicts" is the stated fear, and it should be sized rather than
assumed. Three facts bound it. (a) The catalog carve-out that motivated the fear
is resolved in Decision 8b rather than assumed away. (b) The user's own table
reads conflicting with a concurrent writer of those rows is not spurious; it is
the serialization the connection promises, and the whole point of B2. (c) The
conflict-range *machinery* is not new: in-tx DML already emits read conflict
ranges through exactly this path today.

**Point (c) is narrower than revision 1 claimed, and the overclaim mattered.**
Revision 1 wrote that DML "already emits exactly these ranges over exactly these
keys today." The machinery is the same; **the keys are not.** A DML statement's
read set is whatever its own predicate scan touched. A SELECT can scan ranges no
DML in the same transaction would ever touch — a full table scan, a different
index, a wider range on the same index. The conflict *volume* of this change is
plan-dependent and can be far larger than the DML that motivated the comparison.
Open Question 3 is what sizes it; this decision does not get to assume it away.

#### 2a. The isolation table is per READ CLASS, never per statement type

Keying isolation off "is this a SELECT or a DML" is what produced the defect in
the first place. The correct axis is *what is being read*:

| read class | isolation | which transaction |
|---|---|---|
| user data (records, indexes) | SERIALIZABLE | the active transaction — **any statement kind** |
| cost-model statistics | SNAPSHOT | a separate transaction (Decision 8c) |
| store state / metadata versionstamp | SNAPSHOT | the active transaction — **already correct today** |
| range-set / bunched-map internals | SNAPSHOT + a targeted conflict key | the active transaction |
| catalog (schema, metadata) | SERIALIZABLE | the active transaction (Decision 8b) |

The store-state and range-set rows are the ones most likely to be misread as
bugs by a reviewer scanning for `Snapshot()` calls. They are not. A snapshot read
paired with an explicit narrow conflict key is Java's own pattern, and it is
*stronger* than a serializable range read: it conflicts on the one key whose
change would invalidate the decision, instead of on every key the read happened
to traverse.

#### 2b. The conflict-range shapes this produces, enumerated

The shapes a reviewer should expect, because "it adds read conflict ranges" is
not specific enough to review:

- **Full table scan** → a read conflict range over the whole record subspace of
  that record type. Any concurrent insert, update, or delete of that type
  conflicts. This is the expensive shape and it is the one an unindexed
  predicate produces.
- **Index scan with an equality or range predicate** → a read conflict range
  over the matching span of the *index* subspace, plus point ranges over the
  records actually fetched.
- **Covering index scan** → a read conflict range over the index subspace
  **only**. The record subspace is never touched, so it is never conflicted.
- **Primary-key point lookup** → a single-key read conflict range.

**The covering-index case has a real isolation consequence and it must not be
discovered in production.** A concurrent writer that modifies a **non-indexed**
column of a row the covering scan matched does **not** conflict with it — the
scan never read the record, only the index entry, and FDB conflicts on what was
read. So the isolation an in-transaction SELECT gets is **plan-dependent**: the
same SQL text, planned to a covering index instead of a record scan, has a
strictly smaller conflict footprint.

This is **parity with Java, not a divergence.** Java's mechanism is identical —
`KeyValueCursorBase.java:358` reads through the plain transaction over whatever
subspace the cursor was built on, so a Java covering scan conflicts on the index
subspace and nothing else. It is stated here rather than left implicit because a
serializable guarantee whose footprint depends on the optimizer's choice is a
property users must be told about, and it is repeated in Consequences.

### 3. The result set is pinned to the transaction, and a dead transaction is loud

`runInTx` resolves `c.activeTx` at call time and **silently falls back to
`DB.Run` when it is nil** (`connection.go:201`). `Commit`/`Rollback` set
`activeTx = nil` (`connection.go:183`, `:189`). A `paginatingRows` outlives the
`Execute` that made it — pages are fetched from `Next`, after `QueryContext` has
returned — so a SELECT whose transaction ends mid-iteration would resume
**reading in a fresh auto-commit transaction**, silently, having just been made
transactional. That is a worse bug than the one being fixed.

`database/sql` closes the common door: `Tx.Commit` and `Tx.rollback` call
`tx.cancel()` and then take `tx.closemu.Lock()` before `txi.Commit()`
(`$GOROOT/src/database/sql/sql.go`), so every `Rows` from that `Tx` is
force-closed first. The doors it does not close are real:
`execTransactionStatement`'s raw `COMMIT`/`ROLLBACK` (`connection.go:619-645`,
which mutates `activeTx` behind `database/sql`'s back), `ResetSession`
(`:532-547`), `Close` (`:451-459`), and any non-`database/sql` frontend holding
`driver.Rows` directly.

So: `paginatingRows` captures the `*embeddedTx` at `Execute` time and uses that
context for every page. `embeddedTx` gains a terminal flag; a page fetched
against a terminated transaction returns `ErrCodeTransactionInactive` (25F01).
An asserted boundary, never a silent fallback.

**The flag lives on `embeddedTx` and is set at ALL FOUR doors**, because two of
them currently bypass the transaction's own methods entirely:

| door | site | today |
|---|---|---|
| `embeddedTx.Commit` | `connection.go:181-185` | sets `activeTx = nil` (`:183`) |
| `embeddedTx.Rollback` | `connection.go:188-192` | sets `activeTx = nil` (`:189`) |
| `EmbeddedConnection.Close` | `connection.go:451-459` | cancels `activeTx` at `:453-457` — **does not go through `Rollback`** |
| `ResetSession` | `connection.go:532-547` | cancels `activeTx` at `:537-543` — **does not go through `Rollback`** |

Putting the flag on the connection instead would be wrong for a specific reason:
the connection outlives the transaction and is reused from the pool, so a flag
there would have to be cleared on the next `BeginTx` and a `paginatingRows`
holding a stale reference would see it cleared and resume. The flag has to live
on the object whose death it records.

**And the nil-fallback is fixed on ALL routes, one policy.** Six system-table
sites already reach `runInTx` (Decision 8d); the fallback is as silent for them
as for SELECT. `runInTx` keeps a fallback only where auto-commit is the intended
mode (no transaction was ever open), and asserts wherever a transaction was
captured and has since died. One mechanism, not a SELECT-specific patch.

### 4. `respectActiveTx` splits into the two independent facts it currently conflates

The field is read at two sites with two different meanings:

- `fetchPage` (`:1760`) — "route through the explicit transaction" (a
  *transaction* question).
- `pageRowBudget` (`:1607-1608`) — `if r.respectActiveTx { return 0 }`, i.e.
  "DML must never have its scan bounded by the returned-row cap" (a
  *statement-kind* question).

Redefining the one field to mean "in an explicit transaction" would silently
unbound the page scan of every in-tx SELECT that sets `MAX_ROWS` — a performance
regression introduced by a correctness fix, invisible to any test that only
checks rows. Two fields: `tx *embeddedTx` (routing) and `isUpdate bool`
(scan-bounding policy).

### 5. The scan budget is anchored at the transaction — and the object is named

Revision 1 said "the read time budget is anchored at the transaction" without
naming what moves. Two different objects live in `ExecuteProperties` and only
one of them moves.

**`ScanLimiterState` (`props.ScanState`) becomes TRANSACTION-scoped.** It holds
both the time anchor and the scanned-records/scanned-bytes counters
(`scan_limiter_state.go`, minted at `:130-131` as
`&ScanLimiterState{startTime: env.Now(), env: env}`). Today it is minted fresh
per page by `DefaultExecutePropertiesIn` (`scan_properties.go:255`), which is
harmless only because a page *is* a transaction. Once pages share one
transaction, N pages get N × 4s against a 5-second wall.

**`ExecuteState` (`props.State`) STAYS STATEMENT-scoped.** It is the RFC-130
statement memory budget and recursion cap, minted once per statement at
`cascades_generator.go:1265-1267` and assigned into every page's props at
`:1676` — already correct, already surviving the per-page cursor rebuild. It
must not be dragged along: a memory budget is a property of one statement's
buffering operators, and sharing it across a transaction would make the third
statement in a transaction fail for memory the first two released. Pinned by a
two-statements-full-budgets test.

Java splits these the same way, by different means: the time anchor comes from
`context.getTransactionCreateTime()` (`CursorLimitManager.java:92-94`;
`FDBRecordContext.java:187` assigns it in the constructor, `:676-678` is the
getter), and the record/byte counters come from a shared `ExecuteState` —
`Builder(ExecuteProperties)` copies the *reference* (`ExecuteProperties.java:416-425`,
specifically `:421 this.executeState = executeProperties.state;`), `build()`
keeps it when present (`:597-611`, branch `:598-600`), `query/QueryPlan.java:433`
derives every statement from `connection.getExecuteProperties().toBuilder()`, and
`newExecuteProperties()` runs once per transaction
(`EmbeddedRelationalConnection.java:394`, inside `startTransaction` `:390-408`
guarded by `:392 if (!inActiveTransaction())`). Both are transaction-scoped in
Java. Making Go's single `ScanLimiterState` transaction-scoped reproduces both.

#### 5a. This TIGHTENS in-transaction DML, and the tightening is gated, not hidden

Java can share this state harmlessly because its limits default OFF:
`Options.java:294` sets `EXECUTION_TIME_LIMIT` to `0` = `UNLIMITED_TIME`
(`ExecuteProperties.java:45`), and `CursorLimitManager.java:92` leaves
`timeScanLimiter` **null** in that case. Go's is not off. So sharing the state is
a behaviour change for multi-statement transactions and it gets said out loud.

The measured asymmetry is narrower than "Go's limits are always on", and the
difference decides how much is being tightened:

- **Time: always armed.** `executeProps` unconditionally sets
  `timeLimit := txPageTimeLimit` (`cascades_generator.go:1647`; the const is 4s
  at `:1187`) and applies it at `:1653`. A user option can only *narrow* it
  (`:1648-1652`).
- **Scanned records and scanned bytes: opt-in.**
  `DefaultExecutePropertiesIn` sets both to `0` = no limit
  (`scan_properties.go:248-249`), and `executeProps` wires a real limit only when
  the caller set `OptExecutionScannedRowsLimit` / `…BytesLimit`
  (`:1657-1664`).

So the time budget **must** be transaction-scoped — it is the FDB 5-second wall,
which is a property of the transaction and of nothing else; a per-page 4s budget
inside a shared transaction is simply wrong arithmetic. The record/byte counters
are shared to match Java's `ExecuteState`, and because they are opt-in the
tightening reaches only callers who armed them — for whom Java's behaviour is
shared too.

Gated explicitly by a **three-statement transaction** test: three statements in
one transaction whose combined scanned records cross an armed limit must trip on
the third, where per-page counters stay green. Three, not two: with two
statements a shared counter and a per-statement counter can be distinguished only
if the limit sits between them, and the test would silently become a test of
where the limit was placed.

### 6. The in-transaction pagination boundary — Java's clean stop, with `40001` as a named interim

Revision 1 said: when the transaction budget is gone, the next page fails with
`ErrCodeSerializationFailure` (40001). That is wrong as an end state, for two
reasons, and revision 1 cited neither.

**First, it contradicts merged RFC-203.** RFC-203 §4.2 preserves transparent
transaction rollover "unchanged", and its G12 requires a long scan to return the
complete row set "with no user-visible token and no error". Both were written
unscoped. Read literally they forbid exactly the boundary this RFC needs, and
neither document cited the other. **Resolved by amendment:** RFC-203 §4.2.1 now
scopes rollover preservation and G12 to **auto-commit**, on the ground that
rollover and an explicit transaction are mutually exclusive by construction —
rollover means "open a fresh transaction and keep scanning", and inside `BEGIN`
there is no fresh transaction to open without committing the defect this RFC
removes. RFC-203 §4.2.2 adds the transaction-bound token fence (Decision 9), and
G12a/G12b are the in-transaction gates.

**Second, Java does not raise an error here.** A record-layer limit inside an
explicit transaction stops the cursor **cleanly**, in three verified parts:

- `RecordLayerIterator.fetchNextResult` (`:84-97`) turns any `NoNextReason`
  other than `SOURCE_EXHAUSTED` into
  `ContinuationImpl.fromUnderlyingBytes(result.getContinuation().toBytes())`
  (`:94`) — a continuation, not an exception.
- `RecordLayerResultSet.advanceRow` (`:77-87`) guards every `next()` with
  `currentCursor.hasNext()`, so `ResultSet.next()` returns **false**.
- `RecordLayerResultSet.close` (`:95-97`) commits only
  `if (connection.canCommit() && connection.inActiveTransaction())`, and
  `canCommit()` is false whenever `autoCommit` is off
  (`EmbeddedRelationalConnection.java:179-182`) — so **the transaction stays
  open** across the page boundary.

The reason code is `Continuation.Reason.TRANSACTION_LIMIT_REACHED`
(`RecordLayerResultSet.continuationReason()`, `:121-135`, specifically
`:128-129`).

One precision, because the natural phrasing is wrong and would mislead an
implementer: it is `hasNext()` / `ResultSet.next()` that reports false, **not**
`RecordLayerIterator.next()`. That method *throws*
`ErrorCode.EXECUTION_LIMIT_REACHED` when `terminatedEarly()` (`:115-116`;
`terminatedEarly()` at `:123-128` is true for the TIME/BYTE/SCAN limits and
false for `RETURN_LIMIT_REACHED` and `SOURCE_EXHAUSTED`). The clean stop is a
property of the `hasNext`-guarded caller, not of the iterator.

**And raw `1007` is Java's behaviour only when no limit is armed** — which is
Java's default (`Options.java:294`) but never Go's (Decision 5a). In that case
`FDBStoreTransactionIsTooOldException` reaches `ExceptionUtil.java:59-85`, which
has lanes for the timeout exception (`:64`), `RecordContextNotActiveException`
(`:66`), deserialization (`:68`), uniqueness (`:70`), metadata (`:72`), semantic
(`:75`, `:77`) and planning (`:79`) — and **none** for it, so `code` stays
`ErrorCode.UNKNOWN` from `:63` through the `:84` return. The class hierarchy
makes this real rather than an oversight of reading:
`FDBStoreTransactionIsTooOldException extends FDBStoreRetriableException`
(`FDBExceptions.java:130`), *not* `FDBStoreTransactionTimeoutException`
(`:120`), so the timeout lane does not catch it.

**The decision, in two states.**

**End state — a clean stop.** An in-transaction page that ends on an out-of-band
budget yields a continuation with reason `TRANSACTION_LIMIT_REACHED`, the
transaction stays open, and the caller resumes with that token *in the same
transaction*. Java's shape, Java's reason code, and correct: the transaction's
5-second wall still applies to the whole sequence, so the resume is bounded — it
is a boundary the caller can see and act on, not an unbounded escape hatch.

**Interim — `40001`, named as such.** The end state needs a surface that does not
exist yet: `cascades_generator.go:1215-1218` still rejects any caller-supplied
`CONTINUATION` loudly —

```go
if c.Options().Get(api.OptContinuation) != nil {
```
→ `ErrCodeUnsupportedOperation`, "statement continuations are not supported: Go
SQL tokens are engine-private and no resume entry point exists"

— and RFC-203, though merged, is **not implemented at this head**; I verified
that rejection is still live. Until it is, an in-transaction scan that outgrows
its budget genuinely has nowhere to go, and the honest answer is an error.

So the interim: the next page fails with `ErrCodeSerializationFailure` (40001)
and a message naming the 5-second window. It is the same code `translateFDBCode`
already assigns to FDB's own `transaction_too_old` (1007) at
`connection.go:676-677`, so retry logic is uniform whether the driver pre-empts
or FDB does. It is not 54F01: 54F01 means "raise your limits", and no limit the
caller can raise makes an FDB transaction live longer than 5 seconds.

Pre-emption is also required in the interim to keep the liveness tripwire honest.
Without a pre-page check, an exhausted budget produces a page with zero rows and
an unadvanced continuation, which trips `"query cannot progress under the
configured per-page resource limits"` (`cascades_generator.go:1843-1845`) — a
54F01 telling the user to raise limits that are not the problem.

**Retirement condition, stated so it cannot rot into permanence.** The 40001
interim is deleted — not amended — when RFC-203's in-transaction continuation
surface lands: specifically when `OptContinuation` is accepted for a
transaction-bound token and RFC-203's **G12a** (in-tx clean stop with
`TRANSACTION_LIMIT_REACHED`, transaction stays open, resume returns the
remaining rows with no gap and no duplicate) and **G12b** (a transaction-bound
token is refused outside its transaction) are green. At that point the pre-page
check becomes a boundary rather than an error, and the test written for the
interim is *rewritten*, not deleted — its scan shape is what proves the boundary
lands in the same place.

### 7. `Commit` and `Rollback` get error translation, and `1021` gets a decided meaning

Two independent gaps, both made live by this RFC.

**(a) `embeddedTx.Commit` bypasses translation entirely.** `connection.go:181-185`:

```go
func (tx *embeddedTx) Commit() error {
	err := tx.rctx.CommitWithHooks()
	tx.conn.activeTx = nil
	return err            // raw — never reaches translateFDBError
}
```

`translateFDBError` (`:686-738`) has the `wire.FDBError` / `fdb.Error` lanes at
`:725-732`; `Commit` simply never calls it. So a 1020 conflict on the canonical
`tx.Commit()` idiom reaches the application **with no SQLSTATE at all**. That is
latent today because in-tx reads add no conflict ranges — which is precisely
what this RFC changes, and this RFC is what makes commit-time conflicts the
normal way the new isolation is observed. `Rollback` (`:188-192`) returns `nil`
unconditionally and discards whatever `Cancel()` reported. Both route through
`translateFDBError`.

**(b) `translateFDBCode` has no `1021` lane, and no `1025` either.**
`connection.go:670-682` in full has exactly four: `1031` → 53F00, `1020` →
40001, `1007` → 40001, `2017` → 25F01. Unrecognised codes fall through unchanged
at `:681`. Added:

- **`1025 transaction_cancelled` → 25F01.** The code a rolled-back context
  returns (`pkg/fdbgo/client/transaction.go:613-618`, returning
  `&wire.FDBError{Code: 1025}` at `:615`).
- **`1021 commit_unknown_result` → a new, distinct SQLSTATE.**

**What `1021` means on a non-retrying explicit transaction — decided.** Java is
no help here, and the reason is worth stating because "match Java" is the default
and it does not apply: Java has zero retry machinery for these transactions (the
grep is above), and 1021 has no `case` in `FDBExceptions.java:191-210`, so it
falls to `:204-206`'s `default: if (fdbex.isRetryable())` → `FDBStoreRetriableException`
→ `ExceptionUtil`, which has no lane for it → the application sees
`ErrorCode.UNKNOWN`. Java tells the caller nothing.

Go beats Java here, at no wire cost, because this is a read-side error-surface
question. **`1021` maps to `40003` — SQLSTATE class 40 (Transaction Rollback),
subclass 003, STATEMENT COMPLETION UNKNOWN.** That is the SQL standard's own
code for exactly this condition, so it is a port of the standard rather than an
invention, and it is distinguishable from `40001`: `40001` means "this
transaction definitely did not commit, re-run it"; `40003` means "**the outcome
is unknown** — the transaction may or may not have committed, and re-running it
is not automatically safe."

The documented meaning that ships with it: **on `40003` the application must
determine the outcome before retrying.** A blind retry is a double-apply for any
non-idempotent statement — which is not hypothetical, it is
`TestSQLFault_UpdateRelative_DoubleApply` (Decision 7a).

**`40003` is a Go-only code and the build already knows how to hold that.**
Java's `ErrorCode` enum has no `40003` — the pinned snapshot in
`pkg/relational/api/errcode_java_parity_test.go` is the authority, and
`TestErrorCodesMatchJava` fails if the Go/Java difference is not **exactly**
`goOnlyErrorCodes`, in both directions. So implementing this means registering
`ErrCodeStatementCompletionUnknown = "40003"` in the const block **and** in
`init()` **and** in `goOnlyErrorCodes` with its justification, or the build goes
red. That is the right amount of friction for adding a code Java lacks, and it
is called out here so it is not discovered as a surprise red at implementation
time. It is a read-side extension with no wire implication, which is the
category CLAUDE.md permits with deep coverage — criterion 9(d) and criterion 10
are that coverage.

#### 7a. An explicit transaction does NOT auto-retry — the ruling, and its two booked inputs

**An explicit transaction does not auto-retry, and `1021` on `COMMIT` surfaces
as `40003` with the application owning idempotency.** That is what explicit
transactions are *for*: the application took control of the transaction
boundary, so it also took the retry decision. A driver cannot retry on its
behalf, because retrying an explicit transaction means re-executing statements
the driver does not have — the application issued them one at a time and the
driver never retained them. This is the same conclusion Decision 1 reaches from
the read side, arrived at independently.

Two tests are booked as this RFC's **problem-statement inputs**
(`TODO.md:6891-6896` says so explicitly, and their own file header at
`pkg/relational/sqldriver/sim_sql_fault_test.go:3-30` says "READ THESE AS A
PROBLEM STATEMENT, NOT AS BUGS UNDER TEST"). Their expected post-RFC state:

| test | site | expected after this RFC |
|---|---|---|
| `TestSQLFault_UpdateRelative_DoubleApply` | `sim_sql_fault_test.go:92`, injects `CommitUnknownApplied` at `:114` | **UNCHANGED, stays green as written.** It is an **auto-commit** path; this RFC does not touch auto-commit. It keeps asserting `a == 102` (double apply) and keeps failing loudly if it ever becomes `101`. |
| `TestSQLFault_InsertDurablyCommitted_Spurious23505` | `:156`, injects at `:162` | **UNCHANGED, stays green as written.** Also auto-commit. Keeps asserting the spurious `23505` and `COUNT(*) == 1`. |

**Neither hazard is fixed by this RFC, and neither is made worse.** They are
auto-commit hazards: a statement whose write is durable while the client is told
the outcome is unknown, inherited from FDB and matching Java. This RFC's answer
is not to fix them but to give the explicit-transaction path a *better* surface
than auto-commit has — `40003` with a documented meaning, instead of a
statement-level error whose relationship to durability is unstated. An
application that needs the guarantee uses an explicit transaction and reads
`40003`.

Both tests carry failure messages instructing a future reader to "Update RFC-198"
if their behaviour moves (`:121`, `:131`, `:168`). This is that update, and the
answer is: the behaviour has not moved, and these two remain the inputs.

### 8. Every read outside the transaction, enumerated with a routing decision

Revision 1 listed two carve-outs and called them "the" carve-outs. The
enumeration was incomplete, and one of the omissions refutes the rationale the
other two rested on. The full table, each verified at this head:

| # | site | today | decision |
|---|---|---|---|
| a | `fetchPage` | `cascades_generator.go:1759-1762` — `DB.Run` for SELECT | **ROUTE IN** (Decision 1) |
| b | `cachedLoadSchema` | `connection.go:212-237`; separate `DB.Run` at `:224` **when `activeTx != nil`** | **ROUTE IN** (8b) |
| c | `ensureMetaData` | `connection.go:290` — unconditional `DB.Run`; nests b's `DB.Run` inside it when a transaction is open | **ROUTE IN** (8b) |
| d | six `system_tables.go` sites | already `runInTx` — `execSysSchemata:133`, `execSysTables:169`, `execSysColumns:230`, `execSysIndexes:310`, `execShowDatabases:397`, `execShowSchemaTemplates:423` | **STAY IN** (8d) |
| e | `fetchTableStatistics` | `cascades_generator.go:1981`; `DB.RunRead` at `:1999`, `Snapshot().Get` at `:2007` | **STAY OUT** (8c) |
| f | `ensureCatalogInit` | `ddl.go:946-964`; read-**write** committing tx, `DB.Run` at `:952`, `txn.Commit()` at `:957` | **STAY OUT** (8e) |
| g | `runDDL` | `ddl.go:975-988`; `DB.Run` at `:979`, `txn.Commit()` at `:985` | **STAY OUT** (8f) |

#### 8a. Two of revision 1's citations were wrong, and one of its facts was too

Recorded because the brief inherited them: `ensureCatalogInit` is **not** at
`ddl.go:860` (that is the closing brace of `parseColumns`) and `runDDL` is
**not** at `:886-894` (that is inside `parseColumnType`). More substantively:
`ensureCatalogInit` is **not** "a committing transaction on every
`QueryContext`". It is guarded by `c.sess.CatalogMu` + `c.sess.CatalogReady`
(`:947-951`), so the FDB round-trip happens **once per session**; what happens on
every call is a mutex acquisition. It *is* reached from `QueryContext`
(`connection.go:407`), `PlanExplain` (`:574`), `Ping` (`ddl.go:971`) and
`runDDL` (`:976`) — and notably **not** from `ExecContext`. The correction
narrows the problem substantially and it should not be carried forward
uncorrected.

#### 8b. The catalog binds to the transaction — Java's shape, decided not inherited

Revision 1 kept the catalog carve-out and called it "the pre-existing one that
already governs in-tx DML… this RFC neither widens nor narrows it." **It widens
it**, and that is the finding revision 1 missed.

Today the carve-out governs DML only, and DML's own data reads are already in the
transaction — so plan and data come from the same place for the only statements
that matter. After Decision 1, a SELECT's **data** is read at the transaction's
read version while its **plan** was built from a catalog read at a *different*
transaction's read version. That skew is new, and its failure mode is silent:
a plan built against catalog snapshot A, executed over data at snapshot B, can
scan an index that does not exist at B, or read a record type whose fields have
moved, and return **zero rows with no error**. A carve-out that was
sound-because-unreachable becomes reachable precisely because of this change.

**Java binds the catalog to the transaction.** `RecordLayerSchema.loadStore`
(`:98-108`) asserts `conn.inActiveTransaction()` at `:101`, caches
`currentStore` per transaction (`:102-104`), and registers
`conn.addCloseListener(() -> currentStore = null)` (`:106`) so the binding dies
with the transaction; `RecordContextTransaction.java:88-96` keeps the bound
schema template in the *context's* session (`context.putInSessionIfAbsent`),
not in a connection-level cache.

**Decision: bind the catalog read to the transaction** — sites (b) and (c) both
join `runInTx`. Java's shape, and the ruling's default.

The cost is accepted with its eyes open: catalog reads now add read conflict
ranges over the catalog subspace, so a concurrent DDL that commits during an
explicit transaction will conflict it with `40001`. That is the correct outcome —
a transaction whose plan was built against a schema that has since changed
*should* fail rather than return wrong rows — and it is exactly what Java does.
Two things bound the cost: DDL is rare relative to DML, and the conflict is a
loud `40001` an application can retry, not a silent wrong answer.

Note also that binding the catalog does **not** make DDL transactional: `runDDL`
auto-commits regardless (8f). The two questions are independent and revision 1
was right about that much.

#### 8c. Statistics stay out, and the reason is sharper than "not part of the read set"

`fetchTableStatistics` reads per-type record counts via `DB.RunRead` with
snapshot gets. It stays in its own transaction. The reason is not
taste: a per-type record count is a single key that **every insert of that type
mutates**, so conflicting on it would make every explicit transaction conflict
with every insert in the database — a guaranteed conflict, not a probabilistic
one. Cost-model inputs are advisory; a stale count changes which plan is chosen,
never which rows are returned. Contrast this with the catalog (8b), where a
stale read changes the *answer*. That is the line between the two decisions, and
it is the line, not the fact that both are "planning-time".

#### 8d. The six system-table sites stay in — and they refute the old rationale

All six already call `runInTx` and therefore **already** join the user's
transaction and already add catalog read-conflict ranges when one is open. This
is the fact that decides 8b. The old rationale for the catalog carve-out was
"catalog reads must not conflict with concurrent DDL" — but six catalog read
paths have been conflicting with concurrent DDL all along. The carve-out was
never a policy; it was an inconsistency with a rationale attached after the fact.

They stay in, and 8b brings the remaining catalog reads into line with them.
**One policy: catalog reads join the transaction.**

They also inherit Decision 3's fix: their `runInTx` fallback is as silent as
SELECT's, which is why that fix is written once for all routes rather than at the
SELECT call site.

#### 8e. Catalog bootstrap stays out — because it is a WRITE, and rollback would lie

`ensureCatalogInit` is not a read. It calls `c.sess.Catalog.Initialize(txn)`
(`ddl.go:954`) and commits (`:957`). Joining it to the user's transaction would
put a catalog-bootstrap **write** inside `BEGIN`, and the failure mode is
specific: the user issues `ROLLBACK`, the bootstrap write is undone with it, but
`c.sess.CatalogReady` was already set in memory — so the session now believes
the catalog is initialised when nothing was written, and every subsequent
statement on that connection operates against an uninitialised catalog. Session
bootstrap is not the user's transaction's business. It stays out, and the
in-memory flag is why, not merely "it's DDL-ish".

#### 8f. DDL stays out, unchanged and still divergent

`runDDL` auto-commits in its own transaction even inside `BeginTx`. Java runs DDL
in the connection transaction. This is a real divergence, it is pre-existing,
this RFC does not change it in either direction, and it keeps its own item. It is
listed here so the enumeration is complete, not because anything happens to it.

### 9. A continuation minted inside a transaction is transaction-bound

Decision 6's end state hands the caller a token from inside a transaction, and a
token is bytes: nothing stops it from being redeemed somewhere else.

```sql
BEGIN;
SELECT ... /* MAX_ROWS 10 */;   -- token minted at this transaction's read version
COMMIT;
EXECUTE CONTINUATION ?;          -- would resume at a DIFFERENT read version
```

That splices one logical result set across two read versions and returns a set
that never existed at any instant — non-repeatable against itself, with no error.
It is the rejected alternative "let pages ≥ 2 open their own transactions"
reached through a door the RFC-203 per-path fence does not cover, because both
halves are the same engine.

**The rule: a continuation minted while an explicit transaction was open is
redeemable only inside that same transaction. Redemption anywhere else REFUSES,
loudly.** Not a warning, not a silent fresh read — `24F00 INVALID_CONTINUATION`.

The mechanism lives in RFC-203 §4.2.2 (two layers: the `"GO_V0_TX"` mode value
on the wire, which no entry point outside the minting transaction accepts, and
the `*embeddedTx` pointer identity in process), and its gate is RFC-203 **G12b**,
whose three mutation directions cover the three independent ways the fence can be
wrong. `24F00` and not `25F01`, because the failing case includes a perfectly
active *different* transaction; the cursor state is invalid, the transaction is
not.

### 10. The per-transaction store cache — Java's, and what it costs to skip

`fetchPage` rebuilds the entire record store on **every page**
(`cascades_generator.go:1768-1772`):

```go
store, storeErr := c.newStoreBuilder().
    SetContext(rctx).
    SetSubspace(r.ss).
    SetMetaDataProvider(c.cachedMetaData()).
    Open()
```

Only `r.continuation`, `r.execState` and `r.emitted` survive a page; `evalCtx`
(`:1777`), `props` (`:1783`), the scalar subqueries (`:1784-1797`) and the whole
cursor hierarchy (`:1808`) are rebuilt too.

This is free today, because each page is its own transaction and the store must
be rebuilt anyway. It stops being free the moment pages share one transaction:
each `Open()` reads the store header, so an N-page in-transaction SELECT spends N
header reads out of the same 5-second budget the query itself is competing for —
the cost lands directly on the scarcest resource this RFC introduces.

Java does not pay it: `RecordLayerSchema` caches exactly one store per
transaction (`:98-108`, `currentStore` returned at `:102-104` when already
built) and drops it when the transaction terminates (`:106`).

**Decision: port it.** The store is cached on the `*embeddedTx` and reused for
every page of every statement in that transaction, invalidated when the terminal
flag from Decision 3 is set — the same flag, so there is one lifecycle, not two.
This is Java's structure and it removes a cost this RFC would otherwise create.
Stating the divergence and its measured cost was the alternative the review
offered; it loses, because the measurement would be of a regression we chose to
ship when the fix is Java's existing shape.

## Rejected alternatives

**Snapshot reads for in-transaction SELECT.** Delivers read-your-writes, leaves
the lost update. It is the option that makes the probe test's first assertion
flip while the actual production hazard ships. Java is serializable at three
independent sites; `BeginTx` already advertises serializable. Rejected in
Decision 2.

**Expose the choice as `SET TRANSACTION ISOLATION LEVEL`.** Revision 1 rejected
this on a claim that is **false**, and the correction changes the argument
without changing the conclusion.

Revision 1 said Java "admits no other level", citing
`supportsTransactionIsolationLevel`. Two corrections. First, that method does
exist at `EmbeddedRelationalConnection.java:334-336` — but inside the anonymous
`CatalogMetaData` returned by `getMetaData()`, not in a separate metadata file —
and it reads `return getDefaultTransactionIsolation() == level;` (`:329-331`,
backed by `:89 DEFAULT_TRANSACTION_LEVEL = Connection.TRANSACTION_SERIALIZABLE`).
Second and decisively, **the grammar does offer another level**:
`RelationalParser.g4:620-632` —

```
629: transactionLevel
630:     : READ COMMITTED
631:     | SERIALIZABLE
632:     ;
```

(`READ COMMITTED` is right there; `SNAPSHOT` appears in the grammar only at
`:1359`, in a non-reserved-keyword list, and no other level appears anywhere.)

The real evidence is the **visitor fall-through**, which is the conformance
principle's own architectural signature. `BaseVisitor.java:934-936`:

```java
public Object visitSetTransactionStatement(@Nonnull RelationalParser.SetTransactionStatementContext ctx) {
    return visitChildren(ctx);
}
```

A bare `visitChildren` — as are `visitTransactionOption` (`:940-942`) and
`visitTransactionLevel` (`:946-948`). So Java **parses** `SET TRANSACTION
ISOLATION LEVEL READ COMMITTED`, visits its children, and does **nothing**: the
level is never read by anything. The statement is a silent no-op.

That is a stronger argument for not wiring it, not a weaker one. Wiring it in Go
would make Go the only engine where the statement has an effect — a Go-only
widening of the isolation surface, introduced in the same change that fixes an
isolation bug, and specifically an escape hatch back to the semantics Decision 2
rejects. Rejected.

**The residual divergence is named rather than papered over:** Go today fails
`SET TRANSACTION` with `0A000` (`administrationStatement` has no handler), where
Java silently accepts and ignores it. Go is *stricter*, which is the safe
direction, but it is a divergence from the fall-through-to-default shape the
conformance principle prescribes. It gets its own item, whose decision is
genuinely open — matching Java means silently accepting a level Java then
ignores, which is a bad behaviour to port faithfully — and it is out of scope
here. Java's `setTransactionIsolation` (`:345-348`) is a bare field store with no
validation and no effect on an in-flight transaction, and it only reaches
`ExecuteProperties` at the next `startTransaction` (`:394`); that quirk is part
of the same item and should be decided deliberately, not inherited.

**Buffer DML writes in the driver and replay them over SELECT results.** A
second, driver-local read-your-writes implementation, layered on top of the one
the client already ports from C++, correct only for the shapes someone remembers
to handle (index entries, cleared ranges, atomic ops, split records). The
transaction already does this. Rejected.

**Materialize the whole in-tx result set inside the transaction, then serve it
after.** Bounds every in-tx SELECT by memory instead of by the transaction, and
breaks the RFC-130 statement memory budget's meaning. It also *hides* the
5-second limit rather than reporting it, which is the failure mode the FAQ
explicitly tells users to design around (`docs/sphinx/source/FAQ.md:173-180`:
"you need to decompose your work into a number of smaller transactions").

**Let pages ≥ 2 of an in-tx SELECT open their own transactions (today's
behaviour).** Exactly the defect, applied to page 2 onward: the tail of a result
set read outside the transaction that was supposed to contain it. Rejected in
Decision 3 — and rejected a second time, through the token door, in Decision 9.

**Set an FDB transaction timeout on the explicit transaction at `BeginTx`.**
Would give a hard stop for free (1031 → 53F00, already mapped at
`connection.go:672-673`). But the timeout is anchored at transaction creation, so
it would kill a transaction that idled after `BeginTx` before reading anything —
which FDB itself would have served, because the read version had not been taken
yet. Java sets no default either: `RecordLayerTransactionManager.java:62-70`
applies `TRANSACTION_TIMEOUT` only `if (transactionTimeout != null)` (`:66`),
and `Options.java` declares the name (`:77`) and validates it (`:545`) but never
puts it in the defaults map — so `getOption` returns null, the setter is never
called, and `FDBDatabaseFactory.java:96` stays at
`DEFAULT_TR_TIMEOUT_MILLIS = -1` (`:61`).

**Auto-retry the explicit transaction on 1020/1021.** Rejected in Decision 7a:
the driver does not hold the statements it would have to replay, and Java has no
retry machinery for these transactions either.

## Consequences to expect

**The 5-second wall becomes visible to applications, and that is the point.**
Today an in-tx SELECT over a large table paginates across as many transactions as
it needs and always "works". After this change it is bounded by the transaction
it belongs to. In the interim (Decision 6) a scan that cannot finish fails 40001;
in the end state it stops cleanly with `TRANSACTION_LIMIT_REACHED` and a token
redeemable in the same transaction. The application-facing remedy is the one the
record layer documents: decompose the work, or bound the statement so it stops
before the wall (`MAX_ROWS`, `EXECUTION_SCANNED_ROWS_LIMIT`,
`EXECUTION_TIME_LIMIT` — all already wired through `executeProps`).

**Serialization failures will start appearing where none appeared before.** Any
read-modify-write across two concurrent explicit transactions on overlapping rows
now fails one of them with 40001 instead of silently losing a write. Applications
with no retry loop will see new errors. There is no version of "correct" that
avoids this. And per Decision 1 they will see them *directly*, because the
transparent retry that used to absorb them is gone from this path.

**Isolation strength is plan-dependent, and users must be told.** Per Decision
2b: a covering-index scan conflicts on the index subspace only, so a concurrent
writer that changes a non-indexed column of a matching row does not conflict with
it. The same SQL, planned differently, has a different conflict footprint. This
is parity with Java — `KeyValueCursorBase.java:358` conflicts on whatever
subspace the cursor was built on — but it means "SERIALIZABLE" here is
serializability over *what was read*, which is FDB's model, not over *what was
logically queried*.

**A concurrent DDL can now conflict an explicit transaction** (Decision 8b), with
40001. New, deliberate, and Java's behaviour.

**Read-your-writes changes results, including through indexes.** An in-tx
`INSERT` followed by an in-tx `SELECT` that plans to an index scan must return
the new row, which requires the client's RYW merge over a *range* read, not just
a point get. An in-tx `DELETE` followed by a `SELECT` exercises the cleared-range
merge. These are the RYW paths most likely to have a gap, and they are pinned
explicitly below rather than assumed from "the client ports C++".

**Multi-statement transactions get one shared scan budget** (Decision 5a), so a
transaction whose statements previously each had 4s now has 4s in total. This is
correct — it is the FDB wall — but it is a behaviour change for the
three-statement transaction, and the bulk loader in
`pkg/relational/sqldriver/stress/stress_test.go` is one (see criterion 10).

**Auto-commit behaviour is unchanged.** With no explicit transaction, `runInTx`
still falls through to `DB.Run`, every page is its own transaction, and RFC-203's
rollover preservation applies unmodified. The two 1021 auto-commit hazards
(Decision 7a) are unchanged in both directions.

**A caller deadline still does not reach the FDB read futures on this path.** The
cursor loop honours the statement context (`ExecutePlan(r.ctx, ...)`), so
cancellation is observed between reads; the in-flight read future is bounded by
the transaction's own context, which for an explicit transaction is
`context.Background()`, the pre-existing gap documented at
`connection.go:471-481`. This RFC does not change it and does not widen it — in-tx
DML already reads on that context.

## What this does not do

- It does not make DDL transactional (Decision 8f).
- It does not implement RFC-203's continuation surface. It amends RFC-203, states
  the interim, and names the retirement condition (Decision 6).
- It does not add a SQL isolation-level surface (Rejected alternatives).
- It does not change auto-commit's cross-transaction pagination.
- It does not fix the two 1021 auto-commit hazards (Decision 7a).
- It does not touch the wire. No key encoding, record/index format, continuation
  encoding, or committed byte changes. The `"GO_V0_TX"` mode value (Decision 9)
  is a *value* in an existing string field, not a schema change — RFC-203 §3.2's
  scope-by-value-never-by-schema constraint holds.

## Verification plan

The harness is **SimFDB + `dst.Env`**, not wall-clock FDB timing. It exists, it
already names this RFC, and it is what makes the conflict verdicts this RFC
turns on deterministic rather than lucky.

### The harness, verified

- `pkg/simfdb/conflict.go` injects the codes: `1020` (`:167`), `1007` (`:170`),
  and `1021` with **both** branches resolved at `:180-187` — `CommitUnknownApplied`
  (mutations durable, `:199-224`) and `CommitUnknownDiscarded` (nothing written,
  `:194-197`). The constants are `pkg/simfdb/simfdb.go:102-103` (sentinels `-1021`
  / `-1022`, deliberately outside FDB's code space). `InjectOnce(code int)` is
  `simfdb.go:111`.
- **`InjectOnce` does not perturb the seeded schedule**: `conflict.go:140-154`
  draws all three seeded values unconditionally *before* the injection check.
  That is what makes an injected criterion still a deterministic replay.
- `conflict.go:215` already names this RFC: the applied branch "returns SUCCESS
  having written nothing. An explicit-transaction COMMIT retry — the RFC-198 path
  — lands exactly there and silently loses the whole transaction."
- `pkg/simfdb/explicit_tx_env_test.go:24-25` names it too: "the explicit-COMMIT
  path is exactly the one a transaction-semantics RFC needs to replay."
  `TestExplicitTransactionCarriesTheEnv` (`:29`) and
  `TestExplicitTransactionIsDrivenByTheSimClock` (`:71`) are the wiring proofs.
- `pkg/simfdb/commit_unknown_test.go:73`
  (`TestReCommitAfterCommitUnknownResendsTheBuffer`) is an existing RFC-198 pin
  that lives outside the SQL fault file; the plan accounts for it rather than
  undercounting the evidence base.

**`dst` has no `Sleep`, and every criterion is written accordingly.**
`pkg/dst/clock.go:24-27` says the timer surface (`After`/`NewTimer`/`Sleep`)
is deliberately off the `Clock` interface until a later track; `Clock` is
`Now()` only (`:28-32`). So a "budget expires" criterion is written as
`env.Clock.(*dst.SimClock).Advance(d)` (`clock.go:66`) or `.Set(t)` (`:76`), and
the budget-consuming code reads elapsed time through `Env.Since`
(`pkg/dst/env.go:57`). **No `time.Sleep` appears in any criterion.** Revision 1's
criterion 6 had one; it is gone.

**The scan-limiter seam is MANDATORY.** Any function that arms a time limit must
build its properties with `DefaultExecutePropertiesIn`
(`pkg/recordlayer/scan_properties.go:245`), which threads
`NewScanLimiterStateIn(env)` (`scan_limiter_state.go:130`). This is not advice:
`pkg/docscheck`'s `TestScanLimiterStateArmingIsSeamed`
(`dst_seam_gate_test.go:227`) walks the AST and **fails the build** if any
production function calls `WithTimeLimit` without calling
`DefaultExecutePropertiesIn` **in the same function body**, and it has an
anti-vacuity floor (`:296-302`) that hard-fails if it finds zero arming sites.
The transaction-scoped `ScanLimiterState` of Decision 5 is therefore **minted
through `NewScanLimiterStateIn(env)`**, never `NewScanLimiterState()`, and the
`env` comes from the same `r.env()` that `executeProps` already uses at
`cascades_generator.go:1633`. (Revision 1 cited that line as
`DefaultExecuteProperties()`; the seamed constructor is already there.)

### Criteria

1. **`tx_select_isolation_probe_test.go` flips.**
   `no_read_your_writes_in_explicit_tx` (`:46`) becomes
   `read_your_writes_in_explicit_tx` asserting **777** at `:60`, and the
   `// DIVERGENCE:` header comment at `:59` is deleted, not amended.
   `dml_still_atomic_on_commit` (`:67`) and `dml_undone_on_rollback` (`:88`) stay
   green unchanged — the controls proving the write side was not disturbed.
2. **The lost update becomes a 40001**, in the exact shape of the table above:
   T1 `BeginTx` → `SELECT v` → T2 (separate conn) `UPDATE` and commit → T1
   `UPDATE` → T1 `COMMIT` must fail with 40001. *Mutation direction:* forcing
   `IsolationLevelSnapshot` on the in-tx read path must make this test go
   **green-wrong** (commit succeeds, write lost) while criterion 1 still passes —
   that is the pin that makes snapshot-vs-serializable a *tested* decision rather
   than a prose one. **Kept exactly as revision 1 wrote it.**
3. **Read-your-writes through an index, not just a scan.** In-tx `INSERT` then an
   in-tx `SELECT` with an `EXPLAIN` assertion that the plan is an index scan,
   returning the uncommitted row. A full-scan plan would pass while the index RYW
   path is broken, so the plan shape is part of the assertion.
4. **Read-your-writes over a cleared range.** In-tx `DELETE` then in-tx `SELECT`
   returns nothing, under both a record scan and an index scan.
5. **The result set cannot outlive its transaction — all four doors.** Open an
   in-tx SELECT large enough to need a second page, end the transaction, then
   iterate: must be 25F01, never rows from a fresh transaction. Four cases, one
   per door in Decision 3's table: raw `COMMIT` via `ExecContext`
   (`connection.go:619-645`), raw `ROLLBACK`, **`Close`** (`:451-459`), and
   `ResetSession` (`:532-547`). The `Close` and `ResetSession` cases are the ones
   that currently bypass `Rollback` entirely, so a flag set only in
   `Commit`/`Rollback` passes the first two and fails these. *Mutation
   direction:* restoring the `runInTx` nil-fallback for the pinned transaction
   must redden these and only these.
6. **Whole-transaction time budget, env-clocked.** An in-tx SELECT whose scan
   needs more than the remaining budget fails 40001 naming the 5-second window —
   not 54F01, and not the liveness tripwire's "query cannot progress" message
   (`cascades_generator.go:1844-1845`); assert on the code **and** that the
   message is not the tripwire's. Time is moved with
   `SimClock.Advance`, never `time.Sleep`. *Mutation direction:* reverting the
   anchor to per-page must make this test observe FDB's raw 1007 instead, which
   is the pre-emption's whole reason to exist. Second case, pinning that the
   anchor is the read version and not `BeginTx`: `BeginTx`, `Advance` past the
   budget **with no statement issued**, then a small SELECT — must succeed.
7. **The three-statement shared-budget gate** (Decision 5a). Three statements in
   one transaction whose combined scanned records cross an armed
   `OptExecutionScannedRowsLimit` must trip on the **third**; per-page counters
   stay green through all three. Companion, pinning the split Decision 5 names:
   **two statements in one transaction each get a FULL RFC-130 memory budget** —
   `ExecuteState` is statement-scoped and must not be dragged along. *Mutation:*
   share `props.State` across the transaction → the second statement fails for
   memory the first released.
8. **MAX_ROWS still bounds an in-tx SELECT's page scan.** The regression Decision
   4 exists to prevent: assert the page cursor's returned-row limit is set for an
   in-tx SELECT with `MAX_ROWS`, and that an in-tx DML's is not. A rows-only test
   cannot see this, so it asserts on the bound, not the output.
9. **FDB error translation, on every route this RFC touches.** Four assertions,
   because they are four independent gaps: (a) a conflict on page ≥ 2 surfaces as
   40001, pinning Decision 7's `translateExecError` tail — the function is
   `cascades_generator.go:1880-1970` and its final `return err` at `:1969` has no
   FDB lane today; (b) a page fetched on a cancelled context surfaces as 25F01,
   pinning the new `1025` lane; (c) **`tx.Commit()` on a conflicted transaction
   surfaces as 40001** — today it returns raw (`connection.go:181-185`), so this
   is red before the fix; (d) **`tx.Commit()` under an injected `1021` surfaces
   as `40003`**, pinning the new lane and Decision 7a's ruling.
10. **The 1021/1007 injection criteria, meeting the booked bar.** `TODO.md:6878-6889`
    requires 1007 **and both** 1021 branches by explicit `InjectOnce`, "never by
    natural occurrence", because SimFDB's version counter has no relationship to
    time (so 1007 is naturally unreachable) and the 1021 branch is a per-seed
    coin (so a seed sweep certifies whichever branch it drew). On the
    **explicit-transaction commit path**:
    - `InjectOnce(1007)` → the transaction fails with 40001, no rows lost, no
      silent retry.
    - `InjectOnce(simfdb.CommitUnknownApplied)` → `40003`; the mutations are
      durable; a re-run without an idempotency check double-applies, and the test
      asserts that as the documented consequence rather than hiding it.
    - `InjectOnce(simfdb.CommitUnknownDiscarded)` → `40003`; **nothing was
      written**; this is the branch a naive retry must be safe on and a seed
      sweep would have missed.
    The two `40003` cases must be separate tests: a single test that accepts
    either branch is exactly the "certifies whichever branch it drew" failure the
    booked bar names. **Note the bar also requires 1007, which nothing in
    `sim_sql_fault_test.go` reaches today** — that arm is net-new work, not a
    rename of an existing case.
11. **The four existing `sim_sql_fault_test.go` cases, stated explicitly.** None
    is edited; all four are auto-commit and this RFC does not touch auto-commit:
    `TestSQLFault_UpdateRelative_DoubleApply` (`:92`) and
    `TestSQLFault_InsertDurablyCommitted_Spurious23505` (`:156`) stay green as
    written (Decision 7a); `TestSQLFault_1021HazardsAreDeterministic` (`:207`,
    seed 7, three runs) stays green — it is a determinism property, not a hazard;
    `TestSQLFault_DiscardedCommitUnknownAppliesExactlyOnce` (`:257`) stays green
    — it is the control. Criterion 10 adds **explicit-transaction siblings**
    beside them; it does not convert them. If any of the four moves, the RFC's
    problem statement moved and this document is wrong, which is what their own
    failure messages (`:121`, `:131`, `:168`) already instruct a reader to
    conclude.
12. **Auto-commit is untouched.** The existing auto-commit SELECT suite
    (including pagination and `resource_limits_test.go`) stays green with no
    expectation edits, and RFC-203's **G12** (rollover transparent in
    auto-commit) stays green.
13. **1M stress before and after — with the control corrected.** Revision 1 said
    "the auto-commit path should be unmoved; a difference there means the routing
    split leaked." **That control is wrong.** The loader is
    `stress_test.go`'s `bulkInsert` (`:162`), and it is an **explicit-transaction
    workload**: `h.db.BeginTx(...)` at `:199`, then `batchesPerTx` multi-row
    `INSERT` statements (`:203-216`), then `tx.Commit()` (`:218`). It is squarely
    in this RFC's blast radius, not outside it. Two corrections:
    - The expectation is **not** "unmoved". Each transaction's `batchesPerTx`
      statements now share one 4s budget (Decision 5a). If `batchesPerTx` is high
      enough that the shared budget bites, the loader must be re-tuned, and that
      is a finding about the change, not a regression to chase.
    - Its retry loop (`:197`, `:223`) is a blind five-attempt
      `time.Sleep((attempt+1) * time.Second)` with **no error-code
      discrimination**. It will silently swallow the new 40001s this RFC
      introduces and report a throughput number instead of a correctness signal.
      It is made error-code-aware before the comparison is run, or the comparison
      measures nothing.
    The genuine auto-commit control is criterion 12's suite, which is
    auto-commit by construction.

## Open questions — what I could not establish by reading

Stated so they are not discovered as surprises, and so a reviewer can attack the
right things. (Revision 1 numbered these 1, 3, 4 — there was no 2. Fixed.)

1. **The anchor instant — DECIDED, with a named `pkg/fdbgo` deliverable.**
   Revision 1 left this open and proposed "the first statement execution against
   the transaction" as a proxy for the GRV instant, arguing that opening a store
   reads the header and is therefore within one round trip of it.

   **The proxy is refuted and the decision is made: anchor on the client's
   read-version instant.** The proxy fails because a first statement need not
   take a read version at all — a read fully covered by the transaction's own
   staged writes is served from the RYW buffer, and a store open whose state is
   already in the store-state cache reads nothing. In both cases the proxy fires
   while the GRV has not happened, so the budget starts counting against a
   5-second window that has not opened. It is not a small skew; it is the wrong
   event.

   **The deliverable, named:** `pkg/fdbgo/client` records the instant at which the
   read version is taken and exposes an accessor. The site already exists —
   `transaction.go:697` stamps `metricStart = time.Now()` inside
   `ensureReadVersion` (`:662`) under `readVersionMu` (`:690`), at exactly the
   right moment — but that field is metrics-owned and unexported, and there is
   **no accessor for it or for anything equivalent**; the exported time-ish
   surface is `GetReadVersion`, `SetReadVersion`, `SetTimeout`,
   `GetCommittedVersion` and nothing else. So the change is one field plus one
   accessor, stamped under the same mutex, with the env clock rather than
   `time.Now()`. It is small; it is a **client/wire-tree change and takes its own
   review lap** under the client gate (FDB C++ developer + Torvalds), not this
   RFC's. It is a hard dependency of Decision 5: without it the transaction-scoped
   time budget has no correct instant to anchor on.

2. **Whether any in-tx read path bypasses `ExecuteProperties.IsolationLevel`.**
   The `isSnapshot()` sites cover the leaf cursors, but an unconditional
   `Snapshot()` call reached from a SELECT plan would be a silent hole.
   `range_set.go`, `bunched_map.go`, `index_state.go`, `database.go` and
   `aggregate_function.go` all take snapshots unconditionally; each is
   index-maintenance, store-state or ranked-set internals rather than a query
   leaf, and each is paired with an explicit conflict key where Java pairs one —
   which is the "SNAPSHOT + targeted conflict key" row of Decision 2a's table, a
   correct pattern and not a bug. But "reached from a SELECT plan" is a
   reachability claim, and reachability claims get a test. A negative-result test
   enumerating the query leaves and asserting each consults the isolation level
   is the pin, with a failure message naming what gets re-armed if a leaf ever
   stops consulting it.

3. **The conflict-range volume of a realistic in-tx SELECT.** Decision 2 argues
   the new conflict surface is legitimate; Decision 2b establishes that it is
   plan-dependent and, for a full table scan, large. The *rate* at which real
   workloads will see 40001 is empirical. The 1M stress run answers it for one
   workload and no more — and per criterion 13 that workload is itself an
   explicit-transaction one, so it is a live sample rather than a control.

4. **Whether the per-transaction store cache (Decision 10) changes any observable
   plan or result.** It should not — Java caches the same object — but a store
   built once and reused across statements in a transaction is a lifetime change,
   and the store-state cache interaction with a long-lived transaction is the
   place a stale read would hide. Measured at implementation, with the terminal
   flag's invalidation as the thing under test.

## Citations re-verified

Every `file:line` in revision 1 was re-checked at this head (`f5c2c7f0e` for Go,
tag 4.12.11.0 for Java). The corrections are folded into the text above; this
records the ones a reviewer carrying revision 1's numbers would otherwise trip
over, and the claims that were **refuted** rather than merely relocated.

**Refuted outright** (not a line-number slip — the claim was wrong):

- `ensureCatalogInit` is **not** "a read-write committing transaction on every
  `QueryContext`". The `CatalogReady`/`CatalogMu` guard (`ddl.go:947-951`) makes
  the FDB round-trip **once per session**. (Decision 8a.)
- Revision 1's "Java admits no other isolation level" — the grammar offers
  `READ COMMITTED` (`RelationalParser.g4:629-632`). The real evidence is the
  visitor fall-through. (Rejected alternatives.)
- Revision 1's Decision 2(c), "exactly these ranges over exactly these keys" —
  same machinery, different keys. (Decision 2.)
- Revision 1's "**The executor plumbing this needs is zero**" — contradicted by
  the per-page store rebuild. (Deleted; Decision 10.)
- Revision 1's `ExecuteProperties.java:334-336` for "declares SERIALIZABLE the
  only supported level" — that range is the protected `copy(...)` overload
  (`:333-338`), unrelated. The line numbers were a stray duplicate of the
  `EmbeddedRelationalConnection.java:334-336` citation.
- Revision 1's `FDBRecordContext.java:676` for `ensureActive()` — `:676-678` is
  `getTransactionCreateTime()`; `ensureActive()` is `:546`.
- Revision 1's header booking `TODO.md:6616` — that is an unrelated
  `fdbgo/client: rywDisabled` item. The booking is **`TODO.md:6846`**.
- The review brief's "`sim_sql_fault_test.go:1021` names both branches" — the
  file is **297 lines**; `1021` is the FDB error code. The constants are
  `pkg/simfdb/simfdb.go:102-103`, the branch resolution `conflict.go:180-187`.

**Relocated** (claim right, line wrong): `cascades_generator.go` — `txPageTimeLimit`
`:1187` (was 1183-1186); `respectActiveTx` assignment `:1255` (was 1254);
`pageRowBudget` body `:1607-1608` (was 1606); the `runTx` block `:1759-1762`
(was 1753-1756); `ExecutePlan` `:1808` (was 1802); the liveness tripwire
`:1843-1845` (was 1837-1840); `translateExecError` `:1880-1970` (was 1874-1963);
`fetchTableStatistics` `:1981`/`:1999`/`:2007` (was 1993-2005); the
`OptContinuation` rejection `:1215-1218` (was 1214-1217). `ddl.go` —
`ensureCatalogInit` `:946-964` (was 860), `runDDL` `:975-988` (was 886-894).
`scan_limiter_state.go` — `NewScanLimiterState` `:118-120` and
`NewScanLimiterStateIn` `:130-131` (was 90-97), and it anchors on `env.Now()`,
not `time.Now()`. `connection.go:376`/`:413` wrap `gen.Plan`, not `plan.Execute`;
the `plan.Execute` wraps are `:384` and `:433`. Java —
`ExecuteProperties.java:416-425`/`:421` for the Builder state reference (was
415); `query/QueryPlan.java:433` for `toBuilder` (was 432, and there are two
files named `QueryPlan.java` — the relational one is meant);
`RelationalParser.g4:620-632` (was 619-632); `EmbeddedRelationalConnection.java:98-99`
for the field (was 99) and `:476-495` for `ensureTransactionActive` (was
477-487); `recordlayer/RecordLayerSchema.java` — not `recordlayer/metadata/`,
which is a different 74-line file with no `loadStore`.

**Confirmed exactly as revision 1 had them**, and load-bearing enough to say so:
`connection.go:197-202`, `:183`, `:189`, `:212-237`, `:290`, `:451-459`,
`:471-481`, `:490-497`, `:501-516`, `:532-547`, `:619-645`, `:670-682`;
`RecordLayerIterator.java:84-97`; `RecordLayerResultSet.java:121-135`;
`RecordLayerSchema.java:98-108`; `BackingRecordStore.java:235`;
`AbstractEmbeddedStatement.java:75`, `:90`; `KeyValueCursorBase.java:358`;
`FDBRecordContext.java:187`, `:659-666`; `CursorLimitManager.java:92-94`;
`Options.java:294`; `RecordContextTransaction.java:88-96`;
`ExceptionUtil.java:59-85`; `BaseVisitor.java:934-936`;
`RecordLayerTransactionManager.java:62-70`; `FAQ.md:173-180`;
`EmbeddedRelationalConnection.java:118/212/237/393`, `:125`, `:179-182`,
`:334-336`, `:345-348`, `:394`, `:510-516`, `:521`;
`ExecuteProperties.java:45`, `:402`, `:598-600`; `query/QueryPlan.java:426`.
