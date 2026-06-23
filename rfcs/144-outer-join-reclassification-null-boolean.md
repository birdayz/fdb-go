# RFC-144 — Outer-join reclassification + 4.12 null/boolean fixes (Java 4.12, RFC-135 §4 R7)

**Status:** Implemented (pending review)

## Implementation summary (2026-06-23)

**TASK A — outer-join parity sweep (the centerpiece).** Ported all 53 Java
`join-tests-outer.yamsql` cases as FDB integration tests
(`pkg/relational/sqldriver/outer_join_parity_fdb_test.go`). The sweep found and
fixed **six real divergences** (the materialized-NLJ mechanism is kept, Graefe
ACK'd):
1. **`JOIN … USING (col)` ignored** → cartesian product. `extractJoinClause` now
   synthesizes the equi-join ON (`<leftAlias>.col = <rightAlias>.col AND …`,
   re-parsed via the new `parser.ParseExpression`), the left column qualified by
   the preceding source so it resolves on the left (Java's leftOperators).
2. **Chained outer joins** (`(a LEFT JOIN b) LEFT JOIN c`) dropped NULL-padded
   rows: `SelectMergeRule` flattened the nested outer-join box. Fixed — an
   outer-join `SelectExpression` is now a merge barrier (a child outer-join box
   is never absorbed, and an outer-join PARENT never merges any child).
3. **INNER-then-LEFT** dropped padding (same root cause: the outer-join parent
   absorbed the inner child's quantifiers). Same fix.
4. **Derived table on the right of an explicit JOIN** was unsupported and its ON
   predicate was dropped. `extractJoinClause` now handles `SubqueryTableItem`
   (sets `derivedQuery`), and `upgradeJoinOnPredicates` registers the derived
   scope source so the ON resolves.
5. **Derived-table PRIMARY source + JOIN** silently dropped the JOIN clauses
   (the `extractFromSimpleTable` derived-primary branch returned before parsing
   `AllJoinPart()`). Extracted `parseJoinClauses`, shared by both primary paths.
6. **RIGHT JOIN `SELECT *` column order** emitted the swapped (execution) order
   (`dept.*, emp.*`); now follows SQL declaration order (`emp.*, dept.*`) by
   building the join result value from the pre-swap legs.

**Reclassification:** LEFT/RIGHT OUTER JOIN is now **Java-aligned** (a shared
capability — Java 4.12 supports both). FULL OUTER stays a **documented Go-only
extension** (Java rejects FULL with SYNTAX_ERROR; Go's materialized-NLJ drain
implements it). Pinned in `TestFDB_OuterParity_FullIsGoExtension`.

**TASK B — EliminateNullOnEmptyRule.** Ported Java's `EliminateNullOnEmptyRule`
(#4186) as `rule_eliminate_null_on_empty.go` with the faithful `rejectsNull`
(BC1: substitute a typed NullValue at the alias → constant-fold → reject iff
FALSE/NULL; per-alias; the #4222 `NULL AND …` non-fold limitation respected;
the `CollapseNullStrictValueOverNullValueRule` collapse ported for the
substitution). DELETED `PullUpNullOnEmptyRule` + both `default_rules.go`
registrations. **BC2:** `ImplementSimpleSelectRule` now skips the outer MAP only
on an EXACT flowed-`Type.Equals` match (`isSimplePassthroughOf`), preserving the
MAP on a nullability-widening mismatch. Cascades-level tests in
`rule_eliminate_null_on_empty_test.go` (rejecting eliminates, accepting `IS NULL`
kept, per-alias, no-predicate). No SQL anti-join test (no `nullOnEmpty` SQL
producer — latent-rule hygiene, as framed).

**TASK C — null-supplying nullability (#4274).** Verified. Runtime rows correct;
INSERT…SELECT from an outer join into a NOT-NULL target rejects the NULL-padded
row (load-bearing). Two documented benign divergences: result-metadata
`ColumnTypeNullable` reports source cardinality (not re-typed nullable — a
non-load-bearing flag on a separate descriptor path), and the INSERT…SELECT
rejection surfaces as a proto "required field" error rather than a clean 23502
(a general INSERT…SELECT executor gap, orthogonal to outer joins). Pinned in
`TestFDB_OuterParity_NullSupplyingNullability` / `…InsertSelectFromOuterJoinNotNull`.

**TASK D — boolean literals (#4162).** Verified `WHERE TRUE/FALSE/NULL`,
`ON TRUE/FALSE/NULL`, `ON <expr> AND TRUE`, `ON <boolcol>`, `WHERE flag = TRUE`,
`WHERE flag IS TRUE` (all green). Found a divergence: a BARE boolean column as a
single-table top-level WHERE predicate (`WHERE flag`) does not plan in Go
(parser/resolver + the join-ON path handle it; the single-table-WHERE planner
leg does not). Java supports it. Documented + filed in TODO.md (Known gaps) —
orthogonal to outer joins, pre-existing.

**TASK E — NULL constant-folding (#4224).** Verified Go's runtime 3VL reproduces
the OBSERVABLE result of Java's NULL-operand folding +
`CollapseNullStrictValueOverNullValueRule` (rows identical); the global rule is a
plan-shape-only optimization, NOT ported as a global rule (benign divergence).
The collapse logic IS ported locally where it is load-bearing (TASK B's
rejectsNull). Pinned in `TestFDB_OuterParity_NullConstantFolding`.

---

**Original RFC status:** Draft
**Item:** RFC-135 §4 **R7** — Java 4.12 newly added LEFT/RIGHT OUTER JOIN (#4122) and a set of
null/boolean correctness fixes. Go already has LEFT/RIGHT/FULL OUTER JOIN (a Go-only extension predating
4.12), so the outer-join surface is now **Java-aligned, not Go-only** — "reclassify". But 4.12 also
shipped a real correctness fix Go is missing — `EliminateNullOnEmptyRule` replacing the buggy
`PullUpNullOnEmptyRule` (#4186) — plus null-supplying-side nullability (#4274), boolean literals in
WHERE/ON (#4162), and NULL constant-folding (#4224). R7 ports the genuine 4.12 deltas + reclassifies +
pins parity.
**Reviewers:** **Graefe** + Torvalds.

---

## 1. Problem (verified real)

Java 4.11→4.12 deltas in three areas (commits enumerated from `4.11.1.0..4.12.11.0`):

1. **OUTER JOIN is now a Java feature (#4122 `e95389d77`).** Java added LEFT/RIGHT OUTER JOIN via
   `OuterJoinExpression` (binary QGM box) → `RewriteOuterJoinRule` canonicalising into nested SELECTs
   joined by a `nullOnEmpty` quantifier; FULL is explicitly rejected. **Go already supports
   LEFT/RIGHT/FULL OUTER JOIN** — but via a DIFFERENT mechanism: a materialized
   `RecordQueryNestedLoopJoinPlan` with a `JoinType` flag (`JoinLeftOuter`/`JoinFullOuter`),
   NULL-padding the non-matching side by KEY ABSENCE at runtime (not a `nullOnEmpty`-quantifier rewrite).
   The two are **functionally equivalent** (same result rows). So R7's "reclassification" is real: Go's
   outer-join extension is now Java-aligned (Java 4.12 supports LEFT/RIGHT; Go's FULL stays a documented
   Go-only extension since Java 4.12 still rejects FULL).
2. **The buggy `PullUpNullOnEmptyRule` (#4186 `eb059659e`) — LATENT-RULE hygiene (NOT SQL-reachable).**
   Go has a `PullUpNullOnEmptyRule` (`rule_pull_up_null_on_empty.go`, registered twice in
   `default_rules.go:138,158`) ported from the OLD Java rule. Java 4.12 DELETED it and shipped
   `EliminateNullOnEmptyRule` because PullUp is **incorrect** with predicates that *accept* the null tuple
   a `nullOnEmpty` quantifier injects (`… WHERE x IS NULL` over a null-on-empty leg) — PullUp assumes the
   null tuple is always rejected. **Reachability (empirically resolved — Torvalds caught the original
   draft over-claiming this):** `ForEachNullOnEmptyQuantifier(` has **NO production call site** in Go
   (confirmed: only `rule_select_merge_test.go`, `rule_predicate_push_down_test.go`,
   `rule_implement_simple_select_test.go` construct it). Outer joins use the materialized NLJ; the
   scalar-subquery / FirstOrDefault path uses a `firstOrDefault` streaming **Value** (R4's
   `RecordQueryFirstOrDefaultPlan`), **not** a `nullOnEmpty` quantifier. So PullUp (and the `IsNullOnEmpty`
   branches in `rule_implement_simple_select.go` / `rule_predicate_push_down.go` / `rule_select_merge.go`)
   is **dead on the SQL path** — fires only on synthetic cascades-rule-test inputs. R7 still ports
   `EliminateNullOnEmptyRule` (replacing the buggy PullUp) as **Java-parity / latent-rule hygiene** —
   pinned by a CASCADES-LEVEL rule test (port Java's `EliminateNullOnEmptyRule` test), NOT a SQL-visible
   anti-join regression (which cannot be written — there is no SQL producer). This is the honest framing.
3. **Null-supplying-side nullability (#4274 `32e0a690d`).** Java makes the null-supplying side's flowed
   values nullable in the result value (so NULL-padding can't raise "non-nullable field set to NULL"). Go
   propagates the null-supplying columns' types AS-IS (`value_anchored_join_record.go`
   `NewFieldValue(qov, c.Name, c.FieldType)`), relying on runtime key-absence → NULL. Candidate
   divergence: a downstream consumer that *checks* nullability (e.g. INSERT…SELECT from an outer join
   into a NOT-NULL target, or column metadata) may diverge from Java.
4. **Boolean literals in WHERE/ON (#4162 `66df5b523`).** Java extended `toUnderlyingPredicate` to accept
   `WHERE TRUE/FALSE/NULL`, `ON TRUE`, a bare boolean expr. **Go already accepts these** (`walk.go`:
   `BooleanValue` → `ConstantPredicate(TriTrue/False)`, else `ValuePredicate`, `NullValue` →
   ValuePredicate). Likely already aligned — verify + pin (the conformance bump already "lifted"
   `where_case_returns_bool_probe` / `bare_bool_where_rejected`).
5. **NULL constant-folding (#4224 `724f42a1e` + `CollapseNullStrictValueOverNullValueRule`).** Java folds
   comparisons with a NULL operand → NullValue (3VL "reject"), and collapses a null-strict Value over a
   NullValue child to NullValue. Go has distributed folding (`SimplifyPredicateValues`) but **no
   `CollapseNullStrictValueOverNullValueRule` analog**. Verify whether the difference is OBSERVABLE (Go's
   runtime 3VL eval may already produce the same rows; the rule is a plan-shape optimisation) — port only
   if it changes results or a pinned plan shape.

## 2. Investigation (Java mechanism — key commits)

`#4122` OuterJoinExpression + RewriteOuterJoinRule + RewritingCostModel penalty; `#4274` nullable
null-supplying flow; `#4186` EliminateNullOnEmptyRule (analyses which predicates provably REJECT the null
tuple at a quantifier alias; only eliminates `nullOnEmpty` for those; leaves null-accepting siblings) +
the `ImplementSimpleSelectRule` passthrough-type tightening (skip the outer MAP only on an EXACT flowed-
type match, preserve it when nullability widens); `#4162` boolean/NULL literals as top-level predicates;
`#4224`/`f8fc62a75` NULL folding + the null-strict-collapse rule (ordered first in the value-simplifier
set). `#4272` grammar (LEFT/RIGHT as reserved-in-join) — **already done in Go R3 (RFC-140)**.

Go has: the materialized outer-join NLJ (`nested_loop_join.go`, `streaming_cursors.go`); the SQL
LEFT/RIGHT/FULL → JoinType lowering (`select_parser.go` `extractJoinClause`, `cascades_translator.go`
`translateJoin`); `nullOnEmpty` quantifiers + the buggy `PullUpNullOnEmptyRule`; boolean/NULL-literal
WHERE/ON acceptance; distributed predicate folding. Go does NOT have: `EliminateNullOnEmptyRule`; the
null-supplying nullability re-typing; `CollapseNullStrictValueOverNullValueRule`.

## 3. Fix

### 3a. Outer-join reclassification (Graefe decision — KEEP Go's mechanism)
Go's materialized-NLJ outer join is functionally equivalent to Java 4.12's OuterJoinExpression+nullOnEmpty
(same rows). Re-architecting onto `OuterJoinExpression`+`RewriteOuterJoinRule` is a large rewrite for ZERO
behavioral gain — and Graefe already ACK'd `JoinFullOuter` as a Go extension. **Proposal: keep the
materialized mechanism; do NOT port OuterJoinExpression.** Reclassify: LEFT/RIGHT OUTER JOIN is now
Java-aligned (a shared capability, not a Go-only extension); FULL stays Go-only (Java still rejects FULL).
The conformance entries lifted in the bump (`left_outer_join_basic`) are confirmed Java-now-supports.
Graefe must ACK keeping the materialized mechanism vs porting the QGM box.

### 3b. Port `EliminateNullOnEmptyRule`, replacing `PullUpNullOnEmptyRule` (LATENT-RULE HYGIENE)
**Framing (honest):** `nullOnEmpty` has no SQL producer in Go today (§1.2), so this is Java-parity /
latent-rule hygiene — replace the buggy rule with the correct one so that IF a `nullOnEmpty` producer is
wired later (or a synthetic/cascades path hits it) it is correct, and Go matches Java's rule set. Pin
with a **cascades-level** rule test, NOT a SQL anti-join regression (which is unwritable — no producer).
Port Java's `EliminateNullOnEmptyRule` (#4186): for a SELECT with `nullOnEmpty` quantifier(s), analyse
which predicates PROVABLY REJECT the null tuple at the quantifier's alias; only eliminate `nullOnEmpty`
for those; leave null-accepting siblings (`… IS NULL`). Delete `PullUpNullOnEmptyRule` (both
`default_rules.go` registrations).
- **Binding condition 1 (Graefe) — faithful `rejectsNull`:** the null-rejection test must be Java's exact
  mechanism (`EliminateNullOnEmptyRule.java:81-99`): substitute a typed `NullValue` at the quantifier
  alias, run constant-fold simplification, `rejectsNull ≡ result ∈ {FALSE, NULL}`. `anyMatch` over the
  SELECT's top-level (AND-flattened) predicates, per-alias; leave accepting siblings untouched. Do NOT
  fold `NULL AND …` (the #4222 limitation — rely on top-level AND flattening, not deep boolean folding).
  This replaces PullUp's positional-predicate-equality heuristic.
- **Binding condition 2 (Graefe) — `ImplementSimpleSelectRule` tightening is REQUIRED, not optional:** the
  passthrough must skip the outer MAP only on an EXACT `Type.equals` flowed-type match, and PRESERVE the
  MAP on a nullability-widening mismatch. Go's `rule_implement_simple_select.go:66` currently keys the
  skip on `IsNullOnEmpty()` (not type equality) — that gap drops the widening MAP. Fix to key on exact
  type equality. (Also latent today, since no SQL producer — port for parity + correctness.)
- **Regression bar:** a CASCADES-level exploration-rule test (synthetic SELECT with a `nullOnEmpty`
  quantifier + a null-accepting `IS NULL` predicate) is red on the buggy PullUp (wrongly eliminates) and
  green on Eliminate (correctly keeps it); the synthetic null-on-empty rule-test suite stays correct.

### 3c. Null-supplying-side nullability (#4274) — verify + fix if divergent
Verify whether Go diverges from Java on outer-join result nullability: does Go report the null-supplying
side's columns as nullable in the result metadata, and does INSERT…SELECT from an outer join into a
NOT-NULL target behave like Java? If Go diverges (the as-is type propagation in `NewAnchoredJoinRecord`),
re-type the null-supplying leg's columns nullable in the join result value (Java's `wrapOperandsForOuterJoin`).
If the runtime key-absence model already produces identical observable behavior (rows + metadata), keep it
and document.

### 3d. Boolean literals in WHERE/ON (#4162) — verify parity + pin
Go already accepts `WHERE TRUE/FALSE/NULL`, `ON TRUE/FALSE/NULL`, bare-boolean-expr WHERE/ON. Verify each
produces the SAME result as Java 4.12; add the missing tests (esp. boolean ON predicates — `ON TRUE`,
`ON a.id=b.id AND TRUE`, `ON <boolcol>`). No code change expected unless a divergence surfaces.

### 3e. NULL constant-folding (#4224) — port only if observable
Verify whether Java's `CollapseNullStrictValueOverNullValueRule` + the NULL-operand comparison folding
change observable results / a pinned plan shape in Go. Go's runtime 3VL eval likely already yields the
same rows; if so, this is a plan-shape-only optimisation — port the rule for plan parity only if a
conformance entry or EXPLAIN pin requires it, else document as a benign plan-shape divergence.

## 4. Performance

`EliminateNullOnEmptyRule` is strictly better than the buggy PullUp on the cases PullUp got wrong (correct
results) and equivalent elsewhere; it's an exploration rule (no runtime cost). The nullability re-typing
(if needed) is metadata-only. No cost-model surface beyond what the existing outer-join NLJ already costs.

## 5. Test plan

- **Port `join-tests-outer.yamsql`** (Java's 55+ cases) as FDB integration tests: LEFT/RIGHT OUTER basic,
  anti-join (`WHERE col IS NULL`), ON-vs-WHERE placement, predicate push-down, constant ON (`ON TRUE`,
  `ON 1=1`), compound ON, LEFT JOIN + ORDER BY, GROUP BY/COUNT over outer join, chained/nested outer
  joins, non-equi ON. Each asserts exact rows vs Java semantics.
- **The `EliminateNullOnEmptyRule` regression (3b) — CASCADES-LEVEL, not SQL:** a unit/exploration-rule
  test (synthetic SELECT with a `nullOnEmpty` quantifier + a null-ACCEPTING `IS NULL` predicate) — red on
  the buggy PullUp (eliminates wrongly), green on Eliminate (keeps it); a null-REJECTING predicate
  eliminates correctly under both. NO SQL anti-join test (no `nullOnEmpty` SQL producer exists). The
  synthetic null-on-empty rule-test suite stays green after the swap. (The SQL-visible centerpiece is the
  outer-join parity sweep above, not this rule.)
- **Nullability (3c):** outer-join result column metadata nullable; INSERT…SELECT from an outer join.
- **Boolean ON/WHERE (3d):** `ON TRUE/FALSE/NULL`, `ON <expr> AND TRUE`, `WHERE TRUE/FALSE/NULL`.
- **NULL folding (3e):** if ported, the folded plan shape / result.
- Determinism 10×; FULL-OUTER + the existing `full_outer_join_fdb_test.go` stay green; reclassify the
  conformance entries (coordinate with R8).

RFC-144 implements RFC-135 §4 R7. Graefe (the keep-materialized-mechanism + EliminateNullOnEmptyRule
decisions) + Torvalds gate. NOTE: the final codex pass is deferred to the Jun 25 quota reset; Graefe +
Torvalds gate the build in the interim (PR stays draft for codex + @claude).
