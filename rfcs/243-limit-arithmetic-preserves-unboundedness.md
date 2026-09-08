# RFC-243: LIMIT arithmetic preserves unboundedness and representable offsets

## Problem and reference

Two logical LIMIT rewrites perform arithmetic on values that are not ordinary
bounded row counts:

* `LimitMergeRule` adds two signed 64-bit offsets without checking overflow.
  `LIMIT 1 OFFSET 1` over `LIMIT MaxInt64 OFFSET MaxInt64` can become
  `LIMIT 1 OFFSET MinInt64`. The nested query skips every row of a small table;
  a negative combined skip must never make that query return a prefix instead.
* `PushLimitThroughUnionRule` computes `limit + offset` before recognizing a
  negative limit as the no-cap sentinel. A skip-only logical limit of `-1, 2`
  consequently caps every union branch at one row, discarding required rows.

These are Go read-side extensions, not ports of Java logical LIMIT rules. The
pinned Java reference is `ExecuteProperties.clearSkipAndAdjustLimit`: it tests
`rowLimit == ROW_LIMIT_UNLIMITED` **before** adding the skip, and leaves an
unlimited request unlimited. `SkipCursor.onNext` discards exactly the requested
prefix; `RowLimitedCursor.onNext` supplies the separate finite cap. Neither
licenses turning a no-cap sentinel into a finite cap.

Go SQL's grammar requires `LIMIT` before `OFFSET`. A bare OFFSET-only clause is
rejected, but `parseLimitClause` also uses the negative sentinel for an unresolved
parameter. The driver substitutes bound parameters before planning; pin a
parameterized UNION limit through direct execution, prepared-statement reuse,
and EXPLAIN rather than inferring that boundary from the grammar alone.

## Decision

Keep the current operators and continuation formats. Enforce the numeric domain
at construction, check rewrite arithmetic, and saturate only execution budgets:

1. Decline LIMIT merging when adding nonnegative offsets would exceed MaxInt64.
   Retain both operators so each skip remains exact. Do not saturate the combined
   offset: MaxInt64 is not the mathematical sum and can itself change semantics.
2. Decline branch-cap pushdown for a negative static limit, as already done for a
   runtime cap. A finite branch cap cannot represent an unlimited request.
3. Use a shared `checkedLimitSum` in the two rules: both inputs must be
   nonnegative and their sum must fit int64. The union rule declines explicitly
   for an overflowing finite branch cap instead of relying on a wrapped negative
   value. Preserve the runtime-cap guard and ordinary finite pushdown.
4. Reject negative offsets in both logical constructors and
   `NewRecordQueryLimitPlanFromQuantifier` with
   `expressions.InvalidLimitOffsetError{Offset}`. The physical static constructor,
   runtime-value constructor and `WithInner` all delegate to that constructor;
   test its exported quantifier path directly too. Unlike
   a negative limit, a negative offset has no defined no-cap meaning. Otherwise
   a negative outer offset can also make `iLimit - oOffset` overflow during
   merging. Java's public `RecordCursor.skip` rejects negative skips, although
   directly constructing `SkipCursor` treats them as zero. Go's request-level
   `applySkipLimit` convention still ignores nonpositive skips at every operator;
   this reachable divergence is retained consistently rather than making LIMIT
   or VALUES the sole rejecting operator. Pin negative request skips separately
   from the invalid semantic-offset domain.
5. `executeLimit` also adds `remOffset + emit` unchecked when setting the child
   read budget. It currently wraps negative and therefore disables the budget
   at consumers that test `> 0`; this case does not change rows today. Extract
   `limitChildRowLimit(parentCap, remOffset, remLimit)` and saturate its sum at
   MaxInt, preserving the existing parent-cap and unbounded handling. A read
   budget may saturate because reaching it yields a resumable page boundary;
   a semantic offset may not. Unit-drive the extracted decision, including
   parent-cap, zero, unbounded and overflow cases. Validate negative remaining
   offsets at continuation decoding too, before calculating the child budget;
   negative remaining limits remain valid no-cap sentinels. The decoder tests
   exercise this invalid-input boundary rather than passing invalid offsets to
   a budget helper whose precondition is a validated offset.
6. Preserve child read budgets at union execution, independently of memo
   branch-cap selection. Port `ExecuteProperties.clearSkipAndAdjustLimit` with
   saturating finite addition and Go's existing nonpositive unlimited convention.
   Return properties unchanged when `Skip <= 0` (both property setters accept negative skip;
   do not shrink a positive cap by adding it). Use the helper for concat,
   unordered and merge-sort unions, and multi-value IN unions. A singleton IN
   union returns the child directly with the original properties, without an
   additional skip/limit wrapper, as Java does.
   Java references: `RecordQueryUnionPlanBase.executePlan` and
   `RecordQueryInUnionPlan.executePlan`. Clearing both fields loses the cap;
   it does **not** imply draining whole branches, because these cursors stream.
   The measurable risk is excess range prefetch and a wider FDB read-conflict
   range. Pin this using real FDB: read one union row with WANT_ALL streaming,
   delete a distant unread row in another transaction, and commit the reader
   with an independent write. The finite child cap must avoid that conflict;
   an unlimited positive control must conflict. Also pin bounded pagination,
   finite/unbounded arithmetic, overflow and preservation of other properties.
   The conflict probe asserts serializable isolation and unsplit record storage;
   ID 20 lies beyond the cap plus lookahead/version allowance. Its unlimited
   control measures the actual returned extent, not an assumed batch size.
   Drive resume after a child `ReturnLimitReached` stop too. The bound relies
   on consuming at most one row per child per emitted union row: Java's ordered
   union deduplicates equal heads across children, not repeats within a child;
   pin that distinction. Intersection retains `ClearSkipAndLimit`, like Java's
   `RecordQueryIntersectionPlan`: arbitrarily many unmatched child rows may
   precede one output. This is a union execution audit, not a census of Java's
   other helper users (text, bitmap and record-store paths).
7. The conflict probe exposed a second property loss: executor record/index
   scan construction hard-codes ITERATOR rather than using the caller's
   `DefaultCursorStreamingMode`. Java's `ScanProperties(executeProperties,
   reverse)` inherits that mode. Use `recordlayer.NewScanProperties` (and
   `WithReverse`) in executor record, index, aggregate-index and vector-partition
   scan construction. The simulation hunt's separately configured scans are
   outside this executor audit. Zero is the valid SMALL enum, not an unset
   marker: do not normalize it to ITERATOR, which would make SMALL unexpressible.
   Add an unlimited SMALL control to the conflict probe: reading a prefix must
   not prefetch distant ID 20. WANT_ALL and SERIAL controls must still conflict.
   C++ `fdb_c.cpp` and Go's existing `ModeTargetBytes` pins establish SMALL's
   256-byte target versus 80000 for both WANT_ALL and SERIAL; SERIAL does not
   mean one row at a time. Correct the misleading record-layer enum comment.
   The aggregate-index leaf also omitted `RangeOptions.Mode`; forward its
   `scanProps.CursorStreamingMode` there, as `KeyValueCursorBase` does below
   Java's `StandardIndexMaintainer`. The aggregate SMALL arm exposed this second
   loss after constructor propagation was fixed. This pins the mode at a real
   FDB boundary rather than assuming that setting the field reaches it.
   Vector execution inherits the property too, but the ANN search uses its own
   point/range access algorithm; the prefetch-conflict assertion covers record,
   ordinary-index and aggregate-index leaves, not ANN internals.
8. The new direct-index control also exposed the same cap loss below the union:
   `newScanRangeSetCursor` clears both skip and row limit before opening each
   disjoint physical range. Preserve an adjusted child budget there too, using
   `ClearSkipAndAdjustLimit`. As with Java's IN union, skip and the semantic cap
   still apply once outside the range set. A leaf's returned-limit stop already
   retains its current odometer choice and continuation; only source exhaustion
   advances to another range. Pin the child budget independently as 7+11=18,
   preserve shared state and other properties, and cover multi-range paging.
   Non-1:1 operators (filter and distinct) must continue clearing skip/limit
   before their children: a result cap is not a raw-row cap below them. Pin
   small SQL LIMITs above each over a nonterminal signed-zero range set, with
   three initial rows that the filter rejects or distinct collapses.
9. A request-level `ExecuteProperties.Skip` must apply **after** a semantic LIMIT,
   not be forwarded below it. `executeLimit` currently lets the scan skip first,
   then applies the full semantic cap to later rows. For example, a request skip
   of two above `Limit(1, offset=1, Scan)` can return ID 4 instead of nothing.
   This is independently callable through `ExecutePlan` and is exposed by the
   singleton-IN delegation in decision 6. Compute child properties from
   `props.ClearSkipAndAdjustLimit()` first, then use the existing semantic-budget
   helper to clip the request's skip-plus-cap at the semantic remaining limit.
   Apply the original request skip and cap outside the LIMIT envelope using
   `applySkipLimit`, like Java's `skipThenLimit` at each parent operator.
   The envelope counts all its own rows, including rows discarded by that outer
   skip, so its semantic cap cannot extend. Request skip remains per-execution,
   like Java's `SkipCursor`; continuation bytes do not change. Pin finite,
   unbounded, exhausted-window and zero-skip cases both directly and through a
   singleton IN union, and resume after a request-capped page with skip cleared.
   Enforce the request cap on this operator's output too, not solely through
   child obedience to a budget. LIMIT supplies a derived child read budget;
   this differs from the transparent 1:1 map/projection delegation of the
   original request in decision 12. Add a filtered child that clears limits below
   itself and reapplies them after filtering, with request cap smaller than the
   semantic limit.

   Request skip is deliberately **not** promoted into a persisted SQL OFFSET.
   On an out-of-band stop mid-request-skip, a subsequent execution can request
   the remaining skip, just as with Java's `SkipCursor`, which forwards the
   inner stop and never encodes `skipRemaining`. The regression stops after
   one of two requested rows has been skipped, then explicitly requests the
   remaining one on resume. SQL OFFSET, in contrast, is the envelope's persisted
   semantic offset. Folding the request into it would change that separation
   and require rejecting a valid stacked `OFFSET MaxInt64` plus request skip 1
   when their sum overflows. Pin that unbounded large-offset request returning
   empty without an arithmetic error; do not introduce such a rejection.
10. The full executor suite's existing singleton-IN skip regression exposed a
    missing leaf boundary: `executeValues` never receives execution properties,
    so even direct `ExecutePlan(Values, Skip=1)` incorrectly emits its singleton
    row. Pass properties into the leaf and apply `applySkipLimit` outside its
    continuation-resumed list cursor, like Java's `RecordQueryExplodePlan` over
    constant arrays. There is no corresponding Java VALUES leaf class; replace
    the misleading `ValuesPlan` reference with that actual analogue. Retain the
    singleton-IN regression unchanged and add direct VALUES tests for skips
    0, 1, MaxInt and -1, a cap of 1, and skip-plus-cap ordering. A cap assertion
    must check `ReturnLimitReached` and the continuation, not merely one row:
    this leaf already produces only one row without a cap. Resuming that token
    must exhaust without repeating it.
11. A shared-leaf audit found that record-layer aggregate helpers construct
    `ScanProperties` with no streaming mode: atomic aggregation newly reaches
    SMALL after the count-leaf fix, while VALUE MIN/MAX already did. Java's
    atomic, value, bitmap and permuted aggregate helpers instead derive scan
    properties from `ExecuteProperties.newBuilder()` (ITERATOR by default).
    Use `NewScanProperties(DefaultExecuteProperties()...)` at these aggregate
    boundaries and the permuted-extremum probe; preserve supplied isolation,
    direction and row caps. The extremum probe supplied no isolation and was
    therefore SNAPSHOT: change it to SERIALIZABLE, like Java's FORWARD/REVERSE
    scan defaults, with a fresh ScanState like Java's `clearState()`. A deleting
    transaction replacing MIN 5 by 9 (or MAX 9 by 5) must conflict with a
    concurrent insertion of 7, then retry to publish 7. Both stale replacements
    committed before this fix; the real-FDB regression pins the conflict and
    the correct post-retry aggregate.
    The permuted-MIN null repair must inherit `callerProps`' default streaming
    mode with `NewScanProperties(callerProps)`. The aggregate executor must
    retain the enclosing scan's effective mode for this repair too, including
    a `WithStreamingMode` override; do not rely on the two fields coinciding.
    Pin both the aggregate and repair reads with an explicit nondefault mode.
    The SPFresh record-scan batch has the
    same omitted-default shape and must use the default-properties constructor
    too; its supplied isolation and batch cap remain unchanged. Statistics
    collection already explicitly sets ITERATOR and is not changed.
    Keep the exported Go enum's numeric values: zero is the existing SMALL
    value, not an unset flag. Reordering it would change that public API and
    still not propagate a caller's nondefault mode through a struct literal.
    Explicit default construction mirrors Java's builder without that change.
    Pin these paths with real FDB reads and transparent transaction observers
    that record the actual `RangeOptions.Mode`, forwarding every operation to
    the real transaction. Require a nonempty observed population and correct
    aggregate results, not only a constructor-unit assertion.
12. Do not apply 1:1 delegation to filter, hash/streaming distinct (including
    primary-key distinct's delegation to the hash helper), or DML. Filter and
    distinct clear child budgets and bound their own outputs; DML clears and
    does not reapply, matching Java's data-modification and temp-table-insert
    plans. Map/projection over temp-table insert consequently also ignore
    request skip/cap, as Java does; pin both owning and non-owning insert.
    Mutation verification of the direct filter/distinct pin exposed another
    mask: projection clears the child cap too, so neither operator receives a
    finite request in the original physical shape. Java's `RecordQueryMapPlan`
    forwards the original execution properties to its child and maps only the
    returned rows. Match that in Go's map and projection executors: forward
    properties unchanged and remove their outer skip/limit wrappers. These are
    1:1 operators, unlike filter/distinct. Pin map/projection prefetch budgets
    with the real-FDB conflict probe and prove a request-skipped row is not
    evaluated (integer division by zero must still error without the skip).
    These operators are 1:1 and do not wrap continuations. Over a leaf and a
    filter child, assert exact output caps, the child's returned-limit reason
    and continuation bytes, and that request skip is reapplied only when the
    caller supplies it on resume. Unlike decision 9, these operators delegate
    the original request, not a derived read budget.
    Then repeat the filter/distinct cap mutations: the direct one-execution
    test must fail, while the SQL pagination test remains green. Do not claim
    the instrument catches that regression until this mutation actually fires.
13. First-or-default is not default-on-empty. Java's
    `RecordQueryFirstOrDefaultPlan.executePlan` forwards the original request
    into its child and then takes the first row or fabricates the default; it
    does not apply a second skip/cap to its singleton. Preserve this behavior
    for the non-strict port, including under transparent map/projection, and
    pin it against the live JVM (child skip can select the second row or the
    default). Default-on-empty instead bounds the complete output, explicitly
    in Java. Do not replace one contract with the other.
    The Go-only **strict** scalar-subquery extension has a different invariant:
    it must inspect a second input row to establish cardinality. A request cap
    of one currently hides that row; forwarding request skip can hide it too.
    For strict first-or-default, clear child skip/cap and apply the original
    request to the completed first/default result. Preserve the existing
    out-of-band checkpoint/restart and consumed-token handling. Pin empty,
    singleton and two-row inputs, direct and mapped execution, request skip
    and cap, terminal reasons and continuation resume. Two rows must raise
    21000 even when the request cap is one or request skip would discard the
    result: validation precedes output skipping. A syntactic child LIMIT of
    one is different and still legitimately has scalar cardinality. Pin the
    existing SQL correlated-scalar shape with outer LIMIT 1, a FlatMap strict
    inner leg, and 21000; this caller already clears the request before that
    leg. Pin a real-FDB scanned-row stop after the first scalar row as a
    restart, not a valid scalar, then resume with enough scan budget and
    observe 21000. A request-skipped singleton exhausts with END (not a token
    that can legally be resumed); emitted-row consumed tokens still resume
    empty, with no continuation-format change.
    A child cap of **two** is not a safe refinement: record scans can spend
    it on a Customer and one Order, then stop in-band before a second Order.
    The strict input must therefore remain uncapped. Real-FDB mixed-type
    cases pin this boundary and the before-first-row OOB checkpoint. At the
    resulting 12-case FDB population, adding a child cap of two fails all
    three mixed-type/in-band cases while the other nine cases pass.

Executor constructor census at `2afdb29a3` (four matching lines, scoped to these
non-test files):

```sh
git grep -n 'CursorStreamingMode: recordlayer.StreamingModeIterator' 2afdb29a3 -- \
  pkg/recordlayer/query/executor/executor.go \
  pkg/recordlayer/query/executor/executor_new_plans.go
```

No new SQL syntax, persisted bytes, or cross-engine continuation contract changes.
The existing SQL/executor behavior of representable LIMIT operators is retained.

## Verification

* Rule tests: offset sums at MaxInt64 and just beyond; nested finite and unbounded
  caps; skip-only union offsets 0, 1, 2, 5 and MaxInt64; finite branch caps at and
  beyond the addition boundary; zero limits and runtime caps remain covered.
* Real-FDB SQL scenario `limit_offset_bounds.yaml`: nested overflowing offsets
  return no rows, ordinary nested windows still return the correct row, and a
  large standalone finite limit still returns the tail. Pin SQL's rejection of
  an OFFSET-only union separately. Add a driver test for parameterized UNION
  limits, repeated binds and EXPLAIN, asserting both bounded plan and row count.
* Real-FDB logical API regression: a skip-only limit above UNION ALL retains all
  rows after the skipped prefix, with the full planner and executor in the path.
* SQL pagination can mask a filter/distinct child-cap regression by resuming
  empty pages. In addition to SQL row assertions, drain one direct `ExecutePlan`
  invocation over the signed-zero range-set shape: it must produce the three
  filtered/distinct outputs before its returned-limit stop. Pin this boundary
  independently of the driver's automatic continuation loop. The final filter
  mutation produced `[]` instead of `[4,5,6]`; the hash-distinct mutation
  produced `[a]` instead of `[a,b,c]`. The SQL pagination controls passed in
  both runs. The earlier surviving mutation exposed projection's clearing;
  map/projection delegation is required for this instrument to discriminate.
* Strict scalar request matrix: 48 cases (3 wrappers, 4 input shapes, 2 skips,
  2 caps), plus 12 real-FDB cases for direct/mapped execution, homogeneous
  and mixed record types, and in-band caps versus OOB checkpoint/restart. Removing strict child clearing fails the two-row/cap-1 assertions
  while the SQL FlatMap strict-inner control remains green. Non-strict behavior
  is pinned separately against live Java core plans, not claimed as SQL reach:
  27 value-bearing observations (3 Go wrapper forms, 3 child sizes, 3 skips)
  agree, including second-row selection and default fabrication after child
  skip. The mapped empty-record reproducer also emits 42, as Java requires.
* Register new files with Gazelle; see each new test execute under Bazel. Run
  the relevant targets, `just test`, bounded fuzzing of LIMIT rewrite semantics,
  and the million-row stress target before and after the production changes.
  Regenerate `FEATURE_MATRIX.md` with `just feature-matrix` and the SQL ledgers
  with `just sql-coverage` for the new scenario.
* Final production-code fuzz reruns: `FuzzExecuteProperties_AdjustLimit`,
  21,461,670 executions in 15 seconds; `FuzzLimitMerge_Window`, 7,707,689
  executions in 15 seconds. Both ran uncached under Bazel with their seed
  populations nonempty and no failures.

## Reproduction on 42a79173557936707a32a969e51489667db6ff01

Fresh Bazel runs, before production edits:

* `TestYamsqlConformance/limit_offset_bounds`: 1 of the initial 5 assertions
  failed. The MaxInt64 inner offset plus outer offset 1 returned `[1]` instead
  of no rows; the other 4 assertions passed. The existing rewrite really is
  reachable through this derived-table SQL shape.
* `TestIntegration_UnboundedUnionLimit`: the full planner produced
  `Limit(-1, offset=2, UnorderedUnion(Limit(1, Scan(Order)), Limit(1, Scan(Order))))`.
  Real FDB execution returned 0 rows instead of the required 4.
* In the 5-arm unbounded-union rule test, offsets 2, 5 and MaxInt64 failed;
  offsets 0 and 1 are positive controls for behavior already correct.
  Both overflow-precondition arms of the merge test failed; only outer offset
  1 also changes rows (outer offset MaxInt64 has an empty window either way).
* `TestFDB_UnionLimitParameters` passed on unchanged production code: direct
  execution and prepared reuse each returned the requested row counts for caps
  4, 1, 3, 4 with OFFSET 2; direct and prepared EXPLAIN each showed the bound cap
  in all four iterations. A bound negative SQL limit was rejected with 42601.
  This pins those driver paths, not every possible caller of the embedded API.

The parameter EXPLAIN pin intentionally compares complete plans for the **same**
cap, not across different caps. The pre-fix cap-3 plan had no branch caps while
caps 1 and 4 did. `designationScope.compare`'s REWRITING tiers count selects,
table functions and predicates, not LIMIT nodes; tied alternatives are ordered
by deep semantic hash, which includes the cap. This is not a monotone row-count
threshold. Repeated execution must still be deterministic for a given binding.

## Performance and alternatives

The checks are constant-time and do not change the search for ordinary bounded
limits. Keeping stacked operators costs another cursor only where no exact
single offset can represent the composition. Saturating or wrapping semantic
offsets is rejected on correctness grounds; saturating an execution read budget
is the existing record-layer approach (`cursor.go:saturatingAdd`). Increasing
integer width across the SQL, planner, executor and continuation APIs is
unnecessary: the existing nested representation already expresses both skips
exactly.

## Final verification checkpoint — 2026-09-08

Implementation commit: `e4c0e8ee807967a97989aa9eb5b1206f8563a9de`. Graefe, Torvalds and the independent
Codex review ACKed the implementation and final deltas. The non-strict
first/default NAK was withdrawn after the live-JVM value probes; the suggested
strict child cap of two was withdrawn after the mixed-record mutation failed.

An uncached full sweep passed 91/91 targets before the final mixed-record
test/comment refinement. The complete executor and docscheck targets then
passed uncached (2/2), including all 12 mixed/homogeneous strict-FDB cases.
Final `just test` passed 91/91 (19 executed, 72 cached), with source checksums
unchanged across the run; the commit hook also passed 91/91. These are distinct
execution populations, not a claim that the final invocation ran 91 targets
uncached. Gazelle, module tidy, feature/SQL ledgers and `git diff --check` passed.

### Matched 1M stress

Baseline `42a79173557936707a32a969e51489667db6ff01` (the merge-base at measurement) versus
`e4c0e8ee807967a97989aa9eb5b1206f8563a9de`. Four runs per side, sequential **ABBA twice**.
Both worktrees used the same `/home` XFS filesystem, 97% utilized (about 35 GiB
free); the ext4 threshold does not establish an XFS latency bound. Recorded
one-minute host loads ranged 0.47–3.14. No concurrent heavy build/test workload.
Each run passed uncached with 24 `=== RUN` lines (root plus 23 query arms),
identical row counts, and source checksums unchanged afterward.

```sh
bazelisk test //pkg/relational/sqldriver/stress:stress_test \
  --nocache_test_results --test_output=all \
  '--test_arg=-test.run=^TestFDB_Stress_1M$' --test_arg=-test.v \
  --build_event_json_file=/tmp/bughunt-stress-<run>.bep.jsonl
```

Baseline total seconds: 177.53, 177.22, 177.54, 179.41.
Changed total seconds: 177.16, 177.74, 177.76, 177.30.
Ratio of means: **0.998x**.
This small sample is not a statistical speedup claim. The first pair's apparent
status-count/join slowdown did not retain that magnitude in the repeated
matched runs; report the measured ranges, not an inferred error bound.

Query timing ranges below are milliseconds across **four runs per side**;
COUNT(*) uses the subtest timer because that arm logs no separate query timer.

| Query arm | Rows | Baseline ms | Changed ms | Mean ratio |
|---|---:|---:|---:|---:|
| PK lookup id=0 | 1 | 8.56–17.95 | 8.41–15.61 | 0.951x |
| PK lookup id=N/2 | 1 | 7.42–18.70 | 8.38–18.70 | 1.036x |
| PK lookup id=N-1 | 1 | 5.42–13.30 | 6.34–14.73 | 1.096x |
| idx_customer eq | 8 | 6.75–17.23 | 6.53–19.40 | 1.063x |
| idx_amount range >9000 | 100017 | 191.19–259.43 | 190.24–284.59 | 1.082x |
| idx_status count pending | 1 | 323.56–411.98 | 341.66–386.46 | 1.017x |
| full scan filter amount>5000 | 1 | 545.55–566.70 | 549.18–592.68 | 1.014x |
| GROUP BY status | 4 | 6.04–6.30 | 5.82–7.62 | 1.036x |
| GROUP BY status COUNT only | 4 | 5.34–5.55 | 5.39–5.63 | 1.020x |
| SUM by status (aggregate index) | 4 | 5.62–5.78 | 4.84–5.93 | 0.975x |
| GROUP BY customer HAVING | 47271 | 580.87–700.56 | 578.14–705.09 | 0.993x |
| JOIN 10 orders x customers | 10 | 20.01–33.70 | 21.66–42.58 | 1.066x |
| ORDER BY PK (full) | 1000000 | 3833.98–3959.19 | 3879.44–3961.44 | 1.000x |
| ORDER BY PK + index filter | 8 | 8.79–9.16 | 8.91–11.87 | 1.092x |
| scan all rows ordered | 1000000 | 3666.78–3703.13 | 3658.44–3693.57 | 0.997x |
| scan all rows wide | 1000000 | 3938.25–3977.20 | 3918.23–3977.97 | 0.997x |
| IN-list 5 values | 46 | 19.02–23.97 | 18.68–24.23 | 1.040x |
| PK needle id=999999 | 1 | 5.76–6.44 | 5.70–6.38 | 0.984x |
| PK+filter needle id=500000 | 1 | 7.25–7.77 | 7.39–7.71 | 1.019x |
| full scan sparse filter | 97 | 3349.70–3363.65 | 3317.70–3343.63 | 0.992x |
| UPDATE by index | 8 | 8.92–9.93 | 8.82–9.14 | 0.974x |
| DELETE single row | 1 | 6.33–7.32 | 6.43–6.58 | 0.979x |
| COUNT(*) | 1000000 | 3100.00–3160.00 | 3100.00–3210.00 | 1.006x |

Re-inspectable session artifacts: `/tmp/bughunt-stress-before-{5,6,7,8}` and
`/tmp/bughunt-stress-after-{3,4,5,6}`, each with `.log`, `-tests.log`, `.meta`,
`.bep.jsonl`, `.md5` and `.check.log`. They replace the earlier after runs that
predated the follow-up. The committed tables retain the conclusions independently
of those temporary logs; the regression tests retain the correctness proofs.

## Decision 14 — default expressions retain their evaluation context

A further core-API probe found that both empty-input operators evaluate defaults
with `Evaluate(nil)`, including each record-constructor field. Java's
FirstOrDefaultPlan:110 and DefaultOnEmptyPlan:117 instead evaluate with the
execution's store/context. Go consequently replaces bound parameters and outer
correlations with NULL and reads wall time instead of the statement clock.
The 15-case `TestDefaultEvaluationContext` probe (three operators × five default
shapes) failed in every arm before the fix. The clock assertion expects the
scalar function's documented formatted string, not a Go time.Time.

Implementation: thread the binding-only `evalCtx.RowContext()` through default
materialization, including the per-field constructor path. Never bind a consumed
child row as the default's frontier. Keep evaluation lazy on empty input, keep
strict/non-strict request and continuation contracts unchanged, and propagate
evaluation errors. Preserve duplicate constructor fields by ordinal.

The same probe exposes a second arm: a non-constructor RECORD default accepted
only NULL, although Java accepts an evaluated record. The shared materializer
copies an evaluated ordinal record or protobuf message into the exact declared
output carrier. Positional rows must agree in width and type, permitting root
nullability reconciliation; protobuf rows use the storage-shape admission below.
There is no name-map fallback, fabricated correlation identity, or re-evaluation
per field. A record NULL has an absent-current marker; a present all-NULL record
remains present. Scalar defaults retain their declared type.

The presence matrix also caught `normalizeDefaultOnEmptyResult` dropping a
child's whole-record NULL marker while replacing its layout. Preserve/rebase
that carrier-level absence to the parent's identity layout; never forward the
child's source-window identities. `TestDefaultOnEmptyPreservesNullChild` pins
this independently of default construction.

The binding-only context does not invent a frontier, but an already-bound free
correlation remains valid even when its identifier equals the physical child
edge's alias. Java's correlated-to computation reports this dependency; it does
not forbid it. The live-JVM/core probe pins eight value comparisons (two
operators × empty/nonempty × parameter/same-edge-alias correlation), all 11 for
nonempty inputs and 99 for empty inputs. Java correlation bindings hold
QueryResult; parameter bindings hold the raw scalar. An alias-exclusion gate
would reject inputs Java accepts and is not added.

The shared helper preserves nil-default and nil-context behavior. The complete
executor suite caught a removed nil-default guard through the existing exact
mapped-empty-record regression; restoring the guard fixed the panic. The
result-type census moves GUARDED 10→9 when the duplicate DefaultOnEmpty helper
is removed; PROPAGATED remains 27 because FirstOrDefault's read survives in the
shared helper. RFC-213 and its unit ratchet record this population change while
retaining RAW=0.

### Protobuf storage admission

The initial protobuf comparison reconstructed declared fields with
`NewRecordType`, which panicked on duplicate names. The same pattern already
existed in `explodeElementRow`; that bridge also rejected a valid present object
under a nullable record declaration. A root-only comparison was insufficient:
a live JVM probe accepts and reads Order.tags `[a]` under a nullable ARRAY
declaration backed by a plain repeated field, while Go rejected the record.
`NullableArrayTypeUtils.unwrapIfArray` leaves an existing List alone under
either nullability and unwraps a Message only for a nullable ARRAY.

`ProtoRecordDescriptorCompatible` now reuses values' existing cached descriptor
shape checker through an exact handle. The array rule is
`!wrapped || expected.nullable`. Scalar/record logical nullability is already
independent of protobuf presence: DDL emits optional fields even for NOT NULL.
Widths and recursive value/storage shapes remain checked, including the existing
storage aliases. Logical aliases do not have to equal descriptor field names;
reads address ordinals. A record's own name is provenance, but logical **field**
names remain part of Field equality. Unwrapped storage cannot distinguish an
absent array from an empty array; both are `[]`, the same limitation Java has.

The JVM duplicate-name probe rejects logical type construction with
IllegalArgumentException (Multiple entries with same key). Go's exact handles
also serve machinery-owned ordinal rows and intentionally admit duplicate
columns. Protobuf output admission therefore checks name unambiguity explicitly,
recursively, without reconstructing the type. It reports a typed runtime error
rather than panicking; this differs from Java in rejection timing, not in which
Java-accepted inputs it admits. Raw ordinal constructors retain duplicate names.
Name unambiguity shares the immutable descriptor-verdict memo, avoiding a name
set allocation per output row. Field reads retain their existing ordinal/name
contract rather than gaining an additional duplicate-name restriction.

Default protobuf rows and generic Explode/Stream materialization use this single
admission authority and stamp the exact declared carrier only on success. The
literal-constructor fast path retains its frozen source-type fence: width,
field order, or field-nullability mutation is a typed failure, never a fall-through to
broader storage admission. Descriptor pointer identity alone is not a semantic
mismatch. A live JVM regression executes one inline-record Explode plan against
two independent TypeRepositories and verifies both descriptor identities and
`[[7,9],[7,9]]`. The Go conformance companion and foreign-descriptor unit arm
accept equivalent repositories; the source-mutation negative arms stay strict.

The final review caught another valid input: a present literal constructor is
non-null even when the declared array element type permits NULL. The source
fence must reconcile root nullability without relaxing any field. The committed
JVM probe now runs both root nullabilities through both independent repositories;
it confirmed Java success before the Go root-fence fix. The direct
`TestExecuteExplode_NullableLiteralElement` pin drives the same shape. This proof
uses a present literal with an explicitly nullable element declaration, not a
claim that Java's immutable array constructor can contain a null value.

Materialization also must not reconstruct an exact handle per element. The
executor now obtains the frozen handle directly from the plan's result Value
once per cursor and passes it through literal and generic row admission. The
Stream arm likewise snapshots its element type outside the row loop. A profile
of the retained 128-row `TestExecuteExplode_ProtoTypeAllocation` fixture located
the repeated snapshot/intern walk, and restoring that walk mutation-fails its
allocation ceiling (six allocations per materialized fixture row plus a small
fixed cursor allowance). `ExactTypeHandle.Type()` already returns a shared
thaw; this was redundant re-snapshotting, not fresh thawing on every read. The
allocation pin exercises the executor, separately from the zero-allocation warm
descriptor-admission pin.

The new test populations are 15 context cases, six lazy-error cases, 42 record
materialization cases, six real-FDB empty/nonempty cases, and 24 array-storage
cases (two nesting depths × wrapped/plain × nullable/non-nullable ×
absent/empty/populated). The storage cases exercise both output admission and
FieldValue descent, including arrays under two nested records. Additional pins
cover malformed declarations, duplicate names at depth, scalar/enum storage
aliases, nil descriptors/handles, warm-cache allocation, NULL primary rows,
source mutation, and independent repositories. Fuzz targets cover record
default materialization and concurrent descriptor-verdict replacement.

This is core-API reach, not a claim of new SQL wrong-row reach: SQL currently
uses typed literal defaults in its generated scalar/outer legs. Existing SQL
scalar-subquery, outer-join, and UNNEST cases remain controls. Live Java/core
checks are retained in `continuation_conformance_test.go`, not throwaway probes.
The final review/full-suite/stress checkpoint for this decision follows below;
the earlier committed stress table describes decisions 1–13, not this follow-up.

### Decision 14 draft checkpoint

The implementation and its final delta received Graefe, Torvalds, and independent
Codex ACKs. Both final review findings (valid nullable literal roots and per-row
exact-type re-snapshotting) have retained regressions and compiled mutation reds.
The misleading Explode getter fallback comments were then corrected; no further
runtime behavior changed after the full uncached run.

Verification at this checkpoint:

- Full non-stress Bazel suite: **91/91 freshly executed, passing**, with source
  checksums unchanged across the run. `just test` also passes 91/91. After the
  comment correction, `just test` passes 91/91 again (two executed, 89 cached).
- Focused race run: executor and values targets both pass uncached, including
  the new context, record-shape, nested-array, allocation, and fuzz-seed pins.
- Three live JVM specs pass with 18 explicit result lines: eight bound-default
  comparisons, eight protobuf-shape cases, and two root-nullability cases each
  exercising two independent type repositories in both engines.
- `FuzzProtoRecordDescriptorAdmission`: 29,604,211 executions over 30 seconds;
  `FuzzDefaultRecordMaterialization`: 11,834,566 executions over 30 seconds.
- Five final mutations are present, compile, and fail the intended assertions:
  raw root equality, restored per-row snapshot, old array-nullability predicate,
  removed duplicate-name gate, and disabled source-field fence. All are restored.
  The earlier default-context and whole-record-presence mutations also failed
  their retained regressions.
- Gazelle, module tidy, SQL coverage generation, and whitespace checks pass.

Session artifacts are `/tmp/bughunt-admission-final-{full,java,mutations-summary}.log`,
`/tmp/bughunt-admission-{race,comment-just-test}.log`, and the corresponding checksum
files. The tests and the evidence populations above remain in the repository.
**The final-source matched million-row comparison is still outstanding.** The
preceding stress tables are explicitly for decisions 1–13; they are not promoted
to evidence for decision 14. The TODO checkpoint stays open for that work while
the owner reviews the draft PR.

### CI allocation-measurement isolation

PR #771's unit job at `e3197dae0cb110fc2c2a2f1daec5590a87060de4`
exposed a flaw in the allocation regression harness: `testing.Benchmark` reads
process-wide MemStats, so other parallel tests and FDB client goroutines can
inflate its counts. A retained channel-handshake test demonstrates that an
allocation made in another goroutine is charged to the measured iteration.
This was a test-isolation defect, not evidence permitting a higher ceiling.

The test-only `pkg/testutil/allocs` helper executes an allocation assertion in
an exact-filter subprocess. Parent tests remain parallel; the executor's
TestMain omits background FDB services only for that exact child invocation.
The child retains the original assertion and threshold. It removes inherited
Bazel filtering/sharding/report controls, keeps runfiles paths, and has both a
Go test timeout and a parent process timeout. Success requires an actual RUN,
PASS, and exactly one assertion-completion marker, not merely exit status zero.
Helper pins cover invocation validation, inherited controls, empty/duplicate
results, and an allocating parent with an allocation-free child.

This wraps the Explode executor pin and the three values-layer Benchmark
assertions for descriptor admission, scalar shapes, and quantified-row shapes.
Five uncached repetitions of all tests in the helper, executor, and values
targets passed. Each of the five Explode measurements reported 770 allocations
for 128 rows against the unchanged 784 ceiling. Restoring the per-row snapshot
compiled and failed in the isolated child at 1,026 allocations; the production
source was then restored and checksum-verified. The helper protocol fuzz target
passed 19,211,190 executions over 15 seconds. Artifacts are
`/tmp/pr771-alloc-repeat.log`, `/tmp/pr771-isolated-snapshot-mutation.log`, and
`/tmp/pr771-alloc-fuzz.log`; the tests themselves are retained in the repository.
This correction changes test execution only, not query behavior or the earlier
JVM evidence. Final full-suite, race, review, and stress results follow below.

The isolation review also tightened the helper's supported scope: it accepts
only top-level tests, rejecting subtests before starting another process. Go's
slash-split test filter does not give a full-name regexp the same anchoring
semantics, and subtest parent setup could itself start concurrent work. The
negative invocation pin and a real failing-child regression enforce this
boundary. The environment pin preserves `TEST_SRCDIR`, `TEST_WORKSPACE`, and
`TEST_TMPDIR` as well as `RUNFILES_DIR`. It redirects `COVERAGE_OUTPUT_FILE` and
`GOCOVERDIR` to a child-private directory: rules_go's generated test main writes
the former profile directly, whereas its `COVERAGE_DIR` LCOV output uses unique
filenames and remains shared for Bazel's merger. The allocation-noise control
is rate-limited instead of spinning. The earlier 92-target full uncached pass
and `just test` pass preceded these harness refinements; final evidence must
exercise the refined helper too.

The refined helper passed the full non-stress suite (92/92 freshly executed),
`just test` (92/92, two executed), and all three helper/executor/values race
targets uncached, with frozen source checksums unchanged. The repeated target
run again measured 770 allocations in each of five Explode executions. Its
protocol fuzz passed 18,922,139 executions over 15 seconds. Live Bazel coverage
also passed: the merged LCOV report includes the child-only `measure()` and
completion-marker lines, verifying child coverage survives `GO_TEST_WRAP=0`.
These results are in `/tmp/pr771-isolation-refined-{full,race,just-test}.log`,
`/tmp/pr771-isolation-reviewed-repeat.log`, and
`/tmp/pr771-isolation-{coverage,final-fuzz}.log`.

A final clarification makes the isolation pin's reasoning explicit: the child
invocation check establishes isolation while the parent allocator is active;
the zero-allocation no-op measurement is a separate assertion, not a sensitivity
claim about rate-limited noise (integer allocations/op can round that noise to
zero). Malformed marked children now fail before spawning again, with a retained
real-child regression. The negative-test harness also rejects a malformed marker
rather than treating it as a fresh parent. Final validation of these small guards
is recorded with the commit checkpoint below.

The final allocation-site sweep also found the existing HNSW byte-distance
`AllocsPerRun` assertion in the recordlayer package. A serial test does not
isolate process-wide counters from FDB client goroutines left by the Ginkgo
suite. It now uses the same child helper and a parallel-parent Benchmark,
retaining the zero-allocation requirement across the same three metrics and
rejecting zero measured iterations or a declined byte-direct path. No additional
TestMain bypass is needed: Ginkgo setup belongs to `TestRecordLayer`, which the
exact HNSW child filter does not select. This brings the migrated allocation
assertion population to five (four executor/values sites plus one recordlayer
site); the earlier four-site measurements retain their stated population.

### Allocation-isolation commit checkpoint

The five-site test delta and all review follow-ups received virtual Graefe,
virtual Torvalds, and independent Codex ACKs. The final `just test` run passes
92/92 (three freshly executed, 89 cached); the preceding full uncached checkpoint
executed 92/92 successfully. Final helper/executor/values race targets pass
uncached, as does the full recordlayer race target after its HNSW migration.
All corresponding frozen-source checksum checks pass. Final helper coverage
passes and includes the child-only measure/marker lines; protocol fuzz passes
19,271,222 executions over 15 seconds. Five HNSW repetitions pass, and adding a
query-vector clone compiles and fails all three metrics at one allocation/op.
The mutation was restored in `hnsw_distance_bytes.go` (SHA-256
`229fb4f345fdcd6ef4386fde9e09f3e56961ef96ebab6b37a43b3899f0936a49`).

The review's suggested precision difference was checked against Go's testing
source: both `AllocsPerRun` and `BenchmarkResult.AllocsPerOp` divide as integers;
there is no fractional precision downgrade. `AllocsPerOp` also guards `N <= 0`,
so reporting a failed empty benchmark does not divide by zero. The malformed
child and subtest-filter mutations compile and redden their retained guards.
No allocation threshold or production code changed in this CI correction.

Final artifacts are `/tmp/pr771-isolation-final-just-test.log`,
`/tmp/pr771-isolation-closure-{race,coverage,fuzz}.log`,
`/tmp/pr771-hnsw-{isolated-repeat,final-race,allocation-mutation}.log`, and
`/tmp/pr771-helper-guard-mutations.log`. Final-source matched million-row stress
and GitHub CI/review are the remaining PR gates, not inferred from earlier runs.
