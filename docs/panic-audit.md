# Panic / recover audit (P0.2-CLASSIFY)

Date: 2026-06-07. Inputs: 4 parallel read-only classification passes.

**Policy:** reachable-from-user/external-input → return error; genuine fundamental
invariant → assert (panic, fail-stop); remove all `recover()`.

## Headline

- **158 panics**, **11 recovers** in non-test library code.
- **~24 panics are user-reachable → convert to errors.** Everything else (~134) is a
  legitimate invariant assert or a by-design `Must*` API.
- The effort is **plumbing, not site count**: two interface signature changes unlock the
  bulk of it.

## Convert-to-error worklist (the actual P0.2 work)

### Eval path — `pkg/recordlayer/query/plan/cascades/values` + `predicates` (22 convert)
Root cause: `Value.Evaluate(ctx) any` (values.go:125) and `QueryPredicate.Eval(ctx)
TriBool` have no error channel, so they panic. Fix = add the error channel.
- Arithmetic: `values.go:1700/1706/1712` overflow→22003, `1717/1725` div0→22012,
  `1720` overflow→22003, `1685/1692` type mismatch→22000.
- CAST: `values.go:1935/1945/1949/1955/1970/1974/1980/1999/2041/2056/2076` → InvalidCast
  (22F3H).
- Scalar fn / IN / comparison: `values.go:1295`, `comparisons.go:404/462` → 42804/22000.
- Typed errors already exist (`ArithmeticOverflowError`, `ArithmeticDivisionByZeroError`,
  `ScalarTypeMismatchError`, `InvalidCastError`, `TypeMismatchError`) and the SQL-code
  mapping already exists in `cascades_generator.go:1135 translateExecError`. No new
  mapping layer needed — just deliver the errors through returns instead of panic.

**Signature-change blast radius:**
- `Value.Evaluate(ctx) any` → `(any, error)`: **~60 implementations**, **~80 call sites**
  (44 in values/, 5 predicates/, 5 cascades rules, 26 in executor/).
- `QueryPredicate.Eval(ctx) TriBool` → `(TriBool, error)`: **12 impls**, ~8 call sites.
- Concentration: `executor/executor.go`.

### Record layer — `pkg/recordlayer/**` (excl. query) (2 convert)
- `metadata.go:476` unknown record type — **real bug**: DDL builder caller
  (`relational/.../builder.go:270`) already has dead nil-check code expecting a nil
  return; the panic defeats it. Make `GetRecordType` return nil/error; audit
  `catalog/metadata.go:48/56/63` non-nil assumptions.
- `key_expression.go:1133` `Literal: unsupported value type` — exported, on the
  value-keying path; convert (caller already returns error).

### fdbgo internals — `MustGet` control-flow callers → `.Get()` (8 sites)
Not panics-to-convert per se, but library internals that panic-on-error and only survive
via the `Transact` recover. Switch to `.Get()` + explicit error:
- `fdb/directory/directoryLayer.go:449/594/595/610`, `fdb/directory/node.go:63`
- `recordlayer/keyspace/fdb_resolver.go:65/72/148`

## Remove-all-recovers worklist (11 sites)
- `executor.go:734/918/2505`, `executor_new_plans.go:337` — bridge eval panics→errors;
  delete once Eval/Evaluate return errors. (The `:734`/`:337` `keep=false` default arms
  also silently drop rows — a separate policy violation, removed with the recover.)
- `values.go:416 EvaluateConstant`, `simplifier_value.go:218 tryCastConstant` — delete
  once Evaluate returns errors (inspect returned err instead).
- `merge_cursor.go:24` — catches `tuple.Pack()` panic on user-derived comparison keys;
  delete once tuple encoding returns errors. **See cross-area issue below.**
- `parser.go:39/99/121` — ANTLR panic-bailout → syntax error. Replace strategy with a
  collecting error listener so parse *returns* errors; then delete.
- `transaction.go:509 panicToError` — catches MustGet panics inside Transact; delete once
  the 8 internal callers above use `.Get()`.

## Keep-as-assert (representative; ~134 total)
- Cascades infra: **51, all assert** — BiMap/AliasMap bijection, Memo nil/empty, matcher
  constructor/binding preconditions, physical-wrapper arity, `Verify.verify` ports,
  phase/ordering invariants, RFC-077/037 tripwires, test-only plan constructors. None
  user-reachable.
- fdbgo: **30, all assert/by-design** — `Must*` API (future.go, range_result.go,
  database.go), directory root-partition guards, tuple **encode-side** guards, wire
  vtable/writer **encode/template-build** invariants. Zero decode-path panics.
- Record layer: cursor contract (`cursor.go:122/138/141/159`), iterator contracts
  (`text_tokenizer.go:214`), self-consistency (`tuple_ordering.go:166`), `store_typed.go:238`
  unreachable, `keyspace.go:163` config-time, chaos test harness (2).

## Cross-area issue to resolve
**`tuple.Pack()` reachability.** fdbgo classified its tuple-encode panics
(`fdb/tuple/tuple.go:289/357/406/456/461`) as encode-side asserts ("app packs a bad
tuple"). But `merge_cursor.go:24` recovers a `tuple.Pack()` panic driven by **user record/
index data** via `ComparisonKeyFunc`. So either (a) the record→comparison-key path can
only ever produce encodable types (then the merge recover is dead and removable, tuple
panics stay asserts), or (b) user data can reach an "unencodable element" (then the
comparison-key path must return an error and tuple needs an error-returning pack variant).
**Must verify before deleting `merge_cursor.go:24`.**

## What makes fail-stop safe (P0.2-F)
Fuzz the SQL parser+executor and the wire decoder asserting **no panic on any input**.
This is the load-bearing regression net: a panic in production must mean a genuine bug,
never user input.
