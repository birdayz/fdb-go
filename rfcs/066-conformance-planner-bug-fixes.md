# RFC-066: Cross-engine conformance planner bug fixes

**Status:** Implemented
**Item:** Quality — latent Go divergences in the Java cross-engine conformance
suite (`//conformance:conformance_test`).

## Context: why these were latent

`//conformance:conformance_test` is tagged `conformance_java` and is **excluded
from the CI gate and from `just test`** (`--test_tag_filters=-conformance_java`);
it runs only in the nightly jobs. So master's CI is legitimately green while the
cross-engine suite carries deterministic Go divergences. Additionally the suite's
`Ordered` Describe (used only so `BeforeAll` can set up the shared cluster file)
stops at the **first** failure and skips the rest, masking all but one yamsql
failure per run. These two facts let several real bugs accumulate unnoticed.

This RFC fixes two independent, deterministic divergences, each pinned with a
focused FDB-backed regression test that does not depend on the Java server.

## Bug 1 — IN-list duplicate literals not deduped

`SELECT a, b FROM ta WHERE a IN (1, 1, 1)` on PK `a` returned three identical
rows. The IN-on-an-indexed-column path rewrites to Explode + InJoin (one
iteration per list element); duplicate literals produced duplicate rows.
(`b IN (1,1,1,1)` on a non-indexed column was unaffected — it becomes a
set-membership filter.)

**Root cause.** `InComparisonToExplodeRule` (`rule_in_to_explode.go`) wrapped the
IN-list verbatim in the Explode. Java's `InComparisonToExplodeRule` wraps the
value comparand in `ArrayDistinctValue` (the `ValueComparison` branch) →
duplicates collapse.

**Fix.** Dedupe the IN-list (order-preserving, SQL value equality; `[]byte` by
content) before the Explode, before the single-element collapse so
`col IN (1,1,1)` reduces to a plain `col = 1`. The cascades unit test
`TestInExplode_DuplicateElements` is updated from "3 elements (no dedup)" to "2
deduped elements".

Regression: `TestGoSQLRunner_InListDedup`.

## Bug 2 — join projection reports UNKNOWN column type

`SELECT u.name, o.total FROM Users u, Orders o WHERE u.uid = o.uid` returned the
correct rows but reported `O.TOTAL`'s type as `UNKNOWN` (Java: `BIGINT`).

**Root cause.** `deriveColumnsFromProjection` (`cascades_generator.go`) resolved
column types against a **single** leaf descriptor (`findLeafDescriptor` follows
only the `GetInner()` chain → the inner join leg). A projection over a join
references columns from multiple record types; the other leg's column (`o.total`
from Orders) had no descriptor to resolve against → `UNKNOWN`.

**Fix.** `allLeafDescriptors` collects every scan/index leaf descriptor (both
join sides); `descriptorForColumn` picks the leg that defines each column — the
unique match, else the leg whose record-type name matches the column's
qualifier, else (deterministic) the first match. Type AND nullability resolve
against that same descriptor. Residual limitation: a SQL *alias* qualifying
same-named columns of DIFFERENT types across legs can't be resolved here — the
physical plan's leaves carry record-type names, not query aliases, so the
alias→type map is gone; that case needs the value-level type derivation that
today leaves the FieldValue type UNKNOWN (the same gap forcing this lookup).
Column metadata only — no effect on plannability.

Regressions: `TestGoSQLRunner_JoinProjectionColumnTypes` (far-leg column type),
`TestGoSQLRunner_JoinSameNamedColumnsDisambiguateByQualifier` (same-named
cross-leg columns of differing types disambiguated by qualifier).

## Characterized follow-ups (NOT fixed here)

### Index-intersection drops the SELECT projection (gated on tech-debt 7.2)

`SELECT id FROM t WHERE status = 'active' AND v > 20`, with single-column indexes
on `status` and `v`, deterministically plans to a **bare**
`Intersection(Fetch(IndexScan(IDX_STATUS)), Fetch(IndexScan(IDX_V)))` with no
projection, returning the full record `[id, status, v]` instead of `[id]`.

Root cause: `WithPrimaryKeyIntersector` returns `NoCompensation` and never
applies the result (projection) compensation that Java's
`WithPrimaryKeyDataAccessRule.createIntersectionAndCompensation` applies via
`applyAllNeededCompensations`.

**Why it is NOT fixed here.** Two Go-specific obstacles, both rooted in the
incomplete value-matching infrastructure (tech-debt 7.2, port MaxMatchMap /
PullUp):

1. A single-index leg's compensation is **impossible** today (`PullUp`/
   `MaxMatchMap` returns nil), so the folded intersection compensation is
   impossible. Dropping it on impossibility (Java's `!isImpossible` guard)
   removes the projection-less intersection — but it ALSO removes intersections
   that other queries (e.g. the multi-partition vector scan tests) rely on,
   making them unplannable. The "this plan can't satisfy the result value, so
   suppress it" invariant is correct in principle but can't be applied
   selectively until legs carry feasible compensation.
2. Applying the compensation the Java way — `applyAllNeededCompensations` →
   `buildSelectWithResultValue` — inserts a fresh **logical** SelectExpression
   over the physical intersection. That re-introduces the task cascade this
   intersector deliberately avoids (its doc comment: "fresh child References
   trigger re-exploration loops"), non-deterministically exhausting the planner
   task budget and making unrelated queries intermittently unplannable.

The correct fix is: port MaxMatchMap/PullUp (7.2) so leg compensations are
feasible, and apply the surviving projection as a **physical** projection (no
re-exploration). Until then the bare intersection wins for projected
AND-of-two-indexed-columns queries — a nightly-only conformance divergence, not
a CI-gated regression.

### inner_join column-name metadata divergence

After Bug 2 (`o.total` type now BIGINT), the corpus `inner_join` entry still
diverges on column metadata — likely the qualified name `O.TOTAL` vs Java's
spelling. Needs the Java column-metadata shape captured to align.

### UNION ALL `SELECT *` branch alignment

`SELECT id AS W, ... UNION ALL SELECT * FROM t1` yields `[nil,nil,nil]` rows for
the star branch: union output columns take the left branch's (aliased) names,
the right `SELECT *` branch's record map is keyed by table fields, and
`RecordLayerResultSet.columnValue` extracts by output-column name → miss → null.
Needs each union branch re-keyed to the output column names positionally (a
logical→cascades alignment projection; no output-column resolver exists at the
logical layer yet).

## Test plan

- `TestGoSQLRunner_InListDedup`, `TestGoSQLRunner_JoinProjectionColumnTypes`,
  `TestGoSQLRunner_JoinSameNamedColumnsDisambiguateByQualifier` — FDB-backed,
  Java-free; each fails on master and passes with the fix.
- `TestInExplode_DuplicateElements` (cascades unit) updated to the deduped shape.
- `just test` (the real CI gate) stays green, and the vector-plan tests stay
  deterministic (the reverted intersector experiment confirmed they flake when
  the intersector emits/drops differently).
