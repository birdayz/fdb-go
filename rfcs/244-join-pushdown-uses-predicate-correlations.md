# RFC-244: join pushdown uses the predicate's complete correlation set

## Finding and reference

At `c50ec7e53`, `PushFilterBelowJoinRule.predicateSingleSide` walks only
`ComparisonPredicate` and `ValuePredicate` field values, then disregards fields
whose resolved path has more than one accessor. That is not a conservative
decline: in `A.ID = B.N.ID` it sees A, ignores B, and returns side 0, authorizing
the whole predicate to move below the join to A, where B is unavailable.

`TestPushFilterBelowJoin_NestedDependencyStaysAboveJoin` reproduces this under
Bazel: `A.ID = B.N.ID classified as side 0; both correlations must keep it above
the join`. Three SQL forms (WHERE, reversed WHERE operands, and ON) in
`TestFDB_JoinFilterKeepsNestedDependency` return the correct two pairs against
real FDB on this head. These are negative controls, not evidence that the
faulty rewrite is unreachable for all SQL.

Java 4.12.11.0 `PredicatePushDownRule.onMatch` partitions by
`queryPredicate.getCorrelatedTo().stream().noneMatch(otherAliases::contains)`.
It does not infer dependencies from field depth or enumerate predicate node
classes. Go already exposes that transitive contract through
`QueryPredicate.GetCorrelatedTo` and `predicates.GetCorrelatedToOfPredicate`;
the generic `PredicatePushDownRule` uses it.

## Decision

Replace the local field/predicate walker with the predicate's full correlation
set. A predicate referencing A and B stays above the join, independent of how
deeply either dependency is nested or which predicate/value kind carries it.
A predicate referencing only one owned side remains eligible. As in Java,
external correlations are not sibling dependencies. Predicates referencing
neither owned side remain above the join, retaining this specialized rule's
existing conservative policy: this two-leg rule has no side to choose for a
predicate that references neither leg. Record this divergence in DIVERGENCES.md.
The specialized rule matches Filter(Select(...)); the Java-shaped generic rule
matches Select(...). They operate in the same memo, not separate query pipelines.
The specialized rule must use the same complete dependency contract.

Classify with the actual typed owned quantifier aliases, not identities
reconstructed from source labels: a unique-kind alias and a named-kind alias
can share a label without being the same identity. Require source labels to
agree with the parallel owned labels as a separate consistency check; decline
missing or mismatched labels. Duplicate owned identities naturally classify as
both/neither and decline; there is no redundant duplicate-identity guard.
Preserve the actual owned alias when wrapping a leg, as Java does. Keep the join-kind and strict-single barriers and also
refuse null-on-empty edges: wrapping either flagged edge with an unflagged
ForEach loses its semantics. This does not rework the separate generic rule.

Delete the walkers once their production caller is removed. The scoped search
is `rg -n 'walkPredicateFieldValues|walkValueForFieldValues' pkg --glob '*.go'`;
before removal its production hits were confined to rule_push_filter_below_join.go.

A prior real SQL defect had the same missing-dependency consequence:
`case_cross_table_predicate_fdb_test.go` pins CASE conditions hidden by
`predicateValue.Children` before its fix (`expr/walk.go`). That was a missing
carrier-child contract; this finding is a local classifier discarding children
whose correlations the shared contract already reports.

This changes logical rewrite admission, not wire formats, physical operators,
continuations, or costs. Restricting the missing-sibling cases restores the
rule's stated equivalence; recognizing a deeply nested single-side predicate
uses the same correlation proof as its top-level counterpart.

## Verification

* Unit-drive the decision with direct and nested fields on either/both sides,
  non-field correlation carriers, composite and range predicates, constants,
  and external correlations. Assert actual correlation sets as preconditions.
* Fire the rule on typed two-leg expressions and verify a two-side predicate
  is retained, including mixed pushable/residual predicates.
* Retain the real-FDB SQL controls with explicit row assertions; record plan
  shapes without claiming they reach this particular rule unless demonstrated.
* Run the Cascades and SQL-driver Bazel targets uncached, `just test`, the
  affected determinism checks, and a bounded planner fuzz run.
* Compare the physical-plan corpus and the 1M stress target before/after.
  The initial runs were correctness-only because the filesystem was 98% full.
  After space was freed externally, repeat the timing comparison on the same
  filesystem; the matched measurements below supersede that initial blocker.
* Pin the observed translator shape behind the passing SQL controls:
  translateFilter absorbs ordinary inner-join WHERE predicates into the join's
  SelectExpression, rather than constructing the Filter(Select) shape targeted
  by this rule. The ON spelling also starts inside Select. Assert that shape
  and its cross-leg dependency, without generalizing to every rewrite later
  performed by the optimizer.

## Review

RFC review: Graefe ACK and Torvalds ACK before implementation. The latter
requires the translator pin's failure message to name the re-armed specialized
pushdown path; the pin does so. Implementation review and verification follow.
These ACKs authorize implementation, not merging without the remaining gates.

## Final verification

Reference tree: `c50ec7e5313b45e968e9b196da5eebd191151810`. The modified Go/build
inputs were frozen through verification; the SHA-256 of the sorted-path MD5
manifest was `a9a7da9741dd076a1cb09ca11f9af777dd1779b1152d60dec84336365049f009`,
and every listed input was checked again after the runs.

* `just test`: 92 test targets pass, 45 executed and 47 cached. The changed
  Cascades, embedded and SQL-driver targets executed, including the new tests.
* Ten uncached repetitions of the focused Cascades and embedded targets pass.
* `FuzzPlanner_Determinism`: 15 seconds, 2,959,536 harness executions, PASS.
* EXPLAIN comparison: all 2,955 entries identical (2,783 queries and 172 DML
  across 372 scenario files); no plan-shape or plan-error-classification change.
* `TestFDB_Stress_1M`: two uncached runs per tree, each with 24 RUN lines
  (the parent plus 23 subtests). All four runs pass and agree on the 22 labelled
  query row-count readings. These are correctness results, not timing parity:
  filesystem utilization remained 98%, so no performance ratio is claimed.

### Corpus firing measurement

The requested corpus check does **not** show a formerly live rule becoming
dead. Over the full `//pkg/relational/core/embedded:embedded_test` target,
uncached Bazel coverage measured the same dispatch partition in both trees:
711 `OnMatch` invocations, 707 declined because the child was not a Select,
and four declined at the non-inner-join barrier. Both yield sites have count
zero in both trees. The changed classification body is therefore not reached
by that target on either tree. This is scoped to that target, not every SQL
query the engine can accept. The direct rule tests, including the unique-kind
alias case, independently require successful yields; the real-FDB SQL controls
and translator-shape pin intentionally make no such claim.

Reproduce the measurement in each tree with:

```sh
bazelisk coverage //pkg/relational/core/embedded:embedded_test \
  --instrumentation_filter='^//pkg/recordlayer/query/plan/cascades(:|/)' \
  --combined_report=lcov --nocache_test_results
```

Read the `rule_push_filter_below_join.go` record in
`bazel-out/_coverage/_coverage_report.dat`. At the reference tree, entry/early
returns/yields are lines 51/56/61/155/163; at the implementation they are
52/57/62/157/165. These are execution counts for individual blocks, not sums
across repeated line counters.

### Review and remaining merge requirement

Implementation findings were folded: typed quantifier identities replace
label reconstruction, alias-barrier tests use single-side predicates, the
redundant duplicate-identity guard is removed, and the translator pin checks
only the immediate Filter-over-two-leg-Select boundary. Graefe, Torvalds and
@claude ACKed the code delta; codex re-confirmed with no concrete regressions.
An evidence-only delta review also confirmed that the baseline-zero corpus
counts settle the concern about disabling a formerly live corpus optimization.

The initial disk-space obstacle was resolved externally. The matched timing
comparison below replaces the outstanding measurement requirement; it is not
an assertion of statistical equivalence or merge authorization.

### Matched timing comparison (2026-09-08)

Baseline `c50ec7e5313b45e968e9b196da5eebd191151810` (the merge-base on
2026-09-08) versus implementation `0b8ba2ee2e26e5a53a0a04b36a9181c4ff7fe034`.
The latter commits the Go/build inputs authenticated by the manifest above;
checksums remained unchanged after the measurements and the pre-commit hook.
Both worktrees were on `/home`, the same filesystem. Exact byte readings put
utilization at 94.42–94.44% (human-readable `df` rounds it to 95%). The first
four runs also sampled disk/load every ten seconds; the remaining four
recorded both endpoints. One-minute endpoint loads ranged from 2.09 to 6.51.
This was not an idle-machine experiment.

Four ordinary runs per side, sequential BB A A BB A A, all passed uncached.
Each executed the parent and 23 subtests (24 RUN lines), and all eight agree
on the 22 timed query row counts below plus COUNT(*) = 1,000,000. Values are
milliseconds, listed in run order; ratios compare medians, not individual
samples. These are measurements of these two trees, not a speedup claim or
statistical proof of equivalence. In particular, point lookups did not meet
the aspirational <5 ms threshold on either tree.

| Query | Rows | Baseline ms, four runs | Implementation ms, four runs | Median ratio |
|---|---:|---|---|---:|
| PK lookup id=0 | 1 | 9.614, 13.644, 8.329, 14.456 | 11.152, 8.713, 8.565, 15.569 | 0.854 |
| PK lookup id=N/2 | 1 | 8.683, 15.341, 8.413, 19.211 | 8.660, 8.470, 8.559, 18.869 | 0.717 |
| PK lookup id=N-1 | 1 | 7.600, 15.502, 6.169, 12.841 | 7.355, 6.634, 6.912, 12.017 | 0.698 |
| idx_customer eq | 8 | 8.741, 32.677, 6.449, 17.964 | 6.610, 8.441, 6.808, 16.310 | 0.571 |
| idx_amount range >9000 | 100017 | 195.740, 254.763, 218.805, 285.919 | 209.184, 196.162, 209.597, 279.449 | 0.884 |
| idx_status count pending | 1 | 488.521, 319.086, 377.993, 331.382 | 425.579, 419.209, 407.002, 383.647 | 1.165 |
| full scan filter amount>5000 | 1 | 624.514, 605.185, 634.990, 555.657 | 663.863, 554.057, 730.302, 659.858 | 1.076 |
| GROUP BY status | 4 | 11.230, 5.974, 10.943, 6.100 | 12.955, 5.802, 12.491, 6.008 | 1.086 |
| GROUP BY status COUNT only | 4 | 10.157, 5.314, 9.493, 5.409 | 14.628, 5.545, 15.923, 5.557 | 1.355 |
| SUM by status (aggregate index) | 4 | 25.528, 5.718, 23.211, 5.881 | 10.237, 6.163, 10.945, 5.733 | 0.564 |
| GROUP BY customer HAVING | 47271 | 720.723, 586.188, 683.970, 582.220 | 612.086, 660.697, 600.762, 576.186 | 0.955 |
| JOIN 10 orders x customers | 10 | 21.431, 21.912, 19.944, 31.726 | 22.688, 27.778, 21.605, 22.475 | 1.042 |
| ORDER BY PK (full) | 1000000 | 3784.011, 3927.944, 3837.297, 3847.153 | 3785.699, 3840.684, 3834.707, 3838.953 | 0.999 |
| ORDER BY PK + index filter | 8 | 10.241, 9.511, 9.131, 8.757 | 9.316, 8.977, 8.938, 9.115 | 0.971 |
| scan all rows ordered | 1000000 | 3670.773, 3625.917, 3654.923, 3631.664 | 3612.588, 3650.031, 3625.972, 3623.202 | 0.995 |
| scan all rows wide | 1000000 | 3912.833, 3914.967, 3886.950, 3901.994 | 3854.259, 3903.685, 3869.655, 3890.997 | 0.993 |
| IN-list 5 values | 46 | 19.498, 19.232, 20.508, 19.326 | 23.064, 18.535, 18.602, 23.062 | 1.073 |
| PK needle id=999999 | 1 | 5.973, 5.812, 5.942, 5.921 | 6.121, 5.946, 5.739, 6.002 | 1.007 |
| PK+filter needle id=500000 | 1 | 7.387, 7.505, 7.339, 7.435 | 7.860, 7.288, 7.222, 7.938 | 1.022 |
| full scan sparse filter | 97 | 3272.945, 3290.906, 3297.606, 3282.661 | 3267.753, 3268.180, 3300.994, 3292.259 | 0.998 |
| UPDATE by index | 8 | 9.350, 8.957, 8.789, 9.082 | 8.771, 9.018, 8.731, 9.006 | 0.986 |
| DELETE single row | 1 | 6.521, 6.719, 7.030, 6.526 | 6.333, 6.684, 6.390, 6.787 | 0.987 |

The higher small-query medians were investigated with two additional exact
full-suite block-profiled runs per side, not folded into the ordinary table.
All four profile runs passed with 24 RUN lines and nonempty profile artifacts.
For the suspect COUNT-only aggregate, elapsed/blocked time was 5.427/3.960 and
4.532/2.960 ms on baseline, versus 5.546/3.950 and 5.741/4.140 ms on the
implementation. The corresponding nonblocking wall remainder is 1.467–1.572
versus 1.596–1.601 ms; this does not reproduce the ordinary table's multi-ms
spread. For index-status COUNT, baseline elapsed/blocked was 417.535/151.480
and 325.801/70.080 ms, versus 359.229/112.010 and 383.912/114.580 ms. Its
nonblocking remainder overlaps (255.721–266.055 versus 247.219–269.332 ms).
The full-scan-filter nonblocking remainder likewise overlaps (417.938–421.650
versus 413.448–424.480 ms), as does the join (17.107–17.854 versus
15.782–17.760 ms). Blocking stacks include FDB read-version and range-reply
waits. Subtracting blocked time is not a CPU measurement and does not remove
scheduler variation. These observations do not establish a repeatable added
work defect; they also do not prove that every query has identical latency.

Reproduce ordinary runs in each named tree, sequentially, four times:

```sh
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --nocache_test_results --test_output=streamed --test_timeout=1800 \
  --test_arg=-test.run='TestFDB_Stress_1M$' --test_arg=-test.v
```

For the separate profiles, create an absolute OUT directory and add:

```sh
--test_arg=-test.blockprofile="$OUT/block.pprof" \
--test_arg=-test.blockprofilerate=1 \
--sandbox_writable_path="$OUT" --sandbox_add_mount_pair="$OUT"

go tool pprof -list=runStressSuite "$OUT/block.pprof"
go tool pprof -traces -focus=timeQuery "$OUT/block.pprof"
```

The first capture attempt omitted the mount pair: its test passed but the
profile stayed in the disposable sandbox. It supplied no profiling evidence
and is excluded. The corrected captures explicitly required nonempty files.
Local full logs/environments are `/tmp/cascades-hunt-timing-{baseline,after}-`
`{1,2,3,4}.{log,env}`; profiles and their logs/lists/traces are in
`/tmp/cascades-hunt-profile-{baseline,after}-{1,2}/`. The tables above retain
the measurements independently of those temporary artifacts.

The stress executables retained after profiling have SHA-256s
`5e24dd947ba0d8b495ca527ffe3c88e115396dcf11507d18310c91f7e6b29d93`
(baseline) and
`0fc9188ebbc67d6a9d92560c6f81922ba0b70ddd83d970d2d49d69f76f90be37`
(implementation). The pprof Build ID labels are identical despite different
binary bytes; they are not a substitute for these fingerprints. An attempted
objdump check could not disassemble the stripped executables and supplied no
evidence. The frozen source manifest, separate output roots and executable
hashes identify the measured sides.

The final Graefe/Torvalds evidence delta ACKed this matched comparison,
including the per-query-line block accounting and explicit statistical limits.
This closes the timing measurement requirement, not GitHub CI or merge gates.
The implementation commit's hook ran generation, lint, build and `just test`:
92 targets passed (three executed, 89 cached); source checksums stayed intact.
