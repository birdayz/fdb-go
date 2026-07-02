# RFC-173 — Name-Burial Inventory (Slice 2 entry gate)

**Status: COMPLETE** (two-axis sweep, 2026-07-02). This is the Round-5-mandated enumeration of
every name-keyed row producer/consumer and alias-swap site, each slotted into its owning slice —
so nothing is discovered one blocker at a time mid-slice (the lesson of Slice 1's Step 2b).
Consumed by Slices 2–4; §4 of `173-ordinal-column-resolution.md` references this file.

Slice legend: **S1** = done (non-join frontier flip, merged #437) · **S2** = 2-way wedge ·
**S3** = N-way flip + interning + node identity · **S4** = name-machinery deletion ·
**S6** = extensions. `S2→S4` = introduced/consumed by S2, deleted by S4.

## Key conclusions

1. **The boundary invariant:** `mergeRows`, `qualifyOuterRow`, `remapUnionColumnsByPosition`, and
   the aggregate output (`finalizeGroup`/`emptyScalarResult`) all emit rows with **no
   `Positional`** — exactly where the S1 ordinal frontier dies today. S2 re-births the ordinal row
   at the 2-way join producers; S3 at union/aggregate/intersection/lateral. **Every re-birth site
   must extend the `DisablePositionalEmission` oracle gate** (Graefe standing obligation).
2. **`executeProjection` straddles**: it writes all four key shapes (source-bare + alias + `_N` +
   dense positional slot) and serves both frontier and over-join rows — its name map cannot fully
   delete until S3 flips the join producers beneath it.
3. **The `AnchoredJoin` flag is the linchpin**: introduced/consumed by S2, load-bearing for node
   identity + interning in S3, deleted only in S4. Every read site is enumerated below (Axis 2:
   A2–A3, A7, A10–A17, B4, B7–B9, C4, D1, D18, E6–E7) and must be visited before the flag dies.
4. `qualifyTypeFallback` exists at `executor.go:2140-2161` (Axis-2's "not found" was a
   directory-scope artifact — it is an executor mechanism, Axis 1 site A15).
5. Union name-recovery gates + RFC-142 shadowing plumbing lean **S6** (explicitly TODO-tracked
   follow-ups, not core join-model flips); if RFC-142 shadowing is scoped into the N-way work,
   the D14–D17 rows move to S3.

---

## AXIS 1 — Executor (`pkg/recordlayer/query/executor/`)

### A. Name-keyed row PRODUCERS

| # | file:line | site | keys written | slice |
|---|-----------|------|-------------|-------|
| A1 | query_result.go:133 `protoToMap` | scan-row name map from proto | bare `UPPER(field)` (unset omitted = NULL) | S4 |
| A2 | query_result.go:56-67 `FromStoredRecord` | scan/DML-echo row birth (Datum + Positional) | bare UPPER | name S4; Positional S1 |
| A3 | executor.go:860-886 `buildCoveringRow` | covering-index row (datum + PositionalRow) | bare value cols then PK; last-wins collision (positional keeps both) | name S4; Positional S1 |
| A4 | executor.go:843-848 `coveringIndexCursor.OnNext` | emits `{Datum,Positional}` | via A3 | S1 / name S4 |
| A5 | executor.go:1410-1472 `executeProjection` | the **triple-write** projected map | `projNames[i]` (source/physical) + `UPPER(alias)` + `_N` for computed + dense slots; Positional only when input frontier | name S4 (blocked on S3); Positional S1 |
| A6 | executor_new_plans.go:462-507 `executeMap` | result-value output | resultValue record fields | name S4; Positional S1 |
| A7 | streaming_cursors.go:313-367 `finalizeGroup` | aggregate group output (`Complete:true`, no Positional) | `aggKeyName` + paren-stripped alias + `aggResultName` + `agg.Alias` + spaced `FUNC(OPERAND)` | **S3** |
| A8 | streaming_cursors.go:369-383 `emptyScalarResult` | empty-input scalar aggregate row | `aggResultName` + `agg.Alias` | **S3** |
| A9 | streaming_cursors.go:214-248 `computeGroupKey` (output) | group-key tuple feeding A7 | values only | S3 |
| A10 | executor_new_plans.go:83-99 `aggregateIndexCursor.OnNext` | aggregate-index scan row | bare `groupCols[i]` + `canonicalName` | S3 |
| A11 | executor_new_plans.go:200-216 `multiIntersectionMergeCursor.OnNext` | merges child Datums, evals resultValue | copies all child bare/dotted keys | S3 |
| A12 | executor.go:1797-1819 `remapUnionColumnsByPosition` | union branch re-key by position; **drops Positional** | `targetKeys[i]` from `m[srcKeys[i]]` | S3 |
| A13 | executor.go:2056-2110 `mergeRows` | **THE 2-way join producer**; drops Positional | Pass A bare (inner overwrites if namespace differs); Pass B `ALIAS.COL`; Pass C `TYPE.COL` | **S2** |
| A14 | executor.go:2119-2134 `qualifyAlias` | writes `ALIAS.COL` per bare col | `alias+"."+UPPER(col)` | **S2** |
| A15 | executor.go:2140-2161 `qualifyTypeFallback` | writes `TYPE.COL` for unaliased legs | `recType+"."+UPPER(col)` fill-if-absent | **S2** |
| A16 | executor.go:2167-2211 `qualifyOuterRow` | LEFT/FULL-OUTER unmatched outer (+ FULL-OUTER inner drain streaming_cursors.go:877); drops Positional | verbatim copy + `outerQual.COL` + `TYPE.COL` | **S2** |
| A17 | flat_map_cursor.go:181-217 `computeResult` | flatMap 2-way output; identity-over-outer → A16; LEFT-OUTER empty inner `{}` | qualified via A16 or raw computed | **S2** |
| A18 | executor.go:3000-3005 `explodeOrdinalityRow` | WITH-ORDINALITY row | `_0`,`_1` (`OrdinalFieldName`) | S3 |
| A19 | executor.go:2947-2992 `executeExplode` | lateral UNNEST rows | element-as-datum / A18 | S3 |
| A20 | executor.go:3007-3017 `executeValues` | VALUES row | bare `col.Name()` | S3 |
| A21 | executor.go:2917-2944 `executeTableFunction` | table-function rows | element-as-datum | S3 |
| A22 | executor.go:2891-2914 `executeTempTableInsert` | temp-table write (recursive CTE) | inherits inner keys | S3 |
| A23 | continuation.go:325-347 `decodeSortContinuation` | rebuilds Datum from JSON on resume | name keys from JSON | S4 |
| A24-25 | executor.go:2440,2558,2730 DML echo | delete/insert/update row echo | via A2 | name S4; Positional S1 |
| A26 | executor_new_plans.go:600-604 `executeInJoin` binding | binds IN value as correlation (scalar) | no name-map row | S3 |

### B. Name-keyed row CONSUMERS

| # | file:line | site | read mechanism | slice |
|---|-----------|------|----------------|-------|
| B1 | executor.go:3593-3597 `fieldFromDatum` | `m[UPPER(key)]` | S4 (kill list) |
| B2 | executor.go:3617-3623 `sortKeyFromResult` | positional-first, Datum fallback | fallback S4 (kill list) |
| B3 | executor.go:3824-3848 `compareByField` | positional-first, `m[field]`/EqualFold scan | fallback S4 (kill list) |
| B4 | executor.go:3604-3608 `sortEvalRow` | `Positional` else `Datum` | Datum branch S4 |
| B5 | executor.go:3920-3943 `queryResultKey` | iterates ALL map keys (CTE dedup) | S4 |
| B6 | executor.go:3892-3912 `queryResultKeyForCols` | `m[col]` per canonical col | S4 |
| B7 | executor.go:1342-1366 `distinctKey` | iterates ALL map keys (DISTINCT) | S4 |
| B8 | resultset.go:115-132 `columnValue` | `m[UPPER(colName)]` — **final materialization, last to flip** | S4 |
| B9 | resultset.go:134-140 `columnValueByName` | via B8 | S4 |
| B10 | streaming_cursors.go:821-835 `lookupJoinKey` | `m[col]`, `m[alias.col]`, dot-split EqualFold scan | **S2** |
| B11 | streaming_cursors.go:702-708 `tryBuildHashIndex` | lookupJoinKey per inner row | **S2** |
| B12 | streaming_cursors.go:910-913 NLJ hash probe | lookupJoinKey(outerJoinCol) | **S2** |
| B13 | executor.go:2221-2240 `passesJoinPredicates` | merged qualified 2-way row → RowContext | **S2** |
| B14 | streaming_cursors.go:737-819 `extractEquijoinColumns`/`splitQualified`/`matchesAlias` | parses `ALIAS.COL` from predicates | **S2** |
| B15 | streaming_cursors.go:214-221 `computeGroupKey` (read) | `k.Evaluate(row.Datum)` | S3 |
| B16 | streaming_cursors.go:267-272 `accumulateRow` | `agg.Operand.Evaluate(row.Datum)` | S3 |
| B17 | executor_new_plans.go:787-843 `mergeSortCursor` | `key.Evaluate(a.Datum)` | S3 |
| B18-19 | executor.go:1915-1942 / executor_new_plans.go:148-174 intersection comp keys | `kv.Evaluate(qr.Datum)` | S3 |
| B20 | scalar_subquery.go:64-77 `EvaluateScalarSubquery` | `row.Datum.(map)` 1-col extract | S4 |
| B21 | continuation.go:296 `encodeSortContinuation` | serialize Datum map | S4 |
| B22 | memory_budget.go:146-201 `estimateQueryResultBytes` | map key-len + value bytes | S4 (adapt) |
| B23-25 | executor.go:2497-2607,2680-2686 DML input paths | Datum map → proto fields | S4 |
| B26 | executor.go:1642-1656 union branch-key scrape | `mapKeysOrdered(items[0].Datum)` | S3 |
| B27 | executor.go:3079,3196 `recursiveUnionOutputColumns` fallback | scrape first-row Datum keys | S3 |
| B28 | flat_map_cursor.go:159,183,192 `computeResult` binding | outer map cast + inner Datum correlation bind | **S2** |

### C. PositionalRow birth/propagation (oracle-gate registry)

| # | site | kind | gate |
|---|------|------|------|
| C1 | query_result.go:59-64 `FromStoredRecord` | birth | explicit `DisablePositionalEmission` |
| C2 | executor.go:845-848 covering cursor | birth | explicit |
| C3 | executor.go:1463-1469 `executeProjection` | propagate (input-gated) | implicit via C1/C2 |
| C4 | executor_new_plans.go:497-507 `executeMap` | propagate (input-gated) | implicit |

Every S2/S3 re-birth site (A7, A8, A11, A12, A13, A16, A17, A18-A22) must join this registry with
an explicit or upstream-implied gate.

---

## AXIS 2 — Planner / Translator / Resolver

### A. Anchored-join name machinery

| # | file:line | mechanism | slice |
|---|-----------|-----------|-------|
| A1 | values/value_anchored_join_record.go:10 | `AnchoredJoinLeg{Alias, Columns}` | S2 |
| A2 | values/value_anchored_join_record.go:54-99 | `NewAnchoredJoinRecord` — the key-set contract (qualified always, bare last-leg-wins, dotted verbatim; sets `AnchoredJoin=true`) | S2 |
| A3 | values/value_anchored_join_record.go:121-132 | `NewScalarSubqueryAnchoredRecord` | S2 |
| A4 | values/value_anchored_join_record.go:155-185 | `anchoredColumnsByQuantifier` (dotted fields grouped by QOV) | S3 |
| A5 | values/value_anchored_join_record.go:192-206 | `leftmostQOV` | S3 |
| A6 | values/value_anchored_join_record.go:217-231 | `ReEnumerationLeg`/`reEnumColumn` | S3 |
| A7 | values/value_anchored_join_record.go:259-339 | `NewReEnumerationAnchoredRecord` (dotted split, lastBareOcc) | S3 |
| A8 | values/value_anchored_join_record.go:344-355 | `allPrefixedBy` | S3 |
| A9 | values/values.go:2444-2472 | `RecordConstructorValue.AnchoredJoin` flag — **the linchpin** | S2→S4 |
| A10-12 | map_field_values.go:87-88; replace.go:198-202; simplifier_value.go:391-394,480-484 | flag preservation through WithChildren/Replace/simplifier | S2→S4 |
| A13 | map_field_values.go:314-332 | RC EqualsWithoutChildren splits on `AnchoredJoin` | S3 |
| A14 | semantic_hash.go:94-106 | RC hash folds `AnchoredJoin` | S3 |
| A15 | rule_partition_select.go:425-430,505-513 | `buildUpperResult` + re-enum call sites | S3 |
| A16 | rule_partition_select.go:860-908 | `rebaseBuriedLowerReferences` (dotted rewrite) | S3 |
| A17 | rule_partition_select.go:910-922 | `isAnchoredJoinResult` gate | S3 |

### B. Dotted-name / prefix classifiers

| # | file:line | mechanism | slice |
|---|-----------|-----------|-------|
| B1 | values/value_correlation.go:27-51 | `MergeSeedLegsOfValue` (dotted-prefix leg recovery) | S3 |
| B2 | values/value_correlation.go:56-70 | `leftmostQOVOfValue` | S3 |
| B3 | values/value_correlation.go:83-119 | `GetCorrelatedToOfValue` **AnchoredJoin skip** (hiding) | S4 (via S5 duality removal) |
| B4 | values/value_correlation.go:121-151 | `GetCorrelatedToOfAnchoredJoinLegs` (re-exposure) | S2/S3 |
| B5 | predicates/predicate_correlation.go:55-108 | `AddMergeSeedAliases` | S2 |
| B6 | rule_partition_select.go:717-740 | `quantifierMergeSeedLegDeps` | S3 |
| B7 | rule_match_intermediate.go:673-698 | `valueCorrelationWithSeeds` | S2 |
| B8 | rule_implement_nested_loop_join.go:756-793 | `mergedOuterLegAliases` (dotted prefixes) | S2 |
| B9 | rule_implement_nested_loop_join.go:795-908 | `rebaseOuterLegRefsToMerged`/`rebaseOuterLegValue` | S2 |
| B10 | rule_implement_nested_loop_join.go:1831-1855 | `fieldValueAliasAndCol` + `bareColumnName` | S2 |
| B11 | values/values.go:371-378,405-414 | `FieldValue.Evaluate` runtime dotted merge-key resolution (RFC-043) | S4 |

### C. Name-based identity / interning

| # | file:line | mechanism | slice |
|---|-----------|-----------|-------|
| C1 | values/map_field_values.go:260-262 | FieldValue EqualsWithoutChildren name compare | S3 (node-identity flip; Round-5 finding 1) |
| C2 | values/semantic_hash.go:107-108 | `"field:"+Field` hash | S3 |
| C3 | values/simplifier_value.go:208-225 | `composeFieldOverConstructor` name fold | S4 |
| C4 | expressions/select.go:221-256 | `InternsAliasAware` gate | S3 (built) → S4 (widened+deleted) |
| C5-6 | expressions/reference.go:305-371 | alias-aware interning tier + interface | S3 |
| C7 | memo.go:118-140 | `NextMergeAlias` plan-hash hack | S3/S6 |
| C8 | expressions/select.go:195-218 | Select EqualsWithoutChildren alias-aware | S3 |
| C9 | embedded/connection.go:36-46 (+eval_map.go:55-56) | `ambiguousColumnMarker` sentinel (42702) | S4 |

### D. Translator / resolver plumbing remaining post-S1

| # | file:line | mechanism | slice |
|---|-----------|-----------|-------|
| D1 | cascades_translator.go:610-636 | `buildJoinResultValue` → `NewAnchoredJoinRecord` | S2 |
| D2 | cascades_translator.go:103-127 | `tableColumns` leg feed | S2 |
| D3 | cascades_translator.go:3372-3379,3505-3511 | binary-seed call sites | S2 |
| D4 | cascades_translator.go:341-381 | `derivedOutputColumns` | S2/S6 |
| D5-7 | cascades_translator.go:383-520+ | union name gates (`unionOutputColumns`/`unionBranchNormalizable`/`aggregateNamesStableForUnion`) | S6 |
| D8 | cascades_translator.go:67,77-85,228,677,3878,3957 | `cteColumnsScope` | S6/S4 |
| D9 | values/type.go:502-523 (+values.go:463-473) | `OrdinalFieldName` `_N` emulation | S4 |
| D10-11 | cascades_translator.go:4012-4079 | `normalizeLegToOutputColumns`/`legPhysicalOutputNames`/`recursiveRemapValues` dot-split | S4 (kill list) |
| D12 | values/values.go:591-607 | `ProjectionColumnName` contract | S4/S6 |
| D13-14 | expr/expr.go:204-273 | `ResolveIdentifier` qualification + `ResolveColumnShadowingQualified` (RFC-142) | S4/S6 |
| D15-17 | semantic/scope.go:41-49,123-132; logical_predicate.go (5 sites); plan_visitor.go (3 sites) | Shadowing sources + consumers (RFC-142) | S6 (or S3 if scoped in) |
| D18 | cascades_generator.go:2808-2832 | `SELECT *` from anchored-RC bare fields | S2 |
| D19 | cascades_generator.go:2754,2850,2902 | `qualifyAndMergeColumns` | S2/S4 |
| D20 | cascades_translator.go:3613-3644 | `existsInnerCorrelation` merged-row routing | S2 |

### E. Other name-load-bearing

| # | file:line | mechanism | slice |
|---|-----------|-----------|-------|
| E1 | logical_predicate.go:5298-5311 | `resolveCorrelatedColumnValue` flat `Field:"ALIAS.COL"` emission | S2/S4 |
| E2 | logical_predicate.go:3091-3097 | `bareTableName` schema-qualifier strip (schema-level, not column model) | S6/front-end |
| E3 | logical_predicate.go:3405-3415 | dotted-key classifiers in DML/predicate resolution | S4 |
| E4 | logical_predicate.go:4560-4563 | dotted `qualifier.COL` lowering | S6/front-end |
| E5-6 | rule_implement_nested_loop_join.go:305-363,434,762-792,1534,1785-1826 | NLJ seed-aware classification + `AnchoredJoin` gates | S2 |
| E7 | physical_flat_map_wrapper.go:47 | AnchoredJoin gate ref | S2 |
| E8 | rule_partition_binary_select.go:202 | `AddMergeSeedAliases` call | S2 |
