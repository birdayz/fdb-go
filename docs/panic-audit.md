# Panic / recover audit & boundary gate

Last refreshed: 2026-07-21 (LATEST PRIOS item 1, the executor panic sweep). Prior refresh:
2026-06-20 (RFC-134). Original classification: 2026-06-07.

**Policy.** Reachable-from-user/external-input → return an error; a genuine fundamental
invariant → assert (panic, fail-stop). `recover()` is legitimate **only** at a deliberate
panic→error boundary (translating a panic into a returned error / failed future / failed
connection), never as a silent swallow. `SECURITY.md` scopes crash/DoS: untrusted input must
produce errors, never process crashes.

**The assert-locality rule (2026-07-21).** An assert (panic) may guard only an invariant
established *within the same function/derivation* — a local impossibility (e.g.
`ordinal_join.go` `ordinalJoinSpans`' sum-of-widths pin, `positional_merge.go`'s
idx-in-own-range bake). Any check that detects a malformed **plan** — an inconsistency
produced by a *different component* (the planner) and merely observed at execution — returns
an error instead: a planner bug must fail the QUERY, never the process. This is Java parity
(`RecordCoreException` propagates through the cursor future chain as a recoverable error) and
it matters because exported executor entry points (`ExecutePlan`) deliberately have **no**
recover boundary — direct Record Layer API clients would crash on what SQL clients survive
only via the `cascades_generator.go` boundary recover.

## The gate (RFC-134) — this is what keeps the discipline honest

The discipline is no longer a one-time audit; it is **enforced on every build**:

1. **`norecover` nogo analyzer** (`pkg/linters/norecover`) — the recover ratchet. It counts
   builtin `recover()` calls per file and compares to the allowlist in §2 (baked into the
   analyzer). A `recover()` in a non-allowlisted file, or *more* than a file's allowance, is a
   **nogo build error**. Removing a recover never reddens the build (it fires on *more*, never
   *fewer*), so deleting a boundary needs no edit; adding one is a conscious act (update the
   allowlist + this doc). Test files are exempt. Runs in `just build` / `just test` / pre-commit.
2. **Boundary fuzz-net guard** (`pkg/docscheck/panic_boundary_test.go`) — asserts each of the
   four public input boundaries keeps a seeded no-panic fuzz target, so malformed input is
   actually exercised (not an empty fuzz). Losing a fuzzer or its `f.Add()` seeds → red.

## Headline (current)

- **173** `panic(` and **34** `recover(` text occurrences in non-test code (grep, 2026-07-21).
  The grep count over-states *callable* recovers — several are in comments/strings. The
  analyzer's **AST** count of builtin `recover()` calls is **§2's allowlist**.
- The vast majority of panics are legitimate invariant asserts or `Must*` APIs (§4).
- The §3 eval refactor is **DONE** (landed with the RFC-173 campaign): `Value.Evaluate`
  returns `(any, error)` and `QueryPredicate.Eval` returns `(TriBool, error)`. Eval failures
  flow as errors; the boundary recovers in §2 remain as backstops, no longer the primary
  channel.
- **2026-07-21 executor sweep (LATEST PRIOS item 1):** `ComparisonKeyFunc` carries an error
  return (`func(T) (tuple.Tuple, error)`) — the intersection/multi-intersection comparison-key
  closures return errors instead of panicking (Java parity:
  `KeyedMergeCursorState.comparisonKeyFunction` failures are `RecordCoreException`s through
  the future chain). The `ordinal_join.go` cross-component malformed-plan tripwires
  (`evaluateOrdinalJoinRow`, `newOrdinalJoinBuild`, `widenLegTypesFromPlan`,
  `probeOuterBakedType`) and the un-lowered DistanceRank row-eval arm
  (`predicates/comparisons.go`) return errors per the assert-locality rule above.
  `NewRecordType`'s duplicate-field-name panic stays a §4 constructor assert. The full
  reachability story, each step verified live: the first probe's "every call site is
  descriptor-sourced" claim was WRONG (Torvalds review catch) — `unnest_seed.go` builds a
  seed leg RecordType from USER SQL aliases. But the review's counterexample
  (`... AT "_0"` with no AS, claimed to collide with the seed's reserved `_0` element slot)
  is ALSO wrong: `lateralUnnestCandidate` is the only `LogicalUnnest` producer and
  `unnestAliases` defaults the element alias to the array FIELD NAME, so the seed's
  `Alias == ""` fallback is producer-dead and the query is benign (FDB pin:
  `array_unnest_ordinality_fdb_test.go` "AT alias spelling the reserved element name is
  benign"). Defense anyway: the shared `unnestAliasReject` guard
  (`pkg/relational/core/query/unnest_alias_guard.go`, wired into all four unnest
  translate/admission sites, deduping the four inline AS==AT checks) rejects the
  reserved-name collision as a producer invariant, so a future producer that skips the
  default hits a typed DuplicateAlias, never the constructor assert. The remaining
  `NewRecordType` call sites are descriptor-sourced (unique by construction; DDL rejects
  duplicate columns with 42701).

## §2 — The panic→error boundary allowlist (the `norecover` allowlist)

Every `recover()` below is a deliberate panic→error translation; this table IS the analyzer's
allowlist (file → permitted AST count). Keep the two in sync.

| File | Count | Boundary / role |
|---|---|---|
| `pkg/fdbgo/client/panicbackstop.go` | 1 | client callback/goroutine backstop: panic → error/log, never crash the host |
| `pkg/fdbgo/client/database.go` | 2 | `Run`/retry callback backstop on the client transact loop |
| `pkg/fdbgo/client/grv.go` | 1 | background GRV goroutine: a panic fails the conn, not the process |
| `pkg/fdbgo/transport/conn.go` | 2 | read/write IO goroutines: panic → fail connection |
| `pkg/fdbgo/fdb/panic.go` | 1 | `panicToError` — a panicking user closure becomes a tx error |
| `pkg/fdbgo/fdb/transaction.go` | 1 | `Transact` closure boundary |
| `pkg/fdbgo/libfdbc/backend.go` | 1 | cgo libfdb_c backend: translate cgo/callback panics to Go errors (`cgo && libfdbc` only) |
| `pkg/relational/core/parser/parser.go` | 4 | ANTLR bailout panic → syntax error |
| `pkg/relational/core/embedded/connection.go` | 2 | public SQL connection API: panic → SQL error |
| `pkg/relational/core/embedded/cascades_generator.go` | 1 | executor eval bridge: panic → SQL error |
| `pkg/recordlayer/merge_cursor.go` | 1 | `tuple.Pack()` on user-derived comparison keys → cursor error |
| `cmd/fdb-stacktester/directory_ops.go` | 1 | binding-tester harness binary (cgo-dependent build) |
| `pkg/relational/conformance/explaindiff/explaindiff.go` | 1 | RFC-183 EXPLAIN-differ: a planner panic on one corpus query → `<PLAN-PANIC: …>` marker, so the corpus-wide dump completes instead of aborting. Surfaced, not swallowed: `TestNoPlanPanics` fails on any marker. |

None silently swallow: each maps to a returned error / failed future / logged-and-failed conn.

## §3 — Convert-to-error worklist (DONE)

The big eval refactor LANDED (with the RFC-173 campaign): `Value.Evaluate` returns
`(any, error)` and `QueryPredicate.Eval` returns `(TriBool, error)`. Arithmetic overflow/div0,
CAST failures, and type mismatches return typed errors (`ArithmeticOverflowError`,
`InvalidCastError`, …) mapped by `translateExecError`; the §2 boundary recovers remain as
backstops only. The 2026-07-21 executor sweep (see Headline) extended the error channel to the
last no-error-channel closure contract (`ComparisonKeyFunc`) and converted the executor's
cross-component malformed-plan tripwires per the assert-locality rule.

## §4 — Keep-as-assert (representative; the large majority)

Cascades infra (BiMap/AliasMap bijection, Memo nil/empty, matcher preconditions, physical-wrapper
arity, phase/ordering invariants), fdbgo `Must*` APIs (future, range_result, database), directory
root-partition guards, tuple **encode-side** guards, wire vtable/writer encode/template-build
invariants (zero decode-path panics), record-layer cursor/iterator contracts, config-time checks.
None are user-input-reachable.

## §5 — Fail-stop safety (the four boundary fuzz nets)

The load-bearing regression net: a panic in production must mean a genuine bug, never user input.
The four audit-named boundaries each have a seeded no-panic fuzz target, pinned by the fuzz-net
guard (§"The gate"):

- **parser** — `FuzzParse` (`pkg/relational/core/parser/fuzz_test.go`)
- **planner** — `FuzzTranslateToCascades` (`pkg/relational/core/query/cascades_translator_test.go`)
- **wire decode** — `FuzzNewReader` (`pkg/fdbgo/wire/reader_fuzz_test.go`)
- **tuple decode** — `FuzzUnpack` (`pkg/fdbgo/fdb/tuple/tuple_malformed_test.go`)

RFC-134 pre-flight fuzzed each at `-fuzztime=60s`: zero panics / zero new crashers (see the PR).

## Cross-area note

`tuple.Pack()` encode-side panics (`fdb/tuple/tuple.go`) are classified as asserts ("app packs a
bad tuple"), but `merge_cursor.go:24` recovers a `tuple.Pack()` panic driven by **user record/index
data** via `ComparisonKeyFunc`. That boundary recover is therefore on the allowlist (§2). If the
comparison-key path is ever proven to only produce encodable types, the recover becomes dead and
both it and its allowlist row should be removed (the gate fires on the now-zero count being
exceeded, never on its removal).
