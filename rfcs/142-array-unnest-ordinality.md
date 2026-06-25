# RFC-142 — Correlated array UNNEST in FROM + ordinality (Java 4.12, RFC-135 §4 R5)

**Status:** Implemented
**Item:** RFC-135 §4 **R5** — bind the `(AT atAlias=uid)?` clause R3 (RFC-140) parsed-but-rejects: lateral
array unnest in the FROM list (`FROM t, t.arr AS x`) and its 4.12 ordinality companion
(`FROM t, t.arr AS x AT ord`, Java #4112 / `PRecordQueryExplodePlan.with_ordinality`).
**Reviewers:** **Graefe** + Torvalds (Cascades lowering of a correlated Explode under a FlatMap).

---

## 1. Problem (verified real)

Two gaps, one feature:

1. **Base lateral unnest is absent.** Go does not support `FROM t, t.arr AS x` — a comma-separated FROM
   source that is a *correlated array field* of a prior source (SQL-standard lateral `UNNEST`). The FROM
   parser (`select_parser.go` `parseFromSource`) treats every comma source as a **table cross-join**:
   `FROM T1, T1.arr1 AS val` builds `joinClause{tableName: "T1.arr1"}` and resolves `T1.arr1` as a table
   name → catalog miss / wrong plan. There is no `generateCorrelatedFieldAccess` path; Go's
   `ExplodeExpression` exists only for `INSERT … VALUES` (`cascades_translator.go:2685`) and `IN (list)`
   (`rule_in_to_explode.go`), never for a FROM-clause correlated array.
2. **Ordinality (the 4.12 delta) is parsed-but-rejected.** R3 added `(AT atAlias=uid)?` to
   `atomTableItem` and rejects it at all 6 consumers (`rejectAtOrdinality` in `select_parser.go` ×4,
   `scope_build.go`, `ddl.go`). `AT ord` binds a 1-based ordinal column to each unnested element.

Ordinality is meaningless without base unnest, so R5 delivers both. **Java-parity port** (Java supports
both; base unnest predates 4.12, ordinality is the #4112 addition). **Read-side / not wire-format** — the
Explode plan is planner-computed, never persisted; the proto `PRecordQueryExplodePlan.with_ordinality` is
already synced (`gen/record_query_plan.pb.go`).

## 2. Investigation (Java mechanism)

- **`QueryVisitor.visitAtomTableItem`** extracts `atAlias` and calls `LogicalOperator.generateAccess`.
- **`LogicalOperator.generateCorrelatedFieldAccess`** (the heart): when the FROM identifier resolves to a
  *correlated field* in scope (not a table), it asserts the field is `ARRAY`-typed
  (`INVALID_COLUMN_REFERENCE` otherwise), builds `new ExplodeExpression(fieldValue, withOrdinality)` where
  `withOrdinality = atAlias.isPresent()`, wraps it in a `forEach` quantifier, and registers the output
  expressions:
  - **no AT:** the AS alias → the bare exploded element (`QuantifiedObjectValue` of the explode quantifier).
  - **with AT:** the explode yields a 2-field struct; the AS alias → `FieldValue.ofOrdinalNumber(qov, 0)`
    (element), the AT alias → `FieldValue.ofOrdinalNumber(qov, 1)` (the ordinal, type `INTEGER`).
- **`ExplodeExpression.explodeResultType(elementType, withOrdinality)`**: with ordinality, a
  `Type.Record` of two **anonymous** fields `(elementType, INT NOT NULL)`; without, the bare element type.
- **`RecordQueryExplodePlan.executePlan`**: with ordinality, `IntStream.rangeClosed(1, list.size())`
  builds a `DynamicMessage{field0 = element, field1 = i}` per element — **1-based `INT`**, the ordinal an
  intrinsic of array position (resets per outer row; filters on elements/ordinals are separate FILTER
  nodes above and do not renumber).
- **Validation:** `AT` on a table / CTE / view / function → `WRONG_OBJECT_TYPE`; the AT column must be a
  correlated array. Lowers to `FlatMap(outer, Explode(correlated field))` — Java's `RETURN (q0.id, q1._0,
  q1._1)` plan shape.

Go has every piece *except* the FROM-clause wiring: `RecordQueryNestedLoopJoinPlan` / the FlatMap cursor
with `WithBinding(outerAlias, outerRow)` correlation (R4), `ExplodeExpression` / `RecordQueryExplodePlan` /
`ImplementExplodeRule`, anonymous-field `RecordType` (`type.go`), and `FieldValue` (name-based today).

## 3. Fix

### 3a. Semantic detection (NOT string heuristics)

A comma/JOIN FROM source whose dotted name `a.f` resolves — **against the semantic scope** — to an
in-scope source alias `a` carrying an **array** field `f` is a lateral unnest; otherwise it is a table.
This resolution happens where the scope is available (the cascades translator / `SemanticAnalyzer`), not by
string-splitting in the raw parser. The parser stops rejecting AT and instead carries the dotted ref + the
optional AS/AT aliases through; the translator classifies.

**Structural requirement (no string-split hack — Torvalds).** The parser today collapses the source name
via `strings.Join(FullId().AllUid(), ".")` (`select_parser.go:1588`), handing the translator one opaque
string. R5 must preserve the **uid segments** (the `[]uid` of `FullId().AllUid()`) on the carried FROM
source so the translator resolves segment-by-segment against the scope (`a` = first segment is an in-scope
alias, `f` = remaining segment(s) a field on it). The translator MUST NOT `strings.Split(name, ".")` a
re-joined string back apart — that is the forbidden text-heuristic. The classification is purely: does
segment 0 name an in-scope source whose record type has an array field named by the remaining segment(s)?

### 3b. Lowering (base unnest)

For a detected lateral source `outer.f AS x`: build the correlated array `Value`
(`FieldValue{f}` over the outer quantifier), `NewExplodeExpression(arrayValue)`, wrap in a `forEach`
quantifier correlated to the outer, and assemble `FlatMap(outer, explodeQuantifier)` with a result value
projecting the outer columns + the element bound to `x` — exactly R4's FlatMap machinery, inner = Explode
instead of a Scan. The AS alias binds to the explode quantifier's flowed object value.

### 3c. Ordinality

- `ExplodeExpression` / `RecordQueryExplodePlan` gain a `WithOrdinality bool`; `ImplementExplodeRule`
  threads it; `GetResultType`/`GetResultValue` return the anonymous 2-field `Record(element, INT NOT NULL)`
  when set, else the bare element.
- `executeExplode` (executor.go): when `WithOrdinality`, yield `QueryResult{Datum: struct{_0: element, _1:
  i+1}}` with a **1-based** counter per array (resets each outer row, since the cursor re-runs per outer
  binding).
- **`FieldValue` ordinal access:** add an ordinal-indexed `FieldValue` (access record field by position,
  mirroring Java `FieldValue.ofOrdinalNumber`) so AS → field 0, AT → field 1. The element binding uses
  ordinal 0, the AT binding ordinal 1 (type INT NOT NULL).
- The 6 `rejectAtOrdinality` sites: AT on a **correlated array** routes to 3b/3c. AT on a genuine
  **table/CTE/view/function** is rejected with **one converged code: `ErrCodeWrongObjectType` (42809)**,
  the Java `WRONG_OBJECT_TYPE` equivalent (Torvalds: today the parser throws `ErrCodeUnsupportedQuery`
  while `scope_build.go:45` throws `UnsupportedFromShapeError` — two errors for the same shape, so a
  rejection test isn't revert-proof; R5 converges both to `ErrCodeWrongObjectType` at the single
  scope-based classification point). The DDL aggregate-index site keeps rejecting (no ordinality in index
  defs) — also `ErrCodeWrongObjectType`.
- **Explode equals/hash (Graefe):** `ExplodeExpression.EqualsWithoutChildren` /
  `HashCodeWithoutChildren` (and the physical plan's) must incorporate `WithOrdinality` — Java hashes
  `(collectionValue, withOrdinality)`; the current Go pointer-equality on `collectionValue` alone would
  conflate an ordinal and a non-ordinal Explode over the same array. Fold the flag in when adding it.

### 3d. Validation

A FROM dotted source that resolves to a non-array correlated field, or AT on a non-array, errors cleanly
with `ErrCodeWrongObjectType` (42809) — the converged code above, matching Java's
`INVALID_COLUMN_REFERENCE` / `WRONG_OBJECT_TYPE` intent. Unresolvable dotted name → the existing
table-not-found path (unchanged).

## 4. Performance

No new cost-model surface: the lateral unnest is a `FlatMap(outer, Explode)` — the same physical shape R4
already costs; Explode is a leaf over an in-memory array (no I/O). Ordinality adds a single integer per
element in the cursor. Non-unnest FROM lists are untouched (the classifier only fires when a dotted source
resolves to an in-scope array field).

## 5. Test plan

Port Java's `array-join-at.yamsql` expectations as FDB integration tests (real array columns):
- **base unnest** `SELECT id, val FROM t, t.arr AS val` — one row per element, correct cross-product.
- **with ordinality** `… AS val AT at` — `(id, val, at)` with **1-based** `at`, resetting per outer row
  (`(1,101,1),(2,201,1),(2,202,2),(2,203,3)`); EXPLAIN shows `EXPLODE … WITH ORDINALITY`.
- **AT only** (no AS) `… arr AT at`.
- **filter on element preserves ordinal** `… WHERE val > 201` → `(2,202,2),(2,203,3)` (ordinal = original
  position, not filtered rank).
- **filter on ordinal** `… WHERE at + 1 = 3` → `(2,202,2)`.
- **multiple independent unnests** `FROM t, t.arr1 AS v1 AT a1, t.arr2 AS v2 AT a2` — **cleanly REJECTED
  in R5** (`UNSUPPORTED_QUERY`, never silent-wrong rows; pinned by a guard test). Chaining the second
  unnest's outer through the first unnest's merged-row columns (`NewAnchoredJoinRecord` over
  `LogicalUnnest` legs whose types are `UnknownType`) is a genuine Cascades extension, not a one-liner —
  Graefe judged the clean rejection an acceptable R5 boundary and the chained-FlatMap lowering the
  immediate follow-up (see §6 / TODO R5).
- **empty / single-element arrays**, **NULL handling**, **ordinal type is INT**.
- **rejections:** AT on a real table / CTE / derived table / non-array field → clean
  `ErrCodeWrongObjectType` (42809), revert-proof on the one code+message.
- **name-collision (Torvalds):** the unnest AS/AT aliases must NOT collide with a real same-named column
  of the outer source — `SELECT id, val, at FROM t, t.arr AS val AT at` where `t` itself has a column
  named `val` or `at` resolves to the unnest binding (the codex motivation behind R3's original guards);
  pin that the right value flows, not the outer column.
- EXISTS/R4 + the broader suite stay green; EXPLODE plan-shape pin; 10× determinism.

RFC-142 implements RFC-135 §4 R5. Graefe + Torvalds gate.

## 6. As built

**Parser (`select_parser.go`).** `joinClause` gained `segments []string` (the un-flattened
`FullId().AllUid()`, quote-stripped via the new `uidSegments` helper) and `atAlias string` (the `AT`
alias via `atAliasOf`). Comma sources carry both through; `rejectAtOrdinality` no longer fires on comma
sources (the translator classifies) but still rejects AT on the PRIMARY source and JOIN sources, now
converged on `ErrCodeWrongObjectType`. `lateralUnnestCandidate` emits a `logical.LogicalUnnest{Segments,
Alias, AtAlias}` (a new leaf operator) as the comma source's right child whenever it is a dotted (≥2
segment) source or carries an `AT` alias; the translator does the real classification.

**Translator classification + lowering (`cascades_translator.go`).** `translateJoin` detects a
`LogicalUnnest` right child and calls `translateUnnestJoin`, which: resolves segment 0 to a scan source
in `j.Left` (`findOuterScanTable`), asserts the remaining segment names a repeated proto field
(`unnestArrayElementType`, deriving the element type from the field's scalar kind), builds the correlated
array `FieldValue{arr}` over `QOV(outerAlias)`, wraps `NewExplodeExpressionWithOrdinality(arr,
atPresent)` in a named forEach quantifier, and assembles a `SelectExpression` with `[outerQ,
explodeQ]`, no predicates, source aliases `[outer, asAlias]`. The result value
(`buildUnnestResultValue`) anchors the outer leg's columns (`NewAnchoredJoinRecord`) plus the element
(`QOV(inner)` bare, or `ofOrdinalNumber(_0)` under ordinality) and the ordinal (`ofOrdinalNumber(_1)`),
with the unnest's AS/AT bare keys SHADOWING any same-named outer column. AT on a non-array → `setTranslateErr(ErrCodeWrongObjectType)` (surfaced by `PlanRecordQueryWithMetadata` /
`cascades_generator.go` via the new `TranslateToCascadesWithError`).

**FlatMap reuse, not a parallel rule.** `ImplementNestedLoopJoinRule`'s Explode guard now only bails when
the Explode leg is NOT correlated to the other leg (the uncorrelated IN-list shape, owned by
`ImplementInJoinRule`); a correlated lateral-unnest Explode falls through to the existing
`yieldGeneralFlatMap` path → `NewRecordQueryFlatMapPlan(outer, explode, …, resultValue, false)` (the
non-existential, no-FirstOrDefault path Graefe confirmed). The FlatMap cursor binds the raw inner Datum
(scalar element OR 2-field ordinality record), not a forced map cast.

**Ordinality threading (Stage 2).** `ExplodeExpression` + `RecordQueryExplodePlan` carry `withOrdinality`,
folded into `EqualsWithoutChildren`/`HashCodeWithoutChildren` (Graefe: ordinal vs non-ordinal over the
same array are distinct), `GetResultType` (→ `values.ExplodeOrdinalityResultType`, an anonymous 2-field
`Record(element, INT NOT NULL)` keyed `_0`/`_1` via `values.OrdinalFieldName`), and `Explain`.
`ImplementExplodeRule` threads the flag. `executeExplode` yields `{_0:element, _1:i+1}` per element,
1-based, resetting per outer binding (the cursor re-runs the Explode per outer row). The ordinal-indexed
`FieldValue` is `values.NewOrdinalFieldValue` (Java `ofOrdinalNumber`) — name access on the `_<n>` key.

**WHERE on the unnest column.** A virtual `Shadowing` `ScopeSource` (`unnestScopeSourceAdder`, added in
`buildSelectScope` + `buildWherePredicateForJoins`) exposes the AS/AT columns so a WHERE / projection /
ORDER BY reference resolves (and validation passes); the new `Shadowing` flag on `ScopeSource` makes the
unnest binding win over a same-named real column instead of erroring ambiguous. `rewriteUnnestPredicate`
(called unconditionally for any unnest WHERE, via `mapPredicateValues`) rebases the AS/AT references to what
the inner Explode actually flows so the NLJ rule pushes them into the inner Explode's `PredicatesFilter`:
- **WITH ORDINALITY** — the inner flows a 2-field record; AS→`_0`, AT→`_1` (`FieldValue.ofOrdinalNumber`),
  Java's `EXPLODE … WITH ORDINALITY | FILTER _._0 …` shape; the element's ordinal stays its original array
  position, not the filtered rank.
- **NON-ORDINAL** — the inner flows a BARE SCALAR; the AS reference collapses to the whole
  `QuantifiedObjectValue(unnestCorr)` (Java's `generateCorrelatedFieldAccess` primitive branch binds the
  alias to `getFlowedObjectValue()`, NOT a FieldValue). A FieldValue over the scalar reads a named subfield
  of a scalar and evaluates NULL, filtering every element out — the codex-found P1a silent-wrong bug. The
  executor's `executePredicatesFilter` complements this by binding a bare-scalar inner row under
  `innerAlias` (mirroring Java's quantifier binding a primitive flowed value) so the QOV resolves.

**Aliasless unnest (`FROM t, t.arr`, neither AS nor AT).** `unnestAliases` — the single source of truth for
the (AS, AT) pair — defaults the element binding alias to the LAST segment (the array field name `ARR`) when
no `AS` was written, mirroring Java's `visitAtomTableItem` defaulting `tableAlias` to `visitTableName(...)`.
The element is then referenceable by the field name and `unnestSourceCorrelation` never yields the zero
`CorrelationIdentifier` that previously panicked in `NewQuantifiedObjectValue` (the codex-found P1b panic).

**Classifier precision (codex round-4).** The lateral-unnest classifier was too LOOSE
(`len(segments)≥2 || AT` → unnest), so four shapes mis-classified. The fix makes the classifier match
Java's `LogicalOperator.generateAccess` resolution order — CTE/table/view/function FIRST, else
`resolveCorrelatedIdentifier` (an in-scope correlated field). A comma source is a lateral unnest IFF a
plain comma source (no ON/derived) AND `unnestCandidateShape` holds: a DOTTED source (≥2 segments) is an
unnest only when segment 0 names a **visible in-scope FROM-source alias** (`visibleFromAliases`: the
primary alias + each prior leg's effective alias — a derived/CTE leg contributes only its OUTER alias,
never the hidden body tables); a source carrying an `AT` alias is always a candidate so the translator can
reject a non-array AT cleanly. `unnestCandidateShape` is the SINGLE predicate shared by the logical
lowering (`lateralUnnestCandidate`) and the WHERE/projection scope binding (`isLateralUnnestJoin`), so they
never diverge.
- **P1 (silent-wrong, alias == outer correlation):** `FROM T1 AS X, X.arr AS X` (and the aliasless
  field-name-collides-outer variant) made `innerCorr == outerCorr`; `translateUnnestJoin` now rejects when
  the unnest's element/ordinal alias collides with the outer FlatMap correlation OR any already-bound outer
  source alias (`outerBoundAliases`, which — like `findOuterScanTable` — does not descend into CTE bodies)
  → `ErrCodeDuplicateAlias`, never `innerCorr == outerCorr`.
- **P2a (schema-qualified comma join):** `FROM A, s.B AS B` — segment 0 (`s`) is the session schema, not a
  visible source → NOT an unnest; stays the table path, `resolveQualifiedTableNames` strips `s.` and B
  plans as a normal cross join (`NestedLoopJoin`, no Explode/FlatMap).
- **P2b (CTE/derived-hidden scan):** `FROM (SELECT … FROM T1) AS d, T1.arr AS x` — only `d` is visible;
  `T1` is hidden in the derived body, so segment 0 is not visible → NOT an unnest (a clean
  `UndefinedDatabase`, not a correlated unnest against the hidden scan). Defence-in-depth:
  `findOuterScanTable`/`outerBoundAliases` resolve a `LogicalCTE` against its `Main` only, never `Body`.
- **P2c (scalar / missing correlated field):** `FROM T1, T1.val AS x` where `val` is a present SCALAR →
  `ErrCodeWrongObjectType` (Java's "repeated type" assert); a genuinely-MISSING field on a known source →
  a clean `ErrCodeUndefinedColumn` (Java's `resolveCorrelatedIdentifier` field-lookup failure), distinct
  from the present-scalar case and never a silent table fallback.

**Table-first resolution order (codex round-5).** Round-4 classified a DOTTED comma source as an unnest
whenever segment 0 named a visible alias — but did NOT run Java's `generateAccess` table-first step against
metadata (the parser-side classifier has none). Two shapes still mis-classified:
- **R5b (valid-query-fails): schema-qualified table whose qualifier ALSO names a prior alias.**
  `FROM PA AS s, s.PB AS B` — the prior source `PA` is aliased `s`, which equals the session schema name, so
  `s.PB` is BOTH "field PB on source s" AND "schema-qualified table PB". Java resolves the TABLE first
  (`tableExists`: qualifier == schema name AND `PB` exists), so it is a normal cross join, never a correlated
  unnest. The fix threads the session `schemaName` into the classifier and a `tableResolver`
  (`newUnnestTableResolver` = Java's `tableExists`): `unnestCandidateShape` now returns false when a visible-
  alias-led dotted name ALSO resolves to a real schema-qualified table (the table branch wins). A backstop
  `demoteSchemaQualifiedUnnest` pass (run with `md` + `schemaName` before validation/translation) rewrites any
  surviving schema-qualified-table `LogicalUnnest` to a `LogicalScan`. AT on such a table → `WRONG_OBJECT_TYPE`
  (Java's table-branch `atAlias.isEmpty()` assert), surfaced by the demotion pass; the AT keeps the source a
  `LogicalUnnest` (so the AT is not silently dropped) while the scope binding (`isLateralUnnestJoin` +
  `schemaQualifiedTableUnnest`) declines to register a virtual unnest source for it. Now plans IDENTICALLY to
  the un-aliased `FROM PA, s.PB AS B` (`NestedLoopJoin`, no Explode/FlatMap).
- **R5a (silent-wrong): CTE/derived alias validated against base-table metadata.**
  `WITH T1 AS (SELECT ID AS ARR FROM T1) SELECT V FROM T1, T1.ARR AS V` — the CTE alias `T1` shadows the real
  record type `T1`; the CTE OUTPUT column `ARR` is the SCALAR `ID` renamed, but the base table `T1` has an
  ARRAY column `ARR`. Round-4 resolved segment 0 (`T1`) via `findOuterScanTable` → the base table `T1`, then
  validated `ARR` against the base-table descriptor and silently exploded the WRONG column. Java validates the
  field against the in-scope source's OUTPUT type (the CTE's projected/flowed columns), where `ARR` is a
  scalar → not unnestable. The translator's derived/CTE leg-column TYPES are best-effort `UnknownType`
  (`legColumns`), so the CTE output element type is not recoverable at the lowering point; rather than validate
  against the wrong base-table metadata, `translateUnnestJoin` now detects a CTE/derived outer source
  (`outerSourceIsCTE`, against `cteScope`/`cteExprScope`/`cteColumnsScope`) and cleanly rejects with
  `UNSUPPORTED_QUERY` ("unnest over a CTE/derived-table output is not yet supported") — never the silent base-
  table explode. Normal (non-unnest) CTE/derived queries and real-table unnests are unaffected.

**Scope-aware multiple-unnest guard + nil-md + AT-on-bare-source (codex round-6).** Three edge cases:
- **P2a (valid-query-fails): the multiple-unnest guard descended into hidden derived/CTE bodies.**
  `FROM (SELECT v FROM T1, T1.arr AS v) AS d, T2, T2.arr AS x` is TWO FROM scopes — the inner unnest lives
  in the derived table `d`'s OWN body; the outer scope has ONE unnest (`T2.arr`). The recursive
  `containsLateralUnnest(j.Left)` walk counted `d`'s hidden inner unnest and wrongly rejected the outer
  query as multiple-unnest. The fix stops the walk at `LogicalCTE.Body` (inspect only `Main`, the visible
  alias projection) — mirroring `findOuterScanTable` / `outerBoundAliases`. The example now plans (outer
  single unnest over `T2.arr`; the derived table's own single unnest is its own scope).
- **P2b (panic): the metadata-less translation path panicked on `LogicalUnnest`.** `TranslateToCascades` /
  `TranslateToCascadesWithSubqueries(op, nil)` (scalar-subquery / DML translation, unit tests) reach
  `translateUnnestJoin`, which classifies via `resolveRecordType → t.md`. A nil-md guard at the function
  top now declines cleanly (`UNSUPPORTED_QUERY`, "requires record metadata to classify") instead of
  dereferencing nil — Java never reaches an unnest without a `SemanticAnalyzer`/metadata, and no production
  caller unnests without md.
- **P3 (wrong code): AT on a bare source alias returned `UNDEFINED_COLUMN`.** `FROM T1, T1 AT ord` — AT is
  present but there are NO field segments (the source is the bare table/alias `T1`). The known-source /
  missing-field branch reported `UNDEFINED_COLUMN` for an empty column name; since this is AT-on-a-table
  (not on an array field) it now converges with the other AT-on-table rejections on `WRONG_OBJECT_TYPE`
  (42809).

**Derived-alias-shadows-real-table + later-same-named-column (codex round-7).** Two recurring merged-row /
metadata-source silent-wrong classes:
- **P1 (silent-wrong): a derived-table alias colliding with a REAL same-named table validated against the
  wrong metadata.** `FROM (SELECT ID AS ARR FROM T1) AS D, D.ARR AS V` where a REAL table `D` ALSO exists with
  an ARRAY column `ARR`. The round-5 CTE/derived-output rejection (`outerSourceIsCTE`, via the `cteScope` maps)
  MISSED this: the derived table's `LogicalCTE` body is registered into `cteScope` only when its leg is
  *translated* (`translateCTE`) — AFTER the metadata-validation guard in `translateUnnestJoin` — so the scope
  check was still false, and `findOuterScanTable` resolved segment 0 (`D`) to the REAL table `D`, validated
  `ARR` against ITS array metadata, and exploded the derived row's SCALAR `ARR` (one wrong scalar row per outer
  row). The fix detects the derived/CTE source STRUCTURALLY (`outerSourceIsDerivedTable`: a `LogicalCTE` leg in
  `j.Left` whose `Name` == segment 0, OR'd with `outerSourceIsCTE`) — reading the logical tree directly, so it
  fires INDEPENDENT of `cteScope` population order and regardless of whether the alias also names a real table.
  The in-scope derived source is preferred over the catalog table, exactly as Java's
  `generateCorrelatedFieldAccess` resolves the in-scope quantifier alias, never the catalog table. The
  derived-output unnest is now cleanly rejected (`UNSUPPORTED_QUERY`) in ALL cases. A genuine real-table unnest
  of the same-named `D` (no `LogicalCTE` leg) is unaffected (control test).
- **P2 (silent-wrong): the unnest element binding overwritten by a LATER FROM item's same-named column.**
  `FROM t, t.arr AS v, u` where `u` also has a column `v`. The unnest's `Shadowing` scope source makes a bare
  `v` resolve to the element, but the unnest is NOT the rightmost FROM leg — `u` is — so the outer
  NestedLoopJoin's `mergeRows` OVERWRITES the bare `v` key last-leg-wins with `u.v`, and `SELECT v` returned
  `u.v`. The element flows the merged row under BOTH bare `v` AND qualified `v.v`, and `mergeRows` preserves
  dotted keys verbatim — so the qualified `v.v` survives. The root cause: the plan-visitor's BARE-column
  projection path emitted an unqualified `FieldValue(v)` even when the column bound to the `Shadowing` unnest
  source, discarding the source correlation. The fix qualifies a bare column that binds to a `Shadowing` source
  to its correlation (`ResolveColumnShadowingQualified` → `FieldValue(QOV(v), v)`, mirroring `ResolveIdentifier`'s
  multi-source qualification and Java's `generateCorrelatedFieldAccess` binding the alias to a quantifier),
  reading the protected `v.v` key. An EXPLICITLY-qualified `u.v` is unambiguous and unaffected (control test) —
  the qualification only redirects the bare reference the unnest binding owns. The output column stays labeled
  `v` to the user (`projectionColumnName` returns the field name). WHERE-on-element in the same 3-item shape is
  likewise correct (the qualified `v.v` reference survives the merge).

**ORDER-BY-shadowing qualification + WHERE-EXISTS-over-unnest (codex round-8).** Two unnest ×
other-feature interactions, both silent/translation failures the round-7 fixes did not cover:
- **P2a (silent-wrong order): the shadowing qualification was not applied to ORDER BY sort keys.** The round-7
  P2 fix qualifies a bare SELECT projection (`v` → `v.v`) so a later same-named `u.v` cannot overwrite it via
  `mergeRows`, but the SORT key for `ORDER BY v` was still emitted BARE — `mergeRows` clobbered the bare `v`
  key with `u.v` so the sort tied on a constant and the rows came back in insertion order (a no-op sort,
  identical asc/desc — the desc test is the revert-proof). The fix qualifies a bare sort key that binds to the
  `Shadowing` unnest source to `v.v` (`qualifyShadowedSortKeys`, reusing the projection path's
  `ResolveColumnShadowingQualified` — the two cannot diverge), AND fixes the latent root cause in
  `ImplementInMemorySortRule`: a `FieldValue` carrying a `Child` (a correlated/qualified `LEG.COL` reference)
  was collapsed to its BARE `Field` name, dropping the leg correlation; it now routes through the executor's
  `ValueExpr` (per-row evaluation of the qualified key) — the bare-field fast path is kept only for a childless
  `FieldValue`. This was a pre-existing latent bug for any qualified in-memory sort key; the unnest shadow
  merely exposed it.
- **P2b (translation failure): a `LogicalUnnest` right child in the EXISTS-join path returned nil.** `SELECT v
  FROM t, t.arr AS v WHERE EXISTS (…)` routed join+EXISTS to `translateJoinWithExists`, which translated the
  join's right child via `translateRef` → nil for an unnest → a generic "Cascades translation failed". The
  unnest CANNOT be flattened into `implementJoinWithExistential`'s binary NLJ (a correlated Explode in a plain
  NLJ materializes its inner ONCE against an unbound context → zero rows). The fix routes the unnest+EXISTS to a
  NESTED shape (`translateUnnestExistsFilter`): the unnest stays its own `FlatMap(outer=Scan, inner=Explode)`
  (lowered by the shared `translateUnnestJoin` — no duplicated lowering) as the existential's OUTER, and the
  EXISTS wraps it via the shared `buildExistentialSelect`. A WHERE on the unnest ELEMENT/ORDINAL alongside the
  EXISTS is rewritten (`rewriteUnnestPredicate`) and folded INTO the inner Explode (the IDENTICAL merge the
  non-EXISTS unnest+WHERE path performs) while ONLY the existential markers (`extractExistsPredicates`) thread
  to the outer semi-join — so the element filter never leaks onto the outer scan (which would drop every row).
  NOT-EXISTS, WITH-ORDINALITY, and element/ordinal-filter-AND-EXISTS compositions are all pinned revert-proof.

**Unnest virtual source missing from three more scope-resolution paths (codex round-9).** The unnest's
virtual `Shadowing` source (`unnestVirtualScopeSource`, refactored into the single source of truth shared by
`unnestScopeSourceAdder` + `buildOuterScopeSources`) was registered into the main SELECT scope but NOT into
three other resolution paths, each a distinct silent-wrong / translation failure:
- **P2a (silent-wrong, no-op sort): a COMPUTED ORDER BY over an unnest column.** `ORDER BY v + 0 DESC` flows
  through `upgradeSortKeyValues`, which built its resolver via `buildProjectionResolverWithCTEScopes` — that
  resolver resolves the dotted unnest source (`t.arr`) as a TABLE, fails, and returns nil, so the sort key
  stayed RAW TEXT (`InMemorySort(["v" + 0 DESC])`); the executor compared a non-existent field → a no-op sort
  (rows in insertion order). The fix falls the resolver back to `buildSelectScope` (the single scope builder
  that knows the unnest virtual source) — the SAME fallback the projection/GROUP BY/HAVING `upgradeXxx` paths
  already use — so the computed expression resolves against the unnest binding and the qualified FieldValue
  (Child = `QOV(v)`) routes through `ImplementInMemorySortRule`'s `ValueExpr` per-row path (round-8), sorting
  for real. The round-8 P2a bare-key path (`qualifyShadowedSortKeys`) and this computed-key path are both
  covered; the desc test is the revert-proof (a no-op sort coincides with asc).
- **P2b (silent-wrong, overwrite): duplicate AS == AT alias.** `FROM t, t.arr AS X AT X` appends the element
  and the ordinal under the SAME bare+qualified names in `buildUnnestResultValue`;
  `RecordConstructorValue.Evaluate` stores fields in a map, so the ordinal (appended last) silently OVERWRITES
  the element — `SELECT X` returned the ordinal. `translateUnnestJoin` now rejects `u.Alias == u.AtAlias`
  cleanly (`ErrCodeDuplicateAlias`) BEFORE constructing the result, alongside the existing
  unnest-alias-vs-outer-alias rejection (Java binds AS and AT to two distinct quantifier columns).
- **P2c (translation failure): a correlated subquery referencing the unnest element.** `SELECT VAL FROM t,
  t.arr AS VAL WHERE EXISTS (SELECT 1 FROM U WHERE U.V = VAL)` — the inner EXISTS correlates to the unnest
  ELEMENT binding `VAL`. Two gaps: (1) `buildOuterScopeSources` built the subquery's `outerScopes` from REAL
  table sources only, so a correlated reference to `VAL` could not resolve — now it registers the SAME
  `unnestVirtualScopeSource` for a lateral-unnest leg (the systematic single-source-of-truth registration); and
  (2) a BARE outer-correlated column unresolvable in the subquery's OWN scope surfaced as a `ColumnNotFoundError`
  the subquery WHERE walk silently degraded to a TEXT predicate (which the translator then rejects). The walk
  now maps that bare `ColumnNotFoundError` to `ErrCodeUndefinedColumn` — mirroring the qualified-outer-ref path
  (`SourceNotFoundError` for `T1.ID`) — so `BuildExists` falls to `buildCorrelatedExists`, which resolves `VAL`
  against the enriched `outerScopes` and the existing EXISTS-over-unnest lowering binds it at execution. Only the
  SUBQUERY / derived-table inner-plan build reaches that walk (the main query plans via the PlanVisitor and
  validates columns separately), so the main-query text fallback is unaffected. EXISTS / NOT-EXISTS /
  WITH-ORDINALITY / outer-id-carrying variants are all pinned revert-proof.

**Real-table alias shadowing a CTE name (codex round-10).** The CTE/derived-output rejection over-fired:
`WITH X AS (…) SELECT V FROM T1 AS X, X.ARR AS V` has a CTE named `X` in the global WITH scope, but the FROM
uses `T1 AS X` — a REAL table aliased `X`, which SHADOWS the unused CTE. The rejection guard checked
`outerSourceIsCTE(segment-0 NAME)`, which returned true because a CTE named `X` existed in the global cteScope
maps → the valid query was wrongly rejected `UNSUPPORTED_QUERY`. The fix ties the rejection to the ACTUAL
source bound in `j.Left`: `findOuterScanTable` resolves segment 0 to the scan's TABLE name FIRST (a real table
`T1` for `T1 AS X`, the CTE name `X` for a CTE reference `FROM X`), and `outerSourceIsCTE` is now keyed on that
RESOLVED table (`outerSourceIsCTE(outerTable)`), not the segment-0 alias. A real table aliased with a CTE's
name therefore resolves to the visible scan and unnests; only a CTE GENUINELY used as the source (segment 0
resolves to the CTE name) — or the structural `outerSourceIsDerivedTable` derived-primary leg — still rejects
(the round-5 boundary). This is Java's in-scope-alias-shadows-CTE resolution: the visible quantifier alias wins
over a same-named catalog/CTE name.

**Schema-qualified table inside a subquery + multiple-unnest guard order (codex round-11).** Two bugs:
- **P2 (valid-query-fails): the table-first demotion did not reach SUBQUERY plans.** `SELECT ID FROM T1
  WHERE EXISTS (SELECT 1 FROM PA AS s, s.PB AS B)` (session schema `s`). Inside the EXISTS subquery, the
  prior source `PA` is aliased `s` (== schema name), so the parser (nil resolver) classified `s.PB` as a
  correlated unnest. The top-level `demoteSchemaQualifiedUnnest` walked only `op.Children()` — but the EXISTS
  subquery plan is carried on `LogicalFilter.ExistsSubqueries[].Plan` (and scalar plans on
  `LogicalProject`/`LogicalAggregate`), which are NOT children — so the surviving `LogicalUnnest`
  mis-translated (`column PB missing on source s` / `Cascades translation failed`). The fix: a shared
  `subqueryPlans(op)` helper enumerates every subquery-plan side field, and both `demoteSchemaQualifiedUnnest`
  AND `resolveQualifiedTableNames` now recurse into them — Java's `generateAccess` runs table-first at EVERY
  FROM-source point, including inside subqueries. The catalog sub-build (`buildLogicalPlanForSelectWithCTECatalog`)
  ALSO runs the demotion before its post-build projection-value resolution (the top-level demotion mutates the
  tree too late to recover the nested projection Values) AND strips the schema qualifier off the parser's
  schema-qualified FROM sources (`normalizeSchemaQualifiedSelectSources`: `s.PB`→`PB`) so the subquery SCOPE —
  which reads `sq.joins`, not the demoted tree, and whose `analyzer.ResolveTable` does not strip a schema
  qualifier — registers the table source instead of degrading the resolver to nil. The plain
  (no-alias-collision) `FROM PA, s.PB AS B`-in-a-subquery sibling is covered by the same passes (it is a plain
  `Scan("S.PB")`, stripped by the subquery-aware `resolveQualifiedTableNames` + the scope normalization). The
  top-level query builds its scope through the PlanVisitor and is untouched (the R5b top-level cross-join +
  AT-on-table tests stay green). Now plans IDENTICALLY to a normal cross join (`NestedLoopJoin(Scan PA, Scan PB)`
  inside the existential), no Explode.
- **P3 (wrong error code): the multiple-unnest guard ran BEFORE array validation.** `FROM T1, T1.arr AS V, U
  AT O` (AT on a non-array table) and `FROM T1, T1.arr AS V, T1.id AS X` (a scalar field) — both INVALID
  array sources following a prior real unnest — reported `UNSUPPORTED_QUERY` ("multiple unnests") because
  `containsLateralUnnest(j.Left)` fired before checking whether the new right side is a valid array source.
  The guard now runs only AFTER the `!isArray` validation confirms the right side IS a genuine array unnest,
  so an AT-on-non-array / scalar candidate gets the faithful `WRONG_OBJECT_TYPE` first; a genuine SECOND array
  unnest (`FROM T1, T1.arr1 AS V, T1.arr2 AS W`) still reaches the guard → `UNSUPPORTED_QUERY` (control).

**EXISTS-over-unnest correlating to the OUTER TABLE + non-default-schema subquery (codex round-12).** Two
bugs:
- **P2a (silent-wrong, drops rows): an EXISTS over a lateral unnest whose residual correlation references the
  ORIGINAL OUTER TABLE, not the unnest element.** `SELECT VAL FROM T1, T1.ARR AS VAL WHERE EXISTS (SELECT 1
  FROM U WHERE U.V > T1.ID)` — the existential's residual is `U.V > T1.ID`, correlated to T1 via `T1.ID`.
  `buildCorrelatedExists` resolved `T1.ID` against the outer scope's REAL table source T1, so the
  `ExistsSubquery.JoinPredicate` carried `FieldValue{ID, Child:QOV(T1)}`. But the existential's OUTER in the
  round-8 `translateUnnestExistsFilter` path is the UNNEST FlatMap, whose merged output row is bound under the
  unnest's AS/AT alias (`sourceAlias(join)` = `VAL`), NOT under `T1`. So at execution `QOV(T1)` was UNBOUND →
  `U.V > NULL` was false for every row → ALL rows silently dropped (an equality on the inner PK like
  `U.ID = T1.ID` escaped this because the fast-path `tryExistsFlatMap` rewrote the probe to the bare `ID` key
  off the FlatMap outer; only a NON-PK / NON-equality residual hit the slow-path residual filter where `QOV(T1)`
  is unbound). The unnest FlatMap output anchors the outer leg's columns under BOTH bare (`ID`) and qualified
  (`T1.ID`) keys (`buildUnnestResultValue` → `NewAnchoredJoinRecord`), exactly as a non-unnest `WHERE EXISTS`
  correlates to its FROM source. The fix (`rebaseUnnestOuterLegPredicate`, the query-package twin of the
  real-JOIN+EXISTS `rebaseOuterLegRefsToMerged`) rebases every EXISTS subquery's `JoinPredicate` so a reference
  to an outer-table leg alias (`outerBoundAliases(join.Left)`, e.g. `T1`) reads the qualified `T1.ID` key off
  the unnest FlatMap's merged binding (`QOV(unnestAlias)`). A residual referencing the unnest ELEMENT (`VAL`,
  the merged corr itself, the round-9 P2c path) is bound by the FlatMap already and is left untouched. EXISTS /
  NOT-EXISTS / WITH-ORDINALITY / both-correlations (outer-table AND element, `UV.V = VAL AND UV.V > T1.ID`) are
  pinned revert-proof.
- **P2b (misclassification in a non-default schema): the session schema was not threaded into the subquery
  planners.** The round-11 fix demoted a schema-qualified-table unnest INSIDE an EXISTS subquery, but the
  `existsSubqueryPlanner` (and the scalar-subquery planner) built the subquery plan through
  `buildLogicalPlanForQueryWithCTECatalog` → `buildLogicalPlanForSelectWithCTECatalog`, which fell back to the
  HARDCODED `defaultEmbeddedSchema` (`s`). So in a session whose schema is NOT `s` (e.g. `main`), a
  schema-qualified source INSIDE the subquery — `EXISTS (SELECT 1 FROM PA AS main, main.PB AS B)` with session
  schema `main` — was demoted/normalized against `s` instead of the active schema → mis-classified / left an
  unresolved text filter ("column PB missing on source main"). The fix carries the visitor's actual schema name
  (`v.schemaName`) onto `existsSubqueryPlanner.schemaName` and threads it through the recursive CTE-catalog
  builders (`buildLogicalPlanForQueryWithCTECatalog` / `…BodyWithCTECatalog` / `…UnionWithCTECatalog` /
  `buildUnionRightBranchStrippingOrderBy` → `buildLogicalPlanForSelectWithCTECatalog`), replacing the hardcoded
  default with the session schema so the schema-qualified-table demotion (`demoteSchemaQualifiedUnnest` /
  `normalizeSchemaQualifiedSelectSources`) uses the ACTIVE schema. The non-CTE `…WithCatalog` variants (reached
  only from the top-level EXPLAIN generators) keep the default; the CTE-catalog path only short-circuits to
  them when the active schema IS the default. `PlanRecordQueryWithMetadataSchema` is the schema-parameterized
  harness entry (mirroring the real session path `NewPlanVisitorWithSchema` + the schema-threaded demotion),
  shared by the default-schema `PlanRecordQueryWithMetadata`. Plain + alias-collision + filter + no-match cases
  under schema `main` are pinned revert-proof.

**EXISTS over a lateral unnest that is NOT the rightmost FROM item (codex round-13).** One bug:
- **P2a (silent-wrong, drops rows): the existential-residual rebase covered only the TWO top-level join
  legs, not the outer table BURIED under a non-rightmost lateral unnest.** `FROM T1, T1.ARR AS V, U WHERE
  EXISTS (SELECT 1 FROM U2 WHERE U2.ID = T1.ID)` — the unnest is followed by ANOTHER comma source `U`, so the
  TOP-LEVEL `LogicalJoin`'s right child is `U` (a scan), NOT the unnest. The unnest-EXISTS guard
  (`translateUnnestExistsFilter`, which fires only when `join.Right` is a `LogicalUnnest`) falls through to the
  GENERIC join+EXISTS path (`translateJoinWithExists` → `ImplementNestedLoopJoinRule.implementJoinWithExistential`).
  Go's two-level join+EXISTS plan binds the inner-join's MERGED row under ONE fresh `mergedOuterCorr` (unlike
  Java's single FlatMap that keeps every FROM quantifier bound), so the existential residual `U2.ID = T1.ID`
  must rebase `QOV(T1).ID` onto that merged binding. But `T1` is BURIED under the left leg's unnest subtree —
  the left leg flows one merged row under the unnest alias `V`, and `T1`'s columns survive only as the merged
  row's verbatim qualified `T1.ID` key (`NewAnchoredJoinRecord` propagates a nested leg's dotted columns by
  name; `mergeRows` preserves dotted keys verbatim). The rule's `rebaseOuterLegRefsToMerged` rewrote only the
  TWO top-level quantifier aliases `{V, U}`, leaving `QOV(T1).ID` unrebased → `QOV(T1)` UNBOUND below the
  FlatMap → `T1.ID` evaluates NULL → `U2.ID = NULL` false for every row → ALL matching unnested rows silently
  dropped (an equality on the inner PK escaped via the fast-path probe; only a slow-path residual referencing
  the buried table hit this). The fix (`mergedOuterLegAliases`) derives the COMPLETE outer-leg alias set the
  merged row anchors columns for — ALGEBRAICALLY from the anchored result value's (`sel.GetResultValue()`,
  a `NewAnchoredJoinRecord`) dotted field-name PREFIXES (`{T1, V, U}`), the merged row's column schema — and
  feeds that to the SAME existing `rebaseOuterLegRefsToMerged`, so the buried `T1.ID` residual reads the
  verbatim `T1.ID` key. `leftAlias`/`rightAlias` are always included, so a plain `FROM A, B` cross join's
  prefixes are `{A, B}` (the two quantifier aliases) and the rebase is unchanged (no-op for any already-bound
  reference). EXISTS / NOT-EXISTS / WITH-ORDINALITY / element-correlation / both-table-AND-element /
  filtering-residual / plain-WHERE-on-the-buried-table over a non-rightmost unnest are pinned revert-proof; the
  element-correlation and plain-WHERE cases (which reference `V` — a top-level leg — or route through the
  non-EXISTS `rebaseUnnestOuterLegPredicate` path) are part of the audited class and stay green on revert,
  while the buried-table EXISTS shapes fail on revert.

**Correlated subquery whose OWN FROM has an inner lateral unnest (codex round-15).** One bug:
- **P1 (valid-query-fails): the correlated-subquery fallback bypassed unnest lowering.** `SELECT ID FROM
  OUTER_T WHERE EXISTS (SELECT 1 FROM T1, T1.ARR AS V WHERE V = OUTER_T.col)` — the EXISTS subquery has its
  OWN lateral unnest (`T1.ARR AS V`) AND correlates to the outer query (`V = OUTER_T.col`). The catalog-aware
  inner-plan build hits an undefined column on the outer ref, so `BuildExists`/`BuildScalar` fall back to
  `buildCorrelatedExists`/`buildCorrelatedScalar`. Those fallbacks rebuilt EVERY inner `sq.join` as a plain
  `LogicalScan` — bypassing the lateral-unnest classification — so `T1.ARR` was scanned as a TABLE name →
  `table not found: T1.ARR`. The fix routes each inner FROM leg through the SAME helpers the main FROM path
  and the round-8 EXISTS-over-unnest path use: `correlatedSubqueryJoinRight` emits a `LogicalUnnest` for a
  comma source classified by `lateralUnnestCandidate` (over `visibleFromAliases` + `newUnnestTableResolver`),
  and `addCorrelatedJoinScopeSource` registers the SAME `unnestVirtualScopeSource` (Shadowing) into the
  subquery's inner scope so the inner WHERE on the element/ordinal resolves. No parallel lowering — the
  Cascades translator's `translateUnnestJoin` then lowers the inner unnest to `FlatMap(Scan, Explode)` INSIDE
  the correlated subquery, and the outer-correlated residual binds. EXISTS / discriminating-EXISTS / NOT-EXISTS
  / WITH-ORDINALITY / SCALAR-in-projection / outer-id-carrying variants are pinned revert-proof (each fails with
  `table not found` on revert).

**Unnest × {non-rightmost-filter, GROUP BY, aggregate ORDER BY} (codex round-16).** Three bugs, all
unnest crossed with another clause:
- **P1 (silent-wrong, drops rows): a WHERE on a BURIED (non-rightmost) unnest element/ordinal.** `FROM T1,
  T1.ARR AS V, U WHERE V > 0` — the lateral unnest is NOT the rightmost FROM item (`U` is), so the TOP-LEVEL
  `LogicalJoin`'s Right is `U` and the unnest is BURIED in `join.Left`. `translateFilter`'s unnest-element
  WHERE rewrite only fired when `join.Right` was a `LogicalUnnest`, so for the buried unnest the rewrite was
  SKIPPED — the `V > 0` reference stayed `FieldValue{V, QOV(V)}` on the OUTER NestedLoopJoin where `V` reads
  the FlatMap binding under an ambiguous correlation and evaluates NULL → every matching row dropped (got=[];
  the plan put the predicate on the NLJ, not the inner Explode). The fix (`pushBuriedUnnestPredicateDown`,
  reusing the round-13 `buriedUnnestLegs` left-subtree recursion) pushes each conjunct that references ONLY a
  buried unnest's element/ordinal correlation (and no rightmost-leg source) DOWN to a `LogicalFilter` wrapping
  the inner join where the unnest IS the rightmost source — making the buried case structurally identical to
  the direct `FROM T1, T1.arr AS V WHERE V > 0` shape, so the SAME proven direct path
  (`rewriteUnnestPredicate` → folded into the inner Explode's `PredicatesFilter`, Java's `EXPLODE … | FILTER`)
  handles it for EVERY comparison operator. A cross-leg conjunct (`V = U.x`) or an outer-table conjunct stays
  at the outer level (residual). The buried element/ordinal WHERE (`>` / `=` / `>=` / arithmetic), the
  outer-id-carrying variant, the ordinal `AT > 1` / `AT + 1 = 3` buried variants, and the rightmost-unnest
  control are all pinned revert-proof.
- **P2a (silent-wrong grouping): GROUP BY on a buried unnest element.** `SELECT V, COUNT(*) FROM T1, T1.ARR
  AS V, U GROUP BY V` where `U` also has a `V`. A SIMPLE column group key does NOT populate `groupByExprs`, so
  `GROUP BY V` bypassed the resolver and fell back to a bare `FieldValue{V}` — which `mergeRows` overwrites
  last-leg-wins with `U.V`, so grouping collapsed every element into ONE group (`U.V=999`). The fix routes a
  simple group key through `ResolveColumnShadowingQualified` (the SAME helper the projection/ORDER-BY paths
  use) in `upgradeAggregateOperands`, so a key that binds to the unnest Shadowing source resolves to the
  QUALIFIED `V.V` (which `mergeRows` preserves verbatim) — grouping on the unnest element. An
  explicitly-qualified `U.V` group key binds to U's real source (not Shadowing) → left for the bare fallback
  (control: groups by U's column).
- **P2b (silent-wrong order): aggregate ORDER BY on the group key.** `SELECT V, COUNT(*) FROM T1, T1.ARR AS V
  GROUP BY V ORDER BY V DESC` — for a GROUPED query the ORDER BY sort sits ABOVE the aggregate, and
  `upgradeSortKeyValues` maps a group-key sort key to the EXPLAIN of the group-key Value. With the P2a fix
  populating the group key to the qualified `FieldValue(QOV(V), V)`, the explain is `V.V` — but the aggregate
  output keys the group-key column by the executor's `aggKeyName` (a FieldValue's bare `Field`, i.e. `V`), so
  the sort read a `V.V` column the aggregate output does not carry → a no-op sort (DESC ignored). The fix is
  the root cause: `groupKeyExplainMap` now uses `aggregateGroupKeyOutputName` (the exact mirror of the
  executor's `aggKeyName` — the bare field for a FieldValue, the explain for a computed key) instead of the
  raw explain, so the sort key arrives carrying the correct aggregate-output column name. `qualifyShadowedSortKeys`
  is unchanged (the grouped key now arrives with a Value set, so its `Value != nil` guard skips it); only a
  PRE-aggregate (non-grouped) bare ORDER BY over an unnest still gets the round-7/8 `V.V` qualification — the
  ASC + DESC grouped cases and the non-grouped shadowing-sort control are all pinned revert-proof.

**Convergence pass — every scope/resolver/column-enumeration path made unnest-aware (codex round-18).**
A full audit of every embedded-planner path that builds a scope/resolver from `sq.joins` or enumerates a
source's columns surfaced two silent-wrong bugs of the SAME class (a planning path UNAWARE of the
lateral-unnest virtual source), plus two convergence targets a fallback was masking. The root pattern: a
loop over `sq.joins` resolving each leg as a REAL table (`ResolveTable`/`GetRecordType`) that FAILS or is
wrong for a `LogicalUnnest` leg, with no `isLateralUnnestJoin` check to register the shared
`unnestVirtualScopeSource` instead.
- **P2a (silent-wrong): the explicit JOIN ON predicate was dropped when an explicit JOIN precedes a comma
  unnest.** `FROM T1 INNER JOIN U ON U.ID = T1.ID, T1.ARR AS V` — `upgradeJoinOnPredicates` built its
  ON-resolution scope by resolving EVERY join clause as a real table; the unnest leg (`T1.ARR`, not a table)
  made that scope build ABORT (`scopeOK=false → return nil`), so the explicit JOIN's `ON` predicate was never
  attached → the T1/U join degraded to a CROSS join BEFORE the unnest → non-matching row pairs (silent-wrong).
  The fix makes `upgradeJoinOnPredicates` skip the lateral-unnest leg and register its virtual source via the
  SAME `isLateralUnnestJoin`/`unnestScopeSourceAdder`/`newUnnestTableResolver` helpers every other scope
  builder uses, so the ON predicate resolves against the real-table legs and the T1/U join stays an INNER join
  (the inner Explode's outer scan carries the `[=]` equality). `schemaName` was threaded into the function for
  the table resolver. Revert-proof: pre-fix returns the 2×4 cross product with the wrong K pairings.
- **P2b (silent-wrong): a qualified star over an unnest alias was not expanded.** `SELECT V.* FROM T1,
  T1.ARR AS V` (and the `AT` variant) — `expandQualifiedStars` (mixed `a.*`) and `expandProjQualifier` (lone
  `a.*`) enumerated columns ONLY from `md.GetRecordType`, so they could NOT expand `V.*` → the query was left
  with an unexpanded star → the nil-projCols path returned the ENTIRE FlatMap row (outer T1 columns included)
  instead of just the unnest source's columns (silent-wrong). The fix expands a qualified star over the unnest
  virtual source to its element column (the AS alias) for a non-ordinal unnest, or element + ordinal (AS + AT)
  under ordinality — derived from the SAME `unnestVirtualScopeSource` column list, so the star enumeration can
  never diverge from the WHERE/projection scope binding. `schemaName` threaded into both functions. Revert-proof:
  pre-fix leaks `ID|ARR1|ARR1_NN|STRARR|T1.*` alongside the element.
- **Convergence (fallback no longer load-bearing):** `buildWherePredicateForJoinsWithCTEScopes` (the CTE-aware
  WHERE builder, the twin of the already-unnest-aware `buildWherePredicateForJoins`) and
  `buildProjectionResolverWithCTEScopes` (the projection/GROUP-BY/HAVING/ORDER-BY resolver, whose 5 callers
  each fell back to `buildSelectScope`) were made directly unnest-aware via the shared helpers, so a CTE-in-scope
  unnest WHERE / aggregate resolves at the primary builder rather than declining and relying on a fallback.
- **Audited ALREADY-CORRECT paths:** `buildSelectScope` (1582), `buildWherePredicateForJoins` (777),
  `buildOuterScopeSources` (4337), `logical_builder.go` join chain (235/267), the PlanVisitor `visitFrom`
  (953), and the derived-only loops (which gate on `j.derivedQuery != nil`) already handle the unnest leg. The
  OLD map-based `execSelect*` pipeline (`join.go`, `resolveQualifierColumns`, `buildProjectionResolver`,
  `expandStarSlots`) is reached ONLY by the EXPLAIN-only plan-equivalence harness — the real query path is
  Cascades (`planSelectCascades` → `PlanVisitor`); an unnest there errors cleanly at `ResolveQualifiedTableName`
  (`Unknown database`), never silent-wrong. The degenerate ALIASLESS field-name-collision lone star
  (`SELECT ARR1.* FROM T1, T1.ARR1`) now CLEANLY REJECTS (the rebuild-path projection-source derivation treats
  the qualifier as a table) rather than leaking the whole row — acceptable (never silent-wrong); the spec'd
  aliased `V.*` is fully correct.

Tests (round-18): the R18 subtests in `array_unnest_ordinality_fdb_test.go` — P2a explicit JOIN ON before a
comma unnest (base / ordinality / element-WHERE, with the new `JU` table matching a proper subset of T1's
ids so cross-join vs inner-join are 8-vs-4 rows), P2b qualified star (non-ordinal element-only / ordinality
element+ordinal / mixed-with-named-outer-column), and the convergence CTE-scope WHERE (element / ordinal) +
GROUP-BY-over-unnest — each proven revert-proof (P2a returns the cross product, P2b leaks the whole outer row
on revert).

**Streaming-aggregate pre-aggregate sort must carry the QUALIFIED group key (codex round-19).** One bug,
the streaming-aggregate twin of the round-8 P2a in-memory ORDER BY fix and the follow-on to the round-15/16
GROUP-BY-shadowing fix:
- **P2a (silent-wrong, wrong group counts): the REQUIRED pre-aggregate sort used the BARE group key, not
  the qualified one.** The round-16 fix made `GROUP BY V` over a shadowing unnest alias resolve to the
  QUALIFIED key `V.V` (`GroupKeyValues`, so the aggregate cursor groups on the unnest ELEMENT, not a later
  same-named column). But `ImplementStreamingAggregationRule` builds its `InMemorySort(FullScan)`
  pre-aggregate sort (Go's streaming-aggregation requires sorted input — Java refuses unsorted GROUP BY; Go
  inserts the sort) and keyed each `SortKey` off `fv.Field` ONLY (the bare `V`) for any `*FieldValue` group
  key. For `FROM GD, GD.ARR AS V, GW` where `GW` also has a column `V`, the streaming aggregate GROUPS by the
  qualified `V.V` (the element), but the inserted sort ordered by the merged row's BARE `V` — which
  `mergeRows` keys last-leg-wins as `GW.V` (a constant → a NO-OP sort). Sort and group key DISAGREE, so a
  streaming aggregate (which emits a new group on every group-key CHANGE between adjacent rows) splits
  contiguous-only-after-sorting array elements into multiple NON-CONTIGUOUS groups → duplicate groups with
  WRONG counts (an array with duplicate/non-contiguous element values across outer rows, e.g. `{1,2},{1,2}`,
  splits each value into two count-1 groups instead of one count-2 group). The fix routes a CORRELATED/
  qualified `FieldValue` group key (`fv.Child != nil`) — and any non-`FieldValue` computed key — through the
  `SortKey.ValueExpr` per-row path, EXACTLY like `ImplementInMemorySortRule`'s ValueExpr branch (round-8
  P2a): the pre-aggregate sort then evaluates the SAME qualified `V.V` (or `V.O` for the ordinal) the
  aggregate cursor groups by, so sort and grouping agree and each value/ordinal is one contiguous group. A
  BARE childless `FieldValue` group key keeps the fast `Field` path (the index-ordered / non-shadowed case is
  unchanged — `orderingSatisfiesGroupingKeys` and the index-streaming-agg rule are untouched). Revert-proof
  FDB tests (`GD`/`GW` tables, arrays with non-contiguous duplicate values): `GROUP BY V` over a shadowing
  later source (element) and `GROUP BY O` over a shadowing later `O` column (ordinal) each split into
  count-1 groups on revert; the no-shadowing-source control passes both ways.

**Unifying post-aggregate group-key rebase: projection + HAVING + ORDER BY all read the bare output
name (codex round-20).** One bug class, closed for the whole sub-class in one helper — the projection/
HAVING twin of the round-18 ORDER-BY rebase:
- **P2 (silent-wrong, NULL): a POST-aggregate consumer referencing a grouped unnest key read the qualified
  PRE-aggregate value `V.V` off a bare-`V` aggregate row.** The round-15/16 GROUP-BY-shadowing fix stores the
  QUALIFIED `FieldValue(QOV(V), V)` (explain `V.V`) in `GroupKeyValues` so grouping is on the unnest ELEMENT,
  but the aggregate cursor OUTPUTS that key under `aggKeyName` = the BARE `V` (`finalizeGroup`). Round-18
  already rebased the post-aggregate ORDER BY (`aggregateGroupKeyOutputName` → bare `V`); round-20 found the
  same mismatch in the other two post-aggregate consumers: a COMPUTED projection (`SELECT V + 1, COUNT(*) …
  GROUP BY V`) and a HAVING that stays above the aggregate (`HAVING V > x AND COUNT(*) > 1`). Both resolve `V`
  against the PRE-aggregate Shadowing scope → qualified `V.V`, which reads the MISSING `V.V` key off the
  bare-`V` aggregate row → NULL: the computed projection column was NULL, and the residual HAVING dropped
  EVERY group (`NULL > x` false). The fix is the SAME `aggregateGroupKeyOutputName` rebase round-18 uses,
  applied uniformly: a single helper (`rebasePostAggregateGroupKeyValue`, via `values.MapFieldValues` +
  `ValuesStructurallyEqual`) rewrites any reference to a QUALIFIED grouped-unnest group key down to the bare
  aggregate-output name, run over (1) every post-aggregate `ProjectedValues` slot and (2) the HAVING
  predicate (`rebaseHavingGroupKeyPredicate`). The HAVING rebase mirrors `PushFilterThroughGroupByRule`
  EXACTLY (`havingPredicatePushesBelowAggregate`): a PURE group-key HAVING (`V > 1`, a single group-key
  comparison) is PUSHED BELOW the aggregate where it MUST keep the qualified `V.V` (the pre-aggregate element
  binding — rebasing it there would read a later same-named column, the round-16 trap), so it is left
  untouched; everything that stays ABOVE (a residual referencing an aggregate) is rebased to bare. ORDER BY is
  already done (round-18) and shares the SAME `aggregateGroupKeyOutputName` — no duplication. Audit: the three
  post-aggregate consumers now resolve the group key to the bare output name (projection `(V + 1)` → bare `V`,
  residual HAVING `AND(CMP(V …), CMP(COUNT(*) …))` → bare `V`, ORDER BY `InMemorySort([V …])` → bare `V`),
  while the pushed-below HAVING stays `CMP(V.V …)`. Revert-proof FDB tests (`GD`/`GW`/`T1`): the computed
  projection + the residual-HAVING-AND-aggregate read NULL / drop all groups on revert; shadowing variants
  prove the element (not a later `GW.V`/`U.V`) is used; the pure-group-key HAVING, `HAVING COUNT(*) > 1`
  control, and the no-shadow projection control pass both ways.

**AT-on-a-table source must surface WRONG_OBJECT_TYPE, not a masking undefined-column (codex round-22).**
One bug:
- **P1 (wrong SQLSTATE / error masking): an AT on a single-segment TABLE source bound a virtual unnest scope,
  masking 42809.** `SELECT U.ID FROM T1, U AT O` — a comma source `U` that is a REAL distinct TABLE (a
  SINGLE-segment name, not `alias.field`) carrying an `AT` ordinal alias. AT on a table is `WRONG_OBJECT_TYPE`
  (42809), which the translator's `translateUnnestJoin` raises (segment 0 `U` does not resolve to a visible
  in-scope scan via `findOuterScanTable` → `unnestFallbackOrReject`'s AT path). But the SELECT scope registers
  a VIRTUAL unnest binding for any AT source (`isLateralUnnestJoin` → the unconditional AT shortcut in
  `unnestCandidateShape`), correlation = the AT alias `O` — SHADOWING the real table `U`. So a reference to
  `U`'s own column (`U.ID`) failed to resolve at scope validation with a MASKING `42703` (undefined column)
  BEFORE the translator ran. The masking is bidirectional: simply NOT registering the virtual unnest (and
  registering the real `U` instead) breaks the AT-alias-reference (`SELECT O …` → 42703 on `O`) and ambiguous
  column shapes (`FROM T1, T1.arr AS V, U AT O` → `ID` ambiguous between T1 and U), because the scope-level
  column validation runs BEFORE translation either way. The ROOT FIX is Java's `generateAccess` ordering: AT
  -on-a-table is rejected at FROM-source ANALYSIS time, BEFORE the SELECT/WHERE column resolution. A new pass
  `rejectAtOrdinalityOnTable` (in `cascades_generator.go`) walks the built logical tree and, for any
  `LogicalJoin{Right: LogicalUnnest{AtAlias != ""}}` that is in truth an AT on a table / non-array source,
  raises `WRONG_OBJECT_TYPE` — run EARLY in `VisitQuery` (right after `visitFrom`, before any projection column
  resolution) so the faithful 42809 is the surfaced error regardless of what the query references. It MIRRORS
  the translator's `translateUnnestJoin` AT-rejection EXACTLY (the translator is the authority; the early pass
  is a faithful echo, with no per-case divergence): reject IFF (1) segment 0 does NOT resolve to a visible
  in-scope SCAN in the outer leg (`findOuterScanInLeg`, the embedded twin of `findOuterScanTable` — a
  table/schema-qualified/unknown qualifier), OR (2) segment 0 resolves to a REAL base table whose remaining
  segment(s) name a MISSING / single-segment-bare / PRESENT-SCALAR field. A GENUINE array (planned), a
  CTE/derived-output source (record type not in md → left to the translator's `outerSourceIsCTE`
  `UNSUPPORTED_QUERY`), and a missing field on a real table (the translator's `UNDEFINED_COLUMN`) are NOT
  rejected here, so the early pass never diverges from the translator's per-case code. A harness-level backstop
  (`PlanRecordQueryWithMetadataSchema` + the generator's `Plan`, after `demoteSchemaQualifiedUnnest`, before
  `validateTablesAndColumns`) reaches an AT-on-table inside an EXISTS/scalar subquery (whose plan is attached
  only after `VisitQuery` returns), recursing into `subqueryPlans`. Revert-proof FDB tests: `SELECT U.ID FROM
  T1, U AT O` and `SELECT O FROM T1, U AT O` (referencing the AT alias) and the EXISTS-subquery variant →
  42809 (the `U.ID` case is 42703 on revert); controls — a genuine `T1.ARR1 AS X AT O` still unnests (FlatMap
  over Explode WITH ORDINALITY), a non-array correlated field `T1.ID AS X AT O` → 42809, and a normal
  `FROM T1, U` (no AT) cross join is unaffected (NestedLoopJoin, no Explode/FlatMap).

**DML rebuild paths classified a schema-qualified comma source against the WRONG schema (codex round-23).**
One bug, audited and closed across all four DML paths — the DML twin of round-12 P2b (which threaded the
session schema into the SELECT-side subquery planners):
- **P1 (non-default-schema DML, fail/wrong-rows): the DML SELECT/WHERE rebuild hard-coded the default
  schema.** `INSERT INTO dst SELECT … FROM PA AS main, main.PB AS B` in a session whose schema is `main`
  (NOT the default `s`). `PA` is aliased `main` (== session schema name), so `main.PB` is BOTH "field PB on
  source main" AND the schema-qualified TABLE PB; Java's `generateAccess` resolves the TABLE first. The live
  DML path (`planDML`) builds the logical op through `buildLogicalPlanForInsertWithCatalog`, whose
  INSERT … SELECT rebuild re-planned the inner SELECT with the HARDCODED `defaultEmbeddedSchema` (`s`). So
  the in-builder table-first demotion (`demoteSchemaQualifiedUnnest` / `normalizeSchemaQualifiedSelectSources`,
  run inside `buildLogicalPlanForSelectWithCTECatalog`) classified `main.PB` against `s` — qualifier `main`
  != `s` → it stayed a correlated `LogicalUnnest` over the non-existent `PA.PB`, which the DML path's
  `resolveQualifiedTableNames` cannot repair (it strips a schema qualifier off a `LogicalScan`, not a
  `LogicalUnnest`) → the INSERT FAILS / inserts the wrong rows. The fix threads the SESSION schema
  (`g.c.sess.Schema`, the same source the SELECT planner's `NewPlanVisitorWithSchema` uses) through the three
  DML catalog builders (`buildLogicalPlanFor{Insert,Update,Delete}WithCatalog` gain a `schemaName`
  parameter), replacing the hardcoded default, so the schema-qualified source classifies against the ACTIVE
  schema. The EXPLAIN / DDL-explain / explain-only DML callers pass `g.sessionSchema()` (the active CONNECT
  schema, default when no session) so the explain text matches the live plan.
- **Audit (all DML paths, no round-24 on UPDATE/DELETE):** (1) **INSERT … SELECT** — the SELECT-source
  rebuild, fixed above. (2) **INSERT VALUES** — no SELECT/WHERE, no comma source, no classifier reach (safe).
  (3) **UPDATE … SET/WHERE** and (4) **DELETE … WHERE** — the single-table DML FROM has no comma source, so
  the unnest classifier never fires on the DML's own FROM; but a schema-qualified comma source CAN live inside
  a `WHERE EXISTS (SELECT 1 FROM PA AS main, main.PB AS B)` subquery, whose plan is built by the
  `existsSubqueryPlanner` in `upgradeDMLWhereWithCatalog` — which was constructed with NO `schemaName`
  (defaulting to `s`). `upgradeDMLWhereWithCatalog` now takes the session schema and threads it onto the
  `existsSubqueryPlanner.schemaName` (the round-12 field) and `buildSelectScope` / `buildOuterScopeSources`,
  so the subquery's schema-qualified source classifies against the active schema. Revert-proof FDB tests
  (real `sql.Open(…&schema=main)` connection driving the live `planDML` path):
  `INSERT INTO DST SELECT B.id, B.v FROM PA AS main, main.PB AS B WHERE B.v >= 200` inserts exactly PB's
  matching rows; `DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA AS main, main.PB AS B WHERE B.id = 10)`
  deletes all rows (match) and a no-match variant deletes zero (control) — each FAILS (`DML Cascades
  translation failed`) on revert.

**Correlated-subquery schema-qualified source + SELECT-* unnest column metadata (codex round-25).** Two
bugs:
- **P2a (valid-query-fails): the correlated-subquery fallback did not strip a schema qualifier off its
  inner FROM sources.** `SELECT ID FROM T1 WHERE EXISTS (SELECT 1 FROM PA AS s, s.PB AS B WHERE B.ID =
  T1.ID)` (session schema `s`). The outer correlation (`B.ID = T1.ID`) makes the catalog-aware inner build
  fail with an undefined column, so EXISTS/scalar fall back to `buildCorrelatedExists` /
  `buildCorrelatedScalar`. Those fallbacks rebuild the inner FROM clause themselves and handed the raw
  schema-qualified `s.PB` straight to `Analyzer.ResolveTable`, which — unlike the normal catalog-aware SELECT
  path (`normalizeSchemaQualifiedSelectSources`, run in `buildLogicalPlanForSelectWithCTECatalog`) — does NOT
  strip a schema qualifier → `table not found: S.PB`, rejecting a valid correlated subquery. The fix runs the
  SAME `normalizeSchemaQualifiedSelectSources(sq, p.effectiveSchemaName(), p.md)` pass at the top of both
  fallbacks, BEFORE the scan/join tree and scope sources are built, so `s.PB` → `PB` (the real cross-join
  table) — Java's `generateAccess` resolves the table first at every FROM-source point. It strips both the
  primary and the comma/JOIN legs; a dotted reference whose qualifier is a prior FROM alias (a genuine
  lateral unnest) is not a `[schema, table]` pair and survives for the unnest classifier. EXISTS /
  NOT-EXISTS / schema-qualified-primary-source / correlated-SCALAR-subquery variants are pinned revert-proof
  (each fails `table not found: S.PB`/`S.PA` on revert).
- **P2b (metadata gap): `SELECT *` over a lateral unnest omitted the element (and ordinal) columns.**
  `SELECT * FROM T1, T1.ARR AS V [AT O]`. The unnest lowers to `FlatMap(outer, Explode)` whose result value
  is a source-anchored join record (`buildUnnestResultValue` → `NewAnchoredJoinRecord`) carrying the outer
  columns PLUS the element `V` (and, under ordinality, the ordinal `O`). But `deriveColumnsFromFlatMap` —
  because the result value is an ANCHORED join — fell through to MERGING the outer scan's columns with the
  inner Explode's, and the inner Explode has NO derivable record columns, so the element/ordinal were
  DROPPED from the result-set COLUMN metadata: `SELECT *` advertised only the outer columns. The per-ROW
  datum still carried `V`/`O` (`RecordConstructorValue.Evaluate` writes the bare key), so the defect lived
  ONLY in the column metadata (the dimension the row-datum tests never probed). The fix adds a lateral-unnest
  branch to `deriveColumnsFromFlatMap` (gated on an AnchoredJoin result value AND `findExplodePlan(inner) !=
  nil`, the structural marker of the unnest leg) that derives the `SELECT *` columns from the result value's
  BARE (non-dotted, user-visible) fields — the same per-field derivation the RFC-141 projected-EXISTS fold
  uses (`foldedColumnDef`), restricted to the bare keys (the qualified `ALIAS.COL` forms are
  resolution-convenience duplicates, exactly as a normal join's `SELECT *` reports bare labels via
  `qualifyAndMergeColumns`). So `SELECT *` over an unnest returns the outer columns AND `V` [+ `O`],
  matching `SELECT id, V [, O]`. A metadata-only harness accessor `ResultColumnLabelsForPlan` (the no-FDB
  analog of the driver's `paginatingRows.Columns()`, over the same production `deriveColumnsFromPlan`) lets
  the planner harness assert the column SET for array-column shapes that have no SQL array-literal seed form.
  Base / WITH-ORDINALITY / aliasless (field-name-shadowing element) variants are pinned revert-proof (the
  column set drops `V`/`O` on revert).

**Catalog SELECT builder did not mirror the PlanVisitor's unnest shadowing rewrites + non-ordinal element
type (codex round-26).** Two bugs and a dual-path convergence audit. The CATALOG SELECT builder
(`buildLogicalPlanForSelectWithCTECatalog_postBuild`) is a SEPARATE path from `PlanVisitor.visitSelectQuery`
— it serves SUBQUERY (scalar/EXISTS via `BuildScalar`/`BuildExists`), DML (INSERT…SELECT via
`buildLogicalPlanForInsertWithCatalog`), and derived-table-inside-a-subquery SELECTs. A top-level SELECT
uses the PlanVisitor; only these non-top-level SELECTs reach the catalog builder.
- **P2a (silent-wrong column/order): the catalog builder did not qualify a SHADOWED unnest bare projection
  / ORDER BY key.** `SELECT V FROM GD, GD.ARR AS V, GW ORDER BY V DESC` (GW has a REAL scalar column `V`)
  INSIDE a subquery/DML. The PlanVisitor qualifies a bare projection column that binds to the unnest
  `Shadowing` source to `V.V` (`ResolveColumnShadowingQualified`, step 2) and the matching ORDER BY sort key
  (`qualifyShadowedSortKeys`, step 15a) — but the catalog builder applied NEITHER. So the subquery's inner
  projection AND sort emitted the BARE `V`, which `mergeRows` overwrites last-leg-wins with `GW.V` (the
  unnest is not the rightmost FROM leg) → the subquery projected/sorted the WRONG column (GW.V) instead of
  the unnest element. The fix mirrors the SAME two steps in the catalog builder, reusing the existing
  `ResolveColumnShadowingQualified` + `qualifyShadowedSortKeys` helpers (no duplicated logic), so the
  catalog/subquery/DML SELECT path shadows IDENTICALLY to the top-level path. Revert-proof on the inner
  plan SHAPE (an EXISTS subquery renders its inner plan inline): with the fix the inner is
  `Project([V.V], InMemorySort([V.V DESC], …))`; on revert it is the bare `Project([V], InMemorySort([V
  DESC], …))` — asserted on BOTH the projection and the sort, asc + desc, and projection-without-ORDER-BY.
  (A scalar-subquery-over-unnest's VALUE is not yet pre-evaluated in execution — a separate gap — so the
  plan-shape assertion is the faithful axis, exactly as the column-type tests assert on the planned Value.)
- **AUDIT (dual-path convergence):** every unnest-aware step the PlanVisitor applies, and the catalog
  builder's status, verified IDENTICAL after the P2a fix:
  - qualified-star expansion (`expandProjQualifier`/`expandQualifiedStars`) — PRESENT in catalog builder.
  - **bare-projection shadowing qualification (`ResolveColumnShadowingQualified`) — was MISSING; mirrored
    (P2a).**
  - **ORDER-BY shadowing qualification (`qualifyShadowedSortKeys`) — was MISSING; mirrored (P2a).**
  - GROUP-BY simple-key shadowing + post-aggregate group-key rebase (`upgradeAggregateOperands`,
    `upgradeProjectionValues` → `rebasePostAggregateGroupKeyValue`, `upgradeHavingPredicate` →
    `rebaseHavingGroupKeyPredicate`) — PRESENT (shared helper calls, same args).
  - JOIN-ON unnest-aware scope (`upgradeJoinOnPredicates`) — PRESENT.
  - subquery outer-scope unnest source (`buildOuterScopeSources`) — PRESENT.
  - post-aggregate ORDER-BY rebase (`upgradeSortKeyValues`) — PRESENT.
  - schema-qualified-table demotion + normalization (`demoteSchemaQualifiedUnnest`,
    `normalizeSchemaQualifiedSelectSources`) — PRESENT.
  - buried-unnest predicate pushdown (`pushBuriedUnnestPredicateDown`) — in the SHARED translator
    (`translateFilter`), reached by BOTH paths.
  - AT-on-a-table early rejection (`rejectAtOrdinalityOnTable`) — in the generator's `Plan`, recursing into
    `subqueryPlans`, so it covers catalog-built subquery/DML plans (attached after `VisitQuery` returns).
  No further divergence: the catalog/subquery/DML SELECT now behaves identically to the same SELECT at top
  level for every unnest-aware step.
- **P2b (wrong metadata type): a NON-ordinality unnest's element column was typed UnknownType → BIGINT.**
  In `buildUnnestResultValue` the non-ordinal element was the BARE `QuantifiedObjectValue` (Java's
  primitive-branch binding), whose `Typ` defaults to `UnknownType`; `deriveColumnsFromFlatMap` →
  `foldedColumnDef` then fell back to BIGINT. So `SELECT * FROM T1, T1.STRARR AS VAL` advertised `VAL` as
  BIGINT even though every row is a STRING. The WITH-ORDINALITY path already preserved the element type via
  `NewOrdinalFieldValue(qov, 0, elementType)`. The fix types the non-ordinal element QOV to the array's
  `elementType` (`values.NewQuantifiedObjectValueOfType(innerCorr, elementType)`), so the element column
  reports the real type (STRING for STRARR, INTEGER for an INT array) — matching the ordinality path. `Typ`
  is metadata only (`Evaluate` ignores it), so execution is unchanged. Revert-proof via a new harness
  accessor `ResultColumnTypesForPlan` (the no-FDB analog of the driver's column-type metadata, over the
  same `deriveColumnsFromPlan`): `SELECT *` over a STRING array reports `VAL`=STRING (BIGINT on revert), an
  INT array reports INTEGER (BIGINT on revert), and the WITH-ORDINALITY STRING variant reports STRING
  element + INTEGER ordinal (the ordinality path was already correct, pinned so the non-ordinal fix does not
  regress it).

**Lateral unnest lowering gated to COMMA-origin sources (codex round-28).** One bug:
- **P1 (invalid shape returns rows): an explicit `INNER JOIN` with a dotted array source lowered as a lateral
  unnest.** `SELECT V FROM T1 INNER JOIN T1.ARR1 AS V` — an explicit JOIN whose right source is a dotted
  `alias.field` and which carries NO `ON` clause. The lateral-unnest classifier (`unnestCandidateShape`,
  shared by `lateralUnnestCandidate` / `isLateralUnnestJoin` / `correlatedSubqueryJoinRight`) ran for EVERY
  `fs.joins`/`sq.joins` entry; its only comma-vs-JOIN discriminator was `j.onExpr != nil`, but a no-ON inner
  join ALSO has `onExpr == nil`, so the explicit JOIN passed the gate and silently planned as
  `FlatMap(Scan(T1), Explode(ARR1))` — RETURNING the unnested elements instead of surfacing the table/join
  error. This contradicts `extractJoinClause`, which already treats every JOIN source as NEVER lateral. Java
  unnests ONLY via the comma-syntax `FROM t, t.arr AS x` correlated-field path
  (`LogicalOperator.generateCorrelatedFieldAccess`); an explicit JOIN right source is added by the JOIN
  visitor as a normal table/derived operator, never a lateral array unnest (and the entire `array-join-at`
  / unnesting yamsql corpus uses comma syntax exclusively — there is no JOIN-unnest test). The ROOT FIX
  carries the comma-vs-JOIN ORIGIN on the `joinClause` (a new `fromComma` flag, set `true` ONLY at the two
  COMMA-source construction sites in `parseFromSource`; left `false` on every `extractJoinClause`-produced
  explicit-JOIN entry) and gates the single shared `unnestCandidateShape` predicate on it (`if !j.fromComma
  { return false }`), so the lowering AND the scope binding can never diverge. An explicit JOIN with a dotted
  `alias.field` source now falls to `logical.NewScan(j.tableName, …)` → resolved as a qualified table whose
  `alias` is an unknown database qualifier → `ErrCodeUndefinedDatabase` (42F00, the existing table-not-found
  path, unchanged). Revert-proof FDB test: `SELECT V FROM T1 INNER JOIN T1.ARR1 AS V` → 42F00 (returns the
  unnested rows on revert); controls — the SAME dotted source via COMMA (`FROM T1, T1.ARR1 AS V`) still
  unnests (FlatMap over Explode, the four ARR1 elements), a normal explicit `INNER JOIN U ON U.ID = T1.ID`
  is unaffected (no Explode), and a plain `FROM T1, U` comma cross join is unaffected (NestedLoopJoin).

**Early AT-on-table rejection in EVERY SELECT build path, not only post-attach (codex round-29).** One bug,
the error-masking twin of the top-level early pass:
- **P1 (wrong SQLSTATE in subqueries): an AT-on-a-table source inside a subquery whose OWN predicate
  resolves first surfaced a masking `42703` instead of `42809`.** `SELECT id FROM T1 WHERE EXISTS (SELECT 1
  FROM UV, U AT O WHERE U.ID = 1)` — the AT shortcut keeps `U AT O` a `LogicalUnnest` so the AT survives to a
  clean rejection, and the subquery scope registers a VIRTUAL unnest binding (correlation `O`) that SHADOWS
  the real table `U`. The post-attach backstop (`rejectAtOrdinalityOnTable` in `cascades_generator.go` /
  `plan_harness.go`) walks an already-ATTACHED subquery tree (via `subqueryPlans`), but the subquery here is
  built during `VisitQuery` through the catalog SELECT builder, and resolving `U.ID` against the shadowing
  virtual binding FAILS with `ErrCodeUndefinedColumn` (42703) BEFORE the plan is ever attached — so the
  backstop never sees it and the masking 42703 surfaces instead of the intended WRONG_OBJECT_TYPE (42809).
  The ROOT FIX runs the SAME early AT-on-table rejection (`rejectAtOrdinalityOnTableWithCTEs`, the existing
  helper — reused, not duplicated) on the built FROM tree INSIDE `buildLogicalPlanForSelectWithCTECatalog`,
  AFTER the FROM tree + schema-qualified demotion and BEFORE `_postBuild`'s WHERE/projection column
  resolution, threading the in-scope WITH-CTE names from `cteScopes` (exactly as the top-level PlanVisitor
  seeds from `v.cteScopes`). This is the single choke point EVERY catalog SELECT body flows through, so it
  covers all the build paths the audit enumerated: EXISTS / scalar subqueries (`BuildExists` / `BuildScalar`
  → `buildLogicalPlanForQueryWithCTECatalog` → the builder), derived tables, UNION branches, CTE bodies, the
  INSERT … SELECT source (`buildLogicalPlanForSelectWithCatalog` → the builder), and the DML WHERE-EXISTS
  subquery (`upgradeDMLWhereWithCatalog` → `existsSubqueryPlanner.BuildExists` → the builder). The correlated
  fallbacks (`buildCorrelatedExists` / `buildCorrelatedScalar`) are reached ONLY when the catalog builder
  returns `ErrCodeUndefinedColumn`; an AT-on-table now returns `WRONG_OBJECT_TYPE` from the builder FIRST, so
  the fallbacks never see an AT-on-table.
- **DML error propagation (the DML axis of the same masking).** The DML WHERE-EXISTS rebuild
  (`upgradeDMLWhereWithCatalog`) returned a bare `bool` and SWALLOWED the catalog builder's carried error into
  a silent text fallback that cannot plan the EXISTS — so the faithful `42809` was lost as a generic
  `0AF00` "DML Cascades translation failed". It now returns the carried `*api.Error` (gated on the WHERE
  actually containing an EXISTS atom, the one shape the text builder cannot plan at all — a plain comparison
  WHERE still falls back), and the three DML catalog builders (`buildLogicalPlanFor{Delete,Update,Insert}WithCatalog`)
  thread it up so `planDML` surfaces it, matching the SELECT path's translation-error precedence.
  `buildLogicalPlanForInsertWithCatalog` likewise surfaces the SELECT-body build error rather than swallowing
  it into the original mis-classified-unnest source. Revert-proof FDB tests:
  `... WHERE EXISTS (SELECT 1 FROM UV, U AT O WHERE U.ID = 1)` (and a correlated `U.ID = T1.ID`, a NOT-EXISTS,
  and a scalar-subquery-in-projection variant) → 42809 (42703 on revert); the live DML path
  `DELETE FROM USRC WHERE EXISTS (SELECT 1 FROM PA, PB AT O WHERE PB.id = 10)` and
  `INSERT INTO DST SELECT … FROM PA, PB AT O WHERE PB.id = 10` → 42809 (0AF00 on revert); controls — a genuine
  unnest inside a subquery (`FROM T1 AS T, T.ARR1 AS V WHERE V > 0`) still plans, the top-level
  `FROM T1, U AT O` AT-on-table stays 42809, and a genuine DML WHERE-EXISTS still affects rows.

**Out of scope (clean-rejected, never silently wrong).** Multiple/chained unnests in one FROM
(`containsLateralUnnest` guard → `UNSUPPORTED_QUERY`; needs nested-FlatMap merged-row threading and Java's
deep-tuple `q3._0._1` shape), struct-array element field access, unnest over a CTE/derived-table OUTPUT (the
rejection keyed on the RESOLVED bound source: `outerSourceIsCTE(outerTable)` / structural
`outerSourceIsDerivedTable(j.Left, seg0)` → `UNSUPPORTED_QUERY`; needs an output-type resolver the
leg-column `UnknownType` model does not yet provide), and a computed SELECT projection over the ordinal
(driver-level column projection, not
unnest semantics — covered by the WHERE-on-ordinal test). A derived-table column projected THROUGH an outer
unnest's FlatMap (`SELECT d.V … FROM (…) AS d, T, T.arr AS x`) flows only when QUALIFIED to a base source
(`PB.ID`); the bare/derived form is the same pre-existing merged-row resolution limitation, not an unnest
defect (the P2a tests assert the qualified outer column + the unnested element).

Tests: `pkg/relational/sqldriver/array_unnest_ordinality_fdb_test.go` (FDB, 10× determinism;
includes the P1a non-ordinal-element-filter + P1b aliasless shapes, the codex round-4 P1/P2a/P2b/P2c
classifier shapes, the codex round-5 R5a CTE/derived-output rejection + R5b schema-qualified-alias-equals-
schema cross-join/AT shapes, the codex round-6 R6 two-scope-derived-unnest + AT-on-bare-source shapes, and the
codex round-7 derived-alias-shadows-real-table rejection (P1) + later-same-named-column-after-unnest qualified
projection (P2) — with their real-table-D and explicit-qualified-`u.v` controls — and the codex round-8
ORDER-BY-shadowing asc/desc (P2a) + WHERE-EXISTS / NOT-EXISTS / WITH-ORDINALITY / element-AND-EXISTS /
ordinal-AND-EXISTS compositions over a lateral unnest (P2b), and the codex round-10 real-table-aliased-with-a-
CTE-name shadowing-unnest (`FROM D AS X, X.ARR AS V` while a CTE `X` exists) with its still-rejected
CTE-genuinely-the-source control (`FROM X, X.ARR AS V` — X IS the CTE) — each proven revert-proof, and the
codex round-12 EXISTS-over-unnest-correlating-to-the-OUTER-TABLE (P2a: EXISTS / NOT-EXISTS / WITH-ORDINALITY /
filtering / both-outer-table-AND-element) + non-default-schema schema-qualified-table-inside-a-subquery (P2b:
plain / alias-collision / filter / no-match under session schema `main`, via `PlanRecordQueryWithMetadataSchema`)
— each proven revert-proof, and the codex round-16 buried-unnest WHERE on the element/ordinal (P1: `>` / `=` /
`>=` / arithmetic / ordinal `AT > 1` / outer-id-carrying / rightmost-unnest control), GROUP BY on a buried
unnest element (P2a: per-element counts vs the explicitly-qualified-`U.V` control), and aggregate ORDER BY on
the group key asc/desc (P2b: with the non-grouped shadowing-sort control) — each proven revert-proof, with an
`assertRowsOrdered` execution-order assertion for the ORDER BY cases) +
translator unit tests for the nil-md `LogicalUnnest` clean error (codex P2b) and the Explode/plan ordinality
equals/hash/result-type. Full sqldriver + EXISTS/join + cross-engine conformance suites green, no regressions.

**DML duplicate-alias guard + normalize/build ordering + ordinal nullability metadata (codex round-31).**
Three bugs:
- **P1 (silent-wrong, DML dual-path gap): the duplicate-unnest-alias guard was not run for DML.** The
  round-29 `rejectDuplicateUnnestAlias` pass ran in `planSelectCascades` but NOT in `planDML`. An
  `INSERT INTO dst SELECT V FROM T1, T1.ARR AS V, U AS V` reached translation without the guard, so the
  later `U AS V` overwrote the unnest's `V` keys (`mergeRows` last-leg-wins) and the INSERT wrote WRONG
  rows instead of raising the duplicate-alias error. The fix runs the SAME `rejectDuplicateUnnestAlias`
  pass on the DML `logicalOp` in `planDML` (after `resolveQualifiedTableNames`, before translation); it
  recurses through `LogicalInsert.Source` / `LogicalUpdate.Input` / `LogicalDelete.Input` (their Children)
  and subquery plans, so a colliding alias anywhere in the DML's FROM scope is rejected with
  `ErrCodeDuplicateAlias`.
- **P2a (silent-wrong, normalize/scan desync): normalized schema aliases desynced from already-built
  scans.** `normalizeSchemaQualifiedSelectSources` strips a NO-alias schema-qualified source `s.PB` → `PB`
  on the `selectQuery`, but in `buildLogicalPlanForSelectWithCTECatalog` it ran AFTER
  `buildLogicalPlanForSelect`, so the built `LogicalScan` still carried Alias `S.PB` (a no-alias source
  parses with `alias == tableName == "S.PB"`). In a catalog subquery a predicate like `PB.ID = PA.ID`
  resolved (via the normalized selectQuery scope) to `QOV(PB)` while translation bound the scan as `S.PB` —
  so the bare table-name reference read NULL → misfiltered rows (silent-wrong). The ROOT fix moves the
  `normalizeSchemaQualifiedSelectSources` call (and the `schemaName` default) to BEFORE
  `buildLogicalPlanForSelect`, so the built scan carries the normalized alias `PB` and resolver + scan
  agree. `demoteSchemaQualifiedUnnest` stays after the build (defence-in-depth for the unnest-misclassified
  variant, plus its subquery-plan recursion).
- **P2b (metadata nullability): the synthesized ordinal column reported nullable.** `SELECT * FROM t,
  t.arr AS v AT ord` — the synthesized `ord` field's value is `NewOrdinalFieldValue(qov, 1, NotNullInt)`
  (Java's `Type.primitiveType(INT, false)`), but it has NO proto descriptor field, so `foldedColumnDef`
  defaulted it to `ColumnNullable` and the result-set metadata reported the NOT-NULL ordinal as nullable.
  The fix derives the column's nullability from `f.Value.Type().IsNullable()` when no descriptor resolves,
  so a NOT-NULL synthesized column (the ordinal, an EXISTS boolean) reports `ColumnNoNulls` while a
  genuinely nullable element column (a nullable array element type, an UnknownType fallback) still reports
  `ColumnNullable`.

Tests (round-31): the R31 subtests in `array_unnest_ordinality_fdb_test.go` — P1 the new
`TestFDB_ArrayUnnestDMLDuplicateAlias` (live `planDML` via the SQL driver: INSERT…SELECT later source
reusing the unnest AS alias / AT alias → `ErrCodeDuplicateAlias`, with the non-colliding-later-source
control that still succeeds); P2a a no-alias schema-qualified subquery source (`EXISTS (SELECT 1 FROM EXA,
s.EXB WHERE EXB.ID = …)`, constant-compare revert sentinel + the prompt's `EXB.ID = EXA.ID` cross-leg shape
+ the aliased-sibling control); P2b the ordinal-NOT-NULL / element-NULLABLE same-query check (via the new
`ResultColumnNullabilityForPlan` harness, no Docker) plus the AT-only ordinal — each proven revert-proof
(P1: no rejection on revert; P2a: empty/dropped rows on revert; P2b: ordinal reports nullable on revert).
Full sqldriver + cascades/query/embedded/executor/values suites green, no regressions.
