# RFC-242 — A union's legs are aligned once, by the translator

**Status:** r3. r1: Graefe ACK with one non-blocking condition (assert leg alignment at the
logical constructor too — folded as § Fix C); Torvalds NAK with five findings, all folded: a pin
that passed with the defect present now asserts no leg is a `Map` (test plan 2); the translator's
join-leg gate built on the deleted remap premise is deleted too (§ Fix D); two census figures
were overstated and are restated with the commands that produce them; the stale references are
swept (§ Fix E); the scratch probes are gone from the tree. r2 (implementation lap, head
`835d5a462`): Graefe ACK; Torvalds ACK after one residue; @claude review pass with two notes
(folded: `FlowedTypesEqual` in the shared helper, a `TODO-production.md` reference); codex NAK on
three points, all folded in r3 — the rule now declines non-for-each legs (§ Fix A), the adjacent
CTE defect is fixed here (§ Fix F), and the repository-editable nightly cause is fixed with
the host cause escalated. r3 delta: Graefe ACK conditional on the CTE arm's duplicate-name
handling; Torvalds NAK — the coverage-timeout claim refuted by measurement (reverted), the
dispatch step's missing `actions: write`, four stale comments, and the same duplicate-name
hole, resolved on his measurement (the reader's `42702`) rather than by a decline. Awaiting r4
delta confirmation.
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
asserted bridge; a leg that disagrees fails the call loudly, as today. The rule also declines a
union with a leg that is not a for-each quantifier: Java's matcher is
`all(forEachQuantifierOverRef(…))` (`ImplementUnorderedUnionRule.java:63-64`), and a
concatenating union over an existential leg would emit that leg's rows. The r2 text listed this
as unreachable and left as found; codex's review held that "does no more than Java" has to be
true of the rule itself, not only of what SQL reaches, and the guard is one line.

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
asserts through the same helper (`requireSetOperationResults`) over the same leg list as
Java's `mergeValues(ImmutableList.of(initial, recursive))` — with the equality assertion Go
keeps and Java's `mergeValues` does not make (Rejected, third bullet). The physical constructor's check stays as
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

**F. A CTE body publishes its exact row under its SQL names, repeated names included.**
`buildCTEColumnSource` has two arms that build the body and publish its exact row — the
join/derived-bodied arm, and (since this RFC) the single-table aggregate arm, which calls
`buildExactScopeSourceOrBodyError` before `buildDerivedTableSourceFromAgg`, the order the
derived-table path already had (see the first adjacent-finding section for the mechanism). The
parse-tree derivation stays as the aggregate arm's fallback for a row `semantic.Column` cannot
carry — a catalog nested record the exact derivation refuses and the parse-tree one carries
verbatim — never for a row the exact derivation published. Three things about those arms were
wrong, each measured at the merge-base and each in the CTE spelling only, where the
derived-table spelling of the same body already answered as Java does:

- *The join-bodied arm declined a repeated output name.* `scopeSourceNamesUnique` withheld the
  whole source when the row named a column twice, on the theory that a published duplicate
  would bind silently. The decline was the silent bind: the CTE fell to the ON-only class, and
  `u.g` over `WITH u AS (SELECT ga.g, c.id AS g FROM ga, c)` then bound one duplicate in SELECT
  and the other in ORDER BY, in seven read paths, while the derived-table spelling reported
  `42702`. The gate is deleted. A repeated name is published as stated, and every read of it —
  bare or qualified, in SELECT, WHERE, ON, ORDER BY, GROUP BY, HAVING, EXISTS and a scalar
  subquery, Graefe's r4 delta measured all of them — meets the semantic scope's own
  per-source ambiguity check and reports `42702`, Java's `AMBIGUOUS_COLUMN`, byte-identical to
  the derived-table spelling. The first cut of the aggregate arm had the same gate and fell to
  the parse-tree derivation on a repeat; Torvalds's r3 delta measured the reader loud without
  it, and Torvalds's r4 delta measured the join arm silent with it.
- *The join-bodied arm took its names from the exact derivation.* The derivation labels a
  qualified reference by its datum key — `ga.g` is `GA.G` there — so, with the gate gone, the
  row `SELECT ga.g, c.id AS g` carried `GA.G` and `G`: two distinct names for what SQL calls `G`
  twice, and `u.g` bound the second without ever meeting the ambiguity check. Both arms now
  pass the SQL output-name authority the derived-table path passes — `projectionOutputNames`
  for a spelled projection, `aggOutputCols` for an aggregate body — so the two spellings of one
  body publish one row by construction. A star body spells no projection and keeps the
  derivation's labels, as before.
- *`aggOutputCols` named an unaliased grouping key by its qualified spelling.* The parser
  mints a grouping item's `outName` from the reference's display text, so `GROUP BY ga.g`
  published `GA.G` — a name no reader can write — and `u.g` over
  `(SELECT ga.g, SUM(v) AS s FROM ga GROUP BY ga.g) u` was `42703` in both spellings. The
  body's own projection labels that slot `G` (`aggregateProjectionItem` treats an `outName`
  equal to the reference as no alias and labels the stripped reference); `aggOutputCols` now
  applies the same rule and publishes the bare name.

One shape stays out of the global scope on purpose, keyed on its SHAPE and never on its
names: a `SELECT *` body over a lateral unnest that the narrow single-source admission does
not take — a second base table beside the unnest, or an EXISTS in its WHERE — is the gathered
multi-source unnest cluster. The translator flows that cluster as its raw per-leg positional
seed and binds an aggregate's keys and operands over the CTE to the seed by ordinal
(`exactGatheredCTEGroupKeyValue`, which admits only a CTE absent from the scope); a
published exact row minted a read over the CTE's own quantified object instead, which nothing
declares at execution. At the merge-base the uniqueness gate happened to withhold the
repeated-name body of that shape (`A.K` beside `B.K`) and the unique-name body was published
and failed as an undeclared binding; the decline now covers both, and both answer.

Pinned in both spellings: the repeated name through every read path that bound silently
(`cte_published_row_names.yaml`), the qualified grouping key over a single-table and a joined
body, and the unique-name control beside each. The gathered-unnest CTE aggregates are the
sqldriver `TestFDB_UnnestExistsGather` pins (`agg_cte_*`), which the first cut of this round
reddened.

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
9. **The rule's matcher.** `TestImplementUnorderedUnionRule_DeclinesANonForEachLeg` fires the
   rule over a union with an existential leg and asserts it yields no union plan, with the same
   two references as for-each legs as the control that does implement.
10. **The CTE aggregate body.** `cte_expression_aggregate_join_leg.yaml` (real FDB) and the
    golden `cteagg` scenario (SimFDB): the failing shape, the same body as a derived table, the
    bare-column control, and the aggregate read through the CTE without a join. The first
    fails at the merge-base with the column-order error and returns the rows after § Fix F.
    The same scenario pins the duplicate-output-name body in both forms as the reader's
    `42702`, so the CTE and derived-table spellings of one body cannot drift apart and the
    parse-tree fallback is never what answers a published row.
11. **The published CTE row.** `cte_published_row_names.yaml` (real FDB): the join-bodied
    repeated name through the ten read paths that bound silently at the merge-base, in the CTE
    and the derived-table spelling, each `42702`; the qualified grouping key over a single-table
    and a joined body, both spellings, rows and labels; the unique-name join-bodied control. The
    ORDER BY metadata pins move with the rule they pin: the repeated-name body that was the
    "underivable" specimen now resolves a computed key and a computed projection
    (`TestOrderByExactMetadata_Computed*OverRepeatedNameCTEResolves`), and the stays-loud pair
    keeps a specimen that is genuinely underivable — a row with a catalog struct column the
    semantic column model cannot state. The SimFDB golden `ctenames` pins the plans and rows
    of the planning shapes. The sqldriver probe suite's Q53–Q56, which pinned
    complete-schema-or-decline (`0AF00` for any repeated name), are re-pinned to Java's
    answers — `42702` for a read that spells the repeated name, rows for a read of a unique
    column beside it — and Q57 pins the loud floor of § What this does not close beside the
    aliased control that answers; `TestFDB_UnnestExistsGather`'s `agg_cte_*` pins hold the
    gathered-unnest decline.
12. **Repeated output names.** `repeated_output_names.yaml` (real FDB) and the SimFDB golden
    `repnames`: the labels of
    `SELECT g AS a, g AS a`, `SELECT id, g AS id` and a star over a body that repeats a name,
    and the values of `SELECT *` over such a body beside another table — the repeated-name leg
    first and second, the CTE and the derived-table spelling, an aggregate body and a plain one,
    the unique-name control beside each. Unit pins: `frozenSchemaRenamesSlot` on the six
    rename-versus-dedup shapes; `mergedRVSequenceDiverges` tolerating a repeated display name
    and still rejecting a different name and a reordering; `derivedOutputColumns` naming a
    repeated output exactly as `values.DedupFieldNames` does, at one, two and three
    repetitions.

## Adjacent finding, surfaced by the § Fix D probe — fixed here (§ Fix F)

Probing the gate's operand forms turned up a shape that failed identically at the merge-base,
with no union in it:

```sql
WITH u AS (SELECT g, SUM(v * 2) AS s FROM ga GROUP BY g)
SELECT c.w, u.s FROM u, c WHERE u.g = c.id
-- column "S" resolves against source "U", which declares no column order to bind a plan-time ordinal
```

The same body as a derived table (`FROM (SELECT g, SUM(v * 2) AS s …) u, c`) returned the
correct rows, and the same CTE with a bare-column operand (`SUM(v) AS s`) worked. The r2 text
named this as a separate change; codex's review held it to the DFS rule — a defect surfaced by
a fix is fixed in the same change — and that is right, so it is § Fix F below.

**Mechanism.** A CTE's scope source is built by `buildCTEColumnSource`
(`embedded/logical_predicate.go`). For a single-table body with aggregates it went straight to
the parse-tree derivation `buildDerivedTableSourceFromAgg`, which types each aggregate from its
argument's *catalog* column; an expression argument has no catalog column, so `S` was published
as `UNKNOWN`, and the enclosing query's ordinal bind — which refuses a row it cannot type —
declined the source. The derived-table path (`buildDerivedTableSourceWithCTEs`) had the right
order all along: build the body and publish its **exact** row first
(`buildExactScopeSourceOrBodyError`, the same call the join-bodied CTE arm makes), and fall back
to the parse-tree derivation only when the exact one has nothing to publish. The CTE aggregate
arm now takes that order. A body that does not build raises its own error, as the join arm's
does.

## Second adjacent finding, surfaced by Graefe's r4 delta — fixed here

Measuring every read path of a published repeated name, Graefe found the one that was loud and
wrong: `SELECT * FROM (SELECT g, SUM(v) AS g FROM ga GROUP BY g) u, c` died `XX000` at the
result set's alignment guard, in both spellings, while the unique-name control answered four
columns. Pre-existing at the merge-base; Java answers `[G G ID W]`.

**Mechanism, in two layers.** Go keeps a projection's record type name-addressable by suffixing
a later occurrence of a repeated output name (`values.NewRecordConstructorValue`: `G`, `G_2`).
Java has no such suffix — `Type.Record` keeps repeated field names and binds every read by
ordinal (`Expressions.java:91`, `LogicalOperator.java:367`) — so the suffix is a property of
the Go type and nothing user-visible may carry it, which the result set's alignment guard
already assumed ("the user-visible labels stay `[X X]`"). Two consumers did not hold to that:

1. *The label followed the frozen output schema for an aliased item.*
   `deriveColumnsFromProjection` took the frozen name unconditionally when the item carried a
   user alias, so `SELECT g AS a, g AS a` reported `[A A_2]` and `SELECT id, g AS id` reported
   `[ID ID_2]`; for an unaliased reference it took the frozen name unless the same label had
   appeared at an earlier slot — a heuristic that reads a column-list rename of a repeated
   alias as a dedup. Both arms now ask the structural question: the NATURAL schema (the same
   Value program and aliases, deduplicated by the same rule) names the slot, and only a frozen
   name the natural schema does not produce is a rename (`frozenSchemaRenamesSlot`). The
   heuristic is deleted with its test. Through a join, the leg's second `G` then carries label
   `G` over datum key `U.G_2`, and the alignment guard's repeated-display rule aligns it by
   ordinal — which is where the second layer showed.
2. *The derived leg's ordinal layout stated the names verbatim.* With the label fixed, the same
   query answered `[100 100 100 1]`: the grouping key in both `G` columns, silently. The join
   seed builds each leg's baked positional reads over the leg type `derivedOutputColumns`
   states — `[G G]`, the projection's names verbatim — while the row the projection emits is
   typed by its record constructor, `[G G_2]`. `OrdinalIn` requires the read's domain to equal
   the row's, so the baked read of slot 1 declined its ordinal and fell back to a by-name read,
   which answered the first `G`. `derivedOutputColumns` now applies `values.DedupFieldNames`,
   the rule extracted from the constructor so both sites state one layout, and
   `mergedRVSequenceDiverges` — the metadata twin of the alignment guard — tolerates a repeated
   display name the way the guard does instead of routing every duplicate-name leg through the
   fallback that publishes the suffix.

Both layers are pinned in `repeated_output_names.yaml` (§ Test plan 12). The suffix itself
stays: it is what keeps Go's record types name-addressable, it is now purely internal, and the
two consumers that must agree with it are pinned against the rule rather than against each
other.

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
- An aggregate or a sort over a UNIQUE column of a join body that repeats a bare leaf
  (`WITH u AS (SELECT ga.g, c.id AS g, c.w FROM ga, c) SELECT u.w, COUNT(*) FROM u GROUP BY
  u.w`) is refused at execution — an undeclared binding, or `edge lookup … read as
  RECORD(G,G,W), declared RECORD(GA.G,G,W)` — in the CTE and the derived-table spelling. The
  derived spelling has failed so since before this RFC; the CTE spelling was served by the
  name model while its body was declined and fails loudly now that the body is published. The
  cause is not this RFC's and is wider than it: the engine names runtime slots by three rules
  (the constructor's suffix, the qualified datum key a join projection mints for a repeated
  bare leaf, the verbatim names of a raw positional merge) while the declared type and the
  scope carry one, so a read bound to the CTE's quantified object finds a row of a different
  name. Java binds every such read by ordinal. It is pinned as a loud floor in
  `cte_published_row_names.yaml` beside the aliased control that answers, and booked in
  `TODO.md` ("Exact quantifier binding over a CTE or derived body") with the measurements and
  the fix it needs; a first cut of this round tried a projection boundary and a deduplicated
  flowed layout for it and reverted both — they moved the mismatch between the three rules
  rather than removing it.
- The nightlies that are red for a runner-host reason (the FDB container disappearing about
  thirty minutes into every Docker-backed job, the factory batch SIGKILLed, the coverage job
  cancelled from outside after 3–67 minutes with no timeout annotation) need host access; the
  one repository-editable cause — the bot pin-bump PR carrying no checks — is fixed in the same
  pull request as a workflow edit, and the host cause is escalated to the owner as a STOP, not
  filed. A draft of this RFC also blamed the coverage lane's job timeout; Torvalds's delta lap
  measured the run durations and refuted it, and the cap is back at its value with the
  measurement in its comment. The `TODO.md` entry records which is which.
