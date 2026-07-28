# RFC-196 — A zero-valued float equality terminates the scan prefix

Status: **implemented on `fix/cq29-cardinality-bound-violations`, NOT merged, awaiting Graefe + Torvalds ACK.**
Closes: TODO CQ-28.

## Process note, stated up front

CLAUDE.md requires a Graefe ACK on an RFC *before* implementation for any
matching/data-access change, and CQ-28's own TODO entry repeated that. The
implementation was written first and this document after. That ordering was
wrong. The work is on a branch and unmerged, so the gate can still function as a
gate — but reviewers should read this as a design under review, not a
rationalisation of something already shipped. If the design is rejected, the
commit comes out.

## The defect

`v = 0 AND w = 5` against index `(v, w)` returned NOTHING for a stored
`(-0.0, 5)`.

IEEE says `-0.0 == +0.0` and this engine's `=` follows IEEE (deliberately — see
DIVERGENCES.md on Java's buggy bit-identity `=`). FDB tuple encoding preserves
the sign bit, because it is Java's encoder and wire compatibility is the hard
line, so the two zeros are **distinct, adjacent index keys**. An equality probe
for `0` must therefore span both.

The executor already widens a zero bound that TERMINATES the scan range. With
both columns consumed into the equality prefix, the zero was not terminal, so no
widening applied and the probe sought only `(-0.0, 5)`'s own key... or rather,
only the key matching whichever sign the comparand carried.

## Why the obvious fixes do not work

**Widen the whole range.** The union of `(-0.0,5)` and `(+0.0,5)` is not a
contiguous interval: the span between them also contains `(-0.0, w>5)` and
`(+0.0, w<5)`. A single `TupleRange` spanning both returns WRONG rows rather
than missing ones — strictly worse.

**Rewrite to `v IN (-0.0, +0.0)`.** Attempted and reverted twice. `IN` is
executed as a join over an exploded element list, emitting one row per matching
element, which is sound only when the elements are mutually exclusive under the
comparison. The two zeros are IEEE-*equal*, so on the residual-filter path every
zero row matched both probes and came out duplicated. This direction is dead,
not merely unlucky — it manufactures exactly the pair that breaks IN-as-join.

**Multi-range union execution.** What CQ-28's TODO assumed was required
("a genuine two-way range union… infra that does not exist today"). That
estimate was wrong, and believing it is why the item sat open as LOW and why the
two failed attempts went looking for a rewrite instead.

## Decision

**A zero-valued float equality terminates the scan prefix during index
matching**, even when more indexed columns could be consumed
(`match_candidate_index.go`, the prefix builder).

Consequences fall out of existing mechanisms:

- the zero equality is now the last comparison, so the executor's existing
  widening applies and the range spans both adjacent keys;
- the trailing predicate is not consumed into the prefix, so it survives as a
  **residual filter** — the standard partial-match mechanism, not new machinery;
- the scan is bounded to the two zero groups, and the residual drops the
  in-between keys, leaving exactly the wanted pair.

One scan. No new execution machinery. ~10 lines.

## Cost, stated honestly

This shape no longer uses the full composite prefix: it scans both zero groups
instead of seeking a single key. The bound is however many rows share a zero in
the leading column. For a column where zero is a common value this is a real
regression in scanned rows — paid to return correct ones.

Reviewers should push back here if that trade is wrong for a workload we care
about. The alternative that preserves the seek is genuine multi-range execution,
which is a much larger change.

## Known gap, deliberately uncovered

Only a **compile-time-constant** zero is detectable at match time. A correlated
or parameterised comparand that happens to be zero at runtime keeps the full
prefix and can still miss the row.

Covering it would mean de-sargging every correlated composite join whose leading
column is a float, on the possibility of a runtime zero — trading a rare wrong
row for a broad performance cliff. I judged that the wrong trade. This is the
single most reviewable judgement in the change.

## Verification

- The known-bug sentinel fired on its own terms ("now returns [1] … flip this
  sentinel") rather than silently starting to pass, and is now an asserting test.
- It guards BOTH error directions: rows at `w=9` and `w=1` sit either side of the
  target, so a naive interval spanning both zeros fails loudly instead of
  looking correct.
- A companion test runs every predicate across three tables — single-column
  index, composite-only index, and no index — because the defect IS the access
  path changing the answer.
- Mutation-verified: reverting reddens exactly the two composite-index cases and
  leaves single-column and unindexed green.
- `just test` 56/56.
