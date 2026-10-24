# RFC-272: SQL driver robustness (quality review 2026-10-24)

**Status: Draft**

## Problem

The 2026-10-24 quality review (TODO.md, "Quality review 2026-10-24", SQL section) lists nine
findings in the database/sql driver and the embedded connection. The multi-page auto-commit
SELECT finding is excluded by the owner and is not touched here.

1. In an explicit transaction the caller's context never reaches FDB: `BeginTx` drops its
   context, `FDBDatabase.NewRecordContext` pins `context.Background()`, and the transaction is
   created bound to the database's background context. A statement against a wedged cluster
   cannot be interrupted, and database/sql's rollback on a cancelled `BeginTx` context waits for
   that statement forever (`Tx.rollback` takes `closemu.Lock`). `TxOptions.ReadOnly` is
   silently ignored.
2. Panics are recovered only in `ExecContext`, `QueryContext` and `paginatingRows.Next`.
3. `Connector.initialize` runs in a `sync.Once`: one failed open breaks the `*sql.DB` for good,
   and the first caller's context governs everybody's initialization.
4. `ResetSession` keeps the previous borrower's statement timeout, result-byte cap,
   fail-on-scan-limit flag, plan and execution-stats loggers and slow-query threshold.
5. The parser's nesting guard counts parentheses inside string literals, quoted identifiers
   and comments, so `SELECT '((((…' …` with 501 parentheses in a literal is a syntax error.
6. The GROUP BY duplicate-key check falls back to `EqualFold` over `GetText()`.
7. A stale `driver.Tx` handle's `Commit`/`Rollback` clears a newer transaction from the
   connection, which then runs the rest of that transaction in auto-commit.
8. A `driver.Valuer` whose `Value` returns itself recurses until the goroutine stack overflows,
   a fatal error that no `recover` catches.
9. `functions.ApplyMathOp`, `IsTruthy`, `LiteralMatchesPKKind` and `ToIntegerArg` have no
   production caller; the `txctl_statements_probe_test.go` note says `START TRANSACTION` opens
   no driver-tracked transaction, which the code contradicts.

## Investigation

- Java Relational 4.14.2.0 has no statement cancellation (`RelationalStatement.cancel` throws
  UNSUPPORTED_OPERATION) and swallows `setReadOnly` as a JDBC hint
  (`EmbeddedRelationalConnection.java:143`). database/sql is the contract on this surface:
  `driver.ConnBeginTx` must "set the read-only transaction property if supported or return an
  error"; the `BeginTx` context "is used until the transaction is committed or rolled back".
- An in-flight FDB operation can only be interrupted by cancelling its transaction
  (`fdb_transaction_cancel`; the pure-Go `Transaction.Cancel` fails every pending operation
  with 1025). The auto-commit DML path already binds its own transaction this way
  (`context.AfterFunc` in `cascadesPlan.Execute`), detaching before commit so a dispatched
  commit's outcome is never replaced by the context error.
- database/sql's `DefaultParameterConverter` calls `Value` once and refuses a result that is
  not a `driver.Value`. This driver's `CheckNamedValue` passes every value through, and
  `parameterConstant` recurses through Valuer results, pointers and array elements so it can
  type more than `driver.Value` (int32 lanes, UUIDs, arrays).

## Fix

1. **Context.** `beginTransaction(ctx)` builds the record context with the caller's context
   (`FDBDatabase.NewRecordContextIn`). `BeginTx` registers `context.AfterFunc` on its context:
   when it ends, the FDB transaction is cancelled and the `embeddedTx` is ended with the
   context's cause; database/sql then rolls back. While `runInCapturedTx` runs work on a
   captured transaction, the statement's context is bound the same way: a context already
   ended returns its error and leaves the transaction alone; one that ends during the call
   cancels the FDB transaction, the statement returns the context error (54F01 for the
   statement timeout, as today) and the transaction is over. Later statements get 25F01, and
   `Commit` returns 25F01 wrapping the cause. This is PostgreSQL's rule for a cancelled
   statement, and it is forced here: a statement cut off midway may have buffered part of its
   writes. `Commit`, `Rollback`, `Close` and `ResetSession` detach the binding first and wait
   for one already running. `TxOptions.ReadOnly` is refused with 0A000, like an unsupported
   isolation level: enforcing it would need a write gate on every route (DML, DDL in its own
   transaction, direct access), and Java does not enforce it either.
2. **Panics.** The same `recoveredPanicError` (XX000 "internal error", value and stack logged)
   at every exported entry that does work: `embeddedTx.Commit`/`Rollback`, `BeginTx`, `Close`,
   `ResetSession`, `Ping`, `SetSchema`, `PlanExplain`, `CollectStatistics`, `ClearStatistics`,
   `StatisticsStatus`, `ApplyMetadataOperation`, the five DirectAccess `Execute*` methods (an
   own transaction is rolled back on the way out) and `paginatingRows.Close`.
3. **Connector.** A mutex-guarded single flight replaces the `sync.Once`: one `Connect`
   initializes while concurrent ones wait on a channel or their own context; success is kept,
   a failure (including the initializer's context ending, also during the stored-query
   warm-up) goes to that caller only and the next `Connect` retries.
4. **ResetSession** restores the connector's configuration: the option set `SetOptions`
   installed (unchanged) and `New`'s values for the six Go-local settings, which no DSN sets.
5. **Nesting guard** counts `LEFT_ROUND_BRACKET` tokens of the filled token stream, which the
   parser then consumes, instead of runes.
6. **GROUP BY fallback**: decided by a reachability probe (see the PR); a reachable arm gets a
   typed parse-tree comparison, an unreachable one is removed.
7. **Stale handles.** `Commit`/`Rollback` clear `activeTx` only while it is still this
   transaction; `Commit` on an ended transaction returns 25F01 instead of committing again;
   `Rollback` on one is a no-op.
8. **Valuer recursion.** `parameterConstant` carries a depth bound of 32 across Valuer
   results, pointers and array elements; deeper nesting (any cycle) is 22023.
9. Remove the four helpers with their tests, fuzz target and benchmarks; correct the comments
   and TODO-production.md P0.3-F that name them; the txctl note states the actual behaviour
   (tracked on a `*sql.Conn`; on a `*sql.DB` the pool's `ResetSession` rolls it back before
   the next statement) and the probe asserts both.

## Performance

One `context.AfterFunc` registration per page or catalog call on an explicit transaction and
one per `BeginTx` with a cancellable context; nothing on the auto-commit SELECT path. The
nesting guard reuses the token stream the parser needs anyway and drops the separate rune scan.

## Test plan

Each fix gets a regression that fails before it: SimFDB-backed internal tests in
`pkg/relational/core/embedded` (fast lane) for the context binding, stale handles, the panic
boundaries, ResetSession and parameter depth; parser unit tests for the guard; a connector
retry test in `pkg/relational/sqldriver`; and end-to-end tests over real FDB in
`pkg/relational/sqltest/core` for the SQL-visible behaviour (statement context in an explicit
transaction, ReadOnly, ResetSession through a pool, the literal-parentheses query, the
self-returning Valuer).
