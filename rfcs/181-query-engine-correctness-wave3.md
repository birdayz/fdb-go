# RFC-181: Query-Engine Correctness Wave 3 — provenance, set-op gates, and the type lattice

Status: DRAFT — findings from the post-RFC-180 hunt (five parallel audits over
master `8dd22ac7a`, the RFC-180 merge). Graefe ACK required on this RFC and on
every implementation commit (Cascades core throughout).

## Summary

RFC-180 closed the winner-map/ordering representation, the pruning/pinning
family, the unsigned comparator domain, and the metadata QOV family. The
closing codex loop of that PR oscillated between two irreconcilable demands on
plan-side alias resolution — the structural proof that the remaining
correctness debt concentrates in ONE architecture gap (the engine-internal
name model: strings where Java carries typed provenance) plus a small set of
independent wrong-rows holes that predate it.

Five audits (name model, set-ops/merge, continuations, planner/memo residuals,
type coercion) produced the findings below, ranked by wrong-rows risk. The
P0 batch is fixable now with existing machinery; the name-model retirement
(WS-N) is the structural program; the ConstraintsMap port (WS-P) retires three
Go-only planner crutches at once.

## Ground rules (unchanged from RFC-179/180)

Java tag 4.12.11.0 is the reference; wire compat is the hard line; every fix
lands with a red-proofed pin; correct-or-loud, never silently-maybe-wrong;
coarse-toward-NULLABLE / decline-toward-enforcer-sort are the sanctioned safe
directions where exactness needs provenance the engine does not yet carry.

## P0 — wrong-rows holes, fix first (each is a small, independent commit)

### P0.1 PK-intersection has NO ordering gate (row DROPS, plain SQL)

`WithPrimaryKeyIntersector` (intersector_primary_key.go, planner.go:622) builds
`RecordQueryIntersectionPlan(legs, pkValues)` from `candidate.ToScanPlan(...)`
with no check that any leg's scan order is PK-monotonic. The executor's
intersection cursor (merge_cursor.go) is a strict sorted merge that advances
non-maximal legs forever: an INEQUALITY-bound leg (`idx(a)` with `a > 5` emits
`(a, pk)` order) silently DROPS intersection rows. Nothing between
`MaximumCoverageMatches` and the intersector filters non-equality prefixes.
Compounding: `executeIntersection` hardcodes `reverse=false` while
`ToScanPlan` honors `IsReverseScanOrder()` — one reverse leg alone breaks
monotonicity. Java gates every intersection on
`intersectOrderings` + `enumerateSatisfyingComparisonKeyValues` +
`isCompatibleComparisonKey` (WithPrimaryKeyDataAccessRule.java:112-163,
AbstractDataAccessRule.java:1103/1145).

Repro: `WHERE a > 5 AND b = 3`, indexes on `a` and `b`, matching PKs
interleaved in `a`-order. Fix: port the Java gate (comparison keys derived
from the INTERSECTED ordering, compatibility per leg), thread reverse.
`ImplementIntersectionRule` gets the same gate (latent — no SQL producer yet).

### P0.2 Streaming-agg ordered alternative shares the inner group (wrong GROUPS)

`rule_implement_streaming_agg.go:133` memoizes the agg-over-ordered
alternative via `call.MemoizeExpression(orderedExpr)` — which returns the
EXISTING multi-member inner ref, not a pinned singleton. At extraction,
`physicalStreamingAggWrapper.WithChildren` relinks to whatever leaf-replaceable
plan the group's GLOBAL winner carries, with no grouping-key ordering
re-check: if the winner is a cheaper unordered scan, extraction builds
`StreamingAgg(unordered scan)` → groups split → wrong rows. Currently
shielded only by the cost model's index-favoring criteria (the PR-#201 latent
class). Fix: pin the ordered child at rule time (`FinalOf(orderedExpr)`
singleton, exactly the pinOrderedSpine discipline).

### P0.3 Union-leg ordering bake: delegator-hint lie + arity bake (dup rows through DISTINCT)

Correction to RFC-180's queued item: set-op wrappers FREEZE their concrete
plan snapshots (`WithChildren` keeps `w.plan`), so the feared EXTRACTION
relink is structurally unreachable. The live vectors are RULE-TIME:

- `ImplementDistinctUnionRule` derives each leg's ordering from
  `computeWrapperRichOrdering(firstPhysicalExpr)`; for delegating wrappers
  that is the first-known GROUP estimate, untethered to the wrapper's BAKED
  child plan — comparison keys can describe an order the executed leg does
  not produce → merge-front dedup misses → duplicates from `UNION` (distinct).
- `yieldFromMergedOrdering` appends EVERY physical member of each leg
  partition into `childPlans` while minting one quantifier per leg (a 2-leg
  union can execute a 3+-way merge); safe only while the hint lie is absent.
- `ImplementInUnionRule` has the same delegator vector on its single baked
  member (narrower reach).

Fix (machinery exists): convert the chosen `comparisonParts` to per-leg
`RequestedOrdering`s, run `bestSatisfyingMember` + `pinOrderedSpine` per leg
(executable-plan verification included), decline the yield when any leg
declines, bake ONLY the pinned member's plan (one child per leg), memoize
`FinalOf(pinned)` — Java's memoizePlan singleton shape.

### P0.4 Extraction-side sort elision lacks the executable-plan verification

RFC-180's `planHasDirectChild` verification landed only in rule-time
`pinOrderedSpine`. `rebuildOrderedSpine` (properties/extract.go) still ends in
`WithChildren` unverified — every delegator has a keep-old-plan branch
(non-`isLeafReplaceable` pinned inner: Union/Intersection/FlatMap/NLJ/InJoin
children) that ships the OLD unordered child under an already-elided sort.
Narrow trigger (logical-sort-best + non-leaf-replaceable ordered inner) but
the same class RFC-180 fixed at rule time. Fix: the same verification at
`rebuildOrderedSpine`'s return; decline elision on mismatch.

### P0.5 Missing 32-bit INT arithmetic lane (silent wrong VALUES on INTEGER schemas)

Java types INTEGER columns/small literals as INT and runs `ADD_II`/`MUL_II` =
`Math.addExact(int,int)` — overflow at 2^31 errors with 22003. Go widens
everything to int64 and checks only the int64 boundary:
`SELECT int_col + int_col` near 2^31 returns the wide value where Java errors.
SUM already has the INT-bound check (streaming_cursors.go:540-550);
`ArithmeticValue.Evaluate` has no INT lane at all. Fix: port the II/IF/FI
lanes with exact bounds; FLOAT (32-bit) lane likewise (Java computes ADD_FF in
float32 — overflow to ±Inf at 3.4e38 where Go returns the float64 value).

### P0.6 `bestPhysicalChild` ties resolve by insertion order (hot path)

`planning_cost_model.go:509` calls `Reference.GetBest` with a NON-total
comparator inside cost evaluation of every logical member during PLANNING;
ties flip with insertion order → run-to-run plan flips. One-liner: wrap with
`lessWithHashTieBreak`. The two `properties/extract.go` GetBest sites need the
tie-broken comparator injected (package boundary).

### P0.7 PushSetOperationThroughFetchRule reuses the un-pushed plan snapshot

`buildSetOp` passes the OLD plan (children still the Fetch plans) into the new
set-op wrapper and caps it with a nil-inner fetch shell — extraction then
executes `Fetch(SetOp(Fetch(leg)…))`, double-fetching, while `HintCost`
prices it as pushed-down, so the cost model PREFERS the lie; it also takes
`fetchWrappers[0]`'s TVF instead of Java's `pushValueFunction` combination
(PushSetOperationThroughFetchRule.java:236-240 rebuilds via
`withChildrenReferences`). Rows likely correct (PK re-fetch idempotent) —
cost/EXPLAIN lie + I/O waste. Fix: rebuild the set-op plan over the pushed
inner plans, Java's shape.

Related metadata-only note: `rule_implement_unordered_union.go` memoizes all
partition exprs but bakes `planExprs[0]` — mismatch is cosmetic (no merge
contract), record it when touching the file.

## WS-N — retire the engine-internal name model (the structural program)

RFC-173 killed the name model for runtime rows (Positional-only QueryResult).
What survives is plan-time and metadata-time: the engine answers "is this the
same column?" by comparing RENDERED STRINGS. Java converts string→typed
exactly once (SemanticAnalyzer scope resolution) and carries typed provenance
thereafter (Ordering keyed on Value semantic equality; ResolvedAccessor
equality ordinal-only; quantifier aliases are UUIDs disjoint from user text;
per-quantifier row binding; positional result metadata). Two systemic defects
poison every Go site: quoting-blindness (a legal column can be named `"a.b"`,
`"x"`, or `"Q$2"` — every dotted-split and ToUpper/EqualFold bridge mishandles
it; the two split directions even disagree: last-dot in colref.go vs first-dot
in value_correlation.go) and ONE string namespace for user + machine aliases
(NamedCorrelationIdentifier vs q$N indistinguishable by type, compared
exact-string in maps but EqualFold at leg binders).

Findings (each with wrong-rows modes; full detail in the audit):

- **N-F1 (wrong rows):** ordering satisfaction via rendered strings in
  rich_ordering.go — `orderingKeyFor`'s bridge 2 (`ColumnNameValue` against
  `keyLookup`) BYPASSES the normLookup ambiguity poison guard: a lazy carrier
  `"X"` satisfies a baked `X#1` of a different leg → sort elided → wrong
  order; `EnumerateSatisfyingComparisonKeyValues` feeds set-op merge keys →
  wrong dedup; case-fold bridge conflates quoted identifiers; flat
  `Field:"A.ID"` vs `Field:"ID",Child:QOV(A)` render identically.
- **N-F2 (wrong rows):** join-leg resolution by dotted prefix / folded name —
  `fieldValueAliasAndCol` (first-dot + ToUpper) picks NLJ index-probe keys; a
  quoted dotted column or case-colliding quoted alias keys the join wrongly;
  `rowLegsBinder` binds correlations first-match-EqualFold over merged-row leg
  names; `rebaseOuterLegRefsToMerged` deliberately DOWNGRADES typed QOV
  provenance to dotted strings.
- **N-F3 (zero rows / wrong plan):** `MergeSeedLegsOfValue` re-derives buried-
  leg correlation sets from dotted prefixes — misses unbind Explodes (zero
  rows, its own comment); quoted `"X.Y"` mints phantom dependencies.
- **N-F4 (wrong metadata → wrong client values):** generator derivation
  (descriptorForColumn multi-leg bare-name search; innerByName last-wins;
  legPlanFor EqualFold; qualifyAndMergeColumns dotted datum keys; the
  three-way group-key name contract) — Java flows types positionally from the
  plan's Type.Record.
- **N-F5:** the parseColRef survivor catalog (qualifier-discarding eval sites,
  resolver-gated mis-splits producing spurious 42703, correlation minted from
  a split, label stripping).
- **N-F6 (the enabler):** the shared alias namespace; TODO 7.1.

Program (phases; each phase is separately shippable and Graefe-gated):

- **Phase A — resolver provenance end-to-end:** ResolveIdentifier output
  becomes typed `(leg CorrelationIdentifier, []ResolvedAccessor, display
  Identifier)`; every FieldValue born baked; rebases structural (Java's
  translateCorrelations); kills N-F2/N-F3/N-F5.
- **Phase B — alias namespace split:** machine-minted quantifier aliases only;
  user aliases live in the resolver scope table; delete every EqualFold on
  correlation names; kills N-F6 + the leg-capture half of N-F2.
- **Phase C — ordering on Value identity:** poset/binding maps keyed on
  semantic Value identity (writeSemanticHash/ValuesStructurallyEqual exist);
  requested parts carry resolved values; delete orderingKeyFor's three
  bridges + normLookup; kills N-F1.
- **Phase D — metadata from the flowed type:** FieldValue.Typ populated and
  preserved; ColumnDef positional from the top result value; delete
  descriptorForColumn/innerByName/qualifyAndMergeColumns/colref.go; kills
  N-F4 and ends the RFC-180 metadata whack-a-mole permanently.

Interim pins to land BEFORE the program (red today): quoted case-colliding
ORDER BY; a column literally named `"A.ID"` on a join; duplicate-named ORDER
BY with a baked requested key (the poison bypass); quoted `"Q$1"` table alias;
cross-leg same-name-different-type metadata.

## WS-P — planner-core arc: the ConstraintsMap port (one arc, three crutches retired)

- Dual insertion (physical yields in BOTH member sets) exists because
  ExploreGroupTask iterates only Members() and NeedsExploration counts member
  growth; Java's convergence is ConstraintsMap-epoch-based and finals-routed
  (ExploreGroup handles finals via exploreExpressionAndOptimizeInputs).
  Killing dual insertion naively breaks match re-consumption; the port
  retires it properly, reverts the ContainsFinal compensation to Java's
  containsExactly, and removes the resurrection churn.
- Un-gate OptimizeInputs for finals in REWRITING (Java's shape) so groups
  cross the phase boundary pruned-to-1 (Java Verify), then retire the Go-only
  cost tiebreakers 15b/15c (guarded by stress + EXPLAIN parity comparison —
  DIVERGENCES records that removing them today regresses).
- The maxRoundsPerRef=10 cap becomes obsolete with epochs; until then, export
  the observed max rounds in stress/conformance runs (evidence, not a silent
  raise). Also: fix the stale DIVERGENCES.md claim that advancePlannerStage is
  unported (it exists; the missing piece is the prune-to-1 contract).

## WS-T — type lattice parity batch

- **Plan-time promotion (systemic):** Java rejects unpromotable comparison
  pairs at plan time (PromoteValue lattice + Type.maximumType); Go dispatches
  per-row and degrades unknown pairs to UNKNOWN — silent empty results where
  Java errors, including the empty-table shape (no row evaluated → no error).
  Fix: port the lattice check into predicate typing; runtime arms become
  same-typed.
- **Cast unification:** TWO Go cast engines disagree (cascades values.CastValue
  lenient BIGINT→BOOLEAN + raw strconv error text vs functions.CastValue's
  Java-verbatim arms). Port the strict arms/messages into the cascades value;
  delete the divergence ("no parallel pipelines" in miniature). Several
  RFC-082 cast known-reds then retire.
- **Overflow/edge lanes:** P0.5's INT/FLOAT lanes; `MinInt64 / -1` (Java wraps
  via JVM division, Go errors — align to Java); number+string `+` overloads
  (ADD_IS/SI/... — Java concatenates, Go errors); uint64 arithmetic admission
  (the comparator fix one tier down).
- **Go-only TIMESTAMP/DATE extension:** no Java counterpart (4.12 Type has
  no such codes); stored as fixed-width strings so index and in-memory order
  agree. Allowed extension per CLAUDE.md — needs its own deep edge coverage
  (parse failure arms, mixed string/time comparisons), not parity work.
- **Document-and-pin (Go-right):** UTF-16 vs code-point string order for
  supplementary-plane characters — Java's residual filter disagrees with
  Java's OWN index order; Go is self-consistent with the wire. Pin as a
  documented divergence, do not port. Signed-zero equality/join dimension and
  NaN-payload GROUP BY keys: pin with corpus entries. One comment-rot fix in
  normalizeNLJHashKey (describes pre-fix cmpAny).

## WS-C — continuation surface round 2 (audit complete)

Context that raises every stake: `paginatingRows` applies a 4s per-page time
limit unconditionally and `pageRowBudget` forces in-band page breaks — every
gap below is reachable INSIDE one Go SQL statement, not just via client
resume.

Tier 1 — silent row loss/duplicates on resume:

- **C1 (WORST — silent, reachable, untested): recursive CTE resume
  misroute.** Go materializes the recursion eagerly and returns FromList;
  mid-stream tokens are bare 4-byte list indices, and on resume those bytes
  are fed to the SEED plan with no decode and no rejectUnsupportedResume
  guard — a scan-seeded CTE silently accepts the junk as a raw scan key
  (key_value_cursor.go's legacy fallback) → wrong seed set + re-emission from
  row 0. Java is fully resumable (RecursiveUnionCursor.Continuation +
  RecursiveStateManagerImpl restore phase/frontier/child). IMMEDIATE stopgap:
  the one-line rejectUnsupportedResume guard (silent corruption → 0A000);
  real fix: port Java's continuation + state manager.
- **C2: cross-engine SQL continuation handoff fails SILENT both ways.** The
  inner record-layer cursor framing is byte-identical and conformance-proven
  (incl. the magic KeyValueCursorContinuation wrapper). But Go has NO
  ContinuationProto envelope (no version/binding_hash/plan_hash; naked inner
  bytes; OptContinuation defined but never consumed — Go has no SQL resume
  entry point at all). A Go token handed to Java parses as an
  execution_state-less proto → treated as BEGINNING → PlanValidator hash gate
  SKIPPED → Java silently restarts from row 1. A Java token fed to Go's
  record layer is consumed as a raw key suffix → wrong/empty rows. Also: Go's
  aggregate/sort continuations reuse Java's proto message NAMES with
  Go-internal payloads Java would misread. Decision needed: adopt
  ContinuationProto + hashes, or declare Go SQL tokens engine-private —
  either way pinned by a cross-engine SQL continuation conformance test.
- **C3: FlatMapPipelinedWithCheck discards the pending inner on first check
  mismatch** (cursor_combinators.go:730-746) where Java keeps it ARMED until
  the saved outer reappears (FlatMapPipelinedCursor.java:206-220). The
  executor's own flatMapCursor has the correct kept-armed port — only the
  generic combinator diverges, and IN-JOIN uses it. Duplicate-row mode when a
  saved outer reappears later; narrow (IN-join outers are plan-literal
  lists) but real.
- **C4: LoadByKeys drops its incoming continuation** yet emits
  resumable-looking tokens → replay-from-0 duplicates. Direct-API only (no
  SQL producer).
- **C5: DISTINCT seen-set rebuilt per page** — duplicates straddling an
  internal 4s page break. JAVA-PARITY weakness (Java's
  UnorderedDistinctPlan has the identical fresh-HashSet shape), but Go's
  auto-paging surfaces it inside one statement. TODO note, not a divergence
  fix.
- **C6: nil-continuation invariant hazard** — singleResultCursor emits
  value+nil-continuation (legal in Go, impossible in Java); every parent
  snapshot maps nil child → START. Safe today only by FirstOrDefault's ≤1-row
  shape; a future 2+-row nil-continuation cursor under a merge would silently
  replay. Harden with an invariant check or ban the shape.

Tier 3 — restart inefficiency (correct results): intersectionMultiCursor
omits consume() on discarded non-max children (diverges from Java
IntersectionCursorBase.java:127-132 AND from Go's own intersectionCursor);
recursive CTE recomputes the full recursion every page.

Verified sound: union/intersection continuation framing (byte-faithful
UnionContinuation/IntersectionContinuation ports, loud on corrupt tokens),
streaming aggregate movement table, NLJ INNER/LEFT wave-2 machinery,
in-memory sort codec, LimitRows window, IN-join/IN-union resume (modulo C3;
the dedicated sub-agent's independent Java cites never arrived — marked
verified by the primary auditor directly).

WS-C execution order: C1 stopgap guard (immediate) → C3 kept-armed port →
C4 guard/pass-through → intersectionMultiCursor consume() → C2 decision +
conformance pin → buffered-union per-branch states (existing WS-A
follow-up) → C1 full RecursiveCursorContinuation port.

## Non-goals

Wire-format changes (none required by any finding); porting Java's
UTF-16-order bug; new query capabilities.

## Execution order (owner directive: the name-model findings are the priority)

1. **WS-N interim pins** (red today — they document the live wrong-rows
   surface) then **WS-N phases A→D** — the priority axis. Each phase its own
   review cycle. P0 items that are name-model-rooted (P0.3's delegator-hint
   lie is N-F1's set-op face) fold into the phase that kills their class.
2. Interleaved with WS-N phase boundaries: the INDEPENDENT P0 holes that
   don't wait on provenance — P0.1 (intersection gate), P0.2 (streaming-agg
   pin), P0.4 (extraction verification), P0.5 (INT lane), P0.6 (tie-break) —
   each a small commit with its red pin.
3. WS-C fixes (small, executor-local) as their audit lands.
4. WS-T parity batch (independent of planner work).
5. WS-P (planner-core arc; last — WS-N Phase B simplifies it).
