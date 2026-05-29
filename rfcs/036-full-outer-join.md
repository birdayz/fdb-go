# RFC-036: FULL OUTER JOIN (Go-only query extension)

Status: Implemented

## Problem

`TODO.md` lists "No RIGHT/FULL OUTER JOIN — Only LEFT OUTER is implemented" as a vs-Java
gap. Two findings reframe it:

1. **Java's SQL layer supports no outer joins at all.** `QueryVisitor` does not override
   `visitOuterJoin`; it falls through to `BaseVisitor.visitOuterJoin` →
   `visitChildren(ctx)`, which (unlike `visitInnerJoin`) never calls
   `addOperator`/`addInnerJoinExpression` — the right table is silently dropped. There are
   **zero** outer-join yamsql tests. So Go's existing LEFT OUTER JOIN is already a Go-only
   extension *beyond* Java, and RIGHT/FULL are further extensions, not parity work.

2. **RIGHT OUTER JOIN already works.** `cascades_translator.go:606-609` normalizes RIGHT →
   LEFT by swapping branches (`A RIGHT JOIN B` ≡ `B LEFT JOIN A`). It is wired SQL→logical
   (`logical_builder.go`, `plan_visitor.go`, `logical_predicate.go`), translated, planned,
   executed, and pinned by `TestFDB_RightJoin` (embedded_fdb_test.go:3966), a logical-builder
   test, and a plandiff corpus entry. The TODO line is **stale**.

The user has approved net-new query-side extensions ("we go beyond Java on querying") with
the hard constraint: **never sacrifice wire compatibility.** Outer joins are purely a
read-path planner/executor feature — they touch no key encoding, record format, index entry,
or continuation format. Java apps continue to read/write the same records; they merely cannot
*express* a FULL OUTER query. Wire compat is untouched.

So the genuinely-new work is **FULL OUTER JOIN**.

## Investigation

### FULL OUTER semantics
`A FULL OUTER JOIN B ON p` = the union of:
- every `A` row joined to matching `B` rows (or NULL-padded `B` when no match) — i.e. LEFT OUTER, and
- every `B` row that matched **no** `A` row, NULL-padded on the `A` side.

There is no Java reference, so the design must be Cascades-native.

### Execution model (the key enabler)
The non-correlated nested-loop path (`streaming_cursors.go` `nljCursor`) **fully materializes
the inner side** into `innerRows []QueryResult` up front (built via the P0.1
`CollectAllBounded` materialization limit, so it is memory-bounded). It streams the outer side
row-by-row and, for `JoinLeftOuter`, emits a NULL-padded row when an outer row matched nothing
(`streaming_cursors.go:836-841` via `qualifyOuterRow(outerRow, outerAlias)`).

Because the inner is already materialized, FULL OUTER needs only two additions to this same
cursor:
1. a `matchedInner []bool` bitmap (length `len(innerRows)`), set whenever an inner row passes
   the join predicates against any outer row (in both the hash-probe and linear-scan paths);
2. a **drain phase** after the outer is exhausted: emit each unmatched inner row NULL-padded on
   the left via `qualifyOuterRow(innerRow, innerAlias)` — exactly symmetric to the existing
   LEFT-OUTER unmatched-outer emission. `qualifyOuterRow` qualifies the supplied row's columns
   under its alias and leaves the other side's columns absent; downstream qualified column
   refs (`a.col`) resolve absent → NULL via the existing
   `JoinMergeResultValue.Evaluate`/projection path.

This reuses all existing null-padding, projection, and bound-materialization infrastructure.
It is deterministic (outer order, then inner order) and Halloween-safe (inner already
materialized).

### Why NOT a logical UNION decomposition
`FULL = (A LEFT JOIN B) ∪ALL (B ANTIJOIN A, null-left)` is algebraically valid but would scan
both inputs twice, double predicate evaluation, and require a new rewrite rule plus union
ordering glue — more moving parts, more divergence risk, no benefit over the single-cursor
approach given the inner is already materialized. Rejected.

### Why FULL must NOT use the FlatMap (correlated) path
`FlatMap` re-scans the inner per outer row (correlated PK/index probe). It cannot observe which
inner rows were globally unmatched, so it cannot produce the drain set. FULL OUTER must route
**only** to the materialized `nljCursor`. The NLJ rule will skip `tryFlatMapPlan` and the
correlated-FlatMap yields for `JoinFullOuter`, yielding only the materialized NLJ. A correlated
inner (where the inner ranges over the outer's alias) is incompatible with FULL OUTER and is
not standard SQL; such a shape yields no FULL plan (planner returns no plan → clear error),
rather than a silently-wrong one.

## Fix

Thread a new `JoinFull`/`JoinFullOuter` value through the existing pipeline; no new enum
*concept*, just one more arm everywhere LEFT/RIGHT/INNER are already handled. The full call-site
list (verified by grepping every `joinType`/`JoinLeft*`/`JoinRight` switch) is:

1. **Grammar** `pkg/relational/core/parser/grammar/RelationalParser.g4`: extend the `outerJoin`
   rule `(LEFT | RIGHT) OUTER? JOIN` → `(LEFT | RIGHT | FULL) OUTER? JOIN`. `FULL` is already a
   lexer token. Regenerate via `just generate-parser` (ANTLR/Bazel) + `just gazelle`.
2. **Embedded parse** `select_parser.go`: add `joinTypeFull` to the `joinType` enum;
   `extractJoinClause` sets it when `j.FULL() != nil`. Outer joins are ON-only in
   `extractJoinClause` (it reads `j.Expression()`, not USING) — this is a pre-existing limitation
   shared by LEFT/RIGHT; FULL matches it. Tests use ON.
3. **Logical** `logical/operators.go`: add `JoinFull` to `JoinKind` + `String()`. Map
   `joinTypeFull → logical.JoinFull` in the three logical builders that switch on join type
   (`logical_builder.go:262`, `plan_visitor.go:836`, `logical_predicate.go:3592`).
4. **Translator** `cascades_translator.go`: TWO functions build joins, both must be patched:
   - `translateJoin` (:595): add `case logical.JoinFull → expressions.JoinFullOuter` (no operand
     swap — FULL is symmetric).
   - `translateJoinWithExists` (:664): this function has its own `JoinRight` swap (:671) and
     join-type switch (:737); it builds join+EXISTS selects. FULL OUTER cannot drain through the
     EXISTS/FlatMap shape, so **reject** FULL+EXISTS here with a clear `UNSUPPORTED_OPERATION`
     error rather than letting the default arm silently mistranslate it to INNER.
   - Add `logical.JoinFull` to the filter-merge guard at :165 (WHERE must stay *above* an outer
     join, never merged into ON — same as the existing `JoinLeft`/`JoinRight` exclusions).
5. **Expressions** `expressions/select.go`: add `JoinFullOuter` to `JoinType`.
6. **Plans** `plans/nested_loop_join.go`: add `JoinFullOuter` to `JoinType` + `String()` ("FULL OUTER").
7. **NLJ rule** `rule_implement_nested_loop_join.go`: map `expressions.JoinFullOuter →
   plans.JoinFullOuter` in the `:101` switch. Extend the existing swap-suppression at `:120`
   (`canSwap := joinType != plans.JoinLeftOuter`) to also exclude `plans.JoinFullOuter`. For FULL,
   add an explicit early guard: do **not** call `tryFlatMapPlan` (:113) and do **not** take either
   correlated-FlatMap branch (:137-145); yield **only** the materialized NLJ (no operand swap). FlatMap re-scans
   the inner per outer row and structurally cannot observe global inner-match state, so it is not
   a valid FULL implementation — skipping it is correctness, not pruning. The FULL NLJ advertises
   **no ordering property** (the drain appends unmatched-inner rows after the outer stream, so the
   output is unordered on any input key — the rule must not attach a sort/PreserveOrdering
   property to it).
8. **Executor (Cascades, primary path)** `streaming_cursors.go` `nljCursor`: add
   `matchedInner []bool` (length `len(innerRows)`), set `matchedInner[idx]=true` whenever an inner
   row passes the join predicates — in **both** the hash-probe (:770) and linear-scan (:797) paths,
   and on **every** passing index for a given outer row (many-to-many). After the outer is
   exhausted (`outerExhausted`), for `JoinFullOuter` run a drain phase: emit each
   `!matchedInner[i]` inner row NULL-padded on the left via `qualifyOuterRow(innerRows[i],
   innerAlias)`. `JoinLeftOuter`/`JoinInner`/etc. behavior is byte-for-byte unchanged.
9. **Executor (legacy embedded path, defensive)** `embedded/join.go` `execSelectJoin`: this naive
   NLJ executor is still *live* for INFORMATION_SCHEMA / explain-only / UNION-branch routing
   (`select_query_full.go:39` ← `execSelectQueryFull`), and it already implements RIGHT via a
   `matchedRight` bitmap (:148,:213) + LEFT null-pad (:183). Extend its switch with `joinTypeFull`
   = LEFT's unmatched-left emission **and** RIGHT's unmatched-right emission, so no path silently
   drops FULL rows. Two specific guards must both include `joinTypeFull`: the `matchedRight`
   allocation at `:148` (`if jc.joinType == joinTypeRight`) and the LEFT unmatched-left emission at
   `:183` (`if jc.joinType == joinTypeLeft && !matched`), plus the RIGHT drain at `:213`
   (`if jc.joinType == joinTypeRight`). Real FDB join queries route through Cascades
   (`planSelectCascades`), so this is belt-and-suspenders consistency, not the primary path.
10. **Dead `plangen`** (`plangen.go:840,878`): a swap+join switch with **no non-test callers**
    (verified — CLAUDE.md "no parallel pipelines"). **Not extended.** FULL hits its
    default→INNER arm but the code is unreachable. Deleting the whole dead module is a separate
    cleanup, out of scope for this PR.

### NULL join-key correctness (verified, not assumed)
`Comparison.EvalAgainst` returns `TriUnknown` when either operand is nil (`comparisons.go:367`),
and `passesJoinPredicates` (`executor.go:1285`) treats anything `!= TriTrue` as no-match. So
`NULL = NULL` never matches (SQL 3VL): NULL-keyed rows on either side match nothing and both land
in their respective drain/null-pad sets. With the hash index, NULL keys hash into the `idx[nil]`
bucket, but the residual predicate still rejects `NULL = NULL`, so they are never marked. Pinned
by a mandatory NULL-key test.

## Performance

- LEFT/RIGHT/INNER paths: untouched; the `matchedInner` slice is allocated only for
  `JoinFullOuter`.
- FULL OUTER: same `O(N_outer × N_inner)` (or `O(N_outer)` with the existing hash index) as
  LEFT OUTER, plus one `O(N_inner)` drain pass. Inner materialization is already bounded by the
  P0.1 `CollectAllBounded` limit — no new memory-bomb surface.
- No stress-baseline regression expected (no change to existing plan shapes). Will spot-check
  the join stress subtests.

## Limitation: single-transaction execution

The `matchedInner` bitmap is cross-outer-row state that is **not** serialized into the
continuation. The driver rebuilds the cursor from scratch on every transaction page
(`txPageTimeLimit = 4s`), which would reset the bitmap mid-scan and produce wrong drain
results. So `executeNestedLoopJoin` clears the FULL-outer's time/row limits, forcing the whole
join to complete within a single transaction (one cursor, one bitmap). Very large FULL OUTER
joins therefore fail *loudly* at FDB's 5s hard transaction limit rather than returning
silently-wrong rows — the same limitation class as the already-materialized inner side, and as
the legacy `execSelectJoin` path which materializes everything. INNER/LEFT/RIGHT are unaffected
(no cross-outer state; they resume correctly per outer row). Serializing the bitmap into the
continuation (like aggregation/sort accumulators) is a future enhancement if unbounded FULL
joins are needed.

## Test plan

E2E is the bar (NO-FAKE-CHECKBOXES). FDB integration tests (`TestFDB_FullOuterJoin*`) on real
FoundationDB, all `t.Parallel()` with unique key prefixes:

- both-sides matched (rows present on both)
- left-only unmatched (NULL right columns)
- right-only unmatched (NULL left columns) ← the new drain path
- both-sides have unmatched rows in one query (the union of all three above)
- NULL join-key values on both sides (NULLs never match — both become unmatched rows)
- many-to-many matches (a left row matching multiple right and vice versa)
- FULL OUTER + WHERE filter above the join (NULL-padded rows correctly filtered)
- large inner side (>100 rows) to exercise the hash-index probe + drain interaction
- `SELECT *` and explicit `a.col, b.col` projections; `EXPLAIN` asserts the plan is a
  materialized NLJ with FULL OUTER (not FlatMap)
- FULL OUTER + EXISTS in WHERE → asserts the clear UNSUPPORTED error
- RIGHT OUTER regression: add a NULL-join-key case to pin existing behavior

Determinism: 10× repeat on the new FULL OUTER tests (planner must produce identical plans).
Full `just test` green; `just generate` diff clean in CI.
