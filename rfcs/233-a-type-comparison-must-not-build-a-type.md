# RFC-233: A type comparison must not build a type

**Status:** PROPOSED (v2). v1 was NAK'd on a false premise; see §7.
**Base:** `34e6f0dc1` (branch `perf/plan-structural-hash-memo`, PR #754)
**Relates to:** RFC-232 (exact `FieldValue`/QOV resolution), RFC-197
**Wire impact:** none. No key, record, index, continuation, or protobuf plan
encoding changes. Read-side planner only.

## 1. Decision and scope

Comparing two flowed row types, or asking whether one exists, must not allocate
an ordinary `Type` graph.

After this change:

- type EQUALITY between exact-carrying values is answered by `exactTypesEqual`;
- row-SHAPE agreement is answered by `exactRowShapesAgree`;
- an existence check is answered from handle nil-ness;
- a value carrying no handle falls back to `SharedFlowedType` rather than
  `FlowedType()`, so a miss costs a cache read instead of a graph rebuild;
- `Type()` and `FlowedType()` keep returning a FRESH graph, unchanged.

Three of those four primitives already exist. This RFC is mostly ROUTING.

## 2. Why — the measurement

The RFC-232 gap resisted CPU profiling for a structural reason rather than an
elusive one: **the planner is allocation-bound.** Measured over a 171s run of
`TestStatsInvariant_PurePlannerSweep`:

| runtime node | cumulative |
|---|---|
| `runtime.gcDrain` | **42.72%** |
| `runtime.scanSpan` | 31.61% |
| `runtime.mallocgc` | 16.78% |

No application node exceeds ~2.3% flat. A diffuse CPU profile is the EXPECTED
output for a workload shaped like this, which reframes the earlier "nothing above
2%" result from a dead end into a diagnosis.

Over the same sweep, 112.01 GB total:

| site | alloc | share |
|---|---|---|
| **`exactType.thaw`** | **8.87 GB** | **7.92%** |
| `GetCorrelatedToOfValue.func1` | 8.17 GB | 7.29% |
| `newStructuralKey` | 8.10 GB | 7.23% |

`thaw` is the single largest allocator in the planner, and the waste concentrates
in comparisons: `a.FlowedType().Equals(b.FlowedType())` allocates two complete
graphs to produce one bool.

The census below is stated in OCCURRENCES, because the first draft of this table
reported `grep -c` output as call-site counts. `grep -c` counts LINES, and **50**
of these lines carry two calls — which is exactly what the pattern above looks
like. The distribution is 91 lines with one call and 50 with two, so
91 + 2×50 = 191 over 141 lines; the arithmetic is stated because the first
correction of this row said 41, and 141 + 41 = 182 ≠ 191 refutes it without
needing to re-run anything.

Population: non-test sources under `pkg/`. Every command below is pasteable AS
WRITTEN and carries that population itself — the previous draft printed bare
patterns with no path, no `--include` and no test exclusion, which reproduce
different numbers (`grep -ro '\.FlowedType()'` alone gives 392). Note the quoted
glob: unquoted, fish does not expand `--include=*.go` and the command silently
returns zero.

```sh
# 141 lines / 191 occurrences
grep -rno '\.FlowedType()' pkg --include='*.go' | grep -v '_test\.go:' | wc -l
grep -rno '\.FlowedType()' pkg --include='*.go' | grep -v '_test\.go:' \
  | awk -F: '{print $1":"$2}' | sort -u | wc -l
# 33 Equals-shaped lines
grep -rn "\.FlowedType()\.Equals(\|Equals([a-zA-Z_]*\.FlowedType())" pkg --include='*.go' \
  | grep -v '_test\.go:' | wc -l
# 13 existence checks
grep -rno '\.FlowedType() [=!]= nil' pkg --include='*.go' | grep -v '_test\.go:' | wc -l
```

| shape | measured |
|---|---|
| `.FlowedType()` total | **141 lines / 191 occurrences** |
| `Equals`-shaped | 33 lines |
| existence checks | 13 |

`QuantifiedRowShapesAgree` is counted SEPARATELY and is NOT a subset of the rows
above: 16 real call sites (20 raw identifier hits, less 3 prose comments and the
definition), of which only 10 also carry a `.FlowedType()` call. Presenting it
inside the same partition was a second error in the same table.

## 3. Design

### 3.1 `exactTypesEqual`, NOT pointer identity

Handle identity is **strictly stricter** than `Type.Equals`, so substituting it
would be wrong in the plan-changing direction. `RecordType.Equals` compares
`Nullable` and `Fields` and deliberately ignores `RecordName` (matching Java);
`EnumType.Equals` ignores `EnumName` for the same reason. The intern probe does
the opposite — `exactProbe.matches` rejects on `existing.name != p.name` — and
`exact_type_intern.go`'s own header states the asymmetry as intentional:

> IDENTITY HERE IS FINER THAN CANONICAL IDENTITY, deliberately. … Two types that
> are `exactTypesEqual` may still be two objects; **nothing depends on the
> converse.**

Two records differing only in `RecordName` therefore give `Equals=true` with
`handleIdentity=false`. That is a FALSE NEGATIVE: the planner would conclude the
types differ where they are equal.

`exactTypesEqual` is the correct primitive and already exists: pointer fast path,
then hash, then `bytes.Equal` on canonical bytes — and the canonical encoding
excludes the name **precisely because `Equals` does**. It is Equals-equivalent by
CONSTRUCTION rather than by an interning premise, while keeping the O(1) win for
the dominant same-pointer case.

### 3.2 No carve-out for shape agreement

v1 proposed treating `QuantifiedRowShapesAgree` as fast-TRUE-only because it is
nullability-tolerant. That reasoning was sound but unnecessary:
`exactRowShapesAgree` already exists as its exact-handle twin, built on
`exactTypesEqual` and already nullability-tolerant. It is symmetric. Use it.

### 3.3 The design does not depend on interning completeness

A consequence worth stating, because it retires a gate v1 leaned on.
`exactTypesEqual` compares canonical bytes, so it is correct even for a node that
never entered the table. That matters concretely: `sharedPrimitiveExactType`'s
fallback for an unadmitted code builds `&exactType{…}` directly, bypassing
`internedExactType`, and only calls `finishCanonical()`. Under pointer identity
that node would compare unequal to everything forever; under `exactTypesEqual` it
compares correctly.

So `TestEveryExactNodeIsInterned` is NOT a correctness gate for this change —
which is good, because it drives only primitive, record and relation shapes, and
never enum, array or `anyRecord`. It remains a valuable SHARING pin; it is simply
not load-bearing here.

### 3.4 Make the fallback cheap, and the win reaches every read site

For a value carrying no handle, route to `SharedFlowedType` / `SharedExactType`
rather than `FlowedType()`. Those read `thawCache` on the interned node, so a
miss costs a cache read instead of a rebuild. This extends the benefit past the comparison sites to every site that only READS
the type — all 191 occurrences, not just the 33 Equals-shaped lines.

## 4. `Type()`'s fresh-graph promise stays

Narrowly stated, because v1 got this factually wrong: **thaw is already
memoized.** `thawCache` + `thawShared` cache the thawed graph on the interned
handle, and it ships publicly as `SharedFlowedType`/`SharedExactType`. What
cannot be memoized is the PUBLIC promise that `Type()`/`FlowedType()` hand back a
graph the caller may mutate — pinned by `rfc232_qov_exact_identity_test.go`,
which mutates a `Type()` result and requires a later one to be unaffected.

Changing that contract is REJECTED. It would mean loosening a defensive
immutability guarantee across 141 call SITES spread over 191 occurrences to buy what `SharedFlowedType`
already provides at zero aliasing risk.


### 4.1 Why not just route comparisons through `thawShared()`?

The obvious cheaper-looking alternative, and it must be answered here or the next
engineer will "simplify" this change into the weaker one.

`thawShared()` gets you amortized-zero ALLOCATION — its doc cites ~4M saved
allocations on a 20k-row scan — but the comparison it feeds is still
`Type.Equals`, an O(n) walk over the thawed graph. `exactTypesEqual` is O(1) in
the dominant same-pointer case and, failing that, a hash compare plus one
`bytes.Equal` over a canonical encoding. So routing through `thawShared` fixes
the allocation and leaves the walk; comparing handles fixes both.

That is also why §3.4 routes only the READ-ONLY fallback through
`SharedFlowedType`: for a value that carries no handle there is nothing to
compare by handle, so amortizing the rebuild is the whole available win.
## 5. Expected effect

Deliberately no predicted wall-clock number: allocation volume and time are
different currencies, and this campaign was already burned once assuming
otherwise — 8.9 GB attributed to `newStructuralKey` bought 2.8% when memoized.

The claim to test is the ratio on `TestStatsInvariant_PurePlannerSweep`, n>=2 per
side, sequential, against the true merge-base. Current position: **1.150x** vs
master (from 1.184x pre-memo); end-to-end 1M is 1.020x.

## 6. Risk and the required pin

The risk is entirely in the arithmetic, not the fallback — v1 asserted the
opposite and that inversion is what let the false premise through. Both existing
name tests (`TestRecordTypeEqualsIgnoresRecordName`,
`TestExactInterningKeepsRecordNamesApart`) stay GREEN under the wrong
substitution, because neither observes a comparison SITE.

So the required pin is a differential one, and it is BUILT rather than described:
`exact_type_equality_differential_test.go` sweeps all 3,364 ORDERED pairs of a 58-entry
type corpus and requires `exactTypesEqual` to equal `Type.Equals` and
`exactRowShapesAgree` to equal `QuantifiedRowShapesAgree`, pair for pair. Ordered
rather than unordered because `Equals` dispatches on the receiver's concrete type,
so a mismatched pair takes two different code paths depending on which side is the
receiver.

It was verified by MUTATION, not by passing: replacing `exactTypesEqual`'s body
with `return left == right` — v1's design, exactly — produces **22 disagreements**,
naming the record-name pairs, the enum-name pairs and a nested-inner-name pair.
Both existing name tests stay green under that same mutation. The corpus also
carries its own vacuity guard, because the sweep is only as strong as its
population: a separate arm asserts that at least one `Equals`-equal pair holds two
DISTINCT handles, and names the record and enum pairs specifically, so a corpus
that keeps some disagreement while losing either half still fails.

## 7. What v1 got wrong

Recorded rather than silently revised, because the error is instructive.

v1 claimed handle identity was exactly `Type.Equals` "in BOTH directions", citing
interning. The converse direction is false by deliberate design, stated in the
interning file's own header — the doc was there and the RFC contradicted it. v1
also asserted thaw was un-memoized when `thawCache` has shipped all along, and
its risk section claimed the arithmetic was safe and the fallback was the risk,
which is backwards.

The common shape: every one of those was a claim about the code written from
intent rather than from reading it. That is the same failure CLAUDE.md's
scope-sentence rule names for gates, appearing here in a design document.

v1 also built its motivation table out of `grep -c` output and reported it as
call-site counts. `grep -c` counts LINES: the population is 141 lines but 191
occurrences, because 50 lines carry two calls — which is precisely the
`a.FlowedType().Equals(b.FlowedType())` shape the RFC is about. The
`QuantifiedRowShapesAgree` row was worse: a raw identifier count including three
prose comments and the definition, presented as a subset of a population only 10
of its lines belong to. Every error ran the same direction, overstating the win.

That one is not merely instructive, it is embarrassing in a useful way: the
`grep -c` counts LINES rule was committed to CLAUDE.md in the same push as the
table that violated it.

And v2's FIRST correction of that row was itself wrong: it said 41 lines carry
two calls, when the figure is 50. The number was never measured — it was
back-derived, and the sentence announcing that the previous draft had shipped an
unmeasured count shipped an unmeasured count. Worse, the document already
contained its own refutation: 141 + 41 = 182, not 191. That is why the
distribution and the arithmetic now sit in §2 next to the number rather than
behind it — a total that does not decompose is a number nobody can check, which
is the whole complaint against the first version.

The stale v1 claims had also settled into `TODO.md` (the 62-of-141 figure, the
"not an approximation" premise, and the "do NOT memoize thaw" instruction), where
the next reader would have found them. Correcting only the copy you remember
writing is not correcting the claim; that entry now carries the same census and
points back here.
