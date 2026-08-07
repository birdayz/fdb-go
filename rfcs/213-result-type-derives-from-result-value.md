# RFC-213 — A plan's result TYPE derives from its result VALUE

**Status:** DRAFT — awaiting Graefe + Torvalds
**Item:** CQ-97
**Scope:** `pkg/recordlayer/query/plan/plans` (interface + 12 plans), read-only impact on 28 consumer sites

---

## 0. The booking was wrong, and the correction is the design

CQ-97 books this as: *"`RecordQueryFlatMapPlan.GetResultType()` returns `values.UnknownType`
unconditionally where Java derives it from the result value"*, with
`RecordQueryNestedLoopJoinPlan` mentioned in passing and a blast radius of "50 call sites,
21 forwarders / 29 deciders".

Three of those claims moved under measurement. **The item is real and it is six times
larger than booked, in a different shape.**

| booked | measured |
|---|---|
| FlatMap (+NLJ in passing) | **12 plans** stub `GetResultType` unconditionally |
| "50 call sites, 21/29" | **48 sites, 20 forward / 28 decide** (AST, not grep) |
| a `GetResultType` defect | a **`GetResultValue` defect** — see §2 |

The last row is the design. Everything else follows from it.

---

## 1. Java is the spec, and it has no stub anywhere

```java
// RelationalExpression.java:195-196
default Type.Relation getResultType() {
    return new Type.Relation(getResultValue().getResultType());
}

// RelationalExpression.java:200
@Nonnull
Value getResultValue();
```

Two facts, both verified at 4.12.11.0:

- **`:195` is the only definition of `getResultType()` in the entire Java tree.** No plan
  overrides it — `RecordQueryFlatMapPlan` included, which defines only `getResultValue()`
  (`RecordQueryFlatMapPlan.java:205`) and inherits the derivation.
- **`getResultValue()` is ABSTRACT** on the same interface. Every relational expression is
  *required* to have a result value.

So Java's result type cannot be unknown, and not by discipline — **structurally**. The
value is mandatory, so the type is always derivable. There is no code path to a stub
because there is no stub to reach.

---

## 2. Go inverted the dependency, and every symptom follows

```go
// plans/plan.go:70 — RecordQueryPlan
GetResultType() values.Type      // required
// GetResultValue()              — NOT on the interface
```

Go requires the **type** and leaves the **value** optional (28 of the plans implement one
voluntarily). With no mandatory source to derive from, a plan that cannot answer returns
the `UnknownType` singleton.

**That single inversion is the whole divergence.** The 12 stubs, the three standing
workarounds, and the 28 fail-closed consumers are all downstream of it. This is why the
fix is not "make FlatMap derive its type": closing FlatMap alone leaves the inversion in
place, so a thirteenth stub still arrives silently and the workarounds still stand.

### The 12, by what they could derive from

Measured by `TestResultTypeStubInventoryIsCurrent` (AST, not grep — 22 further plans
mention `UnknownType` inside `GetResultType` as a *nil-guard* before forwarding, and are
correct forwarders; a textual gate reports 34 and describes neither population).

| tier | plans | derivable from |
|---|---|---|
| **1** | `FlatMap`, `NestedLoopJoin`, `StreamingAggregation`, `TempTableScan`, `Values` | **`GetResultValue()`** — Java's derivation applies verbatim |
| **2** | `Limit`, `Projection`, `TempTableInsert` | **`GetInner()`** — pass-throughs that change no row shape |
| **3** | `LoadByKeys`, `RecursiveDfsJoin`, `RecursiveLevelUnion`, `TextIndex` | nothing yet — §5 |

`RecordQueryLimitPlan` is the sharpest case in the file: a LIMIT cannot alter a type, its
inner is one call away, and it answers unknown anyway. It is in tier 2 rather than tier 1
purely because nobody gave it a value to derive from — which is the inversion, stated in
one plan.

---

## 3. This is NOT a wrong-results bug. Measured, not assumed.

The brief asked whether the stub reaches a decider that decides *wrongly*, and whether
that is reachable from SQL. **All 28 deciders fail CLOSED.** Enumerated:

- **7 type-assert** `.(*values.RecordType)` and decline — `UnknownType` is a
  `*PrimitiveType`, so the assertion simply fails: `executor.go:3045`, `executor.go:4423`,
  `intersector_primary_key.go:1223`, `rule_implement_distinct_final.go:903`,
  `rule_implement_nested_loop_join.go:2067` and `:2423`,
  `rule_implement_unordered_union.go:187`.
- **1 guards on the ordinal domain** and declines with the reason written down —
  `planning_cost_model.go:2552`: *"Unknown means there is no declared column order to
  state the proof in, so the probe declines rather than falling back to the names it still
  has."*
- **2 route through `bakeMergeComparisonKeys`**, which itself type-asserts and, on a miss,
  passes the key through lazily — *"loud at runtime, never a wrong slot"*
  (`rule_implement_in_union.go:61-95`).
- **2 fingerprint sites** (`scan_range_execution_identity.go:414,447`) take **concrete**
  `*RecordQueryAggregateIndexPlan` / `*RecordQueryVectorIndexPlan` parameters, both of
  which return a stored real type. **No stub can reach a fingerprint** — statically, by
  the parameter type. There is no continuation-collision exposure.
- The remainder **store or propagate** the type into a constructor without branching on
  it.

**So the cost is lost optimizations and lost proofs, never a wrong row.** That is what
makes this an RFC item rather than a bug to fix immediately — and it is also why it has
been invisible: declining costs a plan, and a worse plan is not red.

### Three standing workarounds are the actual damage

Each exists *because* the type cannot be asked for, and each is a place someone already
paid this cost:

1. **`planBuriedLegConcat`** (`rule_implement_nested_loop_join.go:2067`) walks the plan to
   its scan leaves, with the comment saying outright that `GetResultType` "cannot be used
   for a join leg".
2. **`distinctKeyColumns`** (`rule_implement_distinct_final.go:903`) special-cases
   `*RecordQueryProjectionPlan` to call `GetProjections()` *before* it tries the type —
   a route around a tier-2 stub.
3. **`planColumnNamesWithMD`** (`executor.go:3045`) descends through `innerPlanAccessor`
   to the innermost plan and then falls back to the metadata descriptor.

A comment describing a substitute for a capability Java has is a standing admission that
the capability is the answer. There are three.

---

## 4. Decision

**Port Java's dependency direction: make `GetResultValue()` a requirement of
`RecordQueryPlan` and derive `GetResultType()` from it.** Not "fix FlatMap", not "fix
twelve plans" — invert the inversion, so the stub becomes unrepresentable for any plan that
has a value, and the residue is a named, argued list rather than an open set. Java's
`getResultType()` is a *default* on the interface precisely so no plan can forget; Go's
analogue is a shared derivation on the embedded plan base, with `GetResultValue()` on the
interface so the compiler enforces the precondition the way Java's abstract method does.
Phasing is by tier, because the tiers differ in what they can answer, not in how they are
implemented: tier 1 derives immediately, tier 2 forwards (and `Limit` is the proof that
forwarding is a one-line correctness win with no design content), and tier 3 is the part
that needs an argument rather than a patch. The blast radius is bounded and known — 20
forwarders propagate whatever they are given and change with no edit, and all 28 deciders
currently fail closed, so every plan they *start* accepting is a strict improvement over
declining. The risk is therefore not wrong answers but newly-*enabled* optimizations firing
on shapes that never reached them, which is exactly what the stress/golden comparison and
the census equalities exist to catch.

### Rejected

- **Fix `RecordQueryFlatMapPlan` only** (what CQ-97 booked). Closes 1 of 12, leaves the
  inversion, so a thirteenth stub arrives silently and all three workarounds stand. It
  would also let CQ-97 be checked off with the divergence intact — the failure mode this
  workstream has hit repeatedly.
- **Twelve independent local fixes, no interface change.** Treats symptoms. Nothing
  prevents a new plan from stubbing, tier 3 still has nowhere to derive from, and the
  next reader has twelve unrelated one-liners instead of one rule.
- **Teach consumers to tolerate `UnknownType` better.** Entrenches the divergence and
  points the fix at the wrong end: the deciders are already correct — they fail closed,
  deliberately, with the reasoning written down. The defect is upstream of every one of
  them.
- **Return `Type.Relation`-shaped types to match Java exactly.** Go's `GetResultType`
  returns the ROW type where Java returns `Relation(rowType)`; every Go consumer expects
  the row. Matching Java's *wrapper* here changes 48 call sites for no behavioural gain
  and is a wire-irrelevant cosmetic difference. Java's derivation is ported; its wrapper
  is not.

---

## 5. Tier 3 is the part that is not obvious

`LoadByKeys`, `RecursiveDfsJoin`, `RecursiveLevelUnion` and `TextIndex` have neither a
result value nor an inner. They cannot be fixed by rule and each needs its own answer —
which is why this RFC does **not** claim a zero. The inventory gate is a *debt list*, and
a list that asserts a zero nobody can satisfy is a wish, not an invariant. Implementation
proposes tier 1 and tier 2 in one phase each, and tier 3 as a separate argued phase whose
outcome may legitimately be "this plan states no row and here is why".

---

## 6. Acceptance

- `TestResultTypeStubInventoryIsCurrent` shrinks by exactly the tier being closed, as a
  deliberate edit. It fails in **both** directions today — a new stub is unnamed growth, a
  removed stub is unattributed shrinkage.
- `TestRecordQueryPlanStillDoesNotRequireGetResultValue` **goes red on purpose** when phase 0
  lands. It is written so that whoever adds `GetResultValue()` to the interface — for any
  reason — is told they have landed this RFC's precondition.
- At least one of the three workarounds in §3 is retired, or its comment is restated as a
  deliberate choice rather than a description of a missing capability.
- Stress + golden comparison, because newly-enabled optimizations are the expected effect
  and the census equalities on the EXISTS path are stated against populations that
  currently decline.

## 7. What is measured vs. inferred

**Measured:** the 12-plan inventory and the 20/28 split (AST, uncached, gate committed);
every consumer's fail-closed disposition (read individually, enumerated in §3); the
fingerprint sites' static unreachability (parameter types); Java's two citations.

**Inferred, and flagged as such:** that closing tiers 1–2 will *enable* optimizations
rather than merely stop declining. Nothing here measures how many corpus firings currently
decline on an unknown type. That number is implementation's first deliverable, and it
should be a census on the decline path before any plan is changed — the same
gate-before-conversion order CQ-68 established, and for the same reason: a conversion
whose "before" was never measured cannot show it moved anything.
