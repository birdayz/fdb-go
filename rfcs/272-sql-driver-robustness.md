# RFC-272: SQL driver robustness (quality review 2026-10-24)

**Status: Draft**

## Problem

The 2026-10-24 quality review lists nine SQL-driver findings (PR #994, `TODO.md` section
"Quality review 2026-10-24", "SQL"; not yet on master). The owner excluded the tenth, an
auto-commit SELECT running each page in its own transaction; it stays open and is not touched.

1. In an explicit transaction the caller's context never reaches FDB: `BeginTx` drops its
   context, `FDBDatabase.NewRecordContext` pins `context.Background()`, and the transaction is
   bound to the database's background context. A statement against a wedged cluster cannot be
   interrupted, and database/sql's rollback on a cancelled `BeginTx` context waits for that
   statement forever (`Tx.rollback` takes `closemu.Lock`). `TxOptions.ReadOnly` is ignored.
2. Panics are recovered only in `ExecContext`, `QueryContext` and `paginatingRows.Next`.
3. `Connector.initialize` runs in a `sync.Once`: one failed open breaks the `*sql.DB` for good,
   and the first caller's context governs everybody's initialization.
4. `ResetSession` keeps the previous borrower's statement timeout, result-byte cap,
   fail-on-scan-limit flag, plan and execution-stats loggers and slow-query threshold.
5. The parser's nesting guard counts parentheses inside string literals, quoted identifiers
   and comments: `SELECT '((((…'` with 501 of them in a literal is a syntax error.
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
  UNSUPPORTED_OPERATION) and accepts `setReadOnly` as an ignored hint
  (`EmbeddedRelationalConnection.java:143`). database/sql is the contract on this surface:
  `driver.ConnBeginTx` must "set the read-only transaction property if supported or return an
  error"; the `BeginTx` context "is used until the transaction is committed or rolled back".
- An in-flight FDB operation can only be interrupted by cancelling its transaction
  (`fdb_transaction_cancel`; the pure-Go `Transaction.Cancel` fails pending operations with
  1025). The auto-commit DML path already binds its own transaction this way (`context.AfterFunc`
  in `cascadesPlan.Execute`), detaching before commit so a dispatched commit's outcome is never
  replaced by the context error.
- database/sql discards a pooled connection only on `driver.ErrBadConn` from `ResetSession`, or
  when `driver.Validator.IsValid` reports false as it returns to the pool.
- database/sql's `DefaultParameterConverter` calls `Value` once. This driver's
  `CheckNamedValue` passes every value through and `parameterConstant` recurses through Valuer
  results, pointers and array elements, to type more than `driver.Value` (int32 lanes, UUIDs,
  arrays).
- Item 6 is reachable (probe with an instrumented arm over SimFDB): the check walks keys with
  `WalkExpression` (predicate position), which refuses a comparison, while the key itself is
  built with `WalkExpressionForProjection`; unresolvable keys and a nil scope also reach it.
  The arm then refuses `GROUP BY c = 'a', c = 'A'` and `amount = ?, amount = ?` (Java: two keys,
  literals compared by value, `Literals.java:199-205`) and returns 42702 for
  `GROUP BY nosuch + 1, nosuch + 1` (Java: 42703, grouping items resolve before the pull-up,
  `QueryVisitor.java:287-305`).

## Fix

1. **Context.** `beginTransaction(ctx)` builds the record context with the caller's context
   (`FDBDatabase.NewRecordContextIn`). `BeginTx` binds its context with `context.AfterFunc`:
   when it ends, the FDB transaction is cancelled and the `embeddedTx` ends with the context's
   error as cause; database/sql then rolls back. `runInCapturedTx` binds the statement's context
   the same way while it runs work on a captured transaction, a context already ended there also
   ends the transaction, so does a statement that fails with its context ended before reaching
   FDB (planning checks the context first), and so does `paginatingRows.Close` when the
   statement's context ended before the result set was read to its end (database/sql closes the rows itself then and never
   fetches another page), so whether a deadline lands during a read or between two pages does
   not decide the outcome. The statement returns the context error (54F01 for the statement
   timeout, as today); later statements and `Commit` get 25F01 wrapping the cause. This holds for
   SELECT too: a read cannot be abandoned without cancelling its transaction, and it is
   PostgreSQL's rule for a cancelled statement. In-transaction catalog reads take the statement
   context (`cachedLoadSchema(ctx, …)`). `Commit`, `Rollback`, `Close` and `ResetSession` detach
   the binding first and wait for an abort already running. `TxOptions.ReadOnly` is refused with
   0A000: enforcing it would need a write gate on every route (DML, DDL in its own transaction,
   direct access), and database/sql forbids ignoring it.
2. **Panics.** `recoveredPanicError` (XX000 "internal error", value and stack logged) at every
   exported entry that does work: `embeddedTx.Commit`/`Rollback`, `BeginTx`, `Close`,
   `ResetSession`, `Ping`, `SetSchema`, `PlanExplain`, `CollectStatistics`, `ClearStatistics`,
   `StatisticsStatus`, `ApplyMetadataOperation`, the five DirectAccess `Execute*` methods,
   `paginatingRows.Close` and `Connector.Connect`. After any recovered panic (the existing three
   sites included) the connection is closed and its transaction cancelled: `IsValid` reports
   false so database/sql discards it. `ResetSession` returns `driver.ErrBadConn` instead of XX000
   after one, the only error database/sql discards a connection for there.
3. **Connector.** A mutex-guarded single flight (`initFlight`) replaces the `sync.Once`: one
   `Connect` initializes while concurrent ones wait on a channel or their own context; success
   is kept; a failure (also the initializer's context ending during the stored-query warm-up,
   which logs its own failures) goes to that caller only, a panicking attempt releases its
   waiters, and the next `Connect` retries.
4. **ResetSession** restores the connector's configuration: the option set `SetOptions`
   installed and `New`'s values for the six Go-local settings, which no DSN sets. `SetOptions`
   keeps its meaning (it sets what `ResetSession` restores); per-borrow options go through
   `SetOption`, and the operator guide's per-tenant recipe (`docs/mt-saas.md`) switches to it, so
   nothing a tenant borrow sets reaches the next borrower.
5. **Nesting guard** tracks depth over the lexer's tokens in a pass of its own (no error
   listener), so the parser still lexes lazily and reports the first error it meets:
   `LEFT_ROUND_BRACKET` increments, `RIGHT_ROUND_BRACKET` decrements, and the maximum is
   compared with 500, as the rune scan did.
6. **GROUP BY**: the check walks keys with `WalkExpressionForProjection`, the walk that builds
   them, and the `GetText` arm is removed; a key that does not resolve fails on its own error
   when the key is built (`upgradeAggregateOperands`).
7. **Stale handles.** `Commit`/`Rollback` release the transaction from the connection only
   while it is still the connection's transaction, also when it already ended (a context ended
   it), so the connection is free for the next `BeginTx`; a handle for a different transaction
   touches nothing. `Commit` on an ended transaction returns 25F01 instead of committing again.
8. **Valuer recursion.** `parameterConstant` carries a depth bound of 32 across Valuer results,
   pointers and array elements; deeper nesting (any cycle) is 22023 naming the parameter.
9. Remove the four helpers and what they alone used (`ApplyBitOp`, `ToFloat64`,
   `Add/Sub/MulInt64Checked`, the Java-number renderers), with their tests, fuzz targets and
   benchmarks; correct the comments and TODO-production.md P0.3-F that name them; the txctl note
   states the behaviour (tracked on a `*sql.Conn`; on a `*sql.DB` the pool's `ResetSession`
   rolls it back before the next statement) and the probe asserts both.

**Docs.** CHANGELOG (breaking: ReadOnly, cancelled statements end the transaction, removed
helpers; fixed: items 2-8), a DIVERGENCES.md section for the driver transaction surface, the
BeginTx, Connector, ResetSession and setter doc comments, `docs/mt-saas.md` and its compile pin,
and `docs/panic-audit.md` with the norecover allowlist. If PR #994 has merged when this lands,
its SQL items are ticked; the excluded auto-commit SELECT item stays open.

## Performance

One `context.AfterFunc` registration per page or catalog call on an explicit transaction and
one per `BeginTx` with a cancellable context; nothing on the auto-commit SELECT path. The
nesting guard's rune scan becomes a lexer pass over warmed, pooled lexer state: linear, without
the parser's prediction cost.

## Test plan

Deterministic seams: `pkg/relational/internal/simgate` (testonly) wraps SimFDB so a test can
hold a read, a commit or a cancel in flight until it releases it (a read or commit also until
the transaction is cancelled), or make one transaction call panic. Where a test asserts that
something does not happen while a call is held, it waits 200 ms: a correct driver passes
always, a broken one fails nearly always.

- Item 1 (embedded, SimFDB): a statement context and a BeginTx context each interrupt a read held
  in flight (statement error, 25F01 with the cause afterwards, write not committed, Rollback frees
  the connection for the next BeginTx), also the statement's first read, its catalog load; a
  context ended between two pages ends the transaction, at the driver and through database/sql's
  `Rows` (closed by the ended context), while a result set read to its end keeps it; through
  database/sql's `Tx`, a cancelled BeginTx context with a read held in flight lets the rollback
  complete (a 30 s watchdog fails a hang); a commit held in flight while the BeginTx context ends
  keeps its success; Close and ResetSession wait for an abort held in its Cancel; a 40-iteration
  Commit/cancel race (race lane) as extra coverage; ReadOnly is 0A000; BeginTx with an ended
  context opens nothing, and the record context carries the BeginTx context (START TRANSACTION:
  none); a context ended before planning ends the transaction. End to end
  (`sqltest/core`): a statement timeout in an explicit transaction makes Commit 25F01 and drops
  the write; ReadOnly is refused.
- Item 2: a table test drives every listed entry, `ExecContext`, `QueryContext` and `Rows.Next`
  included, through a deterministic panic (simgate call, panicking logger, Struct or metadata
  operation) and asserts XX000 (`ResetSession`: ErrBadConn), `IsValid` false, no open
  transaction, ErrBadConn on the next statement, and no committed write; `Connector.Connect`
  with a panicking initialization returns XX000 and the next `Connect` succeeds.
- Item 3 (`sqldriver`): retry after a failed open; an initializer's context ending during the
  warm-up is not kept; `initFlight` unit tests for one attempt shared by concurrent callers, a
  waiter leaving on its own context, and a failure or panic reaching only its caller; each
  releases the first attempt only after the other callers are blocked on it (a context whose
  `Done` reports that `initFlight.do` reached its waiting select).
- Item 4: all six settings set to non-defaults (asserted), `ResetSession`, each back at `New`'s
  value with the `SetOptions` set kept; end to end through a one-connection pool, including a
  `SetOption` (MAX_ROWS) that the next borrower no longer sees.
- Item 5: parser tests at depth 500 and 501, parentheses in a literal, a quoted identifier and
  both comment forms, and the first syntax error unchanged by the guard's pass; end to end a 600-parenthesis literal, 600 sibling tuples, and 501 levels.
- Item 6: embedded regression (refused duplicates including `<>`/`!=` and `1`/`01`; literal-case
  and parameter-value keys accepted; unresolvable keys report their own error; a duplicated
  unresolvable key fails as the single key does), end-to-end cases in `sqltest/agg`, and four
  rows in the live-JVM `DuplicateGroupByJavaProbe` (`<>`/`!=` and `1`/`01` both 42702,
  literal-case keys planned by Go and declined by Java's planner, an undefined key twice both
  42703). The probe runner binds no parameters, so the parameter-value shape is Go-only.
- Item 7: a stale handle's Commit (25F01) and Rollback leave the newer transaction, through
  BeginTx and through SQL COMMIT/START TRANSACTION.
- Item 8: self-return, a two-Valuer cycle, a Valuer returning its own array, a self pointer, and a
  chain past the bound are 22023; a chain within it binds; end to end through database/sql.
- Item 9: the txctl probe asserts both START TRANSACTION behaviours.
