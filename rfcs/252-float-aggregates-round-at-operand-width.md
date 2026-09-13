# RFC-252: FLOAT aggregates round at the operand width

Status: implemented and locally verified; design and implementation reviews ACKed. No push or merge requested.

## Measured defect

At `35b293e7f301d3aede9464687251bbf3420e5514`, streaming SUM and AVG widen all
floating inputs to double before accumulating. SQL FLOAT input values
`16777216, 1, -16777216` therefore produce SUM=1 and AVG=1/3 instead of zero.
The mistake is not confined to the return type: rounding after each addition
is observably different from rounding only the final answer. Two MaxFloat32
operands also produce a finite sum where float addition must overflow to +Inf.

`TestAggregateFloatPrecision` failed under uncached Bazel for FLOAT rounding
and overflow, for both functions and every continuation split; the DOUBLE
controls passed. `TestFDB_AggregateFloatPrecision` failed against real FDB on
scalar and grouped SQL at all five scanned-row budgets (1000000, 1, 2, 3, 4).
It returned the incorrect double-precision answers above.

## Reference and decision

Read in full: Java 4.12.11.0 `NumericAggregationValue.java`.
`encapsulate` selects an operator from the operand's STATIC type. SUM_F adds
`(float)s + (float)v`; AVG_F stores a float sum and a long count, then widens
the sum once to double for division. SUM_D/AVG_D accumulate in double.
`NumericAccumulator` starts with absent state, preserving a first negative
zero. NULL operands do not participate.

Use `AggregateSpec.OperandIntType` to select a FLOAT accumulation branch for
SUM/AVG, before the existing runtime-carrier integer branch. Despite its legacy
name, `aggregateOperandIntType` already stores the resolved operand's complete
`Type().Code()` verbatim, including FLOAT and DOUBLE. Keep this public field's
name for source compatibility, and update its comment to state the expanded
numeric-width purpose; do not introduce another type read or another metadata
field. The unit fixture sets this plan-time code explicitly, just as the SQL
translator does. Convert each operand directly to float32, seed from the
first non-NULL value, and perform each subsequent addition in float32. Retain
the existing double state slot as an exact widening of the rounded float sum;
all float32 values, infinities and signed zeros are exactly representable in
that slot. Mark this lane non-integer even if a promoted expression currently
returns an integer runtime carrier. AVG divides this rounded sum in double.
The existing INTEGER/BIGINT overflow and DOUBLE paths remain unchanged.

Extend the existing numeric-to-float32 conversion helper to admit the double
runtime carrier used for stored FLOAT columns. Integer carriers must convert
directly to float32 rather than via double, avoiding double rounding at large
integer boundaries. The static type, not the runtime carrier, is authoritative.
There is no per-row type derivation: the new branch reads the same carried
static code the existing integer overflow branch already reads.

Rejected alternatives:

- Narrow only the final sum: fails cancellation and AVG after overflow.
- Dispatch on `val.(float32)`: stored FLOAT values reach the executor as float64.
- Add another type field, re-read Operand.Type() per row, or infer width from
  column names/schema positions: duplicates the static code already carried by
  the translator. Renaming the public field adds source-compatibility churn
  without changing this authority; its documented meaning is what expands.
- Change the continuation schema: unnecessary; widen a rounded float exactly
  into the existing double state. Existing token layout remains unchanged.

The change is confined to evaluated read-side arithmetic. No planner rule,
property, key, stored record/index representation, or token schema changes.
It does not address pre-existing cross-engine aggregate-token layout differences.

## Acceptance

- Retain the unit and real-FDB reproducers; add NULL/empty, signed zero, native
  float32, expression/CAST and overflow controls. Unit tests round-trip every
  prefix, including the empty prefix and complete group.
- Assert explicit expected values and row counts, not only paged/unpaged equality.
  Prove ordered input with EXPLAIN; compare FLOAT with DOUBLE controls. The final
  FDB fixture uses primary key `(g, id)`, so ordering does not depend on the cost
  model choosing a secondary index. An expanded earlier fixture chose a scan
  plus sort and failed the fixture's no-sort requirement, before checking rows;
  no optimizer change or weakened ordering assertion was used to resolve it.
- Graefe and Torvalds design ACK before production edits; joint implementation
  review plus Codex at completion. No push or merge requested.
- Gazelle, module tidy, affected full Bazel targets uncached, aggregate codec
  fuzzing, and `just test`. Hash source/tests before and after final verification.
- Sequential 1M stress twice per source state, both states in the same
  `/var/tmp/query-hunt-252/compare` worktree and Bazel output filesystem because
  `/home` is 98% full. Baseline is the SHA above, also the merge-base when the
  measurement started. Record actual populations, load and timings in TODO.md.

## Design review and focused verification

Both virtual reviewers ACKed the revised design before the production edit:

- Graefe: session `99d0feef-a93b-43ba-be22-7eab82c2dead`.
- Torvalds: session `4690c2e7-6860-45d0-870f-b95fe9e77ef8`.

Their initial NAKs identified ambiguity about the type-dispatch location and
the unit fixture's omitted plan-time code. The revision uses the existing
`OperandIntType` field, sets it in both fixture construction paths, and orders
the FLOAT branch before the integer-carrier branch. Neither reviewer had
remaining design objections. No public field was renamed.

The final unit matrix (28 subtests) failed on the old source on precision,
overflow and integer-carrier conversion, then passed after the edit. The
real-FDB test's final primary-key-ordered fixture also failed at all five
budgets before the edit, then passed after it, including grouped/scalar CAST
and CASE results. Both targets ran uncached, with their RUN and result lines
checked. The SQL fixture checks actual stored FLOAT values before aggregation
and asserts output row counts and exact bits (not numeric equality for zeros).

## Implementation review

Reviewed arithmetic source blob: `b75b2393656567b6c2c2b5884a88d045d38aa3ee`.

- Graefe: ACK, session `d95aa6a8-758e-4378-b20c-a16b74e830db`.
- Torvalds: ACK, session `a9067346-33f8-4258-aba1-aaf34a4f752c`.
- Codex: no actionable correctness findings in the complete uncommitted change.

Following that review, the remaining stale integer-only comment at the type
producer's call site was corrected, and the unit NULL-count case was made
nonzero so an incorrect AVG denominator cannot hide behind a zero numerator.
Arithmetic source was not changed. Generated corpus ledgers and plan-shape
golden were refreshed; the golden diff adds only the new scenario's two
queries and updates the corpus header (no existing plan changes).

The executor, sqldriver, yamsql and explaindiff Bazel targets then all ran in
full and uncached and passed. Output includes 29 unit RUN lines (parent plus
28 cases), six FDB RUN lines (parent plus five budgets), the new scenario's
`2/2 passed` report, and `TestPlanShapeGolden`. Source/test hashes were checked
unchanged after the run. A reporting-only grep initially lacked `--` before
a pattern beginning with `---`; the corrected command read the saved complete
log and verified `Executed 4 out of 4 tests: 4 tests pass`.

Sequential 1M stress also passed twice per state. Both source identities,
loads, complete query timing population and interpretation limits are recorded
in TODO.md, section 11, “Stress test 1M baseline — RFC-252 FLOAT aggregate
precision”. The benchmark's aggregate-index timings are reported without
attributing their movement to this evaluated-aggregate branch.

## Final verification

Graefe and Torvalds reconfirmed their ACKs on the final delta in the same
implementation-review sessions. Codex also reconfirmed no actionable findings
(session `01a09c5e-0356-7090-8928-2663f2f95f72`). No arithmetic edits followed
the implementation reviews.

`FuzzAggregateContinuation` passed all five corpus seeds and **2,845,272 fuzz
executions** in 15 seconds with four workers, under uncached Bazel. This is a
codec robustness check, not a substitute for the arithmetic/SQL regressions.

`just test` passed all **92 Bazel test targets** in 643.218 seconds: **45 executed,
47 cached**. This is not an uncached whole-repository claim; the four directly
affected targets have their separate full uncached pass documented above. All
six changed source/test/scenario files were MD5-checked unchanged through the
fuzz and repository-wide runs. No failing test or skipped regression remains.
