# RFC-187 — Alias-aware column identity in index/aggregate matching (kill leaf-name column matching)

**Status:** REVIEWED (draft 2). Graefe **conditional-ACK** — ruled name-path is the correct
match-domain identity (see §3.0), conditions 1–4 folded below. Torvalds NAK (draft 1) folded
(missed sites §1.2, R1 resolved §2, §3.2 decided). Awaiting delta re-confirm on this draft.
**Tracks:** TODO.md "FINDINGS 2026-07-22" item **A** — subsumes finding 1 (CRITICAL wrong rows)
and finding 4 (wrong ORDER BY).
**Baseline:** master at `139060801`. Work branch `feat/rfc187-column-identity-matching`.
**Relationship to RFC-173:** planner-side analog of RFC-173 (stop resolving columns by leaf name),
on the index-**match** path. The deep unification (ordinalize the candidate) IS an RFC-173-endgame
follow-up (§8) — this RFC fixes the wrong-rows/wrong-order bug within the current representation.

---

## 1. The defect class

The planner decides "does this query column reference the same field as this index/candidate
column?" by comparing the **leaf field name only** (`strings.EqualFold(fv.Field, …)` /
`eqFold(fv.Field, colString)`), discarding the accessor chain. When a nested path's leaf name
collides with a differently-rooted column of the same source, it binds/matches the **wrong
column**. Direct violation of the CLAUDE.md prime directive ("don't strip qualifiers with string
hacks"). The precedent fix `buildTranslateValueFunction` (`match_candidate_index.go:422-431`) was
hardened against exactly this fused-multi-accessor collision ("if a covered column shares the leaf
name, silently read the WRONG slot (wrong rows)"); the match/ordering/aggregate paths were not.

### 1.1 Full site list (Torvalds draft-1: my "one authority" claim missed half of these)

| # | Site(s) | Sides | Effect |
|---|---------|-------|--------|
| S1 | `rule_match_intermediate.go:756` `valuesMatchColumn` FieldValue arm (+ `:767` Cardinality recurse, `:818` DistanceRowNumber) | query `Value` ↔ candidate `Value` | **CRITICAL, wrong rows** — nested `WHERE addr.city='x'` sargs top-level `city` index, marked matched → no residual re-check |
| S2 | `rule_ordered_index_scan.go:132` (calls `valuesMatchColumn`) + `:135` `eqFold(fv.Field,colName)` | sort-key `Value` ↔ index column | **wrong ORDER BY** — sort elided against wrong same-named column |
| S3 | `rule_ordered_primary_scan.go:68` `eqFold(fv.Field,pkCols[i])` | sort-key ↔ PK column string | wrong ORDER BY on PK |
| S4 | `aggregate_index_candidate.go:129,151,157,175,198,204` | query `FieldValue` ↔ index-column **string** | `GROUP BY/COUNT(addr.city)` matches top-level `city` agg index → wrong column |
| S5 | `rule_aggregate_data_access.go:195` (`eqFold` + `fieldNameOnly`) | query `FieldValue` ↔ string | group-key `WHERE addr.city='x'` → AISCAN on top-level `city` |
| S6 | `expression_partition.go:254-277` `orderingPartitionHash` + `:203-243` grouping | ordering-key hash | **wrong ORDER BY** — same-leaf-name orderings from different sources collapse into one partition |
| S7 | `rule_push_filter_through_groupby.go:99` | query `FieldValue` ↔ grouping key `FieldValue` | pushdown misclassification |
| S8 | `rule_streaming_agg_from_index.go:80,131` | query `FieldValue` ↔ index column string | agg-from-index misclassification |
| S9 | `rule_push_requested_ordering_through_groupby.go:126,137` | ordering ↔ grouping key | ordering pushdown misclassification |
| S10 | `rule_implement_streaming_agg.go:212` `EqualFold(fv.Field,oFV.Field)` | query `FieldValue` ↔ query `FieldValue` | grouping-key equivalence misclassification |

Related but distinct (dotted-name *string* checks, a name-model artifact, NOT leaf-vs-path — see
§2): `left_outer_existential.go:112`, `rule_implement_nested_loop_join.go:1499`,
`box_conjunct.go:149`, `ordinal_seed.go:736`. Triaged in §3.5 (out of A's wrong-rows scope; booked).

---

## 2. Root cause — match-time column identity is compared across a MIXED, MULTI representation

The canonical alias-aware `SemanticEqualsUnderAliasMap` (`values/semantic_equals.go:17`) exists and
is correct, and draft 1 proposed routing every site through it. **That is dead on arrival**, because
at match time the two sides are in *different, incompatible representations*. Proven in code:

- **Query side is BAKED** for qualified refs: `plan_visitor.go:1772 resolveQualifiedBaked` resolves
  `t.city` to a `SourceRelativeBaked()` FieldValue (ordinal `Resolved`, `Child=QOV`), and the
  front-end translator runs **before** `planner.Plan`. So `orient.column` (`rule_match_intermediate.go:649`)
  is baked.
- **Candidate side is LAZY, name-based**: `ValueIndexScanMatchCandidate.ColumnValue`
  (`match_candidate_index.go:126`) = `NewFieldValue(base, columnNames[i])` — a lazy FieldValue; the
  candidate carries only `columnNames []string`, **no ordinals**. The aggregate candidate is the
  same (`groupCols []string`). *Go's candidate is name-based by construction, unlike Java's
  KeyExpression-resolved (ordinal FieldPath) candidate.*
- `EqualsWithoutChildren` (`values/map_field_values.go:329`) declares **baked ≠ lazy**. So raw
  `SemanticEqualsUnderAliasMap(bakedQuery, lazyCandidate)` returns **false → NO index binds** for
  the common qualified-ref case. Graefe's "assert both-lazy" is therefore unavailable; the mixed
  representation is real, and leaf-name `EqualFold(Field,Field)` was the *bridge* across it (a baked
  node still carries its last-accessor `Field`).

Worse, a nested/qualified column ref exists in **three** plan-time forms:
- **(a) nested Child chain**: `FieldValue{Field:"city", Child: FieldValue{Field:"addr", Child:QOV}}`.
- **(b) fused baked**: `FieldValue{Field:"CITY", Child:QOV, Resolved:[ADDR,CITY]}`.
- **(c) flat dotted**: `FieldValue{Field:"addr.city"}` (e.g. `clustered_outer_scalar.go:610`,
  `cascades_translator.go:3530`; `NewFieldValue` stores verbatim; `fieldNameOnly` strips it).

The leaf-name compare is only *wrong* (collides) for forms (a)/(b) (leaf `Field="city"`); for form
(c) it happens to not collide (`"addr.city"≠"city"`). So finding 1's severity is
representation-dependent — **the implementation's first step is a probe test establishing which form
`WHERE addr.city=5` takes at match time** (§5.0). Regardless of the answer, the fix below is correct
and uniform across all three forms.

**Conclusion (Graefe ruling §3.0): identity can only be compared in a domain both operands can
express. The candidate's domain is names. Therefore match-domain identity is the full accessor
NAME-PATH — not the leaf, not the ordinal.** The ordinal identity (`FieldPath.Equals`) is a
*different domain* (runtime row evaluation, produced by the bake); unifying the two by ordinalizing
the candidate is the RFC-173-endgame follow-up (§8), not this fix.

---

## 3. Design

### 3.0 The primitive (Graefe conditions 1 & 2 baked in)

Add to `values`:

```
// AccessorNamePath returns the ordered accessor NAME path of a plan-time column
// reference (root-alias EXCLUDED), normalizing all three representations:
//   (a) nested Child chain  → walk Child, prepend each Field
//   (b) fused baked         → Resolved.Accessors[].Field
//   (c) flat dotted Field   → split on '.'
// ok=false when ANY accessor is pure-ordinal (Field==""): a machinery-owned bake
// (join/gather) has no name to compare — callers MUST treat ok=false as "cannot
// establish identity → do NOT match" (Graefe condition 1: asserted bridge, never a
// silent name fallback). Names are UPPER-cased (the resolver's normalization).
func AccessorNamePath(v Value) (path []string, ok bool)

// ColumnNamePathsEqual reports whether two plan-time column refs denote the same
// accessor path. false if either AccessorNamePath is !ok (Graefe condition 1) or
// the paths differ at ANY position (condition 2: every intermediate accessor, not
// leaf, not leaf+root).
func ColumnNamePathsEqual(a, b Value) bool
```

`ok=false` at a match site is a *conservative reject* (predicate stays residual → correct rows via
the slow path; ordering not elided → materialized sort). Combined with a loud one-line log at the
pure-ordinal case, it makes any future machinery-baked column reaching these sites visible instead
of silently mis-matching. This is the "one column-identity notion, no second authority" the whole
RFC turns on — every site below calls `ColumnNamePathsEqual` (value↔value) or compares against a
candidate path built the same way (value↔string, §3.3).

### 3.1 S1/S2 — `valuesMatchColumn` and its callers

`valuesMatchColumn(query, candidate)` → its FieldValue arm becomes `ColumnNamePathsEqual(query,
candidate)`. This subsumes the Cardinality (`:767`) and DistanceRowNumber (`:818`) arms: extend
`AccessorNamePath` to descend `CardinalityValue.Child` and `DistanceRowNumberValue`'s
partition+argument values (a distance key's identity is its metric class + those paths — encode the
metric class as a synthetic leading path element), then **delete** `distanceRowNumberValuesMatch`
and `fieldNamesMatch`. (Torvalds draft-1: deletion is safe iff `WindowedValue` exposes
partition/argument values via `Children()` — confirm during impl; if not, `AccessorNamePath` reads
them via the concrete-type accessors directly.) Callers: `rule_match_intermediate.go:649`,
`rule_ordered_index_scan.go:132` (Torvalds' unlisted caller — it needs no signature change since
`ColumnNamePathsEqual` takes two values; the `sourceAlias` bridge is unnecessary in the name-path
design because names are compared directly and roots are excluded — a simplification over draft 1).

### 3.2 S4/S5/S8 — aggregate & agg-from-index (Graefe condition 3: candidate must carry the REAL path)

The aggregate candidate mis-flattens nested columns: `cascades_generator.go:2213-2228` indexes
`gke.FieldNames()` (which for `GROUP BY addr.city` returns `["addr","city"]`,
`key_expression.go:836`) by the *logical* `groupingCount`, so `groupCols[0]="addr"` — the parent, not
the path. Name-path against a corrupt candidate path is still wrong. **Fix the candidate to carry
the real multi-name path** (Graefe condition 3 — the "Preferred" option, no reject-nested terminal
state):

- `AggregateIndexMatchCandidate` holds `groupPaths [][]string` and `aggPath []string` (accessor
  name paths), built by walking the `GroupingKeyExpression`'s nesting structure correctly
  (`NestingKeyExpression` contributes `[parent, ...child]`), not the flat `FieldNames()[i]` index.
- `MatchesGroupBy` / `MatchesSingleAggregateOf` / `groupColEqualityIndex` compare the query
  `FieldValue`'s `AccessorNamePath` against the candidate path element-wise. A nested query key vs a
  flat candidate → unequal → no false match; a nested query key vs a matching nested candidate →
  equal → correct match (this also *enables* nested aggregate indexes, which the flat model broke).
- Transitional `reject-nested` guard is used ONLY if the impl finds the group-key path plumbing is
  larger than A's scope for a given sub-case — and only *with* a test pinning "nested key does not
  match flat agg index" and a TODO to complete the path (Graefe: never a terminal state). Default is
  the full path fix.

### 3.3 S3 (PK), value↔string sites generally

Where a candidate side is a bare string (`pkCols[i]`, `colNames[i]`), the candidate string is the
column's declared name/path — compare the query `FieldValue`'s `AccessorNamePath` against the
string parsed to a path (split on '.'), element-wise. Single-element candidate path (top-level PK
column) vs a nested query path → unequal → correct.

### 3.4 S6 — ordering-partition hash + grouping (finding 4)

Two coupled fixes:
1. `orderingPartitionHash` hashes the full `AccessorNamePath` (root-alias excluded — preserves the
   existing reason for name-based hashing: dodging minted `q$N` nondeterminism, since accessor
   *names* are deterministic), plus direction + nulls placement. Collision-free across
   same-leaf/different-source.
2. `toPartitionsFromMap` gains a structural-equality tiebreak mirroring the sibling
   `RollUpPlanPartitions` (`orderingsEqual` → `ValuesStructurallyEqual`, `:367-372`): two orderings
   sharing a hash bucket must additionally pass structural equality to co-partition. Hash is the
   optimization; equality is the authority (the hash-map contract the hash-only code violated).
   **Note (Graefe):** `ValuesStructurallyEqual` here is deliberately alias-*sensitive* (not the
   alias-mapped primitive) and that is correct — partition members share ONE correlation namespace,
   so alias-invariance is neither needed nor wanted.

### 3.5 S7/S9/S10 — value↔value ordering/grouping equivalences; and the dotted-name checks

S7/S9/S10 compare two query `FieldValue`s → `ColumnNamePathsEqual`. The dotted-name *string* checks
(`left_outer_existential.go:112` etc., `strings.Contains(fv.Field,".")`) are a different concern —
they test *whether a ref is a qualified/composed name* (a representation probe), not column
identity; they do not conflate columns and are out of A's wrong-rows scope. Booked as a follow-up
(consolidate representation probes once ordinalization §8 lands), not touched here.

---

## 4. What this is NOT

- Not a wire-format change (index/key/continuation format untouched; only *which candidate binds*).
- Not the RFC-173 executor migration (disjoint files) — though §8 (ordinalize the candidate) is its
  endgame.
- Not a new/removed/reordered Cascades rule; the match algorithm's *comparison step* is corrected in
  place (Graefe: fidelity fix to match-then-implement, not a structural change).

---

## 5. Test plan (red→green on the unprobed dimension; positive binds pinned per Torvalds R1)

### 5.0 Reachability probes FIRST (before any code change)
- `TestProbe_NestedPredicateColumnRepresentation`: plan `SELECT * FROM t WHERE addr.city='x'` (t has
  nested `addr.city` + top-level `city` index) and inspect `orient.column`'s form (a/b/c) and
  whether the `city` index binds today. Establishes finding-1's actual reachability and pins the
  baseline. If it binds wrongly today → red; if form (c) dodges it → documents the safe rep and we
  still verify the ordering/aggregate sites independently.
- `TestProbe_FlatQualifiedColumnStillBinds`: `WHERE city='x'` (baked qualified ref) must bind the
  `city` index — the Torvalds R1 positive test that the baked-vs-lazy path does not regress index
  binding. Must stay green after the fix (name-path bridges baked↔lazy).

### 5.1 Per-site regressions (the "nested leaf collides with same-source top-level column" axis)
1. **S1** `TestFDB_NestedFieldShadowsTopLevelIndex` — `WHERE addr.city='NYC'` returns only nested
   matches; assert rows AND plan (the `city` index is NOT sargable-bound).
2. **S4/S5** `TestFDB_AggregateIndex_NestedGroupKeyNoFalseMatch` — `GROUP BY addr.city` does not use
   a top-level `city` agg index; correct groups. Plus `TestFDB_AggregateIndex_NestedGroupKeyMatches`
   — a nested agg index on `addr.city` now DOES match (condition 3 enables it).
3. **S6** `TestOrderingPartition_SameLeafDifferentSourceNoCollapse` (unit) +
   `TestFDB_OrderBy_SelfJoinSameLeafName` (FDB): self-join exposing `x` from both legs; `ORDER BY`
   one leg's `x` does not elide the sort. Plus a hash⟺equality-consistency unit test (§6 R3).
4. **S2/S3/S8/S9** ordering/agg-from-index nested-vs-flat pins.
5. **Non-regression**: full `sqldriver`/`recordlayer`/yamsql suites green (flat matches still fire).
6. **Determinism**: affected planner tests ×10 (skill §2).
7. **Stress**: 1M before/after — no row-count or plan regression.

---

## 6. Risks & mitigations

- **R1 (was the crux) — RESOLVED by the name-path design.** Name-path is bake-tolerant: it reads the
  name from baked (Resolved.Field) or lazy (Field/Child) alike, so baked-query↔lazy-candidate binds
  correctly. §5.0 `TestProbe_FlatQualifiedColumnStillBinds` pins it. The residual risk is a
  **pure-ordinal** accessor (`Field==""`) reaching a site → `ok=false` → conservative miss; the loud
  log surfaces it. This is asserted, not silent (Graefe condition 1).
- **R2 — case/normalization drift.** Names are UPPER-cased to the resolver's rule (condition 2). Pin
  a mixed-case column test.
- **R3 — hash/equality consistency (S6).** `equal ⟹ same hash` pinned by a unit test over
  same-leaf/different-source and different-leaf/same-source pairs.
- **R4 — plan churn.** Correcting matches can change chosen plans. Full suite + stress + determinism
  gate; any EXPLAIN change reviewed as correctness or neutral, never waved through.
- **R5 — scope of condition 3 (aggregate path plumbing).** If the correct group-key path proves
  larger than A, the transitional reject-nested guard ships *with* its test + a completion TODO
  (never terminal). Default is the full path fix.

---

## 7. Implementation order (DFS, one site to completion, green per commit, e2e per site)

0. §5.0 probes — establish reachability + baseline; commit as red/characterization tests.
1. `values.AccessorNamePath` + `ColumnNamePathsEqual` + unit tests (all 3 reps, pure-ordinal ok=false,
   case-norm, Cardinality/DistanceRowNumber descent).
2. **S1** (CRITICAL) — `valuesMatchColumn` → primitive; delete `distanceRowNumberValuesMatch`/
   `fieldNamesMatch`; §5.1.1 + R1 positive. Land fully first.
3. **S2/S3** ordered-scan/PK sort-key sites.
4. **S6** ordering-partition hash + structural tiebreak; §5.1.3 + R3.
5. **S4/S5/S8** aggregate candidate real-path (condition 3); §5.1.2 (both directions).
6. **S7/S9/S10** value↔value equivalences.
7. Full suite + determinism ×10 + 1M stress; milestone review lap (Graefe + Torvalds), fold, delta.

No site "done" until a SQL/FDB test exercises it e2e and pins corrected behavior (CLAUDE.md). No
`t.Skip`, no "for now", no deferral of A.

---

## 8. Tracked follow-up (Graefe condition 4) — ordinalize the candidate (RFC-173 endgame)

Option (2): resolve candidate `columnNames`→source-relative ordinals against the record type at
construction so both match sides are baked, unifying match-domain and evaluation-domain identity
under `FieldPath.Equals` (true Java parity). Larger, entangled with RFC-173 (also forces query
unqualified refs fully baked). **Explicitly recorded, not dropped** — booked in TODO.md as a
post-RFC-173 item. A ships name-path (the correct match-domain identity today); §8 later collapses
the two domains into one.
