# RFC-213 — A plan's result TYPE derives from its result VALUE

**Status:** IMPLEMENTED by RFC-232 (2026-08-13; rev 1 was NAK'd and rev 2's diagnosis was corrected)
**Item:** CQ-97
**Scope:** `pkg/recordlayer/query/plan/plans` result contracts and their consumers

---

## Current state after RFC-232

RFC-232 completed this RFC with a stronger exact-value contract than the draft
sequence anticipated:

- `expressions.RelationalExpression` requires `GetResultValue()`, and
  `plans.RecordQueryPlan` embeds that interface while retaining
  `GetResultType()`.
- The unconditional plan-level `UnknownType` stub inventory is **zero**.
- Aggregate-index construction passes an exact candidate-derived result type;
  the former call-site `UnknownType` stub inventory is **zero**.
- The result-type consumer classifier measures **FORWARD 1 / GUARDED 10 /
  PROPAGATED 27 / RAW 0** (the pinned test's comment carries every movement
  since this sentence was first written: 14/29 at that time; RFC-235 retired
  three reads; RFC-242 retired three more — `planColumnNamesWithMD` and
  `physicalPlanColumnNames`, both GUARDED tail reads of a union leg's row, and
  `columnRenameValue`'s PROPAGATED read — by deleting the union re-alignment
  they served). `RAW == 0` remains the correctness ratchet. The
  post-implementation growth is executor exact-layout admission (projection,
  UPDATE, aggregate index, multi-intersection, and DefaultOnEmpty) plus VALUES
  passing its declared type to the runtime validator. The descendant producer
  walkers no longer consult declared result types: exact carrier-handle
  identity plus `OrdinalLayout.RawEqual` is the stronger physical authority for
  crossing a row- and layout-preserving unary wrapper. The PK point-probe cost
  proof likewise now takes both its row type and pointer-exact current owner
  from `scan.ProvidedOutputLayout().Carrier()`: the retired declared result-type
  read could not identify the selected evaluation phase after exact filter
  normalization and made every PK point probe over-decline. One further read lets
  `firstOrDefaultResultFromValue` materialize an empty arm in the plan's exact
  record/layout carrier, or carry the declared scalar type into its positional
  row. None of this growth is a new unresolved producer.
- `TestResultTypeStubInventoryIsCurrent`,
  `TestRecordQueryPlanRequiresGetResultValue`,
  `TestResultTypeConsumersFailClosed`, and
  `TestResultTypeStubsCreatedAtCallSites` pin those facts.

Sections 0–6 below preserve the pre-implementation diagnosis and Java
comparison. Their plan counts and consumer populations are historical baseline
measurements, not descriptions of the current tree.

---

## 0. Two corrections, one of them to this RFC

CQ-97 books this as *"`RecordQueryFlatMapPlan.GetResultType()` returns `values.UnknownType`
unconditionally where Java derives it from the result value"*, blast radius "50 call sites,
21 forwarders / 29 deciders". Measured, the item is six times larger and a different shape:
**12 plans** stub it (13 counting one made at the call site, §3a), and the split is
**20 forward / 28 decide** over 48 sites.

**Rev 1 of this RFC was then wrong in its own §1**, and the error was the same class it was
correcting — a claim made from reading one level of a chain and not the next.

> Rev 1: *"Java's result type cannot be unknown, and not by discipline — structurally…
> there is no code path to a stub because there is no stub to reach."*

**False.** Java has a stub, one level down, and it is reached in production. §1 is rewritten
around what Java actually does, and the design in §4 changes its claimed mechanism as a
result. The direction survives; the reason for it is different and better.

---

## 1. Java is the spec — and Java has a sentinel, at the VALUE level

```java
// RelationalExpression.java:194-196   (in cascades/expressions/)
@Override
default Type.Relation getResultType() {
    return new Type.Relation(getResultValue().getResultType());
}

// RelationalExpression.java:200
@Nonnull
Value getResultValue();
```

```java
// values/Value.java:107-111
@Nonnull
@Override
default Type getResultType() {
    return Type.primitiveType(Type.TypeCode.UNKNOWN);
}
```

**Both ends of the chain are defaults, and the terminal one is a SENTINEL.** The relational
end derives from the value; the value end falls back to `UNKNOWN`. That fallback is
first-class, not an accident:

- `TypeCode.UNKNOWN` is an enum constant (`typing/Type.java:774`), and so is a second erased
  code `ANY` (`:775`), used deliberately in built-in signatures and as `Type.fromObject`'s
  fallback (`:728-729`).
- `Type.isUnresolved()` (`:298-300`) is defined as exactly `typeCode == TypeCode.UNKNOWN`.
  Java has an erased type **and a predicate for asking about it**.
- It is **reached**: `values/EmptyValue.java` declares no `getResultType()` override, so it
  answers `UNKNOWN`, and it is planted into the expression graph by
  `KeyExpressionExpansionVisitor.java:124`, `ScalarTranslationVisitor.java:116` and
  `AggregateIndexExpansionVisitor.java:238`.

So Java is not "totally typed". **It puts the sentinel at the value level, gives it a name
(`EmptyValue`), and gives it a predicate (`isUnresolved()`).** Go puts it at the plan level,
unnamed, as a method that opts out.

That is the whole difference, and it is what makes one of them a *statement* and the other
an *absence*. `EmptyValue` says "this expression yields no row" and travels with the
expression; `GetResultType() { return UnknownType }` says "nobody implemented this here" and
is indistinguishable from an oversight — which is precisely how twelve of them accumulated
without anyone deciding to.

**Scope note on a claim rev 1 overstated.** Rev 1 said `:195` is "the only definition of
`getResultType()` in the entire Java tree". Tree-wide there are 60 definitions (50 on
`Value`, one on `Typed` at `typing/Typed.java:38` which `:194` overrides with a covariant
return, plus `Reference`, `InSource`/`InComparandSource`, four on relational
`QueryPlan`/`CopyPlan`, and an unrelated `TypeCode` overload). **Scoped to the
`RelationalExpression` hierarchy the claim holds exactly**: within `cascades/expressions/`,
`:195` is the sole definition, every other hit is a call, `RecordQueryFlatMapPlan.java` has
zero definitions and only `getResultValue()` at `:205`, and there are no test overrides.
The scoped claim is the one this RFC uses.

## 2. Go inverted the dependency

```go
// plans/plan.go:70 — RecordQueryPlan
GetResultType() values.Type      // required
// GetResultValue()              — NOT on the interface
```

Java requires the **value** and derives the **type**. Go requires the **type** and leaves
the value optional — 28 plans implement one voluntarily. With no mandatory source, a plan
that cannot answer opts out. Every symptom below is downstream of that inversion.

### The 12, by what they could derive from

Measured by `TestResultTypeStubInventoryIsCurrent` (AST, not grep — 22 further plans mention
`UnknownType` inside `GetResultType` as a *nil-guard* before forwarding and are correct
forwarders; a textual gate reports 34 and describes neither population).

| tier | plans | derivable from |
|---|---|---|
| **1** | `FlatMap`, `NestedLoopJoin`, `StreamingAggregation`, `TempTableScan`, `Values` | `GetResultValue()` — Java's derivation applies verbatim |
| **2** | `Limit`, ~~`Projection`~~, `TempTableInsert` | `GetInner()` — pass-throughs that change no row shape |
| **3** | `LoadByKeys`, `RecursiveDfsJoin`, `RecursiveLevelUnion`, `TextIndex` | **`EmptyValue`'s analogue** — see §5 |

`RecordQueryLimitPlan` is the sharpest case: a LIMIT cannot alter a row type, its inner is
one call away, and it answers unknown anyway.

> **CORRECTION (RFC-226): `Projection` was in tier 2 and must not be — it is tier 1.** A
> projection is precisely the node that changes the row shape; `Limit` and `TempTableInsert`
> belong in tier 2, it does not. Deriving `RecordQueryProjectionPlan.GetResultType()` from
> `GetInner()` as tier 2 prescribes would make it state its inner's row, which is the exact
> falsehood `expressions/logical_projection.go:94-121` already documents. Tier 2 would make the
> physical twin agree with the logical one *on the wrong answer* — worse than today, where the
> physical side at least declines honestly.
>
> **An earlier draft of this note claimed the tier-2 assignment was the CAUSE of two live
> defects. RFC-226 rev 5 measured that and it is false** — both defects survived the type change
> unchanged and had unrelated causes (a subquery outer scope that registered no CTE leg, and a
> join seed that refused a projection leg). What the row-stating change does deliver is that the
> *consumers* of a projection's row can finally read one: the seed reconstruction is built on
> `GetResultType().(*values.RecordType)` and could not have been written while a projection
> answered `UnknownType`. The correction to this table stands on Java's contract and on the
> node's role, not on a defect attribution.
>
> `Projection` derives from `GetResultValue()` like the rest of tier 1: a record constructor over
> its projected columns and their aliases. That is Java's contract for the node holding this role
> (`GraphExpansion.java:396` → `SelectExpression`; `RecordQueryMapPlan.java:143-147` stores the
> result value and defines no `getResultType`). Graefe confirmed the correction is mandatory.
> See `rfcs/226-a-projection-states-the-row-it-produces.md` §0c and §4. RFC-226 also measured
> that the executor emits one `PositionalRow` slot **per projection**
> (`executor/executor.go:2589-2609`), so the projection's produced row is its column list — not
> its inner's row — which is the same conclusion reached from the execution side.

---

## 3. Not a wrong-results bug — and this half is now a committed instrument

Every consumer of a result type fails **CLOSED** on an unresolved one. Rev 1 established
that by reading 28 sites; reading does not survive the 29th, so it is now
`TestResultTypeConsumersFailClosed`, an AST classifier over the whole non-test tree:

```
FORWARD 20   GUARDED 7   PROPAGATED 21   RAW 0     (48 sites)
```

`FORWARD` is structural, not a proxy: the call is the sole operand of a `return` inside a
`GetResultType` method — the definition of pass-through. `GUARDED` is type-asserted or
type-switched **at the call site**. `PROPAGATED` means *not decided here* — handed to a
constructor, returned, or assigned — and explicitly **not** "unguarded": the classifier does
no dataflow, so `planning_cost_model.go:2552`, which assigns and then guards on the next
line via `OrdinalDomainOfType(layout).IsKnown()`, reads as PROPAGATED. **`RAW` is the class
that must stay empty**, and it is the assertion that separates this RFC from a bug report:
a fail-closed consumer declines an optimization (invisible — a worse plan is not red), a raw
consumer returns wrong data.

*(Rev 1 guessed 8/20 for GUARDED/PROPAGATED from reading; the classifier corrected it to
7/21. That is the second time in this item that reading lost to an instrument.)*

### 3a. A thirteenth stub, made at the call site

`RecordQueryAggregateIndexPlan.GetResultType()` returns a stored field, so the method-body
inventory correctly does not list it — **and every aggregate index plan the planner builds
still carries `UnknownType`**: the constructor defaults nil to it (`aggregate_index.go:84-86`)
and all three production callers pass the singleton *explicitly*
(`rule_aggregate_data_access.go:129`, `:299`, `:724`).

Rev 1 asserted these plans "return a stored real type" — false, because it inspected the
method and not the callers. **A producer census that reads only method bodies is blind to
this construction, and the blindness already produced a false claim in a reviewed
document**, so it is now pinned by `TestResultTypeStubsCreatedAtCallSites`.

Consequence for the continuation fingerprint (`scan_range_execution_identity.go:414`): the
`result-type` field is hashed unconditionally and is **always the same singleton**, so it
contributes **zero entropy**. This is not a collision — the surrounding fields (index name,
index type, scan type, reverse, record type, aggregate function, group columns, aggregate
column, canonical aggregate column, both grouping counts, physical key types) already
determine an aggregate plan's result type, and the vector twin passes a real
candidate-derived type (`vector_index_match_candidate.go:331`). It is **dead weight that
reads as load-bearing**, which will mislead whoever later introduces a case where the result
type varies independently. Rev 1's static-unreachability argument stands as written
(concrete pointer parameters, no subtyping, both plans leaves returning stored fields).

### 3b. Two standing workarounds — one fewer than rev 1 claimed

1. **`legOrdinalSafety`** (`rule_implement_nested_loop_join.go:2019`) says outright that
   `GetResultType()` "cannot be used for a join leg", which is why `planBuriedLegConcat`
   walks to the scan leaves instead. *(Rev 1 placed this comment inside
   `planBuriedLegConcat`; it is one function up.)*
2. **`planColumnNamesWithMD`** (`executor.go:3045` at the time) descended through
   `innerPlanAccessor` to the innermost plan and fell back to the metadata descriptor.
   Deleted by RFC-242 together with the union position-remap it served.

**Withdrawn:** rev 1 called `distinctKeyColumns`' `*RecordQueryProjectionPlan` branch "a
route around a tier-2 stub". It is not. Its own comment (`rule_implement_distinct_final.go:896-898`)
says it returns `GetProjections()` to obtain the projected **values** (`g/2`, `f(g)`), which
the generic path structurally cannot produce because it synthesises one `FieldValue` per
`RecordType` field. Fixing `RecordQueryProjectionPlan.GetResultType()` perfectly would not
retire that branch. The claim inflated the case and is dropped.

---

## 4. Decision

**Port Java's arrangement: give the sentinel a name at the VALUE level, put
`GetResultValue()` on `RecordQueryPlan`, and derive `GetResultType()` from it.**

Rev 1 claimed inverting the dependency makes the stub "unrepresentable". **That is false and
Java disproves it** — `Value.getResultType()` defaults to `UNKNOWN` and `EmptyValue` reaches
it in production. Inverting does not delete the sentinel; it **relocates** it from plan level
to value level, which is exactly what Java did and is the actual argument for the change:

- At plan level the sentinel is an **opt-out**. `GetResultType() { return UnknownType }` is
  indistinguishable from an unfinished method, which is how twelve accumulated silently.
- At value level it is a **statement**. A plan that flows no row says so by flowing Go's
  `EmptyValue` analogue, and a consumer asks `isUnresolved()` rather than pattern-matching a
  failed type assertion. The information is the same; its *provenance* is the difference,
  and provenance is what makes a new one a decision instead of drift.

The compiler enforces the precondition the way Java's abstract method does: once
`GetResultValue()` is on the interface, a new plan cannot forget it, and the derivation lives
in one place instead of forty.

### Sequence — corrected, because rev 1's was not executable

Rev 1 called the interface change "phase 0". In Go an interface method is a **compile-time
requirement on every implementation**, including tier 3's four plans which by rev 1's own
account have nothing to return. **Tier 3 gates the interface change, not the reverse.**

- **Phase 0 — the `EmptyValue` analogue.** Port Java's named "no row" value plus an
  `IsUnresolved()` predicate on `values.Type` (Java has both; Go has neither by name). This
  is what lets all twelve satisfy the interface on day one.
  *Companion (EE2): the consumer classifier has no unit pin over explicit state — all
  four gates in `result_type_stub_census_test.go` run over `sourceTreeRoot`, so §3's
  20/7/21/0 split and RAW's reachability were both established by hand and nothing keeps
  either established. Add a synthetic-source pin, one site per arm, in the shape of
  `TestIsNamedCallSeesQualifiedCalls`. Cheap, and it makes the 8/20 → 7/21 correction
  reproducible rather than anecdotal — which matters because that correction is the one
  fact §4's framing rests on.*
- **Phase 1 — `GetResultValue()` joins `RecordQueryPlan`.** Tier 3 returns the empty value;
  tiers 1–2 already have one or can forward. Compiles only after phase 0.
- **Phase 2 — derive.** `GetResultType()` becomes a shared derivation; tier 1 derives from
  its value, tier 2 forwards. The 20 forwarders change with no edit.
- **Phase 3 — the call-site stub (§3a)** and the fingerprint's dead field.

### Rejected

- **Fix `RecordQueryFlatMapPlan` only** (what CQ-97 books). Closes 1 of 13, leaves the
  inversion so a fourteenth arrives silently, and lets CQ-97 be checked off with the
  divergence intact — the failure mode this workstream has hit twice in two days.
- **Twelve independent local fixes, no interface change.** Treats symptoms; nothing prevents
  a new stub, tier 3 still has nowhere to derive from, and the next reader inherits twelve
  unrelated one-liners instead of one rule.
- **Teach consumers to tolerate `UnknownType` better.** Points the fix at the wrong end. The
  consumers are already correct — they fail closed, deliberately, with their reasoning
  written down. The defect is upstream of all 28.
- **Match Java's `Type.Relation` wrapper.** Go's `GetResultType` returns the ROW type where
  Java returns `Relation(rowType)`, and every Go consumer expects the row. Porting the
  wrapper changes 48 sites for no behavioural gain and is wire-irrelevant. Java's
  *derivation* is ported; its *wrapper* is not.

---

## 5. Tier 3 is no longer an open question

Rev 1 left `LoadByKeys`, `RecursiveDfsJoin`, `RecursiveLevelUnion` and `TextIndex` as "an
argued set", because it believed Java had no answer for a plan that states no row. **Java
has exactly that answer**: `EmptyValue` + `TypeCode.UNKNOWN`, a value-level statement of "no
row" rather than a plan-level opt-out (§1). Tier 3 is phase 0's consumer, not a residue —
which is also what makes the phasing executable.

This RFC therefore claims no zero on the stub inventory, but for a narrower reason than rev 1
gave: the inventory shrinks to the plans that legitimately flow an empty value, and those
remain listed *with that as their stated reason* rather than as unfinished work.

---

## 6. The payoff, MEASURED — rev 1 inferred it

Rev 1's §7 admitted that "closing tiers 1–2 will *enable* optimizations rather than merely
stop declining" was inferred, and owed the census to implementation. Since phase 0 is blocked
by §4's corrected sequence anyway, the census was run now. One uncached real-FDB corpus run
(`go clean -testcache && go test ./pkg/relational/sqldriver/ -count=1 -v`), verbatim:

```
[sqldriver real-FDB corpus] result-type reads by consumer (RFC-213 payoff):
  bakedIntersectionKeys                    resolved 412    UNRESOLVED 0
  distinctKeyColumns                       resolved 4      UNRESOLVED 31
  planBuriedLegConcat                      resolved 252    UNRESOLVED 0
  planRowRecordType                        resolved 620    UNRESOLVED 104
  predicatesFilterIsFullPKPointProbe       resolved 14486  UNRESOLVED 0
  TOTAL                                    resolved 15774  UNRESOLVED 135
```

**135 unresolved reads, and the distribution is the finding — not the total.**

- **`distinctKeyColumns` declines 31 of 35 reads — 89%.** This is the concentrated payoff.
  A DISTINCT whose inner is a stubbed plan cannot derive its key columns and is left on the
  hash-set path.
- **`planRowRecordType` declines 104 of 724 — 14%.**
- **`predicatesFilterIsFullPKPointProbe` declines ZERO of 14,486.** This refutes the natural
  assumption — which rev 1 came close to making — that the cost model is losing point-probe
  proofs to the stub. It is not; not once over the corpus. **Whatever this RFC is worth, it
  is not worth a cost-model argument**, and that is now measured rather than assumed.
- **`planBuriedLegConcat` declines zero of 252** — the walk workaround (§3b) *succeeds* at
  finding real types, because it descends to scan leaves that have them. Its cost is the walk
  itself, not lost information. Retiring it is a simplification, not a correctness win.
- **`physicalPlanColumnNames` does not appear: zero reads.** The consumer is never reached by
  any corpus query. Stated because a silent absence and a zero are different facts. (RFC-242
  later deleted the function outright, with the union rename it fed.)

So the honest payoff claim is narrow and specific: **DISTINCT key derivation is the one place
where the stub demonstrably and repeatedly costs a plan.** Everything else is coherence.

`AssertUnresolvedResultTypeCensus` floors the *classified reads* at 1,000 (measured 15,909).
There is deliberately **no floor on the unresolved count** — RFC-213 is unimplemented, so
those reads are the defect and their number is a measurement, not a contract. What must not
happen silently is the consumers going dark, because a later "unresolved is now 0" would then
be indistinguishable from success.

---

## 7. Acceptance

- `TestResultTypeStubInventoryIsCurrent` is a zero ratchet: any unconditional
  plan-level `UnknownType` producer fails.
- `TestResultTypeConsumersFailClosed` keeps `RAW == 0`. If it ever fires, **RFC-213's framing
  is wrong and the named site is a live defect to fix ahead of this RFC.**
- `TestResultTypeStubsCreatedAtCallSites` keeps the aggregate call-site stub
  population at zero.
- `TestRecordQueryPlanRequiresGetResultValue` pins the completed interface
  inversion instead of waiting for it to happen.
- Exact result-value/result-type tests in the plan and Cascades packages cover
  the formerly stubbed plan shapes.

## 8. Measured vs. inferred

**Measured on the historical baseline:** Java's chain in both directions (§1,
file:line); the 12-plan inventory and the 13th at its call site; the
20/7/21/0 consumer split; the 135 unresolved reads and their distribution; the
fingerprint's zero-entropy field.

**Measured after RFC-232:** zero unconditional plan stubs, zero aggregate
call-site stubs, the required result-value interface, and the 1/14/31/0
consumer split. Producer-lineage recovery subsequently added the intentional
outer/inner equality pair described in the current-state ledger, yielding
1/14/33/0. Exact FirstOrDefault empty-arm materialization then added the
intentional declared-type read described there, yielding 1/14/34/0.
Correlated FlatMap construction then retired two `GetResultType()` fallbacks:
the selected-inner fallback and the planner-local exact predicate-edge helper.
Selected outer/inner edge types now come solely from
`ProvidedOutputLayout().Carrier().FlowedType()` and fail closed when that
physical layout is unavailable, yielding 1/14/32/0.
The two descendant producer walkers then retired all four of their outer/inner
`GetResultType()` reads. Two belonged to that pinned population and two had
arrived transiently with `descendantRetainedResultProducer`; carrier pointer
identity plus `OrdinalLayout.RawEqual` already proves the exact physical row
and layout, so the declared-type comparisons were redundant. The stable census
was therefore 1/14/30/0. `predicatesFilterIsFullPKPointProbe` then retired one
more declared result-type read: `scan.ProvidedOutputLayout().Carrier()` is the
single authority for both the exact scan row and the pointer-exact current
owner. `GetResultType()` could not identify that selected evaluation phase and
caused every PK point probe to over-decline after exact filter normalization.
The stable census is therefore 1/14/29/0.

**Inferred, and flagged:** that relocating the sentinel to value level will keep new stubs
from accumulating. That is a claim about *future* human behaviour, and no instrument can
measure it — the inventory gate is the closest available proxy, and it is committed.

**Withdrawn from rev 1:** "Java structurally cannot express an unknown result type" (§1);
"inverting makes the stub unrepresentable" (§4); "`distinctKeyColumns`' projection branch is a
stub workaround" (§3b); "the only definition in the entire Java tree" (unscoped, §1);
"aggregate index plans return a stored real type" (§3a).
