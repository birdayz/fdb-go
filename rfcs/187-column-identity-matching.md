# RFC-187 — Alias-aware column identity in index/aggregate matching (kill leaf-name column matching)

**Status:** DRAFT — awaiting Graefe (Cascades alignment) + Torvalds (code quality) ACK before implementation.
**Tracks:** TODO.md "FINDINGS 2026-07-22" item **A** (systemic leaf-name column matching), which
subsumes finding 1 (CRITICAL, wrong rows) and finding 4 (wrong ORDER BY).
**Baseline:** master at `139060801`. All citations resolve against that.
**Relationship to RFC-173:** this is the *planner-side* analog of the RFC-173 fight — stop
resolving columns by leaf name — on the index-**match** path (candidate ↔ query column identity),
distinct from RFC-173's executor/result-row resolution. No overlap in files; same principle.

---

## 1. The defect class

The planner decides "does this query column reference the same field as this index/candidate
column?" by comparing the **leaf field name only**, discarding the `FieldValue` accessor chain
(`Child`) and the baked ordinal path (`Resolved`). When a nested path's leaf name collides with a
differently-rooted column of the same source, the planner binds/matches the **wrong column**.

This is a direct violation of the CLAUDE.md prime directive: *"NEVER detect SQL features by
string-matching … fix the resolution infrastructure — don't strip qualifiers with string hacks."*
The precedent fix `buildTranslateValueFunction` (`match_candidate_index.go:405-448`) was hardened
against exactly this fused-multi-accessor collision ("if a covered column shares the leaf name,
silently read the WRONG slot (wrong rows)"). The match-**binding** path and its siblings were not.

### 1.1 The `FieldValue` representation (why leaf name is insufficient)

`values.FieldValue` (`values/values.go:187-243`) carries a *whole accessor path*, in one of two forms:

- **Lazy** (`Resolved == nil`): a chain of nodes. `t.addr.city` =
  `FieldValue{Field:"city", Child: FieldValue{Field:"addr", Child: QOV(src)}}`. `Field` is only
  the **last** accessor. `Children()` returns `[Child]`.
- **Baked / fused** (`Resolved != nil`): one node holding an ordinal `FieldPath`. The compose rule
  fuses `t.addr.city` into `FieldValue{Field:"CITY", Child: QOV(src), Resolved:[ADDR,CITY]}`
  (`match_candidate_index.go:422-431`). `Field` is display-only; identity is the ordinal path.

`EqualsWithoutChildren` (`values/map_field_values.go:313-338`) already encodes the correct
per-node identity: baked → `Resolved.Equals` (ordinal-list, Java `FieldValue.java:411-420`), lazy
→ `Field == Field`, baked-vs-lazy → unequal. Combined with `Children() = [Child]`, the canonical
`SemanticEqualsUnderAliasMap` (`values/semantic_equals.go:17`) already compares the **full accessor
path modulo an alias map** — this is Go's `value.semanticEquals(other, aliasMap)`. **The primitive
is correct; the matching sites bypass it.**

### 1.2 The sites (all leaf-name shortcuts)

| # | Site | Sides compared | Effect |
|---|------|----------------|--------|
| S1 | `rule_match_intermediate.go:755` `valuesMatchColumn` FieldValue arm | query `Value` ↔ placeholder `Value` | **CRITICAL, wrong rows** — nested `WHERE addr.city='x'` sargs top-level `city` index, marked matched → no residual re-check |
| S2 | `rule_match_intermediate.go:765-776` Cardinality/DistanceRowNumber arms | `Value` ↔ `Value` | same leaf-name shortcut on wrapped fields |
| S3 | `aggregate_index_candidate.go:129,151,157,175,198,204` `MatchesGroupBy`/`MatchesSingleAggregateOf` | query `FieldValue` ↔ index-column **string** | `GROUP BY addr.city` / `COUNT(addr.city)` matches top-level `city` agg index → aggregates wrong column |
| S4 | `rule_aggregate_data_access.go:195` `groupColEqualityIndex` | query `FieldValue` ↔ string | group-key `WHERE addr.city='x'` → AISCAN bound on top-level `city` |
| S5 | `expression_partition.go:254-277` `orderingPartitionHash` + `:203-243` `toPartitionsFromMap` | ordering-key `Value` hash | **wrong ORDER BY** — same-leaf-name orderings from different sources collapse into one partition, sort elided |
| S6 | `rule_push_filter_through_groupby.go:107,135` + `rule_streaming_agg_from_index.go:80` | leaf-name membership | pushdown eligibility misclassified (documented "at worst leaks a duplicate") |

### 1.3 Why the shortcut was introduced (and why it's the wrong fix)

`valuesMatchColumn`'s comment (`:748-763`) states the reason: *"the alias map is built only after
binding."* True — but the alias correspondence it needs is **trivially known at the call site**:
the query column is guaranteed (by the `colCorr ⊆ sourceAlias` guard, `:631-636`) to be over
`sourceAlias`; the placeholder is over the candidate's base alias. A one-entry `AliasMap`
`{sourceAlias → placeholderRoot}` bridges them. The shortcut traded a known 1-entry map for
dropping the entire accessor chain — a false economy that costs correctness.

---

## 2. Root cause

There is **no single alias-aware column-identity primitive used across the match path**. Each
site hand-rolls a leaf-name comparison. The canonical primitive (`SemanticEqualsUnderAliasMap`)
exists and is correct, but the FieldValue-vs-`Value` sites don't call it, and the
FieldValue-vs-`string` sites (aggregate) can't (the candidate exposes only bare leaf strings).

Java's mechanism is uniform: candidate placeholders and query values are both `Value` trees, and
matching compares them with `value.semanticEquals(otherValue, boundAliasMap)`
(`Placeholder` / `PredicateWithValueAndRanges` in `AbstractDataAccessRule`). There is no
leaf-name compare anywhere in Java's match path — nested vs top-level is distinguished by the
`FieldPath` length/ordinals, structurally.

---

## 3. Design

**One principle: match-time column identity is the full accessor path compared alias-invariantly
at the root — never the leaf name.** Concretely:

### 3.1 FieldValue-vs-`Value` sites (S1, S2, S5) → the canonical primitive

Replace the leaf-name / name-only arms with `SemanticEqualsUnderAliasMap` supplied a minimal
root-alias bridge built at the call site.

- **S1/S2 `valuesMatchColumn`.** Rewrite as:
  ```
  // queryValue is guaranteed correlated to sourceAlias (caller guard);
  // placeholderValue is over the candidate base.
  aliases := values.AliasMap{sourceAlias: placeholderRoot(placeholderValue)}
  return values.SemanticEqualsUnderAliasMap(queryValue, placeholderValue, aliases)
  ```
  `placeholderRoot` extracts the single root correlation of the placeholder
  (`GetCorrelatedToOfValue`, expected size 1; if ≠1 fall back to structural-equality-only, i.e.
  the empty map, which is still *correct* — it just won't bridge aliases). This one call subsumes
  the FieldValue, Cardinality, and DistanceRowNumber arms: `SemanticEqualsUnderAliasMap` recurses
  structurally through `CardinalityValue.Child` and `DistanceRowNumberValue`'s partition/argument
  values under the same map, so the three bespoke name-only helpers
  (`distanceRowNumberValuesMatch`, `fieldNamesMatch`, the Cardinality arm) are **deleted**, not
  patched. This is the sargability decision, so it must stay conservative: when in doubt (roots
  not both single, representations mismatched) it returns *false* → the predicate stays a residual
  filter → correct rows via the slower path (never wrong rows).

  `valuesMatchColumn` currently gains its `queryValue` sourceAlias as a NEW parameter (today it
  only receives the two values); the call site at `:649` has `sourceAlias` in scope.

- **S5 `orderingPartitionHash` + `toPartitionsFromMap`.** Two coupled fixes:
  1. **Hash the full accessor NAME path**, not `fv.Field`. Walk the `Child` chain (lazy) or read
     `Resolved.Accessors[].Field` (baked) to emit every accessor name, root-alias-excluded (the
     existing reason for name-based hashing — dodging minted `q$N` nondeterminism in
     `ExplainValue` — is preserved because accessor *names* are deterministic, unlike aliases).
     For pure-ordinal baked accessors (`Field == ""`) emit the ordinal. This keeps the hash
     alias-invariant AND collision-free.
  2. **Add a structural-equality tiebreak** in `toPartitionsFromMap` mirroring the sibling
     `RollUpPlanPartitions` path (`orderingsEqual` → `ValuesStructurallyEqual`, `:367-372`): two
     orderings sharing a hash bucket must additionally pass structural equality to land in the
     same partition. This makes S5 collision-*safe*, not merely collision-*unlikely* — the hash is
     an optimization, equality is the authority (the standard hash-map contract the hash-only code
     violated).

### 3.2 FieldValue-vs-`string` sites (S3, S4) → compare the full path, reject nested-vs-flat

The aggregate candidate exposes columns as bare leaf strings (`groupCols []string`,
`aggColumn string`). A query `FieldValue` for a **nested** path must not match a **flat** index
column of the same leaf name. Two options, in preference order:

- **Preferred (principled): the candidate exposes accessor paths.** Give
  `AggregateIndexMatchCandidate` the columns' full accessor identity (the index key expression is
  already a nested `KeyExpression` in the metadata — the flattening to `[]string` is where the
  path is lost). Compare the query `FieldValue`'s full path against the candidate column's path.
  This also fixes *nested aggregate indexes* (`COUNT(addr.city)`), which the flat-string model
  cannot represent today.
- **Minimum-correct (if the metadata plumbing is out of this RFC's scope): reject non-flat query
  FieldValues.** A grouping-key / aggregate-operand `FieldValue` matches a flat index column only
  when it is a **single top-level accessor over the base** — `Child` is the base `QOV` (not a
  FieldValue chain) and, if baked, `Resolved.Single()` holds. A nested path (`addr.city`) is
  rejected → no false aggregate-index match → correct rows via the general aggregation path.

  The implementation will determine which is reachable: **if index metadata can define aggregate
  indexes on nested columns, the Preferred option is mandatory** (else we'd lose a real match).
  The RFC commits to the Preferred option unless the implementation proves nested aggregate index
  columns are structurally impossible here, in which case the Minimum-correct guard ships with a
  test pinning that a nested grouping key does not match a flat agg index. **No leaf-name compare
  survives either way.**

### 3.3 S6 pushdown eligibility

`rule_push_filter_through_groupby.go` / `rule_streaming_agg_from_index.go` decide whether a
predicate references only the grouping keys. Same fix shape as S3: compare the predicate field's
full accessor path against the grouping key's full path (via `SemanticEqualsUnderAliasMap` on the
`Value`s, which are available here), not `EqualFold(fv.Field, …)`. These are documented "at worst
leaks a duplicate" today, so they are lower risk, but they take the same primitive for consistency
— one column-identity notion across the engine, no second authority.

### 3.4 The shared primitive

All FieldValue-vs-`Value` sites route through **one** exported helper (proposed
`values.ColumnValuesMatch(query, candidate Value, aliases AliasMap) bool` — a thin, documented
wrapper over `SemanticEqualsUnderAliasMap`, or the raw primitive if a wrapper adds nothing). No
site re-implements accessor-path comparison. This is the "fix the resolution infrastructure"
mandate: the infrastructure is the primitive; the sites call it.

---

## 4. What this is NOT

- **Not a wire-format change.** Index entry format, key encoding, continuations — untouched. This
  changes only *which candidate the planner binds*, i.e. plan selection on the read path.
- **Not the RFC-173 executor migration.** RFC-173 ordinalizes result-row resolution; this
  ordinalizes/path-compares *match-time candidate identity*. Disjoint files.
- **Not a new Cascades rule or phase change.** No rule added/removed/reordered; the match
  algorithm's *comparison step* is corrected in place. (Graefe: this is a fidelity fix to the
  existing match-then-implement step, not a structural change.)

---

## 5. Test plan (every site gets a red→green regression on the unprobed dimension)

The dimension that let these through is **"nested path leaf name collides with a differently-rooted
same-source column."** Every fix pins that axis:

1. **S1 (CRITICAL) — `TestFDB_NestedFieldShadowsTopLevelIndex`**: table with nested `addr.city`
   AND a top-level `city` column with a value index on `city`. `SELECT * FROM t WHERE
   addr.city = 'NYC'` must return only rows whose *nested* `addr.city='NYC'` (not top-level
   `city='NYC'`). Assert both rows AND plan shape (the `city` index must NOT be sargable-bound;
   `addr.city` stays a residual filter or uses a correct nested index if one exists). Red on
   baseline, green after.
2. **S3/S4 — `TestFDB_AggregateIndex_NestedGroupKeyNoFalseMatch`**: agg index on top-level `city`;
   `GROUP BY addr.city` must NOT use it (falls back to general aggregation, correct groups).
3. **S5 — `TestOrderingPartition_SameLeafDifferentSourceNoCollapse`** (unit) +
   `TestFDB_OrderBy_SelfJoinSameLeafName` (FDB): a self-join exposing `x` from both legs with
   `ORDER BY` on one leg's `x` must not elide the sort against the other leg's `x`. Assert output
   order.
4. **S2 — Cardinality/DistanceRowNumber**: a nested-field `CARDINALITY(addr.arr)` / vector
   distance over a nested field must not collide with a top-level same-named index. (Vector pins
   coordinate with the existing `TestVectorPlan_*` sentinels.)
5. **Non-regression (matches must still fire): existing green suite** — the flat-column matches
   (`WHERE city='x'` on a `city` index) must still bind. The full `sqldriver`, `recordlayer`, and
   yamsql suites are the authority; `just test` green is the gate.
6. **Determinism**: the affected planner tests run 10× (skill §2 loop) — path-comparison must not
   introduce plan nondeterminism.
7. **Stress**: 1M stress before/after (planner change) — no row-count or plan regression.

---

## 6. Risks & mitigations

- **R1 — switching leaf-name → full-path turns some *current* matches into non-matches.** If those
  current matches were *correct* (flat-vs-flat), the path comparison still matches them (same
  path). If they were *wrong* (nested-vs-flat), turning them off is the fix. The risk is a
  *representation mismatch at match time*: query column baked while candidate placeholder lazy (or
  vice versa) → `SemanticEqualsUnderAliasMap` returns false (baked-vs-lazy contract) → missed match
  → **missed optimization, not wrong rows** (residual-filter fallback). **Mitigation:** the
  implementation must first *establish the representation contract at each call site* (are
  match-time query columns and placeholders consistently lazy? the `FieldValue` doc says candidate
  columns are lazy — confirm the query side) and, if a mixed representation is real, add a
  name-path normalization so identity is bake-invariant at match time. This is the single most
  important implementation validation — it will be called out explicitly for the reviewers.
- **R2 — S3 nested aggregate index columns.** Covered by §3.2: Preferred option handles them;
  Minimum-correct option is only taken if they're structurally impossible (proven by test).
- **R3 — hash/equality consistency (S5).** The new path-hash must satisfy
  `equal ⟹ same hash` with the structural equality tiebreak. Pinned by a unit test asserting the
  invariant over same-leaf/different-source and different-leaf/same-source pairs.
- **R4 — plan churn.** Correcting matches can change chosen plans on unrelated queries. The full
  suite + stress + determinism gate catches regressions; any EXPLAIN change is reviewed as either
  a correctness fix or a neutral shape change, never waved through.

---

## 7. Implementation order (DFS, one site to completion each, green per commit)

1. Introduce the shared primitive (`ColumnValuesMatch` or direct use) + its unit tests.
2. **S1** (CRITICAL) — `valuesMatchColumn` → primitive; establish/validate the representation
   contract (R1); regression test #1. This is the headline; land it first, fully.
3. **S2** — fold Cardinality/DistanceRowNumber into the primitive; delete the bespoke helpers.
4. **S5** — ordering-partition hash full-path + `toPartitionsFromMap` structural tiebreak; tests
   #3, R3.
5. **S3/S4** — aggregate candidate path comparison (Preferred or Minimum-correct per §3.2);
   test #2.
6. **S6** — pushdown eligibility onto the primitive.
7. Full suite + determinism (10×) + 1M stress; milestone review lap (Graefe + Torvalds), fold,
   delta re-confirm.

No site is "done" until a SQL/FDB test exercises it end-to-end and pins the corrected behavior
(CLAUDE.md e2e rule). No `t.Skip`, no "for now", no deferral — DFS each site to completion.
