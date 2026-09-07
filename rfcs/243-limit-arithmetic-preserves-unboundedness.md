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
