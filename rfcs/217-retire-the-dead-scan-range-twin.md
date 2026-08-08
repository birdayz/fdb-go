# RFC-217 — retire the dead scan-range twin

Status: implementing
Item: TODO.md CQ-34 neighbourhood (see §6)
Area: query executor — scan range binding

## 1. The fact

`scanComparisonsToTupleRange` (`executor.go:1265-1546`, 282 lines) had **zero
production callers** and 27 test call sites.

```
$ grep -rn "scanComparisonsToTupleRange(" --include='*.go' pkg/ | grep -v "_test.go"
pkg/recordlayer/query/executor/executor.go:1265:func scanComparisonsToTupleRange(...)
```

Its own definition, and nothing else. Against that, 27 hits in `*_test.go`, all
in `executor_test.go`.

The binder every scan actually executes is `bindScanComparisonsToRangeSet` /
`...WithTerminalWidening` (`scan_range_binding.go:256`, `:273`), reached from
`executor.go:268` (primary scan), `:383` (index scan), `:655` (vector partition
prefix), and `executor_new_plans.go:105`.

So the tree carried two implementations of one concept. One was executed; the
other was tested. Twenty-seven tests reported on a function no query could
reach, which is worse than no coverage: it reads as coverage.

## 1a. What Java does, and why "drift" is the wrong word

An earlier draft of this RFC said the dead twin had "silently drifted into
wrongness." That is wrong, and the correction matters more than the phrasing,
because it changes what this deletion *is*.

Java's counterpart is `ScanComparisons.toTupleRange` (`ScanComparisons.java`
:283-311, :670-674):

```java
public TupleRange toTupleRange() { Tuple low = ...; return new TupleRange(low, high, lowEndpoint, highEndpoint); }
```

A **scalar** return type — one range, exactly like the Go twin. And `grep -n NaN`
over `ScanComparisons.java`, `TupleRange.java` and `Comparisons.java` returns
**zero hits**: Java's index-bound construction has no NaN awareness at all and is
structurally incapable of emitting the negative-NaN block.

The twin therefore never drifted **from Java**. It was a faithful port on the
day it was written and it stayed Java-faithful to the end; what it became stale
against is the *live Go binder*, which was deliberately evolved past Java on
this axis (§5, `..._MultiEqualityThenInequality`). Both statements hold at once,
and the distinction is the whole point: Java-faithful, Go-stale.
**Java is the side that is wrong**, and inconsistent with itself:
`Comparisons.java:237-239` / `:757-761` evaluate `GREATER_THAN` through
`Double.compare`, where NaN is greatest — so Java's *residual predicate* says
negative-NaN rows qualify while Java's *index scan* cannot reach them. The
index and non-index plans for the same query disagree, and no Java test covers
it.

So `bindScanComparisonsToRangeSet` emitting two ranges is **a deliberate
read-side divergence from Java**, not a bug fix against it. That is sanctioned —
CLAUDE.md's "wire compat is the hard line; query reach is not" — because nothing
about the stored bytes changes; Go merely reaches rows Java's index path drops.

It is written down here because the consequence is externally visible: a
cross-engine row-diff over a table containing raw negative NaNs will show Go and
Java returning **different row sets**, and the next person to hit that must find
this paragraph rather than conclude Go has a bug. Retiring the live binder
instead of the dead one — see §8 — would have re-armed the row drop and matched
Java by reintroducing Java's own inconsistency.

**Scoped to NaN ordering** — the only axis this deletion turns on — Go has one
answer and Java has two, which is what makes the two-range emission forced
rather than optional. `values.CompareFloat64` (`cascades/values/coercion.go:26`)
is Double.compare-faithful, so NaN is greatest; the predicate path
(`predicates/comparisons.go:588`, `cmpAny`) reaches the same verdict on NaN,
because its IEEE precheck `af == bf` is false for NaN and it falls through to
`CompareFloat64`. Index and residual therefore agree that a negative-NaN row
qualifies for `> 3.14`, and a single-range emission that cannot reach those keys
is a wrong-rows bug.

This is deliberately NOT a claim that `CompareFloat64` is "the single comparator
on the logical side" — it is not. On SIGNED ZERO the two paths differ ON
PURPOSE: that same `cmpAny` precheck makes `-0.0 == +0.0` true (RFC-082), while
sort consumers call `CompareFloat64` directly and keep `-0.0 < 0.0` to match
tuple/index order. That divergence is documented at the `cmpAny` call site and
is orthogonal to this RFC; the NaN agreement above is what carries the argument.

Java holds two at once — `Comparisons` via
`Double.compare` (NaN greatest) and `RelOpValue.java:708-714` `GT_DD` via
primitive IEEE `>` (NaN never qualifies) — which is why Java is no use as an
oracle on this point.

## 2. Retraction — where RFC-179's F11 actually lives

An earlier claim held that `executor.go:1379` (the STARTS_WITH arm of the dead
function) was RFC-179's F11 fix, "unreachable from SQL and therefore provable
only by unit test". **That is retracted, in both halves.**

F11's STARTS_WITH behaviour is **live**. It is simply not in the function
credited with it: `bindScanComparisonsToRangeSet` implements it, and the two
tests that were said to prove F11 (`TestScanComparisonsToTupleRange_StartsWith`,
`_EqualityPlusStartsWith`) reproduce on the live binder unchanged — `low` and
`high` both `("abc")` with PREFIX_STRING/PREFIX_STRING endpoints. They are
re-pointed here and still pass.

The dead arm was not "unreachable pending a producer". It was unreachable from
anything, and no planner rule could ever have reached it.

## 3. What lands

Delete `scanComparisonsToTupleRange`, re-point all 27 tests at the live binder.
**No test is deleted, none is renamed, and no new test is added.**

Also deleted: `coerceTupleElement`, a two-line wrapper over
`coerceTupleElementForKey` that went dead solely from the above. Its substantive
doc comment — the FLOAT-to-float32 downcast rationale and the tuple encoder's
dispatch on runtime type — is preserved, merged onto `coerceTupleElementForKey`,
where that logic actually lives.

Twelve comment-only references to the deleted symbol elsewhere in the tree are
retargeted at the live binder (and two stale `executor.go` file-path citations
corrected to `scan_range_binding.go`). Each was checked to describe behaviour the
live binder still has, so the rename is accurate rather than cosmetic.

## 4. Why this adds NO new tests

An earlier analysis proposed four new pins on axes where the dead function was
the buggy one. **All four are already covered on the live binder.** Writing them
would be duplicate coverage, which is padding.

| Axis | Already pinned at |
| --- | --- |
| Prefix gap `[eq, EMPTY, eq]` | `scan_range_binding_test.go:1840` — literally `{equality, EmptyComparisonRange(), equality}`, asserted via `errors.As(&InvalidScanComparisonShapeError{})` with the `Component` check at `:1871` |
| NaN comparand | `scan_range_binding_test.go:1062`, `:2025` — both assert `UnsupportedPhysicalFloatEquivalenceError` |
| `col = NULL` | `TestScanRange_EqualityOverNullIsEmptyNotANullProbe` (`scan_range_null_boundary_test.go:51`) — asserts `spec.empty`, the exact assertion the proposal called new |
| `< 0.0` on DOUBLE | `TestOrderedFloatRangeSetSelectsExactlyTheLogicalRows` (`float_ordered_range_exactness_test.go:122`) — runs `ComparisonLessThan` against threshold `0.0` on `NotNullDouble` through the live binder, asserting exact logical-equals-physical selection over a domain that includes two negative-NaN encodings (`:22-23`) |

The fourth is the one worth spelling out, because it looks like a gap and is
not. A `low=(null)excl` bound that swept the negative-NaN block would be caught
by that test as *"returns a NON-qualifying key (wrong answer)"* — `NaN < 0` is
FALSE, so those keys must not be selected. The axis is covered by an exactness
proof rather than by a bound-shape assertion, which is strictly stronger.

Adding zero tests to a change of this size is the correct outcome here, not a
gap: the behaviour being deleted was never executed, and the behaviour being
kept was already pinned.

## 5. The A/B/C split, re-derived

Re-derived from scratch against this branch head rather than inherited. Buckets:
**A** = re-pointed cleanly, the live binder satisfies the same claim; **B** = the
live binder is deficient relative to the old expectation; **C** = the old claim
was wrong.

**A = 27. B = 0. C = 0.**

Every one of the 27 written claims is satisfied by the live binder. There is no
case where the live binder is deficient. Four needed the claim re-expressed
rather than re-pointed, marked A\* below.

- **A\* — the three empty-comparison tests** (`..._Empty`, `TestScanComparisons_Empty`,
  `..._EmptySlice`). The dead function returned `TupleRangeAllOf(nil)`
  (`Low=nil`, TREE_START/TREE_END); the live binder builds a non-nil empty
  prefix, so `TupleRangeAllOf` yields `Tuple{}` with RANGE_INCLUSIVE on both
  ends. Different spelling, same range. Rather than assert the new enum values —
  which would be asserting whatever the binder happened to produce — these now
  assert the *observable* key range through `ToFDBRange` against the independent
  constant `recordlayer.TupleRangeAll`. Measured equal: both spellings produce
  the byte-identical range `B..B\xff` over subspace `0x42`.
- **A\* — `..._NullComparand_EmptyRange`.** The dead function expressed
  unsatisfiability as a degenerate range (`Low==High`, INCLUSIVE/EXCLUSIVE); the
  live binder expresses it structurally as `spec.empty` with `materialize == nil`,
  so no range is opened at all. The test now asserts all three. Strictly stronger.

Two further notes that are not bucket changes but are findings:

- **`..._StartsWithPlusInequality_Loud`** — the live binder rejects this earlier
  and more precisely. The dead function fell through its tail combiner to a bare
  `fmt.Errorf`; the live binder returns a typed
  `*InvalidScanComparisonShapeError{Component: 0}`. Asserted with `errors.As`,
  never a substring.
- **`..._MultiEqualityThenInequality` — the dead function was incomplete.**
  `> 3.14` on a DOUBLE key opens **two** physical ranges, not one: the finite
  tail above the comparand, plus the negative-NaN block, which is logically
  greatest (all NaNs are) but sorts *below* -Inf in tuple order. The dead
  function emitted only the first, so it silently dropped logically-qualifying
  rows. The old test never asserted a range count, so its written assertions are
  satisfied by `ranges[0]` verbatim — hence A, not C. But the behaviour it was
  reporting on was wrong, and had anything ever called it, that would have been a
  wrong-rows bug. This is the sharpest argument for the deletion: the dead
  function had gone stale relative to the LIVE Go binder (not relative to Java —
  see §1a; it matched Java to the end), and the tests pointed at the stale one.

## 6. Relation to CQ-34

CQ-34 describes the sargable gate and the range builder kept in manual lockstep,
and NAMED `scanComparisonsToTupleRange` as one of the two halves. That citation
went stale with this deletion and has since been repointed in place: the entry
now names `bindScanComparisonsToRangeSet` (`scan_range_binding.go:256`) and
carries a parenthetical recording the move and warning that the line numbers
quoted further down in it still refer to the deleted function. The
lockstep concern itself is unaffected by this change and remains open — this RFC
removes a decoy, it does not close CQ-34.

## 7. Proof obligations

**The 1M stress comparison does not apply here, and this is an argument on the
merits rather than a skipped gate.** A stress run measures plan choice and
execution latency. This change deletes a function with zero production callers
and edits tests; the compiled production path is unchanged *by construction*, so
there is no mechanism by which a plan could move. A stress delta here would be
measuring only the disks and the neighbours.

What replaces it, all discharged:

1. **No definition and no caller — a COMMIT-PINNED historical measurement, not a
   standing invariant.** §1 shows the PRE-deletion state: the non-test grep
   matched the function's own definition and nothing else. Post-deletion, both
   greps below were run at commit `0dd2736fe`, and what is recorded here is the
   *set of locations they matched at that commit* — `grep -rn` also prints each
   matching line's source text, which is elided below:

   ```
   # at 0dd2736fe
   $ grep -rn "scanComparisonsToTupleRange(" pkg/
   (exit 1, no output)

   $ grep -rn "scanComparisonsToTupleRange" pkg/
   pkg/relational/sqldriver/sargability_differential_oracle_fdb_test.go:13
   pkg/relational/sqldriver/negative_zero_index_sarg_probe_test.go:143
   ```

   Both remaining hits are PROSE in comments that name the retired function in
   order to point the reader at the live binder. Neither is a compiled reference.
   That is the classification: two prose hits, zero code hits.

   The first grep is **not durable and must not be read as one**. It is a
   substring match, not a Go parse, and a normal edit falsifies it in either
   direction: a comment that happens to write `scanComparisonsToTupleRange(` trips
   it with no code present, while a generic call `scanComparisonsToTupleRange[T](`,
   a function-value reference passed without parentheses, or `name /* c */ ()`
   would all evade it with code present. It is also unrestricted by extension, so
   it would match non-Go files. Re-deriving the post-condition after any later
   change means re-running it (and preferring a real reference search), not citing
   this block.
2. **plandiff and explaindiff goldens byte-identical.** The reproducible check is
   the three-dot diff against the merge base, not `git status` (which is empty at
   a clean HEAD and says nothing about branch-versus-base):

   ```
   $ git diff --stat master...HEAD -- pkg/relational/conformance/
   pkg/relational/conformance/rowdiff/gen.go | 6 +++---
   ```

   No file under `pkg/relational/conformance/{plandiff,explaindiff}/` is touched;
   the only conformance change on the branch is the comment-only edit in
   `rowdiff/gen.go`.
3. **Suite results below are UNRETAINED.** Both suites were observed green
   (`plandiff 35.5s ok`, `explaindiff 13.9s ok`), as was the full suite before
   commit, but no artifact of those runs was kept, so these are reported
   timings rather than reproducible evidence. Re-run them if the claim matters.
4. **Every one of the 27 tests still exists and still passes** — verified by name.

## 8. Rejected alternatives

- **Keep the function and mark it deprecated.** It is a second implementation of
  a live concept that had already gone stale relative to it (§5, the negative-NaN
  block) — stale against the live Go binder, not against Java, which it still
  matched. A deprecated twin goes staler still and keeps reading as coverage.
- **Delete the function and its 27 tests.** A test count dropping by 27 with no
  analysis is exactly the false green this change exists to remove. Every test
  was landed in a bucket instead.
- **Keep the tests pointed at a thin shim.** Same defect, one indirection later.
- **Retire the LIVE binder and keep the twin** — i.e. treat the twin's single-range
  emission as the correct behaviour and delete the plural binder instead. This is
  the alternative §1a points here for, and it is rejected on the merits rather than
  on convenience: it would re-arm the negative-NaN row drop (§5,
  `..._MultiEqualityThenInequality`) and would match Java only by reintroducing
  Java's own internal inconsistency, where the residual predicate says negative-NaN
  rows qualify and the index scan cannot reach them. Go cannot hold both answers
  **on this axis**: it does have two float comparators — the PREDICATE one
  (`cmpAny`, `predicates/comparisons.go:588`) and the SORT/index one
  (`values.CompareFloat64`) — but they are split on SIGNED ZERO, not on NaN
  (`plans/ordering.go:401-409`). For NaN the predicate path never reaches its own
  IEEE branch: `af == bf` is false for NaN, so `cmpAny` falls straight through to
  `values.CompareFloat64` (`comparisons.go:598-604`), the same total order the
  index bounds use. Predicate and index therefore agree about negative NaN by
  construction, which is exactly what Java does not do.
