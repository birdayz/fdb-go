# RFC-184 — Eliminating the plan/memo divergence class: one edge, one identity, one instrument

**Status:** ACK-WITH-CHANGES from Graefe and Torvalds (2026-07-19); changes
folded, DELTA re-confirmation pending. Requires both ACKs before implementation,
per the query-engine gate.
**Tracks:** RFC-183 §11–§15 (the measurements this RFC acts on), RFC-176
(semantic plan identity), RFC-182 (the row-soundness harness).
**Supersedes:** RFC-183's P5 terminal step, which RFC-183 §12 correctly refused
to attempt as a paragraph in another RFC.

**Provenance (read first).** Every code citation, line number, and §N reference
in this RFC resolves against the **RFC-183 implementation branch**
(`rfc183-fully-linked-plans`, commit `a71aa6388`), NOT against `master`. On that
branch RFC-183 is 856 lines with §8–§15, it is *ACK-WITH-CHANGES from Graefe and
Torvalds with changes folded*, `plan_reachability.go` and
`TestCorpusPlanReachability` exist, the shell architecture is deleted, and the
identity method is named `EqualsPlanWithoutChildren`. On `master` none of that is
true yet — `master` carries only RFC-183 §1–§7 (174 lines), the shell machinery
is still present, and the method is named `EqualsWithoutChildren`. A reviewer
auditing this RFC must check out `a71aa6388`; auditing against `master` will
appear to refute citations that are in fact correct.

**Hard prerequisite (load-bearing, not a footnote).** RFC-184's W2 **is**
RFC-183's deferred P5. It presupposes RFC-183 P0–P4 landed, ACK'd, **and merged
to master**. As of this writing RFC-183 is ACK'd on its branch but **unmerged,
unpushed, no PR** (`git merge-base --is-ancestor rfc183-fully-linked-plans
master` → NO). Therefore RFC-184 cannot be *implemented* — and arguably cannot
*merge* — before RFC-183 lands on master. This dependency is stated again in §7's
dependency table; it is not optional and it is not hidden.

## 1. Why this RFC exists

RFC-183 closed six defects and drove genuine unreachable plan/memo edges across
the 2407-query corpus to **0**, ratcheted by `TestCorpusPlanReachability` and
proven by mutation. The honest trajectory is **343 → 158 → 0**, not "343 → 0":
the raw 343 was an over-count (RFC-183 §12) that conflated legitimate group
multiplicity with true divergence; asking the right reachability question (§14)
corrected the baseline to 158; §15 drove that to 0. Quoting "343 → 0" would
overstate the delta against the instrument's own honest count, so this RFC does
not.

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
compensating plan must memoize it and range the quantifier over the *compensated*
reference.** RFC-183 §12 measured this as systematic — roughly ten construction
sites across six parent types — not local to four rules. RFC-183 §15 closed the
reachability *symptom* at all of them, which means the sites agree for every
query in the 2407-corpus; what has *not* been done is removing the ability to
fall out of lockstep. Critically — and this is Graefe's gate on W2 — **corpus
reachability = 0 is necessary but not sufficient** for this precondition: a rule
can range over the compensated reference for all 2407 corpus queries and still
drop the compensation on a shape the corpus does not contain. Establishing
sufficiency is what W4 exists for, and why §7 makes W4 *gate* W2 rather than
trail it.

### Proposed end state

Plans store children **only** as quantifiers. `GetChildren()` resolves through
the reference. The 22 `physical_*_wrapper.go` files (**2283 LOC**) collapse; the
separate 1760-line `physical_wrapper.go` loses most of its reason to exist —
though not all of it: `scanComparisonCorrelations`, `dataAccessExprCorrelations`,
and the ordering derivations are genuine helpers that **relocate** onto the
plans-as-expressions when the wrappers go, they do not vanish. (The "~4043 LOC"
figure an earlier draft attached to `physical_*_wrapper.go` was wrong: 4043 is
the *combined* count of the 22 wrapper files plus `physical_wrapper.go`.) Once
the collapse is done, plan/memo disagreement is not a bug you can write.

**The severity-class argument (Graefe's strongest justification for W2, stated
explicitly).** Collapsing to single quantifier-based storage does not eliminate
winner-selection — a group still holds alternatives and a wrong winner is still
possible. What it eliminates is a whole severity class: today the plan pointer
can execute an expression the memo never costed (fiction → potentially wrong
rows, silent). After W2, `GetChildren` resolves to a *valid, costed member of
the group that was actually evaluated* — at worst a suboptimal winner, never
fiction. That is a **soundness bug (tier 1) downgraded to a cost-quality concern
(tier 3, regression-detectable)**. That downgrade is the real Cascades payoff,
and it is stronger than "delete some adapters."

**The 38 no-quantifier edges are defects, not a modelling gap.** An earlier draft
classified the plan children with no modeled quantifier (TypeFilter 32,
MultiIntersection 6) as benign. Graefe's review corrects this and he is right in
strict Cascades terms: a plan child with **no quantifier** is a subtree the memo
**cannot cost, cannot explore, and cannot apply rules to** — the instrument
emits them as `ReasonNoQuantifier` *violations*, and this is *more* fundamental
than `ReasonAbsent` (a group exists and the plan drifted off it), because here
**no group exists at all** for a subtree the executor will run. Their observable
cost impact today is plausibly nil but **unverified**, which is exactly the
"believed harmless because the corpus didn't punish it" reasoning this RFC
exists to reject. So the honest headline is **"0 `ReasonAbsent`, 38
`ReasonNoQuantifier` outstanding"**, and closing the 38 is a W2 exit criterion
(§7). Step 1 (real quantifier storage on plans) is what closes them.

## 3. Second-order cause — identity is hand-rolled, ~77 times

`RecordQueryPlan` requires each implementation to supply its own
`EqualsPlanWithoutChildren` and `HashCodeWithoutChildren` (`plan.go:76-84`, on
the RFC-183 branch — on `master` the method is still the older
`EqualsWithoutChildren`). Measured on `a71aa6388`:

- **41** plan types in `plans/` implement the pair by hand;
- **36** adapter `EqualsWithoutChildren` methods across **24** files in
  `cascades/`;
- **22** `physical_*_wrapper.go` files, each also hand-implementing correlation
  and quantifier reporting.

That is **~77 hand-written identity implementations**, each a copy of the same
structural invariant. Every copy is a place to get it wrong, and two of the six
recent bugs were exactly that:

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
An adapter with neither is silently unsound — it over-merges two non-equivalent
expressions into one memo group (an interning-soundness violation). That rule
currently lives in ~77 independent heads.

### Proposed end state

Identity derives from a single place — node-local fields plus the quantifier
list — rather than being restated per type. Options, in preference order:

1. **Structural default.** A shared implementation over declared fields +
   quantifiers; types opt out only with a documented reason. Removes the failure
   mode entirely for the common case.
2. **Generation.** Emit the pair from the type definition (the Calcite /
   CockroachDB `optgen` approach). Mechanical, but keeps ~77 artifacts.
3. **Conformance test over all implementers.** Weakest — detects rather than
   prevents — but cheap and worth doing *immediately* regardless of which of the
   above lands, because it is the only item here that ships this week.

Note this is downstream of §2: once children are quantifiers everywhere, the
"exclude children" rule becomes universally correct and the hazard largely
evaporates. **Sequencing matters — do not generate ~77 copies of an invariant we
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
| Memo identity fragility | Universal — Calcite, others | Known industry fix: **generate** identity (CockroachDB does). We hand-write it ~77×. Below standard, not novel. |
| Group multiplicity vs. chosen plan | Universal, by design | Not a defect. RFC-183 §13 already learned this the expensive way. |
| **Dual-storage child edge** | **Self-inflicted** | Ours alone. |

The dual edge is the only self-inflicted one, and it comes from port
*sequencing*: the executor's plan tree was ported first, Cascades second, and
the halves glued with adapters so plans could pretend to be expressions — where
Java needs none because `RecordQueryPlan` **is** a `RelationalExpression`. The
nuance that stops this overcorrecting: two representations is not the sin —
serious engines lower an optimized plan into a separate executable tree — the
sin is that ours are two *mutable* slots that **coexist during search and drift
while rules fire**, with no forcing function to keep them equal. Lowering is
one-way after search; this is not.

One operational warning: expect the discovery rate to *rise* before it falls.
Every new instrument surfaces a backlog of already-latent defects, so when W1
ships and cost diffs begin firing, that spike is the instrument working, not the
refactor regressing — do not read it as a reason to disable the instrument.

## 7. Plan

Ordered by leverage, and deliberately sequenced so nothing entrenches what a
later step deletes.

| WS | Tier | Work | Depends on |
|---|---|---|---|
| **P** | — | **RFC-183 P0–P4 merged to master.** Not a workstream of this RFC — an external precondition. W2 is RFC-183's P5 and cannot begin until its predecessor lands. | — |
| **W0** | 2 | Conformance test over all `RecordQueryPlan` / adapter implementers: identity must not exclude children unless quantifiers are reported. Ships immediately (against the RFC-183 branch). | — |
| **W1** | 3 | Differ emits cost + ordering; baseline header splits identical / both-error / skipped; one corpus variant planned under real statistics. | — |
| **W4** | 2 | Generative harness asserts memo invariants (reachability, arity, identity/hash), not just row-level truth — over shapes the corpus does not contain. | W0 |
| **W2** | 1 | Plans store children **only** as quantifiers; wrapper layer collapses. RFC-183's deferred P5. | **P**, W0, W1, **W4** |
| **W3** | 2 | Identity derived structurally rather than ~77× by hand. | W2 |

**W4 gates W2 — this is Graefe's required re-sequencing, not the original
ordering.** W2 is sound only if every compensating rule memoizes-then-ranges the
quantifier over the *compensated* reference; drop that anywhere and collapse
silently loses a `DefaultOnEmpty` or a residual filter (wrong rows). Corpus
reachability = 0 (RFC-183 §15) is *necessary but not sufficient* — it proves
lockstep for 2407 written queries, not for shapes nobody wrote. W4 is the only
instrument that can establish sufficiency, by asserting the memo invariants over
*generated* shapes. So W4 must land and cover the compensation sites before W2
collapses anything. The alternative Graefe accepts is an exhaustive per-rule
enumeration proving every compensating rule memoizes-then-ranges; W4 is the
cheaper and more durable of the two.

**W1 before W2, correctly stated.** The original draft said "W2 moves costs by
construction" — that is wrong and would misdirect anyone watching the differ.
The memo's *search-time* costs do not move; the memo always costed the
quantifier's group. What W2 changes is the **extracted** plan: at a divergent
edge, extraction stops following the raw plan pointer and follows the group's
cost-winning member instead. In the non-divergent majority nothing moves; at a
divergent edge the extracted plan changes to the one that was *actually costed* —
a **correction** (cost ≤ prior), not a regression. W1 exists to make those
extraction corrections **visible** and confirm each is a correction, not a silent
quality regression from a wrong winner pick. You are changing which expression
extraction yields; you must be able to see the delta. That is why W1 is a
prerequisite, not caution.

### Exit criteria

- **W0:** the test fails on a mutated adapter (mutation-proven, per §15's
  standard — a test that only ever passes proves nothing).
- **W1:** cost/ordering diffs are stable across a no-op change. If they are not,
  that instability is a finding and blocks W2 until understood.
- **W4:** the harness reproduces at least one historical defect from this batch
  when the fix is reverted, **and** exercises every compensating-rule site
  (FlatMap, RecursiveDfsJoin, InJoin, UnorderedUnion, PredicatesFilter,
  Projection) with generated shapes — because that coverage is what licenses W2.
- **W2:** `ReasonAbsent` reachability stays at 0; the 38 `ReasonNoQuantifier`
  edges reach 0; row-diff and 1M stress unchanged; every extraction delta W1
  surfaces is a confirmed correction (cost ≤ prior), not a regression. Explicitly
  **not** "green suite".
- **W3:** count of hand-written identity implementations falls; no behavioral
  change (zero drift under W1's cost-aware differ).

### Measurable targets

Every number below is countable today, so progress is not a matter of opinion.
Anything without a current measurement is marked as such rather than guessed.

| Metric | Today (on `a71aa6388`) | Target |
|---|---|---|
| `ReasonAbsent` reachability edges (corpus) | 0 | 0, and **unrepresentable** after W2 |
| `ReasonNoQuantifier` edges (plan children with no quantifier) | 38 (TypeFilter 32, MultiIntersection 6) | 0 |
| Storage locations for the child edge | 2 | 1 |
| Hand-written identity implementations | ~77 (41 plan + 36 adapter across 24 files) | ≤ 5, each with a documented opt-out reason |
| Differ blind axes | 3 (cost, ordering, statistics) | 0 |
| Corpus plan-type coverage | ~25 of 41 | 41 of 41, or a recorded reason a type is unreachable from SQL |
| "Identical" entries that are both-sides errors | ~255, uncounted | reported separately |
| Memo invariants asserted at the choke point | 2 (children-memoized, no-shell-yield) | 5 (+ reachability, quantifier/child arity, identity-hash agreement) |

The last row is the one that actually encodes §6's principle: it moves checks
from corpus tests to `Yield`, i.e. from *detection over 2407 queries* to
*prevention over all inputs*.

### The standing regime — two new rules

W0–W4 are one-time work; a small regime keeps the classes closed. Most of it is
already repo policy (mutation-prove tests; fields-not-rendered-text diagnostics;
nightly generative harness treated as CI under the red-nightly rule) and is not
restated here. The two rules that are *new* and load-bearing:

1. **Every memo invariant is asserted at the construction choke point (`Yield`),
   not only checked over a corpus.** Corpus tests remain as ratchets, never as
   the primary guarantee. This is the detection→prevention move; without it the
   classes reopen the moment someone writes a rule the corpus doesn't exercise.
2. **Every planner-touching change runs the cost-aware differ, not just the
   suite.** This is what makes tier-3 manageable: we cannot verify cost, but we
   can refuse to let it move unnoticed.

### What is explicitly out of scope

Tier 3 is not addressed by this RFC beyond instrumentation. Making the cost
model *good* — real statistics, calibrated operator costs, cardinality
estimation quality — is separate work with its own success criteria, and
conflating it with this RFC would make both unshippable. W1 buys the ability to
**see** cost move. It does not buy correct cost, and should not be reviewed as
if it did.

## 8. Risks and unknowns

1. **W2 is a semantic change, not a deletion.** RFC-183 §11 proved this; two
   independent attempts refused it for two different correct reasons. The
   compensation-memoization precondition appears satisfied by RFC-183 §15, but
   "appears satisfied at 2407 queries" is exactly the confidence level that has
   been wrong before — which is why this RFC makes **W4 a blocking gate on W2**
   (§7) rather than leaving this as a noted risk. The residual risk is that W4's
   generated coverage still misses a compensation shape; mitigated by the
   exhaustive per-rule enumeration Graefe accepts as the alternative gate.
2. **RFC-183 does not merge.** W2 is its P5; if RFC-183 stays unmerged, RFC-184
   is inert. This is the single largest external dependency and it is outside
   this RFC's control (see the §7 prerequisite row **P**).
3. **Cost instability may be pre-existing and large.** W1 may reveal that cost
   is not reproducible across unrelated changes — a bigger finding than anything
   in this RFC, and one that would reorder it.
4. **Some of the 38 `ReasonNoQuantifier` edges may have real cost impact.** They
   are now correctly classified as defects (§2), but whether any is
   *observable* today is unmeasured. If W1's cost-aware differ shows one of them
   moving a plan, it escalates from "memo-visibility defect, latent" to "active
   mispricing" and jumps the queue.
5. **Structural identity may not be expressible cleanly in Go** without
   reflection costs on a hot path. Fallback is W3 option 2 (generation).

## 9. What this RFC does not claim

Folded from Torvalds' cut of a longer section — one line worth keeping: **a
reviewer should reject any later summary of this RFC that flattens the three
tiers (§6) into a single "bug-free" promise.** Tier 1 reaches zero by
construction; tier 2 reaches orders-of-magnitude by derivation and generative
testing; tier 3 (cost, plan quality) never reaches correctness at all and is
managed by regression detection only. The RFC also does not claim the current
state is broken (reachability is 0, ratcheted, mutation-proven on the RFC-183
branch), does not claim diffgen is bad (only blind on the cost axis), and does
not claim the dual edge is normal for optimizers (identity fragility and cost
blindness are; the dual edge is self-inflicted — §6).

