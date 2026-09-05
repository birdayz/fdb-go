# RFC-242 — A union's legs are aligned once, by the translator

**Status:** DRAFT r2. r1: Graefe ACK with one non-blocking condition (assert leg alignment at
the logical constructor too — folded as § Fix C); Torvalds NAK with five findings, all folded:
a pin that passed with the defect present now asserts no leg is a `Map` (test plan 2); the
translator's join-leg gate built on the deleted remap premise is deleted too (§ Fix D); two
census figures were overstated and are restated with the commands that produce them; the
stale references are swept (§ Fix E); the scratch probes are gone from the tree. Awaiting r2
confirmation from both.
**Area:** Cascades implementation rule `ImplementUnorderedUnionRule` and the union executors
(query-engine gate: Graefe + Torvalds).
**Found by:** the engine fuzz nightly, red on `FuzzPlanner_WithBatchA_NoPanic` on 2026-08-30,
08-31, 09-03 and 09-04 (crashers `455b6e6975d288bc`, `9027cb5e7737c749`, `d98799d80e18558c`);
reproduced locally in under two seconds of fuzzing as corpus entry `4a35dedaab03e663`.
**Wire impact:** none. Read path only; no key, record, index or continuation bytes change.

## Problem

A valid `UNION ALL` over a table whose columns were declared with quoted lower-case names does
not plan:

```sql
CREATE TABLE t ("id" BIGINT, "k" BIGINT, PRIMARY KEY ("id"))
SELECT * FROM t UNION ALL SELECT "id", "k" FROM t            -- fails
SELECT "id", "k" FROM t UNION ALL SELECT * FROM t            -- fails
SELECT * FROM t WHERE "k" > 10 UNION ALL SELECT "id", "k" FROM t   -- fails
SELECT * FROM t UNION ALL SELECT "id", "k" FROM t WHERE "k" > 10   -- fails
SELECT * FROM t UNION ALL SELECT * FROM t                    -- plans
SELECT "id" AS w, "k" AS x FROM t UNION ALL SELECT * FROM t  -- plans
```

The same six shapes over `CREATE TABLE u (ID BIGINT, K BIGINT, PRIMARY KEY (ID))` all plan.
Measured over SimFDB with a nine-query probe (the six above plus three upper-case controls):
four fail, five plan and return the six expected rows. The four fail in two different ways,
depending on which leg is the bare scan:

| shape | EXPLAIN | rows |
|---|---|---|
| `SELECT * … UNION ALL SELECT "id", "k" …` (scan leg first) | `XX000 internal error while planning query: unclassified planner failure` | — |
| `SELECT "id", "k" … UNION ALL SELECT * …` (projection leg first) | plans | `resolution error 46 at qov.binding: exact QOV "q$121" (RECORD<id LONG NULL, k LONG NULL> NOT NULL) has no declared runtime binding` |

Same defect, two exits: with the scan leg first the rename produces a leg the union rejects;
with the projection leg first the rename produces a leg the union *accepts* and the executor
cannot bind. Both are the rename firing where nothing needed renaming.

The fuzz target that found it drives the planner API directly, where the same failure shows as
an error from the physical union's constructor:

```
RecordQueryUnorderedUnionPlan result Value: input quantifier 0 type
  PlannerFuzzRow RECORD<K LONG NOT NULL, g LONG NULL, x LONG NULL> NOT NULL
disagrees with input quantifier 1 type
  RECORD<K LONG NOT NULL, G LONG NULL, X LONG NULL> NOT NULL
```

The two legs entered the logical union with the *same* type — the fuzz fixture checks
`Type.Equals` before it builds a union, and the SQL translator states one exact row for every
leg (§ Investigation). Planning then renamed one leg's fields to their upper-case spelling and the
union refused its own rename. Nothing is missing; two spellings of one name were compared under
two different normalizations, which is the defect class RFC-237 exists to end.

## Investigation

### Three mechanisms align a union's legs. Java has one.

| layer | Go | Java |
|---|---|---|
| SQL translator | `exactUnionResultRow` derives one exact positional row (names from leg 0, `MaximumType` per slot) and `normalizeUnionLeg` re-emits any leg that differs as a projection onto that row (`cascades_translator.go`) | `SemanticAnalyzer.validateUnionTypes` folds `Type.maximumType` over the legs; `LogicalOperator.generateUnionAll` wraps any leg whose type differs in a promoting select (`LogicalOperator.java:604-646`) |
| implementation rule | memoize each leg's plan partition, **then read every leg's column names off its physical plan, compare, and wrap a differing leg in a rename `Map`** (`rule_implement_unordered_union.go:92-146`) | memoize each leg's plan partition and yield `RecordQueryUnorderedUnionPlan.fromQuantifiers` (`ImplementUnorderedUnionRule.java:57-71`). Nothing else. |
| executor | `executeUnorderedUnion`, `executeUnionStreaming` and `executeUnionBuffered` each read every leg's column names off its plan again (`planColumnNamesWithMD`) and re-type a differing leg's rows by position (`remapUnionColumnsByPosition`); `executeUnion` additionally chooses a non-resumable *buffered* path when a leg's names cannot be read statically | `RecordQueryUnionPlanBase` takes the first leg's type (`RecordQuerySetPlan.mergeValues`, "let's just pick the first result type for now") and the cursors concatenate rows as they are |

The translator layer is the port of Java's. The other two are Go-only, and they date from before
the translator did this work: RFC-078 added the executor remap in 2026-06 for aggregate legs with
mismatched aliases, RFC-080 relaxed its gate, RFC-183 §14–15 and RFC-184 W2 made the rule's rename
`Map` a memo-reachable compensating operator. RFC-226 (2026-08-10) then made every projection state
its row and the translator state one exact row per leg, at which point every leg of every SQL
union carries the *same* `RecordType` into `NewLogicalUnionExpression`, and the physical
constructor (`newPlanExprBaseForFirstQuantifier`, `plan_expression.go:196-208`) refuses any leg
whose flowed type is not `Equals` to leg 0's.

So at the point either Go-only mechanism runs, the legs are already exactly aligned or planning
has already failed. There is nothing left for them to align.

### What they do instead: fold case on one path and not the other

`physicalPlanColumnNames` (the rule's walker) answers from the first arm that matches:

| leg's physical top | names come from | folded? |
|---|---|---|
| `RecordQueryProjectionPlan` | `GetOutputNames()` | no — exact |
| `RecordQueryMapPlan` | result `RecordConstructorValue` field names | **`strings.ToUpper`** |
| `RecordQueryStreamingAggregationPlan` | returns nil (rename skipped) | — |
| anything with `GetInner()` | descends | — |
| terminal (`Scan`, `CoveringIndexScan`, …) | `GetResultType()` record fields | **`strings.ToUpper`** |

`planColumnNamesWithMD` (the executor's walker) has the same shape with the same two folds at the
tail (`executor.go:3031-3048`), plus a third over the proto descriptor.

A SQL union leg is a bare scan whenever the star projection is an identity over the whole row and
is eliminated: `SELECT * FROM t1 UNION ALL SELECT id, col1, col2 FROM t1` plans as
`UnorderedUnion(Scan(T1), Project([…], Scan(T1)))` (`plan_shape.golden`, `union_star.yaml#2`).
With upper-case DDL the fold is a no-op and both walkers agree. With quoted lower-case DDL the
scan leg reads as `[ID K]` and the projection leg as `[id k]`, so the rule decides they differ
and wraps whichever leg is *not first* in a rename `Map` targeting the first leg's spelling:

- scan leg first: leg 1 (the projection) is renamed to `ID`/`K`; its new type
  `RECORD<ID, K>` is not `Equals` to leg 0's `RECORD<id, k>`; the union constructor refuses it,
  the rule fails the call, and no combination yields — planning fails.
- projection leg first: leg 1 (the scan) is renamed to `id`/`k`; its new type *is* `Equals` to
  leg 0's; the constructor accepts, and the plan carries a `Map` whose rename value reads its
  input through `values.UniqueCorrelationIdentifier()` (`columnRenameValue`,
  `rule_implement_unordered_union.go:231`) — a correlation nothing at runtime declares. The
  executor reports `qov.binding … has no declared runtime binding` on the first row.

So on the only path where the rename is accepted, the rename cannot execute. The mechanism has
been dead on every committed shape (census below) and broken on the first shape that reached it.

The census also shows the executor's walker folds on exactly the same legs, so removing only
the rule's rename would let those queries plan and then hand the same disagreement to
`remapUnionColumnsByPosition`, which would re-type leg 1's rows to names leg 0 does not have.

RFC-237 §1: Java folds an identifier in one place, the parser, and "everything downstream compares
EXACTLY". These four `ToUpper` calls are a second fold, inside the planner and the executor, and
the fuzz found the one input where a fold-then-compare and an exact compare disagree.

### Census: neither Go-only mechanism fires on any committed shape

Instrumented at `36b97f1e9` (markers on the rule's rename, the executor's remap, the buffered
path, and both walkers' case folds) and run over every SQL corpus with `--nocache_test_results`:
`yamsql_test`, `golden_test`, `sqlpage_test`, `metamorphic_test`, `rowdiff_test`, `factory_test`,
`factorycorpus_test`, `factorycorpus/full:full_test`, `explaindiff_test`, and the `Union`-named
subset of `sqldriver_test`.

All eleven targets passed (`Executed 9 out of 9` and `Executed 1 out of 1`, no cached results),
so no marker that panics ever fired. Counts per corpus, from the test logs:

| corpus | rule reached its rename decision | rule inserted a rename `Map` | executor union entered | executor re-typed rows | buffered path | a fold changed a name | walker reached the folding tail |
|---|---|---|---|---|---|---|---|
| `explaindiff_test` | 360 | 0 | 0 (EXPLAIN only) | 0 | 0 | 0 | 0 |
| `rowdiff_test` | 1936 | 0 | 0 (plans only) | 0 | 0 | 0 | 0 |
| `yamsql_test` | 37 | 0 | 36 | 0 | 0 | 0 | 10 (8 `Scan`, 2 `CoveringIndex`) |
| `sqlpage_test` | 47 | 0 | 1049 | 0 | 0 | 0 | 0 |
| `sqldriver_test` (`--test.run=Union`: 42 test functions, 90 `=== RUN` lines with subtests) | 148 | 0 | 123 | 0 | 0 | 0 | 64 |
| `golden`, `metamorphic`, `factory`, `factorycorpus`, `factorycorpus/full` (8151 cases) | 0 | 0 | 0 | 0 | 0 | 0 | 0 — these corpora contain no `UNION` |
| **total** | **2528** | **0** | **1208** | **0** | **0** | **0** | **74** |

Two of those columns carry the finding. "A fold changed a name" is zero because every union
leg the suite has ever named is upper-case: the dimension that was never probed is a union leg
whose column names are not their own upper-case spelling. "Walker reached the folding tail" is
74 because bare-scan legs are common — the fold is *reached* constantly and has simply never had
anything to change.

Independently, the committed plan corpora at the merge-base `36b97f1e9` hold 36
`UnorderedUnion(` plan lines in `plan_shape.golden` and 4 across the yamsql and golden testdata
(the factory corpus has none); **0 of 40** carry a `Map(` leg. Measured as line counts:

```
git grep -c 'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance/explaindiff/testdata     # 36
git grep -c 'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance/yamsql/testdata \
    pkg/simfdb/hunt/golden/testdata pkg/relational/conformance/factorycorpus/testdata          # 4 (yamsql 3, golden 1)
git grep    'UnorderedUnion(' 36b97f1e9 -- pkg/relational/conformance pkg/simfdb | grep -c 'Map('   # 0
```

(r1 quoted 17 for the second figure; that count matched `Union(` and so included `InUnion(`.)
The rename `Map` that RFC-183 §14 counted ten unreachable edges for has not been produced by any
committed query since RFC-226.

The mechanisms are dead on every shape the suite knows and wrong on the first shape it did not.

## Java

`ImplementUnorderedUnionRule.onMatch` (4.12.11.0, `rules/ImplementUnorderedUnionRule.java:57-71`):

```java
final ImmutableList<Quantifier.Physical> quantifiers =
        Streams.zip(planPartitions.stream(), allQuantifiers.stream(),
                (planPartition, quantifier) -> call.memoizeMemberPlansFromOther(
                        quantifier.getRangesOver(), planPartition.getPlans()))
                .map(Quantifier::physical)
                .collect(ImmutableList.toImmutableList());
call.yieldPlan(RecordQueryUnorderedUnionPlan.fromQuantifiers(quantifiers));
```

`RecordQueryUnorderedUnionPlan.fromQuantifiers` → `RecordQueryUnionPlanBase(quantifiers,
reverse)` → `RecordQuerySetPlan.mergeValues(quantifiers)`, which types the result as the first
non-existential leg's flowed type and never compares legs. `UnorderedUnionCursor` concatenates the
child cursors' results unchanged.

Java can afford that because `LogicalOperator.generateUnionAll` has already promoted every leg
onto `SemanticAnalyzer.validateUnionTypes`'s common type before the logical union exists. Go's
translator does the same job in `exactUnionResultRow` + `normalizeUnionLeg`, and Go's physical
constructor additionally *asserts* the alignment that Java assumes. That assertion is the reason
the two Go-only mechanisms can be deleted rather than repaired: an unaligned union is loud at
construction, so nothing downstream needs to guess.

One divergence from `mergeValues` is pre-existing and untouched here: Java wraps every leg's
flowed value in a `DerivedValue`, so the union's result carries every leg's correlations; Go's
result is leg 0's QOV alone (`logical_union.go:54-58`). That is a difference in what the value
*refers to*, not in the row it states, and this RFC changes neither.

## Fix

One authority per RFC-237 and per Java: the translator aligns; the physical constructor asserts;
nothing below re-derives names.

**A. `ImplementUnorderedUnionRule` becomes Java's rule.** Delete the rename block (lines 92-146),
`physicalPlanColumnNames`, `colNamesEqual`, `columnRenameValue`, `recordTypeFieldCount` and the
`childPlans` slice that existed only to feed them. What remains is: roll up each leg's plan
partitions, decline a combination with an empty leg (the Go partition representation can produce
one, Java's matcher cannot), memoize each leg's plans into a physical quantifier, and yield
`NewRecordQueryUnorderedUnionPlanFromQuantifiers`. The constructor's exact-type check is the
asserted bridge; a leg that disagrees fails the call loudly, as today.

**B. The union executors stop re-typing rows.** Delete `remapUnionColumnsByPosition`,
`planColumnNames`, `planColumnNamesWithMD`, `streamingAggOutputNames` (its only caller) and the
`innerPlanAccessor` interface (its only user). `executeUnorderedUnion` executes each leg and
concatenates. `executeUnion` always takes `executeUnionStreaming`, whose `md`/`targetKeys`
parameters go; `executeUnionBuffered` — the path that existed only to peek rows for column names,
and that refuses every continuation — is deleted with them. Comments that describe the buffered
fallback (`executor.go:83`, `memory_budget.go:249`,
`continuable_without_duplicates_property.go:76`) are corrected.

A leg's rows flow under the names its own plan states. Legs state the same names by construction
(translator) and by assertion (constructor), so a downstream by-name read resolves identically on
every leg. Mixed row kinds — a bare scan leg emitting record rows beside a projection leg emitting
positional rows — are already the shape of `union_star.yaml#1` and `#2` and pass today with the
remap a no-op.

**C. The logical constructor asserts too.** `requireSetOperationResult` took leg 0's flowed
value unchecked, so a union built with unaligned legs (a planner-API producer has no translator
in front of it) entered the memo and died later, from a rule body, as `XX000 unclassified
planner failure` — a group with no realizable implementation. It now requires every flowing leg
to state a row `Equals` to leg 0's. The recursive union had refused disagreeing states through
its own `FlowedTypesEqual` check — the same contract in a second implementation — and now
asserts through the same helper (`requireSetOperationResults`), which is literally Java's
`mergeValues(ImmutableList.of(initial, recursive))`. The physical constructor's check stays as
the re-check at the plan boundary. Also here: the
result-value comment at `logical_union.go:60` cited `TestSetOperationResultValueStatesChildZerosRow`,
which does not exist in the tree; the pin is `TestSetOperationsStoreFirstNonExistentialExactQOV`.

**D. The translator's join-leg gate goes with the remap it was built on.** `unionOutputColumns`
anchors a UNION used as a join leg or derived table to its first branch's columns, and it
declined — returning nil, which surfaces as an unsupported-shape error — whenever the branches'
names differed and any branch failed `unionBranchNormalizable`, a classification of "whether the
executor's union position-remap can remap this branch" (`cascades_translator.go:1051`). The
remap is gone; `normalizeUnionLeg` re-emits every differing branch onto the union's row by
ordinal, whatever the branch's shape. The gate, `unionBranchNormalizable`,
`aggregateNamesStableForUnion` and their unit test are deleted; the anchoring keeps only the
width check. Probed before deciding: the three operand forms the gate's aggregate arm refused
(qualified `SUM(ga.v)`, constant `COUNT(1)`, group-only) return identical, correct rows at the
merge-base and after the deletion — every SQL-built branch is a `LogicalProject`, so the
aggregate arm was reachable only from directly constructed trees, and deleting it changes no
SQL-visible behaviour. That is the finding: a live gate, eight unit tests, and a paragraph of
justification, all defending a premise no SQL query could reach.

**E. Stale references.** Every comment that described the rename `Map`, the position-remap, the
buffered fallback or the walkers as live — `default_rules.go`, `streaming_cursors.go` (three
sites), `executor_new_plans.go`, `aggregate_index.go`, the RFC-078/080/081 e2e test headers, two
ordering-contract test headers, `DIVERGENCES.md`, and two error-translation fixtures that used
`"buffered union"` as their context string — is restated in terms of the mechanism that remains.
The sweep is the residue of a scoped grep over `pkg`, `TODO.md`, `DIVERGENCES.md` and
`.claude/skills` for the deleted symbols and phrases; what remains names them only as deleted.
Two census gates moved with the deletion and are updated with it: RFC-213's result-type
consumer census loses the three walker reads (GUARDED 12 → 10, PROPAGATED 28 → 27, each site
named in the pinned test's comment and in RFC-213 §3), and RFC-238's line cites into the
edited files are re-pointed at the lines their cited text now occupies. `newChargeReleasingCursor`
lost its only caller with the buffered path and is deleted too, as are the two
`OutputColumnNames` accessors (aggregate index, streaming aggregation) whose only production
caller was the executor walker; the tests that read them read the plan's stated result row
instead, which is what production derives the ordering domain from.

### Rejected

- **Remove the four `ToUpper` calls and keep both mechanisms.** Fixes the fuzz input and the four
  queries, and leaves two dead re-derivations of a name whose only remaining effect is to
  disagree with the translator the next time a walker arm and a projection arm answer
  differently. RFC-183 already paid once for the rename `Map` living outside the memo; RFC-078's
  three walker arms exist because each new physical plan type needed one. "Long-term correct" is
  the criterion, and that means one mechanism.
- **Compare with `EqualFold` instead.** Makes the rule's decision case-insensitive while the
  constructor's stays exact, so the rule would decline to rename `id`/`ID` legs that the constructor
  then rejects. It moves the disagreement, and it adds a case-insensitive comparison at a site
  RFC-237 says compares exactly.
- **Relax the constructor to Java's "pick the first type".** Would make Go accept a union Java
  accepts, but Go's executor reads columns by name where Java's reads a `Message` by descriptor;
  the exact check is what lets the executor drop its defensive remap. Keep the stronger assertion.

## Duplicate mechanisms — what this collapses

Before: three places compute "the union's column names" (translator, rule walker, executor
walker) and two of them fold case. After: one place states them, one place asserts them. The
`values.OutputColumnName` authority RFC-229 established for projections is untouched; the deleted
walkers were readers of it, not authorities.

Not collapsed, and not touched: `planColumnNamesWithMD`'s sibling in the aggregate-index and
streaming-aggregate cursors (`CanonicalAggColumnName`), which name a cursor's *own* output row
rather than compare two legs'.

## Performance

None expected. The deleted code ran once per union implementation (a walk over each leg's plan
chain) and once per union execution (a walk plus, when names disagreed, a per-row `MapCursor`).
The census shows the per-row path never engaged on a committed shape. No cost-model input
changes; no plan choice changes — the goldens are expected to move by zero lines, and the
`just golden` gate is the measurement.

## Test plan

Every proof is committed; each names the dimension that was unprobed.

1. **Fuzz corpus.** `testdata/fuzz/FuzzPlanner_WithBatchA_NoPanic/4a35dedaab03e663` (input
   `[]byte{35, 4, 4, 33}` = `Union(TypeFilter(TypeFilter(Scan)), TypeFilter(Projection(Scan)))`
   over a row type with lower-case fields), plus the same bytes as an `f.Add` seed with the
   regression note, so the shape replays under `go test` and under Bazel (the target globs
   `testdata/**`).
2. **Planner unit pin, on both exits.** The fuzz shape driven through `Plan` deterministically in
   both leg orders (`union_leg_names_not_refolded_test.go`). Scan leg first asserts the legs'
   flowed types are `Equals`; projection leg first asserts additionally that **no leg is a
   `RecordQueryMapPlan`**, because on that exit the rename's type matched and only the `Map`'s
   presence betrays it — a type-only assertion there passes with the defect present (verified
   against the merge-base by review). The upper-case row is the control.
3. **Both asserted bridges have tests.** `NewRecordQueryUnorderedUnionPlanFromQuantifiers` over
   two legs whose types differ only in field-name case returns the `disagrees` error
   (`unordered_union_leg_types_test.go`) — the constructor check the fix relies on had no test
   driving it. The logical constructor added in § Fix C is pinned the same way for union and
   intersection, beside a pin that an existential leg is still exempt and a positive control
   (`set_operation_leg_types_test.go`).
4. **SQL e2e, real FDB.** New yamsql scenario `union_quoted_identifiers.yaml`: the quoted
   lower-case table, the four previously failing shapes with their rows and `plan_contains`
   pinning a bare-scan leg beside a projection leg with no `Map` between them, the
   scan-first/projection-first pair in both orders (the two exits above), `columns:` pinning
   the quoted spelling of the labels, and the upper-case control.
5. **SQL e2e, SimFDB.** The same shapes as a `golden` corpus scenario so `just test` runs them
   without Docker and the baseline captures both plan and rows.
6. **Executor.** The RFC-078/080/081 e2e tests (`union_aggregate_remap_test.go`,
   `union_scalar_aggregate_alias_test.go`) stay as they are; they now prove the translator alone
   handles mismatched aggregate aliases. `plan_column_names_test.go` and the two
   `TestPhysicalPlanColumnNames_*` tests are deleted with the walkers they test.
7. **Negative result pinned.** A union whose legs' names differ at the *logical* boundary is not
   constructible from SQL (the translator normalizes) — pinned by the yamsql scenario's
   `SELECT "id" AS w, "k" AS x … UNION ALL SELECT *` case, whose plan shows both legs projected.
8. **The gate's shapes.** `union_join_leg_aggregate_forms.yaml` (real FDB) and the golden
   `unionjoinleg` scenario (SimFDB) run the three operand forms the deleted gate refused —
   qualified, constant, group-only — as union join legs, plus the bare-column control, and
   assert rows. They pass at the merge-base and after the deletion, which is the measurement
   § Fix D rests on: the gate's aggregate arm was never reached from SQL.

## Adjacent finding, surfaced by the § Fix D probe — not this RFC's mechanism

Probing the gate's operand forms turned up a shape that fails identically at the merge-base and
after this change, with no union in it:

```sql
WITH u AS (SELECT g, SUM(v * 2) AS s FROM ga GROUP BY g)
SELECT c.w, u.s FROM u, c WHERE u.g = c.id
-- column "S" resolves against source "U", which declares no column order to bind a plan-time ordinal
```

The same body as a derived table (`FROM (SELECT g, SUM(v * 2) AS s …) u, c`) returns the correct
rows, and the same CTE with a bare-column operand (`SUM(v) AS s`) works. So a non-recursive
CTE whose body carries an **expression** aggregate cannot be a join leg: the CTE-scan arm of
the leg-column derivation loses the aliased column where the derived-table arm keeps it. It is a
translator column-derivation defect independent of union alignment, and it is chased next as
its own change rather than folded into this one, so that this RFC's review covers one
mechanism.

## Rides alongside, not part of this RFC

The engine fuzz nightly was red for a second, unrelated reason: `FuzzRebaseValue_NoPanic` built
its alias map from two fuzzed strings and `t.Fatal`ed when `NewAliasMap` refused an empty name
as a zero correlation — reporting a guard as a crash. That fixture fix and its corpus entry
`d442d027a0e3b992` land in the same pull request as their own commit; nothing in this RFC's
mechanism depends on them.

## What this does not close

- `planColumnNamesWithMD`'s descriptor-name fold and the aggregate-index / streaming-aggregate
  output-name arms are deleted here only because their sole caller is. Whether the aggregate
  cursors' `CanonicalAggColumnName` is itself an RFC-237 second fold is a separate question with
  its own census; it does not compare legs and is out of scope.
- The two `EqualFold` lookups in `rule_implement_in_union.go:130` and `physical_key_types.go:295`
  are identifier LOOKUPS against physical field names, the class RFC-237 §Scope permits, not
  presentations. Not touched.
- Java's rule matches `all(forEachQuantifierOverRef(…))` (`ImplementUnorderedUnionRule.java:64-66`);
  Go's rule takes the union's quantifiers whatever their kind. A logical union with a
  non-for-each leg is not constructible from SQL, so the residual is unreachable today; it is a
  matcher-fidelity item, not an alignment one, and is left as found.
