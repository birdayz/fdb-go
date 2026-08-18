# RFC-234: A Type is immutable, so stop rebuilding it

**Status:** PROPOSED (v2). v1 was NAK'd twice — once for pricing an
object-count argument in space, once for a gate rule that would have failed 17
legitimate sites. See §9.
**Base:** branch `perf/plan-structural-hash-memo`, PR #754
**Relates to:** RFC-233 (a type comparison must not build a type), RFC-232
**Wire impact:** none. No key, record, index, continuation, or protobuf plan
encoding changes. Read-side planner only.

## 1. Decision

`ExactTypeHandle.Type()`, `QuantifiedObjectValue.FlowedType()` and
`fieldValue.Type()` return the SHARED thawed graph rather than a fresh one, and
the immutability that makes sharing safe becomes an enforced gate covering
tests as well as production sources.

The defensive rebuild those three perform protects a mutability that, measured
over the whole tree, no production code uses — and that exactly four test files
DO use, which is the substantive cost of this change and is priced in §7.

## 2. Why — measured with the instrument the argument is about

RFC-233 was motivated by a census of call SITES and delivered 0.3% of allocation
volume, because a census says nothing about the allocation attributable to it.
v1 of THIS RFC then made a subtler version of the same mistake: it argued that
object COUNT is the term that matters and priced its caller table in SPACE. Both
tables are given below, labelled, and the argument uses the object one.

All figures from `go test -memprofile` over
`TestStatsInvariant_PurePlannerSweep` at `48c128d59`, 175s. Units are pprof's
own: it prints `MB` for 2^20 bytes, so its `112924.83MB` is **110.28 GiB**.
Total **1,525,153,551 objects**.

| node | objects | share |
|---|---|---|
| `exactType.thaw` | **175,709,271** | **11.52%** |
| next-largest (`io.WriteString`) | 56,285,055 | 3.69% |

`thaw` is the largest single object allocator in the planner, at more than three
times the next node. Its callers:

| caller | objects | share of thaw | (space share) |
|---|---|---|---|
| `QOV.FlowedType` | 45.63M | **25.97%** | 32.8% |
| `exactType.Type` | 43.81M | **24.93%** | 30.9% |
| `fieldValue.Type` | 39.68M | **22.58%** | 8.4% |
| `physicalFlowedRecordType` | 32.02M | **18.22%** | 22.7% |

`fieldValue.Type` is a top-THREE caller by objects and looks negligible by
space — the single clearest reason to fix the currency before arguing from the
table.

The first three are the same thing wearing three names: a public accessor
building a fresh ordinary graph out of an immutable handle, every call, so the
caller MAY mutate it. Together **129.12M objects, 73.48% of thaw and 8.47% of
everything the planner allocates.**

The planner is allocation-bound. From a CPU profile of the same test at the same
head (`-cpuprofile`, 169.6s): `runtime.gcBgMarkWorker` is **45.42% cumulative**,
against `Planner.plan` at 47.65%. v1 quoted a `gcDrain` figure taken from a
different tree and cited it as if it came from the memory profile, which has no
CPU samples at all.

## 3. The experiment — run BEFORE proposing the expensive half

`thawShared()` already ships. Flipping the three accessors to it is six lines and
needs no gate, so the RFC is refutable in three minutes: if object count does not
fall by ~73% of thaw, the handles are not re-thawed often enough for the cache to
pay and the proposal dies.

Done, at `48c128d59` + the six-line flip:

| | before | after | delta |
|---|---|---|---|
| `exactType.thaw` objects | 175,709,271 | **46,843,187** | **−73.34%** |
| `thaw` share of all objects | 11.52% | **3.35%** | |
| total objects allocated | 1,525,153,551 | **1,400,360,164** | **−8.18%** |

The three accessors vanish from thaw's caller list entirely; what remains is
`physicalFlowedRecordType` (68.92% of the much smaller thaw) and
`resolveAgainstQOV` (22.36%). The predicted 73.48% and the measured 73.34% agree
to within a tenth of a point.

Wall clock on that run was 166.3s against ~170.7s for the same head, but that is
n=1 and is NOT the claim; §8 states how it will be measured.

## 4. Java is the reference, and Java's Type is immutable

Measured over `.../cascades/typing/Type.java`: **35 `private final` field
declarations, 0 public setters, 0 non-final instance fields** — the thirteen
lines a loose "declared but not final" pattern matches are all nested `static
class` declarations, checked individually.

```sh
J=.../cascades/typing/Type.java
grep -o 'private final' $J | wc -l            # 35 (occurrences, not lines)
grep -cE 'public void set[A-Z]' $J            # 0
```

The decisive one is not a count:

```java
public List<Field> getFields() {
    return Objects.requireNonNull(fields);
}
```

The internal list handed straight out, no defensive copy anywhere in the class.
Java shares its Type graphs with every caller and does not think about it,
because immutability is in the type system. Go's exported fields make that
inexpressible, and the defensive copy is Go's substitute — which is a standing
admission that immutability is the real answer.

## 5. Production Go is already immutable in fact

A go/types census — not a grep — over every assignment, `++`/`--`, and
address-of whose target resolves to a field of a `values.Type` implementation or
of `Field`/`EnumValue`, its value-typed members. Population `./pkg/...` plus
`./cmd/...` and `./gen/...`: **103 packages, 0 errors, 21 hits.**

| target | count | shape |
|---|---|---|
| `RecordType.Legs` | 4 | assignment |
| `Field.Ordinal` | 9 | assignment |
| `Field.Name` | 4 | assignment |
| `Field.FieldType` | 4 | assignment |
| any `++`/`--` | **0** | — |
| any `&t.Field` | **0** | — |

The two zeros carry positive controls: injecting `rt.Fields[0].Ordinal++` and
`return &rt.Nullable` into a real file makes the census report one of each, so
they are measurements rather than an arm that cannot fire. A widening to `./...`
adds three more packages and no hits.

All 21 were read individually. Every one writes a graph its own function
constructed one to six lines earlier — with one exception that has to be stated
rather than folded into the count.

**`restoreQOVRecordLayout` writes `record.Legs` on a PARAMETER**, and recurses
into `record.Fields[i].FieldType`. It is safe only because its single caller
passes a fresh `thaw()`. That is a one-caller invariant, it is not expressed
anywhere today, and §6's gate must express it — particularly since §7 nominates
that same caller as the next optimization.

The name-only version of this census reported 22 hits and was WRONG about 18 —
`semantic.Column` and `api.ColumnDescriptor` also have a `Nullable` field. A
census that cannot tell those apart cannot support a claim about Type
mutability, which is why this one resolves through `go/types`.

## 6. The gate

The rule is about PROVENANCE, not about packages. v1 said "fails on any
assignment from a package other than `values`", which would have failed 17 of the
21 legitimate sites — they live in `executor`, `cascades`, `embedded` and
`core/query`. The correct rule:

> An assignment into a `values.Type` field is allowed only when the graph being
> written was constructed by the same function, or was received under an
> explicit contract that says so.

Concretely the gate must catch four shapes v1 did not enumerate:

1. the shallow copy-on-write `withLegs := *record`, which SHARES `Fields` with
   its source — so `withLegs.Fields[i].X = …` writes the interned graph even
   though `withLegs` looks local;
2. a write through a parameter, which is `restoreQOVRecordLayout`'s shape and
   needs the one-caller invariant named at the site and asserted;
3. `++`/`--` (`*ast.IncDecStmt`, not `*ast.AssignStmt`) and address-of;
4. **test files.** v1 excluded them and §7 shows that is exactly backwards.

The gate must also fail loudly on a package that does not load. The scratch
instrument printed a REDUCED count when a package had errors, which is the
shrinking-population failure this repo keeps naming.

## 7. The real cost is in the tests, and it is measured

Running the full suite under the §3 flip: **70 of 72 targets pass, 2 fail, 7
tests**.

| test | what it asserts |
|---|---|
| `TestRFC232QOVSnapshotsAndDefensivelyThawsItsType` | mutating one `Type()` reaches the next |
| `TestOrdinalLayoutSnapshotsEveryMutableInputAndGetter` | getter results are private |
| `TestFullUnorderedScanStoresExactTypeAndCanonicalNames` | same |
| `TestReferencePreparedApplyAndReadsAreDefensive` | same |
| `TestRequireFlowedObjectValueReturnsExactView` | same |
| `TestFlowedHelpersRefuseAValueThatCannotStateARow` | collateral |
| `TestFlowedTypeHelpersAnswerRatherThanCrash` | collateral |

The last two are the finding. Neither asserts anything about defensive copying;
they fail because **a sibling test mutated a shared graph and corrupted the
interned handle underneath them.** `TestRFC232QOVSnapshotsAndDefensivelyThawsItsType`
writes `first.RecordName = "THAW-MUTATED"` on a `Type()` result; interning keys
on shape, so another test that snapshots the same shape gets the corrupted node,
and `FlowedTypeEquals(good, its own row)` answers false.

That is cross-test corruption through an interned handle, under `t.Parallel()`,
and it is order-dependent. It is the strongest argument in this RFC for the gate
covering test files — a rule that stopped at production sources would have left
exactly this in place.

## 8. Scope, and what is NOT in it

`physicalFlowedRecordType` stays out, and the exclusion is priced in the same
currency as the target rather than the one that flatters it: **32.02M objects,
18.22% of thaw and 2.10% of all objects** — 2.10% against the target's 8.47%, a
quarter of the size. It thaws and then WRITES leg boundaries into the result, so
it cannot simply share; the no-layout path could, and that split is the obvious
follow-on, but it is a separate change with its own aliasing question (the result
escapes into `OrdinalSeedLegWindow.Typ`).

`resolveAgainstQOV` (10.31M, 5.87% of thaw) is likewise left alone.

Migrating individual call sites to the already-shipped `SharedFlowedType` is
NOT the alternative: 102 non-test `.FlowedType()` occurrences remain after
RFC-233, each needing a per-site retention judgement, to reach a subset of what
flipping the accessor reaches in six lines. The accessor flip plus a gate is
both smaller and stronger.

## 9. Expected effect, and how it will be measured

No predicted wall-clock number. The measured allocation effect is −124.8M
objects (−8.18%) and −73.3% of thaw. Two prior data points bracket the
translation: the structural-hash memo removed ~8.9 GB for 2.8%; RFC-233 removed
~1.6 GB for ~1.4%.

The claim to test is the ratio on `TestStatsInvariant_PurePlannerSweep`, taken as
ADJACENT base/head pairs, n>=3, against `d31bf28e0` — the current merge-base,
named as a SHA because a ratio called "vs master" expires. Current position:
**0.958x**.

## 10. What v1 got wrong

Recorded rather than revised away.

It priced an object-count argument in SPACE — the four caller shares were
`-sample_index=alloc_space` to the decimal, inside a section declaring that
object count is what matters. `fieldValue.Type` reads as 8.4% there and is 22.58%
by objects. It then multiplied a space share (72.1%) by an object total to get
"~127M objects"; the honest derivation is 73.48% → 129.12M.

Worse, §7 excluded `physicalFlowedRecordType` by quoting its SPACE share while
the target was inflated with objects — instrument-shopping in a single document.

Its gate rule would have failed 17 of the 21 sites the same RFC blesses, and its
census claimed "all non-test sources" while loading only `./pkg/...`.

And it cited a `gcDrain` percentage as if measured on a profile that contains no
CPU samples.

The common shape with RFC-233 §7 and §8: a number produced by one instrument,
reported as another's. The remedy that worked here was the §3 experiment —
three minutes to make the central claim falsifiable before writing anything
expensive.
