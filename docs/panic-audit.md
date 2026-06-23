# Panic / recover audit & boundary gate

Last refreshed: 2026-06-20 (RFC-134). Original classification: 2026-06-07.

**Policy.** Reachable-from-user/external-input → return an error; a genuine fundamental
invariant → assert (panic, fail-stop). `recover()` is legitimate **only** at a deliberate
panic→error boundary (translating a panic into a returned error / failed future / failed
connection), never as a silent swallow. `SECURITY.md` scopes crash/DoS: untrusted input must
produce errors, never process crashes.

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

- **155** `panic(` and **22** `recover(` text occurrences in non-test code (grep). The grep
  count over-states *callable* recovers — several are in comments/strings. The analyzer's **AST**
  count of builtin `recover()` calls is **§2's allowlist** (17 across 12 files, of which 15 are in
  the default pure-Go build; 2 are behind `cgo && libfdbc` / the binding-tester binary).
- The vast majority of panics are legitimate invariant asserts or `Must*` APIs (§4).
- The user-reachable-panic conversion worklist (§3) — adding an error channel to
  `Value.Evaluate` / `QueryPredicate.Eval` — remains **deferred**: a large signature-change
  refactor, tracked here, not part of the gate. The gate proves the *existing* boundaries hold
  (the four fuzz nets find no live panic, §5); the refactor would let more of the eval path
  return errors *directly* rather than via the boundary recover.

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

None silently swallow: each maps to a returned error / failed future / logged-and-failed conn.

## §3 — Convert-to-error worklist (DEFERRED — the big eval refactor)

Root cause: `Value.Evaluate(ctx) any` (`values.go`) and `QueryPredicate.Eval(ctx) TriBool` have no
error channel, so arithmetic overflow/div0, CAST failures, and type mismatches `panic` and are
caught by the executor's boundary recover (`cascades_generator.go`) instead of returned. Typed
errors and the SQL-code mapping already exist (`ArithmeticOverflowError`, `InvalidCastError`, …,
`translateExecError`); the work is delivering them through returns. Signature blast radius:
`Value.Evaluate` → `(any, error)` is ~60 impls / ~80 call sites; `QueryPredicate.Eval` →
`(TriBool, error)` is ~12 impls. This is a multi-change refactor tracked as its own item — the
gate (the boundary recover + the fuzz net) keeps these safe in the meantime.

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
