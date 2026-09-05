# RFC-242 — A union's legs are aligned once, by the translator

**Status:** r28. r27 (head `25be07e4b`): Graefe ACK and Torvalds ACK, each with the same required fold, and a codex P2 saying it too — the control also replaces the right leg with a derived-table projection, which is descriptor-relevant, so the pair had two variables and only prose excluded the second. r28 commits the arm all three measured: the wrapper kept, the repeat restored, still a raw map. r27. r26 (head `73c0f8e62`): Graefe NAK, Torvalds NAK and a codex P2, all three measuring the same thing — the one-statement witness repeated `R` as well as `ID`, so the control removed only one of two repeats and could attribute nothing; the retired query was left declared but unrun above the comment claiming it was the control; and the arbitrary-row read had moved to the control rather than being removed. r27 names the CTE's struct apart so exactly one name repeats, reads both texts through one helper that demands a single qualifying row, and deletes the dead query. r26. r25 (head `cbbbc5ed1`): Graefe NAK and Torvalds NAK on the same two — the live booking still described the blunt control r25's own fold had just declared insufficient, and the stored-struct assertion had no witness that its query was poisoned; Torvalds also found "blast radius" doing double duty for two different axes, and the first-non-NULL read pinning an arbitrary row of an unordered result. r26 states the sharp control in the booking, reads the computed and stored structs out of ONE poisoned row, and names the axes apart. r25. r24 (head `04ac0f531`): Graefe ACK with two folds (the planner pin claimed a shared query text covering only the ID half; the booking's reproducer snippet was stale); Torvalds NAK — the r23 fold still carried the stale `(1, 1, true)` measurement and the refuted LATENT classification, in the very bullet r24 edited, and the control removed the join and the duplicate name together; codex three P2 — the pin accepted any non-empty result and any map, and the stored-STRUCT measurement had no committed witness. r25 retracts both in place, keeps the join in the control, and asserts exact rows, values in both representations, and the stored-struct bound. r24. r23 (head `dd64dbe4e`): Graefe NAK and Torvalds NAK — the retracted map framing survived in five more sites and r23 wrongly claimed it had grepped for them, and the FDB negative could go vacuous against a different query; codex P2 went further and refuted the LATENT classification — a computed STRUCT through the poisoned plan comes back a raw map where the same CTE without the join returns an api.Struct, and the no-loss pin could not discriminate with both ID slots equal. r24 sweeps every site, joins the two pins on one query with distinct slot values, and reclassifies the booking as user-visible. r23. r22 (head `8ef36a765`): Graefe NAK, Torvalds NAK and @claude NAK, all three on the same inverted absolute (r22 replaced "every constructor is stamped" with "every constructor is a map"; the measured census is three of four with one survivor) and all three confirming the mechanism sound; codex P2 went further — the data loss those homes claimed does not exist, because the rows flow positionally. r23 states the population in every home, restores the dropped qualifier, and pins the no-data-loss negative. r22. r21 (head `b96603bbf`): Graefe NAK and Torvalds NAK and a codex P2, all three on the same vacuous pin — it planned through a route that never bakes, and its surviving assertion was a tautology; Torvalds also measured the finding understated, the failure being per-repository rather than per-row. r22 bakes the pin, asserts the collateral and the survivor, and states the population in every home. r21. r20 (head `e02ad1451`): Graefe NAK and Torvalds NAK on the same paragraph — the withdrawal of r19's booking rested on a false negative and the both-orders witness was vacuous; codex two P2 — the same two. @claude NAK on the same vacuous witness Graefe found, answered after r21 was cut and folded by it. r21 restores the booking with the reproducer committed as its pin, fixes the witness, and enumerates the swallow arm. r20. r19 (head `c5d07a707`): Graefe ACK (non-blocking: the synthetic counter can collide with an escaped declared name); Torvalds NAK on that collision, measured in both orders; codex P2 — the same collision, with the array witness; @claude ACK. r20 puts synthetic names in a namespace no identifier can reach and moves the guard to Java's own site. r19. r18 (head `eff9e2e4c`): Graefe ACK (a booking: one declared name over two shapes comes back as raw maps, pre-existing; pin the array-of-named-record spelling); Torvalds ACK (the same finding, with Java loud where Go is silent — fold it); codex P2 — the same finding, propagate the conflict as Java does; @claude ACK with the same array-pin ask. r19 makes that failure loud, pins the array spelling, and records the r17 lap below. r18. r17 (head `589a305e5`): Graefe ACK (one booking: the retag guard refuses a named source Java accepts); Torvalds NAK on that same shape, measured — `VALUES (STRUCT RECORD (…)) AS A(W(X, Y))` accepted at the merge-base and refused at r17; codex P2 — the same named-source rejection, with Java's line; @claude ACK (its lap was queued behind the busy runner and answered after r18 was cut). r18 ports Java's rule: the definition renames the fields and keeps the name. r17. r16 (head `46c47a0b8`): Graefe NAK and Torvalds NAK on one measured finding — the inline-values retag minted every VALUES row as a record named RECORD and the bridge laundered it back, so two VALUES rows of different shapes still collided into raw maps (codex's P2 through a second door); codex P2 — the same special case dropped a struct literal DECLARED with the name RECORD to a synthetic name after the bridge; @claude ACK. r17 mints the VALUES row anonymous, drops both name special cases, and pins the VALUES spellings. r16. r15 (head `00a5851d9`): Graefe ACK with one prose fold (the decline set is wider than the bridge's default arm and a NULL literal beside the path reaches it, non-discriminating; book the enum-as-STRING typing); Torvalds ACK with the same scope sentence, the status line's lost @claude mentions, and the same booking; codex P2 — the widened bridge admits an unnamed record and the bridge back named it RECORD, so two anonymous shapes in one derived row claimed one descriptor and their array elements came back as raw maps; @claude ACK. r16 folds the prose, restores the mentions, keeps an anonymous record anonymous through the bridge, and books the enum typing and a fieldless-message table found on the way. r15. r14 (head `ac5cf7ab3`): Torvalds ACK (one nit: a shape-true decline rebuilt the identical body through the net); Graefe NAK — the shape rule's decline must be final, a fallthrough to the leaf lookup re-legitimises the homonym mistyping; codex failed on model capacity (no verdict); @claude lap in flight. r15 returns the exact derivation's answer unconditionally in all three arms and pins, as a negative result, that no shape reaches that decline today. r14. r13 (head `5c80ef758`): Graefe ACK; @claude ACK; Torvalds NAK — the shape rule alone regressed the alias that names a struct column (`st2 AS p`, `p.co`), the post-lookup net r13 deleted must stand beside it in all three arms; codex two P2 — an alias match is not proof of qualification (the same shape), and a declared-STRUCT nested field must survive the exact route. r14 restores the net in all three arms and publishes nominal records through the exact derivation (a pre-existing 0AF00/42703 under codex's finding), pinned in twelve spellings. r13. r12 (head `3315a141d`): Graefe ACK; @claude ACK; Torvalds NAK — the nested-path decision ran after a lookup by the leaf name, so a leaf with a top-level homonym was typed as that column; codex P2 — the newly admitted quoted-dot nested member labels as RFC-238's residual does. r13 decides the nested path by its shape in all three arms, pins the homonym in four spellings, and pins the label residual on the admitted shape. r12. r11 (head `27635cda7`): Graefe NAK and Torvalds NAK on one measurement — the "second gate" r11 read into the climb pin was a fixture artifact (a seed without the MaxMatchMap), and the pin as seeded could never go red; @claude ACK with a bookkeeping item (two fold bullets described a state the diff did not show); codex found no actionable regression. r12 re-seeds the pin as the planner does and shows it red under the mutation, restates the single gate, fixes the nested-field projection of a derived body Torvalds found pre-existing, and restates the two bullets. r11. r10 (head `bd8ec0c66`): Torvalds ACK with five non-blocking folds; Graefe NAK — the group-by rule's PRESERVE branch still stated its keys over its own inner quantifier, and the XX000 yaml pin r10 claimed to remove was still there (a codex run had reverted the working tree under the r10 edits; the removal was redone); @claude NAK on the same yaml fact; codex two P2 (the same preserve branch; a real column named `__ROW_VERSION` classified as the pseudo-column by its name alone). r11 folds all of those, fixes two pre-existing bugs Graefe's probes surfaced (a nested derived table's unaliased qualified output published under its display name; the real `__ROW_VERSION` column), and books the IN-over-aggregate-subquery gap. r10. r9 (head `e92bd661d`): Graefe ACK with one required fold to the BOOKING — the receiving side of the ordering-through-a-projection remainder is not missing ordering parts but a match that never climbs (`correlatedToEquals`), restated in all three homes and pinned; Torvalds NAK with two folds — the swap pin's data was monotone (moved to a fixture whose data discriminates) and an XX000 yaml pin was being credited as a correct rejection (pinned in Go instead) — plus non-blocking folds, all taken; the group-by push rule's synthesized ordering is rebased into its child's current-row space, found when the projection push rule went loud. r8 (head `56a3df6ed`): Graefe ACK with one required fold (the swapped-name body whose sort must stay — a wrong answer at the merge-base); Torvalds NAK on that shape plus six folds; @claude ACK with one residual (the row-versioned remainders unpinned as negatives). r7 (head `cd7bdc5ed`): Graefe ACK (one non-blocking booking: a redundant sort over a renamed grouping key, which r8 fixes as the third adjacent finding); Torvalds ACK with four folds; @claude NAK on coverage (the union-bodied derived table's fix had no regression pin, the added sort on golden #25 was unexplained, a fixture comment cited the wrong RFC-238 section); codex two runs, four P1 findings (a silent wrong answer for quoted case-distinct labels, star bodies bypassing the star-expansion visibility rules, an aliased expression reclassified into a grouping key losing its alias — reported twice). r8 folds all of those. Earlier rounds — r6 folded: 
r5 (head `452479f68`): Torvalds ACK with three folds; Graefe NAK — the loud floor r5
left was wider than stated and the fix is the ordinal-bound edge, not a wider pin (folded as
the second adjacent finding's third and fourth layers); codex five findings, all folded; @claude
ACK with one stale comment, restated. r6 folds all of those. Earlier rounds — r3. r1: Graefe ACK with one non-blocking condition (assert leg alignment at the
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
    column beside it — and Q57 pins the reads bound to the quantified object of a body that
    repeats a bare leaf (an aggregate key, a sort key, a WHERE, both spellings) beside the
    aliased control; `TestFDB_UnnestExistsGather`'s `agg_cte_*` pins hold the gathered-unnest
    decline, with a unique-name twin beside the repeated-K pins so the arm tells the shape
    from the names, and an aggregate-bodied CTE over that shape so the decline is known to
    stop at star bodies.
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
13. **Sort elision across a renaming projection.** `TestSortElisionCrossesARenamingProjection`
    (the rule-time winner and extraction both elide `ORDER BY h` over `(SELECT status AS h)`
    whose source is STATUS-sorted; fails with the translation removed) and
    `TestSortElisionDeclinesAComputedSlot`; `cte_published_row_names.yaml` §6 pins
    `plan_not_contains: InMemorySort` over a renamed primary key in both spellings with the
    DESC twins; `plan_shape.golden` records the 16 corpus queries that lose their sort, and
    `ordering_through_a_projection.yaml` pins the swapped-name body (`g AS id, id AS g` over
    non-monotone g) whose sort must STAY, in both spellings, beside the elided twin.
14. **Star-body visibility.** `derived_star_visibility.yaml`: the unnest alias shadowing the
    outer column (three reads, both spellings), quoted case-distinct labels (four reads), the
    reclassified alias (both spellings); `derived_star_row_versions.yaml`: the star over
    row-versioned tables in both spellings and a two-column read.
15. **Ordering through a projection.** `ordering_through_a_projection.yaml`: the base table
    takes the index on `g` and the reverse primary-key scan; through a derived table and a CTE,
    unrenamed and renamed, the rows are right and the sort is pinned as still in memory (the
    booked receiving-side remainder), a computed slot keeps its sort.
    `TestPushRequestedOrderingThroughProjection_*` expect the pushed constraint in the child's
    current-row space.
17. **A real `__ROW_VERSION` column and a nested derived table's bare name.**
    `derived_star_visibility.yaml` §5 (both spellings, GROUP BY, ORDER BY) and
    `cte_published_row_names.yaml` §9 (join body, single-table body, CTE spelling, aliased
    control).
18. **A nested-field projection in a derived body.** `cte_published_row_names.yaml` §10: the
    single-level body, the derived-over-derived body, the aliased control, the CTE spelling.
19. **The nested leaf with a top-level homonym.** `cte_published_row_names.yaml` §11 (four
    spellings, the top-level control) and `TestFDB_QuotedDotNestedMemberLabel` (the quoted-dot
    member's value and RFC-238's label residual, both spellings, the aliased control).
20. **The alias that names a struct column.** `cte_published_row_names.yaml` §12: five spellings
    (the derived table bare and under a WHERE, the derived-over-derived body, the CTE bare and
    under a WHERE) beside the top-level control.
21. **The nested field of a declared STRUCT type.** `cte_published_row_names.yaml` §13: no
    homonym (bare, under a WHERE, the CTE, the derived-over-derived body), a homonym of another
    type, a homonym of the same type, the top-level control;
    `TestSemanticColumnFromExactTypeCarriesRecordName` (the nominal record round-trips under its
    name; the fieldless record still declines).
22. **The shape rule's decline is final, and no shape reaches a decline the walk would answer differently.**
    `TestDerivedNestedEnumFieldTypesAsStringSoTheShapeRuleNeverDeclines` (Java-authored metadata:
    an enum field beside its STRING homonym plans through the derived table and the CTE, the
    exact row is the one STRING column; red once the exact derivation carries enums) and
    `TestSemanticColumnFromExactTypeDeclinesEnum` (the bridge's own contract).
23. **An anonymous record through a derived row.**
    `TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities` (two anonymous shapes in
    one derived row, the CTE and derived-over-derived spellings, two top-level controls, and two
    VALUES rows of different shapes at top level and through a derived table: every array element
    an `api.Struct`), the anonymous arm of `TestSemanticColumnFromExactTypeCarriesRecordName`
    (round-trips with no record name; a record named RECORD keeps that name), the inline-values
    pins expecting an anonymous retagged row, and
    `TestRetagInlineValuesRecordTypeIsCopyOnWriteAndKeepsANamedSourcesName` (a named source
    keeps its name with the renamed fields); `TestFDB_ADeclaredRecordNameSurvivesTheBridge`
    adds `STRUCT RECORD` and `STRUCT foo` under a VALUES nested definition, at top level and
    through a derived table.
24. **One declared name over two shapes.** `TestFDB_OneDeclaredNameOverTwoShapesIsRefused` (the
    same-name spellings at top level, under VALUES nested definitions and through a derived
    table fail XX000; two distinct declared names beside them are structs) and
    `TestFinalizePlanReturnsTheNameClashAndKeepsTheMapForNoMessageForm` (the compile error is
    returned; a type with no message form keeps its map and its neighbour is stamped).
25. **The array of named records under a VALUES definition.** Two tuples in
    `TestFDB_ADeclaredRecordNameSurvivesTheBridge`, at top level and through a derived table.
26. **The synthetic namespace is unreachable.**
    `TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier` (seven identifiers escape outside it,
    an identifier that would have to start with it is refused, the `__type$` witness runs in both
    orders under distinct names, the genuine declared clash still errors).
16. **Bodies the walk serves.** `cte_published_row_names.yaml` §7–§8: a WHERE over a star join
    and over a union of star joins, both spellings; the named-STRUCT join body, both spellings.

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
columns. His r5 delta then measured the loud floor r5 had left — an aggregate, a sort, and (as
he found) a WHERE over a unique column of a join body that repeats a bare leaf — and named the
fix: bind the CTE or derived quantifier's edge by the row the plan flows, not by the SQL
labels. Java answers every one of these; all were pre-existing at the merge-base in the
derived-table spelling, and the CTE spelling met them once its body was published rather than
served by the name model.

**Mechanism.** The engine names runtime slots by three rules — a record constructor names a
repeated output by the name-addressability suffix (`G`, `G_2`; Java has no such suffix,
`Type.Record` keeps repeated names and binds every read by ordinal, `Expressions.java:91`,
`LogicalOperator.java:367`); a projection over a join names a repeated bare leaf by its
qualified datum key (`GA.G` beside `G`); a raw positional merge keeps every leg name verbatim —
while the SQL names a reference spells are none of those. Four consumers had let one of the
runtime names, or the SQL name, stand where the other belonged:

1. *The result-set label followed the frozen output schema for an aliased item.*
   `deriveColumnsFromProjection` took the frozen name unconditionally when the item carried a
   user alias, so `SELECT g AS a, g AS a` reported `[A A_2]`; for an unaliased reference it took
   the frozen name unless the same label had appeared at an earlier slot — a heuristic that
   reads a column-list rename of a repeated alias as a dedup. Both arms now ask the structural
   question: the NATURAL schema — the freeze site's own naming rule, `values.ProjectionSlotName`,
   deduplicated by `values.DedupFieldNames` — names the slot, and only a frozen name the natural
   schema does not produce is a rename (`frozenSchemaRenamesSlot`). The heuristic is deleted
   with its test.
2. *The derived leg's ordinal layout stated names the row does not carry.* `derivedOutputColumns`
   re-derived a projection's names by a third rule; with the label fixed, `SELECT *` over the
   repeated-name leg answered `[100 100 100 1]` — the grouping key in both `G` columns — because
   the join seed's baked read of slot 1 declined its ordinal domain (`OrdinalIn`) against a
   layout named `[G G]` for a row typed `[G G_2]`, and fell back to a by-name read of the first
   `G`. The layout takes the exact type's names when it has them — the record constructor's
   names for every slot — and applies `values.DedupFieldNames` otherwise; `mergedRVSequenceDiverges`
   compares the merged display sequence under the same rule, exactly, slot for slot.
3. *The scope stated the SQL labels as the row.* A read bound to the CTE's or derived table's
   quantified object — a WHERE, a sort key, an aggregate key or operand over a unique column —
   minted that object from the scope's columns, and the executor's edge check refused the row
   the plan declared (`edge lookup U: read as RECORD(G,G,W), declared RECORD(GA.G,G,W)`) or found
   no binding at all. The scope source already has the carrier for a row that differs from the
   columns exposed for resolution, `FlowedColumns`; `exactVirtualScopeSource` fills it with the
   exact row's own field names, the resolver takes a column's ordinal from its POSITION in the
   SQL-named list and names the read by the flowed slot (`sourceColumnOrdinal`,
   `resolvedSourceColumnRef`), and every site that installs a registered CTE into a reading scope
   carries the source WHOLE (`cteSourceAs`) instead of rebuilding it from its `Table`. The
   layout is stated only for a body whose row IS a record constructor's — a projection or an
   aggregate at the top, or a union, which flows its first leg's — because the exact type of a
   projection-less join names its fields by the leg-qualified datum keys of the retired row map,
   not by the row the executor's merge flows; a first cut stated it for every body and a WHERE
   over a gathered-unnest star derived table regressed (Graefe, r6). The derived-table join body
   and the union-bodied derived table publish their exact row first, as the aggregate and CTE
   arms do (the union-derived spelling of the repeated-leaf body was refused as the same
   edge-layout mismatch while its CTE spelling answered),
   and the exact type's projection arm applies the constructor's dedup so the two agree on a
   repeated alias (`[G G_2 N]`) — while the label derivation, which is what a source publishes
   for RESOLUTION, keeps the SQL names repeated: one per-slot naming rule
   (`projectionSlotSQLName`) feeds both, and only the type deduplicates. The first cut of the
   dedup let it reach the labels too, and `C."X"` over a body that repeats an aggregate alias
   resolved to one column instead of `42702` (Q56).
4. *The dedup could mint an authored name.* `DedupFieldNames` counted occurrences, so
   `[X X X_2]` became `[X X_2 X_2]` and a lateral unnest over the authored `x_2` could not
   address one column (codex, r5). Every authored name is reserved before a suffix is minted.

Two shapes a first cut of this round broke, both answering before it, are pinned with the
rest (codex, r5): a mixed qualified star in a CTE body, whose parsed name list has the wrong
width for the exact row (`projectionOutputNames` yields nothing for it, and the body's own
labels stand), and an aggregate body over the gathered-unnest shape, which the shape-keyed
decline must not catch (its items live in `aggCols`, `projCols` nil). A grouping key's alias
presence is now recorded by the parser (`groupColAliased`) rather than inferred from a string
comparison — `ga.g AS "GA.G"` is aliased.

All four layers are pinned in `repeated_output_names.yaml` and `cte_published_row_names.yaml`
(§ Test plan 11–12) and as Q57 of the sqldriver probe suite, in both spellings.

## Third adjacent finding, surfaced by @claude's r7 delta — fixed here

@claude asked why golden #25 (`SELECT u."GA.G" AS z FROM u ORDER BY z` over an aliased
grouping key) gained an `InMemorySort` at r7 when r6's plan had none. r6's plan had none because
r6 had LOST the alias (codex's r6 finding): the outer read resolved to the bare `G`, the name
coincided with the grouping key the streaming aggregate already orders by, and the sort was
elided on that coincidence. r7 kept the alias, and the same sort was then kept over an input
already in that order. Graefe measured the same redundant sort on a plain alias (`g AS h`) at
the merge-base: it is pre-existing, and it is every renamed column, not one query.

**Mechanism.** Java derives a map plan's ordering by pulling its child's ordering up through
the map's result value (`OrderingProperty.visitMapPlan` → `Ordering.pullUp`), so `RemoveSortRule`
compares `ORDER BY h` against an ordering that already says `H`. Go resolves ordering
satisfaction the other way round: an order-PRESERVING wrapper (`orderingDelegator`) answers
through its source group, and the request walks down the delegator chain to the member that
provides the order. That walk carried the request UNTRANSLATED through a projection or a map, so
`H` was matched against the source's key `ID` and satisfied only when the two spellings happened
to coincide. The dual of Java's pull-up is a push-down at each reshaping delegator:
`requestedOrderingBelow` restates the request through the wrapper's result value
(`RequestedOrdering.PushDownThroughValue`, the translation
`PushRequestedOrderingThroughProjectionRule` already uses for the constraint) and rebases the
pushed parts from the wrapper's child edge into the source group's current-row space
(`requestedOrderingAtInnerCurrent`). The push-down's upper alias is the root the request's parts
name — the group's current carrier for the constraint the sort rule pushed, the sort's own inner
quantifier for the keys as spelled — because both reach the walk. A part the result value cannot
express (a computed slot) drops, and a request that lost a part is not satisfiable below the
wrapper: the sort stays.

**A wrong answer under it.** The coincidence cut both ways. `SELECT u.id FROM (SELECT v AS id,
id AS v FROM ga) u ORDER BY u.id` — the body SWAPS the names — met the scan's primary key `ID`
by spelling, dropped the sort, and answered the rows in ID order for a sort on V: measured on
SimFDB at the merge-base `36b97f1e9` and at `cd7bdc5ed`, `[30],[10],[20]` over rows (1,30),(2,10),
(3,20), in both spellings. With the request pushed through the projection it is a sort on V,
which nothing provides, and the sort stays. Pinned in both spellings with `plan_contains:
InMemorySort` and rows that DISCRIMINATE — `g AS id, id AS g` over g = (200, 100, 300), not
monotone in id, so ID order and G order differ — beside the elided twin on the other column
(`ordering_through_a_projection.yaml`).

**The constraint never crossed either.** The same root mismatch sat in
`PushRequestedOrderingThroughProjectionRule`: it pushed the constraint through the projection's
result value with the INNER quantifier's alias as the upper alias and without the rebase into
the child's current-row space, so a constraint rooted at the projection's current — which is
how every constraint arrives — failed the push-down's root check and nothing was pushed. `ORDER
BY u.g` over `(SELECT g FROM ga) u` therefore never reached the index on `g`, and `ORDER BY u.id
DESC` never produced a reverse scan, while the same ORDER BY over the base table did both;
every sort through a derived table or CTE was an in-memory sort over a forward scan. The rule
now crosses through `requestedOrderingBelow` too — one translation for the constraint going down
and for the satisfaction walk — and the constraint reaches the scan group
(`TestPushRequestedOrderingThroughProjection_*` expect it in the child's current-row space).
The child cannot act on it yet: `SatisfiesRequestedOrdering` sees no order in any candidate
(for the base query too — the ordered full index scan there comes from `OrderedIndexScanRule`,
whose matcher is a sort DIRECTLY over a scan), because the scan group's one partial match is the
unadjusted LEAF and never climbs to the candidate's `MatchableSortExpression`, whose adjustment
would mint the matched ordering parts (that machinery is ported): `matchWithCandidate` refuses
at `correlatedToEquals`, Go's stand-in for Java's correlation-set equality, which demands zero
node-local correlations on the candidate expression while the candidate's own select reports
the placeholder's parameter alias and its own inner alias — two aliases Java's
`getCorrelatedTo` does not count (measured by Graefe's r9 delta, with both Go-only ordered-scan
rules filtered out). It is the only gate: with the match seeded as `MatchLeafRule` seeds it and
that gate admitted under mutation, the leaf match climbs to the `MatchableSortExpression` and
carries an ordering part (r12; an r11 reading of a second gate was a fixture artifact — a seed
without the MaxMatchMap). The closure is that set equality with those exclusions, after which
both Go-only ordered-scan rules retire. Booked in `TODO.md` ("Ordering through a projection reaches
the child group but not the index"), the refusal pinned by
`TestAdjustMatches_LeafMatchDoesNotClimb`, and
`ordering_through_a_projection.yaml` pinning both halves — the base table taking the index and
the reverse scan, the derived table and CTE still sorting. Making the projection rule loud on a
foreign-rooted constraint found the one pusher that stated its ordering over its own inner
quantifier rather than the child's current row — the group-by rule's synthesized ordering, which
reached the projection under `GROUP BY u.w ORDER BY u.w` as a root no child could act on; it
crosses the same rebase the sort and select pushes cross now.

**Where it applies.** All three delegator walks: `memberSatisfiesOrderingDepth` (satisfaction),
`pinOrderedSpineDepth` (the rule-time pin) and extraction's `rebuildOrderedSpine`, which now
carries the translated request level by level instead of re-deriving it from the sort at every
level. `ImplementSortRule` judges an order-preserving member through the walk
(`memberSatisfiesOrdering`) rather than through the member's own derived ordering, which inherits
the source's keys untranslated. `SortElisionSelector.OrderedChildWinner` takes the requested
ordering; `Planner.OrderedChildWinnerForSort` is the sort-expression entry.

**Measured.** Over the yamsql corpus (`plan_shape.golden`), 16 queries lose an in-memory sort
and none gains one — counted between `cd7bdc5ed` and `56a3df6ed` keyed by fixture file AND SQL
text (`InMemorySort` occurrences in the plan line; an entry index is not a key, a fixture's own
additions renumber it), `#25` among them; the recursive-CTE and FlatMap shapes that keep theirs
keep them. A sort over a projection's computed slot still declines
(`TestSortElisionDeclinesAComputedSlot`). The RFC-201 factory corpus moves too: 42 of 8150
committed scenarios (8150 carry a plan-shape header; the plan census dumps 8060, omitting
candidates with fewer than two TLP renderings) across 10 `single|and(…)` family files change
plan shape (re-blessed with
`factory-rebless-plan-shapes`, which verifies the renderings, schema, setup and frozen rows are
unchanged; machine ledger `retirements/2026-09-05-rfc242-a-sort-is-judged-through-its-source-group.json`
over base commit `36b97f1e9`, prose entry in `RETIREMENT_LEDGER.md`, drift classified by
`factory-plan-census` with no regression class present). Those are not renamed columns: `SELECT * FROM t_rd WHERE c = 1 AND id < 3 ORDER BY c
NULLS LAST, id` now plans as `Fetch(PredicatesFilter(IndexScan(IDX_C, [=])))` with no sort, where
the merge-base sorted a filtered scan. The fetch is an order-preserving wrapper, and judging it
through its source group (the walk) sees the index scan's RICH ordering — C equality-bound, ID
ascending under it — where the wrapper's own inherited plain ordering had lost the bound key.
That is Java's `RemoveSortRule` equality-bound arm answering through the delegator, and it is
order-correct: the index stores (c, id), the residual filter preserves order.

## Folds at r8

- **Quoted, case-distinct labels answered the first column twice** (codex r7 P1, a silent wrong
  answer where the merge-base refused): the resolver took a reference's ordinal by a folded first
  match over the source's labels, so `c."x"` and `c."X"` over `(SELECT foo AS "x", bar AS "X")`
  both read slot 0. `sourceColumnOrdinal` matches the exact spelling first and falls back to a
  folded match only when it is unique, in both of its layouts (`derived_star_visibility.yaml`
  §3).
- **A star body bypassed the star-expansion visibility rules** (codex r7 P1): r7's exact-first
  derivation labelled a projection-less star body by the exact row, which neither shadows an
  outer column under an unnest AS/AT alias nor hides the ephemeral `__ROW_VERSION` pseudo-column
  (Java's `nonEphemeralVisible`). The derived join-body builder takes the same order the CTE arm
  takes — the unnest builder first for a star body, then the exact row unless it carries the
  pseudo-column (`exactStarRowCarriesAnEphemeral`), then the catalog walk — and the CTE join arm
  applies the same pseudo-column decline. The uniqueness gate r7 deleted had declined the
  row-versioned star join by accident (both legs carry `__ROW_VERSION`); this declines it by
  rule (`derived_star_visibility.yaml` §1, `derived_star_row_versions.yaml`).
- **An aliased expression reclassified into a grouping key lost its alias** (codex r7 P1,
  reported by both runs): `v / 10 AS bucket … GROUP BY v / 10` is classified as an expression
  item and turned into a grouping key after GROUP BY parsing, and r7's alias-provenance flag was
  set only on items born as grouping keys. It is set on every item now
  (`derived_star_visibility.yaml` §4).
- **The eighth CTE-install site** (Torvalds): `buildWherePredicateFromCTEScope` installed a CTE
  source without `cteSourceAs`; routed. The named-STRUCT join body, the one class the exact
  derivation declines and the walk still serves, is pinned in both spellings
  (`cte_published_row_names.yaml` §8).
- **Coverage** (@claude, Graefe): the union-bodied derived table's motivating shape (a WHERE and
  a GROUP BY, both spellings, plus the repeated-name read) and the star-union shapes Graefe found
  answering at r7 are pinned (`cte_published_row_names.yaml` §5, §7); the DESC twins pin that the
  order is real (§6); the fixture comment cites RFC-238 §2's `qualifierStrippedLabel` residual
  rather than §7d.

## Folds at r9

- **The swapped-name body** (Graefe, Torvalds): a wrong answer at the merge-base and at
  `cd7bdc5ed` — pinned in both spellings with the sort that must stay (§ Third adjacent finding,
  "A wrong answer under it"; `cte_published_row_names.yaml` §6).
- **The constraint crosses the projection** (Torvalds's booking, half): the projection push rule
  translates through `requestedOrderingBelow` and accepts only a current-rooted constraint; the
  receiving side — the leaf match that never climbs past `correlatedToEquals` — is booked (§ Third
  adjacent finding, "The constraint never crossed either"; `ordering_through_a_projection.yaml`).
- **No folded fallback in `sourceColumnOrdinal`** (Torvalds): a panic before both fallback loops
  over the explaindiff corpus (2736 queries) and the SimFDB probes never fired; the loops are
  gone, both walks match by exact spelling, and the doc above them says so.
- **`groupColAliased` on the promoted expression items** (Torvalds), and the claim narrowed to the
  one item not built from a SELECT-list item — the ORDER-BY-harvested aggregate.
- **Counts with their method** (Torvalds): 16 golden entries lose a sort, by entry index; the
  factory ledger names 8150 scenarios with a plan-shape header and 8060 census dump lines.
- **The row-versioned remainders pinned as negatives** (@claude): `derived_star_row_versions.yaml`
  (0AF00 / XX000) and `TestFDB_DerivedStarRowVersionsWhere` (`edge lookup D`).
- The two new fixtures carry their description above `name:` so `FEATURE_MATRIX.md` shows it;
  the status line's dangling round marker is gone; the sort rule's comment records the
  two-space coverage arms.

## Folds at r10

- **The receiving-side booking, restated** (Graefe, measured): not missing ordering parts but a
  leaf match that never climbs past `correlatedToEquals` (§ Third adjacent finding, "The
  constraint never crossed either"; `TODO.md`; the fixture header). The refusal is pinned by
  `TestAdjustMatches_LeafMatchDoesNotClimb`: a value-index candidate, a
  scan-leaf match, no adjusted twin, and the candidate's select reporting the node-local
  correlations the stand-in refuses on.
- **The swap pin discriminates** (Torvalds): moved to `ordering_through_a_projection.yaml`, whose
  `g` is not monotone in `id`, so the rows differ between the two orders.
- **XX000 pinned where it cannot be credited** (Torvalds): the coverage classifier counts any
  non-0A SQLSTATE as a correct rejection, so the CTE spelling of the row-versioned unnest star is
  pinned in Go (`TestFDB_DerivedStarRowVersionsUnnestCTE`); the yaml keeps the 0AF00 half.
- **A foreign-rooted constraint is loud** at the projection push rule (`call.Fail`), as at the
  select push rule; the rule's history comments are cut to the why; its tests expect the failure
  and assert the pushed root and column without the shared rebase helper. Going loud found the
  group-by push rule stating its synthesized ordering over its own inner quantifier; it is
  rebased into the child's current-row space now (its tests expect that space).
- The alias-provenance doc names its two readers and the items they consult; the lost-sort count
  is keyed by fixture file and SQL text; the fixture header separates the negative pins from the
  permanent positives; the status line is one list; the test plan is numbered in order.

## Folds at r11

- **The group-by rule's PRESERVE branch rebases too** (Graefe required, codex P2): the keys it
  pushes under ANY are stated in the child's current-row space, pinned by
  `TestPushRequestedOrderingThroughGroupBy_PreserveWithKeysPushesCurrentRootedKeys`.
- **The XX000 yaml pin is gone** (Graefe, @claude): the codex run that reviewed r9 executed
  `git restore --worktree .` under the uncommitted r10 edits and reverted them; the removal was
  redone from the record, with the duplicate 0AF00 entry and the stale comment.
- **The pseudo-column is the one of VERSION type** (codex P2): a real `"__ROW_VERSION" STRING`
  column is star-visible and is not it; classifying by name alone declined the body's row and
  `WITH d AS (SELECT * FROM rv, rw) SELECT d.v FROM d WHERE d."__ROW_VERSION" = 'a'` did not
  plan — at the merge-base either. Both spellings, a GROUP BY and an ORDER BY are pinned
  (`derived_star_visibility.yaml` §5).
- **A nested derived table's unaliased qualified output** (pre-existing, surfaced by Graefe's
  probes): `SELECT x.w FROM (SELECT u.w FROM (…) u) x` was 42703 because the derived-of-derived
  scope named the column by its display spelling U.W; it is the bare name now, with a join body,
  a single-table body, the CTE spelling and an aliased control pinned (`cte_published_row_names.yaml`
  §9).
- **The climb pin is named for what it measures** (Torvalds): `TestAdjustMatches_LeafMatchDoesNotClimb`.
  (r11 also read a second gate into it; r12 shows that was a fixture artifact — see Folds at r12.)
- Torvalds's other folds: the projection rule's message states the fact (an outer key can arrive
  foreign-rooted through a select pusher without any pusher having erred); a rootless request's
  silent decline is pinned (`…_NoRootDeclinesSilently`); the alias-provenance doc says
  "every SELECT-list-born item" (r12: "every non-aggregate SELECT-list item"); the indexing in the
  climb pin's failure path (`GetQuantifiers()[0]`, a panic on a quantifier-less parent) is gone.
  The "duplicate BUILD entry" two laps saw is not one: `adjust_match_leaf_climb_test.go` appears
  once in the cascades test target's `srcs` and once in its gazelle-generated `embedsrcs`, as
  every test file in that target does; removing the `srcs` entry fails the build-membership gate.
- **Booked, not fixed:** an IN subquery over an aggregate or DISTINCT body does not translate
  (four shapes, identical at the merge-base; `TODO.md`).

## Folds at r12

- **The climb pin is a sentinel now** (Graefe, Torvalds): seeded through `matchLeafWithCandidate`
  — the MaxMatchMap built, as `MatchLeafRule` always builds it — the leaf match climbs through the
  candidate's select to the `MatchableSortExpression` with an ordering part when
  `correlatedToEquals` is admitted under mutation, and `TestAdjustMatches_LeafMatchDoesNotClimb`
  goes red. r11's seed carried no MaxMatchMap, so that mutation stopped at the select adjuster's
  nil-map check, which r11 read as a second gate refusing the placeholder; Go admits an unbound
  placeholder as Java does. `correlatedToEquals` is the only gate, and the three homes say so.
- **A nested-field projection in a derived body** (pre-existing, Torvalds's probe): `SELECT x.x
  FROM (SELECT t1.w.x FROM t1) x` and the derived-over-derived spelling were refused as a
  projection slot with no resolved Value — the catalog walk looked the bare leaf up as a column,
  failed, and declined the whole resolver. The nested path is decided by its shape before any
  lookup, and a failed lookup of two or more segments goes to the exact derivation, in all three
  arms (`cte_published_row_names.yaml` §10; the two rules as they stand at r14).
- **Two r11 bullets restated as measured** (@claude): the "duplicate BUILD entry" is the test
  file's gazelle-generated `embedsrcs` entry beside its `srcs` entry; the climb pin's failure path
  lost an indexing that could panic, not a `panic(`. The alias-provenance doc says "every
  non-aggregate SELECT-list item" (Torvalds).

## Folds at r13

- **The nested path is decided by its shape** (Torvalds, measured): r12 decided it after the
  catalog walk had looked the leaf up as a column, so `SELECT x.sk FROM (SELECT st2.p.sk FROM
  st2) x` over a table with a top-level STRING `sk` beside the struct column typed the slot as
  that STRING (0AF00 "declared RECORD(SK:STRING?)", 42804 under a WHERE, a raw resolution error
  under an expression — identical at the merge-base). `nestedProjectedPath` strips the body
  source's qualifier and sends two or more remaining segments to the exact derivation before any
  lookup, in the single-table arm, the derived-over-derived arm and the CTE arm; r13 deleted the
  post-lookup branches, which r14 restores beside it (see Folds at r14)
  (`cte_published_row_names.yaml` §11: four spellings beside the top-level homonym).
- **The quoted-dot nested member's label** (codex P2): `SELECT x."a.b" FROM (SELECT tq.s."a.b"
  FROM tq) x` is admitted now and reads the member's value; its label is `b`, over the base
  table and through the derived table alike — RFC-238 §2's declared residual
  (`qualifierStrippedLabel`: a nested member is not a top-level field, so the dot in its name is
  stripped as a qualifier). Pinned as that residual end to end
  (`TestFDB_QuotedDotNestedMemberLabel`: value right, label `b`, red once the residual closes);
  closing it is RFC-238's, not this RFC's.
- The alias-provenance doc says "every SELECT-list item that is not a bare aggregate call".

## Folds at r14

- **The post-lookup net returns, in all three arms** (Torvalds, measured): the shape rule strips
  the body source's qualifier, so with `FROM st2 AS p` and a struct column `p`, `p.co` lost its
  qualifier, the leaf lookup found no top-level `co`, and the arm declined — 0AF00 in the derived
  spellings where r12 answered `[6]`. Java's `lookupNestedField` resolves `P.CO` through the
  attribute `P` when the qualified form `P.P` fails; the Go reading of that is the branch r12 had
  and r13 deleted: after a failed lookup, a reference of two or more segments goes to the exact
  derivation, and a one-segment miss still declines without a body build. Both rules now stand
  in the single-table arm, the derived-over-derived arm and the CTE arm. The CTE arm never had
  the net: its bare projection answered through a later fallback, and a WHERE on the published
  column failed to translate (0AF00, identical at the merge-base) because that fallback publishes
  no typed row. Five spellings pinned beside the top-level control
  (`cte_published_row_names.yaml` §12).
- **A nominal record is published by the exact derivation** (codex P2, chased to its cause): the
  shape rule's unconditional return made a declared-STRUCT nested field (`tn.p.child`, `child` a
  `child_s`) 0AF00 where the leaf lookup had answered — but only when the table happened to carry
  a top-level homonym of the same type. With no homonym the read was 0AF00 at the merge-base too,
  and with a homonym of another type it was refused as a column that does not exist (42703):
  `semanticColumnFromExactType` declined every record not literally named `RECORD`, citing a
  carrier `semantic.Column` does not have — it has one, `StructTypeName`, the field the forward
  bridge (`expr.structColumnType`) reads first, added in the same commit as the gate. The exact
  derivation now publishes a nominal record as `RECORD` carrying its name there, so the round trip
  mints the same named `values.RecordType`; record identity is name-insensitive under both
  equalities (`RecordType.Equals`, the exact canonical form) as Java's `Type.Record.equals` is.
  The fieldless record still declines. Pinned in seven spellings (`cte_published_row_names.yaml`
  §13) and at the bridge (`TestSemanticColumnFromExactTypeCarriesRecordName`).
- The §11 fixture comment says four spellings, which is what it pins.

## Folds at r15

- **The shape rule's decline is final again** (Graefe): r14 made the shape rule's exact route a
  first try that fell through to the leaf lookup on a decline — so a shape-decided path whose
  exact derivation declines would be looked up by its leaf and typed as a top-level homonym,
  the r13 error re-legitimised by a comment. All three arms now return the exact derivation's
  answer unconditionally: a decline is the whole source declining, and every reader of it
  reports the unresolved slot. Measured on the way: every pinned shape-true body succeeds on
  the exact route (§11, §13, the derived-over-derived body: 5 of 5), the alias that names a
  struct column never fires the shape rule (one segment after the strip; the post-lookup net
  alone covers it), and the one leaf Graefe named as still declining — an enum-typed field,
  reachable from Java-authored metadata only — does not: the exact logical derivation types
  an enum field as STRING (the catalog kind `ENUM` bridges forward to STRING) before the
  bridge sees it, so no shape reaches a decline the walk would answer differently (the decline set is wider than the bridge's arm and a NULL literal beside the path reaches it; see Folds at r16). That is the negative result
  pinned on a descriptor-built table with a STRING `color` beside the enum `p.color`
  (`TestDerivedNestedEnumFieldTypesAsStringSoTheShapeRuleNeverDeclines`: the derived and CTE
  spellings plan, the exact row is the one STRING column; it goes red when the exact
  derivation starts carrying enums, naming the homonym shape as the one to pin as a loud
  decline), and at the bridge (`TestSemanticColumnFromExactTypeDeclinesEnum`: an enum has no
  lossless semantic carrier). This also removes Torvalds's r14 nit — a shape-true decline no
  longer rebuilds the identical body through the net.
## Folds at r16

- **The decline set stated as it is** (Graefe, measured): r15's comment and test-plan title said
  the exact derivation's decline is "reached by no shape today". The set is wider than the
  bridge's default arm — `exactVirtualScopeSource` declines on any inexact result type, a width
  disagreement or a label failure — and a shape reaches it: `SELECT x.sk FROM (SELECT t.p.sk,
  NULL AS n FROM t) x` declines (`placeholder type is not exact`). It does not discriminate r14
  from r15 — the NULL slot declines the walk arm too, and the translator refuses `SELECT NULL AS
  n FROM t` at top level with the same 0AF00 — so finality decides no outcome there; the enum is
  the only leaf where it would, and the enum arrives typed STRING. The comment at the arm, the
  pin's comment and test plan 22 now say so.
- **An anonymous record stays anonymous through the bridge** (codex P2, reproduced over FDB):
  r14 admitted a record constructor's row through the exact derivation, and the bridge back
  (`expr.structColumnType`) named every record with no `StructTypeName` by the SQL kind
  `RECORD` — so `SELECT [x.s], [x.q] FROM (SELECT (1 AS lat, 2 AS lon) AS s, (3 AS z) AS q FROM t)
  x` put two different shapes under one descriptor name, the synthesized result descriptor did
  not compile, and the driver handed both array elements back as raw maps where the same two
  shapes at top level, never bridged, are structs. `Type` is the SQL kind, never a name: an empty
  `StructTypeName` now rebuilds an anonymous record, and the proto repository mints a unique
  message name per anonymous shape (Java's `ProtoUtils.uniqueTypeName`, deterministic here).
  The public struct name is unchanged (`publicOrdinalTypeName` renders an anonymous record as
  `RECORD`). Pinned over FDB in three bridged spellings beside two top-level controls
  (`TestFDB_AnonymousRecordsThroughADerivedRowKeepDistinctIdentities`, red under the old
  fallback name) and at the bridge (the anonymous arm of
  `TestSemanticColumnFromExactTypeCarriesRecordName`).
- **The enum-as-STRING typing is booked on its own** (Graefe): `sqlTypeToCascadesType("ENUM")`
  is `TypeString`, so the exact derivation is inexact one layer before RFC-232's carrier gap;
  `TODO.md`, "The exact derivation types an enum field as STRING", pointing at the pin that
  goes red when it closes and at the nullable-element entry beside it.
- **A table with a fieldless nested-message column is unqueryable** (found while measuring
  \@claude's r14 shape — an exact-derivation decline for a reason unrelated to the nested
  path): `expr.structColumnType` turns a fieldless record into UNKNOWN and the flowed row then
  resolves nothing, `SELECT t.sk FROM t` included; Java-authored metadata only, identical at
  the merge-base. `TODO.md`, "A table with a fieldless nested-message column cannot be queried
  at all", with the reproducer and the closure.
## Folds at r17

- **A VALUES row is an anonymous record too** (Graefe and Torvalds, both measured over FDB):
  r16's rule — the kind is never a name — was contradicted one site over. The inline-values
  retag (`retagInlineValuesRecordType`) minted every VALUES row as a record NAMED `RECORD`, the
  bridge laundered that name back to anonymous with a special case, and two VALUES rows of
  different shapes (`SELECT a.w, b.v FROM VALUES ((3, 4)) AS a(w(x, y)), VALUES ((5)) AS
  b(v(z))`) still claimed one descriptor and came back as raw maps — codex's P2 through a
  second door, pre-existing at the merge-base. The retag mints an anonymous record, the bridge
  carries a record's name unconditionally (a record that happens to be named `RECORD` is a
  named record and keeps it), and the VALUES spellings — two shapes at top level and through a
  derived table — join the FDB pin.
  One visible change rides with it: a VALUES row's nested record now reports the synthesized
  anonymous name (`__type__…`, Java's `ProtoUtils.uniqueTypeName` spelling) as a record
  constructor's row already did, where the retag's minted name made it read `RECORD`; the
  inline-values execution pin says so (`TestFDB_InlineValuesExactExecution`).
  codex's P2 on r16 is the same finding from the other side: a struct literal DECLARED with the
  name `RECORD` (`STRUCT RECORD (1 AS lat, 2 AS lon)`) came back under a synthetic `__type__`
  name after the bridge while keeping `RECORD` at top level; the unconditional carry keeps it
  (`TestFDB_ADeclaredRecordNameSurvivesTheBridge`: the derived and CTE spellings and the
  top-level control all report the declared name).
- **The fieldless booking names its second population** (Graefe): the catalog also leaves
  `StructFields` empty for a self-referential message re-entered on the descent path
  (`columnForField`'s recursion stop), so a recursive message column poisons its table the
  same way, and a fieldless `values.RecordType` cannot serve that arm without a carrier that
  tells the two apart. Said in the entry.
## Folds at r18

- **A nested column definition keeps the record's name** (Torvalds, measured; Graefe booked the
  same): r17's retag guard refused any NAMED source, which widened a pre-existing divergence to
  the `RECORD`-named literal — `SELECT A.W FROM VALUES (STRUCT RECORD (3 AS P, 4 AS Q)) AS
  A(W(X, Y))`, accepted at the merge-base, was refused at r17 as a nominal record. Java's
  `TypeUtils.setFieldNames` renames a record's fields and keeps its name
  (`fromFieldsWithName` when named, `fromFields` when not) and never rejects a source. The
  retag now does the same: a named source keeps its name with the renamed fields, an anonymous
  source stays anonymous, and the two-shape collision stays fixed. Pinned at the retag
  (`TestRetagInlineValuesRecordTypeIsCopyOnWriteAndKeepsANamedSourcesName`) and over FDB in
  `TestFDB_ADeclaredRecordNameSurvivesTheBridge` (`STRUCT RECORD` and `STRUCT foo` under a
  nested definition, at top level and through a derived table: the declared name and the
  renamed fields).
## Folds at r19

- **One declared name over two shapes fails loudly** (Torvalds; Graefe measured the same and
  booked it): `SELECT [STRUCT foo (1 AS p)], [STRUCT foo (2 AS p, 3 AS q)] FROM t` — two record
  literals declared with one name and two shapes, bridged through a derived table or not —
  came back as raw maps with no error, because `FinalizePlan` swallowed every descriptor error
  as "a type with no message form" and let the constructors keep their map representation.
  Java's `TypeRepository.build` throws on the duplicate message name
  (`IllegalStateException(DescriptorValidationException)`, TypeRepository.java:100). The clash
  is now refused where it is known exactly — at definition, when a DECLARED name is reached for
  a second shape (`values.DeclaredNameClashError`, `defineRecordLocked`) — `FinalizePlan`
  returns it, and the three planning paths report it as XX000. Every other descriptor failure
  stays what it was, swallowed: a type with no message form, a name protoname cannot escape, and
  a synthesised file that does not validate for any other reason — the last reached by a join row
  carrying one field name twice, which a nested FULL OUTER JOIN produces and answers today.
  Making every compile failure a query failure broke that working query, measured
  (`TestNestedFullOuter_AncestorNullExtensionReachesLeg`). That row is booked (`TODO.md`, "A join
  row that names one field twice leaves its plan's rows unstamped") and pinned at r21.
  Pinned over FDB
  (`TestFDB_OneDeclaredNameOverTwoShapesIsRefused`: the same-name spellings at top level, under
  VALUES nested definitions and through a derived table fail XX000, and two distinct declared
  names beside them are structs; red with the swallow restored) and at the walk
  (`TestFinalizePlanReturnsTheNameClashAndKeepsTheMapForNoMessageForm`). Pre-existing at the
  merge-base for the unbridged spelling; r18 had opened one more spelling to it.
- **The array-of-named-record spelling is pinned** (Graefe): `VALUES ([STRUCT foo (3 AS p, 4 AS
  q)]) AS a(w(x, y))` at top level and through a derived table, in
  `TestFDB_ADeclaredRecordNameSurvivesTheBridge` — the shared retag arm, measured rather than
  inferred.
## Folds at r20

- **The synthetic namespace is unreachable, and the guard is Java's** (Torvalds, measured;
  Graefe named the same non-blocking): r19 refused the clash in the record arm and its comment
  claimed anonymous shapes never take part. They can. `__type$` escapes to exactly `__type__1`
  (protoname preserves a leading `__` and turns `$` into `__1`), the same name the counter
  mints, so with an anonymous record defined first the query was refused as a clash it is not —
  Java names its anonymous types by random UUID and runs it — and with the declared struct
  first the file failed to validate and EVERY record in the plan fell back to a map. Skipping a
  taken name at mint time fixes only the second order. Go now buys by CONSTRUCTION what Java
  gets from randomness: synthetic names live under `__0type__`, and `__0` is an invalid START
  sequence for an identifier while nothing escapes INTO a leading `__0` (the escapes insert
  `__0`/`__1`/`__2`, each starting with an underscore, so an output starting `__0` needs an
  input starting `__0`, which is rejected). The guard moves to `registerLocked`, where Java
  keeps it (`registerTypeToTypeNameMapping` throws `IllegalArgumentException` on a name already
  bound to a different type) so every path that names a type is covered by one check, and it
  now only ever sees a genuine declared-vs-declared clash. Pinned
  (`TestSyntheticTypeNamesAreUnreachableFromAnyIdentifier`: seven identifiers escape outside the
  namespace, an identifier that would have to start with it is refused, the witness runs in both
  orders under distinct names, and the genuine clash still errors).
- **The swallow arm says what it covers** (Torvalds): its comment named only "a type with no
  message form", while a file that fails to validate for another reason returns through the same
  call. Both sites in FinalizePlan say so; a THIRD — RecordConstructorValue.Evaluate's own doc,
  where a reader lands asking why a row is a map — still asserted that every constructor in a
  plan is stamped, and r22 corrects it.
- **The r19 booking was withdrawn here on a FALSE NEGATIVE; r21 restores it.** r20 argued the
  booked row cannot exist because a record constructor disambiguates repeated names, and cited a
  probe reporting the swallow arm entering zero times over the whole `core/embedded` suite. Both
  halves were wrong, and r21 records the correction with the measurements — the probe printed to
  the test binary's stderr, which `go test` discards for a passing package unless `-v` is given.
  Read the r21 fold, not this paragraph.

## Folds at r21

- **The r19 booking is RESTORED, and r20's withdrawal is retracted** (Graefe and codex each
  measured the shape; codex named the reproducer): r20 reasoned that a record constructor
  disambiguates a repeated field name — true of `NewRecordConstructorValue` (`ID`, `ID_2`) and
  FALSE of `NewRawRecordConstructorValue`, which keeps names VERBATIM by design for ordinal-join
  seeds, where two legs of `SELECT * FROM a JOIN b` both carry `ID` and positional access makes
  the duplicate unambiguous. r20 also leaned on a probe reporting the swallow arm entering zero
  times; that was a false negative, because the probe printed to the test binary's stderr and
  `go test` discards a passing package's output without `-v`. Re-run with `-v`, the very query
  r19 named produces the row: `RECORD<ID, S, BID, FOO, ID>` fails descriptor validation on the
  duplicate `ID` — and the damage is not that row: compilation is per-repository, so THREE of the
  four constructors in that plan end up without a descriptor though only ONE repeats a name (r22 measures it). The entry is restored with r19's attribution intact, and the
  reproducer is committed as the pin rather than a probe deleted afterwards
  (`TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped`, as r22 re-cut it: the planned FULL OUTER
  JOIN still holds a duplicate-name row, a row repeating NO name lost its descriptor beside it,
  and one constructor is stamped — the last being what proves the plan was baked at all).
  Java refuses the row outright (`normalizeFields` disambiguates INDEXES, not names; two `ID`s
  throw in `computeFieldNameFieldMap`), so the silence is Go's divergence.
- **The both-orders witness was vacuous** (Graefe and codex, measured): the arm declared a record
  named `__type__1`, which escapes to `__type__01` and never collided with the counter under any
  prefix. The witness is `__type$`, which escapes to exactly `__type__1`; the arm uses it now.
  The test's first loop is what went red under the old prefix, and it carried the vacuous arm.
- **The swallow arm enumerates rather than counts** (Torvalds): r20 replaced "only a type with no
  message form" with "TWO failures reach that arm", the same over-claim one layer up — a record
  or field name protoname cannot ESCAPE is a third, and is not a missing message form. The
  comment lists all three and names the shape that reaches the last.
- **`DeclaredNameClashError.Name` is the escaped message name** (both reviewers): its doc said
  "the declared record name" while the guard, like Java's, compares proto names (`a.b` is
  reported as `a__2b`).

## Folds at r22

- **The blast radius is the whole repository, and three durable sentences understated it**
  (Torvalds, measured): a row that names one field twice has a message form, so the failure
  surfaces only when the FILE is compiled — and compilation is per-repository, with the bad
  message left in it. Every type asked for afterwards fails the same way: on the pinned query THREE of the four
  constructors end up without a descriptor though only ONE repeats a name, while the fourth, resolved before the
  bad message was appended, keeps its descriptor, so which rows survive is walk-order dependent. `TODO.md`, the r21 fold and
  the swallow comment each said "three constructors" or "the row"; all three now state the
  population and the order dependence, and the mechanism is pinned where it was found
  (`TestDuplicateFieldNameRowPoisonsTheWholeRepository`, in `values`).
- **The SQL pin baked nothing, and its surviving arm was a tautology** (Graefe measured the first,
  Torvalds the second): the reproducer plans through `PlanRecordQueryWithSubqueries`, which
  returns the plan UNBAKED, so every constructor was trivially unstamped; and "every
  duplicate-name row is unstamped" cannot fail, because such a row can never be stamped. The r21
  mutation that appeared to confirm it only exercised the other arm. The pin now bakes, and asserts what
  discriminates: a COLLATERAL row (repeats no name, lost its descriptor anyway) and the SURVIVOR
  beside it, the survivor being what proves the bake ran — measured red both ways, with the bake
  removed and with the raw constructor deduplicating.

## Folds at r23

- **The absolute was inverted, not removed, and three copies said so** (Graefe and Torvalds,
  both measuring `constructors=4 duplicates=1 unstamped=3 stamped=1`): r22 replaced "every
  constructor in a plan is stamped" with "leaves every constructor in that plan a map" — false in
  the other direction, contradicting the pin's own survivor assertion in the same commit. Three
  homes carried it: `RecordConstructorValue.Evaluate`'s doc (written by r22), the pin's own
  header (untouched since r21, six lines above the assertions that disprove it), and the r22
  fold's description of what the pin asserts — that last inside the bullet claiming to fix
  exactly this, and naming a "control query" r22 had removed. The r21 fold's description was a
  fourth, still calling the pin the tautology r22 re-cut. All four stated three-of-four with the survivor
  and the order dependence after r23 — but only four: r24 found five more sites still carrying
  the map framing, so r23's claim that they were "found by grepping for the claim rather than by
  remembering where it was written" was itself false, and is retracted in the r24 fold.
- **The data loss was never there** (codex P2, measured over FDB): every home said the unstamped
  join row keeps a map "in which the SECOND `ID` overwrites the first". It does not. The plan
  paths that emit a row — `executeProjection`, the flat-map cursor's record-constructor arm,
  `evaluateOrdinalJoinRow` — build a dense positional row field by field, and the result set
  reads those slots by ORDINAL, so both `ID`s arrive with their own values. The map branch of
  `RecordConstructorValue.Evaluate` is not even entered for those constructors (probed: the only
  entries during that query are the INSERT path's). What the poisoning costs is descriptor
  IDENTITY — such a row cannot be handed out as an `api.Struct` — and the title says "leaves its
  plan's rows unstamped" now. TWO claims in this bullet were later refuted and are retracted
  here rather than left standing: the measurement `(1, 1, true)`, which was taken on the
  equal-slot fixture r24 replaced (the pinned shape returns `(1, 2, true)`, `(2, NULL, true)`,
  `(NULL, 1, NULL)`), and the conclusion that the booking is therefore LATENT — r24 measured a
  computed STRUCT coming back a raw map through this plan, which is user-visible. See Folds at
  r24 and r25. The pin is
  `TestFDB_ADuplicateNameJoinRowLosesItsStructTypeNotItsValues`.
- **The third exception's qualifier was dropped** (both reviewers): `Evaluate`'s enumeration said
  "a row whose descriptor cannot VALIDATE" and then "none of the three fails the query" — but a
  declared-name clash is a validation failure that DOES fail the query. The arm carries its "for
  a reason other than a declared-name clash" qualifier again, as `FinalizePlan`'s does.

## Folds at r24

- **The retracted framing survived in five more sites, and r23 claimed otherwise** (Graefe and
  Torvalds, each grepping): r23 removed the data-loss claim from the homes it remembered and
  wrote that it had found them by grepping. It had not. `plan_finalize.go` still said "end up
  maps" three lines above r23's own "costs descriptor IDENTITY rather than data"; `TODO.md` said
  it too, and kept "nothing reads those rows by name" inside the entry r23 retitled; the values
  mechanism pin's header still said "flows as a map" and "fall back to maps" and quoted the
  RETIRED entry title, leaving its closure pointer dangling; and the planner pin's own NAME still
  ended `…JoinRowAMap`. All are swept, the pin is
  `TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped`, and the sweep was verified with the
  greps in both reviews rather than asserted.
- **The LATENT classification was wrong** (codex P2, measured over FDB): a computed STRUCT
  selected through the poisoned plan comes back as a raw `map[string]any` where the same CTE read
  without the duplicate-name join returns an `api.Struct` — same values, wrong type, because
  there is no descriptor to present it with. The booking is user-visible, not latent, and says
  so. codex also showed the no-loss half could not discriminate: both `ID` columns were inserted
  as 1 and the join predicate required them equal, so one slot read twice passed as a preserved
  pair. The predicate is `a.id + 1 = c.id` over ids 1 and 2 now, and the pin requires a row whose
  two slots differ. Its scope sentence is corrected too: the loss falls on constructors resolved
  AFTER the bad message, not on every computed row — the root projection was resolved first and
  keeps its descriptor.
- **The negative could go vacuous** (both reviewers): `TestFDB_ADuplicateNameJoinRowStillReturnsEveryField`
  asserted no census, and its sibling asserted one on a DIFFERENT query, so nothing pinned that
  the shape it reads back is still the poisoned one — the day the booking closes it would pass
  silently. Both pins run the same query text now, the planner one asserting the census that is
  the other's precondition, each naming the other.
- Torvalds probed the latent framing and it holds: a stored STRUCT column read through the
  poisoned plan still returns an `api.Struct`, because it carries the STORED descriptor rather
  than a constructor's.

## Folds at r25

- **The refuted classification was still standing in the bullet r24 edited** (Torvalds): r24
  updated that bullet's pin NAME and left above it both the stale measurement `(1, 1, true)` —
  taken on the equal-slot fixture r24 itself replaced — and the sentence "so the booking is
  LATENT, not a wrong answer", which codex had refuted. That is the failure r24 was cut to fix,
  reproduced inside r24; both are retracted in place now, and the greps re-run.
- **The pin asserts values and the whole result, not shapes** (codex, three P2): it accepted any
  non-empty result containing one unequal `ID` pair and never checked `foo`, so a dropped
  null-extended row or an aliased pair stayed green; it accepted ANY map for the poisoned struct,
  including an empty one, and checked the control only for its type; and Torvalds's stored-STRUCT
  measurement had no committed witness. The pin now asserts the exact unordered rows
  `(1,2,true) (2,NULL,true) (NULL,1,NULL)`, the values `X=1 Y=10` in BOTH representations, and a
  stored struct column through the same poisoned join keeping its `api.Struct` and its value —
  which is also what bounds the blast radius to COMPUTED rows.
- **The control is sharpened to isolate the duplicate name** (Torvalds, measured): the shipped
  control removed the join AND the duplicate name together, so it could not tell which caused
  the raw map. Removing only the duplicate name — the same FULL OUTER JOIN topology over
  `(SELECT id AS cid FROM c_md)` — still returns an `api.Struct`, so the attribution is to the
  repeated name rather than to joining. That control is what the pin runs.

## Folds at r26

- **The booking still shipped the retired control** (Graefe and Torvalds, independently): r25's
  own fold declares the blunt control insufficient — it removed the join AND the duplicate name,
  so it could not say which caused the raw map — and the live `TODO.md` entry went on describing
  exactly that control, in two places. It was also false about what the pin runs. The entry
  states the sharp control now: the same FULL OUTER JOIN with only the repeated name removed.
  Third round in a row that a retraction reached the code and missed a durable home; the greps
  r25 re-ran covered the LATENT wording, not this.
- **The stored half had no poison witness** (both reviewers): it ran a THIRD query text with no
  census and no control, so "the damage is confined to computed rows" rested on an assertion
  that stays green if the shape stops being poisoned. Graefe's guard is the one taken: read the
  computed struct and the stored column out of ONE row of the poisoned join — the computed value
  beside it is the witness, and if the plan is ever clean that assertion fails instead of
  passing as a bound. (r27 had to finish the job: the first cut of that guard made the witness
  row repeat TWO names, so the control removed only one of them and could attribute nothing.)
- **The two axes are named apart** (Torvalds): "blast radius" was doing double duty eight lines
  apart — WITHIN a plan the failure spreads across the whole repository, ACROSS row kinds it
  stops at computed ones. Both say which they mean now.
- The planner pin's header said the struct half runs "a different text"; there are two, and it
  names them and says the census speaks for neither.

## Folds at r27

- **The witness repeated TWO names, so the control attributed nothing** (Graefe, Torvalds and
  codex, each measuring it independently): r26's one-statement guard reads a computed struct
  beside a stored one, and both were called `R` — so the join row repeated `R` as well as `ID`,
  and the control, which removes only the repeated `ID`, still ran over a poisoned plan. Graefe
  measured the table: strip the `ID` repeat alone and the computed value is STILL a map; strip
  both and it is a struct. The comment and the booking each claimed the pair "differs only in
  whether a leg repeats `ID`", which was false of the read being performed — a scope sentence
  written from intent again. The CTE names its struct `RR` now, so the witness repeats exactly
  one name and the control removes exactly that; measured red by putting the repeat back.
- **Two more of the same shape, both found by all three**: `computedThroughTheDuplicate` was
  dead — declared, never run, sitting directly above the comment claiming the control matched it,
  which is what made the false sentence read true; and the arbitrary-row read was not removed at
  r26 but MOVED to the control, which returns two non-NULL structs while the assertion names one.
  Both texts are read by one helper now, which requires exactly ONE row carrying both columns and
  fails if a shape ever yields more.

## Folds at r28

- **The control changed the leg's topology too, and only prose said that was inert** (all three
  reviewers, each measuring it): removing the repeated `ID` requires renaming `c_md`'s column,
  and the dialect cannot rename a base column in place, so the control also wraps that leg in a
  derived table — and derived-table projections are descriptor-relevant, which is what the tests
  above this one pin. Two variables, not one. Each reviewer measured the missing arm and got the
  same answer; it is committed now as a third read that keeps the wrapper and restores the repeat
  (`SELECT id AS id` beside the control's `SELECT id AS cid`, so the only textual difference is
  the alias) and still comes back a raw map. Measured red by removing the repeat from it. The
  booking says the wrapper is forced and why the third read is what makes the attribution sound.

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
- Two reads of a gathered multi-source unnest star body (`SELECT * FROM a, b, a.arr AS x`),
  both pre-existing at the merge-base in the same form: an aggregate over the DERIVED spelling
  (`… FROM (…) d GROUP BY d.aid`) is refused at execution as an undeclared binding of `D`, and
  a WHERE over the CTE spelling (`WITH d AS (…) SELECT d.aid FROM d WHERE d.aid = 1`) does not
  plan. The CTE spelling stays out of the global scope by shape (§ Fix F) and an aggregate over
  it answers through the translator's seed bake; the derived spelling has no such arm, and the
  translator already books the projected-output-layout ordinalization it needs
  (`translateAggregate`, the positional-gather comment). A projection or a WHERE over the
  derived spelling answers, as before. Recorded in `TODO.md` ("Exact quantifier binding over a
  CTE or derived body") with the measurements. Two more of the same class, over row-versioned
  tables and pre-existing at the merge-base in the same form: a WHERE over the derived star
  join (`edge lookup D: read as RECORD(ID,Y,ID,Z), declared RECORD(AA.ID,Y,BB.ID,Z)` — the
  row-version rewrite has already produced Java's explicit projection, and that projection
  names its slots by the leg-qualified datum key while the scope's walk publishes bare names),
  and a star over a lateral unnest (the rewritten projection carries the outer column beside
  the element, as the top-level star does, while the derived unnest scope shadows it). Both
  are in the same `TODO.md` entry with their measurements.
- A sort over a derived table's or CTE's column is still never answered by an index, nor a DESC
  over its primary key by a reverse scan: the constraint now crosses the projection (§ Third
  adjacent finding) but the scan group's leaf match never climbs to the candidate's sort
  (`correlatedToEquals`), so the data-access rule has no matched ordering parts to satisfy it
  with. `TODO.md`, "Ordering through a projection reaches the
  child group but not the index", with the measurement and the Java mechanism that closes it.
- A struct member declared with a dot in its name labels as `b`, not `a.b`, over the base table
  and through a derived table alike: RFC-238 §2's `qualifierStrippedLabel` residual, pinned on
  the derived shape this RFC admits (`TestFDB_QuotedDotNestedMemberLabel`).
- An array literal with a NULL element (`[x.id, NULL]`, Java's `maximumType(LONG, NULL)`: a
  NULLABLE element) cannot be published by the exact derivation, because `semantic.Column`
  carries the array container's nullability and no element bit; a join-bodied CTE projecting
  one has no publisher and every read of it hits the loud floor (0AF00), identical at the
  merge-base. It is the specimen the loud-floor pins use now that a nominal record publishes
  (`TestOrderByExactMetadata_UnderivableCTEComputedProjectionStaysLoud`). RFC-232's bridge
  residual; `TODO.md`, "An array literal with a NULL element cannot be read through a CTE or
  derived table", with the two closures.
- A table with a fieldless nested-message column (Java-authored metadata; this DDL cannot
  declare one) cannot be queried at all — `SELECT t.sk FROM t` is 42703 with the column `e`
  present and plans without it — because `expr.structColumnType` turns a fieldless record into
  UNKNOWN and the flowed row then resolves nothing. Identical at the merge-base; surfaced by the
  r15 measurement of an exact-derivation decline beside a nested path. `TODO.md`, "A table with
  a fieldless nested-message column cannot be queried at all", with the reproducer and the
  closure.
- An enum field is typed STRING by the exact derivation (`sqlTypeToCascadesType("ENUM")`), one
  layer before RFC-232's carrier gap; `TODO.md`, "The exact derivation types an enum field as
  STRING", pointing at the pin that goes red when it closes.
- The nightlies that are red for a runner-host reason (the FDB container disappearing about
  thirty minutes into every Docker-backed job, the factory batch SIGKILLed, the coverage job
  cancelled from outside after 3–67 minutes with no timeout annotation) need host access; the
  one repository-editable cause — the bot pin-bump PR carrying no checks — is fixed in the same
  pull request as a workflow edit, and the host cause is escalated to the owner as a STOP, not
  filed. A draft of this RFC also blamed the coverage lane's job timeout; Torvalds's delta lap
  measured the run durations and refuted it, and the cap is back at its value with the
  measurement in its comment. The `TODO.md` entry records which is which.
