# RFC-180: Query-Engine Quality Remediation

**Status:** Draft — awaiting Graefe/Torvalds review
**Scope:** everything the post-RFC-179 quality assessment surfaced: the un-audited
cursor families, plan-identity stragglers, the ordering-representation split, the
front-end text IR, the dead/skipped safety nets, and the yamsql-corpus fallout
**Reference:** Java `fdb-record-layer` tag 4.12.11.0 (the spec)
**Prerequisite reading:** RFC-179 (the principles this RFC finishes enforcing)

## Summary

A five-dimension quality assessment of the query engine (planner core, executor,
SQL front-end, test suite, residual ledger) — run after RFC-179's ~40 fixes
landed — found that **RFC-179's own principles are still violated in the code it
didn't reach.** The five root-cause patterns are eradicated from the
scan/filter/aggregate/sort/flatMap core, and determinism in the planner is
engineered and pinned. But:

- the **union/IN cursor family** drops, misroutes, or fakes continuations
  (pattern 3, wholesale);
- **plan-identity** in `plan/plans/` stopped migrating halfway — `in_union`,
  `intersection`, `update` still exclude comparands from equality (the F21
  class, marked DONE, isn't);
- **dedup/group keys** still use the lossy `%T:%v` encoder for composite slots
  that F53 proved are live (pattern 2);
- the planner carries **three parallel ordering representations** where Java
  has one (pattern 4 — and per the project owner: only Java's survives);
- the front-end **re-parses SQL text by string munging** as a live fallback
  (the repo's own no-text-matching rule, violated in production code);
- two safety nets were dark: the 319-file yamsql corpus **skipped since May**
  (re-enabled by this RFC: **67/319 scenarios fail**), and a 6.6k-line dead
  `plangen` parallel pipeline (deleted by this RFC).

Two actions were executed immediately on owner direction, ahead of this review:
`pkg/relational/core/query/plangen/` is deleted, and the yamsql runner skip is
removed. The corpus is red; Workstream Y makes it green honestly.

## Ground rules (unchanged from RFC-179)

Every fix: **read Java → port Java's mechanism → revert-proven regression →
Graefe/Torvalds gate → commit.** No invented shortcuts. Corpus expectations are
only ever corrected against **Java's verified answer** (via the cross-engine
harness), never against Go's current output.

---

## Workstream Y — yamsql corpus remediation (RED right now; first priority)

Re-enabling `TestYamsqlConformance` (skip removed at
`pkg/relational/conformance/yamsql/runner_test.go:61`; the skip predated the
Cascades unification and was added *while 18 scenarios failed* — skip-while-red)
surfaces **67 failing scenarios of 319**. Failure histogram:

| Count | Kind | Bucket |
|---|---|---|
| 43 | row-set mismatch (ordered) + 1 unordered | Y3: per-scenario root-cause |
| 21 | expected `0AF00 could not plan`, got rows | Y1: corpus rot — Go's old failure was recorded as "expected"; re-record from Java |
| 14 | expected `0A000`, got `0AF00` (DISTINCT aggregates) | Y2: error-code verdict vs Java |
| 8 | expected `22000`, got `42809` (NULL in IN list) | Y2 |
| 7 | `42883 Unsupported operator NULLIF` | Y4: parity gap — verify Java supports NULLIF, then port |
| 4+4 | `0A000 EXISTS within OR` / `0AF00 could not plan` | Y4/Y1: RFC-082-documented declines the corpus expected to work — verify against Java |
| 4 | expected `22000`, got `42804` (incompatible comparison) | Y2 |
| 6 | `plan does not contain AggregateIndexScan/COVERING/IndexScan` | Y3: plan regression vs Explain-format drift — decide per case |
| 3 | `0AF00 subqueries not supported in UPDATE SET` | Y4 (= TODO L121c) |
| 2 | `42804 expected boolean, got STRING NULL` (CASE WHEN) | Y3: CASE type-derivation bug candidate |
| 1 each | unbound scalar-subquery alias; **ordinal "malformed plan" runtime error on `SUM(MAX(val))`**; UNION `ORDER BY 1` literal-column resolution; `22F3H` leaking `strconv` internals; plain `UNION` unsupported; 2× `42601` parser gaps; CAST-AS-UUID; `42703/22000 expected, got nil` | Y3/Y4 |

Protocol, in order:

- **Y1 (corpus rot):** for each scenario whose expectation encodes a *Go failure*
  (`0AF00 expected`), obtain Java's answer via the cross-engine harness
  (`conformance/` Java server; add the query to the inline cross-engine suite if
  absent) and re-record. A corpus entry is only valid if its provenance is Java.
- **Y2 (error-code verdicts):** for each drifted SQLSTATE, read Java's
  `ErrorCode` mapping for that exact failure site. Whichever side diverged from
  Java loses: fix Go's code/message, or re-record the corpus — never both, never
  by taste. The `22F3H … strconv.ParseInt` message leaks Go internals and is a
  guaranteed Go-side fix (Java wording + `22018`).
- **Y3 (row mismatches + plan asserts):** per-scenario DFS. Each is either an
  engine bug (fix + pin, RFC-179 style) or a Java-verified re-record. The two
  loudest are engine bugs on their face: the `SUM(MAX(val))` **runtime**
  malformed-plan error (Java rejects nested aggregates at *plan time* — wrong
  layer, RFC-173 tripwire misuse) and the unbound scalar-subquery alias
  (executor contract violation).
- **Y4 (parity gaps):** verify Java support first (a TODO can be stale or
  mis-framed). Confirmed gaps become ports (NULLIF, plain UNION-distinct, UUID
  cast, UPDATE-SET-subquery = L121c) or stay documented declines only if Java
  also declines.
- **Y5 (keep it lit):** the runner stays un-skipped from this commit forward.
  The corpus joins CI the moment it is green; until then the branch does not
  merge. Scenario fixes land in batches with the bucket name in the commit.

## Workstream A — union/IN continuation family (pattern 3, wholesale)

The one cursor family RFC-179 never reached. Verified defects:

- **A1** `executor.go:1689` (`executeUnionStreaming`): incoming continuation
  silently discarded — children always start nil; a resumed UNION replays from
  row 0.
- **A2** `executeUnionBuffered` / `executeUnorderedUnion`
  (`executor_new_plans.go:480`): the *parent's* continuation bytes are fed
  verbatim to **every child**; a raw-key scan child consumes them as a key
  position (silent wrong start).
- **A3** `executeInJoin`/`executeInUnion` (`executor_new_plans.go:823/861`):
  continuation ignored whenever in-values exist.
- **A4** `concatCursor` (`executor_new_plans.go:1250`): forwards child
  continuations with no branch tag, while the wire-compatible
  `recordlayer.ConcatCursors`/`ConcatContinuation` sits unused.
- **A5** `mergeSortCursor` (`executor_new_plans.go:928`): no continuation
  support at all — per-child positions, peek buffers, dedup `lastKey` are
  memory-only; also treats in-band child `ReturnLimitReached` as exhaustion.
- **A6** `streaming_cursors.go:17`: `nonEndContinuation = []byte{0}` fake
  per-row tokens in aggregate/customSort/NLJ cursors. Java pairs every emitted
  row with a genuinely resumable continuation
  (`AggregateCursorContinuation(previousContinuationInGroup)`).
- **A7** `flat_map_cursor.go:569-604`: `ToBytes()` errors swallowed (`, _ =`);
  a failed `proto.Marshal` returns the fake token instead of an error.

Today this is shielded only by *non-local* driver invariants (out-of-band stops
error 54F01; the driver EOFs at `emitted>=maxRows`) — which means any union/IN
query that needs more than one 4-second page **fails loudly where Java
paginates**, and every one of A1–A6 becomes silent wrong rows the moment a
row-granular resume surface (e.g. `ExecuteContinuationStatement`) lands.

**Fix (read Java first):** port Java's merge-cursor continuation architecture —
`UnionCursorContinuation`/`MergeCursorState` (per-child typed proto states,
`RecordCursorProto.UnionContinuation`), `ConcatCursor`'s branch-tagged
continuation, and per-row aggregate/NLJ continuations. Route everything through
the RFC-179 typed codec. A7 becomes error-propagating. Pins: mid-stream resume
e2e per operator (union streaming/buffered/unordered, IN-join/union, concat,
merge-sort, aggregate emit phase) with adversarial page boundaries, exactly like
the F4/F5 pins.

## Workstream B — finish the F21 comparand-into-identity migration (plan/plans/)

RFC-179 marked F21 DONE; `plan/plans/` says otherwise. Verified:

- **B1** `plans/in_union.go:75`: `EqualsWithoutChildren` = `reverse` +
  `len(bindingNames)` only. `inSources` and `comparisonKeys` excluded — Java
  `RecordQueryInUnionPlan` compares both. Sibling plans differing only in
  comparison keys collapse into one memo group; the survivor may not produce
  the ordering the winner claimed.
- **B2** `plans/intersection.go:66-83`: comparison-key equality is length-only,
  the hash folds only the count, `reverse` missing — the exact "equal ⟹
  same-hash violation" `comparator.go`'s own comment warns about.
- **B3** `plans/update.go`: SET transforms compared **by count** — `SET a=1` ≡
  `SET a=2` on the write path.
- **B4** `plans/flat_map.go` / `nested_loop_join.go`: `resultValue` excluded
  (Java: `semanticEqualsForResults`).
- **B5** `plans/filter.go:88`, `predicates_filter.go:105`,
  `nested_loop_join.go:142`: hash folds `Explain()` display text while equality
  is structural; `predicates.SemanticHashCode` exists and is unused.

**Fix:** mechanical, per-class port of Java's `equalsWithoutChildren` /
`hashCodeWithoutChildren`. Pins: equal⟹same-hash property tests per plan type +
a memo test that two comparison-key-different in-union plans do NOT collapse.

## Workstream C — dedup/group keys onto the lossless codec (pattern 2 residue)

F53 built a lossless composite encoder for sort continuations; the dedup paths
kept the lossy one. `fmt.Sprintf("%T:%v", …)` collides on composites
(`[]any{"a b"}` ≡ `[]any{"a","b"}`) and splits equal protos across
generated/dynamicpb representations:

- **C1** `executor.go:1521` `packedDedupKey` composite fallback
- **C2** `executor_new_plans.go:1060` `mergeSortCursor.extractKey`
- **C3** `streaming_cursors.go:413` `computeGroupKey`
- **C4** correct-or-loud violations in the same family: intersection
  comparison-key `%v` fallback (`executor.go:2033`,
  `executor_new_plans.go:307`) and `cteDedupKeyer`/`queryResultKey` collapsing
  all nil-Positional rows onto one `"<nil>"` key (`executor.go:3844/3864`) —
  these silently degrade; they must error.

**Fix:** one shared lossless key encoder (reuse `appendContValue`'s type-tagged
encoding, or tuple-pack via the F53 codec), loud error on unencodable types.
DISTINCT/GROUP-BY/UNION-dedup over ARRAY/STRUCT columns get e2e pins with
crafted colliding values.

## Workstream D — one ordering representation: Java's (owner-directed)

The planner has three ordering domains; Java has one (`Ordering` /
`OrderingPart` / `RequestedOrdering`). Owner verdict: **only Java's survives.**

- **D1** `expressions/physical_properties.go:14`: name-string ordering in a
  fixed `[8]orderingColumn` array; `OrderingFromSortKeys` silently truncates at
  8 keys. `properties/extract.go:157` (`sortWinnerFromChild`) can therefore
  elide a sort verified on only the first 8 of 9+ ORDER BY keys — the most
  plausible unpinned wrong-rows path in the planner. Also blind to
  qualification (bare vs dotted names).
- **D2** the winner map and requirement keys throughout keep using D1's type.
- **D3** `expression_partition.go:262` (`RollUpPlanPartitions.makeKey`):
  partition merge keyed on `fmt.Sprintf("%v")` of property values — interface
  pointers stringify as addresses, so semantically-equal orderings never merge
  (under-merging inflates enumeration and diverges alternatives from Java's
  `PlanPartitions.rollUpTo`, which groups by property `equals()`).
- **D4** `expression_partition.go:147` (`toPlanPartitionsFallback`): lumps all
  members into ONE unordered partition, contradicting ImplementSortRule's
  stated invariant ("partitions are keyed on ordering properties") — if the
  nil-PlanPropertiesMap window is reachable, unordered members leak through as
  sort-free finals.

**Fix (Graefe-led, staged):** migrate winner/requirement keys and
`sortWinnerFromChild` onto the existing Java-faithful `rich_ordering.go`
(value-based `OrderingPart`s, no arity cap, no name strings); delete
`physicalOrdering`'s string array; replace D3's stringify with property
equality; make D4 impossible (error) rather than approximate. Interim pin
regardless of schedule: a 9-sort-key e2e that today demonstrates the
truncation.

## Workstream E — compensation determinism (F20 class)

`compensation.go:923` and `:1008` build `matchedSet` as a Go map and then range
over it to produce the ordered quantifier list `Apply` emits into the merged
SelectExpression — plan-shape nondeterminism whenever ≥2 matched quantifiers
merge. Java uses insertion-ordered `LinkedIdentitySet` unions.

**Fix:** insertion-ordered union (slice + seen-set), exactly like the
`partialMatchOrder` fix. Pin: a determinism loop over a two-matched-quantifier
merge shape (10 runs, byte-identical plan hash).

## Workstream F — front-end text IR: kill the reparse, then the strings

The standing no-text-matching violation ("criminal", per owner):

- **F-1 (now):** delete `parseAggregateText` / `parseOperandValue` /
  `parseAtomValue` (`cascades_translator.go:6414-6491`). When
  `AggregateOperands[i] == nil`, the translator **declines typed**
  (`setTranslateErr` with a specific diagnostic) — it never re-parses SQL text
  by splitting on `(` and `+-*/`. The fallback's own comment admits it mangles
  nested arithmetic and drops HAVING groups.
- **F-2:** replace `strings.Contains(name, "(")` aggregate classification
  (`cascades_generator.go:2528`) and the `aggregateArgText` /
  `isBareColumnIdentifier` text classifiers (`cascades_translator.go:729-761`)
  with resolved-Value inspection.
- **F-3 (staged, larger):** retire the text-shaped logical IR itself —
  `LogicalAggregate.Aggregates []string`, `Having string`,
  `LogicalFilter.PredicateText`, `canonicalTextOf` — in favor of the resolved
  `Value` fields that already ride alongside. Text fields become
  load-bearing-never, then deleted. This is the root fix; F-1/F-2 stop the
  bleeding.
- **F-4:** `embedded/connection.go:742`: replace the
  `strings.Contains(msg, "not_committed")` fallback by preserving the typed
  FDBError through wrapping (`errors.As` all the way).
- Also: convert the ~120 bare-`nil` translator declines that collapse into one
  generic `0AF00` into typed `setTranslateErr` diagnostics as they are touched
  (no big-bang; new declines must be typed).

## Workstream G — structural nets

- **G1** yamsql corpus: re-enabled (Workstream Y owns green).
- **G2** `plangen`: deleted (this RFC's first commit).
- **G3** comparator differential fuzz — the generative net for the whole
  Theme-2 family: (a) `compareValues` order vs FDB `tuple.Pack` byte order on
  fuzzer-generated typed slots; (b) `cmpAny`-vs-`compareValues` agreement on
  the documented predicate/sort split (NaN, ±0.0, int32/int64/float32/float64,
  []byte, [16]byte). Every RFC-179 Theme-2 bug (F8, F9/F13, F10, F14, F27,
  F28, F42, F48) would have fallen to this fuzzer.
- **G4** exotic-float cross-plan agreement e2e: write NaN/-0.0/±Inf into an
  indexed DOUBLE column via the record-layer API (no SQL literal path exists,
  by design), then assert indexed plan ≡ in-memory plan ≡ Java for ORDER
  BY/GROUP BY/MIN/MAX — the axis the test-suite audit found unprobed.

## Workstream H — small correctness + hygiene batch

- **H1** `executor.go:2834`: int64→float32 conversion has no range check —
  SUM(BIGINT) into a FLOAT column silently writes ±Inf. Mirror the float64
  arm's `22003` range check (verify Java `CastValue` first, per the existing
  TODO). *Silent wrong-value write; do not wait for RFC-083.*
- **H2** OrElse wrapper drops its serialized decision when the active child
  emits nil-byte continuations (executor cursor combinators).
- **H3** `plans/index_key_value_to_partial_record.go:75-92`: silent nil on
  malformed ordinal paths — Java throws; make it loud.
- **H4** resumed in-memory sort buffer bypasses the RFC-130 memory charge
  (`executor.go:3752`).
- **H5** stale/misleading comments: `ordinal_join.go:637/:657` describe a
  non-existent `buildCoveringRow` with an inverted layout claim (the actual
  `buildCoveringLogicalRow` builds LOGICAL rows); `compareValues:1211` "NaN
  sort key" note (unreachable); `plan_visitor.go:29` / `logical_predicate.go:32`
  still describe the deleted naive generator as alive;
  `expression_partition.go:113` claims ordering isn't a partition dimension
  while the map path keys on `orderingHash`.
- **H6** `decodeSortContinuation` pre-release legacy branches
  (`continuation.go:1024-1096`): dead by the same argument that deleted JSON
  tag 15 — delete.
- **H7** `executor/covering_optimizer.go`: unwired skeleton — delete (it can
  return from git history when someone actually wires it).
- **H8** `planner.go:60`: doc says the MaxTasks cap "returns the partial
  result"; code returns nil+error. Fix the comment (the behavior matches
  Java's throw).

## Workstream I — planner guard parity (Graefe-scheduled, after A–F)

- **I1** Java's missing complexity guards: `maxTaskQueueSize`,
  `maxNumMatchesPerRuleCall`, `isRuleEnabled` (Go has only `MaxTasks`).
- **I2** the Go-only silent 10-round `maxRoundsPerRef` cap
  (`unified_tasks.go:59`) exists because Java's
  `ReExploreExpression.shouldPushRule` constraint-dependency gating was never
  ported — port the gate, then re-evaluate the cap (loud if kept).
- **I3** rule selection is a flat O(rules×expressions) scan; Java indexes rules
  by root operator class. Perf, not correctness — last.

## Explicitly out of scope (tracked elsewhere, not lost)

RFC-083 batch (F37/F51 corners; needs its re-ACK — H1 is pulled forward out of
it because it is a silent wrong-value write), TODO L1632 (GROUP-BY dropped
residual — next TODO grind), RFC-167 open phases, RFC-173 slices, the NLJ
dotted-string/alias-case cleanup (RFC-173's namespace unification is the real
fix; piecemeal string surgery would churn), RFC-164 WS-1 generative
row-differential (booked; G3/G4 are this RFC's narrower nets).

## Execution order

Correctness-first, contained-first, then architecture:

1. **Y** — corpus triage to green (Y2 error-code verdicts and Y1 re-records in
   batches; Y3 engine bugs DFS'd as found; Y4 parity ports as verified). The
   two Y3 engine bugs on their face (`SUM(MAX)` layer, scalar-subquery
   binding) immediately.
2. **E** (tiny) → **B** (mechanical) → **C** (shared encoder) → **H1–H8**.
3. **F-1/F-2/F-4** (kill the reparse + classifiers), then **A** (the big
   continuation port), then **D** (ordering unification, Graefe-led design
   first), then **F-3** (IR migration), then **G3/G4**, then **I**.

Every commit through the standing gate: Graefe + Torvalds on each, @claude +
Codex on the PR, re-request after every push.

## Findings index

| Id | Severity | One-liner | Status |
|---|---|---|---|
| Y1–Y5 | wrong-rows/red-net | 67/319 yamsql failures | **OPEN — red** |
| A1–A7 | wrong-rows (latent) + loud-gap | union/IN continuation family | OPEN |
| B1–B5 | memo-collapse / nondeterministic | plan-identity stragglers | OPEN |
| C1–C4 | wrong-rows | lossy dedup/group keys | OPEN |
| D1–D4 | wrong-rows (D1) / divergence | ordering-representation split | OPEN |
| E1 | nondeterministic | compensation map-order | OPEN |
| F-1–F-4 | wrong-rows-history / rule-violation | text IR reparse | OPEN |
| G1 | process | yamsql re-enabled | **DONE (red)** |
| G2 | dead-code | plangen deleted | **DONE** |
| G3–G4 | net-gap | comparator fuzz + float cross-plan e2e | OPEN |
| H1 | wrong-value write | int64→float32 ±Inf | OPEN |
| H2–H8 | minor/hygiene | see workstream H | OPEN |
| I1–I3 | parity/perf | planner guards | OPEN |
