# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.11.1.0**. FDB wire protocol: **7.3.75**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

## Phase 7: Cascades alignment — close remaining Java divergences

### 7.1 Unify alias namespaces

**Priority: HIGH.** Quantifier aliases (`q$0`, `q$1`) and table aliases (`R`, `P`, `C`) are separate namespaces. Java unifies them — the quantifier alias IS the table alias. Go's split causes:

- `PartitionBinarySelectRule` needs `rightAliasSet` workaround to match predicate correlations (table aliases) against quantifier aliases
- `planContainsJoin` guard in EXISTS compensates for alias qualification gaps in materialized multi-table inners
- `predicateReferencesAlias` string matching persists in ~10 call sites because correlation-set classification only finds QOV-based (table alias) references

**Fix:** At quantifier creation in the SQL translator (`cascades_translator.go`), use `NamedCorrelationIdentifier(tableAlias)` as the quantifier alias instead of `UniqueCorrelationIdentifier()`. Audit every `GetAlias()` consumer. This collapses three workarounds into zero.

**Verification:** Remove `rightAliasSet` expansion from `PartitionBinarySelectRule`. Remove `planContainsJoin` guard. Convert remaining `predicateReferencesAlias` sites to `GetCorrelatedToOfPredicate`. All 46 test targets pass.

### 7.2 Port matching infrastructure for index intersections

**Priority: HIGH.** `IndexIntersectionRule` is a Go-only logical rewrite rule that generates intersection alternatives combinatorially during REWRITING. Java doesn't have it — Java uses `MatchLeafRule` + `MatchIntermediateRule` + `ImplementIntersectionRule`, a match-then-implement pattern during PLANNING.

The Go-only approach loses to REWRITING cost model pruning for:
- 3-way intersections (pruned in favor of 2-way)
- Compound index vs intersection (compound index filter pruned before PLANNING can try it)
- DISTINCT over GROUP BY (hash tiebreak during REWRITING)

Three test assertions relaxed with root-cause documentation (`index_scan_e2e_test.go`).

**Fix:** Port `MatchLeafRule`, `MatchIntermediateRule`. Verify `ImplementIntersectionRule` works with match-based input. Delete `IndexIntersectionRule`. Restore fatal assertions on all 3 plan quality tests.

### 7.3 Convert remaining predicateReferencesAlias sites

**Priority: MEDIUM.** Blocked on 7.1 (alias namespace unification). `yieldGeneralFlatMap` uses correlation-set classification (Phase 2 of RFC-005). The following paths still use string-based `predicateReferencesAlias`:

- `implementExistentialSelect` NLJ fallback (line ~347)
- `implementJoinWithExistential` (line ~461)
- `tryFlatMapPlan` residual classification (~6 sites)
- `tryExistsFlatMap` residual classification (~2 sites)
- `buildExistsFlatMap` residual classification (~2 sites)

Both approaches produce identical results for QOV-based FieldValues (the common case). Legacy flat FieldValues (`Field: "ALIAS.COL"`, nil Child) are only found in non-predicate contexts (ORDER BY, GROUP BY, projections). Safe to convert after 7.1 confirms all predicate FieldValues use QOV children.

### 7.4 FlatMap wrapper correlation propagation

**Priority: LOW.** `physicalFlatMapWrapper.GetCorrelatedToWithoutChildren()` returns empty. `JoinMergeResultValue` stores `OuterAlias`/`InnerAlias` as struct fields, not as `QuantifiedObjectValue` children — `GetCorrelatedToOfValue` finds nothing. Correct for joins (correlations flow through quantifier children). When correlated subqueries are ported, `JoinMergeResultValue.Children()` must return QOV nodes.

NLJ wrapper correlation propagation (walks predicates) is already correct and active.
