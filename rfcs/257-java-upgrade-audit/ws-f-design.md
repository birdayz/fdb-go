# RFC-257 WS-F design — planner scheduling, explicit readers and planner properties

Status: design v4, for Graefe + Torvalds + storage review. Nothing in this design is
implemented. The only code it adds is the measuring instrument of section 0: the oracle
spec with its access-path reducer and Go record-layer rows, the target steps it calls,
and the Go test-harness runner, whose sequence method now records each execution's
plan-cache event.

v4 answers the three v3 NAKs (`ws-f-design-review-v3/`):
- **The crossing** (all three gates). It is decided: a crossing with no final is Java's
  checked error, never carried, and the RFC-182 shape and WS-E's union-leg probe are named
  census members (2.3, 2.6).
- **The memoizer** (4.3 item 3). It is named, `ImplementationRuleCall.
  MemoizeFinalExpressionsFromOther`, which carries the ordering constraint and is marked
  explored under D7. The extraction check is an invariant whose failure is an error.
- **4.3 item 2** is stated in Java's terms, with its carrier named: a `pastRecordTypeHorizon`
  mark set in `ComputeMatchedOrderingParts` and read only by the in-union satisfaction
  check.
- **The acceptance** can see plans. Every EXPLAIN row is accepted by access path
  (SAME-PATH, SAME-CLASS, or DIFF-PATH with a declared reason and Go's path), with a
  per-phase owner (13).
- **Section 12** reasons by the new plan's outermost decoder. The prefix plan is
  withdrawn, and F-7a and the new F-8 have rows.
- **F-7a** is framed as feeding Go's rule-internal pre-selections, which F-8 replaces with
  the target's per-partition yields (4.2 item 2).
- **Go record-layer rows** exist for the five `w8_rl_*` shapes. They measured that Go's
  IN rule matches a logical filter where the target's matches a select (4.3 item 9).
- **The in-union's size** is the product of its sources. Go nests one-source in-unions
  (4.3 item 10).
- **The sparse `?`.** v3's "a `?` is ONE cache key" is refuted: Go substitutes bindings
  into the text, and the implication rule follows what the planner sees (5 item 3).
- **The in-join rows** have a measured cause: covering emission, F-6 (4.3 item 3).
- **A quoted primary key** without a dot is found to be case-folded by the primary
  candidate (9.2).
- **The error arm.** The record-core arm `translateFDBError` lacks is decided (7).

The v3 paragraph follows.

v3 answers the three v2
NAKs (`ws-f-design-review-v2/`), each of whose central findings was a measured claim that
the instrument could not support; the instrument now measures what each claim rests on.
The in-union rule's view is dumped on both engines: the two rules compute the same union
ordering, and the ORDER BY id difference is upstream, in the data-access match's
record-type position (4.3), which also moves the IN-list decision (the partition memoized
whole, a leg read as the target's match reads it, Go's match extension kept and
declared). The target's cost relation over the root members is measured under three name
sets: it is a cycle on the covering class (4.2). The target's REWRITING survivor is
measured for LEFT JOINs, and `outerJoinCount` is decided (2.3), with a census arm of its
own (2.6). The target's record layer is measured under its default configuration (the
in-union fails at size 0, 4.3). The sparse index's prepared form and a same-connection
binding sequence are measured (5). The configuration reaches the rule-level comparators
with its metadata, as its own step F-7a (4.2); F-7 is split so each step has one cause
(12). Section 12's continuations are rewritten around where the exposure really is, with
a transition table. Acceptance is code in the oracle, with a ratchet (13). D7 marks every
restricted reference explored, as Java does (2.4). The v2 paragraph follows; its
IN-list cause, its "the rule's in-union over COVERING wins", its EXPLAIN INSERT failure
and its "plan identities never change" are superseded as above.

v2 answered the three v1 NAKs
(`ws-f-design-review-v1/`). The oracle pinned BOTH engines (70 probes, section 0)
and separated Go setup failures from probe answers, which corrected two v1 claims (Go
reads and explains a quoted dotted table; only its DML fails). The planner rule trace
showed the target's in-union rule never builds an index in-union ordered by the primary
key, its only in-union there loses by its own cost model, and an IN list longer than 24
fails at execution in the target (section 4.3). The relational configuration's index
preference was measured against every plan class it meets (section 4.2), W6's
REWRITING comparator and plannability exit rule were decided (section 2.6), the phases
that change plans were reclassified, and every stale-text sweep was case-insensitive
with its own control. Umbrella
section: `rfcs/257-java-4.14.2.0-upgrade.md` "WS-F — Planner scheduling, explicit
readers and properties"; audit: `rfcs/257-java-upgrade-audit/cascades.md` W6–W15.
Java reference: `fdb-record-layer/` at tag 4.14.2.0 (`fdacd162a`). The previous pin is
4.12.11.0 (`rfcs/257-java-4.14.2.0-upgrade.md:22`); every "what changed" claim below is
against that tag, not 4.11.1.0 as the three research prompts wrongly said.

Labels. MEASURED means a live 4.14.2.0 JVM answer from the WS-F oracle (section 0) or
a `git` command whose output is quoted. SOURCE means a file:line read during this
design, cited. HYPOTHESIS means neither, and each one names the step that measures it.
The three research reports (`ws-f-research/{scheduling,readers,properties}.txt`) were
written without `git`; every one of their claims this design relies on and that a
`git` diff or a run could settle was re-checked here, and the ones re-checked are
listed in section 0.3.

This workstream changes the Cascades planner, the cost model's configuration and the
executor, so it needs a Graefe and Torvalds ACK on this design before implementation,
and one joint review lap per phase of section 12 (CLAUDE.md, owner ruling 2026-07-18).

## 0. Evidence

### 0.1 The WS-F oracle

`conformance/ws_f_probe_conformance_test.go`, with `ws_f_access_path_test.go` (the
access-path reducer) and `ws_f_record_layer_go_test.go` (the Go record-layer rows), all
three in `//conformance:rfc257_oracle_test`'s `srcs` with a `gazelle:exclude` like the
other RFC-257 oracles. One Ginkgo spec,
bounded by a 20-minute context, runs 155 probes (v3: 130) against the live JVM and the Go driver
on one FDB cluster: plain statements (schema and setup fresh per probe; rows of a probe
whose SQL leaves the order open are compared sorted), prepared statements (the target
binds through its JDBC setters via `runPreparedExtended`, Go through database/sql), one
statement text executed with several bindings on ONE connection (a new target step,
`runPreparedSequence`, and a new Go runner method, `RunPreparedSequenceWithSetup`), one
INSERT through each engine's update path with a read-back, target planner traces in four
modes (below), and five RecordQuery probes on the target's RECORD layer (a new target
step, `wsfRecordLayerIn`, planning through its `CascadesPlanner` under a chosen
configuration) with, from v4, their Go rows: `wsfGoRecordLayerIn` builds the target's
query graph (a type filter over the full scan, a select holding the IN comparison, a sort),
plans it the way a Go record-layer caller must (the exported `NewPlanner`, match
candidates from an index definition derived from the meta-data as the SQL layer derives
its own, no statistics, `DefaultPlannerConfiguration` or the relational one), and
executes it with `executor.ExecutePlan` over the same five orders in its own subspace. A
second spec drives the acceptance check's verdict arms without the JVM, and checks that
every pinned plan of either engine reduces to known operators.

v4's instrument changes, each answering a v3 gate finding:
- **Access paths.** `wsfAccessPath` reduces either engine's EXPLAIN to one access-path
  tree: leaves with their relation or index, their bound comparison kinds (`=`, `≡` for
  NOT_DISTINCT_FROM, `<>` for a range) and covering or not; FETCH, FILTER, SORT, IN-join,
  IN-union, union, flat-map, join and default-on-empty above them. Projections, IN
  values, predicate text and names are dropped. Every EXPLAIN row both engines answer
  with a plan is accepted by path, never by "DIFF" (section 13).
- **Plan cache.** The Go sequence runner records each execution's plan-cache event
  through a plan logger on the connection. Each Go element of a sequence carries its
  event, and the event is pinned. The target's step has no plan cache (its engine is
  built with `planCache = null`, `sql_plan_steps.java`), so its elements are
  per-binding plans. v3's "same-connection MEASURED" wording claimed a reuse the target
  cannot show. A prepare failure is a fixture error with phase `prepare`, never an
  execution's answer.
- **ROOT-PAIRS.** It also records the member count with the index-scan preference in
  force, and the memo order `OptimizeGroup` iterates with the winner its sequential pass
  picks with the target's model (`CascadesPlanner.java:650-658`). In all 27 root rows
  that winner is the plan the target's EXPLAIN shows, and each row asserts it: the two
  renderings compared once their quantifier names are erased (`wsfPlanShape`). The trace
  is refused, and the spec fails, on any of these: an unreadable line, two members that
  render the same, a memo order or winner naming no listed member, or a member count the
  trace does not list. The verdict-arm spec drives each of these refusals.
- **REWRITING-RESULT.** It renders each select's predicates, sorted, with aliases
  masked, not only their count. Both answers are pinned: `wsfPins` (target) and
`wsfGoPins` (Go at this tree); the spec fails if a probe lacks either pin, a pin lacks a
probe, or any answer moves. A Go failure in a SETUP statement renders as
`SETUP-ERROR <phase>`, never as the probe's answer. Messages are kept whole up to 400
runes. Each probe also prints SAME or DIFF from a comparator that ignores only the
target's exception class (the one field Go has no counterpart for).

The rule trace is a target step, `planRuleTrace` (`conformance/sql_plan_steps.java`):
it EXPLAINs a query with a thread-local `PlannerEventListeners` listener that counts,
per named rule, the ended rule calls and their final and exploratory yields, and, for
each final an `ImplementInUnionRule` call yielded, ranks it at the end of planning
against every other final member of its reference with the target's own
`PlanningCostModel` under the target's own configuration. v3 adds three modes, each
switched on by a pseudo-rule name so no existing caller sees them: IN-UNION-PARTITIONS
(the inner reference each in-union call read, its ordering partitions and the union
ordering and satisfying keys per request, replaying `onMatch`), ROOT-PAIRS (the root
reference's final members at its last PLANNING OptimizeGroup, each with its residual,
type-filter and unmatched-field counts and plan hash, and the model's verdict on every
pair, run under three table and index name sets so a hash-decided pair shows), and
REWRITING-RESULT (the query graph REWRITING hands to PLANNING).

The acceptance of section 13 is in the spec as data (`wsfAcceptance`, `wsfOpenUntil`)
and asserted.

Evidence (v4): `ws-f-oracle/evidence-run.txt`. Two uncached runs on one tree, bound by
the tree's hash before and after each, with identical extracted lines (155 probes). The
plandiff and docscheck targets ran on the same tree. Thirteen instrument mutations, each
exiting 3:
- both pins;
- the reducer's fetch;
- an EXPLAIN row accepted as DIFF;
- the ROOT-PAIRS reader's refusal and its winner;
- the plan-shape comparison;
- the cache marker;
- both sides of the ratchet;
- the class, DIFF-PATH and record-layer comparisons.

The ratchet runs over explicit tables (`wsfRatchetGaps`), and the verdict-arm spec drives
each of its arms. Each ROOT-PAIRS row asserts that its sequential winner is the plan the
target explains.

| Group | What the target does (MEASURED) | Go at this tree (MEASURED) |
|---|---|---|
| W9, `T1` with I1(col1), I2(col2) | NOT DISTINCT FROM NULL/literal → `ISCAN(I2 [NOT_DISTINCT_FROM NULL])`, `ISCAN(I1 [NOT_DISTINCT_FROM …])`; IS DISTINCT FROM → covering I1 full range, residual, FETCH; the conjunction binds NOT_DISTINCT_FROM and leaves `> 5` residual | same rows; plans are a primary scan plus filter, except the conjunction (`IndexScan(I1, [<>] COVERING)` plus filter) |
| W9, `T2` with I3(col1, col2) | `col1 = 10 AND col2 IS NOT DISTINCT FROM NULL` → `ISCAN(I3 [EQUALS …, NOT_DISTINCT_FROM NULL])`; with IS DISTINCT FROM 1 the second column stays residual over `COVERING(I3 [EQUALS …])` | same rows; `IndexScan(I3, [=, *] COVERING)` plus filter in both |
| W9, `T3` with sparse I4 `WHERE col2 IS NOT NULL` | NOT DISTINCT FROM NULL does NOT use I4 (`SCAN | FILTER`); NOT DISTINCT FROM 3 does (`ISCAN(I4 [NOT_DISTINCT_FROM …])`); a bound NULL parameter returns the null rows | same rows; primary scan plus filter |
| W8, relational configuration, `T1` | `SELECT *` with no predicate → `ISCAN(I1 <,>)`; `SELECT col1` → `COVERING(I1 <,>)`; `col1 <> 10` → covering I1 + residual (+ FETCH for `*`); `col1 = 10 OR col2 = 3` → union of two coverings compared by ID, then FETCH | `Scan(T1)` or a primary scan plus filter for all of them; same rows |
| W8, IN lists, `T1` | `IN (10, 20) ORDER BY id` → `SCAN | FILTER … IN …`; ORDER BY col1 → `INUNION` over `COVERING(I1 [EQUALS q0])`, FETCH; no ORDER BY → `INJOIN` over `ISCAN(I1 [EQUALS q0])`; 25 literals ORDER BY col1 → XXXXX RecordCoreException "too many IN values" (24 literals: rows); a bound 25-element array likewise | `InUnion(IndexScan(I1, [=]))` for both orders, `Fetch(InJoin(IndexScan(I1, [=] COVERING)))` without; 25 literals return rows; a bound `[]int64` is refused by Go's driver ("unsupported type []int64") |
| W8, rule trace | ORDER BY id: `ImplementInUnionRule` yields ONE final, an in-union over `SCAN | FILTER col1 = q` compared by (ID, COL1); against `FILTER(SCAN)` and the FLATMAP the model ranks it worse (compare = +1). ORDER BY col1: its only final is `INUNION{ISCAN(I1 [EQUALS q])}`, which LOSES (compare = +1) to `FETCH(INUNION{COVERING(I1 [EQUALS q])})`, built by the fetch push-through over the partition's covering member, and to the sorted in-join (v2 said the rule's in-union over COVERING wins; it is the push-through's). No ORDER BY: no in-union; `ImplementInJoinRule` yields the winner | (no Go instrument; the W6 rule-call observer is its counterpart) |
| W10 enums | as v1: ten rows and plans, an unknown literal is XX000 | 0A000 "enum types … are not yet supported" |
| W13 subscripts | as v1, plus `arr[?]` bound INT → 8, bound LONG → XXXXX ClassCastException; `arr[id]` over an empty result → no rows, no error | 0AF00 for every form |
| W13 quoted dotted table | EXPLAIN SELECT → `SCAN([IS foo.table$nested, …])`; INSERT → stored and read back | EXPLAIN SELECT → `PredicatesFilter(Scan(foo.table$nested), …)`; SELECT works; EXPLAIN INSERT → `Insert(foo.table$nested)`; executing the INSERT (as probe and as setup) → 42F00 "Unknown database foo" |
| W8, IN plans over T5 (the tie-breaking rows) | ORDER BY col1 → `[IN … SORTED] | INJOIN q0 -> { COVERING(I5 [EQUALS q0]) | MAP … }`, rows `[2 10] [5 10] [1 20] [3 20]`; DESC → the in-join SORTED DESC, rows `[1 20] [3 20] [2 10] [5 10]`; ORDER BY id → `SCAN | FLATMAP { EXPLODE … | FILTER }` | ORDER BY col1 and id → `InUnion(IndexScan(I5, [=]))`, same rows; DESC → `InMemorySort(… DESC, Fetch(InJoin(…)))`, rows `[3 20] [1 20] [5 10] [2 10]` (the ties in the other order) |
| W8, the in-union rule's view (`w8_iup_*`) | ORDER BY col1 / none: the inner reference holds the I1 plans, union ordering `(COL1 CHOOSE, ID)`, keys `(COL1, ID)`; ORDER BY id: the inner reference holds only `SCAN | FILTER COL1 EQUALS q` | same partitions and keys for ORDER BY col1 (scratch instrument); ORDER BY id: the I1 partition too, keys `(ID, COL1)` (section 4.3) |
| W8, `col1 = 10 ORDER BY id` | `SCAN([IS T1]) | FILTER …`, its only root member | `IndexScan(I1, [=])` |
| W8, root pairs under three name sets | `SELECT col1`: COVERING < ISCAN < SCAN < COVERING, a cycle, the (primary, covering) pair decided by unmatched fields, not the hash (section 4.2) | (no Go instrument) |
| W8, the record layer's default configuration | `price IN (10, 20)` sorted by the primary key → INUNION over the price index, EXECUTE fails "too many IN values" at size 0; sorted by price, unsorted, one value → INJOIN, rows | v4, the same graph through Go's exported planner: no in-join and no in-union in any row; sorted by price (and one value) → `Fetch(PredicatesFilter(IndexScan(wsf_price, [*] COVERING)))`, by the primary key → `InMemorySort(PredicatesFilter(TypeFilter(Scan(Order))))`, unsorted → the filtered scan; every row returns its ids, the primary-key sort included (section 4.3 item 9) |
| W8, two IN lists over I3(col1, col2) (`w8_in5x5_*`, `w8_in4x6_*`) | ONE in-union over both sources (`[IN … ⋈ IN …] INUNION q0, q1`); 5×5 fails "too many IN values" (the product, 25, over 24), 4×6 returns rows | two NESTED one-source in-unions, `InUnion(InUnion(IndexScan(I3, [=, =])))`; both return rows (section 4.3 item 10) |
| W8, an index naming id (`w8_explicit_id_*`, I8(col1, id)) | `IN (10, 20) ORDER BY id` → in-union over `COVERING(I8 [EQUALS q0])` COMPARE BY `(_.ID, _.COL1)`, FETCH: the I4 shape is admitted | `InUnion(IndexScan(I8, [=, *]))`, same rows |
| W8, IN beside another index's equality (`w8_intersection_*`) | `col1 IN (10, 20) AND col2 = 3 ORDER BY id` → `SCAN | FILTER`, no intersection; the in-union rule's reference holds one final | `PredicatesFilter(IndexScan(I2, [=]))`, ordered by id past the record-type position (the item 1 extension) |
| W8, the in-join root pairs (`w8_root_in_*`) | no ORDER BY: nine members, none a fetch over an in-join over a covering scan; the winner is `INJOIN { ISCAN(I1 [EQUALS q]) }`. ORDER BY col1 DESC: the winner is the SORTED DESC in-join over the covering scan, no fetch | `Fetch(InJoin(IndexScan(I1, [=] COVERING)))`; `InMemorySort(DESC, Fetch(InJoin(…)))` (section 4.2 item 3) |
| W13, a quoted primary key without a dot (`w13_quoted_pk_*`) | `SCAN([IS footab, EQUALS …])` | `PredicatesFilter(Scan(footab))`: no primary-key scan for a quoted key column, dot or not (section 9.2) |
| W9, `CAST` literals over the sparse I4 | `CAST(NULL AS BIGINT)` → `SCAN | FILTER`; `CAST(3 AS BIGINT)` → `ISCAN(I4 [NOT_DISTINCT_FROM CAST(…)])` | scan plus filter for both; same rows |
| W8, IN-subquery | 0AF00 "IN predicate does not support nested SELECT" | 0AF00 "Cascades planner could not plan query" |
| W6, LEFT JOIN | REWRITING survivor: the rewritten null-on-empty form; indexed ON column → `ISCAN(I1) | FLATMAP { ISCAN(I6 [EQUALS q0.COL1]) | ON EMPTY NULL }`; ON naming only the preserved side → the same FLATMAP over `COVERING(I6) | FILTER | FETCH` | `FlatMap(outer=Scan(T1), inner=DefaultOnEmpty(IndexScan(I6, [=])))`; `NestedLoopJoin(LEFT OUTER, …)` (RFC-152); same rows |
| W9, sparse `?` | `EXPLAIN … FROM ?` bound 3 → `COVERING(I4 [NOT_DISTINCT_FROM …])`; one statement, one connection, 3 then NULL (and the reverse, re-bound or re-prepared) → the right rows each time, each execution planned fresh (no plan cache) | scan plus filter; the right rows each time; every execution after a DIFFERENT binding is a plan-cache MISS, and the same binding twice is a HIT (`w9_sparse_param_value_then_value_reused`): Go's cache keys a `?` statement on its substituted values (section 5 item 3) |

### 0.2 What the oracle refutes

- **The umbrella's "preserve existing null-safe singleton scans" and the audit's
  "source-confirmed existing support" (cascades.md W9) are false at the planner**
  (section 5).
- **Go's SQL planner does not run the target's SQL planner configuration**, and the
  target's configuration reaches every plan class above: unfiltered scans become index
  scans, projections become covering scans, disjunctions become index unions
  (section 4.2).
- **The v1 premise of section 4.3 was wrong**: the configured in-union size does not
  decide plan generation in either engine. **v2's cause was wrong too**: the two
  in-union rules compute the same union ordering from the same partitions; for ORDER BY
  id the target's inner reference never holds the I1 access, because its match ordering
  stops at the record-type position of a relational table's key, and Go's does not
  (section 4.3).
- **The target's cost relation is cyclic on the covering class**: COVERING < ISCAN <
  SCAN < COVERING for `SELECT col1`, with the (primary, covering) pair decided by the
  unmatched-field rung, not the hash (section 4.2).
- **The target's record layer fails an in-union under its own default configuration**
  (size 0), so the size check is a behaviour change for Go record-layer callers
  (section 4.3).
- **Go has no SQL array subscript** (section 9.1).
- **v1 over-claimed the dotted-name failure, and v2 still did**: Go resolves the quoted
  name on its scan path, and EXPLAIN INSERT works; only EXECUTING a DML statement (and
  the other string-splitting sites) fails (section 9.2).
- **Go has no enum DDL** (section 6).
- **v3's "a `?` is ONE cache key for every binding" is false at this tree**: Go
  substitutes a statement's bindings into its text, so the cache keys each distinct
  binding apart (section 5 item 3).
- **Go's IN machinery does not accept the target's graph**: its IN-to-explode rule
  matches a `LogicalFilterExpression` where the target's matches a `SelectExpression`, so
  a record-layer caller that builds the target's graph gets no IN plan at all
  (section 4.3 item 9).
- **The in-union size is a product in the target, and Go nests**: Go builds one
  one-source in-union per IN list, so no per-in-union count sees the product
  (section 4.3 item 10).
- **v3's intersection hypothesis does not arise for the probed query**: the target
  builds no intersection there (section 4.3 item 2).

### 0.3 Research claims re-checked for this design

- `git diff 4.12.11.0 4.14.2.0` of `ScanComparisons.java` and `RangeConstraints.java`:
  NOT_DISTINCT_FROM joins the EQUALITY arm, IS_DISTINCT_FROM is written into the NONE
  arm, NOT_DISTINCT_FROM becomes a `Range.singleton` and an allowed compile-time type.
  Upstream commit `7cefc75de` (#4598) also records that without the change a leading
  NOT_DISTINCT_FROM under an unsatisfiable ORDER BY raised UnableToPlanException.
- W15: `git show --stat 5cec45354` lists the SpotBugs removal's 56 files; the 24 main
  query/plan files among them change only imports, annotations and comments, except a
  `var` typing change in `PlannerEventStatsCollectorState`. No execution port.
- The relational planner configuration sets `PREFER_INDEX` and an in-join-as-union
  size of 24 at 4.14.2.0 (`PlannerConfiguration.java:158-161`) and already did at
  4.12.11.0 (`git show 4.12.11.0:…/PlannerConfiguration.java`, line 152).
- `visitSubscriptExpression` exists at 4.12.11.0 (`ExpressionVisitor.java:703` there,
  `:796-801` at the target), so the subscript surface is pre-existing Java function,
  and #4302 (`f4bc3c33a`) changed only the closing bracket of its explain.
- The scheduling report's Go facts that this design builds on were re-read:
  `Task.Run(ctx, p)` returns nothing (`planner.go`, the `Task` interface);
  `FinalizeExpressionsRule.OnMatch` yields the bound expression itself
  (`rule_finalize_expressions.go:40-45`); `SelectMergeRule` is registered in the
  default expression rules and `PredicatePushDownRule` in `RewritingRules()`
  (`default_rules.go`); `PredicatePushDownRule` returns after one pushed quantifier
  ("One quantifier per rule firing", `rule_predicate_push_down.go:171`).

## 1. Decisions at a glance

| Item | Decision | Section |
|---|---|---|
| W6 conditional chains, progress, staleness, pruned-input scheduling, disentangled finalization, merge/pushdown | port Java's mechanism in full, including the physical REWRITING prune under Go's REWRITING comparator (decided), every restricted reference marked explored as Java's are; `outerJoinCount` stays omitted with its PLANNING re-derivation (measured, decided); the prune becomes the default only through the plannability gate's split exit rule, whose census has a preserved-divergence arm | 2 |
| W7 explicit index-entry readers | raw KEY/VALUE binding, helper, extraction rules, aggregate reader active, covering-Value plan built as the target ships it, old covering plan decoded by the reader and emitted under Java's gate | 3 |
| W8 vector-engine preference | NONE default, Java's rung at Java's priority, candidate/plan engine identity; SQL proof after WS-D | 4.1 |
| W8 relational planner configuration (found here) | Go's SQL planner builds the target's relational configuration (PREFER_INDEX, in-union size 24, fetch method carried and declared, rewriting-disable option, right-deep, vector preference); the rule-level comparators get the planner's configuration AND metadata (F-7a); Go's rank-based primary-vs-index rung is kept against the target's measured cyclic relation, the pairwise (primary, covering) verdict a declared divergence (F-7c) | 4.2 |
| IN-list plans (found here) | the union-ordering arithmetic already agrees (measured); the partition is memoized whole; the in-union reads its legs as the target's match does at the record-type position, while Go's match ordering past that position stays a declared read extension; the unordered arm and the size-less constructors are deleted; the "too many IN values" check runs first at execution, counting the execution's values (F-7b) | 4.3 |
| W9 null-safe scans | the index-match gate admits NOT_DISTINCT_FROM; IS DISTINCT FROM stays residual; a sparse index is implied by a non-null LITERAL comparand, never by a `?` (Go's cache has no plan constraint; WS-H closes it); the range builder ports Java's compile-time test with all seven range-matchable kinds | 5 |
| W10 enum comparisons | pins land with WS-J F6 (enum DDL); the index-predicate half is WS-J F10 | 6 |
| W11 snapshot DML | implemented once, in WS-E section 6.6, with the error type (and its XXXXX SQLSTATE) decided here | 7 |
| W12 zero-based EXPLODE ordinality | port the flag, validation, identity, distinctness (a plan-changing step) | 8 |
| W13 subscript | port the SQL subscript expression (Java's function, typing and evaluation) | 9.1 |
| W13 quoted dotted names (found here) | typed identifier segments at every site that re-splits or case-folds a joined name; the string splitter is deleted | 9.2 |
| W13 display names | decode stored names in the five Explain renderers once RFC-238 section 7c stores them | 9.3 |
| W14 bottom-up fold | a `FoldValue` helper, landed with its first WS-J consumer | 10 |
| W15 | nothing to port (0.3) | 11 |
| Continuations across plan changes | the exposure is the record-layer API (SQL continuations are engine-private); a record-layer continuation is valid only for its plan, in both engines; per step, each transition is refused (salted leaf) or undetected (unsalted primary scan, declared parity), pinned both ways | 12 |

## 2. W6 — conditional rule chains and rewrites after child optimization

### 2.1 Target (SOURCE, scheduling report sections 2.1–2.5, spot-checked)

- `ConditionalCascadesRule` (`ConditionalCascadesRule.java:88-204`) wraps an ordered
  list of rules that share a root class, a root operator and `onlyOnPrunedInputs`;
  its constraint dependencies are the union of its members'; `onMatch` on the wrapper
  throws. Enablement keys are the simple class names
  `ConditionalExplorationCascadesRule` / `ConditionalImplementationCascadesRule`.
- `Task.execute()` returns progress (`CascadesPlanner.java:551-562`). A rule call makes
  progress iff it created a partial match, a memo-admitted final or exploratory
  expression, a constraint push that changed a child for a non-preorder rule (which
  re-queues the rule), or a constraint change on a child explored before (which
  queues its ExploreGroup) (`:1102-1158`). A deduplicated yield, a match without a
  yield and a preorder push onto a never-explored child are not progress.
- A chain member that makes no progress queues its successor; one that makes progress
  stops the chain; a stale expression (the group no longer contains it) ends the chain
  with no follow-up (`:1233-1248`).
- Staleness at push time: a re-exploration queues a rule only if the group is on its
  first exploration or one of the rule's dependencies was pushed after the group last
  committed exploration; forced explorations queue every rule (`:956-970`,
  `:1388-1391`); a wrapper is filtered first by its own name and union dependencies,
  then each member (`:839-862`).
- Ordering: `OptimizeInputs` queues pruned-input rules first and one `OptimizeGroup`
  per child on top, so every child is pruned before the chain runs (`:1371-1386`).
- REWRITING (`RewritingRuleSet.java:50-107`): exploration = {decorrelate→simplify
  chain, RewriteOuterJoinRule}; implementation = {merge→pushdown chain,
  FinalizeExpressionsRule}. `FinalizeExpressionsRule` yields one final per element of
  the cross product of the children's `SelectMergeable` partitions, over fresh
  references memoized from those partitions and marked explored. `SelectMergeRule`
  and `PredicatePushDownRule` run on those finals after the children are pruned to one
  member, and pushdown handles every eligible leg in one yield, keeping a residual set
  from which a predicate leaves only after a successful push.

### 2.2 Go today (SOURCE)

Tasks return nothing; every rule re-fires on every re-exploration round; there is no
rule dependency declaration, no conditional type, no pruned-input class; merge and
pushdown are independent exploration rules that scan every member of a child;
finalization promotes the same pointer; the REWRITING stage boundary deliberately does
not prune (`unified_tasks.go`, ExploreGroupTask's stage-boundary comment), and the
RFC-186 designated final (`designated_final.go`) makes costing see a one-final world
while the memo keeps every alternative (DIVERGENCES.md "REWRITING prune: virtual
(designation) vs Java's physical prune").

### 2.3 The coupling that decides the design

Disentangled finalization is Java's physical REWRITING prune: every final ranges over
fresh child copies that `OptimizeInputs` prunes to one member. Go tried a universal
prune and reverted it because PLANNING lost the only implementable form of the RFC-153
buried-leg shape and the cross-join-EXISTS shape. So W6 cannot land by porting the
rules alone.

Decision: port Java's mechanism in full, and make PLANNING re-derivation parity a
gating step of this workstream (step W6-7 below). The prune becomes the default only
when the census has no lost plan and no unaccepted change (the split exit rule of
2.6); the designated-final mechanism is then deleted, and
Java's `Verify(finalMembers.size()==1)` becomes a checked invariant that returns an
error (design principle 4), which closes the DIVERGENCES entry.

The invariant covers EVERY crossing, the one with no finals included (v4; v3 left it
open, and all three v3 gates found it). Go has two crossing arms today
(`unified_tasks.go:74-109`): a reference with finals goes through
`AdvancePlannerStage` (Java's `advancePlannerStage`, which verifies exactly one final,
`Reference.java:208-211`, called from `CascadesPlanner.java:745-749`), and a reference
with NONE goes through `AdvanceStagePreservingMembers`, which carries every exploratory
member into PLANNING. Java has no second arm: a reference that crosses with no final
fails its `Verify`, so no Java plan is ever built from an unfinalized group. The second
arm is the RFC-182 fix (`asymmetric_union_planning_test.go`): under Go's current
scheduling, a union leg reached through a merged group was never finalized in
REWRITING, crossed with no finals, and kept its REWRITING exploration stamp.
Decision: with the prune, a crossing with no final is the same checked error as one with
two, and `AdvanceStagePreservingMembers` is deleted with the designated-final mechanism
when the option becomes the default. It is not given the prune's one-member form.
Under the ported scheduling every group REWRITING reaches is finalized bottom-up: a
leaf yields itself (D7), a root yields when each child has a partition, and a new
final's children are restricted final references (D10, D11) whose copies are marked
explored (D7). So a group with no final at the crossing is a group REWRITING did not
reach, a scheduling defect to fix, not a state to carry. Pruning it to one member
instead would choose among members no REWRITING rule ran on, which is the carried
state under another name. Until the default flips (option off) the second arm stays as it is. With
the option on, a crossing with no final returns the error, and the census counts it as
a lost plan (2.6 step 7). The census names two queries that reach the arm today:
`TestAsymmetricUnion_BothLegsPlan` (the RFC-182 shape) and WS-E's
`union_leg_type_annulling_fold_where` probe. The WS-E probe's leg must reach PLANNING
as ONE final, the fold, which wins on the conjunct rung. So the probe is also WS-E's
test that the unfolded member, whose primary-key probe raises 22012, never crosses
(ws-e-design.md section 8 cites this paragraph).

The physical prune makes the REWRITING comparator decide which single tree PLANNING
ever sees, so the comparator is part of this design. Decision: the prune's comparator
is Go's `RewritingCostModelLess` as it stands, with two changes. First, it stops being
built on the designation scope (`newDesignationScope`, which step 7 removes): its
derived properties read each child through the one final `Verify(==1)` guarantees, as
Java's `ExpressionCountProperty.forReference` does. Second, the predicate-count-by-level
producer becomes dense, as Java's is, so the highest-level tie-break reads the tree
depth Java's `getHighestLevel` reads, not the highest predicate level; this removes the
residual divergence recorded at `comparePredicateCountByLevel` ("Finding
6-followup"), which that comment says flips REWRITING survivors and which therefore
lands with the census.

`outerJoinCount`, Java's first REWRITING criterion, decided (v2 left the interaction
with the physical prune open). MEASURED with the trace's REWRITING-RESULT mode (the query
graph REWRITING hands to PLANNING, rendered at the first PLANNING event;
`w6_rewriting_*`): for both probe LEFT JOINs the target's REWRITING survivor is the
REWRITTEN form, `SelectExpression[preds=0](forEach: T1, forEachNullOnEmpty:
SelectExpression[preds=1](T6))`, and its plans are `ISCAN(I1 <,>) | FLATMAP q0 -> {
ISCAN(I6 [EQUALS q0.COL1]) | ON EMPTY NULL ...}` for the indexed ON column and the same
FLATMAP over `COVERING(I6) | FILTER q0.COL2 EQUALS ... | FETCH` for the ON predicate that
names only the preserved side (`w6_left_join_*_explain`). Go today plans the indexed one
as `FlatMap(outer=Scan(T1), inner=DefaultOnEmpty(IndexScan(I6, [=])))`, the target's
probe shape (its outer is the primary scan until F-7c's preference), and the
preserved-only one as RFC-152's `NestedLoopJoin(LEFT OUTER, ..., Scan(T1), Scan(T6))`,
with the same rows in both engines (`*_rows_unordered`). Go omits `outerJoinCount`
(documented at `RewritingCostModelLess`), so its REWRITING survivor is the UN-rewritten
outer-join select, and it re-derives the rewritten form in PLANNING with a Go-only
registration of `RewriteOuterJoinRule` in `PlanningExplorationRules` (`default_rules.go`,
the comment beside it), which is how Go reaches the FlatMap probe above.

Decision: keep both Go-only pieces, the omission and the PLANNING re-derivation. The
alternative, porting `outerJoinCount`, makes the rewritten form the survivor and loses
the un-rewritten select the RFC-152 materialized join is implemented from; recovering it
from the rewritten form would need a Go-only PLANNING rule that pulls the ON predicates
back out of the null-on-empty inner, a reverse rewrite with its own correlation
analysis. Keeping the omission costs one comparator criterion and one rule registration,
both existing, documented and pinned, and it leaves the target's plan reachable through
the target's own rule (`RewriteOuterJoinRule`) run in PLANNING. Under the physical prune
PLANNING holds only the un-rewritten select, which is exactly the input the PLANNING
registration already takes today (the virtual prune also hands PLANNING that survivor's
tree), so the argument is unchanged, and it is now a checked one: with the prune option
on, the two LEFT JOIN probes must plan the FlatMap probe and the materialized join
respectively, and the census's preserved-divergence arm (2.6) holds every shape whose
survivor differs from the target's because of the omission to that test. The omission
and the registration are listed together in DIVERGENCES.md, each pointing at the other.

Rejected:
- keeping same-pointer finals and adding partition cross products on top — partitions
  of a shared child group mean nothing to a parent that ranges over the whole group;
- skipping the prune of the fresh copies — Java's merge/pushdown matchers require one
  expression per child, so without the prune the rules keep scanning members, which is
  Go's current non-Java behaviour;
- keeping the designated final permanently — it is the "Go substitute for X" the
  CLAUDE.md rule names; X (the physical prune plus re-derivation) is the answer.

### 2.4 Mechanism

- **D1 progress.** `Task.Run(ctx, p) bool` on all seven task types, values per 2.1.
  One shared `executeRuleCall` computes a call's progress strictly after Go's commit
  sequence — `Err()` clear, `verifyChildrenMemoized` (PLANNING),
  `prepareReferenceMemberBatch`, `CommitStagedInserts`, `batch.commit` — so a failed
  call never advances a chain (it sets `capErr` and the loop stops). Progress is the
  OR of: a committed yield that inserted (per-yield `inserted` flags); partial-match
  growth (PLANNING); a changed-child constraint push by a non-preorder rule (which
  re-queues the rule task); a constraint change on a previously explored child (which
  queues its ExploreGroupTask). Go-only: a committed `InsertReExploring` that inserted
  counts, so `CommitStagedInserts` returns its real insert count.
- **D2 rule metadata.** Optional interfaces `ConstraintDependencies() []any` (keys are
  the existing `*PlannerConstraint` pointers) and `OnlyOnPrunedInputs() bool`. The Java
  dependency declarations are ported rule by rule — the population is every Java rule
  whose constructor passes a constraint set or that pushes a constraint, enumerated at
  implementation with the command recorded in the step's evidence, not a regex count.
  Go-only constraint keys (`OrdinalLayoutConstraintKey`,
  `ReferencedFieldsConstraintKey`) are declared on every rule that reads them; the
  population is found by reading, and a test fails any rule that reads a key through
  `GetPlannerConstraint*` without declaring it (the read goes through one accessor
  that records the key against the running rule).
- **D3 conditional types.** `ConditionalExplorationCascadesRule` and
  `ConditionalImplementationCascadesRule` (Java's names, so `DISABLED_PLANNER_RULES`
  means the same in both engines). Constructors return `(*T, error)` with a typed
  `InvalidConditionalRuleError` for: empty list, differing matcher root operator,
  differing `OnlyOnPrunedInputs`. Dependencies are an order-preserving union. The
  wrapper's matcher is a root-type matcher so the rule index buckets it. `OnMatch`
  fails the call with an unsupported-invocation error. The production rule sets are
  built once per process; a build error is returned from the first plan call as
  `capErr`, and a unit test pins that every production chain builds.
- **D4 `ConditionalTransformTask`** carries (phase, reference, expression, filtered
  members, index). Run: stale expression → false, nothing pushed; otherwise run member
  `index` through `executeRuleCall`; progress → true; else push index+1 if present and
  return false. Push-time filtering mirrors `pushTransformExpressionIfNeeded`: wrapper
  enabled and not stale, then each member enabled and not stale; none left → nothing
  pushed.
- **D5 staleness gate.** `Force` on `ExploreExprTask` and `OptimizeInputsTask`.
  Unforced: the ExploreGroupTask loops. Forced: every rule-yield site. A forced
  request coalescing with a pending task upgrades it. A task pushes a rule iff forced,
  or the group's first exploration, or a dependency was pushed after the group's last
  committed exploration. Go's group-wide re-arm (`Absorb`, `InsertReExploring` via
  `scheduleReExplore`) becomes Java's per-expression model: forced `ExploreExprTask`s
  (plus `OptimizeInputs` for finals) for exactly the folded or inserted expressions;
  a bare tick bump re-fires nothing. `ScheduleFreshReference` stays a first
  exploration.
- **D6 REWRITING partitions.** `ExpressionPartition`, `ToExpressionPartitions(ref)`
  over `FinalMembers()` in insertion order, `RollUpExpressionPartitions`,
  `FilterExpressionPartitions`. The only partitioning property is `SelectMergeable`
  (the expression implements `RelationalExpressionWithPredicates`: Select and
  LogicalFilter), computed on read — equivalent to Java's lazy map because it is a
  pure function of the top node. `PlanPartition` (physical) is not reused.
- **D7 FinalizeExpressionsRule** becomes a REWRITING implementation rule over
  exploratory roots. Per quantifier, the child's partitions; any child with none →
  no yield; a leaf yields itself (keeps the leaf-identity pin; structurally Java's
  `withQuantifiers(∅)`); otherwise one final per cross-product element, each child
  rebuilt with `RebuildQuantifier(q, call.MemoizeFinalExpressionsFromOther(ref,
  part.Expressions()))`. Every reference `newRestrictedFinalReference` mints is marked
  explored, as Java's `Reference.newReferenceFromFinalMembers` marks every one
  (`Reference.java:587-594`), which both of Java's restricted memoizers reach
  (`CascadesRuleCall.java:486-490`, `memoizeFinalExpressionsFromOther`, and
  `:518-523`, `memoizeMemberPlansFromOther`). That is Finalize's references and every
  PLANNING caller's: the population is 12 non-test call sites, 9 of
  `MemoizeFinalExpressionsFromOther` (the in-join, in-union, simple-select and
  unordered-union rules, the four push-through-fetch rules for distinct, filter, in-join
  and map, and `rule_push_distinct_below_filter.go`, which moves a distinct below a
  filter and is not a push through a fetch; v3 called all five "push-through-fetch") and 3 of
  `MemoizeMemberPlansFromOther` (`dml_inner_candidates.go`, `rule_implement_filter.go`,
  `rule_implement_projection.go`), listed by `git grep -n -E
  'MemoizeFinalExpressionsFromOther\(|MemoizeMemberPlansFromOther\(' -- 'pkg/*.go'
  ':!*_test.go' | grep -v 'func '` (v2 counted 9 and missed the second entry point).
  Without the mark every fresh copy re-runs the rules its source already ran. The mark
  is one change to the one shared helper, so it ships with the step-7 option and its
  effect on PLANNING callers is in that step's census (goldens and task counts with the
  option on), not decided by a separate measurement.
- **D8 root membership.** In REWRITING an `ExploreExprTask` on a final queues no
  rules, only child exploration — the net effect of Java's six REWRITING matcher
  predicates — so Go's REWRITING-placed normalization rules never fire on the new
  finals. Merge and pushdown additionally require the root to be a final of its
  reference.
- **D9 pruned-input scheduling.** `OptimizeInputsTask.Run` queues pruned-input rules
  before it captures its dependent floor; the stack is [chain][OptimizeGroup per
  child][ExploreGroup batch], bottom to top. REWRITING yields of new finals queue
  `OptimizeInputs{Force}` and `ExploreExprTask{Force}`; PLANNING keeps its
  physical-only gate.
- **D10 PredicatePushDownRule** becomes a pruned-input implementation rule on a final
  root: Java's matcher (ForEach, not null-on-empty, child rolls up to one partition
  with one expression), the record-result check, the residual set, quantifier-order
  iteration, one yield. A pushed leg is memoized with `MemoizeFinalExpression` (not
  marked explored, so the chain recurses into it); untouched legs are carried with
  `MemoizeFinalExpressionsFromOther`. The raw `InitialOf`+`Insert` construction that
  bypasses the memo is deleted.
- **D11 SelectMergeRule** becomes a pruned-input implementation rule on a final root:
  a quantifier is eligible if it passes Java's partition matcher and every Go guard of
  2.5; zero eligible → return without a yield (the chain falls through to pushdown);
  merge in Java's topological order (declaration order without dependencies), rename
  duplicate aliases, rebase retained quantifiers that depend on a merged alias through
  the composed translation map; Go's positional-seed regime, source-alias rebuild and
  `JoinType` stay.
- **D12 rule sets.** REWRITING exploration = {decorrelate→simplify chain,
  RewriteOuterJoinRule} plus Go's REWRITING-placed normalization rules; REWRITING
  implementation = {merge→pushdown chain, FinalizeExpressionsRule}. `SelectMergeRule`
  and the second `DecorrelateValuesRule` registration leave the default rule list.
  `OptionalRewritingRuleNames()` expands the chains and excludes Finalize, so its name
  set is unchanged; a test pins it. Per-phase placement follows the target's rule sets,
  which is what the DIVERGENCES.md entry "Reference: finalMembers partially aligned"
  means by "per-phase rule-set parity": each rule in Go's REWRITING list whose target
  counterpart is in the target's PLANNING rule set (`PlanningRuleSet`) moves to
  PLANNING; each Go rule with no target counterpart stays where it is only with its
  reason recorded in DIVERGENCES.md. The mapping (Go rule → target rule set, or "none")
  is enumerated by rule name in step 7's evidence, and the census runs on the placement
  that results, so a shape that needs a rule in PLANNING shows up as rule-caused rather
  than being masked by a REWRITING copy.

### 2.5 Go extension guards (each keeps its existing assertion)

| Guard | Today | After |
|---|---|---|
| outer-join parent/child opaque | SelectMerge and pushdown bodies | merge eligibility; pushdown visitor |
| strict-single (parent, nested, push edge) | both rules | pushdown eligibility adds `!IsStrictSingle`; the visitor keeps its nested check |
| null-on-empty | both rules | Java's matcher already excludes it |
| dissolved LEFT box, chained lateral unnest, existential wrap, positional windows | SelectMerge | merge eligibility; the merge `break` becomes "not eligible" |
| row-shape rebase proofs, union all-or-nothing, translation-failure declines | pushdown | visitor, unchanged |
| an un-rewritten outer join survives REWRITING | cost model | unchanged; `TestRewritingBoundary_KeepsUnrewrittenOuterJoin` |

### 2.6 Steps and the plannability gate

1. A test-only rule-call observer (rule, expression, progress) beside the
   ReachabilityCollector — the Go counterpart of the target's `planRuleTrace` step,
   with the counterparts of its IN-UNION-PARTITIONS mode (whose first pins are the
   four queries of 4.3, today measured by a scratch instrument) and its
   REWRITING-RESULT mode (Go's REWRITING survivor rendered in the target's form, which
   the census's comparator-caused and preserved-divergence arms read) — and a
   merge-base baseline: EXPLAIN of every plan-shape golden and harness test, and
   per-query task counts. No behaviour change.
2. D1 as a refactor; the Java planner-test ports that need only plain rules.
3. D2, D3 and the `ConditionalCascadesRuleTest` port.
4. D4 and `CascadesPlannerTest` 1–6.
5. D5, the dependency declarations, the re-arm conversion, the constraint re-queue.
   Full suite, goldens, task counts against the 150k/250k budgets
   (`planner_options.go`).
6. D6 and the `SelectMergeablePropertyTest` port.
7. D7–D12 land behind ONE test-only planner option that turns on the physical prune
   and the rule sets that ship with it (the census is taken on the configuration that
   ships, not on D7/D8 alone; D9–D12 presuppose the prune, so they never run without
   it). Old merge and pushdown remain the default path until the option becomes the
   default. With the option on, the whole plan corpus runs: every plan-shape golden,
   every yamsql `plan_contains`, the harness tests, the conformance plan-diff goldens
   and the 1M stress query set. Every query whose plan changes is recorded in
   `ws-f-oracle/w6-rederivation-census.txt` with both EXPLAINs and one class:
   - **lost plan** (no plan, or an error, including the checked crossing error of
     2.3 for a reference that reaches the crossing with no final or with more than
     one): the exit rule allows ZERO. The corpus includes, by name, the two queries
     that reach the no-finals arm today, `TestAsymmetricUnion_BothLegsPlan` and WS-E's
     `union_leg_type_annulling_fold_where` (whose leg must cross as the one fold
     final), each with the observer's record of the arm it crossed by;
   - **comparator-caused shape change**: Go's REWRITING survivor differs from the
     target's for a reason other than the `outerJoinCount` omission, measured by the
     target trace's REWRITING-RESULT mode against the Go observer's rendering of Go's
     survivor in the same form (step 1): fixed in the comparator (2.3);
   - **preserved-divergence shape change**: the survivors differ because of the
     `outerJoinCount` omission (Go keeps the un-rewritten outer-join select): accepted
     when PLANNING reaches the target's plan for the same SQL through the PLANNING
     registration of `RewriteOuterJoinRule`, or the RFC-152 materialized join wins on cost
     (the preserved extension), each with a plan pin; otherwise fixed in PLANNING, never
     by porting `outerJoinCount`;
   - **rule-caused shape change**, split by the target's own plan for the same SQL,
     measured with the target's EXPLAIN and, where the choice is in question, its
     `planRuleTrace`: target-equal and target-closer changes are accepted; a change
     away from the target's plan is fixed by porting the PLANNING mechanism the target
     uses to reach its plan from its single final;
   - **Go-extension shape** (SQL the target rejects: nullable-array reads, LIMIT, the
     in-memory sort fallback, and the other approved extensions of the preserved list;
     v2 listed the materialized outer join here, but a LEFT JOIN is SQL the target
     accepts, so it is the previous arm's): no target plan exists, so the shape is kept by
     a Go-only re-derivation that is documented at the rule, listed in DIVERGENCES.md and
     pinned by a plan test, or the entry is a lost plan.
   The option becomes the default when the census has no lost plan and no unaccepted
   change; then the designated-final mechanism, its coherence instrument and the
   DIVERGENCES entry are removed together, and `Verify(==1)` becomes a checked error.
   The census file and its classification are the step's evidence.

Steps 5 and 7 regenerate goldens; every moved line is classified in a committed
explain-diff against the target's EXPLAIN of the same SQL where the target accepts it.
Goldens with no target counterpart (Go-only syntax or Go-only goldens) are classified
by the Go-extension arm above.

### 2.7 Tests

Ports: `ConditionalCascadesRuleTest` (all; the mismatched-root-operator case needs an
optional root-operator override in the test rule), `CascadesPlannerTest` 1–6,
`SelectMergeablePropertyTest`, `FinalizeExpressionsRuleTest` (2×2 → 4 finals, one
quantifier → 2, leaf → 1), all of `PredicatePushDownRuleTest` (notably
`testPartitionPredicatesByJoinSource`: one firing, only the join predicate left) and
`SelectMergeRuleTest` (notably `retainNonMergeableChildren`, `doNotMergeDefaultOnEmpty`,
`combineTwoJoins`, `mergeUpAvoidingDuplicates`,
`cannotMergeDueToCorrelationsBetweenSiblings`), through a Go port of
`RuleTestHelper.run`/`preExploreForRule`. New pins: a chain member that matches without
progress, one with progress, a deduplicated yield, a stale expression, member vs
wrapper disablement, a re-arm that re-explores only new expressions, a non-preorder
push that re-queues its rule, a re-run of Finalize that makes no progress, and every
guard of 2.5 through the new harness. End to end, through the observer and
`PlanQueryForTest`: a two-leg join with per-leg predicates (merge makes progress on
the root final, pushdown never runs on it), a Select over UNION ALL (merge makes no
progress, pushdown pushes in one firing), and disabling `SelectMergeRule` (pushdown
still fires) versus the wrapper name (neither fires). yamsql `plan_contains` pins for a
predicate pushed through UNION ALL, sort, DISTINCT and a multi-leg derived-table join,
with index-scan tokens captured from a real run.

### 2.8 Stale text to correct when this lands

`rule_predicate_push_down.go` (the "Java's per-quantifier rule" comments),
`rule_select_merge.go`, `default_rules.go`'s rule-set comments,
`rule_predicate_push_down_test.go`, `unified_tasks.go` (the stage-boundary comment and
both crossing arms), `asymmetric_union_planning_test.go`'s header (it describes the
deleted arm as the fix), `reference.go`'s `AdvanceStagePreservingMembers`,
`planning_cost_model.go` (the RFC-186 designation comments), `expression_partition.go`
(the comment at `:207-217` saying the memoizer copies no constraint, and its pin
rationale), `designated_final.go`,
`rewriting_final_invariant.go` (the instrument being deleted), DIVERGENCES.md (the
REWRITING-prune entry and every other copy of its claims), and TODO.md's copies. The
word "designated" alone is not a spelling of this mechanism (the intersection plans
designate a driving stream, `multi_intersection.go`, `merge_cursor.go`,
`rule_aggregate_data_access.go`), so the closure grep enumerates the mechanism's
spellings instead (one line, so it can be pasted as printed; v3 wrapped it inside the
quotes): `git grep -c -i -E 'per-quantifier rule|universal (forced |boundary )?prune|prune-to-1|designated[ -]final|designation ?scope|designation/extraction|designation.staleness|virtual prune|RFC-186|PreservingMembers|no-finals (path|arm)' -- pkg TODO.md DIVERGENCES.md CHANGELOG.md CLAUDE.md`.
v4 adds the crossing arm's spellings (`PreservingMembers`, "no-finals"; "RFC-182" alone
is not one, since the rowdiff harness and its findings carry that number). At the v4
tree the printed command counts 119 lines in 36 files, of which the added spellings
account for 11 lines in 9 files (`TODO.md` 1, `asymmetric_union_planning_test.go` 2,
`reference.go` 2, `unified_tasks.go` 1, and one each in `memo_admission.go`, its test,
and three planner tests). At the tree v3 was written against, v3's expression counted 108 lines in 30 files (DIVERGENCES.md 6, TODO.md 2,
`designated_final.go` 21, `planning_cost_model.go` 12, `unified_tasks.go` 11, `planner.go`
7, the rest 1 to 14; `abstract_data_access_rule.go:895` and `tie_break_hash.go:14` among
them, which v2's grep missed). That count is the positive control. At the landing tree
the same command is re-run and every remaining hit is listed in the step's evidence with
its disposition, deleted, rewritten to describe the physical prune, or a citation of the
RFC-186 design document as history; the step does not close while any hit describes the
virtual prune or a designated final as current behaviour. `shifts/` and `rfcs/` are
outside the population: they are history, this design included.

## 3. W7 — explicit index-entry readers

### 3.1 Target (SOURCE, readers report section 1)

A reader Value evaluates with the raw `IndexEntry` bound under `Quantifier.current()`
(`RecordQueryPlanWithIndexEntryToQueriedRecord.java:139-150`). `IndexEntryObjectValue`
reads `KEY` or, for anything else, `VALUE` (`:127-137`), walks its ordinal path (null
midway → null, out of bounds → error), converts with `tupleValueToRuntimeValue`,
throws on a missing binding, and explains as `KEY:[0]`. `IndexEntryToRecordValueHelper`
builds the reader as a trie over field names: first covering source wins, target field
order, uncovered → `NullValue(type)` except a non-null array → empty array. The
aggregate candidate builds a reader by a static layout function (ordinary: grouping
from KEY, aggregates from VALUE; permuted, decided by index type even at size 0: all
from KEY). `RecordQueryAggregateIndexPlan` prefers the reader, falls back to copiers
into the dynamic result descriptor, carries the reader through strictly-sorted,
translation and minimize, excludes it from equality, hash and plan hash, and never
explains it. `RecordQueryCoveringIndexValuePlan` exists with full identity,
serialization and property support, but normal covering planning still emits the old
`RecordQueryCoveringIndexPlan` (`ValueIndexScanMatchCandidate.java:276-282`). Aggregate
cardinality reads candidate evidence: scalar → at most one row; the
equality-bound-grouping arm compares a record value to per-column values and never
fires (readers report section 1, "Why that last check can't fire").

### 3.2 Go today

`values.IndexEntryReader` requires `PrimaryKey() any` / `IndexValues() any`, which the
real `*recordlayer.IndexEntry` does not implement (its methods return `tuple.Tuple`), and
which mean the extracted primary key and the key prefix, not raw KEY and VALUE;
`Evaluate` wants a map the executor never passes; the walk descends `[]any` only;
misses read as NULL; `OTHER` reads nil. Only a fake entry tests it. Covering plans are
emitted unconditionally and decoded by name, falling back to a fetch for nested,
expression and multi-type layouts.

### 3.3 Decisions

- **D1 binding.** `values.IndexEntryTuples { IndexEntryKey() tuple.Tuple;
  IndexEntryValue() tuple.Tuple }`, implemented by `*recordlayer.IndexEntry` returning
  `Key`/`Value` (compile-time assertion). `IndexEntryReader` is deleted. `Evaluate`
  takes the executor's `CorrelationBinder`; the executor gives each cursor one reusable
  binder that layers `CurrentCorrelation()` → entry over the outer bindings. The walk
  descends `tuple.Tuple` and `[]any`; null midway → null; out of bounds, a missing or
  mistyped binding and a nil context → errors. `KEY` reads KEY, `VALUE` and `OTHER`
  read VALUE. A checked constructor refuses record and array result types. Explain arm
  `KEY:[0]`. Go keeps source in equality (consistent with its hash).
- **D2 row domain.** The leaf stays a pure extractor; readers convert when they write
  a row slot through one exported `values.TupleElementToRowValue` (moved from the
  executor, which then delegates). Java's INT/BYTES/ENUM conversions are identities in
  Go's row domain. RFC-162's "Java's leaf never renders type" is wrong
  (`IndexEntryObjectValue.java:136`) and is corrected there and in
  `uuid_indexable_roundtrip_fdb_test.go`.
- **D3 helper.** A Go trie with `cover`, `withChild`, `toRecordValue`, `entryColumn`
  and Java's absent-field rule. Reader record constructors stay anonymous. Output is
  materialized directly: top-level fields into `PositionalRow` slots of the plan's
  result type; nested record columns into messages of the stored nested descriptor,
  filled by field name — Java's `MessageCopier` analogue, matching how base scans keep
  nested messages. This never builds a synthesized nested message, so Go's positional
  field numbering of synthesized types (`proto_type.go`) cannot reach a write.
- **D4 extraction.** Port `MatchSimpleFieldValueRule`,
  `MatchFieldValueOverFieldValueRule` and `CompensateToOrderedBytesValueRule`. Go keeps
  covering `__ROW_VERSION` from VERSION indexes, which Java's simple-field rule refuses
  only because its partial record has no place for a version: a Go-only read extension,
  documented at the rule with a test, and listed in DIVERGENCES.md. It passes D8's
  emission gate by one Go-only builder arm for the version pseudo-field (the target's
  builder has no field for it and `isOfPushableTypesOrConstant` refuses it), so the
  gate is the target's in every other respect; a test pins a VERSION index still served
  COVERING after step 7, and one pins that no other pseudo-field takes that arm.
- **D5 aggregate reader.** Port the static layout function; the candidate gains
  `permuted` and `permutedSize`; attach the reader at every rule-built aggregate plan;
  the executor prefers it and keeps the existing layout cursors as the copier-equivalent
  fallback; the vacated-group drop runs before decoding and the PERMUTED_MIN repair
  after, from the raw grouping key. The reader is checked against the result type when
  the cursor opens, stays out of structural key, observational hash and execution salt
  (so in-flight continuations still resume, pinned by the scan-range identity tests),
  and is registered in `plan_finalize.go`. Explain is unchanged.
- **D6 cardinality.** Rule-built aggregate plans carry typed candidate grouping
  evidence. No evidence → unknown maximum; grouping count 0 → at most one row; grouped
  → unknown maximum, which is what the target computes. Go already declines candidacy
  for ungrouped aggregate indexes (`TestUngroupedAggregateIndexDeclinesCandidacy`,
  kept), so the at-most-one arm is unit-pinned, and a cross-engine probe pins that the
  target's equality-bound arm does not fire.
- **D7 covering-Value plan.** `plans/covering_index_value_scan.go`: index plan,
  record-type name, reader, result value; structural key includes the reader; the
  observational hash and a new execution salt of a distinct kind exclude it; primary
  key unless BY_GROUP; explain `IndexScan(IDX, [..] COVERING -> {F: KEY:[0], …})`,
  following the target's rule that the two plans explain alike except after the
  arrow; arms in `plan_properties.go` (distinct, stored, primary key),
  `derivations_evaluator.go`, the hint contracts, `index_scan_carrier.go`,
  `plan_finalize.go`, executor dispatch and rowdiff classification. It is never
  emitted, as in the target; it has consumers all the same, the ported
  `FDBCoveringIndexValuePlanTest` cases, which build it by hand exactly as the target's
  tests do, and the umbrella requires the class. Section 10's "no framework without a
  consumer" is about W14's visitor, for which no consumer exists yet. The continuation
  salt leaves the reader out, as the target's continuation hash does
  (`RecordQueryCoveringIndexValuePlan.java:313-318`); it does include the plan's flowed
  type, as every Go scan salt does (`scan_range_execution_identity.go`, the `flowed-type`
  field), so two covering-Value plans that flow different row types never share a
  continuation. Go's salt for the old
  covering plan includes its covering columns (`scan_range_execution_identity.go:
  359-362`): the reader cannot differ between two plans that share an index plan and a
  record type, because it is a pure function of those two (the candidate builds it from
  them), whereas the old plan's covering column list can differ for one index plan.
  A test pins that pure-function claim. The cost model's tie-break hash gets an arm for
  the new plan, so equal-cost ties never fall to map order
  (`planning_cost_model.go`, the tie-break hash). Go's distinct-records has no
  aggregate arm and answers false where the target answers true
  (`DistinctRecordsProperty.java:157-159`); that is fixed in its own step (section
  3.4) because it changes plans.
- **D8 old covering plan.** It stays the emitted class with byte-identical explain,
  identity and salt. In the last step its decoder becomes the reader built by the Go
  port of `computeIndexEntryToLogicalRecord`, and covering is emitted only when that
  construction succeeds — the target's gate (one queried record type, a valid builder,
  no repeated covered field). Push-through uses the logical key/value Values, as the
  target does. This is the faithful port of the old plan, because the target's old
  plan evaluates the same extraction Values through `FieldWithValueCopier` and proves
  copiers and readers equivalent (`IndexEntryTranslatorEquivalenceTest`); it removes
  three Go gaps (nested covering served by fetch, function-key columns never
  pushable, multi-type COVERING labels that fetch).
- **D9 serialization.** No stored bytes and no Go continuation change. The aggregate
  reader's proto field 8 (written and read last, absent → fallback), plan tag 41, the
  leaf proto with an explicit `TupleSource` mapping (Go's iota 0/1/2 against proto
  1/2/3) and `theValueSurvivesSerialization` belong to the WS-G plan codec and are
  listed there.

### 3.4 Steps, tests, risks

Steps: (1) binding, real-entry FDB tests, the five stale comment sites
(`value_index_entry_object.go`, `map_field_values.go`, `semantic_equals.go`,
`index_entry_semantic_test.go`) and the RFC-162 claim; (2) helper, extraction,
nested materialization; (3) aggregate reader; (4) cardinality evidence; (5)
covering-Value plan and its registries; (6) the candidate-side construction and the
equivalence tests; (6a) the aggregate distinct-records arm; (7) the old covering
decoder and emission gate. Steps 1–6 change no explain and no plan shape (a moved salt
digest is a bug). Step 6a changes plans: distinct-records drives rule firing
(`plan_properties.go`), so a true answer lets DISTINCT removal fire over aggregate
index plans, and a wrong true drops a DISTINCT and returns duplicates. It therefore
carries a distinctness proof per Go aggregate mode — the ordinary layout, the permuted
MIN/MAX layout with its null repair, the vacated-group drop (`liveGroupsOnly`) and a
grouped scan resumed across pages — each as an FDB test that runs the DISTINCT query
with and without the removal and compares rows, plus the golden classification. Step 7
changes plans (fetch disappears for nested and function-key projections, COVERING
disappears for multi-type scans) and takes the golden classification and the 1M
stress comparison.
Ports: `AggregateIndexEntryToRecordValueTest` (except serialization),
`IndexEntryTranslatorEquivalenceTest` on real entries,
`FDBCoveringIndexValuePlanTest` (all ten, the plan built by hand from a real covering
scan), `GroupByTest.rewritingAnAggregateIndexPlanKeepsItsEntryReader`, the
`ExplainPlanVisitorTest` cases. Go-specific: every leaf error arm, OTHER, nested
tuples, both aggregate paths with the vacated-group drop and MIN repair, UUID and
float grouping keys, a resumed continuation across pages, all three cardinality arms.
The reader is proven live by mutation: removing it leaves the fallback green;
corrupting one ordinal reddens the aggregate FDB tests. Step 7 adds a yamsql file with
a nested-struct index and an order-descending index, each asserting
`plan_contains: COVERING` and `plan_not_contains: Fetch` with rows.

## 4. W8 — vector-engine preference and the relational planner configuration

### 4.1 Vector-engine preference (SOURCE, properties report W8)

Target: `VectorIndexEnginePreference` {NO_PREFERENCE, PREFER_HNSW, PREFER_GUARDIANN},
planner-configuration proto field 15; the relational option
`VECTOR_INDEX_ENGINE_PREFERENCE` (connection scope, default NO_PREFERENCE) is part of the
planner configuration's equality and hash, so it is part of the plan-cache key. The
cost-model rung runs after the IN rung and before primary-versus-index
(`PlanningCostModel.java:188-193`) and abstains unless both sides have exactly one
vector access with identifiable, different engines, one of them preferred
(`:354-419`); an access's engine comes from its own match candidate; a missing engine
option reads as HNSW, case-insensitively; the metric is read through the typed key
`hnswMetric` with alias `vectorMetric`.

Decisions:
1. `PlannerConfiguration.VectorIndexEnginePreference` (proto numbering), default NONE.
2. `OptVectorIndexEnginePreference = "VECTOR_INDEX_ENGINE_PREFERENCE"` with default
   NO_PREFERENCE in `defaultOptionValues`; wired into `plannerOptionsFrom`, into the
   plan-cache key (both the early-return test and the rendered key, keeping the
   tokens prefix-free), and a DSN parameter `vector_index_engine_preference` that
   accepts the enum names and rejects anything else.
3. The candidate records its engine kind through WS-D's engine parser; SPFresh has its
   own Go-only kind that no preference selects, so a SPFresh index is never read as
   HNSW.
4. `ToScanPlan` copies the kind onto the vector plan with `WithIndexEngineKind`; it
   stays out of identity and hash (the target's plan hash ignores the candidate).
5. The metric is read through WS-D's typed option catalog; Go's convenience spellings
   stay a separate adapter (`ws-d-design.md`).
6. The rung counts vector accesses and their single engine in both cost walks and
   sits right after `compareInPlan`, as in the target.
7. The target's rung is not a total preorder (preferred-engine plan `a`, other-engine
   plan `b`, no-vector plan `c`: `a~c`, `c~b`, `a<b`), and fixing that would break the
   target's own `preferenceDoesNotSeparateAVectorPlanFromANonVectorPlan`. Go keeps the
   target's form, adds a DIVERGENCES.md note beside criteria 6/7, and pins why a mixed
   group cannot reach the rung: the data-access-count rung runs first, and a
   distance-rank predicate is satisfiable only by a vector index. The pin's failure
   message names the hazard it re-opens.
8. The rule-level comparators' configuration is decided in 4.2, because it is 4.2's
   change that makes it observable.
9. Go never serializes the planner configuration; a proto-schema guard pins field 15.

Tests: all nine `PlanningCostModelVectorEngineTest` cases, the default/setter part of
`RecordQueryPlannerConfigurationTest`, a rung-order test, the total-preorder sweeps
extended with a vector-only corpus, a rule-level comparator test with no statistics, a
planner-level test with HNSW and GuardiANN candidates built directly (flipping the
choice), plan-cache key injectivity, and replan stability under NONE (Go's tie-break
hash is FNV, not the target's `planHash`, so the target's NO_PREFERENCE block is not
copyable). SQL end to end needs GuardiANN DDL and execution from WS-D: a sqldriver FDB
test with the DSN parameter asserting the chosen vector index, plus the target's
negative fixtures (`hnswOnly`, `guardiannOnly`, `mixedMetrics`, a non-vector covering
scan) and a union whose legs switch independently. W8's checkbox stays open until
that test is green.

### 4.2 The relational planner configuration (found by this workstream)

SOURCE: the target's relational layer builds its planner configuration in
`PlannerConfiguration.buildRecordQueryPlannerConfiguration` (`PlannerConfiguration.java:
158-169`): index scan preference PREFER_INDEX, in-join-as-union size 24, the index
fetch method, the disabled rule names, `disableRewritingRules()` when the
DISABLE_PLANNER_REWRITING option is set (:164-166), right-deep joins and the vector
preference (the first two lines were already there at 4.12.11.0). Go's SQL planner
starts from `cascades.DefaultPlannerConfiguration()` (`planner_options.go`,
`plannerOptionsFrom`), the record layer's default (PreferScan, size 0). `grep -rn
IndexScanPreference pkg/relational | wc -l` counts 0 lines over every file there, test
files included; the positive control over the same population, `grep -rn
PlannerConfiguration pkg/relational --include='*.go' | grep -v _test | wc -l`, counts
26.

MEASURED (0.1): under the target's configuration every plan class moves. An unfiltered
`SELECT *` is `ISCAN(I1 <,>)` (index scan with fetch over the primary scan); `SELECT
col1` is a covering scan of I1; a residual over an indexed column is served by the
covering scan with the residual below a FETCH; a disjunction over two indexed columns is
an ordered union of two covering scans; `IN` lists plan as in 4.3. Go plans every one of
them as a primary scan.

Decision.
1. One Go function mirrors `buildRecordQueryPlannerConfiguration` field by field and
   is the only place the SQL planner's configuration comes from, on every path
   including `plannerOptionsFrom(nil)` (the test-harness path, `plan_harness.go`,
   which today returns the record-layer default early): PREFER_INDEX, in-union size
   24, the fetch method, disabled rules,
   rewriting disabled exactly when the DISABLE_PLANNER_REWRITING option is set (v1 said
   "when every rule is disabled", which is wrong), right-deep, and the vector
   preference of 4.1. The fetch method: the target passes it into its index plans
   (`ValueIndexScanMatchCandidate.java:268`), and its relational default is
   USE_REMOTE_FETCH_WITH_FALLBACK (Go's `api/options.go`, `defaultOptionValues`). Go
   accepts `INDEX_FETCH_METHOD` and ignores it (`planner_options.go`, the plannerOptions
   comment: neither FDB client Go uses exposes getMappedRange, and Go's index plan has
   no fetch-method field), so v2's "no other value is reachable" was false. Decision:
   the configuration carries the option's value (so the configuration Go builds is the
   target's), and Go executes the standard fetch for every value. The plan-cache key gets
   NO fetch-method arm, in neither the early return nor the rendered key (v4; v3's "so
   the cache key ... agree" implied one and `cacheKeyPart` has none): no Go plan carries
   or depends on the method, so two statements differing only in it plan byte-identical
   plans and may share an entry. A test pins that sharing, with this reason in its
   failure message; the change that first makes a Go plan depend on the method adds the
   arm to both halves in the same commit. That is a
   declared divergence in how records are read, never in which rows or which plan: the
   target's USE_REMOTE_FETCH_WITH_FALLBACK falls back to the standard fetch, its
   explain of an index scan does not name the method (every `ISCAN` of 0.1 ran under
   the default), and USE_REMOTE_FETCH differs from the standard fetch only in the round
   trips. It stays in DIVERGENCES.md and in TODO.md's remote-fetch entry, whose owner
   is the client (getMappedRange), not this workstream.
2. The configuration and the metadata reach every comparator. Without statistics the
   rule-level comparators get neither: `costModelDiagnosticsOnlyContext`
   (`planning_cost_model.go`) replaces the context with `EmptyPlanContext()` and keeps
   only the diagnostics sink, so it strips the configuration AND the metadata. It has
   three callers: `ExpressionRuleCall.CostModel` (`expression_rule_call.go:107`),
   `ImplementationRuleCall.CostModel` (`implementation_rule.go:115`), both when
   `Stats == nil`, and `NewPlanner` (`planner.go:253`), whose comparator
   `WithStatistics` replaces with the full context (`planner.go:365`, even for nil
   statistics); the SQL path always calls `WithStatistics` (`planner_options.go`,
   `newCascadesPlanner`), so SQL's OptimizeGroup ranks with the full context today while
   its rule-internal winner selections (`getWinnerForOrdering` in the filter,
   projection, type-filter, nested-loop-join, in-union and sort rules, among others) rank
   with neither. A non-nil context switches on the metadata-based criteria: criterion
   2's provable maximum cardinality through a unique index's match candidates and
   criterion 12's unmatched key fields (`planning_cost_model.go`, the four `ctx != nil`
   sites, lines 1971, 2594, 2850 and 3004 at this tree). v2 said the wrapper "keeps the
   full configuration and drops only the diagnostics sink", which was backwards (the sink
   is the part it keeps) and silent on metadata. Decision: the three callers pass the
   context they have, whole; `costModelDiagnosticsOnlyContext` is deleted, and a
   comparator with no statistics ranks with the planner's configuration and metadata, as
   the target's rule calls do (they read `call.getContext()`'s configuration and
   metadata whatever the statistics). The metadata-based criteria then apply inside
   rules as well, which is a plan-changing cause of its own; so it lands as its OWN step,
   F-7a, under today's configuration, before the IN-list step and the preference flip,
   and each of the three steps of F-7 has one cause. Its stale text: the comment on
   `costModelDiagnosticsOnlyContext` ("logging must not activate new winner criteria")
   and `NewPlanner`'s "preserve the historical nil-context cost semantics" go with it.
   What F-7a feeds (v4; v3 framed it as matching the target's rule calls, and it does
   not). Java has one PLANNING cost model, built from the configuration alone
   (`PlannerPhase.java:38`, `PlanningCostModel.java:94`). Its implementation rules do not
   choose: each yields one plan per partition of its child, over a reference restricted
   to that partition's plans (`memoizeMemberPlansFromOther`), and `OptimizeGroup` picks the
   winner (`CascadesPlanner.java:650-658`). Go's rules choose inside the rule with
   `getWinnerForOrdering`, a Go-only pre-selection. That is 14 non-test call sites in 10
   files at the v4 tree, counted with `git grep -n 'getWinnerForOrdering(' -- 'pkg/*.go'
   ':!*_test.go' | grep -v 'func '`:
   - `rule_implement_filter.go` 1;
   - `rule_implement_insert.go` 1;
   - `rule_implement_intersection.go` 1;
   - `rule_implement_limit.go` 1;
   - `rule_implement_nested_loop_join.go` 3;
   - `rule_implement_projection.go` 1;
   - `rule_implement_recursive_dfs_join.go` 2;
   - `rule_implement_recursive_level_union.go` 2;
   - `rule_implement_temp_table_insert.go` 1;
   - `rule_implement_typefilter.go` 1.

   The in-union and sort rules, which v3 listed, are not among them: the in-union pins
   through `pinOrderedSpine` (4.3 item 3). So F-7a feeds the context to a Go substitute.
   It makes the substitute rank as the target's OptimizeGroup would. It does not make
   the rules the target's. Decision: the substitute goes, as its own step, F-8. After
   F-7c, each rule with a target counterpart yields per partition as the target's does,
   over `MemoizeFinalExpressionsFromOther` of the partition's plans (the change item 3 of
   4.3 makes for the in-union), and `OptimizeGroup` chooses. The rules with counterparts
   are `ImplementFilterRule`, `ImplementInsertRule`, `ImplementIntersectionRule`,
   `ImplementNestedLoopJoinRule`, `ImplementTypeFilterRule`,
   `ImplementTempTableInsertRule`, `ImplementRecursiveDfsJoinRule` and
   `ImplementRecursiveLevelUnionRule`. Projection's is `MergeProjectionAndFetchRule`
   beside `RemoveProjectionRule`. LIMIT is the approved Go extension with no counterpart:
   its selection stays, documented at the rule and in DIVERGENCES.md. F-8 changes plans,
   one rule family per commit, each with the classified explain-diff; it is listed with
   the plan-changing steps in section 12.
3. The primary-versus-index rung stays Go's rank (`primaryVsIndexRankOf`), and the
   covering class is a DECLARED divergence, measured. The target's
   `comparePrimaryScanToIndexScan` rules only on the pair (lone primary scan, single index
   scan WITH fetch) and abstains otherwise (`PlanningCostModel.java:459-503`); Go's rank
   penalises a lone primary scan under PREFER_INDEX against every other plan. The target's
   own verdicts, pair by pair, are now measured: the trace's ROOT-PAIRS mode ranks every
   pair of the root reference's final members at its last PLANNING OptimizeGroup with the
   target's `PlanningCostModel` under the relational configuration, beside each member's
   residual count, type-filter count and unmatched-field count, for six plan classes, each
   under THREE table and index name sets, so a pair whose verdict only the plan hash
   decides shows as one that moves with the names (`w8_root_*_v0..v2` and their
   `_summary` rows). For `SELECT col1 FROM T1`:
   - `COVERING(I1)` beats `ISCAN(I1)` and `ISCAN(I2)` (fewer fetches), `ISCAN(I1)` and
     `ISCAN(I2)` beat `SCAN` (the PREFER_INDEX rung), and `SCAN` beats `COVERING(I1)`,
     STABLE across the three name sets while the hash order moves (so not the hash): the
     rung abstains on (primary, covering) and the unmatched-field rung decides it, 1 for
     the scan against 3 for every index member. The target's relation is a CYCLE,
     COVERING < ISCAN < SCAN < COVERING (v1 said it was not a total preorder; this is the
     witness), and its winner, `COVERING(I1) | MAP`, is an artefact of the order in which
     OptimizeGroup meets the members.
   - The same cycle holds for the covering-with-residual classes (`col1 <> 10` projecting
     col1 or id): the covering member WITHOUT a fetch beats the covering member with one
     and `ISCAN(I2) | FILTER`, those beat the scan, and the scan beats the covering member
     without a fetch; the latter wins. Where the covering member carries a fetch
     (`SELECT *`), the PREFER_INDEX rung decides it against the scan and there is no
     cycle.
   - A verdict is shown to be a criterion's only where the hash order moved across the
     name sets while the verdict did not (the covering-against-scan pairs above); where
     the hash order did not move, a STABLE verdict proves nothing. Where the verdict
     moved, the hash decides: two single-index scans with the same filter
     (`ISCAN(I1) | FILTER` against `ISCAN(I2) | FILTER`, `w8_root_neq_summary`) and, in the
     disjunction, two union shapes that differ only in which leg carries the equality.
     Neither is a primary-versus-index pair.
   Decision: Go keeps its rank, a total preorder, rather than port a cyclic relation
   whose winners depend on iteration order.

   **What the target's order is (v4).** The target's winner under a cycle depends on the
   order `OptimizeGroup` iterates (`CascadesPlanner.java:650-658`). v3 inferred that order
   from a trace that sorted members by name. ROOT-PAIRS now records the memo order and
   the sequential pass's winner. In all 27 root rows (nine classes, three name sets each)
   that winner is the plan the target's EXPLAIN shows, asserted per row, so the target's
   final plan for each class is measured, not inferred.

   **The declared divergence covers the final plan, not only the pairwise verdict.** Go's
   rank is expected to pick the target's final plan on every class except `w8_no_predicate`
   (the covering scan where one exists, else the index scan with fetch, else the index
   union). That is expected, not measured: Go's SQL planner does not run under
   PREFER_INDEX until F-7c. So each class's EXPLAIN row is accepted SAME-PATH, or
   SAME-CLASS for `w8_no_predicate`, and owned by F-7c. A class whose Go final plan still
   differs at F-7c is moved, by F-7c, to DIFF-PATH `covering-rank` with Go's path. So the
   difference that remains is stated per final plan in code, whichever it is.

   The pairwise verdict on (primary, covering), which the target decides by unmatched
   fields and Go by the rank, is declared as it was. DIVERGENCES.md row 7 ("Two things
   change versus Java, and nothing else") is rewritten to state it with the measured cycle,
   the memo orders, and the classes. The pins: the root-pair probes (target), a Go unit
   test per class driving `primaryVsIndexRankOf` and the full comparator over the same
   members and asserting the target's FINAL plan, and the total-preorder and
   fold-stability sweeps (`cost_model_total_preorder_test.go`) re-run under PREFER_INDEX.
   A hash-decided target pair is not a class verdict and is not pinned as one; Go's
   choice there is its FNV tie-break's, declared with the other hash ties.
4. Plan-shape consequence: every SQL query whose tables have an index can change plan
   (unfiltered and filtered scans, projections, disjunctions, IN lists). This is the
   largest plan-shape change of the workstream and is its own step (F-7c), after every
   other plan-changing step, so each flip has one cause. It regenerates every golden
   with a classified explain-diff against the target's plan for the same SQL, and runs
   the 1M stress comparison at the merge-base, n >= 2 per side, both SHAs recorded.
   Parity is the selection criterion on this shared query surface; the measured
   wall-clock deltas go into the TODO.md stress table beside the target's plan, and a
   regression is examined for an executor defect (a Go fetch slower than the target's
   for the same plan) rather than answered by keeping PREFER_SCAN.

Stale text this makes false, each corrected in the step that makes it false:
`planner_options.go`, the `config` field comment ("Only ShouldJoinRightDeep is
option-driven today; the rest are the Java defaults, exactly as Java's
buildRecordQueryPlannerConfiguration leaves everything"), `plannerOptionsFrom`'s "A
nil/empty Options yields the Java defaults ..., so the no-options path plans exactly as
before", the INDEX_FETCH_METHOD paragraph of the `plannerOptions` comment (rewritten to
the declared divergence of item 1, no longer "not honored ... tracked in TODO.md" as if
the field did not exist), and the DISABLE_PLANNER_REWRITING description, which D12's rule
sets change (F-5); `plan_context.go`, the `AttemptFailedInJoinAsUnionMaxSize` comment
("controls when InUnionRule falls ...", F-7b) and "no SQL surface sets a non-default
today" on `IndexScanPreference` (F-7c); `planning_cost_model.go`, the
`costModelDiagnosticsOnlyContext` comment, deleted with the function (F-7a);
`planner.go`, `NewPlanner`'s historical-semantics comment (F-7a); DIVERGENCES.md row 7
and the index-preference paragraph under it (F-7c); and `planner_options_test.go`'s pin
of the old default, replaced by a pin of the target's configuration (F-7c). Closure, run
at F-7c's tree with its positive control in the same invocation, over `pkg` and
DIVERGENCES.md, with `git grep -n -i -E` and these six alternatives (each on one line in
the source it matches): `defaults, exactly as Java`, `plans exactly as before`, `no SQL
surface sets a`, `costModelDiagnosticsOnlyContext`, `historical nil-context` and
`accepted and NOT honored`. At this tree they match 14 lines in 7 files (the
diagnostics test among them); at F-7c's tree they match zero, and `git grep -c -i
IndexScanPreference` over the same population is non-zero.

### 4.3 IN-list plans (found by this workstream)

MEASURED, both engines, what each in-union rule SAW (v2 inferred it from the survivor;
Graefe and Torvalds showed the inference could not tell a rule difference from a
partition difference). The target's rule trace gains a mode, IN-UNION-PARTITIONS, that
replays `ImplementInUnionRule.onMatch`'s reading at the end of every call: the inner
reference, rolled up into the ordering partitions the rule iterates, each partition's
provided ordering and plans, and per requested ordering the union ordering the rule
builds and the comparison keys it enumerates (probes `w8_iup_*`). Go's counterpart is a
scratch instrument of the same content in Go's rule (`fdb-wsf3-probe`, log
`/var/tmp/fdb-upgrade-recovery/wsf3-go-iup.log`); its committed form is W6 step 1's
observer, whose first pins are these queries.

- ORDER BY col1 and no ORDER BY: the two engines' inner references hold the same
  partitions (the I1 plans in one partition, ordering `(COL1 fixed by the explode
  alias, ID)`), and the rules compute the same union ordering, `COL1` CHOOSE (the
  fixed binding promoted, because the request does not name its direction) and `ID`
  sorted, with the same satisfying keys `(COL1, ID)`. The union-ordering arithmetic of
  the two rules agrees; Go's comment that it keeps "the provider's existing ordering
  set" is right, and v2's "port the ordering computation" had nothing to port.
- ORDER BY id: the partitions differ. The target's inner reference, under the requested
  ordering `ID`, holds ONE plan, `SCAN([IS T1]) | FILTER COL1 EQUALS q` (`innerFinals=1`),
  so its rule can only build the in-union over the scan, which its cost model then ranks
  below `SCAN | FILTER COL1 IN …` (v2's trace). Go's holds the scan AND the I1 partition
  (`Fetch(IndexScan(I1, [=] COVERING))`, `IndexScan(I1, [=])`), whose union ordering
  satisfies `ID` with keys `(ID, COL1)`, so Go builds and chooses the in-union over I1.
- The cause is upstream of the in-union rule, in the data-access rule, and it is not
  specific to IN lists: `SELECT * FROM T1 WHERE col1 = 10 ORDER BY id` is
  `SCAN([IS T1]) | FILTER …` in the target (its ONLY root member, `w8_root_eq_order_by_id_*`)
  and `IndexScan(I1, [=])` in Go (`w8_eq_order_by_id_explain`). The target's
  `AbstractDataAccessRule.satisfiesRequestedOrdering` checks a match against the
  requested ordering over the match's MATCHED ORDERING PARTS
  (`AbstractDataAccessRule.java:779-845`): equality-bound parts are skipped, and the
  first part that is neither equality-bound nor the requested value ends the check
  unsatisfied. `computeMatchedOrderingParts` (`ValueIndexLikeMatchCandidate.java:63-116`)
  walks the candidate's placeholders, which cover the index's key AND the trimmed
  primary key (`ValueIndexExpansionVisitor.java:160-211`), so the suffix is not the
  obstacle as such: a relational table's primary key begins with the record-type key,
  so I1's full key is `(COL1, record type, ID)` (the covering explain's `ID: KEY:[2]`),
  and at match time the record-type part carries no comparison. The plan's ordering binds
  it (`computeOrderingFromScanComparisons` adds the implicit record-type equality,
  `computeEqualityBoundImplicitOrderingParts`, `:119-163`; the I1 partition above reads
  `recordType([_])=[=:IS T1]`), the match ordering does not, so the check stops at the
  record-type part and ID is never reached. MEASURED on the target's record layer, where
  the primary key has no record-type key (the `Order` type, key `order_id`): `price IN
  (10, 20)` sorted by `order_id` builds an in-union over the price index COMPARE BY
  `(order_id, price)` (`w8_rl_default_in2_order_by_pk`), so there the target's match does
  reach the primary-key suffix. Go's `ValueIndexScanMatchCandidate.
  ComputeMatchedOrderingParts` (`match_candidate_index.go`) continues into the
  primary-key suffix without a record-type position (`pk_suffix_ordering_test.go` pins
  it, as the fix of a Go bug where the same query planned an in-memory sort over the
  index). The engines differ only in whether a relational table's record-type position
  stops the match.

Decision.
1. Go's match ordering past the record-type position stays a Go read extension: it is
   correct (the plan-ordering computation of both engines knows the position is fixed
   for a single-type index), it only ever lets Go read an index prefix where the target
   scans the table, for SQL both answer with the same rows, and it touches no stored
   byte. `SELECT id, amount FROM orders WHERE customer_id = 0 ORDER BY id` is one of the
   1M stress queries ("ORDER BY PK + index filter"); porting the target's stop would turn
   it into a full scan. It is declared in DIVERGENCES.md with the measured contrast, and
   its explain rows are declared DIFFs in the acceptance map. v2's "Go's plan is correct
   but the target's rule never enumerates it" located it in the wrong rule.
2. The extension must not turn the target's in-union size limit into a failure the
   target does not have. The target builds an in-union only over legs its data access
   built, and with more values than the in-union size it fails (`w8_in25_order_by_col1_rows`,
   XXXXX "too many IN values"); where no such leg exists it answers
   (`w8_in25_order_by_id_rows`, rows).

   v4 states this in the target's terms, names where the fact comes from, and moves it
   out of the in-union rule. v3 had a per-leg check in the in-union rule; two gates showed
   that was a Go-only mechanism in a different rule from the target's.

   **In the target's terms.** Under a requested ordering, the target's data-access rule
   does not produce a match whose MATCH ordering reaches a requested part only past a
   record-type placeholder that carries no comparison
   (`AbstractDataAccessRule.java:779-847`). So the target's reference holds no such
   plan, and the in-union rule never sees one (`w8_iup_no_order`: the same reference
   holds the I1 plans under PRESERVE, where nothing is requested).

   **The carrier.** Go's extension lives in the same place, the data-access rule's use of
   `ValueIndexScanMatchCandidate.ComputeMatchedOrderingParts`
   (`match_candidate_index.go:807`). There, each matched ordering part the walk reaches
   only by passing a record-type placeholder with no bound comparison is marked
   `pastRecordTypeHorizon`. The mark travels on the `PropRichOrdering` of the plan the
   rule builds, part by part.
   - Plan properties (sort elision, merge keys, the stress query's ORDER BY PK) read the
     parts as they do today, so the read extension of item 1 is unchanged.
   - The one consumer that must see the target's reference is the in-union rule's
     satisfaction check. It treats a marked part as unsatisfying a REQUESTED part, as the
     target's match would have stopped before it. A marked part may still be a free
     comparison-key suffix past the request, as the target's are
     (`COMPARE BY (_.COL1, _.ID)`).
   - An index that names `id` in its own key (the I4 shape, `(col1, id)`) reaches `id`
     through an index key part, not past the placeholder. It is unmarked and stays
     admitted. Pinned: `ORDER BY id` over I4 with `col1 IN (10, 20)` plans the in-union
     over I4 in both engines.

   **Other leg shapes.** A fetch, filter or map over an index scan inherits the mark with
   the ordering (they pass `PropRichOrdering` through). An intersection is different:
   the target's intersection ordering treats the record-type part as equality-bound
   (`AbstractDataAccessRule.java:1060-1093`), so it may build an ID-ordered in-union
   over an intersection leg. That is a HYPOTHESIS, measured by the oracle probe
   `w8_iup_intersection_order_by_id` (`col1 IN (10, 20) AND col2 = 3 ORDER BY id`) in
   IN-UNION-PARTITIONS mode, beside Go's plan for the same query. Whether Go's
   intersection route carries the mark (it would when its ordering is built from the legs'
   match orderings, and not when it is built from their scan comparisons) is not yet read
   from Go's source either. The probe and the W6 step 1 observer settle both engines'
   routes. If they differ, the mark follows the target's intersection rule, and that
   rule becomes a pin. That inconsistency inside the target is also the strongest
   case for the read extension. The DIVERGENCES.md entry cites it, states the extension's
   single-type precondition (a table whose primary key begins with the record-type key,
   read through a single-type index), and records that reporting it upstream is not
   authorized from here (owner item, section 14).

   **Go's plans for the two affected EXPLAIN rows** (`w8_in_union_explain`,
   `w8_in25_order_by_id_explain`). This is a HYPOTHESIS from the rules, measured at the
   steps. After F-7b under PREFER_SCAN, no in-union over I1 satisfies `ID`. The primary
   scan does, with a filter, which is the target's `SCAN | FILTER`. After F-7c, Go's rank
   penalises the lone primary scan against every other plan, while the target's rung
   abstains on this pair, so Go may prefer an in-join over I1 under a sort. F-7c's
   golden classification measures it. If the final plan differs from the target's, it
   is part of 4.2 item 3's declared divergence, which covers the FINAL plan and not only
   the pairwise verdict (item 3), with these two rows named.

   **Pins.** For T1, `ORDER BY id` with 2 and with 25 values plans no in-union and
   returns rows. `ORDER BY col1` with 25 values fails as the target does. The I4 shape
   keeps its in-union. A record-layer type without a record-type key still gets the
   in-union ordered by its primary key (the `w8_rl_*` shape, whose Go rows are new in
   v4's oracle, item 7).
3. The partition is memoized WHOLE, as the target does (`call.memoizeMemberPlansFromOther(
   innerReference, planPartition.getPlans())`, `ImplementInUnionRule.java:205`). Go
   pre-selects one member with the rule-level cost model and pins it
   (`memberSatisfiesOrdering`, `pinOrderedSpine`, `FinalOf(pinned)`), which is why Go's
   ORDER BY col1 plan is `InUnion(IndexScan(I1, [=]))` where the target's is
   `FETCH(INUNION(COVERING(I1 [EQUALS q0])))`: the target's partition held
   `COVERING(I1 [EQUALS q]) | FETCH` beside `ISCAN(I1 [EQUALS q])`, and its fetch
   push-through rule (`PushSetOperationThroughFetchRule`) built the winner from the partition's
   covering member, which Go's pinned single member never offers (Go's
   `rule_push_set_operation_through_fetch.go` fires only when the pinned member is a
   fetch). The in-union ranges over a reference restricted to the partition's plans,
   its comparison keys baked as today. The memoizer is named here, where v3 named the
   wrong one. Java's rule calls `memoizeMemberPlansFromOther`
   (`CascadesRuleCall.java:518-523`), which, like `memoizeFinalExpressionsFromOther`,
   builds the reference with `newReferenceFromFinalMembers` and marks it explored
   (`Reference.java:587-594`). Go's `MemoizeMemberPlansFromOther` exists only on
   `ExpressionRuleCall` (`expression_rule_call.go:309-324`) and copies no requested-ordering
   constraint. The in-union rule is an `ImplementationRuleCall`
   (`rule_implement_in_union.go:157`), and its `MemoizeFinalExpressionsFromOther`
   (`implementation_rule.go:187-223`) does copy the constraint, from the source to the
   restricted reference, which Java needs no copy for because its partition reference is
   ordering-homogeneous. Decision: the in-union rule memoizes the whole partition with
   `ImplementationRuleCall.MemoizeFinalExpressionsFromOther`, which carries the
   constraint and, under D7, marks the reference explored as Java's does. So no rule
   re-runs on the copy, and its `OptimizeGroupTask` keeps, per requested ordering, the
   cheapest member satisfying the ordering the constraint records. That constraint is
   what the pin stood in for. The measured failure at `expression_partition.go:207-217`
   ("an InUnion claiming ASC over a filtered full scan") was taken when the memoizer
   copied NO constraint; that comment is stale since the copy landed, and is corrected
   with F-7b (2.8 lists it). The extraction check replaces the pin with an invariant,
   not a choice: when the plan is extracted, an in-union's child winner must provide an
   ordering satisfying the in-union's comparison keys (`PropRichOrdering`, the property
   the partition was rolled up by, so every member of a partition satisfies it by
   construction). A child that does not is an internal error returned from planning
   (`InUnionChildOrderingError`, carrying the in-union's keys and the child's ordering),
   never a decline and never a silently accepted plan. Pinned: the "ASC over a filtered
   full scan" shape (the harness query of that comment, `ORDER BY col1 ASC` over an
   IN-list with a residual filter on a non-indexed column) plans an in-union whose child
   provides the ordering, and a unit test hands the check a child with the wrong ordering
   and asserts the error. The no-ORDER-BY
   fetch placement (`INJOIN{ISCAN}` in the target, `Fetch(InJoin(COVERING))` in Go) is an
   in-join, not an in-union. v3 left its cause unmeasured; v4 measured it with ROOT-PAIRS
   (`w8_root_in_no_order_*`). The target's root reference holds nine members and none of
   them is a fetch over an in-join over a covering scan. For the equality-bound `SELECT *`
   its data access yields `ISCAN(I1 [EQUALS q])` and no `COVERING(I1 [EQUALS q]) | FETCH`,
   so its `PushInJoinThroughFetchRule`, which Go also has, finds no fetch to push. Go's
   data access yields the covering-plus-fetch form beside the index scan, its push-through
   builds `Fetch(InJoin(COVERING))`, and its covering-favouring rank picks it. So the
   cause is covering emission. W7 moves covering emission under the target's gate at F-6,
   and F-6 owns the row. The DESC tie row (`w8_root_in_order_by_col1_desc_*`) has two
   causes:
   - The target's winner is the SORTED DESC in-join over the covering scan, with no fetch
     because the query is covered. The fetch is F-6's, as above.
   - The target orders by the in-join's own sorted IN list. Go's in-join rule has a
     reverse arm (`rule_implement_in_join.go:353-361`), yet Go plans `InMemorySort` over an
     ascending in-join. That cause is NOT located at this tree. F-7b owns it and locates
     it with the W6 step 1 observer before it changes anything.
4. Go's unordered in-union arm (`richOrdering == nil`, which hard-codes size 0) has no
   target counterpart (the target's rule skips a PRESERVE ordering) and is deleted.
5. The size check follows the target's `RecordQueryInUnionPlan.executePlan`
   (`:151-162`): FIRST, before the empty and single-value fast paths (Go's
   `executeInUnion` has three earlier returns, `executor_new_plans.go`, which the check
   precedes), more bound values than the plan's size fails with `RecordCoreError` "too
   many IN values" carrying the size, SQLSTATE XXXXX as the target's `ExceptionUtil`
   maps it (through section 7's new record-core arm in `translateFDBError`, which F-7b
   lands). The size is the plan's, carried from the configuration; the count is the
   PRODUCT over every IN source of the execution, as Java counts it
   (`RecordQueryInUnionPlan.java:331-337`, the product of each source's value count; v3
   said only "the execution's values", which is wrong once WS-E adds two-source
   in-unions), pinned with a 5×5 query (25 combinations, over the limit of 24, fails
   where each list alone is under it) and a 4×6 one (24, at the limit, answers). The
   count is also the EXECUTION's: Go's in-union executes only sources known at planning today, and when
   WS-E section 4 adds bound arrays (`IN ?`) the check counts each execution's bound
   array, never the size seen when the plan was cached (the `w8_in25_param_*` rows are
   owned by WS-E section 4 in the acceptance map). A single value under size 0 fails, as
   the target's check does. The size reaches every SQL planning path at F-7b, including
   `plannerOptionsFrom`'s early return for nil options (`planner_options.go:75-78`),
   which would otherwise keep size 0 until F-7c; F-7b sets the in-union size on both
   branches, and a test plans with nil options and asserts the size.
6. The size-less constructors go. `NewRecordQueryInUnionPlan` and
   `NewRecordQueryInUnionPlanWithBindingAliases` hard-code 0 (`in_union.go`), and have no
   non-test caller; the target's factory requires the size. They are deleted, their
   test callers pass the size they mean, and the rule's nil-context fallback to 0
   (`rule_implement_in_union.go`, the `call.Context != nil` check) becomes a failed call
   (a rule call without a planner context is a harness defect, as the target's rule call
   always has one). The in_union.go comment on excluding the size from identity is
   rewritten: the size now decides execution, and stays out of identity because it is a
   configuration constant for every plan of one planner run. The fetch push-through's
   rebuild already carries the size (`rule_push_set_operation_through_fetch.go`) and is
   pinned to keep doing so.
7. Record-layer callers. MEASURED on the target's RECORD layer (a new Java step,
   `wsfRecordLayerIn`, a `RecordQuery` through its `CascadesPlanner`, not SQL; probes
   `w8_rl_*`): under the record layer's default configuration (PREFER_SCAN, in-union size
   0) `price IN (10, 20)` sorted by the primary key plans `INUNION` over the price index
   and FAILS at execution with "too many IN values"; sorted by price, unsorted, or with
   one value it plans an `INJOIN` and answers; under the relational configuration the
   sorted-by-price query plans the same in-join. So a record-layer caller of the target
   that meets an in-union under the default configuration fails, and after F-7b a Go
   record-layer caller that builds or plans an in-union under size 0 fails the same way.
   That is a user-visible change for Go record-layer API callers (Go's in-union ran at
   any size before): it has a CHANGELOG entry under F-7b naming the configuration field
   to set (`AttemptFailedInJoinAsUnionMaxSize`), and it is an owner report item (section
   14).
8. The IN-subquery. MEASURED: both engines refuse `col1 IN (SELECT …)` with 0AF00 (the
   target "IN predicate does not support nested SELECT", Go "Cascades planner could not
   plan query"; `w8_in_subquery_*`), so no IN-subquery plan reaches the in-union rule and
   no guard is needed; v2's "keeps its in-join plan" described a plan that does not
   exist. The message difference is WS-E's declared row ([subquery_order]). The
   preserved "Go-only IN-subquery read extension" of the owner's list is not present at
   the base (`71ccd8cf8`): Go refuses every shape (`subquery_in.yaml`,
   `in_subquery_decomposition.yaml`), so nothing is removed and no in-union guard protects
   it. The `w8_in_subquery_*` rows, SAME-STATE, are the tripwire. If the extension is
   re-added they move, and this item's size question re-opens for its plans (the umbrella
   RFC's WS-E section records the same; owner item, section 14).
9. The IN rule's matcher. MEASURED (v4, the Go record-layer rows): the target's
   `InComparisonToExplodeRule` matches a `SelectExpression` holding the IN comparison
   (`InComparisonToExplodeRule.java:126-131`, `RelationalExpressionMatchers.selectExpression`);
   Go's matches a `LogicalFilterExpression` (`rule_in_to_explode.go:57`). Go's SQL
   translator happens to emit logical filters, so SQL gets its in-joins and in-unions. A
   record-layer caller that builds the target's graph (a type filter over the scan, a
   select holding the IN comparison, a sort) gets NO IN plan in Go:
   - sorted by the price index's column, a covering full index scan with the IN list as
     a residual;
   - sorted by the primary key, the filtered scan under an in-memory sort;
   - where the target builds in-joins, or an in-union that fails at size 0, Go answers
     from a scan.
   The in-union size check of item 5 would therefore never reach such a caller.
   Decision: Go's rule matches the target's shape, a `SelectExpression` with an IN
   comparison over a for-each quantifier, as F-7b's first change, before the size check.
   Go's logical filter over an IN list goes through the same path, since REWRITING
   already folds a filter into a select where the target does (the SelectMerge port of
   D11). If a logical filter survives to PLANNING, that is a W6 census finding, not a
   second matcher. The five `w8_rl_*` rows are accepted SAME-RL at F-7b: the same access
   path, in-union size, and ids or "too many IN values" failure as the target's record
   layer, under both configurations. The primary-key-sorted default row is the one that
   must fail as the target's does.
10. The in-union's size is a product, and Go nests. MEASURED (v4, `w8_in5x5_*`,
   `w8_in4x6_*`): for two IN lists over one index the target builds ONE in-union over both
   sources (`[IN … ⋈ IN …] INUNION q0, q1`), and the 5×5 query fails "too many IN values"
   (the product, 25). Go builds two NESTED one-source in-unions
   (`InUnion(InUnion(IndexScan(I3, [=, =])))`), and each would count 5. Decision: the
   in-union rule takes every explode source of the reference, as the target's does (its
   `inSources`), so one in-union carries the product. WS-E section 4 owns multi-source IN
   planning (its two-source in-union); F-7b's size check counts the product of whatever
   sources the plan carries. Both rows are owned by "WS-E section 4 and F-7b" in the
   acceptance map, and the 5×5 row must fail as the target's does.
11. The two remaining 4.3 item 2 probes. MEASURED (v4):
   - An index that names `id` in its own key (I8) is admitted by the target, which builds
     the in-union COMPARE BY `(_.ID, _.COL1)` (`w8_explicit_id_*`). Go's in-union over I8
     differs from it only in the fetch placement, which item 3 moves.
   - For `col1 IN (10, 20) AND col2 = 3 ORDER BY id` the target builds no intersection
     (`SCAN | FILTER`); its in-union rule's reference holds one final
     (`w8_iup_intersection_order_by_id`). Go plans `PredicatesFilter(IndexScan(I2, [=]))`,
     ordered by id past the record-type position: item 1's declared extension, accepted
     DIFF-PATH `record-type-horizon`.

   v3's intersection hypothesis therefore does not arise for this query. It stays a
   hypothesis for a query where the target does build an intersection, and the W6
   census classifies any such plan.

The IN-list work lands as F-7b, under today's PREFER_SCAN, so its golden flips have the
in-union as their one cause.

## 5. W9 — null-safe equality scans

Cause (SOURCE). Go's index-match gate `isSargableComparisonForMatch`
(`match_max_match_map.go:76`) admits `isScanRangeCompatible` types (=, <, <=, >, >=,
STARTS_WITH), IS NULL, IS NOT NULL and the distance-rank bounds, and not
NOT_DISTINCT_FROM. So a null-safe equality never binds a candidate placeholder and
always stays a residual, although `ComparisonRange.Merge` already classifies it as an
exact key and the executor already binds it. That is the target's pre-#4598 behaviour.

Changes:
1. `isSargableComparisonForMatch` admits NOT_DISTINCT_FROM, beside IS NULL. The
   distance-rank exclusion argument in `isScanRangeEqualityType` is untouched.
   IS DISTINCT FROM stays out of the gate. The target admits it into
   `canBeUsedInScanPrefix` (`RangeConstraints.java:756-787`, called from every
   `SelectExpression` constructor), but as a DEFERRED comparison that never bounds a
   scan: MEASURED, on the two-column index I3 `col1 = 10 AND col2 IS DISTINCT FROM 1`
   binds only `col1` and leaves the second column residual
   (`w9_multi_distinct_explain`), exactly what excluding it from Go's gate produces.
   The pin is that probe.
2. The comment on `isScanRangeEqualityType` ("NOT_DISTINCT_FROM is added here and is
   NOT a Java case") becomes false at this target and is rewritten: the target's
   EQUALITY arm is now {EQUALS, IS_NULL, NOT_DISTINCT_FROM, DISTANCE_RANK_EQUALS}, and
   Go's arm differs only by omitting DISTANCE_RANK_EQUALS, for the reason already
   documented there.
3. Sparse indexes. Go builds candidates for sparse value indexes
   (`cascades_generator.go`), so admitting NOT_DISTINCT_FROM adds an implication case:
   whether `col2 IS NOT DISTINCT FROM <comparand>` implies the index predicate `col2 IS
   NOT NULL`. MEASURED on the target: I4 is not used for the NULL form (`SCAN | FILTER`),
   is used for `… FROM 3` (`ISCAN(I4 [NOT_DISTINCT_FROM promote(@c10 AS LONG)])`, where the
   literal is the extracted constant `@c10`), and is used for a PREPARED `?` bound to 3
   (`COVERING(I4 [NOT_DISTINCT_FROM promote(@c10 AS LONG)])`,
   `w9_sparse_not_distinct_value_param_explain`); and one statement text executed on ONE
   connection with 3 then NULL, NULL then 3, re-bound or re-prepared, returns the right
   rows every time (`w9_sparse_param_*`, a new sequence step on both engines). The target
   is safe because its match captures the candidate's predicate as a plan constraint
   (`PredicateWithValueAndRanges.java:350-380`, `captureConstraint`), so its plan cache
   reuses the I4 plan only for a binding that satisfies it. Go's plan cache has no
   QueryPlanConstraint: it keys a statement on its literal text (`query_hash.go`, whose
   comment names the same hazard for literal-bearing index keys). v3 concluded that a
   `?` is ONE key for every binding. MEASURED (v4), that is false at this tree: Go
   substitutes a statement's bindings into its text before planning (the embedded
   `substituteParams` path, which the umbrella's WS-E section already names as the
   dependency real binding replaces). So each distinct binding is its own cache key.
   After a different binding, every Go execution is a plan-cache MISS; the same binding
   twice is a HIT (`w9_sparse_param_*`, each Go element carrying its pinned cache
   event). And the planner sees a bound `?` as the literal it was substituted with.
   Decision: NOT_DISTINCT_FROM implies the sparse predicate exactly when the planner
   sees a non-null LITERAL comparand (a different literal is a different key, so the
   plan cannot be served to a NULL). While Go substitutes bindings, a bound `?` IS such
   a literal: the prepared form bound to 3 plans I4 and bound to NULL plans the scan, as
   the target does, and `w9_sparse_not_distinct_value_param_explain` is accepted
   SAME-PATH at F-1. When WS-E section 4 lands real parameter binding, the comparand
   becomes a `ParameterValue` shared by one cache key across bindings. From then on, it
   implies nothing until WS-H ports the target's plan-constraint identity (below). That
   change moves the row to DIFF-PATH `sparse-param-plan-constraint`, and WS-E's step
   must update its acceptance entry, which is why the reason class stays declared. "Literal" is defined
   through the Values the translator builds (v4): a `LiteralValue`, possibly under
   `PromoteValue` (an int literal promoted to the column's BIGINT) or under a `CAST` whose
   operand is itself such a literal; "non-null" means its value is not NULL after that
   promotion or cast. So `CAST(NULL AS BIGINT)` is a literal whose value is NULL, and it
   implies nothing, pinned with `col IS NOT DISTINCT FROM CAST(NULL AS BIGINT)` beside
   the bare `NULL` and `CAST(3 AS BIGINT)` (MEASURED: the target scans for the first and
   uses I4 for the second, `w9_sparse_not_distinct_cast_*`). A real `ParameterValue`
   comparand, once WS-E binds for real, implies nothing: Go then plans the scan with the
   residual for the prepared form where the target plans I4. That plan difference (same
   rows) is declared in DIVERGENCES.md, pinned by a yamsql plan pin in WS-E's step, and
   closes when WS-H ports the target's plan-constraint identity for the cache (the
   umbrella's WS-H "retain canonical query/temp-function/plan-constraint identity"). At
   that point the `?` form follows the target again and the pin moves. The sequence rows
   are Go pins with rows and cache events. A Go change that made a `?` plan depend on an
   earlier binding shows as a HIT across different bindings, and reddens them.
4. The range builder (`RangeConstraintsBuilder.AddComparisonMaybe`,
   `range_constraints.go:154-165`) classifies compile-time by correlation only. The
   target's test is `isCompileTime`: a record-type comparison or
   `IndexComparison.isSupported` (a simple or null comparison, or a value comparison
   whose comparand tree consists only of `RangeMatchableValue`s), and a type in the
   eight allowed types (`RangeConstraints.java:726-760`). Go ports that test: a
   `values.IsRangeMatchable` predicate true for exactly the Go counterparts of the
   target's SEVEN implementers — literal, constant object, promote, cast, of-type,
   evaluates-to, index entry object (v1 omitted promote, which every SQL literal
   comparand is wrapped in) — and the allowed-type set. IS DISTINCT FROM, STARTS_WITH
   and the distance-rank types are then deferred, never compilable. This is latent:
   `IsCompileTime()` has no production consumer today (`grep -rn '\.IsCompileTime()'
   pkg --include='*.go' | grep -v _test.go` finds only the recursive call inside
   `PredicateWithValueAndRanges.IsCompileTime` itself), so no SQL query can observe
   it; it is pinned by unit tests over every comparison type and every comparand kind,
   and becomes observable when WS-J F10's filtered-index work consumes it.
5. Stale claims corrected: `comparison_range.go:266` ("NOT a Java case — Java's
   switch has no arm for it"), `scan_range_classification_test.go:38` ("Go-only
   addition; Java has no NOT_DISTINCT_FROM arm at all") and `:161` ("Go-only exact
   key"), and TODO.md CQ-35's statement that `Merge` does not classify IS NOT DISTINCT
   FROM as equality. Closure is `grep -rn -i -E 'NOT a Java case|no NOT_DISTINCT_FROM
   arm|Go-only exact key' pkg TODO.md DIVERGENCES.md` returning zero, with
   `isScanRangeEqualityType` in the same grep's population as the positive control.

Tests: the oracle's W9 explain probes become Go yamsql pins in
`distinct_from_java.yaml` (`plan_contains` the index scan for `IS NOT DISTINCT FROM
NULL` and `… 20`, the two-column I3 scan bound by NOT_DISTINCT_FROM NULL; for the
conjunction, the index scan bound by NOT_DISTINCT_FROM with `> 5` residual; for IS
DISTINCT FROM, the residual, whose access path is decided by 4.2 and so is pinned after
F-7c), each with rows; all `MergeComparisonRangesTest` cases; builder bucket tests for
every comparison type; the target's `scoped-keyset-pagination.yamsql` shape (a
leading NOT_DISTINCT_FROM parameter and an OR range on trailing keys) with bound
parameters, which is the shape #4598 fixed. A mutation removing NOT_DISTINCT_FROM from
the gate must redden the yamsql pins, and one treating NOT_DISTINCT_FROM NULL as
implying the sparse predicate must redden the sparse-index test.

## 6. W10 — enum comparisons

MEASURED: the target answers every direct-table enum comparison of
`enum-distinct-from-function.yamsql`'s table (`=`, `<>`, IS [NOT] DISTINCT FROM against
a literal, an OR, `IS NOT DISTINCT FROM NULL`), plans `= 'HAPPY'` and `IS NOT DISTINCT
FROM 'HAPPY'` as index scans over `promote(… AS ENUM<…>)`, and rejects an unknown
literal with XX000 "Invalid enum value for the enum type ANGRY". Go rejects the schema
(0A000, `ddl.go:300-302`).

Decision: W10's Go change is regression pins only (Go's comparison already keeps its
operand Value, `comparisons.go`, so the target's descriptor workaround has no Go
analogue); the pins need enum DDL, which WS-J F6 lands (`ws-j-design.md` section 5).
When F6 lands, the ten oracle rows become Go yamsql tests with rows and
`plan_contains`, and the unknown-literal row is pinned to the target's class and
wording. The index-predicate half of #4624 (an enum comparison inside `CREATE INDEX …
WHERE`) is WS-J F10, which the target itself cannot plan. W10's checkbox closes with
F6.

## 7. W11 — DML refuses snapshot isolation

The target's `QueryPlanUtils.enforceSerializable` throws RecordCoreArgumentException
"Cannot execute plan at SNAPSHOT isolation level" with log info PLAN = the plan class's
simple name (`QueryPlanUtils.java:52-58`), before the child is opened, dry runs
included. WS-E section 6.6 already specifies the executor guard and its tests. It is
implemented once, there. The error type is decided here and WS-E 6.6 is edited to
match (at this tree WS-E 6.6 already names this type): Go's existing `RecordCoreArgumentError`
(`rank_scan.go:19-27`), the Go form of the same Java class, gains `Plan string` (the
plan's type name, the counterpart of the target's simple class name). `Error()` keeps
today's rendering whenever `Plan` is empty, so the seven existing construction sites
(`rank_scan.go`, `pending_writes_queue.go`, `split_helper.go`; `grep -rn
'RecordCoreArgumentError{' pkg --include='*.go' | grep -v _test`) print exactly what
they print today, and renders `<message> (plan=<Plan>)` when it is set. Its SQLSTATE
is the target's: the target's relational layer maps a `RecordCoreException` with no arm
of its own to `ErrorCode.UNKNOWN`, XXXXX (`ExceptionUtil.recordCoreToRelationalException`,
which starts from UNKNOWN and has no arm for `RecordCoreArgumentException`), and a unit test on
Go's SQL error mapping pins XXXXX for it. No admitted SQL statement runs DML at
SNAPSHOT, so the mapping is pinned directly rather than through SQL. The mapping does not
exist today (v4; the storage gate found it): `translateFDBError`
(`connection.go:1010-1062`) has no arm for `RecordCoreError` or `RecordCoreArgumentError`,
so such an error leaves unmapped and the oracle renders it with no SQLSTATE. Decision:
every Go error type that ports a `RecordCoreException` subclass declares it with a marker
method, and `translateFDBError` gains the target's default after its specific arms (the
metadata, uniqueness, not-exists and deserialization arms, which keep their codes): a
marked error becomes `ErrCodeUnknown`, XXXXX, as
`ExceptionUtil.recordCoreToRelationalException` makes it (`ExceptionUtil.java:59-84`).
The population is every Go error type whose doc names a Java `RecordCoreException`
subclass, enumerated with its command at the step (`KeyExpressionError`,
`QueryInvalidExpressionError`, `RecordCoreError`, `RecordCoreArgumentError` among them). A
test drives the arm per type. It lands with F-7b, whose "too many IN values"
`RecordCoreError` is the first SQL-reachable one, and `w8_in25_order_by_col1_rows`
reaching SAME pins it end to end. The guard compares against
SERIALIZABLE exactly, because the isolation enum's zero value is SNAPSHOT
(`scan_properties.go:63`); `DefaultExecuteProperties` sets SERIALIZABLE (`:247`), so
the risk is a struct literal that omits the field. Before landing, a grep for every
`ExecuteProperties{` literal in production and test code (partial literals such as
`ExecuteProperties{ReturnedRowLimit: 5}` included), with `DefaultExecuteProperties` in
the same population as the positive control, lists each literal that reaches DML
execution and each is given SERIALIZABLE or shown not to reach DML.

## 8. W12 — zero-based EXPLODE ordinality

Target: an optional zero-based flag valid only with ordinality (constructor check),
part of equality and translation, a hash term added only when set (existing hashes
unchanged), positions assigned over the full list before resume and skip/limit, explain
unchanged, proto field 3 set only when true, and distinct-records true exactly when
ordinality is on. No SQL surface for the zero base: `AT` stays one-based and no
production code builds a zero-based explode.

Go: checked constructors returning an error; the flag carried through every rebuild
site (`rule_implement_explode.go`, `rule_implement_nested_loop_join.go`,
`rule_select_merge.go`); hash and structural key gain a component only when set;
explain unchanged; the executor's two hard-coded bases (the scalar case and `i+1`)
take the flag; the proto-schema guard covers field 3. The distinct-records arm
(`case *plans.RecordQueryExplodePlan: return p.IsWithOrdinality()` in
`plan_properties.go`) is NOT zero-base-only: it applies to every ordinality explode,
including the one-based SQL `AT` form, so it can remove a DISTINCT above an `AT`
unnest. That makes this a plan-changing step (section 12): it takes the golden
classification, and an FDB test runs `SELECT DISTINCT` over an `AT` unnest with
duplicate array elements with and without the removal and compares rows. Tests: the
applicable `ExplodePlanTest` cases, a real-FDB test that plans a hand-built zero-based
explode over a record's array and resumes it page by page without renumbering, and a
plan pin that an ordinality explode lets a DISTINCT above it be removed.

## 9. W13 — explain, display names, and the surfaces they need

### 9.1 Array subscript

MEASURED (0.1): the target supports `arr[i]` in projections and predicates; it is
1-based; out of range, 0, a NULL array and a NULL index give NULL; an unnamed
subscript column is `_0` and `AS x` names it `X`; explain renders
`_.ARR[promote(@c10 AS INT)]`. A BIGINT index fails at evaluation with XXXXX
ClassCastException (over a query that returns no row it answers zero rows, no error); a
bound parameter is typed by its binding — `setInt(2)` returns 8, `setLong(2)` fails with
the same ClassCastException; a STRING index fails at planning with 22000; a non-array
operand fails at planning with XX000 VerifyException. Go rejects every form.

SOURCE: `ExpressionVisitor.visitSubscriptExpression` (`:796-801`) resolves the function
`[]` with arguments (index, base); `SqlFunctionCatalogImpl.java:125` maps it to the
built-in `subscript`; `SubscriptValueFn.encapsulate` (`SubscriptValue.java:189-202`)
promotes the index to `maximumType(indexType, INT)` (a null maximum is
INCOMPATIBLE_TYPE) and verifies that the source is an array; the value's children are
(index, source), its type is the element type made nullable (constructor, `:59-64`), and `eval`
casts the index to `int` (`:85-105`). The BIGINT failure is therefore a target defect:
`maximumType(LONG, INT)` is LONG, so no promotion to INT is injected, and the `(int)`
cast fails on the first row with a non-null index.

Go has `values.SubscriptValue` but nothing constructs it from SQL (no call of
`NewSubscriptValue` outside `values`), and the translator has no
`SubscriptExpressionContext` arm. Decisions:
- The expression walker gains the subscript arm and resolves it through Go's function
  catalog under the name `subscript`, ported from `encapsulate`: the index promoted to
  the maximum of its type and INT (22000 on no maximum, through Go's existing
  promotion error), a non-array source rejected at planning with XX000 and the
  target's VerifyException-class message.
- `SubscriptValue` is aligned: children (index, source); type = element type,
  nullable; `Evaluate` returns an error for a non-list source (it can no longer be
  reached from SQL) instead of NULL.
- The BIGINT index: "does not work in the target → does not work in Go, in the same
  way". Go fails at evaluation, on the first row with a non-null index, when the
  index's static type is not INT, with XXXXX and a message naming the types. A bound
  parameter's static type is its binding's: Go's parameter typing (WS-E section 4)
  types a Go `int32` as INT and an `int64` as BIGINT, so `arr[?]` with an `int32` works
  and with an `int64` fails, as the two measured bindings do; the
  message differs from the JVM's ClassCastException text and is listed with WS-E's
  other XXXXX wording differences in DIVERGENCES.md. An empty table therefore answers
  zero rows in both engines, and a test pins that too.
- Explain renders `SOURCE[INDEX]` with the closing bracket (#4302), in Go's explain
  syntax.
- Tests: every subscript row of the oracle as a Go yamsql or sqldriver pin with the
  column names; `cast-tests.yamsql`'s gap entry in `javacorpus/gaps.go` moves (the
  first failing query there is a subscript) and the ledger count is updated in the
  same change; a mutation of the 1-based adjustment reddens the pins.

### 9.2 Quoted dotted table names

MEASURED: `"foo.table$nested"` works in the target for SELECT, EXPLAIN SELECT, EXPLAIN
INSERT and INSERT. In Go, SELECT, EXPLAIN SELECT and EXPLAIN INSERT work
(`w13_display_insert_explain` is `OK EXPLAIN "Insert(foo.table$nested)"`; the scan
carrier already has typed segments, `LogicalScan.TablePath`, resolved by
`ResolveQualifiedTablePath` in `cascades_generator.go`), and only EXECUTING the INSERT
fails, with 42F00 "Unknown database foo" (`w13_display_insert`, and the setup INSERT of
`w13_display_rows`). v2 said EXPLAIN INSERT fails; the pin says otherwise. The split is
between two paths over one statement: EXPLAIN renders the logical insert without
qualifying its target, and execution runs the operator rewrite that re-qualifies every
DML target through the string splitter, `functions.ResolveQualifiedTableName(ins.Table,
schemaName)` (`cascades_generator.go`, the `*logical.LogicalInsert` arm of the op
rewrite beside the delete and update arms, line 7104 at this tree), which reads the
quoted dot as a database qualifier. UPDATE and DELETE go through the neighbouring arms of
the same rewrite and fail the same way (their pins land with this step).

SOURCE: the parser joins identifier segments with "." (`functions.FullIdToName`), and
several sites then re-split or case-fold the joined string, which cannot tell a quoted
dot from a qualifier. The population is every non-test line in `pkg/` that calls or
names `FullIdToName`: 26 lines in 7 files (`grep -rn FullIdToName pkg --include='*.go'
| grep -v _test`), of which the table and CTE references are the DML targets
(`logical_builder.go`, delete/insert/update), the two predicate table-name sites
(`logical_predicate.go`), the CTE name sites (`logical_builder.go`,
`logical_predicate.go`, `plan_visitor.go`), `subquery_walk.go` (which also upper-cases
the joined name, folding a quoted name's case), and the eight
`ResolveQualifiedTableName` callers (`cascades_generator.go` 4, three of them the DML
arms of the execution-path op rewrite named above, `logical_predicate.go` 2,
`select_parser.go` 2).

Decision: every table and CTE reference carries the identifier path captured from the
parse tree (`[]string`, one element per `uid`, with quoting's case preserved), as the
scan carrier already does; every resolution calls `ResolveQualifiedTablePath`, the
execution-path DML rewrite included, so EXPLAIN and execution resolve one statement's
target the same way; name comparisons compare paths, never joined strings;
`ResolveQualifiedTableName` is deleted. The column-reference lines of the population are audited in the same change,
each converted to segments or shown (in the step's evidence) not to re-split, compare
or fold the joined string. Tests: the INSERT and EXPLAIN INSERT oracle rows as Go pins
(rows read back, and the explain per 9.3); UPDATE and DELETE on the same table; a
cross-engine write/read pin in each direction (Go INSERT then target SELECT, and the
reverse, on one store), which is also the check that both engines store the same
record-type name for the quoted dotted table; `schema."foo.table$nested"` resolving;
`"schema.foo"` (one quoted segment) not being read as a qualifier; a quoted
mixed-case table name reached through a subquery. The TODO.md dotted-name family (the
block "NEW (round-8 review … DOTTED spellings)") shares this cause for its
quoted-dotted CTE cases; the block is updated in the same change to say which of its
cases the typed path closes, measured by running each named query, and the rest keep
their entry.

A second case-folding site, found by v4's oracle, has nothing to do with dots. MEASURED:
- `"footab"` with a quoted primary-key column `"id"` gets no primary-key scan in Go:
  `PredicatesFilter(Scan(footab))`, where the target plans
  `SCAN([IS footab, EQUALS …])` (`w13_quoted_pk_eq_*`).
- The unquoted `T1` with `id = 1` gets `Scan(T1, [=])` in Go (a planner-harness check
  at this tree).
- The dotted `w13_display_scan_explain` row is the same case: its key column is the
  quoted `"id"`.

SOURCE: the SQL layer's primary-scan candidate upper-cases the primary key's column
names (`cascades_generator.go`, `upperPK[i] = strings.ToUpper(col)` in the
`PrimaryScanMatchCandidate` registration, lines 2903-2907 at this tree). A quoted
lowercase column's physical name `id` becomes `ID`, which no query field matches. The
index candidates pass physical names through VERBATIM, as
`NewPlanContextFromIndexDefs`'s comment requires.

Decision: the primary candidate passes the physical names verbatim too, in F-1 with
the rest of 9.2. Both rows are owned by F-1 and accepted SAME-PATH. The population
check for this case is every `strings.ToUpper` or `EqualFold` over a column or field
name in the candidate builders (`cascades_generator.go`,
`pkg/recordlayer/query/plan/cascades/*candidate*.go`), each listed in the step's evidence
with its disposition.

### 9.3 Display names

The target prints user identifiers for record-type comparisons, type filters and DML
targets (`RecordTypeKeyComparison.java:260-263`, `ExplainPlanVisitor.java:473, 629,
699`; MEASURED in 0.1: `IS foo.table$nested`, `INSERT INTO foo.table$nested`). Go prints
user identifiers today only because its scan leaf still carries the SQL name (the
RFC-238 section 7c namespace gap: `Scan(foo.table$nested)` measured); no Go Explain
renderer decodes anything (`DecodeOnceIfReversible`'s only non-test callers are
`proto_names.go`, `protoname.go` and `cmd/frl/internal/cmd/stats.go:761`, found by
`git grep -n 'DecodeOnceIfReversible(' -- 'pkg/*.go' 'cmd/*.go' ':!*_test.go'` without the
definitions; v2 named `fleet/statistics.go`). When RFC-238 section 7c
moves the scan leaf to stored names, the `Explain()` of `Scan` (`scan.go:212`),
`TypeFilter` (`typefilter.go:105`), `Insert` (`insert.go:136`), `Update`
(`update.go:168`) and `Delete` (`delete.go:106`) must render record-type names through
`protoname.DecodeOnceIfReversible` (one name) and `SafeDecoderOver` (lists), or they
would start printing stored spellings the target no longer prints. That change lands
with section 7c, in the same commit, pinned by the oracle's display rows as Go yamsql
pins; this workstream records the obligation at the section-7c site (a comment in
`scan.go` pointing here) so it cannot be missed. Stored identities and metadata keys
never change. Plan identities DO change, for SQL-built plans over a table whose stored
name differs from its SQL spelling: the scan leaf's record-type name feeds the scan
salts' `record-types` field (`scan_range_execution_identity.go`, lines 348 and 445 at
this tree) and the structural key, and 7c moves that name from the SQL spelling to the
stored one. v2's "plan identities never change" was wrong. The change has no
continuation to break: a Go SQL statement's paging is private to one execution, a
caller-supplied continuation is refused (`cascades_generator.go`, the
`OptContinuation` check at the top of `cascadesPlan.Execute`), and the plan cache is per
connection (`newCascadesGenerator`, `cascades_generator.go:57-65`; v3 said per process); record-layer callers build their leaves from stored names already, so their
salts do not move. 7c's evidence pins both: a SQL plan over `"foo.table$nested"` changes
salt across the move, and a record-layer-built scan over the same stored name keeps
its salt byte for byte. The yamsql `intermingle_escaped_table_name.yaml` comment
that tells a future editor Java prints storage spellings and to switch its pins to
`MY__1TABLE` is false at this target and is rewritten now: the pins stay `MY$TABLE`.
The target's field display (`_.x$y` in a FILTER) has no Go counterpart, because Go's
explain prints a predicate count (`[1 preds]`), not predicates; that is Go's explain
syntax, not a W13 change, and is listed with the other explain-syntax differences in
section 13.
The covering-Value plan's explain is section 3.

## 10. W14 — bottom-up Value fold

`SimpleValueVisitor` (post-order; a pruned child's result is omitted, not replaced; a
shared subtree is visited once per occurrence; the plain entry point skips the root's
prune check). Its consumers are WS-J's port targets (`QuantifierValues`,
`ValueToKeyExpressionVisitor`). `WalkValue` stays; a generic `FoldValue` over
`Children()` lands in the same change as the first WS-J consumer, with the
`ValueVisitorTest` ports (order, pruning, repeated subtrees, type-specific override that
still recurses). No framework without a consumer.

## 11. W15

Verified by `git` (0.3): no execution port. `TempTable`'s private constructor and
`SyntheticRecordPlanner`'s `isDisabled()` are equivalent in Go's model.

## 12. Order, dependencies, review units and continuations

Phases (each a review unit: one Graefe/Torvalds/storage lap at its end, then delta
re-confirmation of the final head):

- **F-1** W9 (section 5) and W13 subscript and dotted names (9.1, 9.2); the 9.3
  obligation comment.
- **F-2** W12 (section 8).
- **F-3** W7 steps 1–6 and 6a.
- **F-4** W6 steps 1–6.
- **F-5** W6 step 7 (the census on the shipping configuration, then the prune as
  default).
- **F-6** W7 step 7 (covering emission under the reader gate).
- **F-7a** the rule-level comparators receive the planner's context (4.2 item 2):
  metadata-based criteria start applying inside rules, under today's configuration.
  One cause: the context.
- **F-7b** the IN-list port (4.3): in-union size 24 in the SQL configuration, the
  partition memoized whole, the unordered arm's deletion, the size-less constructors'
  deletion and the execution check, under PREFER_SCAN. One cause: the in-union.
- **F-7c** the rest of the relational configuration (4.1 core, 4.2 items 1 and 3):
  PREFER_INDEX, the fetch method, right-deep, the vector preference. One cause: the
  index preference.
- **F-8** the rule-internal winner pre-selections go (4.2 item 2): each rule with a
  target counterpart yields per partition and `OptimizeGroup` chooses. One cause per
  commit: the rule family.
- **Dependent closures:** W8's SQL test after WS-D's GuardiANN DDL and execution; W10
  pins with WS-J F6; W14 with WS-J's generator; W11 in WS-E 6.6; W7's serialization
  obligations in WS-G (its section lists the aggregate reader's field 8, plan tag 41,
  EXPLODE field 3, planner-configuration field 15, the in-union's size (field 5 of
  `PRecordQueryInUnionPlan`, `record_query_plan.proto:2162`, which now decides execution,
  so a serialized in-union without it would execute as size 0) and, added by this design, the
  index-entry leaf proto with an explicit `TupleSource` mapping, since Go's iota 0/1/2
  is not the proto's 1/2/3); W13 display with RFC-238 section 7c; Go array parameter
  binding (`IN ?` with a `[]int64`, refused by Go's driver today, `w8_in25_param_*`)
  in WS-E section 4.

Plan-changing steps are F-1 (the W9 gate), F-2 (the ordinality distinct-records arm),
F-3 step 6a (the aggregate distinct-records arm), F-5, F-6, F-7a, F-7b, F-7c and F-8. They
are separated so every golden flip has one cause; each takes the classified explain-diff and the 1M
stress comparison at the merge-base, both SHAs recorded. Every planner step runs the
determinism loop (ten uncached runs of the affected tests) before it closes.

Continuations across the plan-changing steps (v2's section was wrong in two ways, and
both are corrected here).

Where the exposure is. Go SQL has no continuation a caller holds across an upgrade: a
statement's paging is private to one execution, and a caller-supplied continuation is
refused (`cascades_generator.go`, the `OptContinuation` check at the top of
`cascadesPlan.Execute`). The exposure is the record layer: a caller that plans with the
exported `cascades.Planner` under the record-layer configuration, pages with the
exported `executor.ExecutePlan`, and holds a continuation across an upgrade. F-1, F-2,
F-3 6a, F-5, F-6, F-7a and the in-union changes of F-7b (section 4.3, which are not
configuration-gated) reach that caller. F-7a reaches it because a caller who plans with
`NewPlanner` and never calls `WithStatistics` ranks with the rule-level comparators'
context, which F-7a changes (`planner.go:253`, `expression_rule_call.go:107`,
`implementation_rule.go:115`; v3 left it out). F-7c's configuration is the SQL
planner's only. The premise that Go SQL holds no caller continuation expires when
RFC-203's G12a/G12b land (`cascades_generator.go:2456`): from then on, the table below
applies to SQL as well, and each step that lands after them re-runs its SQL rows.

What a continuation is bound to. A primary scan WITH scan comparisons, and every index
scan, runs through the range-set cursor, whose continuation carries a fingerprint of
the leaf's execution salt (`scan_range_execution_identity.go`); one minted by another
leaf is refused with `recordlayer.ContinuationParseError` ("scan range set: error
parsing continuation", `scan_range_set_cursor.go`, `parseScanRangeSetContinuation`). A
primary scan WITHOUT scan comparisons has no salt: the caller's bytes go to
`ScanRecordsByType`/`ScanRecords` as a key suffix unchecked (`executor.go`, the
`GetScanComparisons()` branch and the fall-through after it; `store.go`). Those bytes are
the target's record-scan continuation format, so salting them would break the
record-layer continuation compatibility with the target that Go keeps today. The
target's record layer validates no continuation against its plan either (its plan-hash
check lives in the relational continuation envelope). Decision: this is declared parity,
not a hazard Go fixes alone. A record-layer continuation is valid only for the plan that
minted it, in both engines; Go refuses a mismatch where the new outermost cursor is
salted and, like the target, does not detect one where it is an unsalted primary scan.
The CHANGELOG entry of each step below states which of its transitions are refused and
which are undetected, and tells record-layer callers to restart paged queries across an
upgrade that crosses the step.

The transitions, old plan to new. v4 reasons about the NEW plan's OUTERMOST decoder,
because that decides whether the old bytes are refused: v3 reasoned about the leaf and
was wrong twice. Two decoders matter:
- A flat-map (the in-join, `cursor_combinators.go:628-660`) decodes a
  `FlatMapContinuation`, bytes fields 1 and 2.
- A union (the in-union) decodes a `UnionContinuation`, also bytes fields 1 and 2
  (`record_cursor.proto:28-32, :49-59`).

So each parses the other's bytes cleanly. The in-join's outer, a
`FromListWithContinuation`, reads any first four bytes as a position and clamps it
(`cursor.go:352-374`): no check value, so no check. Java's flat-map and union use the
same protos, so what follows is parity in both engines, not a Go defect. A ONE-value
in-union is not a composite: it hands the continuation straight to its child (Java
`RecordQueryInUnionPlan.java:159-161`, Go `executor_new_plans.go:1866-1877`), so its row
is its child's row. Each row is pinned at its step by presenting the old plan's
continuation to the new plan through `executor.ExecutePlan` on real FDB, in both
directions where both are reachable. The pin records the outcome actually seen: refused,
or the rows returned, so a resume at a wrong position is visible, not assumed. The
CHANGELOG entry of each step states the outcomes.

| Step | Old → new; the new plan's outermost decoder | Outcome |
|---|---|---|
| F-1 (W9 gate) | unfiltered primary scan + residual → index scan `[NOT_DISTINCT_FROM]`; a salted range-set cursor (or a fetch passing to one) | refused (a raw key suffix does not parse as, or match the fingerprint of, a range-set continuation) |
| F-2 (W12) | Go-only DISTINCT over an ordinality explode (`DedupContinuation` or `DistinctHashContinuation`, `executor.go`) → the bare child; the child's decoder | refused when the child's decoder is a salted leaf; undetected when it is an unsalted primary scan (the wrapper's bytes read as a key suffix) or a composite whose fields happen to parse. The v3 plan to prefix the wrapper's bytes is withdrawn: continuations minted before the step carry no prefix, so it cannot guard the step's own transition. Requiring it would refuse every in-flight DISTINCT continuation of an unchanged plan, and accepting the legacy form would reject nothing. |
| F-3 6a (aggregate distinct-records) | DISTINCT over an aggregate index plan → the aggregate index plan; its range-set leaf | refused (salted) |
| F-5 (W6 prune) | any plan whose shape moves; the new outermost decoder | refused when it is a salted leaf (or a fetch or filter passing to one); undetected when it is an unsalted primary scan or a composite that parses the old bytes. The census records, for every changed plan of a record-layer-reachable shape, which of the two it is. |
| F-6 (W7 emission) | fetch over index scan ↔ covering index scan; the index scan, through the fetch | refused (both salted; the covering salt carries `covering` and its columns) |
| F-7a (rule-level context) | any record-layer plan whose rule-internal choice moves; the new outermost decoder | the same rule as F-5 |
| F-8 (per-partition yields) | any record-layer plan whose rule-internal choice moves; the new outermost decoder | the same rule as F-5 |
| F-7b (IN lists) | in-union → `SCAN \| FILTER`; an unsalted primary scan | undetected (declared parity): the `UnionContinuation` bytes are read as a key suffix. The pin shows the rows returned. |
| F-7b | in-union → in-join over the same index; the flat-map | undetected (declared parity): the union's first child continuation becomes the outer's position, its second the inner's. The pin records the rows (a resume at a wrong position shows as missing or repeated rows). |
| F-7b | in-join → in-union over the same index; the union | measured at the step: the flat-map's outer position becomes the first leg's continuation, which a salted index scan refuses; if a leg is not salted, undetected |
| F-7b | one-value in-union ↔ its child | the child's own row (no composite) |
| F-7c (PREFER_INDEX, SQL only) | none reachable by a caller until RFC-203 G12 (SQL continuations are engine-private) | — |

Every plan-changing step runs this table's pins in both directions where both
directions are reachable (an upgrade crosses a step forward only, but a downgrade crosses
it back, and the pins show both). F-7a gets a CHANGELOG entry of its own: record-layer
callers who plan without statistics may get other plans (the metadata-based criteria now
apply inside rules), with the continuation outcome of F-5's rule.

## 13. Acceptance

- The acceptance is code, not reading. The oracle carries `wsfAcceptance`, every probe's
  verdict at acceptance, and `wsfOpenUntil`, the phase or dependency that owns each probe
  not yet there. The spec fails when:
  - a probe has no acceptance entry;
  - a probe is away from its entry and no phase owns it;
  - a probe owned by a phase is already at its entry (the ratchet: a phase closes by
    deleting its entries, and cannot forget one).

  The verdicts (v4; v3's design named two and its code used three, which Torvalds found)
  are:
  - **SAME**: the comparator of 0.1, everything but the target's exception class, and
    for Go sequence elements everything but the pinned plan-cache event;
  - **SAME-STATE**: the error's SQLSTATE, or a non-error answer whole, for the rows whose
    message is declared different (the two BIGINT-subscript rows, the JVM's
    ClassCastException text; the IN-subquery rows, WS-E's [subquery_order]);
  - **SAME-PATH**: both engines explain one access path (`wsfAccessPath`, 0.1);
  - **SAME-CLASS**: one access path up to index names, for a row whose index choice only
    a plan hash decides (`w8_no_predicate_explain`, whose two index scans tie on every
    rung down to the hash, unmatched fields 3 against 3, the names never moving it);
  - **DIFF-PATH REASON -> PATH**: Go explains PATH, the target another path, and REASON
    is one of the declared access-path divergences (`wsfDeclaredPathReasons`: the
    record-type-horizon read extension, 4.3 item 1; the RFC-152 materialized outer join,
    2.3; the real-parameter sparse rule, 5 item 3; the covering rank's final plan, 4.2
    item 3), each also in DIVERGENCES.md;
  - **SAME-RL**: a record-layer row whose access path, in-union size, and ids or "too
    many IN values" failure agree (`wsfRLComparable`);
  - **DIFF**: the target-only measurements (the rule traces, the in-union partition
    traces, the root-pair traces and their summaries, the REWRITING results), whose Go
    column is a fixed "no instrument" marker until W6 step 1's observer gives them one.

  An EXPLAIN row that both engines answer with a plan may not be accepted by SAME, DIFF
  or SAME-STATE (the spec asserts it). So "explain syntax" is never a reason, and every
  plan-changing phase owns the rows it must move: F-1 the W9 and W13 rows, F-6 the fetch
  placements, F-7b the IN rows, F-7c the preference rows. The rows owned by F-5, F-7a and
  F-8 are those their census, explain-diff and continuation pins classify; none of the
  155 probes is theirs. WS-F is accepted when `wsfOpenUntil` holds only entries owned
  outside WS-F (WS-J F6 for the W10 rows, WS-E section 4 for the bound-array rows and the
  two-source in-union rows, RFC-238 section 7c for the display rows), each named in
  section 12's dependent closures.
- Every probe's DIFF-PATH names the Go path, so a phase that moves a declared row reddens
  it and must update the entry deliberately; F-7c, which can move the preserved outer
  join's outer leg, is the expected case.
- The rule traces are not compared rule by rule across the two memos. They are evidence
  for the decisions of sections 2.3, 4.2 and 4.3, and the Go counterparts of the
  in-union and REWRITING measurements are W6 step 1's observer pins. Each yamsql plan pin
  a phase lands is the Go side of an oracle row's access path at that phase, and names
  the row.
- W6: every contract of the audit (no-progress fallback, stop after progress, disabled
  members, stale expressions and dependencies, requirement-caused progress, post-prune
  ordering, 2×2 → four finals, multi-leg pushdown with residuals, mergeability from
  the select root) has a test; the census has no lost plan and no unaccepted change;
  the physical prune is the default; the designated final is gone.
- W7: real-entry tests replace the fake; the reader is proven live by mutation;
  covering is emitted under the target's gate.
- No flake: the determinism loops, the uncached full suite, race, the fuzz targets
  touched, and the 1M stress comparison are in the phase evidence.

## 14. Owner-visible dependencies

No decision here waits on an owner ruling; four are user-visible and go into the
owner report with their CHANGELOG entries:
- Go record-layer API callers: after F-7b an in-union with more values than its size
  fails at execution, and the record layer's default size is 0, as in the target
  (measured on the target's record layer, `w8_rl_*`); the CHANGELOG names the
  configuration field.
- SQL plans move to the target's configuration (F-7c, PREFER_INDEX and size 24), with
  the 1M stress comparison beside the target's plans.
- Two declared read extensions stay: the match ordering past a relational table's
  record-type position (a stress query, "ORDER BY PK + index filter", depends on it) and
  the pairwise primary-versus-covering rank.
- Record-layer continuations across an upgrade that crosses a plan-changing step: some
  are refused, some undetected, as in the target (section 12).
- Go record-layer callers who build the target's query graph get IN plans for the first
  time after F-7b's matcher change (4.3 item 9), and F-8 replaces the rules' internal
  winner choices with the target's per-partition yields: both change record-layer plans,
  each with its CHANGELOG entry.
- The preserved Go IN-subquery read extension is not present at the base; nothing is
  removed (4.3 item 8).
- The target's own inconsistency between its data-access and intersection orderings at
  the record-type position (4.3 item 2) is not reported upstream from here, since
  publication is not authorized.
- WS-E section 4's real parameter binding makes a prepared `?` stop implying a sparse
  index until WS-H's plan-constraint identity lands (5, item 3). The order of those two
  workstreams decides whether the prepared form ever plans the scan.
Its external dependencies are other RFC-257 workstreams (WS-D, WS-E, WS-G, WS-H, WS-J)
and RFC-238 section 7c, each named at its step.
