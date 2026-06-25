# RFC-143 — CARDINALITY() scalar function + cardinality index support (Java 4.12, RFC-135 §4 R6)

**Status:** Phase 1 (scalar function) + Phase 2 (index support) implemented. Open follow-ups (separate
items): the §3a nullable-array-wrapper WRITE side (Go writes plain repeated, so an empty array reads as
NULL — `CARDINALITY([])` is NULL not 0); nested-struct array indexes (blocked on STRUCT column support in
the metadata builder). Both tracked in TODO.md R6.
**Item:** RFC-135 §4 **R6** — port Java's `CARDINALITY(array) → INT` scalar function (Java #4074, 4.11.1.0)
and its index support (`CardinalityFunctionKeyExpression`, Java #4100, **4.12.3.0** — the strict 4.12
delta). `CARDINALITY(arr)` returns the array's element count; the index support lets an ordered index be
defined over `CARDINALITY(arr)` so `WHERE CARDINALITY(arr) = N` / `IS [NOT] NULL` / `ORDER BY
CARDINALITY(arr)` use index scans.
**Reviewers:** **Graefe** + Torvalds (Phase 2's planner index-matching is the Graefe-gated piece).

---

## 1. Problem (verified real)

Two axes, mirroring R5:

1. **The scalar function is orphaned.** Go has `CardinalityValue{Child Value}`
   (`values/value_cardinality.go`) — structurally well-formed, wired into the central tree-rewrite
   switches — but `NewCardinalityValue` has **zero non-test callers**. There is no `CARDINALITY` grammar
   token; `CARDINALITY(arr)` parses as a bare-`ID` `UserDefinedScalarFunctionCallContext` which the
   Cascades walk (`expr/walk.go`) has no arm for → `UnsupportedExpressionShapeError`. The Value is
   unreachable from SQL. Three Java divergences in the existing Go Value (vs `arrays-cardinality.yamsql`):
   (a) `Type()` returns `NotNullLong`, Java is `INT` (32-bit) and **nullable** (NULL array → NULL); (b)
   no array-type validation (Java's ctor enforces `isArray()` → `INCOMPATIBLE_TYPE`/`CANNOT_CONVERT_TYPE`);
   (c) `ExplainValue` has no case → renders bare `cardinality` not `cardinality(_.int_arr)`.
2. **Cardinality index support is absent** (the 4.12.3 delta). Java's `CardinalityFunctionKeyExpression`
   lets `CREATE INDEX … AS SELECT CARDINALITY("arr") AS c … ORDER BY c` build an ordered index over the
   array's repeated-field count, with a fast path using the protobuf repeated-field count. Go has the
   `FunctionKeyExpression` infrastructure + the `Function` proto (field 9) **already, wire-compatible**
   (round-trips byte-identical to Java) — but no `"cardinality"` function evaluator, no index-DDL path
   that produces a function key expression, and the planner flattens every index to a flat `[]string` of
   column names (`IndexColumnNames()`), so a function-keyed index can't structurally match a
   `CardinalityValue` predicate/sort.

**Read-side / not wire format** for the function (planner-computed). The **index entry IS wire format**
— but the `FunctionKeyExpression` proto is already Java's verbatim (field 9, name `"cardinality"`), so a
Go-written cardinality index serializes byte-identically; wire compat holds.

## 2. Investigation (Java mechanism)

- **`CardinalityValue`** (`values/CardinalityValue.java`): `extends AbstractValue`, one child (the array
  Value); `eval = childResult == null ? null : ((List)childResult).size()`; result
  `Type.primitiveType(INT)` (nullable); ctor asserts `childValue.getResultType().isArray()`. Registered
  as a built-in scalar `CardinalityFn` (`FunctionNames.CARDINALITY = "cardinality"`, `Type.any()` arg,
  arity 1).
- **`CardinalityFunctionKeyExpression`** (`metadata/expressions/`): a `FunctionKeyExpression`,
  `getMinArguments()==getMaxArguments()==1`, `createsDuplicates()==false`, `getColumnSize()==1`,
  `toValue() → CardinalityValue`. `evaluateMessage` has two fast paths — a direct repeated field
  (`field("arr", Concatenate)` → `getRepeatedFieldCount`) and a wrapped nullable array
  (`field("arr").nest(field("values", Concatenate))` via `NullableArrayTypeUtils.matchArrayWrapper`) —
  and falls back to materializing the arg list for deeper nesting. Returns
  `Key.Evaluated.scalar(list.size())`, or `Key.Evaluated.NULL` for a null array.
- **DDL → index:** `CREATE INDEX … AS SELECT CARDINALITY("arr") AS c … ORDER BY c`; the
  `MaterializedViewIndexGenerator` recognises a `CardinalityValue` select element and emits
  `function("cardinality", field("arr").nest(field("values", Concatenate)))`.
- **Planner:** a `WHERE CARDINALITY(arr) = N` / `ORDER BY CARDINALITY(arr)` matches the function-keyed
  index via the candidate's key expression (the index's match candidate carries the `CardinalityValue`,
  not a bare field name) → index scan / covering scan.

Go has: the orphaned `CardinalityValue`; the generic scalar-function dispatch (3 hand-maintained
keyword lists: `scalarFunctionResultType`, `IsCascadesSafeScalarFunction`, `evalScalarFunction` — none
list CARDINALITY); the full `FunctionKeyExpression` + named-function registry (`RegisterFunction`) +
wire-compatible `Function` proto; `FieldKeyExpression.evaluateRepeated` (`list.Len()`, the
repeated-count analog); the partial read-side nullable-array unwrap (`unwrapWrappedArray`,
`wrappedArrayFieldName="values"`). Go does NOT have: a SQL→`CardinalityValue` path; a `"cardinality"`
key-expression evaluator; an index-DDL path producing a `FunctionKeyExpression`; planner matching of a
function-keyed index (the index model is flat `[]string` column names).

## 3. Fix — phased (function first, then index)

### Phase 1 — the scalar function (no index; full-scan/in-memory)
1. **Wire SQL `CARDINALITY(expr)` → `CardinalityValue`.** Add the function to the dispatch building the
   **dedicated** `CardinalityValue` (not the generic `ScalarFunctionValue` — it needs distinct typing +
   array validation). **Decision (Torvalds): option (b) — a by-name built-in table at the
   `walkScalarFunction`/`walkFunctionCall` leaf, committed to as the SINGLE dispatch gate, not a fourth
   hand-maintained list.** The 3 existing keyword lists (`scalarFunctionResultType` (53),
   `IsCascadesSafeScalarFunction` (49), `evalScalarFunction` (56)) ALREADY drift bidirectionally —
   `NULLIF/IF/IIF` are in two but not `IsCascadesSafeScalarFunction`; `BITAND/BITOR/BITXOR` are in the
   reverse and are consequently **unreachable through the walker today** (a standing pre-existing bug).
   The by-name table collapses the gate; CARDINALITY (and ideally the drifted set, opportunistically)
   routes through it. **CARDINALITY must also be recognised by the satellite gates** the RFC's first
   draft missed — `polymorphicResultType` / `isAllowedFunction` (`cascades_generator.go:3850`) — or it'll
   pass the walker and fail later. (The `BITAND/OR/XOR`-unreachable standing bug is filed as a follow-up
   TODO, not fixed here unless the by-name collapse fixes it for free.)
2. **Fix the 3 divergences:** `Type()` → nullable `INT` (Java `Type.primitiveType(INT)`, nullable);
   array-type validation at construction (non-array arg → `CANNOT_CONVERT_TYPE`/the Go analog of
   `INCOMPATIBLE_TYPE`); add the `ExplainValue` case (`cardinality(<child>)`).
3. **Eval:** already correct (`childResult == nil ? nil : len(slice)`); confirm empty → 0, NULL → NULL,
   nested-struct array, and the `[]any` representation.
   Delivers: `SELECT CARDINALITY(arr)`, `WHERE CARDINALITY(arr) = N` (full-scan PredicatesFilter),
   `ORDER BY CARDINALITY(arr)` (InMemorySort), `IS [NOT] NULL`.

### Phase 2 — index support (the 4.12.3 delta; Graefe-gated planner matching)
4. **Register the `"cardinality"` key-expression evaluator** (`RegisterFunction("cardinality", …)`):
   count the materialized arg list (`len`), with the repeated-field fast path
   (`m.Get(fd).List().Len()`) and the nullable-array-wrapper descent (reuse `unwrapWrappedArray`); null
   array → null key. `getColumnSize()==1`, no duplicates.
5. **Index DDL → function key expression.** Extend the `AS SELECT CARDINALITY("arr") …` index DDL path
   (`parseAggregateIndexDefinition` / `indexSpec` / `buildIndexKeyExpression`) to recognise a
   `CARDINALITY` select element and produce a `FunctionKeyExpression{name:"cardinality", arguments: the
   field key expr}` (Java's `function("cardinality", field("arr").nest(field("values", Concatenate)))`).
   The `indexSpec` must carry a function/expression, not only `columns []string`.
6. **Planner index-matching (Graefe — ACK'd, with the two corrections below).** The index match-candidate
   model exposes only `IndexColumnNames() []string` (`cascades_generator.go:1756` throws the
   `KeyExpression` away via `FieldNames()` BEFORE cascades sees it), and `ExpandValueIndex` builds a
   `FieldValue` per column name — so a function-keyed index can only match a `FieldValue`, never a
   `CardinalityValue`. The fix (faithful to Java: `CardinalityFunctionKeyExpression.toValue()` produces
   the SAME `CardinalityValue(FieldValue(arr))` on the candidate side via
   `KeyExpressionExpansionVisitor.visitExpression(FunctionKeyExpression)` AND on the query side via the
   `cardinality` built-in — predicate/sort matching then falls out of standard value matching):
   - **6a — `KeyExpression → values.Value` bridge (the load-bearing piece; Graefe).** Go has NO
     `toValue`/`resolveAndEncapsulateFunction` bridge from a `KeyExpression` to a `values.Value`. Build it
     for the `"cardinality"` `FunctionKeyExpression` → `CardinalityValue(FieldValue(arr))`, so BOTH sides
     produce the identical Value. Propagate the index's `KeyExpression` (or the derived Value) into
     `ValueIndexScanMatchCandidate` (not just the flat `[]string`); `ExpandValueIndex` emits
     `CardinalityValue(FieldValue(arr))` for that column. (Vector's `DistanceRowNumberValue` placeholder
     is the in-repo proof the placeholder slot is already Value-generic.)
   - **6b — predicate matching: FALLS OUT.** `WHERE CARDINALITY(arr) = N` (equality range) / `IS [NOT]
     NULL` (null range) bind via `valuesMatchColumn → ValuesStructurallyEqual`, which already has a
     `*CardinalityValue` arm. No bespoke handling once 6a lands.
   - **6c — ORDER BY rule rework (Graefe CORRECTION — does NOT fall out for free).**
     `rule_ordered_index_scan.go` hard-asserts `sk.Value.(*values.FieldValue)` and string-compares
     `.Field` against the flat `colNames[i]`; a `CardinalityValue` sort key fails both. Rework the
     ordered-index-scan rule to match the sort key by **Value-tree equality against the candidate's key
     Value** (mirroring the predicate path), not by FieldValue-name strings — so `ORDER BY
     CARDINALITY(arr)` (incl. REVERSE) binds to the index order. Do NOT ship Phase 2 assuming REVERSE is
     free.
   Covering scans fall out when the projection is index-resident.

### 3a. Cross-engine nullable-array wrapper (small)
Java writes a nullable array as a `message{ repeated R values=1; }` wrapper; Go writes a plain repeated
field. The eval-time + index fast paths must descend the wrapper for **Java-written** nullable arrays
(reuse the read-side `unwrapWrappedArray` shape check). Go's own writes stay plain. (Go never *writing*
the wrapper is a separate latent divergence, out of scope here.)

## 4. Performance

The function is O(array length) (a `len`/`List().Len()`). The index fast path avoids materialising the
array (proto repeated-field count). No new cost-model surface beyond the existing value-index matching;
a cardinality-keyed index is a normal value index whose key happens to be a function — costed identically.

## 5. Test plan

Port `arrays-cardinality.yamsql` as FDB integration tests:
- **Phase 1:** `CARDINALITY([])`→0, `CARDINALITY([1])`→1, `CARDINALITY([1,2])`→2; NOT-NULL vs nullable
  array column (NULL→NULL); nested-struct array; non-array arg (`CARDINALITY(id)`, `CARDINALITY(1)`) →
  `CANNOT_CONVERT_TYPE`; `WHERE CARDINALITY(arr) IS [NOT] NULL`; `WHERE CARDINALITY(arr) = N`
  (full-scan); `ORDER BY CARDINALITY(arr)` (in-memory); result metadata type = INTEGER; EXPLAIN renders
  `cardinality(_.arr)`.
- **Phase 2 (EXPLAIN-asserted index use):** an `AS SELECT CARDINALITY(arr) … ORDER BY` index →
  `WHERE CARDINALITY(arr) = N` uses a COVERING/index scan (`plan_contains` the index), `IS NULL` /
  `IS NOT NULL` null-range scans, `ORDER BY CARDINALITY(arr) DESC` uses the index REVERSE; covering scan
  for index-resident projections; the function-keyed index round-trips through the catalog proto
  (wire-compat pin). Each Phase-2 test asserts the OPTIMIZATION fires (EXPLAIN), not just correct rows.
- Determinism 10×; the broader suite (incl. R5 unnest, EXISTS) stays green.

RFC-143 implements RFC-135 §4 R6. Graefe (Phase 2 planner matching) + Torvalds gate. NOTE: the final
codex pass is deferred to the Jun 25 quota reset (codex exhausted on R5); Graefe + Torvalds gate the
build in the interim.
