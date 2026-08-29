# RFC-232: one SQL execution is one FDB page

**Status:** DRAFT

**Amends:** RFC-203 §§4.1–4.5, §6 steps 1 and 4–6, and gates G2, G3,
G6, G11–G14; RFC-198 §§6 and 9.

**Does not replace:** RFC-203's decision to vendor Java's
`ContinuationProto`, make `MAX_ROWS` a per-execution row limit, or support
`EXECUTE CONTINUATION`; RFC-198's rule that a continuation produced inside an
explicit transaction is redeemable only in that same live transaction.

## Decision

A row-returning SQL execution invokes one FoundationDB data-plan page callback.
Auto-commit wraps that callback in one `DB.Run`, which may retry failed attempts
whose rows and state are discarded. An explicit transaction invokes it once on
the captured `embeddedTx`, with no retry. Either path publishes one successful
attempt and never auto-follows a continuation into a second page behind one
`Rows` object.

If the source is exhausted, the execution ends normally. With continuation
capability enabled and cooperative limits configured to stop rather than fail,
such a budget stopping the outer cursor after scalar-subquery evaluation ends
the execution at a visible page boundary and produces a self-contained
continuation. Scalar evaluation itself is not resumable in `GO_V1`; a stop there
is loud. A policy-disabled or fail-on-limit path is also loud, never clean EOF.
In auto-commit, redeeming a continuation is a new execution in a new transaction
and therefore normally a new read version. Inside an explicit transaction, it
can be redeemed only in the same still-live transaction.

Go exposes the Java result-set contract through `sqldriver.PageRows`. The
standard `database/sql.Rows` type has no driver-extension hook, so an ordinary
`QueryContext` reports a typed `api.ContinuationAvailableError` from `Rows.Err()` at
a non-terminal page boundary. It must never report clean EOF for an incomplete
result. `PageRows` recognizes only that typed boundary, presents Java-style
clean end-of-page semantics, and returns the continuation belonging to that
exact result object.

This is cursor resumability, not a long-lived snapshot. A continuation records
logical execution position and state; it does not contain an FDB read version.

## 1. The defect

Current `paginatingRows.nextRow` consumes its buffer and, whenever the record
cursor is not exhausted, calls `fetchPage` again
(`pkg/relational/core/embedded/cascades_generator.go:1712-1751`). In
auto-commit, every `fetchPage` enters `DB.Run` through `runInCapturedTx` with a
nil captured transaction (`cascades_generator.go:2049-2056`). The result handed
to one `database/sql.Rows` can therefore be assembled from several FDB read
versions without any boundary visible to its caller.

That result is not a statement snapshot. Between pages, a concurrent mutation
can:

- insert a key behind the continuation and never be observed;
- insert a key ahead of it and be observed even though it did not exist on page
  one;
- delete or move an ahead-of-cursor index entry and omit it; or
- move an already-returned index entry ahead of the cursor and return it again.

The anomaly is not fixed by an immutable primary key if the chosen ordering is
an index whose key can change. Even under primary-key ordering, the values in
unchanged rows may come from different times.

The implementation currently documents the mechanism but not a caller-visible
contract. `cascades_generator.go:2052-2054` says every auto-commit page uses its
own transaction; RFC-203 §4.2 and G12 deliberately preserve that rollover as
transparent. That was the wrong decision. It preserves completion by hiding a
semantic boundary.

## 2. What Java actually does

The reference is the pinned Java Record Layer tag 4.12.11.0.

Java's `MAX_ROWS` is “the maximum number of records to return before prompting
for continuation” (`Options.java:58-63`), and its default is
`Integer.MAX_VALUE` (`Options.java:278-295`). `QueryPlan` applies it to one
execution through `ExecuteProperties.setReturnedRowLimit`
(`QueryPlan.java:421-448`).

The relational result set does not open another transaction when the cursor
stops:

1. `RecordLayerResultSet.advanceRow` returns no row when its current iterator
   has no next value (`RecordLayerResultSet.java:77-87`).
2. `getContinuation()` is legal only once that result set has no next value and
   returns the cursor continuation enriched with a reason (`:108-135`).
3. In auto-commit, closing the result set commits the current transaction
   (`:90-100`).
4. The next statement calls `ensureTransactionActive`, which discards any old
   auto-commit transaction and opens a fresh one
   (`EmbeddedRelationalConnection.java:466-495`).
5. The caller explicitly executes `EXECUTE CONTINUATION ?`; `CursorTest` proves
   both a returned-row boundary (`CursorTest.java:161-193`) and a scan-limit
   boundary resumed on another connection (`:196-236`).

Java's continuation proto contains execution state, plan/binding hashes, a
compiled statement, and a reason, but no read version
(`fdb-relational-core/src/main/proto/continuation.proto:31-54`). Its core
`AutoContinuingCursor` may automate the loop for record-layer callers, but the
relational/JDBC layer does not use it. Java SQL exposes pages.

The parity target is therefore:

> one execution, one transaction, one page; another page requires another
> explicit execution.

Go may materialize that page before returning from `QueryContext`—as it already
does—so client think time does not consume the FDB window. The observable page
and continuation semantics, rather than the exact lifetime of the JDBC object,
are the compatibility boundary.

## 3. Semantics

### 3.1 Auto-commit

Each invocation's **data-plan page** performs exactly one `DB.Run`. Catalog
bootstrap or planning may use their own transactions; they do not contribute
rows to the page. The page transaction reads, produces the rows and
continuation, and commits before the buffered rows are delivered.

| Page outcome | Rows outcome | Continuation reason | Next invocation |
|---|---|---|---|
| source exhausted | clean end | `CURSOR_AFTER_LAST` | none |
| outer cursor returned-row limit | visible boundary | `QUERY_EXECUTION_LIMIT_REACHED` | fresh transaction/read version |
| outer cursor scan/byte/time limit | visible boundary | `TRANSACTION_LIMIT_REACHED` | fresh transaction/read version |
| scalar-subquery scan/byte/time limit before outer cursor | `54F01`, no rows | none | retry/rewrite query |
| caller stops consuming early | close, no token | none | query must be restarted |
| execution error | error, no token | none | ordinary retry policy |

There is no “transparent” branch. `MAX_ROWS` remains a per-invocation page size,
including RFC-203's exact-multiple behavior: ten rows at two rows per page
produce pages `2,2,2,2,2,0`; ten rows at three rows per page produce
`3,3,3,1`.

The zero-row terminal probe is necessary because a page that returns exactly
the row limit cannot know whether the source has another row without executing
again. A zero-row non-terminal page is also valid for a blocking operator, but
its continuation must advance. The zero-progress guard compares the canonical
whole `PGoSQLExecutionState`—main cursor plus every sidecar state—not only the
inner cursor bytes. Two consecutive zero-row executions with byte-identical
state fail `54F01` instead of returning another token.

`SetFailOnScanLimitReached(true)` retains its deliberate Java option semantics:
a cooperative scanned-row, scanned-byte, or execution-time limit returns
`54F01` and no continuation. The visible `TRANSACTION_LIMIT_REACHED` row in the
table applies when that flag is false. A raw transaction-too-old failure remains
its ordinary transaction error.

### 3.2 Explicit transactions

All pages execute through the `embeddedTx` captured by the first execution.
They share its read version and see its writes, but only while that FDB
transaction remains usable.

A continuation minted inside an explicit transaction uses mode `GO_V1_TX` and
contains that transaction's random, process-local 128-bit nonce. Redemption
requires all three:

1. the current connection has an active explicit transaction;
2. its nonce equals the token's nonce; and
3. the transaction has not committed, rolled back, been reset, or been closed.

Failure is `24F00 INVALID_CONTINUATION`. A commit followed by auto-commit
redemption, a new transaction on the same connection, and a transaction on a
different connection all fail.

This fence does not extend FDB's approximately five-second MVCC window. A
`MAX_ROWS`/returned-row boundary reached early may leave enough time to resume
in the same transaction. Scan-row, scan-byte, and time budgets are
transaction-scoped under RFC-198, but Java's per-cursor free initial pass lets a
fresh resumed leaf make bounded progress after the shared limit is reached
(`key_value_cursor.go:184-221`, `index_scan.go:550-574`). This can degrade to a
row per page and still runs only until the FDB transaction expires. A pre-page
transaction-time wall or raw `1007` cannot recover and remains a loud
transaction error. In practice a caller that cannot finish promptly rolls back
and retries the whole transaction with a suitable budget. It must never carry
the token into a new snapshot.

This narrows RFC-198 §6's resume claim to the remaining FDB lifetime. It also
supersedes RFC-198 §9's `GO_V0_TX`/pointer mechanism: `GO_V1_TX` plus the active
transaction's nonce is the serialized identity check. The active `*embeddedTx`
owning that nonce is still required; the nonce does not make a transaction
portable.

### 3.3 Statement-scoped values and limits

Each continuation redemption is a new SQL execution. The following reset per
page, matching the existing “per request” comments in
`cascades_generator.go:1280-1284` and `connection.go:106-118`:

- statement timeout;
- result-byte limit;
- memory budget;
- execution statistics duration and retry count; and
- `CURRENT_TIMESTAMP` and the other statement-time functions.

Semantic safety state does not reset merely because a page did. In particular,
recursive-CTE depth is serialized in the continuation so a cyclic CTE cannot
evade its total recursion cap one page at a time.

### 3.4 Mutating statements

This RFC's external continuation surface is persistent-data read-only. A
decoder recursively rejects `Insert`, `Update`, `Delete`, or any unknown node
that might write FDB records. The check is over the complete plan graph, not
just its root. Executor-local `TempTableInsert` nodes are a separate census
class: they are allowed only when their alias and ownership edge are proved to
belong to an enclosing recursive-plan state machine. They mutate in-memory
cursor state, not the database, and their table contents already travel in the
recursive cursor continuation. An orphan or externally targeted temp-table
node fails closed.

Java supports some DML continuations, but a replayable mutation token has no
exactly-once guarantee and Go's `database/sql.Result` has no continuation
channel. Silently committing several DML pages is worse: a later failure leaves
a partial statement committed. After this RFC, one Go DML execution must fit in
one transaction or fail and roll back. Supporting replay-aware DML pages would
require a separate mutation protocol, not weakening the read-only validator.

## 4. Go API

### 4.1 Why RFC-203's connection slot is replaced

RFC-203 §4.4 proposes `EmbeddedConnection.LastContinuation`, reached through a
pinned `sql.Conn.Raw`. That makes the token mutable connection-global state,
not result-set state. Two open statements can overwrite it; pool reuse can leak
a stale token; and `sql.Tx` has no `Raw` method even though an in-transaction
token must be redeemed on the owning transaction.

`database/sql.Rows` does hide its underlying `driver.Rows`, as RFC-203 notes.
The usable result-scoped channel is the return from `driver.Rows.Next`: Go's
standard library retains every non-`io.EOF` driver error in `Rows.Err()` even
after it implicitly closes the rows. The implementation must pin that behavior
with a fake-driver test rather than rely only on a source citation.

### 4.2 Boundary carrier

`pkg/relational/api/continuation.go` defines an error type, not a sentinel. It
lives below both the embedded engine that produces it and the SQL-driver adapter
that consumes it, avoiding an `embedded` → `sqldriver` import cycle:

```go
type ContinuationAvailableError struct {
    continuation Continuation // immutable parsed snapshot
}

func NewContinuationAvailableError(c Continuation) (*ContinuationAvailableError, error)
func (e *ContinuationAvailableError) Error() string
func (e *ContinuationAvailableError) Continuation() Continuation
func (e *ContinuationAvailableError) Reason() ContinuationReason
```

The text is constant and contains no token bytes, literals, keys, or plan text.
The constructor parses a defensive copy of `c.Serialize()`, requires its parsed
execution state and reason to equal `c.ExecutionState()` and `c.Reason()`, and
accepts only nonempty/nonterminal state with the query- or transaction-limit
reason. It stores that one immutable parsed snapshot. `Continuation()` returns
a new copy-on-access value and `Reason()` reads the same snapshot, so neither a
producer with inconsistent methods nor later slice mutation can make the
carrier expose a different token than it redeems.

After the last buffered row of a non-terminal page, `paginatingRows.Next`
returns this error instead of calling `fetchPage` again. Consequently:

- an ordinary `*sql.Rows` returns `false` from `Next` and the typed boundary
  from `Err`; an incomplete result can never look successful;
- `database/sql` performs its normal implicit `Rows.Close`;
- the page's auto-commit transaction has already committed; and
- execution logging records the page as a successful boundary, not a failed
  query. The carrier is an adapter signal, not SQLSTATE `54F01`.

A natural source exhaustion remains ordinary `io.EOF`.

`api.ResultSet.Continuation()` remains the internal record-cursor-position
source and gains Java's exhausted-result precondition. `paginatingRows` combines
that inner state with the SQL plan/bindings/scope into `GO_V1`; raw inner bytes
never become the `database/sql` token. RFC-203's proposed production use of the
method is retained at this lower boundary, without its connection-global slot.

### 4.3 Continuation-aware rows

The public adapter is result-scoped and works with `*sql.DB`, `*sql.Conn`, and
`*sql.Tx`, all of which implement `Queryer`:

```go
type Queryer interface {
    QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type PageRows struct {
    rows *sql.Rows
    // unexported terminal state
}

func QueryPage(
    ctx context.Context,
    q Queryer,
    query string,
    args ...any,
) (*PageRows, error)

func (r *PageRows) Next() bool
func (r *PageRows) Scan(dest ...any) error
func (r *PageRows) Columns() ([]string, error)
func (r *PageRows) ColumnTypes() ([]*sql.ColumnType, error)
func (r *PageRows) Close() error
func (r *PageRows) Err() error
func (r *PageRows) Continuation() (api.Continuation, error)
func (r *PageRows) Reason() (api.ContinuationReason, error)
```

The generic shape permits `*sql.DB`, but current raw embedded continuation mode
must be opted into on a pinned `*sql.Conn`; an explicit `*sql.Tx` inherits that
configuration from its owning connection. Direct `*sql.DB` pagination is for a
future protected connector or for a one-page query that needs no token.

`PageRows.Next` delegates to `sql.Rows.Next`. On false it inspects
`sql.Rows.Err`. It suppresses only a valid `ContinuationAvailableError`, saves
that continuation, and leaves every other error unchanged. Clean EOF creates a
standard END continuation (`execution_state` present and empty,
`CURSOR_AFTER_LAST`). `api.EndContinuation()` supplies that immutable value;
its deterministic `Serialize()` is a parseable version-1 `ContinuationProto`
with present-but-empty `execution_state`, `CURSOR_AFTER_LAST`, and no compiled
statement. Its byte-returning methods return copies. Serializing it is legal but
redeeming an END token is not.

`Continuation` and `Reason` fail with Java's exhausted-result precondition
until `Next` has returned false: `0A000`, “Continuation can only be returned
once the result set has been exhausted”. `PageRows.Close` records whether
exhaustion was observed before closing its private underlying rows; an early
close clears the terminal state and does not manufacture a token. The
underlying `*sql.Rows` is
intentionally not embedded or exported, so callers cannot bypass these state
transitions. `Scan`, `Columns`, and `ColumnTypes` are narrow forwarding methods.

Java can label a token `USER_REQUESTED_CONTINUATION` when
`getContinuation()` is called after the last successful `next()` but before the
caller observes `next()==false`. Idiomatic Go loops do not have that call point:
`PageRows` deliberately requires the terminating `Next()==false`, so `GO_V1`
mints only `QUERY_EXECUTION_LIMIT_REACHED`, `TRANSACTION_LIMIT_REACHED`, or the
synthetic `CURSOR_AFTER_LAST`. This is a surface-timing divergence, not an
execution-position divergence.

Page size is the existing `OptMaxRows` connection/statement option. The public
surface is deliberately narrow:

```go
func SetMaxRows(ctx context.Context, conn *sql.Conn, maxRows int64) error
func EnableUnprotectedPlanContinuations(ctx context.Context, conn *sql.Conn) error
```

`SetMaxRows` accepts zero (unlimited) through `math.MaxInt32` and rejects a
negative or larger value with `22023`; it cannot overwrite DSN security or
tenant options. Both helpers use `sql.Conn.Raw` and a checked driver method that
rejects an active result or transaction, rather than an unenforceable generic
`Options()/SetOptions()` assertion. An explicit transaction is configured on
its owning `*sql.Conn` before `BeginTx`.

The embedded connection retains immutable connector/DSN defaults separately
from programmatic session options. `ResetSession` restores those defaults and
turns the unprotected-plan capability off before pool reuse, so closing a
pinned `sql.Conn` cannot leak page size or capability to the next borrower.

### 4.4 Caller loop

```go
conn, err := db.Conn(ctx)
if err != nil {
    return err
}
defer conn.Close()
if err := sqldriver.SetMaxRows(ctx, conn, 1_000); err != nil {
    return err
}
// Raw GO_V1 contains a plaintext physical plan. The embedding host opts in;
// never expose this mode to an untrusted SQL caller.
if err := sqldriver.EnableUnprotectedPlanContinuations(ctx, conn); err != nil {
    return err
}

query := "SELECT id, payload FROM events ORDER BY id"
var args []any

for pages := 0; ; pages++ {
    if pages == 100 {
        return errors.New("continuation loop exceeded application limit")
    }

    rows, err := sqldriver.QueryPage(ctx, conn, query, args...)
    if err != nil {
        return err
    }
    for rows.Next() {
        if err := consume(rows); err != nil {
            _ = rows.Close()
            return err
        }
    }
    if err := rows.Err(); err != nil {
        return err
    }

    continuation, err := rows.Continuation()
    if err != nil {
        return err
    }
    if api.AtEnd(continuation) {
        break
    }

    query = "EXECUTE CONTINUATION ?"
    args = []any{continuation.Serialize()}
}
```

The application chooses its own maximum page count and cancellation deadline.
The corpus runner retains Java's 100-continuation guard. The library does not
pretend that a fixed global count fits every legitimate export.

For an explicit transaction, the same loop passes the same `*sql.Tx` as
`Queryer`. It must finish before the transaction expires.

## 5. The continuation must be self-contained

RFC-203's envelope design is not implementable against the current Go executor
without three additions.

### 5.1 DISTINCT cannot name statement-local scratch

Production execution always installs `ExecutionScratch`
(`cascades_generator.go:1370-1379`). Hash DISTINCT then places a scratch token,
not the seen-key set, in its cursor continuation. A fresh execution has a fresh
scratch and cannot redeem that reference
(`pkg/recordlayer/query/executor/execution_scratch.go:39-50,482-489`).

When a continuation may leave one `Execute`, the evaluator runs without
statement-local DISTINCT scratch. The existing scratch-less path serializes the
seen keys by value (`distinct_stream.go:150-191,397-481`). Token-size accounting
applies before any page row is returned. A server-side scratch registry is
rejected: it makes an apparently portable token depend on process memory and
turns restarts and load balancing into correctness failures.

### 5.2 Scalar subqueries are separate plans

`cascadesPlan` stores `[]PlannedScalarSubquery` beside its main physical plan
(`cascades_generator.go:1214-1225`), and executes those plans before the outer
plan (`:2138-2150`). Serializing only `physicalPlan` loses them.

The Go serialization arm therefore carries a plan bundle. These messages live
in `proto/relational/sql_continuation.proto`, use `syntax = "proto2"`, package
`com.apple.foundationdb.relational.continuation`, and
`option go_package = "fdb.dev/gen"`; the file imports Apple's
`record_query_plan.proto`.

```proto
message PGoContinuationPlanBundle {
  required com.apple.foundationdb.record.planprotos.PRecordQueryPlan main_plan = 1;
  repeated PGoScalarSubquery scalar_subqueries = 2;
  required com.apple.foundationdb.record.planprotos.PType result_type = 3;
  required PGoContinuationScope scope = 4;
  required bytes canonical_plan_sha256 = 5;
  required bytes binding_sha256 = 6;
}

message PGoScalarSubquery {
  required string correlation_alias = 1;
  required com.apple.foundationdb.record.planprotos.PRecordQueryPlan plan = 2;
}

message PGoContinuationScope {
  required bytes backend_incarnation = 1;       // 16 bytes
  required string database_path = 2;
  required bytes database_incarnation = 3;      // 16 bytes
  required string schema_name = 4;
  required bytes schema_incarnation = 5;        // 16 bytes
  required string template_name = 6;
  required uint64 template_version = 7;
  required bytes template_incarnation = 8;      // 16 bytes
  required bytes metadata_sha256 = 9;           // 32 bytes
  required bytes result_type_sha256 = 10;       // 32 bytes
  repeated PGoRecordTypeDependency record_types = 11;
  repeated PGoIndexDependency indexes = 12;
  repeated PGoPlannerOption planner_options = 13;
  optional bytes transaction_nonce = 14;        // exactly 16 bytes in GO_V1_TX
}

message PGoRecordTypeDependency {
  required string name = 1;
  required bytes incarnation = 2;               // 16 bytes
  required bytes definition_sha256 = 3;         // 32 bytes
}

message PGoIndexDependency {
  required string name = 1;
  required bytes incarnation = 2;               // 16 bytes
  required bytes definition_sha256 = 3;         // 32 bytes
  required uint64 last_modified_version = 4;
  required uint32 index_state = 5;
}

message PGoPlannerOption {
  required string name = 1;
  required bytes canonical_value = 2;
}
```

It is a Go-owned message packed into the existing
`PRecordQueryPlan.additional_plans` `Any` arm. Its only accepted type URL is
`type.googleapis.com/com.apple.foundationdb.relational.continuation.PGoContinuationPlanBundle`.
Java will not execute it, and the serialization mode fence says so before
unpacking. Dependency and planner-option repeated fields are sorted by their
canonical names; duplicate names reject.

One shared deterministic plan-serialization context walks the main plan first,
then each complete scalar entry—alias followed by plan—in listed ordinal order,
assigning `PPlanReference` IDs by stable preorder while preserving DAG sharing.
Result type, plan constraint, and canonically sorted scope follow in the plan
digest framing defined in §6.1. Original extracted literals and arguments are
encoded only in the separate binding framing; they are not part of the plan
digest. Child fields use semantic order; map-backed collections sort their
encoded keys. Mint, digest, and redemption use these exact contexts and orders,
never independent per-subplan contexts.

Scalar subqueries are evaluated from their beginning in each page transaction,
before the outer cursor is opened, just as `fetchPage` does today. Their aliases
and plans are transported so the binding is reconstructed against that page's
read version; their result is not carried from an older page. If a scan, byte,
time, or transaction limit stops a scalar subquery, no outer rows or continuation
are published and the execution returns `54F01`. Resuming *within* scalar
evaluation would require a second phase machine containing the current
subquery's cursor, its first cardinality-check row, completed typed results, and
the next subquery ordinal. `GO_V1` deliberately does not claim that protocol.

### 5.3 Logical execution state crosses pages

`ContinuationProto.execution_state` in mode `GO_V1` contains a Go wrapper, not
bare record-cursor bytes:

```proto
message PGoSQLExecutionState {
  required uint32 state_version = 1; // exactly 1
  required bytes record_cursor_state = 2;
  repeated PGoRecursionLevel recursion_levels = 3;
}

message PGoRecursionLevel {
  required uint32 plan_node_id = 1;
  repeated uint32 branch_path = 2;
  required uint32 levels = 3;
}
```

This carries semantic state that currently lives only in `ExecuteState`. One
scalar count is insufficient: the live map is keyed by recursive-plan identity
plus the nested IN-union branch scope, and several invocations can be live at
once. Canonical plan serialization assigns every node a stable ID; deserialization
maps the ID back to that plan object, and `branch_path` replaces the current
slash-encoded sibling-index scope. Duplicate IDs/paths, an unknown or
non-recursive node ID, an impossible branch ordinal, or a count over the hard
recursion cap rejects the token. The state is versioned by the top-level
`GO_V1` mode. Resource counters that intentionally reset per execution are not
serialized.

`state_version` is required so a valid nonterminal wrapper can never marshal to
zero bytes, which the outer Java proto reserves for END. A `GO_V1` redemption
requires version 1 and a present inner cursor state; an absent wrapper is BEGIN
and a present-but-empty outer `execution_state` is END, neither redeemable.

### 5.4 Serialization census

At this head, the command

```sh
rg '^type RecordQuery[A-Za-z0-9_]*Plan struct' \
  pkg/recordlayer/query/plan | wc -l
```

reports 42 plan structs. `RecordQueryValuesPlan` is SQL-reachable and has no
shared proto arm, disproving RFC-203's older plan census. Plans are only the
outer polymorphic family: their graph also contains Values, QueryPredicates,
comparisons, key expressions, scan parameters, copiers, sort keys, and types,
each with shared arms or `additional_*` `Any` extension points.

The implementation starts with generated/maintained tables assigning every
concrete member of every serialized polymorphic family to exactly one of:

- a shared Java proto arm;
- an allowlisted Go `Any` arm; or
- a loud, non-resumable rejection.

Every plan entry also records persistent read-only, persistent mutating, or
executor-local state mutation, plus SQL-reachable versus internal-only.
Executor-local mutation carries the structural parent/alias invariant that
makes it safe. A new plan, value, predicate, comparison, key-expression,
scan-parameter, copier, sort, or type implementation fails the census until
classified. The read path records whether the actual graph is mintable but does
not reject a known-persistent-read-only graph merely because one object has no
wire codec: matching Java, plan serialization is required only if the page is
nonterminal. A terminal page for such a valid unrepresentable plan succeeds; a
nonterminal page fails `0A000` before buffered rows are published. An object
whose persistent-write effect is unclassified still fails before execution.

### 5.5 Continuation-state census

A second maintained table covers mutable execution state for every resumable
operator: cursor codec and version, buffered rows, seen sets, temp tables,
aggregate/sort state, scalar side plans, recursion counters, scratch/registry
references, retry snapshot behavior, and size accounting. Every entry must be
one of self-contained-by-value, deterministically reconstructible from the
transported plan/bindings, or non-resumable at mint preflight. Process-local
references are forbidden. Adding a cursor/operator or mutable sidecar fails the
census until classified; plan round-trip alone is not evidence of resumability.

## 6. Envelope and validation

### 6.1 Wire shape and modes

Vendor Java's `ContinuationProto` verbatim. Go writes version 1 and uses:

- `GO_V1` for auto-commit, same-engine continuations; and
- `GO_V1_TX` for continuations bound to one live explicit transaction.

There are no shipped `GO_V0` bytes, so using a new mode for the corrected bundle
does not strand a deployed token. `GO_V0`, Java modes, unknown modes, and an
unknown `Any` type URL are refused with `24F00` before plan construction.

A nonterminal `GO_V1*` envelope has one closed shape. `compiled_statement` is
present; its mode is exact; its `plan` is one `PRecordQueryPlan` whose selected
oneof arm is `additional_plans`, containing exactly the bundle type URL from
§5.2. That bundle URL is legal only at this root and is rejected anywhere in
the main/scalar graphs. `plan_constraint` and `queryMetadata` are present,
`copyPlan` is absent, and the outer query metadata equals the bundle result type.
Nested `additional_*` values may use only their census-registered, family-local
type URLs. A terminal END envelope has no compiled statement, matching Java's
set-only-when-not-at-end rule.

The Java `plan_hash` and `binding_hash` fields remain populated and are checked,
but they are 32-bit, unkeyed compatibility checks—not security controls.
`GO_V1.plan_hash` is the signed int32 represented by the first four big-endian
bytes of `canonical_plan_sha256`. `binding_hash` retains RFC-203's Java
Murmur3-32 fold over `(canonicalName, literal)` pairs, with byte literals hashed
by content; redemption recomputes it from the reconstructed original table and
never seeds from the token.

Go's authoritative drift checks use an explicit canonical framing. `F(tag, x)`
is unsigned-varint tag, unsigned-varint byte length, then `x`. `C(T)` is the
type-specific deterministic encoding after rejecting unknown fields, sorting
semantically unordered collections by their encoded key, and retaining the
declared order of semantic lists. Plans share the one reference-ID context from
§5.2.

The plan payload is:

```text
P = F(1, C(main plan))
  || for each scalar ordinal: F(2, F(1, C(alias)) || F(2, C(plan)))
  || F(3, C(result type))
  || F(4, C(plan constraint))
  || F(5, C(scope))
canonical_plan_sha256 = SHA-256("FRLGO-CONT-PLAN-V1\x00" || P)
```

This field-wise encoder deliberately never serializes bundle fields 5 and 6,
so it does not need to violate their proto2 `required` labels and cannot hash
either digest recursively. Complete scalar entries include their aliases.
Bindings do not occur in `P`.

For bindings, redemption merges Java's
`compiled_statement.extracted_literals` and `arguments` by
`literals_table_index` and requires one unique, gap-free table numbered
`0..N-1`. `B` is the concatenation, in that order, of
`F(index+1, F(1, source-kind) || F(2, canonical-name) || F(3, C(type)) ||
F(4, C(value)))`; null has an explicit value tag. Duplicate, missing, negative,
or out-of-range ordinals reject. Then:

```text
binding_sha256 = SHA-256("FRLGO-CONT-BIND-V1\x00" || B)
```

The continuation argument of `EXECUTE CONTINUATION ?` is transport, not an
original query binding, and is excluded.

The binding values remain in those Java `CompiledStatement` fields; the bundle
contains only their digest. Both digests are unkeyed and therefore still not
authentication.

### 6.2 Scope

`PGoContinuationScope` binds the token to:

- cluster/subspace identity and database path;
- persistent database, schema-binding, and schema-template incarnation IDs,
  which change on drop/recreate even when the names and definitions are
  identical;
- schema name under the engine's identifier case rules, template name and
  version, and the exact metadata/type fingerprint;
- the result type in all three representations: plan-derived type, bundle
  type, and Java `compiled_statement.queryMetadata`;
- planner options that can change physical meaning;
- the exact, sorted set of record types and indexes derived by walking the
  transported main and scalar plans, including each definition fingerprint,
  catalog incarnation, and original readable state/version; and
- the explicit-transaction nonce for `GO_V1_TX`.

The dependency walk is authoritative: its output must equal the token's scope
list exactly, so a forged or buggy token cannot omit a dependency. Validation
compares the original incarnations and definitions, not dependencies
recomputed and then silently substituted from current metadata. The catalog
must persist the incarnation IDs before `GO_V1` can be minted; assigning them
to existing catalog rows is an implementation prerequisite.

The IDs live in a continuation-scope ledger under the catalog root, outside
database/schema subspaces that DDL clears. Creation writes a random 128-bit ID
in the same catalog transaction; drop retains a tombstone, and recreate writes
a new ID. Template members receive IDs with the template incarnation, so
deleting and recreating an identical template version cannot reuse record-type
or index identity. A backend incarnation is created once per catalog root.
`GO_V1` stays disabled until migration has populated the ledger and every DDL
writer is ledger-aware. In particular, a Java or administrative writer that can
bypass the ledger is incompatible with this mode until it participates in the
same protocol; exact content hashes alone do not detect identical recreation.

`GO_V1` is intentionally conservative: exact metadata fingerprint equality is
required, so even an unrelated metadata change invalidates the token. A later
mode can admit dependency-local compatible evolution after
`CompatibleTypeEvolutionPredicate` has a real implementation; its current
always-true `Eval` (`compatible_type_evolution_predicate.go:71`) is not used.

Scope encodings reuse `F`/`C` but have their own domains:

```text
metadata_sha256        = SHA-256("FRLGO-CONT-METADATA-V1\x00" || C(metadata))
result_type_sha256     = SHA-256("FRLGO-CONT-RESULT-TYPE-V1\x00" || C(PType))
record.definition_sha256 = SHA-256("FRLGO-CONT-RECORD-V1\x00" || C(record definition))
index.definition_sha256  = SHA-256("FRLGO-CONT-INDEX-V1\x00" || C(index definition))
```

`C(metadata)` is the complete schema-template `RecordMetaData` payload in
canonical record-type/index-name order. A record definition includes descriptor,
record-type key, primary key, and version fields. An index definition includes
record-type membership, root key expression, predicate, type, options, and
version fields; readable state and incarnation remain separate scope fields.
`canonical_value` for a planner option is `F(type-tag, C(value))` using the same
typed scalar/list/record encoder as bindings. Identifiers are first normalized
under the scope's case-sensitivity setting, UTF-8 encoded, and dependency and
option entries are sorted by those bytes. Every digest has exactly 32 bytes;
every incarnation/nonce has exactly 16. Mint and validation reject a noncanonical
ordering or length rather than normalizing caller bytes silently.

### 6.3 Validation order

Redemption first checks the connection's mode/capability without inspecting the
token. Once enabled, it performs these checks in order:

1. On ingress, reject a raw/prepared token over 16 MiB before protobuf work.
   Before decode, hex payload is capped at 33,554,432 characters and base64 at
   22,369,624 characters—the encodings of at most 16 MiB. A prepared `?`
   remains an out-of-band `driver.NamedValue`; it is never expanded into
   `X'hex'` during substitution.
2. Use the continuation-specific bounded decoder, not an unconstrained generic
   protobuf unmarshal: depth at most 128, at most 200,000 decoded messages and
   graph objects, at most 32 MiB cumulative length-delimited payload, and
   a hard 64 MiB cumulative allocation ceiling with overflow-safe accounting
   for every allocation and defensive copy.
3. Require envelope version 1, execution-state version 1, a
   non-BEGIN/non-END state with a present inner cursor, an exact mode, no
   unknown fields, and an allowlisted exact type URL at every nested `Any`
   location—not only `additional_plans`. Reason is required and known:
   nonempty `GO_V1` state admits only `QUERY_EXECUTION_LIMIT_REACHED` or
   `TRANSACTION_LIMIT_REACHED`; empty state requires `CURSOR_AFTER_LAST` and is
   rejected as a redemption. `USER_REQUESTED_CONTINUATION` is not a `GO_V1`
   wire state. Nonterminal tokens also require `binding_hash`, `plan_hash`, and
   the closed-shape `compiled_statement` root, plan constraint, and query
   metadata from §6.1; forbid `copyPlan` and nested bundle URLs; and require the
   transaction nonce present exactly for `GO_V1_TX` and absent for `GO_V1`.
4. Enforce a single acyclic plan/reference graph with unique stable IDs, no
   dangling references, at most 100,000 plan/value/predicate/type/key-expression
   objects, plan depth 128, and 65,536 total literals plus arguments. Cursor
   state, buffered rows, DISTINCT keys, temp-table rows, and recursion entries
   share the global byte/object budgets rather than receiving independent caps.
5. Recursively prove the main plan and every scalar subplan free of persistent
   writes, and prove every executor-local mutation's parent/alias ownership.
6. Derive record-type/index dependencies and result type from that graph and
   require exact equality with the scope, bundle, and Java metadata claims.
7. Deserialize, finalize, and run structural plan invariants.
8. Recompute the canonical plan and binding digests.
9. Validate cluster/database/schema incarnation and transaction scope.
10. Inside the page transaction, revalidate exact metadata, type, store state,
    and original dependencies and evaluate the transported plan constraint
    before the first user-data read. `false` or `unknown` fails `24F00`; it is
    never swallowed as Java currently swallows one Path-1 constraint failure.

There is no continuation compression in `GO_V1`, so there is no decompression
bomb path. Deterministic marshal buffers, cursor state, plan bundle, defensive
copies, and literal decoding are all charged to the hard transport ceiling and
to any lower configured page memory budget. Oversized state produced by the
engine fails `54F01` while minting,
before `QueryContext` returns rows. Malformed, out-of-scope, stale, or mismatched
caller bytes fail `24F00`. A valid query whose plan cannot be serialized fails
`0A000` at a nonterminal mint boundary; source exhaustion needs no compiled
statement and still succeeds.

Java maps some malformed envelopes to `XX000` and does not validate its version;
Go does not copy those fail-open/fault-classification bugs for a caller-supplied
binary parser.

### 6.4 Trust boundary

The serialized plan is plaintext and unsigned. It may contain query literals,
primary keys, buffered sort rows, DISTINCT keys, and aggregate state. Tokens
must never be logged, placed in error text, or treated as harmless metadata.

Raw `GO_V1` is a deserialized-plan capability, not merely a cursor token.
Unkeyed scope and digest checks do not stop a caller from forging a different,
self-consistent read plan. It is therefore default-off behind the Go-only
`OptAllowUnprotectedPlanContinuation` option. That option is programmatic-only,
is rejected in a DSN or SQL statement, and must be set by the embedding host
before the first page is minted. Enabling it asserts the repository's existing
deployment boundary: every SQL caller is trusted to issue arbitrary embedded
queries. Scope and digest checks defend correctness and accidental misuse, not
a malicious peer. The capability must be enabled independently on every
connection that mints or redeems raw bytes, including a fresh-process resume.
The remote connector cannot enable this option.

With the capability off, a query that reaches source exhaustion in its one page
still succeeds and needs no plan token. If it stops non-terminally, minting
fails `0A000` (“unprotected plan continuations are disabled”) before buffered
rows are published: no clean EOF, boundary carrier, or transparent second page
is allowed. `EXECUTE CONTINUATION` and `OptContinuation` fail with the same code
before decoding raw bytes.

Remote/untrusted SQL must not expose raw `GO_V1` tokens. A future remote driver
must wrap the inner Java-compatible proto in an authenticated, confidential
transport token—AEAD or an opaque server-side handle—bound to tenant/principal,
authorization-policy version, cluster/database/schema incarnations, expiry,
and key ID, and must re-authorize on every page. Its constructor fails closed
unless a protector is configured; a negative integration gate proves remote
mode cannot install the unprotected embedded capability. That outer transport
protection is not added to the Java proto and is not claimed by this RFC.

Continuation text is redacted before query normalization, hashing, caching,
tracing, or logging. Prepared bytes bypass SQL-literal substitution; literal
hex/base64 syntax is replaced by a fixed marker at the lexer boundary before
instrumentation sees it. Decoder errors are mapped at the relational boundary
to a fresh typed error without wrapping or formatting the lower-level cause.
This is necessary because existing nested cursor `ContinuationParseError`
values include `RawBytes` in `Error()`. Detailed diagnostics are counters and
reason classes only; they never contain token fragments.

## 7. Execution changes

### 7.1 Planning

`planOne` routes `ExecuteContinuationStatement` instead of returning “only SHOW
administration statements are supported”
(`cascades_generator.go:145-184`). The existing grammar already accepts
`X'…'`, `B64'…'`, and prepared parameters
(`RelationalParser.g4:683-686`); existing parameter substitution renders
`[]byte` as an `X'hex'` atom (`utilities.go:296-306`).

The continuation path is the deliberate exception to that generic substitution:
a `?` consumes the raw `driver.NamedValue` after the statement shape is parsed.
Hex/base64 literal forms remain supported for Java parity, subject to the
pre-decode caps and redaction in §6.3–6.4.

The normal SQL-to-plan path remains Cascades. `EXECUTE CONTINUATION` transports
a plan produced by that same path; it is not a second planner. Continued
statements are uncacheable.

`OptContinuation` resumes a normally planned query only after the same scope,
binding, plan, and read-only gates. The current unconditional rejection at
`cascades_generator.go:1285-1297` is removed only when both entry points share
one validator.

### 7.2 Page production

`cascadesPlan.Execute` first splits persistent DML from resumable reads.
The read path:

1. recursively classifies plan-bundle mintability and proves persistent
   read-only status without yet requiring a serialization;
2. when unprotected continuation capability is enabled, creates a
   self-contained execution context with no process-local DISTINCT scratch;
3. eagerly invokes one page callback—through one `DB.Run` in auto-commit or
   directly once on the captured `embeddedTx`—and publishes one successful
   attempt;
4. classifies the terminal `NoNextReason`;
5. on source exhaustion, returns the terminal page without serializing a plan;
   otherwise requires the recorded mintability, creates and size-checks the
   next continuation, and fails before publishing buffered rows if either step
   fails; and
6. never calls `fetchPage` a second time for that execution.

The DML path performs no token or plan serialization. It drains the mutation
cursor to `SourceExhausted` inside the same page callback. In auto-commit, any
boundary, cancellation, or runtime error returns from `DB.Run` before commit,
discarding staged rows and writes. Inside an explicit transaction, FDB has no
savepoint: any unsuccessful drain after execution starts immediately cancels
the underlying transaction and marks the `embeddedTx` rollback-only. Later
statements fail. `Rollback` clears it; if the caller attempts `Commit` first,
the driver ensures cancellation, clears the active transaction, returns the
rollback-only error, and `database/sql` makes that `sql.Tx` terminal. This is
the mechanism behind §3.4's zero-partial-commit rule, not a check after a page
has committed.

`paginatingRows.nextRow` becomes a buffer drain. After the buffer:

- source exhausted → `io.EOF`;
- non-terminal continuation → `ContinuationAvailableError`; or
- execution failure → that failure.

The earlier `paginatingRows.Next` guard that maps an observed `MAX_ROWS`
boundary directly to `io.EOF` (`cascades_generator.go:1653-1662`) is deleted as
well; changing only `nextRow` would preserve silent truncation.

The current `for { fetchPage() }` loop is deleted, not retained behind an option.
Two SQL execution modes would make the same query text mean either one snapshot
page or an invisible sequence of snapshots depending on connection setup. The
old mode is the defect this RFC removes.

### 7.3 Errors and retries

In auto-commit, FDB retries remain inside the one page's `DB.Run`. Position is
published only after the successful attempt commits, preserving the current
staged-continuation invariant (`cascades_generator.go:2200-2239`). A failed
attempt discards its rows and position. The explicit-transaction path invokes
the callback once and has no such retry loop.

The boundary carrier is not retryable, never wraps `driver.ErrBadConn`, and does
not mark the driver connection bad. It is excluded from `Next`'s `statsErr`
staging, so execution logging records the reason as a successful page boundary.
Generic `database/sql` middleware still sees a nonnil `Rows.Err()` and may count
it as an error; only `PageRows` or boundary-aware middleware can classify the
control signal. A continuation redemption is a new operation with its own
retry loop.

Inside an explicit transaction, no page-level FDB retry occurs because replaying
one page would not replay the user's prior statements. A cooperative cursor
stop that advances state can produce `TRANSACTION_LIMIT_REACHED`. An already
expired pre-page time check or raw FDB `1007` remains a loud transaction error:
re-emitting the input token would make no progress and loop forever.

## 8. Verification gates

Every gate runs through Bazel; SQL-visible gates use real FDB or SimFDB where the
test needs deterministic read versions/faults. New test files are added to their
targets before a green is accepted.

### G1 — one execution invokes one data page

A page that hits each of returned-row, scanned-row, scanned-byte, and time limits
performs exactly one auto-commit **data-plan** `DB.Run`. Instrumented
`fetchPage` calls show no auto-follow under the same `PageRows`;
catalog/planning work is counted separately. An injected retry can create more
than one FDB attempt, but only the final successful attempt's rows, read version,
recursion position, and continuation are published. Mutating the implementation
to call `fetchPage` twice fails the count.

### G2 — generic `database/sql` cannot fake completeness

A fake driver proves a `ContinuationAvailableError` survives
`database/sql.Rows.Next`, implicit close, and `Rows.Err`. A real FDB query that
stops after a page returns that typed error from ordinary `*sql.Rows`; ignoring
the continuation helper cannot yield `Err()==nil` on an incomplete result. The
carrier is not or does not wrap `driver.ErrBadConn`; `*sql.DB` retains its
physical connection, a pinned `*sql.Conn` remains usable, and a `*sql.Tx`
remains active after the boundary. Execution stats classify the page reason and
do not increment the query-error count.

### G3 — `PageRows` state machine

- `Continuation` before `Next()==false` fails;
- boundary error becomes clean `PageRows.Err()==nil` plus a non-END token;
- natural EOF becomes a nonnil END token with `CURSOR_AFTER_LAST`, whose bytes
  parse back to that exact version-1 state;
- close-before-exhaustion returns no token;
- cancellation and execution errors are not mistaken for boundaries; and
- two concurrent result objects cannot exchange tokens;
- `SetMaxRows` rejects type/range misuse and active result/transaction state;
  and
- returning the pinned connection to the pool restores DSN defaults and turns
  off the unprotected-plan capability before the next borrower.

### G4 — page arithmetic

Ten rows with `MAX_ROWS=2` yield `2,2,2,2,2,0`; with `MAX_ROWS=3`,
`3,3,3,1`. The observed page count is asserted. `MAX_ROWS` zero/unset and its
`math.MaxInt32` boundary follow Java; negative and overflow values reject.

### G5 — static-data equivalence

For every resumable plan family in the maintained census, each page is resumed
from serialized bytes on a fresh compatible connection, and a process-restart
lane covers every distinct state codec. Concatenating pages at size one equals
the one-execution result row-for-row, column-for-column, and order-for-order,
with an observed page-count floor of two. The set includes scan, index,
reverse/range, union, intersection, IN join/union, aggregate, DISTINCT, sort
including null ordering, LIMIT/OFFSET, vector, recursive CTE, values, and
scalar-subquery shapes that the planner can produce.

### G6 — self-contained state

- unordered DISTINCT resumes on a fresh connection and in a fresh process;
- scalar subqueries retain alias and plan and are re-evaluated against each
  page's read version; a resource stop before the outer cursor returns `54F01`
  with no rows or token;
- a `RecordQueryValuesPlan` round-trips;
- concurrent recursive invocations restore distinct stable-ID/branch-path
  counters, and a cyclic recursive CTE still reaches its total cap across pages;
- every corpus-produced polymorphic object is classified by the serialization
  census; and
- every resumable operator's mutable side state is classified by the
  continuation-state census with no process-local reference.

### G7 — scope and stale-plan rejection

Cross-backend, cross-database, cross-schema, wrong transaction, result-type
mismatch, identical-definition database/schema/template/index drop/recreate,
index-state change, incompatible metadata, and an unrelated metadata/index
change all fail `24F00` in `GO_V1`. Omitting or adding a plan-derived dependency
or forcing the plan constraint false/unknown also fails. No rejected token
reads table data. A future compatible-evolution mode must get a new gate rather
than weakening this one.

### G8 — hostile decoder

Truncated protobuf, unknown fields/mode/`Any`, cyclic or dangling plan reference,
recursive unknown plan, nested persistent DML, orphan temp-table mutation,
oversized token/state, excessive depth/object/byte/literal counts, inconsistent
reason and execution state, and independent Java/SHA plan/binding mismatches all
fail without panic, unbounded allocation, data read, or FDB mutation. Fuzzing
parse → validate holds cumulative decoder allocation below 64 MiB for every
accepted 16 MiB input and below a small fixed multiple of input for smaller
cases.

A canary literal placed in a valid token and corrupt nested cursor state proves
that raw, hex, and base64 token material appears in no returned error, execution
log, trace, metric label, normalized query, query hash input, or cache key.

### G9 — transaction fence

Within one explicit transaction, returned-row and cooperative scan/byte/time
boundaries that advance cursor state retain the same read version and concatenate
without gaps or duplicates before the FDB wall. The already-consumed
transaction-scoped scan limit demonstrates the per-cursor free initial pass;
an expired pre-page wall and raw `1007` remain loud errors, never byte-identical
tokens. Commit then redeem, rollback then redeem, a new transaction on the same
connection, another connection, and auto-commit redemption of `GO_V1_TX` all
fail `24F00`. The mode and transaction nonce are asserted independently.

### G10 — live-snapshot semantics are demonstrated, not implied

Under auto-commit, a mutation between two page executions produces distinct read
versions. Deterministic tests show both anomaly directions:

- moving an already returned index key ahead of the continuation can duplicate
  it; and
- moving an unseen index key behind the continuation can omit it.

The test names and API documentation state that this is the supported live-cursor
contract, not a bug in continuation positioning.

### G11 — Java corpus

The yamsql runner consumes `maxRows` page sequences through
`EXECUTE CONTINUATION`; metadata remains identical on every page. The
ForceContinuations lane uses page size one, asserts at least two pages for a
multirow query, and retains Java's 100-continuation liveness guard.

### G12 — DML never partially pages

Insert, update, and delete plans cannot mint or redeem a SQL continuation. A DML
operation forced over a page/transaction budget commits zero mutations. A forged
token containing a mutating child is rejected before the first FDB write. In
auto-commit the callback error rolls back; in an explicit transaction the
boundary, cancellation, and an injected mid-DML runtime error each cancel and
poison the whole transaction. A subsequent statement fails; `Rollback` before
`Commit` clears it, while an attempted `Commit` cancels defensively, returns the
rollback-only error, makes `sql.Tx` terminal, and cannot persist earlier staged
writes.

### G13 — capability and liveness fences

With `OptAllowUnprotectedPlanContinuation` off, source exhaustion succeeds but
every nonterminal read fails `0A000` before rows are published, and redemption
fails before token decoding. Every fresh connection/process in the resume lanes
must opt in independently; remote mode cannot. A catalog without a complete
incarnation-ledger migration, or configured with a DDL writer that bypasses the
ledger, cannot enable the capability. A valid but unrepresentable plan succeeds
when its first page exhausts the source and fails `0A000` without publishing
rows when a forced boundary requires serialization. Two consecutive zero-row
nonterminal executions must change the canonical whole execution state, or the
second fails `54F01`.

## 9. Sequencing

1. **Correct RFC authority.** Land this amendment, add a pointer at RFC-203's
   header, and update CQ-78's completion criteria. No implementation starts
   before the query-engine RFC review gate ACKs this head.
2. **Self-contained codecs.** Vendor `ContinuationProto`; add the Go bundle and
   execution-state protos; implement deterministic codecs, every polymorphic
   serialization/state census, persistent-write walk, bounds, and round-trip
   tests.
3. **Scope and validation.** Persist the ledger for backend, database, schema,
   template, record-type, and index incarnations; implement one validator for
   SQL and `OptContinuation`, exact metadata/dependency fingerprints,
   transaction nonces, capability gate, redaction, and error taxonomy.
4. **One-page executor.** Remove auto-follow; make `MAX_ROWS` per execution;
   serialize DISTINCT/recursive state; split read and drain-to-exhaustion DML;
   mint before publishing rows; preserve retry staging.
5. **Go adapter.** Add `api.ContinuationAvailableError`, `PageRows`,
   `QueryPage`, `SetMaxRows`, the explicit unprotected-capability helper, END
   continuation, session reset, and the fake-driver contract test.
6. **SQL entry point.** Route and execute hex/base64/prepared
   `EXECUTE CONTINUATION` and `EXPLAIN EXECUTE CONTINUATION`.
7. **Corpus and operational docs.** Wire yamsql/ForceContinuations; update
   transaction, resource-limit, security, and multi-tenant documentation; add
   execution metrics for page reason and token size without recording bytes.
8. **Joint implementation review.** Graefe, Torvalds, and Codex review the full
   query-engine milestone after all gates are green.

## 10. Rejected alternatives

### Keep transparent transaction rollover

Rejected. It returns a result assembled from snapshots the caller cannot see or
control. Documentation alone does not make a clean `Rows.Err()==nil` truthful.

### Pin one read version into fresh transactions

Rejected. `SetReadVersion` selects a version; it does not extend how long FDB
retains that version. The same read version becomes too old after the MVCC
window. Snapshot reads only suppress read-conflict ranges and do not change that
lifetime.

### Use one explicit transaction for an arbitrarily slow client

Rejected by FDB's transaction lifetime. It is correct only when all storage
reads and client-driven resumes complete inside the remaining window.

### Put the continuation in a hidden column or second result set

Rejected. It changes query shape or makes a caller that does not call
`NextResultSet` silently accept an incomplete result. Continuation is control
metadata, not relational data.

### Store “last continuation” on the connection

Rejected as the primary API. It is ambiguous under overlapping rows, fragile
under pool reuse, and inaccessible directly from `sql.Tx`. A token belongs to
the result object that produced it.

### Reissue the original SQL and re-plan every page only

Rejected as the complete Java-parity surface. It can be a useful direct-access
path through `OptContinuation`, but it cannot implement Java's
`EXECUTE CONTINUATION`, whose token carries no recoverable SQL skeleton.

### Offset pagination

Rejected. It repeats work, is unstable under concurrent writes, and does not
carry operator state for joins, DISTINCT, aggregates, sorts, or recursive CTEs.

### Treat hashes as token authentication

Rejected. Every hash in the Java envelope is unkeyed and can be recomputed by a
forger. The current embedded boundary is trusted; a future untrusted transport
must add confidentiality and authentication outside the Java proto.

### Promise a cross-page snapshot

Rejected because FDB provides no arbitrarily long, user-paced snapshot. If all
source reads can finish inside the read-version window, a separate API may
eagerly drain and spool an immutable result for slow consumption. If the source
scan itself exceeds that window and an exact point-in-time view is required, the
solution is application-layer MVCC, immutable snapshot generations, or a
separate restored/analytical read model—not a continuation-token field.

## 11. Resulting contract

After this RFC, the SQL layer makes three different claims and no stronger one:

1. every individual page is consistent at one FDB read version;
2. pages inside one still-live explicit transaction share that read version;
   and
3. auto-commit pages are a live sequence of snapshots and may observe concurrent
   changes.

That is Java's scalability model made explicit in Go. It solves bounded progress
through large results. It does not manufacture a long-running snapshot that FDB
does not have.
