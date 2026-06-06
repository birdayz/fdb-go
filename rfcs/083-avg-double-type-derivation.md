# RFC-083 — AVG result-type derivation: `AVG(x) → DOUBLE`, INSERT promotion parity

**Status:** Draft v2 — revised after Graefe + Torvalds NAK on v1. Awaiting re-ACK before impl.
**TODO:** Known gaps line 69 (🚩). Graefe ACK'd the *diagnosis* + *direction*; v1 NAK'd on the
empty-source axis (Graefe) and converter convergence (Torvalds). v2 closes both.

## Problem

Java types `AVG(BIGINT) → DOUBLE` / `SUM(BIGINT) → BIGINT` and enforces assignability at PLAN
time via `PromoteValue` (the promotion lattice is up-only: INT→LONG→FLOAT→DOUBLE, **no**
DOUBLE→LONG edge). So `INSERT INTO dst(v BIGINT) SELECT AVG(v) FROM src` is **rejected**
unconditionally — `SemanticException(INCOMPATIBLE_TYPE)`, message *"A value cannot be assigned
to a variable … cannot be promoted …"*, SQLSTATE **22000** — while `SELECT SUM(v)` is accepted.

Go diverges. Two root causes:
1. `AggregateValue.Type()` (`values.go:2450`) bundles `AggSum/Min/Max/Avg` and returns
   `WithNullability(operand.Type())`, so `AVG(BIGINT)` is mistyped **LONG**.
2. There is **no plan-time promotion check** (`IsPromotable`, `type.go:894`, has zero production
   callers). Enforcement is purely the two runtime converters, which disagree with Java and with
   each other.

### Ground truth (empirically verified against real FDB, current HEAD)

| INSERT case | Current Go | Java | Converter |
|---|---|---|---|
| `SELECT SUM(v) → BIGINT` | OK (int64) | OK | `goToProtoValue` |
| `SELECT AVG(v) → BIGINT` (non-empty) | **ERROR, generic** `*errors.errorString` *"cannot convert float64 to proto field kind int64"*, **no SQLSTATE** | 22000 reject | `goToProtoValue` falls through |
| `SELECT AVG(v) → BIGINT` (**empty/all-NULL src**) | **OK — 0 rows inserted** (no float ever materializes) | 22000 reject (unconditional) | none reached |
| `SELECT AVG(v) → DOUBLE` | OK | OK | `goToProtoValue` |
| `SELECT SUM(v) → DOUBLE` | **ERROR** *"cannot convert int64 to proto field kind double"* | OK (LONG→DOUBLE promotes) | `goToProtoValue` falls through |
| `VALUES (5.0) → BIGINT` | **OK** (whole-float silently coerced 5.0→5) | 22000 reject | `ConvertToProtoValue` |
| `VALUES (5.5) → BIGINT` | ERROR 22000 | 22000 reject | `ConvertToProtoValue` |

So: `goToProtoValue` (the INSERT…SELECT / UPDATE converter) is missing the up-promotions Java
allows (LONG/INT→DOUBLE/FLOAT) **and** rejects incompatible types with a generic non-SQLSTATE
error; `ConvertToProtoValue` (the INSERT…VALUES converter) silently coerces whole DOUBLE→LONG;
and neither fires when the source produces zero rows. The yamsql `insert_select.yaml` corpus
claims AVG→BIGINT *succeeds* (lines 67/70) — but that harness is `t.Skip`'d
(`runner_test.go:61`), so the claim was never executed and is wrong vs Java.

## Design

Three coordinated changes. (A) makes the node type correct; (B) adds the emergent *plan-time*
rejection keyed on the now-reliable type (closes the empty-source axis); (C) converges the two
runtime converters on Java's promotion lattice (backstop for sources whose plan-time type is not
yet reliable).

### (A) Node type correctness — AVG → DOUBLE

`AggregateValue.Type()`: split `AggAvg` out of the SUM/MIN/MAX arm.

```go
case AggCount, AggCountStar:
    return NotNullLong
case AggAvg:
    return NullableDouble          // AVG is real division → DOUBLE (Java AVG_*→DOUBLE)
case AggSum, AggMin, AggMax:
    if a.Operand != nil { ...operand-derived... }
    return NullableLong
```

Matches Java's per-operator `resultTypeCode`. SUM/MIN/MAX/COUNT unchanged.

**(A2) Consolidate (Torvalds):** the schema-metadata AVG arms (`aggregateResultType:2057`,
`valueTypeName:2090`) currently hardcode `"DOUBLE"` — a second/third encoding of the same fact.
After (A), `AggregateValue.Type()` is the single source of truth, so the AVG arms delegate to it.
SUM/MIN/MAX **stay descriptor-resolved** (an unbound `FieldValue.Type()` defaults to BIGINT, so
their operand type must come from the record descriptor — unchanged).

### (B) Narrow plan-time promotion guard — closes the empty-source axis (Graefe + Torvalds)

**Two crux findings from review:** (v2) by the time INSERT…SELECT column-alignment runs,
`rewriteAggregateValuesInTree` (`logical_predicate.go:1856/1902`) has already replaced the
`AggregateValue` in the projection with `rewriteAggregateValue` →
`FieldValue{Field: canonicalAggName(…), Typ: UnknownType}` — so keying on `*AggregateValue` is dead
code. (v3) keying on `.Type()`-presence is *also* unsound: `Resolver.ResolveIdentifier`
(`expr.go:235`) types **plain columns concretely** (`Typ: sqlTypeToCascadesType(col.Type)`,
stored at `plan_visitor.go:472/443`), so `SELECT t.amount` reaches the guard concrete-typed and a
type-presence discriminator would false-reject plain-column narrowing the RFC means to defer.
The discriminator must be **provenance** (this projection slot is an aggregate result), not type.
Two parts:

**(B′) Stop discarding the aggregate's result type on rewrite.** `rewriteAggregateValue` sets
`Typ: av.Type()` instead of `UnknownType` — independently correct (a reference must report its
referent's type; the rewrite was discarding it). Post-(A): `AVG`→`NullableDouble`,
`SUM`/`MIN`/`MAX`→concrete operand type (operands are themselves concretely typed by
`ResolveIdentifier`, so this is reliable), `COUNT`→`NotNullLong`. Safe: the only
`Typ==UnknownType` consumers are `scan_match_helpers.go:60,64` (index-matching, not an
"is-aggregate" sentinel).

**(B″) Provenance marker + guard.** Add `AggregateSlots []bool` to `LogicalProject` (parallel to
`Projections`, like the existing `IsComputed []bool`). Set it at the projection-build loops
(`logical_predicate.go:1856/1902`) at the **pre-rewrite** point, via a **tree-contains-aggregate**
walk (NOT a top-level type assert — `AVG(x)+1` has a top-level `ArithmeticValue`):
```
aggSlots[i] = containsAggregate(v)            // before rewrite — WalkValue finds any *AggregateValue
v = rewriteAggregateValuesInTree(v)
```
where `containsAggregate` is `values.WalkValue` (`values.go:336`) returning true on the first
`*values.AggregateValue`. The guard (at the INSERT…SELECT chokepoint, after alignment) keys on the
marker — the **first production caller** of `IsPromotable`:
```
for each slot i where proj.AggregateSlots[i]:
    st := proj.ProjectedValues[i].Type()                       // reliable via (B′)
    if !IsPromotable(st, protoKindToValueType(targetCol[i])) { reject verbatim-22000 }
```
Provenance is the bool; type is the rewritten value's `Type()` (single source, from B′). For
`AVG(x)+1` the rewrite leaves an `ArithmeticValue{FieldValue(DOUBLE), 1}` whose `Type()` is DOUBLE
(`ArithmeticValue.Type()` propagates the double operand, `values.go:1651`) → rejected even over an
empty source (the hole a top-level assert would miss). Plain columns / `amount+1` (no aggregate) →
`AggregateSlots[i]=false` → skipped → **no false-reject**; plain-column narrowing (`LONG→INT`,
`DOUBLE-col→INT`) stays deferred to the runtime backstop (C). Because operands are concretely
typed, the guard is correct for *every* aggregate, not just AVG (`SUM(DOUBLE)→BIGINT` rejects;
`SUM(BIGINT)→BIGINT` accepts).

This closes the empty-source axis emergently: an AVG slot's `Type()` is DOUBLE *structurally,
independent of row materialization*, so empty/all-NULL/all-filtered sources reject via
`IsPromotable(DOUBLE, LONG)=false` — the lattice has no down-edge; nothing consults a materialized
value. (The "aggregate slot ⇒ guard" coupling is load-bearing — comment it and point at the
PromoteValue end-state, per Graefe's standing note.)

Error propagation: surface at the INSERT…SELECT logical-build chokepoint where the existing
"setting column ordering …" rejection already returns `api.NewError`, covering every
`buildLogicalPlanForInsertWithCatalog` caller; verified to surface as 22000 through the driver.

Nullability is orthogonal/unchanged — `IsPromotable` ignores it (`type.go:884-888`).

### (C) Converge the runtime converters on Java's lattice (Torvalds)

Make `goToProtoValue` (`executor.go`) and `ConvertToProtoValue` (`proto_value.go`) agree — both
implement the same promotion lattice and the same 22000 incompatible-type rejection:

1. **`ConvertToProtoValue` (VALUES):** remove the whole-valued-float→int64 coercion
   (`proto_value.go:164-190`). DOUBLE→integer then falls through to the existing verbatim-22000
   error (`:269`). (The coercion's stated justification — "supports INSERT…SELECT SUM(v)" — is
   stale: SUM returns int64, and INSERT…SELECT routes through `goToProtoValue`, not this.)
2. **`goToProtoValue` (SELECT/UPDATE):**
   - Add the missing **promotable widenings** so it matches `ConvertToProtoValue`: `int64`/`int`
     → `FloatKind`/`DoubleKind` (LONG/INT→FLOAT/DOUBLE). This fixes the adjacent `SUM(v)→DOUBLE`
     divergence (currently errors) as a natural consequence.
   - Change the generic fallthrough (`executor.go:2127`) from `fmt.Errorf(...)` to the verbatim
     22000 `api.NewErrorf(api.ErrCodeCannotConvertType, …)`. With all *promotable* conversions now
     handled explicitly, the fallthrough is genuinely "incompatible type" — so `float64→integer`
     (AVG into a BIGINT over a non-empty source where the guard didn't fire, e.g. a bare-column
     double source) rejects **emergently** with Java's wording, no per-kind `case float64:` bolt-on.
     This also aligns every other mismatch (string→int, bool→int) with Java + the sibling converter.

After (B)+(C): AVG→BIGINT rejected 22000 on every path and row-count (plan-time for the aggregate
source; runtime backstop otherwise); SUM→BIGINT and SUM/AVG→DOUBLE accepted; VALUES double→int
rejected 22000.

### Ripple guard (Graefe ACK'd, holds by construction)

AVG must never be lowered to `Sum(LONG) OpDiv Count(LONG)` — `ArithmeticValue` does integer
`LONG/LONG→LONG` (`values.go:1657`) and integer `Evaluate` (`:1722`). Go does not lower AVG: the
streaming cursor computes `sums/float64(count)` directly (`streaming_cursors.go:329`) and AVG has
no aggregate index (`GetIndexTypeName→""`, `values.go:2507`; matches Java `Avg` ∉
`IndexableAggregateValue`). Pinned by an EXPLAIN index-presence test.

## Testing

- **Unit — `AggregateValue.Type()`:** AVG{INT,LONG,FLOAT,DOUBLE} → `NullableDouble`; SUM/MIN/MAX
  operand-derived unchanged; COUNT → `NotNullLong`.
- **Unit — converters (flip BOTH, Torvalds):** `TestConvertToProtoValue_Int64_FromWholeFloat`
  (:178) **and** `TestConvertToProtoValue_Int64_FromWholeFloat_Large` (:729) → expect 22000 reject
  (after verifying Java rejects whole-double→BIGINT). New: `goToProtoValue(int64→DoubleKind)` and
  `(int64→FloatKind)` succeed; `(float64→Int64Kind)` → 22000; fallthrough (string→int) → 22000.
- **FDB integration (`*_fdb_test.go`, the load-bearing pins — yamsql is skipped):**
  - `SELECT AVG(v) → BIGINT` non-empty → 22000; **empty / all-NULL / all-filtered src → 22000**
    (the Graefe axis); `→ DOUBLE` → OK.
  - **`SELECT AVG(v)+1 → BIGINT` over an EMPTY src → 22000** (the tree-contains-aggregate axis,
    Torvalds — a top-level assert would miss it); plain `SELECT amount+1 → BIGINT` still accepted
    (no aggregate → deferred to C).
  - `SELECT SUM(v) → BIGINT` → OK; **`→ DOUBLE` → OK** (pins the converged converter).
  - `VALUES (5.0) → BIGINT` → 22000; `VALUES (5.5) → BIGINT` → 22000.
  - `SELECT AVG(v)` runtime type float64; empty group `SELECT AVG(v)` → NULL (unchanged).
  - **Index-presence (Graefe):** AVG over `src` carrying a SUM aggregate index on `v` — EXPLAIN
    shows no AggregateIndex for AVG (still streams), types DOUBLE, rejects into BIGINT.
- **Corpus + the CASE path (Torvalds):** fix `insert_select.yaml` lines 62-84 (AVG→BIGINT rejects
  22000; SUM→BIGINT OK). The removed coercion's comment explicitly fed **CASE-int/double→int**
  (`INSERT INTO int_col VALUES (CASE WHEN … THEN 1 ELSE 2.0 END)` types DOUBLE → float64): enumerate
  and flip every such corpus/unit case (grep `case-when`/CASE yamsql + the values/scalar tests
  found near `FromWholeFloat`), verifying Java rejects DOUBLE→integer each time (lattice: no
  down-edge, value-independent — whole and fractional alike).
- **Determinism / plandiff / 1M stress:** byte-identical plans for unaffected queries; 10×
  determinism on new FDB tests; stress before/after within noise (type-only + reject-path change).
- **covering_index.go callers** (`:253,:288`): unaffected — tuple-decoded record values are
  kind-aligned, a float64 never reaches an integer field there (stated, not just assumed).

## Out of scope (follow-ups — capture in TODO.md so they aren't dropped)

- **Replace the guard with `PromoteValue` projection nodes (Graefe's end-state).** Java uses ONE
  mechanism — a `PromoteValue` on the projection that both rejects-at-plan and widens-at-runtime —
  rather than v2's split of guard (B) + converter (C), which re-encodes the lattice in two places.
  The follow-up is to converge on that single structure (which also subsumes reliably typing
  `FieldValue`/`ArithmeticValue` projections), not merely "extend the guard to FieldValue".
- **Residual divergences to log now:** bare-column `SELECT double_col → BIGINT` over an empty/all-
  filtered source (runtime backstop misses the 0-row case until the PromoteValue port); the
  same-class **UPDATE … SET int_col = <double-expr>** aggregate/double case. Both currently rely on
  the runtime converter and so share the empty/no-row gap.
- **`GetIndexTypeName` `MIN_EVER_LONG`/`MAX_EVER_LONG` hardcode** — needs `MIN_EVER_TUPLE` for
  non-long operands (separate index-type bug; Java `permuted_min/max`).
- **Un-skip the `TestYamsqlConformance` harness** (`runner_test.go:61` is an unconditional
  `t.Skip` — a NO-SKIPS-rule violation) — separate cleanup; this RFC keeps the corpus *correct*
  but pins behavior via the in-CI sqldriver FDB tests.

## Files

| File | Change |
|------|--------|
| `…/cascades/values/values.go` | AVG arm → `NullableDouble` |
| `…/embedded/cascades_generator.go` | `aggregateResultType`/`valueTypeName` AVG arms delegate to `Type()` |
| `…/query/logical/operators.go` | `LogicalProject.AggregateSlots []bool` (provenance) |
| `…/embedded/logical_predicate.go` | (B′) `rewriteAggregateValue` `Typ: av.Type()`; set `AggregateSlots` at the rewrite loops; (B″) plan-time promotion guard at INSERT…SELECT chokepoint (IsPromotable + `protoKindToValueType`) |
| `…/functions/proto_value.go` | remove whole-float→int64 coercion |
| `…/executor/executor.go` | `goToProtoValue` int64→float/double widenings + 22000 fallthrough |
| `…/functions/proto_value_test.go` | flip BOTH whole-float tests |
| `…/cascades/values/values_test.go` | AVG Type() pins |
| `…/sqldriver/*_fdb_test.go` | AVG/SUM INSERT…SELECT e2e + empty-source + index-presence |
| `…/conformance/yamsql/testdata/insert_select.yaml` | correct AVG/SUM→BIGINT expectations |
| `conformance/yamsql_cross_engine_conformance_test.go` | refresh stale exclusion comment |
