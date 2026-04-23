# RFC 022 — Cascades Conformance Staging

**Status:** Draft — supplements RFC 021 §Phase 2 (Cascades optimizer).
**Author:** nightshift-45 (2026-04-23).
**Scope:** Risk-ordering and sequencing of Phase 2 work. Does NOT
introduce new features; sharpens the existing Phase 2 plan and
re-orders TODO.md Phase 4 sub-phases so the hardest conformance
question is answered first.

## Motivation

RFC 021 §Phase 2 specifies *what* the Cascades port delivers
(memo + rules + cost + planner driver) but conflates two
conformance goals of very different difficulty:

1. **Semantic equivalence.** Same SQL in → same result set out,
   against the same FDB store. Measurable by the yamsql corpus run
   through both Java and Go engines with result-set diff.
2. **Plan equivalence.** Same SQL in → same plan tree out, with a
   hash-identical plan-cache key. Required for cross-engine RPC
   plan-cache sharing.

(1) is tractable and is the right conformance bar for most users.
(2) is substantially harder:

- Java `CascadesCostModel` is a *comparator*, not a scalar cost
  function. Tie-breaks depend on `PlanProperty` iteration order —
  implementation detail, not spec.
- Task-stack scheduling in `CascadesPlanner` (EXPLORE → OPTIMIZE)
  determines which equivalent plan wins; two identical rule sets
  can produce divergent winners if they pop tasks differently.
- Java's planner evolves per point release; plan-cache equivalence
  is a perpetual treadmill rather than a one-shot port.

If we port 4.0–4.6 speculatively and only build the plan-equivalence
harness (TODO 4.7) at the end, and *then* discover we systematically
diverge from Java plans, we've burned many shifts.

## Principles

**P1 — Split the two conformance goals.** Make semantic equivalence
a hard requirement and plan equivalence a separately scoped
deliverable that we either commit to (with a pinned Java version)
or downgrade to "best-effort, may diverge."

**P2 — Measure before building.** Build the plan-equivalence
harness against the existing naive generator first — it already
produces deterministic plans for simple shapes. Once the harness
can diff Go naive vs Java Cascades and report "equivalent
semantically, diverges on ordering" we know exactly what the
Cascades port has to recover vs what it inherits.

**P3 — De-risk the generics decision up front.** RFC 021 Risk #3
identifies the generics-vs-interfaces tension but doesn't resolve
it. Every day we don't resolve it increases the cost of the eventual
pivot.

**P4 — Rule priority follows yamsql coverage.** The ~69 rules in
TODO 4.5 aren't equal. Six of them fire on swingshift-44's 11-branch
pushdown chain, which the yamsql corpus already exercises. Port
those first so the harness gets end-to-end Cascades coverage of
the existing regression suite before less-common rules land.

**P5 — Pin the Java target version.** If plan equivalence is a
goal, pick a Java tag (currently 4.10.6.0 per MODULE.bazel) and
commit to matching *that version*. Java point releases that change
plans are a follow-up, not a running obligation.

## Changes to TODO.md Phase 4 ordering

Current order in TODO.md is 4.0 → 4.7 linearly. Proposed:

1. **4.-1 (new)**: Plan-equivalence harness, built against the
   existing naive generator. Inputs: parsed SQL + catalog. Outputs:
   Go plan tree, Java plan tree, structural diff, plan-cache-key
   hash diff. Run against ~20 simple yamsql queries to baseline
   "what the naive generator already gets right vs wrong."
2. **4.-0.5 (new)**: Generics-vs-interfaces spike. Implement
   `Value` + one `BindingMatcher` in both shapes (a: interface +
   `any`, b: generics + constraints). Measure compile-time safety,
   API friction on a 10-line predicate matcher, and downstream
   impact on `Matcher[? extends Value]`-style patterns. Decide
   before 4.0. 1 shift.
3. **4.-0.25 (new)**: Plan-cache-key compatibility spec. Sub-RFC
   that answers: (a) are we targeting hash-identical cache keys
   with Java? (b) if yes, which Java version do we pin? (c) if no,
   what's the migration path for clients that expect cross-engine
   cache sharing? Answer determines whether 4.4 (cost model)
   chases Java parity or ships a simpler Go-native cost.
4. **4.0–4.6**: Unchanged from TODO.md.
5. **4.5 rule ordering**: Start with the rules that cover
   swingshift-44's pushdown chain so the harness gets coverage of
   the existing 94 yamsql scenarios early:
   - `PrimaryScanRule`
   - `ImplementFilterRule`
   - `ImplementSortRule`
   - `MergeFetchIntoCoveringIndexRule`
   - Index-equality / index-range implementation rules
   - `InComparisonToExplodeRule` (decomposes IN-list)
   Then the broader ~63 rules in batches aligned to yamsql
   feature flags (JOIN, CTE, aggregate, etc.).
6. **4.7**: Kept as "rule-by-rule correctness tests" from Java's
   planner suite, but no longer the *first* time we see Go-vs-Java
   plan diffs.

## Conformance deliverables — graded

| Deliverable                                | Required?      | Built by |
| ------------------------------------------ | -------------- | -------- |
| Semantic equivalence on yamsql corpus      | Hard required  | 4.-1 harness + 4.5 rules |
| Plan-tree structural equivalence           | If 4.-0.25 yes | 4.-1 harness + 4.4 cost + 4.6 driver |
| Plan-cache-key hash identical with Java    | If 4.-0.25 yes | 4.4 + pin to Java 4.10.6.0 |
| Java planner unit-test port                | Required       | 4.7 |
| RecordQuery wire-compat (unchanged)        | Required       | Already passing; not a Phase 2 item |

## Non-goals for RFC 022

- Does not change the Cascades port's target architecture (memo +
  rules + cost stands).
- Does not reduce the rule count (4.5 still ~69 rules).
- Does not specify the generics decision — defers to the spike
  at 4.-0.5.
- Does not pin the Java version — defers to the spec at 4.-0.25.

## Open questions

- Should the plan-equivalence harness live in `conformance/` (next
  to the Java conformance server) or in `pkg/relational/plan-diff/`
  as a Go-only tool invoking Java via subprocess? Former gets the
  Bazel + testcontainers integration for free; latter is easier
  to run locally.
- If we pin to Java 4.10.6.0 and Java 4.11 changes plan shapes,
  what's the bump process? Same as record-layer Java tag bumps
  (shift-owned, conformance suite gates merge), or automatic?
- Does the naive `query.Generator` (Phase 1a) have a stable
  plan-cache-key format today? If not, should it? (Might affect
  RPC plan-cache behaviour for clients hitting Go before Cascades
  lands.) **Answer (2026-04-23):** No stable format. `query.Generator`
  today does not expose a plan-cache key at all — it parses + plans
  + executes each statement inline per call with no cross-statement
  reuse. Clients MUST NOT send plan keys across engine upgrades.
  Plan-cache compatibility is thus strictly a Phase 2 / 4.-0.25
  concern; nothing pre-Cascades pins a wire format we'd have to
  preserve. If a future shift wants to introduce a naive-generator
  cache key (e.g. to speed up repeated ExecContext calls from a
  single connection), it should mark the format as private-to-Go-
  engine and reject cross-engine use — the Cascades cache key
  (when it lands) is a separate namespace.
