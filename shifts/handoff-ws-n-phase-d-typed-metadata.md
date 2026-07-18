# Handoff: WS-N Phase D — metadata from the flowed type (kill the type loss)

Owner directive (2026-07-18, verbatim): *"Go has the same Type machinery, but
the translator mints FieldValue{Typ: UnknownType} all over and downstream code
compensates by name-keyed guessing — this is ridic. we should do it
correctly."*

This is RFC-181 WS-N **Phase D** (`rfcs/181-query-engine-correctness-wave3.md`,
"Phase D — metadata from the flowed type"). It kills the **N-F4 wrong-metadata
family** (wrong client values, not just wrong labels) and ends the RFC-180
metadata whack-a-mole permanently. The `[]any` row slots are the visible
symptom; the disease is that the type is KNOWN at the catalog and then thrown
away mid-plan. Fix the flow, not the symptoms.

Read before starting: CLAUDE.md (all of it — the working rules below are
binding), the RFC's WS-N section, the slice-6 census in TODO.md's RFC-181
block, and this file end to end.

## The spec — Java, exact citations (checkout at `fdb-record-layer/`, tag 4.12.11.0)

Java makes type loss STRUCTURALLY impossible. Port the discipline, not the
mechanics:

1. **Type is part of the Value interface contract.**
   `Value extends Correlated<Value>, TreeLike<Value>, …, Typed, …`
   (fdb-record-layer-core `…/cascades/values/Value.java:95`);
   `Typed.getResultType()` (`…/cascades/typing/Typed.java:38`). A Java value
   cannot exist without its type. Go's `values.Value` has `Type()` — the
   machinery exists; the DISCIPLINE (never Unknown on a value whose type the
   catalog/scope knows) is what's missing.
2. **Client metadata comes positionally from the PLAN'S OWN result type.**
   `QueryPlan.getResultType()` = `recordQueryPlan.getResultType().getInnerType()`
   (fdb-relational-core `…/recordlayer/query/QueryPlan.java:187-188`); execution
   asserts `type instanceof Type.Record` and builds
   `RelationalStructMetaData.of(type)` from it (`QueryPlan.java:423-425`).
   No descriptor search, no name matching, no per-leg guessing — the top
   record type IS the column list, in order, with types and nullability.
3. **Rows carry a schema synthesized from the plan-time type.**
   `RecordConstructorValue.eval` builds a `DynamicMessage` via
   `typeRepository.newMessageBuilder(getResultType())`
   (`…/cascades/values/RecordConstructorValue.java:143-144`). Go's positional
   rows don't need to become proto messages (that's Java mechanics, heavier
   than our slots) — but the ROW TYPE must be the flowed `Type.Record`
   equivalent, not a re-derived name list.
4. **Resolution produces typed expressions once.** `SemanticAnalyzer` resolves
   identifiers against operator outputs and the result carries the type from
   the catalog forward (`…/recordlayer/query/SemanticAnalyzer.java:442` lookup;
   `LogicalOperator.getOutput()` expressions are typed). After resolution,
   nothing re-asks "what type is column X" — it asks the VALUE.

## The defect census (measured on this branch, 2026-07-18 — re-measure before you start)

**UnknownType mints** (each is a site where a KNOWN type was discarded):
- `pkg/relational/core/query/cascades_translator.go` — **48** occurrences of
  `values.UnknownType`.
- `pkg/relational/core/embedded/logical_predicate.go` — **12**.
- `pkg/relational/core/embedded/plan_visitor.go` — **1**.
Not every occurrence is a defect (some are honest unknown-until-runtime
shapes, e.g. the correlated scalar seed's inner leg); classify each: (i) type
known from catalog/scope → populate it; (ii) type derivable from operand
types → derive it; (iii) genuinely unknown → justify in a comment or
restructure so it becomes knowable. The exit state is that `UnknownType` on a
FieldValue is RARE and each remaining site carries a WHY comment.

**The N-F4 name-keyed guessers — all to be DELETED, not patched**
(`pkg/relational/core/embedded/cascades_generator.go` unless noted):
- `descriptorForColumn` (:2509) — multi-leg bare-name descriptor search;
  cross-leg same-name-different-type picks the WRONG type.
- `legPlanFor` (:2638) — EqualFold leg lookup for metadata.
- `qualifyAndMergeColumns` (:3675) — dotted datum-key merging of ColumnDefs.
- `colref.go` (same package) — the last text-splitting column-ref parser.
- `innerByName` — named in the RFC; the func-level grep no longer finds it.
  VERIFY whether it was already retired or renamed; if a last-wins name map
  survives under another name, it is in scope.
- The **"cascades_generator metadata cluster"** — slice-6 census bucket (d),
  ~18 sites deriving column names/types from descriptors/names instead of the
  flowed type. The census with per-site classification is in TODO.md's
  RFC-181 block (slice 6). Bucket (a) — `resolveColumnName` else-arms
  (`logical_predicate.go:2979`, 8 sites across both build paths) — retires
  when Phase D saturates segment population. Bucket (c) —
  `eval_map`/`eval_predicate`/`eval_proto`/`select_helpers` runtime datum-key
  splits (5 sites) — the name-keyed row model's runtime half.

**Metadata entry points that must flip to positional-from-type:**
- `ResultColumnLabelsForPlan` / `ResultColumnTypesForPlan` /
  `ResultColumnNullabilityForPlan` / `ResultColumnDefsForPlan`
  (`pkg/relational/core/embedded/plan_harness.go:378-440`) — these are the Go
  analogue of `RelationalStructMetaData.of(plan.getResultType())`. Today they
  route through the guessers; the deliverable is that they read the top
  plan's flowed record type and NOTHING else.

## Deliverables (RFC-181 Phase D, restated as exit criteria)

1. `FieldValue.Typ` populated at construction and PRESERVED through every
   rebase/bake/translation (`WithResolvedOrdinal`, translation maps, the
   ordinal seeds, pull-up bakes — a rebase that drops `Typ` is a bug).
2. `ColumnDef` (names, SQL type names, nullability) derived POSITIONALLY from
   the top plan's result type. One derivation path. The four guessers +
   `colref.go` deleted.
3. Census buckets (a), (c), (d) drained. Bucket (b) (dotted-defense splits)
   retires with its producers — Phase B territory; do NOT block on it, but do
   not add new consumers.
4. The RED interim pin goes green: **cross-leg same-name-different-type
   metadata** (RFC-181 interim-pins list — a join where both legs have column
   `X` with different types; today the bare-name search picks a wrong
   descriptor). Write it FIRST, watch it fail, keep it as the phase's
   sentinel. Add its siblings: same-name-different-NULLABILITY, and a
   projection reordering columns (positional metadata must follow the
   projection, not the scan).
5. The scalar-subquery typed-gates path (`SubqueryPlanner.BuildScalar` output
   type, `scalar_subquery_typed_gates.yaml`) keeps working — it was the first
   Phase-D-style type-threading; extend, don't regress.

## Execution order (slices; each lands green + reviewed before the next)

- **D1 — type at birth:** the resolver/scope channel returns TYPED values
  (the catalog knows every column type; `expr.Resolver.ResolveIdentifier` and
  friends must attach it). Drain the classifiable `UnknownType` mints in
  `logical_predicate.go` + `plan_visitor.go`, then the translator's 48.
  Mechanical sub-slices by file region; re-run the type-gate suites
  (cast-pair, promotion — they read `Type()` and will get STRICTER as types
  appear: expect new plan-time rejections where runtime arms used to fire.
  Each behavior flip needs a pin and a Java-conformance check — a query that
  now rejects at plan time must be one JAVA also rejects).
- **D2 — preservation:** audit every FieldValue rewrite site (rebases, bakes,
  ordinal resolution, translation maps) to carry `Typ` through. Add a
  tripwire test: walk a planned tree for FieldValues whose `Typ` is Unknown
  but whose ordinal resolves into a typed row — each hit is a preservation
  bug.
- **D3 — metadata positional:** flip the `ResultColumn*ForPlan` family to the
  flowed top type; delete `descriptorForColumn`/`legPlanFor`/
  `qualifyAndMergeColumns`/`colref.go` and the metadata cluster. This is
  where the red sentinel pin turns green.
- **D4 — runtime drains:** the eval_* datum-key splits (census (c)) — with
  types and ordinals flowing, the runtime name-keyed reads collapse to
  ordinal reads (loud `OrdinalResolutionError` on miss, never first-match).
- **D5 — sweep:** full census re-grep, DIVERGENCES.md update (the N-F4 row),
  TODO.md Phase D checkbox with evidence, handover notes.

## Verification gates (all of them, every slice)

- `just test` green before every commit; the pre-commit hook runs the full
  gate (`just generate && just lint && just build && just test`). BUILD.bazel
  regen failures → `git add -A` and retry; gofumpt failures → `just fmt`.
  Full fresh sqldriver runs take ~10-15 min — run long gates with the
  background-commit pattern (never a foreground 10m timeout).
- 1M stress before/after any slice touching the translator or generator
  (thresholds in TODO.md's baseline table); 5-10× determinism on
  planner-sensitive FDB tests.
- plandiff EXPLAIN parity + yamsql conformance every slice; the cross-engine
  corpus governs shared-surface behavior flips.
- Probe hygiene: any "is this arm dead" probe MUST assert the suite's ok
  line — a compile failure reads as zero hits otherwise (this branch learned
  that the hard way).
- Count sentinels (`TestPartitionSelect_ChainInterningBaseline`,
  `TestOrdinalStarPlanningBudget`, shadow-delta): if they move, verify 3×
  stability, understand WHY, re-baseline with the reason in the comment.

## Process rules (binding, from CLAUDE.md — the ones this phase will hit)

- **Graefe ACK MANDATORY** on every commit touching planner/translator/
  metadata derivation; Torvalds alongside; re-request after every commit (an
  ACK covers only the HEAD it reviewed). Codex delta review per landed batch
  (`codex-review` skill; scope reviews to ranges ≤ a few commits — 45m
  timeouts on mega-ranges are known).
- **No stopgaps, no banking, no "for now".** A guard that declines instead of
  fixing is deferred work; the owner has rejected it explicitly and
  repeatedly. If a slice uncovers a wrong-rows bug (this branch found three
  while doing adjacent work — expect it), fix it in the same slice with a
  red-first pin.
- **Java first.** Read the Java class before writing the Go fix. Where Go
  must diverge (positional rows instead of DynamicMessage), the divergence
  and its reason go in DIVERGENCES.md.
- Every deleted guesser needs the test that would have caught its bug class
  (dimension, not volume).
- One logical change per commit; commit constantly; message style: what and
  WHY, no reviewer/shift attributions in code comments (the hygiene test
  enforces this).

## Context that will save you time

- **Phase A slices 1-6 are done** — resolution already flows through ONE
  scope channel (`resolver.ResolveIdentifier`); you are extending that
  channel to carry types, not building a new one. Slice 3's
  `classifyProjFieldValue` and the B1 binding-keyed pull-up show the pattern:
  structural resolution, zero text re-derivation.
- **Phase B/C interplay:** dup-alias carve-out and ordering bridges are NOT
  yours; if a site serves both a name-model residual and a type derivation,
  fix the TYPE half and leave the name half cited to its owning phase in
  TODO.md.
- **The typed-rows RFC candidate** (TODO.md, "typed row representation:
  retire []any slots") is GATED ON YOU. Don't start it; do leave the type
  flow in a state where `PositionalRow`'s type is authoritative — that's the
  handoff seam.
- The WS-P epoch convergence is freshly landed and reviewer-hardened; if a
  planner count/plan-shape moves under a Phase D slice, suspect YOUR change
  first, and use `git worktree add /tmp/fdb-base <base-sha>` A/B probes (the
  established pattern) before re-baselining anything.

## Definition of done

All five deliverables; census re-grep attached to the final commit message;
DIVERGENCES.md N-F4 row updated to "closed" with the pin names; RFC-181
TODO block Phase D checked with a one-paragraph summary; both virtual
reviewers ACK on the final HEAD; codex delta ACK; full gate + stress +
determinism green. Nothing banked.
