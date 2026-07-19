# RFC-184 — Eliminating the plan/memo divergence class: one edge, one identity, one instrument

**Status:** DRAFT — not yet reviewed. Requires Graefe ACK (Cascades alignment)
and Torvalds ACK (code quality) before implementation, per the query-engine gate.
**Tracks:** RFC-183 §11–§15 (the measurements this RFC acts on), RFC-176
(semantic plan identity), RFC-182 (the row-soundness harness).
**Supersedes:** RFC-183's P5 terminal step, which §12 correctly refused to
attempt as a paragraph in another RFC.

## 1. Why this RFC exists

RFC-183 closed six defects and drove genuine unreachable plan/memo edges across
the 2407-query corpus from 343 to **0**. That is real progress and it is
ratcheted (`TestCorpusPlanReachability`, proven by mutation).

It is also not a fix for the *class*. Every one of those six defects was
detected, not prevented. The invariant that now holds is enforced by a test that
runs over a corpus; it is not enforced by the type system, and nothing stops the
next rule author from reintroducing any of them. The corpus is a net, not a
floor.

This RFC states the two structural causes behind that class, the instrument gap
that let them stay invisible, and what it would take to make each one
unrepresentable rather than merely un-observed.

**The honest framing:** the recent bug rate is not evidence the code is
degrading. All six were latent on master and shipping green; what changed is
that we built the first instrument that could see them. But "we have a good
detector now" is a materially weaker position than "the state cannot be
constructed", and this RFC is about closing that gap.

**Scope of the ambition** — stated up front because "drive toward bug-free" is
otherwise unfalsifiable. §6 splits the problem into three tiers: one class can
be driven to zero *by construction*, one to asymptotically-small by
derivation and generative testing, and one (cost and plan quality) cannot be
made correct by any known means and is managed by regression detection only.
The plan in §7 is scoped to the first two; §6's "out of scope" note fences the
third.

## 2. Fundamental cause — the parent→child edge is stored twice

A physical plan node's child exists in **two independent places**:

1. inside the plan object, as a raw `RecordQueryPlan` pointer (what EXECUTES);
2. inside the wrapping expression, as a `Quantifier` over a `Reference` (what
   the memo COSTS).

Nothing in the type system requires them to agree. `WithChildren`
(`physical_wrapper.go:546`, and 21 sibling implementations) makes the split
explicit and permanent: it keeps `plan: w.plan` and swaps only the quantifiers.

**Every defect in the RFC-183 batch is this one fact in a different costume:**

| Symptom | The same underlying fact |
|---|---|
| nil-inner shells (`PredicatesFilter(<nil>)`) | plan slot empty, quantifier slot live |
| FlatMap compensation (RFC-183 §11) | plan holds a filter no `Reference` holds |
| recursive-DFS leg interning (§15) | one group, two different plan children |
| UNION column-rename `Map` (§15) | `Map` in the plan, absent from the group |
| `scanPlanExpression` identity collapse (§15) | two scans intern to one group; group keeps one, plan carries the other |

The consequence is always the same and always silent: **the memo prices an
expression that will never run.** Extraction reads the plan and never consults
the quantifier's group, so both the suite and the explain-differ stay green
while the optimizer makes decisions on fiction (`plan_reachability.go:14–23`).

### Why Java does not have this class

A child in Java is a *dereference through a non-null final reference*, not a
stored pointer. `Quantifier.java:467` declares `@Nonnull private final Reference
rangesOver`, assigned in a private constructor; every factory (`:611-622`)
requires a `Reference`; there is no `setRangesOver` anywhere in `plan/`.
`RecordQueryFetchFromPartialRecordPlan.java:142` implements `getChild()` as
`inner.getRangesOverPlan()` — the plan has no second copy to disagree with.

Java's guarantee is *structural first*, then asserted
(`CascadesRuleCall.java:221-241` `verifyChildrenMemoized`, an unconditional
`Verify`), then preserved by copy-on-write. Go currently has the assertion (P1
landed it) but not the structure.

### What makes this hard, and why RFC-183 was right to stop

RFC-183 §11 falsified the naive version of this fix. The two slots are not
always redundant copies — at some sites they deliberately hold **two different
facts**, because a rule builds a compensating operator locally and ranges the
quantifier over the uncompensated input. Collapsing those by deletion drops a
`DefaultOnEmpty` (wrong outer-join NULL semantics) and residual filters (wrong
rows), silently.

So the prerequisite is not wrapper deletion. It is: **every rule that builds a
compensating plan must memoize it and range the quantifier over that
reference.** §12 measured this as systematic — roughly ten construction sites
across six parent types — not local to four rules. RFC-183 §15 has since closed
the reachability symptom at all of them, which means the sites are now in
lockstep; what has *not* been done is removing the ability to fall out of
lockstep.

### Proposed end state

Plans store children **only** as quantifiers. `GetChildren()` resolves through
the reference. The ~4043 LOC of `physical_*_wrapper.go` collapses, and the
1760-line `physical_wrapper.go` loses its reason to exist. Then plan/memo
disagreement is not a bug you can write.

**Caveat that must survive review:** §15 leaves **38 edges where a plan child
has no quantifier at all** (TypeFilter 32, MultiIntersection 6). I classified
those as a modelling gap in leaf adapters rather than defects, and deliberately
did not fold them into the headline count. That is a judgment call and a
reviewer may well disagree; it is called out here rather than buried because
this RFC's step 1 is exactly what closes them.

## 3. Second-order cause — identity is hand-rolled, 75 times

`RecordQueryPlan` requires each implementation to supply its own
`EqualsPlanWithoutChildren` and `HashCodeWithoutChildren` (`plan.go:76-84`).
Measured today:

- **41** plan types in `plans/` implement the pair by hand;
- **34** files in `cascades/` implement `EqualsWithoutChildren` for the adapter
  side;
- **22** `physical_*_wrapper.go` files, each also hand-implementing correlation
  and quantifier reporting.

Every hand-written copy of a structural invariant is a place to get it wrong,
and two of the six recent bugs were exactly that:

- `scanPlanExpression.EqualsWithoutChildren` compared node-locally **while
  reporting no quantifiers** — so children contributed to identity from neither
  side, and `TypeFilter([EMP], Scan(EMP, X))` interned with
  `TypeFilter([EMP], Scan(EMP, Y))`. Its sibling `physicalScanWrapper` already
  used `plans.Equals` for precisely this reason. One outlier produced what §14
  had read as four independent defect classes.
- An adapter reporting no quantifiers at all, so the memo could not see its
  child.

The invariant being violated is subtle and nowhere stated in the code:
**excluding children from identity is only correct when children are modelled as
quantifiers**, because the child *groups* then carry that part of the identity.
An adapter with neither is silently unsound. That rule currently lives in 75
independent heads.

### Proposed end state

Identity derives from a single place — node-local fields plus the quantifier
list — rather than being restated per type. Options, in preference order:

1. **Structural default.** A shared implementation over declared fields +
   quantifiers; types opt out only with a documented reason. Removes the failure
   mode entirely for the common case.
2. **Generation.** Emit the pair from the type definition. Mechanical, but keeps
   75 artifacts.
3. **Conformance test over all implementers.** Weakest — detects rather than
   prevents — but cheap and worth doing *immediately* regardless of which of the
   above lands, because it is the only item here that ships this week.

Note this is downstream of §2: once children are quantifiers everywhere, the
"exclude children" rule becomes universally correct and the hazard largely
evaporates. **Sequencing matters — do not generate 75 copies of an invariant we
are about to make structural.**

## 4. The instrument gap — what diffgen does not cover

The explain-differ is a good regression net and it is **not** coverage. Stating
its limits precisely, because the belief that it covers this class is the most
dangerous thing in the current state:

| Limit | Evidence |
|---|---|
| **Emits no cost and no ordering** | The differ compares plan shape only. Five of the six recent defects were *costing* defects — the differ is structurally blind to the exact dimension they live in. All six ran `differing=0`. |
| **Plans with default statistics** | `explaindiff.go:249-251` passes nil statistics; `setup:` INSERTs are not replayed (`:35`). Any cost decision that depends on real cardinality is unprobed. |
| **Agreement-on-failure counts as identical** | Plan failures are recorded as entries, not skipped (`:155`) — deliberately, so schema regressions cannot shrink the baseline, but it means "identical" includes both-sides-error entries (~255 in the last dump). |
| **DML never planned** | ~238 `exec:` stanzas are counted as `NonQuery` and skipped. |
| **Plan-type coverage is partial** | ~16 of 41 plan types never appear in the corpus at all. |

Explain is additionally **lossy**, and hid the deciding field four separate
times during RFC-183: `Scan(T,[=])` renders identically for different comparison
operands; a conjoined predicate renders as `[1 preds]` exactly like a dropped
one. Any check built on rendered text reports clean on real divergence — which
is why `plan_reachability.go` compares with `plans.Equals` and prints fields.

### Proposed work

1. **Teach the differ cost and ordering.** Cheapest large win available, because
   it is the blind axis. Emit cost and required/provided ordering per node; diff
   them. Expect initial noise — cost is not currently stable across unrelated
   changes, and that instability is itself a finding worth having.
2. **Plan under non-default statistics** for at least one corpus variant, so
   cardinality-dependent choices are exercised.
3. **Report the three counts separately** in the baseline header — genuinely
   identical, both-sides-error, and skipped — so the headline number stops
   flattering itself.
4. **Close plan-type coverage**: name the ~16 unexercised types and add corpus
   entries, or record explicitly why a type is unreachable from SQL.

## 5. Property-based testing against the memo

`TestCorpusPlanReachability` checks the right property but over a **fixed
corpus** — it can only find what someone thought to write a query for. RFC-182's
generative row-soundness harness is the counter-example that proves the point:
it found a genuine wrong-rows bug that every plan-level tool missed, because it
generated shapes nobody had written down.

Proposed: extend the generative harness to assert **memo-level** invariants, not
just row-level truth — reachability, one-final-member at extraction, quantifier
count matching plan child count, identity/hash agreement. Generated queries hunt
the shapes the corpus does not contain.

## 6. What "bug-free" can and cannot mean here

This RFC's goal is not "no bugs" — that is not a reachable or even a coherent
target for a query optimizer. It is the elimination of specific *classes*, and
being precise about which ones is what makes the plan actionable rather than
aspirational.

### Three tiers, which behave completely differently

| Tier | Class | Reachable end state |
|---|---|---|
| **1 — eliminable by construction** | dual-storage plan/memo divergence (§2) | **Zero, permanently.** The state becomes unwriteable. |
| **2 — asymptotic** | identity/interning soundness (§3); rule semantic correctness | Orders of magnitude, never provably zero |
| **3 — irreducible** | cost model and plan quality | No oracle exists. Regression detection only. |

**Tier 1 is a real claim.** If W2 lands, the dual-edge family does not get rarer
— it stops being expressible. Java is the existence proof: it has never had this
family, and not because Java authors are more careful. The type does not permit
it. One caveat stated plainly: Go cannot *forbid* a future author re-adding a
raw plan pointer to a struct. The guarantee is API-level, not formal. That is
also exactly the guarantee Java has in practice.

**Tier 2 bottoms out at diligence.** Structural identity derivation drives §3
toward zero, but any opt-out is a new place to be wrong. Rule semantics is
sampling an infinite space — the generative harness makes the sample larger and
better-targeted, never complete.

**Tier 3 is the honest ceiling and must not be sold otherwise.** A mispriced
plan returns *correct rows*, just slowly. No differential test, property check,
or type system can flag it, because in every checkable sense nothing is wrong.
The best available is "the plan did not change unexpectedly" — regression
detection, not correctness. Every optimizer ever built has this.

### The metric is silence, not count

What made the RFC-183 batch expensive was not that six defects existed. It was
that all six were **latent on master, shipping green**, invisible because
extraction reads the plan and never consults the memo. A bug that panics is
nearly free. A bug that returns wrong rows under a green suite costs months.

So the organizing principle for everything below is: **convert silent failure
modes into loud ones.** That is what P1's yield assertion and the reachability
ratchet actually bought — not fewer bugs, but fewer bugs that can hide. Where a
choice exists between detecting a violation in a corpus test and asserting it at
the construction choke point, **assert at the choke point.** A corpus test finds
what someone thought to write a query for; an assertion at `Yield` finds
everything.

### Universal vs. self-inflicted — do not take blanket comfort

Being clear about which of our problems are the human condition and which are
ours, because it determines whether the answer is "adopt the known fix" or
"stop doing this":

| Problem | Status | Implication |
|---|---|---|
| Cost-model unverifiability | Universal, permanent | Tier 3. Manage, never solve. |
| Memo identity fragility | Universal — Calcite, others | Known industry fix: **generate** identity (CockroachDB does). We hand-write it 75×. Below standard, not novel. |
| Group multiplicity vs. chosen plan | Universal, by design | Not a defect. §13 already learned this the expensive way. |
| **Dual-storage child edge** | **Self-inflicted** | Ours alone. See below. |

The dual edge comes from port *sequencing*, not from Cascades. The executor's
plan tree was ported first (mirroring `RecordQueryPlan`), Cascades second, and
the halves were glued with adapters so plans could pretend to be expressions.
Java needs no adapters because `RecordQueryPlan` **is** a `RelationalExpression`
— one hierarchy from the start. The wrapper layer is the seam between two
independently-ported halves, and the seam is the bug family. A textbook instance
of the repo's own 1:1-port principle: we diverged architecturally and the bill
arrived later.

**An important nuance so this does not overcorrect:** having two representations
is *not* the sin. Serious engines routinely lower an optimized physical plan
into a separate executable tree. The difference is that lowering is a **one-way
transformation after search completes**. Ours are two mutable slots that
**coexist during search and drift while rules fire**. The defect is not duality
— it is simultaneity with no forcing function to keep the slots equal.

### Expect the discovery rate to rise before it falls

Every new instrument surfaces a backlog of already-latent defects. If W1 ships
and cost diffs begin firing, that will *look* like a regression caused by the
refactor and will not be one. The failure mode to guard against is someone
reading the spike as "the instrument is broken" and disabling it. Stated here so
it is on the record before it happens.

## 7. Plan

Ordered by leverage, and deliberately sequenced so nothing entrenches what a
later step deletes.

| WS | Tier | Work | Depends on |
|---|---|---|---|
| **W0** | 2 | Conformance test over all `RecordQueryPlan` / adapter implementers: identity must not exclude children unless quantifiers are reported. Ships immediately. | — |
| **W1** | 3 | Differ emits cost + ordering; baseline header splits identical / both-error / skipped; one corpus variant planned under real statistics. | — |
| **W2** | 1 | Plans store children **only** as quantifiers; wrapper layer collapses. The terminal step RFC-183 deferred. | W0, W1 (W1 is the instrument that can see W2 regress) |
| **W3** | 2 | Identity derived structurally rather than 75× by hand. | W2 |
| **W4** | 2 | Generative harness asserts memo invariants, not just row-level truth. | W0 |

**W1 before W2 is not optional.** W2 moves costs by construction; without a
cost-aware differ, a flipped-but-still-correct plan passes every existing test
while silently regressing plan quality. That is precisely the failure mode
RFC-183's §11 note describes — "compiled, passed the suite, returned wrong rows"
— and the zero-drift gate that protected P0–P4 cannot protect W2.

### Exit criteria

- **W0:** the test fails on a mutated adapter (mutation-proven, per §15's
  standard — a test that only ever passes proves nothing).
- **W1:** cost/ordering diffs are stable across a no-op change. If they are not,
  that instability is a finding and blocks W2 until understood.
- **W2:** corpus reachability stays at 0; row-diff and 1M stress unchanged;
  the 38 no-quantifier edges reach 0. Explicitly **not** "green suite".
- **W3:** count of hand-written identity implementations falls; no behavioral
  change (zero drift under W1's cost-aware differ).
- **W4:** the harness reproduces at least one historical defect from this batch
  when the fix is reverted.

### Measurable targets

Every number below is countable today, so progress is not a matter of opinion.
Anything without a current measurement is marked as such rather than guessed.

| Metric | Today | Target |
|---|---|---|
| Unreachable plan/memo edges (corpus) | 0 | 0, and **unrepresentable** after W2 |
| Plan children with no quantifier | 38 | 0 |
| Storage locations for the child edge | 2 | 1 |
| Hand-written identity implementations | 75 (41 + 34) | ≤ 5, each with a documented opt-out reason |
| Differ blind axes | 3 (cost, ordering, statistics) | 0 |
| Corpus plan-type coverage | ~25 of 41 | 41 of 41, or a recorded reason a type is unreachable from SQL |
| "Identical" entries that are both-sides errors | ~255, uncounted | reported separately |
| Memo invariants asserted at the choke point | 2 (children-memoized, no-shell-yield) | 5 (+ reachability, quantifier/child arity, identity-hash agreement) |

The last row is the one that actually encodes §6's principle: it moves checks
from corpus tests to `Yield`, i.e. from *detection over 2407 queries* to
*prevention over all inputs*.

### The standing regime — what runs forever

W0–W4 are one-time work. What keeps the classes closed afterwards is a regime,
and it should be written down rather than assumed:

1. **Every memo invariant is asserted at the construction choke point**, not
   only checked over a corpus. Corpus tests remain as ratchets, never as the
   primary guarantee.
2. **Every invariant test is mutation-proven.** RFC-183 §15 caught a "fix" that
   changed nothing and still went green — a test that has never been observed to
   fail proves nothing about the property it names.
3. **Every planner-touching change runs the cost-aware differ**, not just the
   suite. This is what makes tier-3 manageable: we cannot verify cost, but we
   can refuse to let it move unnoticed.
4. **Diagnostics compare fields, never rendered text.** Explain hid the deciding
   field four separate times on RFC-183. This is now a standing rule, not a
   lesson.
5. **The generative harness runs nightly and is treated as CI.** Per the repo's
   red-nightly rule: a red safety net is always in scope, and no freeze exempts
   triage of one.

### What is explicitly out of scope

Tier 3 is not addressed by this RFC beyond instrumentation. Making the cost
model *good* — real statistics, calibrated operator costs, cardinality
estimation quality — is separate work with its own success criteria, and
conflating it with this RFC would make both unshippable. W1 buys the ability to
**see** cost move. It does not buy correct cost, and should not be reviewed as
if it did.

## 8. Risks and unknowns

1. **W2 is a semantic change, not a deletion.** RFC-183 §11 proved this. Two
   independent attempts refused it for two different correct reasons. The
   prerequisite (all compensating plans memoized in lockstep) now appears
   satisfied by §15, but "appears satisfied at 2407 queries" is exactly the
   confidence level that has been wrong before.
2. **Cost instability may be pre-existing and large.** W1 may reveal that cost
   is not reproducible across unrelated changes, which would be a bigger finding
   than anything in this RFC and would reorder it.
3. **The 38 no-quantifier edges may not be a modelling gap.** They are currently
   excluded from the headline on my judgment. If a reviewer reads them as
   defects, the §15 "zero" needs restating.
4. **Structural identity may not be expressible cleanly in Go** without
   reflection costs on a hot path. Fallback is W3 option 2 (generation).

## 9. What this RFC does not claim

- **It does not claim the engine can be made bug-free.** §6 is explicit: tier 1
  reaches zero, tier 2 reaches "orders of magnitude better", tier 3 never
  reaches correctness at all. A reviewer should reject any later summary of this
  RFC that flattens those into one promise.
- It does not claim the current state is broken. Reachability is 0, ratcheted,
  and mutation-proven; three of the six defects are now impossible to
  reintroduce silently.
- It does not claim diffgen is bad. It claims diffgen is **blind on the cost
  axis**, which happens to be where this bug family lives, and that reading
  `differing=0` as coverage is the actual hazard.
- It does not claim W2 is a cleanup. It is a semantic change to how the memo and
  the executor agree, and it gets a full Graefe review on that basis.
- It does not claim the dual-edge problem is normal for optimizers. Identity
  fragility and cost blindness are universal; the dual edge is ours, and §6
  says so rather than hiding it behind "every engine has this".
