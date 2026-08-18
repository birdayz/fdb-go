# RFC-234: A Type is immutable, so stop rebuilding it

**Status:** PROPOSED
**Base:** branch `perf/plan-structural-hash-memo`, PR #754
**Relates to:** RFC-233 (a type comparison must not build a type), RFC-232
**Wire impact:** none. No key, record, index, continuation, or protobuf plan
encoding changes. Read-side planner only.

## 1. Decision

`ExactTypeHandle.Type()`, `QuantifiedObjectValue.FlowedType()` and
`fieldValue.Type()` return the SHARED thawed graph rather than a fresh one, and
the immutability that makes sharing safe becomes an enforced gate rather than a
convention.

The defensive rebuild those three perform exists to protect a mutability that,
measured over the whole tree, no production code uses.

## 2. Why — measured, not censused

RFC-233 was motivated by a census of call SITES and delivered 0.3% of allocation
volume, because a census says nothing about the allocation attributable to it.
This one is motivated by the allocation profile directly, which is the instrument
that refuted the last one.

Over `TestStatsInvariant_PurePlannerSweep` at `a017f0564` (`go test -memprofile`,
175s, 112.92 GB / 1.525B objects):

| node | space | objects |
|---|---|---|
| `exactType.thaw` | 7.23 GB (6.40%) | **175.7M (11.52%)** |

`thaw` is the largest single object allocator in the planner by a factor of three
over the next node. Its callers, by share of thaw:

| caller | share |
|---|---|
| `QOV.FlowedType` | 32.8% |
| `exactType.Type` | 30.9% |
| `physicalFlowedRecordType` | 22.7% |
| `fieldValue.Type` | 8.4% |

The first, second and fourth are the same thing wearing three names: a public
accessor that builds a fresh ordinary graph out of an immutable handle, every
call, so the caller may mutate it. Together **72.1% of thaw — 5.21 GB and ~127M
objects, 8.3% of everything the planner allocates.**

The planner is allocation-bound: on the same run `runtime.gcDrain` is 42.72%
cumulative and no application node exceeds ~2.3% flat. Object COUNT is the term
that matters most there, and this is where it is.

## 3. Java is the reference, and Java's Type is immutable

Measured over `.../cascades/typing/Type.java`: **35 `private final` field
declarations, 0 public setters, and 0 non-final instance fields** — the thirteen
lines a loose "declared but not final" pattern matches are all nested `static
class` declarations, checked individually. Collections are built with
`ImmutableList` / `ImmutableBiMap`.

```sh
J=.../cascades/typing/Type.java
grep -o 'private final' $J | wc -l            # 35 (occurrences, not lines)
grep -cE 'public void set[A-Z]' $J            # 0
```

The decisive one is not a count. `Type.Record.getFields()` is

```java
public List<Field> getFields() {
    return Objects.requireNonNull(fields);
}
```

— the internal list handed straight out, with no defensive copy anywhere in the
class. Java shares its Type graphs with every caller and does not think about it,
because immutability is in the type system.

Go's `RecordType`, `PrimitiveType`, `ArrayType`, `EnumType` and `RelationType`
have EXPORTED fields, so the same guarantee is not expressible in the type
system. The defensive copy is Go's substitute for it — and "Go's substitute for
X" is a standing admission that X is the real answer.

## 4. Go's Types are already immutable in fact

Established by a go/types census, not a grep. Every assignment whose target
resolves to a field of a `values.Type` implementation (or of `Field` /
`EnumValue`, its value-typed members), over all non-test sources:

```sh
# scratch instrument; see §5 for the shipped gate
go run typemut2/main.go     # packages.Load("./pkg/...")
```

**78 packages loaded, 0 errors, 21 assignments.** They are:

| target | count | provenance |
|---|---|---|
| `RecordType.Legs` | 4 | all four write a struct the same function just copied or built |
| `Field.Ordinal` | 9 | all into a local slice or a `Field` VALUE copy |
| `Field.Name` | 4 | same |
| `Field.FieldType` | 4 | same |

Zero assignments to `RecordName`, `Nullable`, `Fields`, `ElementType`,
`TypeCode`, `EnumName`, `Values` or `InnerType` on a Type anywhere in
non-test main sources. Every one of the 21 was read individually; each writes a
graph its own function constructed one to six lines earlier.

The name-only version of this census reported 22 hits and was WRONG about 18 of
them — `semantic.Column` and `api.ColumnDescriptor` also have a `Nullable`
field. A census that cannot tell those apart cannot support a claim about Type
mutability, which is why this one resolves through `go/types`.

## 5. The gate

The census ships as a `pkg/docscheck` test. It fails the build on any assignment
into a `values.Type` field from a package other than `values` itself, and inside
`values` on any assignment that is not into a freshly constructed local — the
four `Legs` sites, which are copy-on-write by construction and are listed
explicitly.

It also has to catch what the scratch version does not: `&t.Nullable` taken as a
pointer, and a write through a `Field` slice reached from a Type rather than
from a local. Both are additions to the same walk.

The gate is the load-bearing half of this RFC. Without it, sharing is safe today
and silently unsafe the first time someone writes `rt.Nullable = true` on a graph
they did not build — a mutation that would then reach every holder of that type,
including a memo's key.

## 6. The pin that must be reconciled, not deleted

`TestRFC232QOVSnapshotsAndDefensivelyThawsItsType` asserts two different things
and only one of them changes.

Its FIRST half — mutating the caller's supplied graph after construction cannot
reach the QOV — is about `SnapshotExactType` copying, stays exactly as it is, and
is if anything more important once graphs are shared.

Its SECOND half — mutating one `Type()` result cannot reach the next — is the
contract this RFC retires. Per the guard-shelf-life rule the direction inverts:
the alarm stops being "the graph is shared" and becomes "a Type was mutated at
all", which is what §5's gate watches. The test half is replaced by an assertion
that two `Type()` calls return the SAME pointer, so a future reintroduction of
the defensive copy is caught as the regression it would be.

## 7. What is NOT in scope

`physicalFlowedRecordType` (22.7% of thaw) stays. It thaws and then WRITES leg
boundaries into the result, so it cannot share, and memoizing it needs a per-QOV
cell — the shape already measured at ~0.25% of planner time for plans, against a
target worth a fraction of that. Priced and rejected in RFC-233 §5; unchanged
here.

## 8. Expected effect

No predicted wall-clock number. Two data points bracket it: the structural-hash
memo removed ~8.9 GB and bought 2.8%; RFC-233 removed ~1.6 GB and bought ~1.4%.
5.2 GB and 127M objects sits between them, and object count should weight it
higher than space alone suggests, since `gcDrain`/`scanSpan` walk objects.

The claim to test is the ratio on `TestStatsInvariant_PurePlannerSweep`, taken as
ADJACENT base/head pairs, n>=3, against `d31bf28e0` — which is the current
merge-base, stated as a SHA because a ratio named "vs master" expires.
Current position: **0.958x**.
