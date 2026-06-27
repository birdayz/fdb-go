# RFC-152 — Cost-model materialization for the LEFT-OUTER rewrite (fix the preserved-only re-scan regression, Java-faithful)

**Status:** Draft — needs a Graefe ACK on the design before implementation (query-engine cost model).

**Origin:** codex review of PR #364 (RFC-150 Phase-2b, the `tryFlatMapPlan` retirement) flagged a P2 perf
regression in `RewriteOuterJoinRule`. This RFC is the Java-grounded fix. It blocks #364's codex ACK.

## 1. The finding

`SELECT a.id FROM a LEFT JOIN b ON a.flag = 1` (an ON-predicate that references **only** the preserved
leg `a`, not the null-supplying leg `b`):

- `RewriteOuterJoinRule` (Go, `rule_rewrite_outer_join.go`) rewrites it into
  `FlatMap(outer=Scan(a), inner=DefaultOnEmpty(PredicatesFilter(Scan(b), [a.flag=1])))`.
- That inner cannot be **probed** by `a` (the predicate `a.flag=1` doesn't constrain `b`), so the FlatMap
  **re-scans the entire `b` from FDB once per `a` row** — O(N) FDB scans of `b`.
- Go's pre-RFC-150 **materialized** LEFT-OUTER NLJ scans `b` from FDB **once** and iterates it in memory.
- The cost model **picks the re-scan FlatMap over the materialized NLJ** → a perf regression vs master for
  this query shape. Rows are correct either way.

## 2. What Java does (the reference)

Read `rules/RewriteOuterJoinRule.java` (4.12.11.0):

- The rule matches **every** `OuterJoinExpression` (`outerJoinExpression().where(isExploratoryExpression())`,
  :71-72) and rewrites it **unconditionally**. `buildInnerSelect` (:121-147) pushes **all** ON-predicates
  into the null-supplying SUBSEL with **no filtering** by which leg they reference. There is **no
  "correlated", no "cross-leg", no "preserved-leg" guard anywhere.**
- Correctness comes from the `forEachWithNullOnEmpty` quantifier (:97-98): it always emits a NULL row when
  the inner is empty, so an uncorrelated/preserved-only inner still null-extends correctly. Java never
  "degrades to INNER".
- Perf comes from the **cost model**: Java canonicalizes the `OuterJoinExpression` away (keeps no
  materialized-NLJ alternative) and lets the cost model choose among the FlatMap implementations.

**Consequences:**

1. Go's `correlated` guard (and the cross-leg refinement codex suggested) are **Go-only band-aids Java does
   not have.** They exist only because Go's representation differs (flag on a `SelectExpression`, no
   `OuterJoinExpression`) and Go's rewrite has gaps Java's `nullOnEmpty` closes structurally.
2. Java would **itself** re-scan `b` per row for `a LEFT JOIN b ON a.flag=1` (it keeps no materialized
   alternative). So codex's "regression vs the materialized LEFT JOIN" is a regression vs **Go's own
   RFC-144 materialized NLJ — a Go-only optimization Java lacks**, not vs Java. Go is *better* than Java here.
3. The honest framing of the desired Go behavior: **keep Go's better-than-Java materialized NLJ for outer
   joins where the rewrite buys nothing, and let the rewrite's FlatMap win only when it enables a probe** —
   decided by the **cost model**, exactly as Java decides among its implementations. This is an allowed
   read-path improvement ("query reach may exceed Java"), not a wire concern.

## 3. Root cause: the cost model cannot tell materialize-once from re-scan-per-row

`cost_formulas.go`:

```go
func nestedLoopJoinCost(outer, inner properties.Cost) properties.Cost {
    // CPU: outer.CPU + outerCard*inner.CPU + outerCard*innerCard*FilterCPU
}
func flatMapCost(outer, inner properties.Cost) properties.Cost {
    // CPU: outer.CPU + outerCard*innerCPU
}
```

Both charge the inner **`outerCard × inner.CPU`** — i.e. both model the inner as **re-evaluated N times**.
So a materialized NLJ (whose executor scans `b` **once** and iterates the buffered rows) and a re-scan
FlatMap (which re-scans `b` from FDB N times) come out at **the same cost**. The model has no term for
"the inner is materialized once, then iterated cheaply from memory", so it cannot prefer the materialized
plan — and a tiebreak picks the re-scan FlatMap.

**Verify-first (the investigation the implementer must do before coding):**
- Confirm Go's materialized LEFT-OUTER NLJ **executor** actually scans the inner **once** (materializes /
  buffers), not re-scans. If it re-scans too, there is no real perf difference and the premise is wrong —
  revisit with Graefe. (Inspect `RecordQueryNestedLoopJoinPlan` execution vs `RecordQueryFlatMapPlan`.)
- Confirm against Java's cost model how it distinguishes a per-row inner re-execution from a buffered/
  materialized one (or whether it relies purely on cardinality + a fetch/IO term). Mirror that.

## 4. Design (Java-faithful — needs Graefe ACK)

1. **Keep the rewrite Java-faithful.** Do NOT add a cross-leg / probe-detecting guard to
   `RewriteOuterJoinRule` (Java has none). The existing `correlated` guard is retained **only** for the
   genuine Go correctness gap (an uncorrelated rewritten inner that would degrade to INNER); if that gap is
   itself closable so `nullOnEmpty` always null-extends like Java, prefer removing the guard entirely and
   relying on the cost model — Graefe to decide. The rewrite must still *produce* the FlatMap candidate for
   preserved-only ON-predicates; the **cost model**, not a rule guard, decides it loses.
2. **Model materialization in the cost.** Make `nestedLoopJoinCost` (the materialized NLJ) charge the inner
   **scanned once** (`inner.CPU` + `outerCard × innerCard × iterateCPU`) rather than `outerCard × inner.CPU`,
   so a materialized NLJ with a non-probe inner is **strictly cheaper** than the re-scan FlatMap
   (`outerCard × inner.CPU`, full re-scan). For a **probe** inner (card-1 correlated `Scan(b,[fk=a.id])`),
   the FlatMap stays cheapest (its `outerCard × 1` beats materializing + iterating). Net: cost model picks
   materialized NLJ for preserved-only, probe FlatMap for cross-leg — the correct plan for each, with no
   rule-level heuristic. Exact formula + constants are Graefe's call (RFC-041/042 cost surface; reuse the
   existing `IterationOverhead` discipline).
3. **No new guard, no string heuristics.** The whole point is that the cost model — the same authority that
   chose the multiway index-nested-loop in RFC-150 — makes this call too.

## 5. Open questions for Graefe

- Does the Go materialized-NLJ executor truly materialize the inner once? (§3 verify-first.) If not, the
  fix changes shape.
- The exact materialization cost term + constants, and whether it can regress any *other* join shape
  (this touches `nestedLoopJoinCost`, used by every NLJ). plandiff is the gate.
- Keep the `correlated` guard (for the uncorrelated→INNER gap) or close that gap and remove the guard
  (fully Java-faithful always-fire)?

## 6. Test plan — typed, as Java tests (NO `strings.Contains` on EXPLAIN)

- **Rule-level:** a cascades unit test via `FireExpressionRule(NewRewriteOuterJoinRule(), ref)` (the existing
  `rule_partition_binary_select_test.go` harness) — assert the rule still **yields** the rewritten
  `*SelectExpression` with a `nullOnEmpty` quantifier (typed structural assertions on the yielded
  expression, not plan strings). The rule is unchanged in *what* it yields.
- **Cost/plan-level:** a test that builds the two competing plans (materialized NLJ vs re-scan FlatMap) for a
  preserved-only inner and asserts the cost model **orders** them correctly via `PlanningCostModelLess` (a
  typed comparison on the wrapper expressions), and the inverse for a probe inner. Assert on the **typed plan
  tree** (`GetRecordQueryPlan()` structure / wrapper types), never the EXPLAIN string.
- **FDB e2e (behavioral, not string):** preserved-only `a LEFT JOIN b ON a.flag=1` returns the correct rows
  AND is not materially slower than master (a perf assertion or a plan-tree-shape assertion via the typed
  plan, not `ContainSubstring`). Keep the existing LEFT/FULL-OUTER row-count pins green.

## 7. Verification + gates

- No perf regression vs master (1M stress + plandiff classified as improvement/neutral; the preserved-only
  shape returns to materialize-once).
- Full `//...` 53/53; all LEFT/FULL-OUTER + correlated-EXISTS pins green.
- Query-engine change → **Graefe ACK on this RFC and the impl**, plus Torvalds + @claude + codex (with
  `--post` to #364). Then codex's P2 on #364 is resolved and #364 merges.

## 8. Scope note

This is a Go read-path cost-model improvement (no wire impact). It is **not** "match Java exactly" — Java is
*worse* on this shape (always re-scans). It is "let the cost model keep Go's better materialized NLJ where it
wins, the Java way (cost-driven, no rule heuristic)."
