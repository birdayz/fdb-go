# RFC-217 — retire the dead scan-range twin

Status: implementing
Item: TODO.md CQ-34 neighbourhood (see §6); companion to RFC-216
Area: query executor — scan range binding

## 1. The fact

`scanComparisonsToTupleRange` (`executor.go:1265`, 283 lines) had **zero
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

The twin therefore never drifted. It was a faithful port on the day it was
written. **Java is the side that is wrong**, and inconsistent with itself:
`Comparisons.java:237-239` / `:756-759` evaluate `GREATER_THAN` through
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

Go has exactly one float semantic to be consistent with, which is what makes the
two-range emission forced rather than optional: `values.CompareFloat64`
(`cascades/values/coercion.go:26`) is Double.compare-faithful and is the single
comparator on the logical side. Java holds two at once — `Comparisons` via
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
| Prefix gap `[eq, EMPTY, eq]` | `scan_range_binding_test.go:1839` — literally `{equality, EmptyComparisonRange(), equality}`, asserted via `errors.As(&InvalidScanComparisonShapeError{})` with the `Component` check at `:1870` |
| NaN comparand | `scan_range_binding_test.go:1061`, `:2024` — both assert `UnsupportedPhysicalFloatEquivalenceError` |
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
  wrong-rows bug. This is the sharpest argument for the deletion: a dead function
  had drifted from the live one, and the tests pointed at the dead one.

## 6. Relation to CQ-34

CQ-34 describes the sargable gate and the range builder kept in manual lockstep,
and names `scanComparisonsToTupleRange` as one of the two halves. That citation
is now stale: the half that executes is `bindScanComparisonsToRangeSet`. The
lockstep concern itself is unaffected by this change and remains open — this RFC
removes a decoy, it does not close CQ-34.

## 7. Proof obligations

**The 1M stress comparison does not apply here, and this is an argument on the
merits rather than a skipped gate.** A stress run measures plan choice and
execution latency. This change deletes a function with zero production callers
and edits tests; the compiled production path is unchanged *by construction*, so
there is no mechanism by which a plan could move. A stress delta here would be
measuring only the disks and the neighbours. (Independently: the tree is at 99%
utilisation, above the ~95% at which ext4 point-lookup latency degrades enough
to report as a planner regression, so a run taken now would be untrustworthy in
addition to being meaningless.)

What replaces it, all discharged:

1. **Zero production callers re-verified at this branch head**, not inherited. §1
   shows the PRE-deletion state: the non-test grep matched the function's own
   definition and nothing else. After the deletion the unrestricted
   `grep -rn "scanComparisonsToTupleRange" pkg/` returns no output at all — every
   reference, test and comment alike, is gone or retargeted (§3).
2. **plandiff and explaindiff goldens byte-identical.** No file under
   `pkg/relational/conformance/{plandiff,explaindiff}/` is modified;
   `git status --porcelain` over that tree reports only a comment-only edit in
   `rowdiff/gen.go`. Both suites green: `plandiff 35.5s ok`, `explaindiff 13.9s ok`.
3. **Full suite green** before commit.
4. **Every one of the 27 tests still exists and still passes** — verified by name.

## 8. Rejected alternatives

- **Keep the function and mark it deprecated.** It is a second implementation of
  a live concept that has already drifted from it (§5, the negative-NaN block).
  A deprecated twin drifts further and keeps reading as coverage.
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
  rows qualify and the index scan cannot reach them. Go has one float comparator,
  so it does not get to hold both answers.
