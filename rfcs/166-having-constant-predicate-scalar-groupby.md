# RFC-166 — `HAVING FALSE`/`HAVING NULL` over a scalar aggregate returns 1 row instead of 0

Status: DRAFT (pending Graefe + Torvalds; codex on impl)
Area: Cascades query engine (predicate pushdown). Requires Graefe ACK on RFC + impl.
Source: bug-hunt finder (rules-agg/pushpull), 3-lens confirmed, empirically reproduced against FDB.

## 1. Summary
`SELECT COUNT(*) FROM t HAVING FALSE` returns **1 row `{0}`** instead of **0 rows** (Postgres/Java return 0).
Same for `HAVING NULL`. Root cause: `PushFilterThroughGroupByRule` pushes a constant row-eliminating
predicate **below** a scalar (zero-grouping-key) `GroupBy`, but a scalar aggregate emits one row even over
empty input — so the `COUNT(*)=0` row survives instead of being filtered out.

## 2. Java spec (the reference)
`PredicatePushDownRule.visitGroupByExpression` (fdb-record-layer-core .../rules/PredicatePushDownRule.java:396-400):
```java
public Optional<GroupByExpression> visitGroupByExpression(@Nonnull final GroupByExpression groupByExpression) {
    // We have to be a little careful here. In particular, we can push down any predicates on a
    // grouping column, but not any on the aggregate value. For now, just don't push anything down
    return Optional.empty();
}
```
Java pushes **nothing** through a GroupBy. It explicitly notes grouping-column predicates *would* be
safe to push, but aggregate-value predicates are not — and conservatively pushes nothing.

## 3. Go gap (root cause)
`pkg/recordlayer/query/plan/cascades/rule_push_filter_through_groupby.go`:
- `predicateReferencesOnlyKeys` returns **`true` for any `*ConstantPredicate`**:
  ```go
  cp, ok := p.(*predicates.ComparisonPredicate)
  if !ok {
      if _, isConst := p.(*predicates.ConstantPredicate); isConst {
          return true            // <-- BUG: a constant is treated as a "grouping-column" predicate
      }
      return false
  }
  ```
- The scalar-GroupBy guard (`len(groupKeySet)==0 && len(GetGroupingKeys())>0`) is **false** for a scalar
  GroupBy (both are 0), so the rule proceeds and pushes the constant. Result:
  `GroupBy([], aggs, Filter(FALSE, scan))` → empty input → scalar `aggregateCursor.emptyScalarResult`
  (`streaming_cursors.go:200-203`) still emits one row → `COUNT(*)=0` returned as a row.

This is a **distinct axis** from the already-fixed HAVING-pushdown bug (TODO.md "HAVING-PUSHDOWN", which was
the RHS-comparand axis). That fix guarded comparison predicates; this is the constant-predicate axis.

### Why it only bites the scalar case
For a non-scalar `GROUP BY g HAVING FALSE`, pushing FALSE below yields zero groups → 0 rows, and keeping it
above also yields 0 rows — same answer. Only the **scalar** (zero-grouping-key) aggregate emits a row on
empty input, so only there does below-vs-above diverge. And for a scalar GroupBy the `groupKeySet` is empty,
so a `ComparisonPredicate` can never satisfy `predicateReferencesOnlyKeys` — **only the `ConstantPredicate`
special-case slips through.** So fixing the constant case fully closes the scalar hole.

## 4. Fix — recommended Option B (surgical; keep the valid extension)
`Go's PushFilterThroughGroupByRule is a Go-only rule` (Java pushes nothing), but pushing a **grouping-column
equality** below a GroupBy is a correct, Java-acknowledged-valid read-side optimization (allowed per CLAUDE.md
"the read-side query surface MAY go beyond Java", with deep tests). The only incorrect behavior is treating a
**constant** as pushable.

**Fix:** drop the `ConstantPredicate → true` special case so a constant predicate is **never** considered a
grouping-column predicate (it references no grouping column). One change in `predicateReferencesOnlyKeys`:
```go
if !ok {
    return false   // not a comparison on a grouping column — incl. ConstantPredicate — keep above the GroupBy
}
```
A constant `HAVING FALSE/NULL` then stays above the (scalar) aggregate and correctly removes the
`COUNT(*)=0` row → 0 rows. `HAVING TRUE` constants stay above too and are dropped by `FilterDropTrueRule`
(no plan regression).

### Alternative Option A (strict 1:1 Java): push nothing through GroupBy
Delete/neuter the rule entirely to match `visitGroupByExpression → Optional.empty()`. Pros: exact parity,
removes a Go-only rule (Graefe: "Go-only rules are suspect"). Cons: regresses the grouping-column-filter
optimization and its plans/tests (`rule_push_filter_through_groupby_test.go`, `planner_agg_fuzz_test.go`,
`plangen/index_scan_e2e_test.go`); changes index selection for `GROUP BY g HAVING g=…`. **Graefe decides A vs
B.** Recommendation: **B** — keep the correct, tested optimization; remove only the incorrect constant push.

## 5. Test plan (red→green)
- yamsql/FDB probe (a test that actually runs — NOT the dead-skipped yamsql corpus, see bug-hunt finding):
  - `SELECT COUNT(*) FROM t HAVING FALSE` → **0 rows** (was 1 `{0}`).
  - `SELECT COUNT(*) FROM t HAVING NULL` → **0 rows**.
  - Control: `SELECT COUNT(*) FROM t HAVING 1=0` → already 0 rows (non-field LHS ⇒ ComparisonPredicate not
    pushed) — keep green.
  - Non-scalar control: `SELECT g, COUNT(*) FROM t GROUP BY g HAVING FALSE` → 0 rows (unchanged either way).
  - Optimization preserved (Option B): `SELECT g, COUNT(*) FROM t GROUP BY g HAVING g = 5` still pushes
    `g=5` below the GroupBy (EXPLAIN shows the filter under the aggregate) and returns the right rows.
- Unit: `rule_push_filter_through_groupby_test.go` — a `ConstantPredicate` is classified residual (not
  pushable); the rule yields nothing for `Filter(FALSE, ScalarGroupBy)`.
- **MUST invert the existing `TestPushFilterThroughGroupBy_ConstantPredPushes` (`:198-226`)** — it currently
  feeds a lone `ConstantPredicate(TriTrue)` and asserts the rule **fires**; Option B makes the constant
  residual so the rule yields nothing → that test would go red. Invert it to assert the constant is residual
  and the rule does NOT fire (Torvalds catch — it pinned the buggy behavior).

## 7. Review verdicts (design)
- **Graefe → ACK, Option B.** "Go-only rules are suspect" is a rebuttable presumption; Java's own comment
  concedes grouping-COLUMN pushdown is valid — keep the tested optimization, close only the constant hole.
  Option B fully closes the bug (scalar keySet empty ⇒ only the ConstantPredicate special-case leaked).
- **Torvalds → ACK, Option B**, with the mandatory test-inversion above (the fix's own suite would otherwise
  go red). No optimization lost: the only constant that benefited was `TRUE`, dropped by
  `FilterDropTruePredicatesRule` (`default_rules.go:45`) above the GroupBy regardless.

## 6. Risk
Option B is a strict reduction of what gets pushed (constants no longer pushed) → can only keep more filters
above the aggregate, which is always correctness-safe. No wire impact (read-side only). Run the agg fuzz +
the existing groupby pushdown tests; 1M stress not required (no index-selection change under Option B, since
constants were never a useful pushdown).
