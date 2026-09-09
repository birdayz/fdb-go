# RFC-245: a primary-key intersection proves its comparison key leg by leg

## Finding and reference

At `d6b5a0d84`, `createPrimaryKeyIntersection` (`intersector_primary_key.go`)
admits a comparison key when `comparisonKeyContainsFreePrimaryKey` holds against
the UNION of every leg's equality-bound values. That is a faithful port of Java
4.12.11.0's `AbstractDataAccessRule.isCompatibleComparisonKey`, whose
`equalityBoundKeyValues` argument `WithPrimaryKeyDataAccessRule.
createIntersectionAndCompensation` builds by flattening the equality-bound
matched ordering parts of all partition legs into one set.

The union is too permissive. A component equality-bound in ONE leg is a constant
of that leg's stream only; in every other leg it still varies. Over
`PRIMARY KEY (pk1, pk2)` with indexes `(b, pk1)` and `(pk2)`:

```sql
SELECT pk1, pk2, b FROM ti WHERE b = 1 AND pk2 = 3
```

the merged intersection ordering carries `pk2` as FIXED (from the `(pk2)` leg),
`enumerateSatisfyingComparisonKeyValues` filters singular fixed values out of the
enumeration, and the only comparison key offered is `(pk1)`. The union check
subtracts `pk2` from the primary key and accepts it. The `(b, pk1)` leg holds
several records per `pk1` differing only in `pk2`, so "equal comparison keys"
no longer means "the same record": the merge cursor, which emits the first
leg's current record when every leg agrees and then advances every leg, returns
records the other leg never matched.

Measured on both engines (`conformance/pk_intersection_leg_bound_key_java_probe_test.go`),
rows `(0,2,1) (0,3,0) (1,4,1) (1,3,0) (2,0,1) (2,3,7) (3,3,1) (4,1,1)`:

```
Java: COVERING(TI_PK2 [EQUALS …]) ∩ COVERING(TI_B_PK1 [EQUALS …]) COMPARE BY (_.PK1) | FETCH
      rows {[0 3 0] [1 3 0] [2 3 7] [3 3 1]}    COUNT(*) = 4
Go (before): Fetch(Intersection(IndexScan(TI_B_PK1, [=, *] COVERING), IndexScan(TI_PK2, [=] COVERING)))
      rows {[0 2 1] [1 4 1] [2 0 1] [3 3 1]}    COUNT(*) = 4
correct:    {[3 3 1]}                           COUNT(*) = 1
```

Each engine emits its own first leg's records, which is why the two wrong
answers differ while the wrong COUNT agrees. With an `ORDER BY` both engines
plan the covering scan with a residual filter and are correct; that is the
control that says the fixture is sound.

The class is reachable only with a composite primary key: a single-component
key fixed in a leg makes that leg max-cardinality 1, and `isPartitionRedundant`
prunes such partitions. That is why neither upstream's fixtures nor the RFC-182
generator (fixed single `ID` key) ever crossed it, and why the yamsql corpus
EXPLAIN baseline does not change under the fix.

## Decision

The soundness condition of the merge is per leg: within every leg the
comparison key together with THAT leg's own equality-bound values must cover
the common primary key. Equivalently, subtract the INTERSECTION of the legs'
equality-bound sets rather than their union. `createPrimaryKeyIntersection`
collects each leg's equality-bound values while merging the orderings and
proves every enumerated comparison key through
`comparisonKeyIdentifiesRecordInEveryLeg`, which applies the existing
single-leg proof `comparisonKeyContainsFreePrimaryKey` to each leg in turn.

Consequences, chosen deliberately:

* The unsound partition is DECLINED, not repaired by widening the comparison
  key. Widening would mean admitting a singular fixed value into the enumerated
  key against Java's `enumerateSatisfyingComparisonKeyValues` design; declining
  keeps the enumeration and the ordering algebra untouched and leaves the
  surviving single-index alternative to apply the other predicate as a
  residual filter. A future planner that widens instead passes every pin here
  (they assert rows, and that any intersection compares on both components).
* Partitions where EVERY leg fixes the same primary-key component still
  intersect on the remaining components — indexes `(a, pk2)` and `(b, pk2)`
  under `a = … AND b = … AND pk2 = …` merge on `(pk1)`. The intersection-of-sets
  form is what preserves this; a proof that ignored equality-bound values
  altogether would decline it.
* Three-way partitions keep their sound pairs: with legs `(a)`, `(b)` and
  `(pk2)`, the `a ∧ b` intersection on `(pk1, pk2)` is built and every
  partition containing the `(pk2)` leg is declined.
* The union `equalityBoundValues` stays as the input to
  `isPrimaryKeyPartitionRedundant`, where Java's pruning semantics are the
  intended ones (a sub-partition that already fixes every equality of the whole
  partition makes the larger partition redundant). Only the soundness proof
  changes.
* This is a deliberate divergence from Java, recorded at the fix site, in
  DIVERGENCES.md ("PK-intersection comparison key") and as a TODO.md section 9
  upstream entry with the reproducer. Direction `DivergenceJavaWrongRowsGoCorrect`.

No wire format, continuation, physical operator or cost formula changes. The
change is in which logical alternatives the data-access rule yields.

## Verification

* Unit: `intersector_leg_bound_pk_test.go` drives the decision on
  fixture legs whose equality-bound column IS a primary-key component
  (`makeLegOverColumns`): decline when one leg fixes `VERSION` and the other
  sorts it; accept with comparison key `(ID)` when both legs fix `VERSION`;
  three-way keeps the sound `A ∧ B` pair on `(ID, VERSION)` and admits no
  partition containing the `VERSION`-fixing leg.
* Real FDB: `TestFDB_PkIntersectionLegBoundComponent` — eleven queries over the
  reproducer's rows with hand-expected results (the two-leg shape, `pk1`-only
  projection, `COUNT(*)`, an added range residual, the three-way shape, the
  `(a)`/`(pk2)` pair, IN-list variants, and a control pair that fixes no
  primary-key component), plus the property that every intersection the
  planner builds for them compares on both primary-key components.
* Cross-engine: `conformance/pk_intersection_leg_bound_key_java_probe_test.go`
  asserts Go's correct rows absolutely, asserts Java's WRONG rows on three arms
  (so a fixed upstream fails the probe and forces reclassification), and asserts
  agreement on two `ORDER BY` controls; corpus entry
  `pk_intersection_leg_bound_component_count` carries the same pin as a
  `DivergenceJavaWrongRowsGoCorrect` annotation with its ordered control beside
  it.
* Net: `TestFDB_MetamorphicCompositePrimaryKey` — a new axis of the existing
  indexed/unindexed twin (`mmTwin`, `metamorphic_twin_test.go`; every other
  axis keys its table on a single `id`, which is exactly the shape under which
  this class cannot arise): 258 queries over a composite primary key, a
  three-column mixed-type index and indexes repeating primary-key components,
  each run against both schemas and again through a paged connection. This is
  what found the defect (2 of the first 240 queries diverged at `d6b5a0d84`;
  the 18 `… AND pk2 = …` probes were added on the finding). It carries
  non-vacuity floors stated with their population: at 258 queries,
  compared=241 nonEmpty=224 bothErrored=17 pagedCompared=239 pagingDeclined=2
  (floors 230 / 200 / ≤25 / 220 / ≤10). Its DML companion
  `TestFDB_MetamorphicCompositePrimaryKeyDML` applies 19 UPDATE/DELETE/INSERT
  statements to both schemas and compares the table and 19 index-backed reads
  after each; every read compares (380 of 380) and all agree. Run at
  `d6b5a0d84` it measures the defect's DML face: its first statement,
  `UPDATE t SET a = 5 WHERE b = 1 AND pk2 = 3`, reports `rows affected
  diverge: indexed=5 unindexed=3` — the unsound merge fed the UPDATE's target
  set, so the indexed schema rewrote two records the predicate never matched,
  and 224 read comparisons disagree from that statement on. The class is not a
  wrong SELECT only; it corrupts data through DML.
* Every pin was run against the unfixed tree `d6b5a0d84`, with the shipped
  files copied into a worktree at that commit: the unit decline and three-way
  arms fail (`comparison key [_current.ID#0] cannot identify a record …`,
  `an intersection with 3 legs was built`); the 258-query net reports
  `10 mismatches` (every `… AND pk2 = 3` shape: `a`, `s`, `d`, `f`, `b`,
  `a ∧ s`, `a ∧ b`, the `pk1`-only projection and `COUNT(*)`) and the FDB pin
  reports 7 wrong-row cases plus 7 plan-property failures (`TI intersection
  compares on 1 key(s)`), while its TJ accept arm passes on both trees — the
  merge whose legs all fix pk2 was never in question; the DML sweep reports
  five statements with divergent `rows affected` (`indexed=5 unindexed=3` on
  the first) and 224 disagreeing reads; the corpus entry was
  mutated (`GoExpectedRows` `1 → 4`) and reported `STALE … Go rows changed from
  the annotation: [[1]]`.
* EXPLAIN corpus (`cmd/explain-differ`), baseline `d6b5a0d84` vs `e937f1513`:
  2955 entries (2783 queries + 172 DML across 372 scenario files), 2955
  identical, 0 differing, 0 shape flips. The corpus holds no composite-key
  intersection shape.
* `just test` at `e937f1513`: 92 test targets pass (44 executed, 48 cached);
  the new tests appear in the Bazel test logs by name.
* Cross-engine corpus spec (`runs the SeedRunCorpus through BOTH engines`): PASS
  at 1433-spec suite scale, 41s, with both new entries reached.
* Planner fuzz at the implementation: `FuzzPlanner_Determinism` 15s,
  2,991,309 executions, PASS; `FuzzPlanner_PlanFullPipeline` 15s, 1,048,943
  executions, PASS.

### 1M stress comparison

Baseline `d6b5a0d84` (the merge-base on 2026-09-08) in a worktree on the same
filesystem (`/home`, 94.6% used) versus implementation `e937f1513`, two
uncached `TestFDB_Stress_1M` runs per side, strictly sequential
(base, branch, base, branch), load average at each start 6.7 / 2.2 / 3.8 / 2.5.
Every run has 24 `=== RUN` lines and 24 passes; all 22 labelled readings agree
on row counts across the four runs. Ratio = min(branch) / min(base):

| query | rows | base | branch | ratio |
|---|---|---|---|---|
| PK lookup id=0 / N/2 / N-1 | 1 | 8.5 / 8.4 / 6.3 ms | 8.6 / 8.4 / 6.3 ms | 1.02 / 1.00 / 1.01 |
| idx_customer eq | 8 | 6.4 ms | 7.0 ms | 1.10 |
| idx_amount range >9000 | 100017 | 191.8 ms | 195.1 ms | 1.02 |
| idx_status count pending | 1 | 340.2 ms | 315.4 ms | 0.93 |
| full scan filter amount>5000 | 1 | 596.5 ms | 631.0 ms | 1.06 |
| GROUP BY status | 4 | 10.6 ms (17.8 on run 2) | 12.6 ms | 1.20 |
| GROUP BY status COUNT only | 4 | 10.1 ms | 9.4 ms | 0.93 |
| SUM by status (aggregate index) | 4 | 12.3 ms | 11.5 ms | 0.94 |
| GROUP BY customer HAVING | 47271 | 678.7 ms | 693.0 ms | 1.02 |
| JOIN 10 orders x customers | 10 | 19.3 ms | 19.5 ms | 1.01 |
| ORDER BY PK (full) | 1000000 | 3767 ms | 3766 ms | 1.00 |
| ORDER BY PK + index filter | 8 | 8.8 ms | 8.8 ms | 1.00 |
| scan all rows ordered / wide | 1000000 | 3660 / 3902 ms | 3620 / 3877 ms | 0.99 / 0.99 |
| IN-list 5 values | 46 | 19.0 ms | 19.2 ms | 1.01 |
| PK needle id=999999 | 1 | 5.8 ms | 5.8 ms | 1.00 |
| PK+filter needle id=500000 | 1 | 7.8 ms | 7.4 ms | 0.95 |
| full scan sparse filter | 97 | 3278 ms | 3246 ms | 0.99 |
| UPDATE by index / DELETE single row | 8 / 1 | 8.7 / 6.3 ms | 8.8 / 6.4 ms | 1.01 / 1.01 |

The two readings above 1.05 are ~10 ms queries whose base runs disagree with
each other by more than the branch differs from either (GROUP BY status: 10.6
vs 17.8 ms between the two base runs). The workload has a single-component
primary key, so no plan in it passes through the changed proof; this is a
no-change confirmation, not a claim of improvement.

## Review

Graefe (Cascades alignment) and Torvalds (code quality) reviewed this RFC and
the implementation together — the change is one commit, so the RFC is recorded
with the implementation rather than ahead of it. Both ACKed with conditions,
all folded:

* Graefe: the forfeited `(pk1, pk2)` merge is a real plan, not a hypothetical
  one — booked as an unchecked TODO.md section 3 item ("Widen the
  pk-intersection comparison key …") referencing this RFC, rather than left in
  prose here.
* Torvalds: the FDB pin's header said "six rows" for a fixture that yields four
  (fixed); the plan-property loop asserted nothing when no intersection was
  built (now floored: TI must build one for the control, TJ must build the
  sound merge); the twin-table net logged every degenerate outcome and
  continued (now floored, populations above); the RFC promised fuzz and stress
  numbers it did not carry (above); the accept direction was pinned only by a
  hand-built fixture (now e2e over table TJ with indexes `(a, pk2)` and
  `(b, pk2)`, whose intersection on `(pk1)` must be built and return the right
  rows).

codex and @claude review on the PR; their verdicts are recorded there.
