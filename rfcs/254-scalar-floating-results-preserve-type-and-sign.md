# RFC-254: Scalar floating results preserve type and sign

Status: implemented. Virtual Graefe and Torvalds ACKed design and implementation;
Codex reported no findings. Verification evidence is recorded below.

## Measured defect

At `ed3504f7e`, the initial `TestScalarMathSignedZero` runs under Bazel and
fails its 12 negative-zero cases, while its nine finite/positive-zero controls
pass (initial population: 21 cases, before changing direct-carrier assertions
and adding UNKNOWN). The revised 21-case test also asserts a floating direct
carrier for those controls; all 21 fail on the original code.
FLOOR(-0), CEIL(-0), CEILING(-0.25), ROUND(-0.25), decimal-precision ROUND,
and POWER(-0, odd exponent) compute negative zero with the Go math operation
and then erase its sign by converting the result to int64. Negative POWER
underflow has the same defect. Both ScalarFunctionValue.Evaluate and
SimplifyValue return positive zero for FLOAT and DOUBLE expressions.

The bad conversion is in the Floor/Ceil/Round arm and the Power arm of
`values.go`: being integral and within int64 range does not mean the
floating-point representation survives an integer carrier. Numeric equality
alone cannot pin this; negative and positive zero compare equal.

## Reference and scope

Read Java 4.12.11.0 `SqlFunctionCatalogImpl.java` in full, including
lookupBuiltInFunction and createSynonyms. These function spellings are not
registered: they are Go-only read-side extensions, not Java parity repairs.
Their existing evaluator intentionally uses Go math.Floor/Ceil/Round/Pow;
those operations preserve or produce signed zero. This change preserves the
result of that existing operation, not a new rounding rule. Explicit FLOAT
and DOUBLE result types must carry that result into later IEEE arithmetic.

No parser, planner rule, cost, persisted expression admission, record/index
encoding, continuation, or explicit CAST semantics changes. In particular,
float64FitsInt64 is a range predicate also used by explicit integral CAST
and scalar integer argument conversion; changing it would incorrectly make
negative-zero-to-integer casts fail.

## Decision

Delete the floating-result-to-int64 compaction in both scalar arms. Floating
FLOOR/CEIL/ROUND and all POWER results stay float64 after their existing
calculation and domain handling. Integral FLOOR/CEIL/ROUND keep their existing
integral-input arm. POWER keeps its current NaN/Infinity-to-NULL policy;
rounding keeps NaN/Infinity. Leave float64FitsInt64 unchanged.

The initial design proposed retaining compaction except for negative zero.
Both design reviewers rejected that: ScalarFunctionValue.Evaluate coerces
typed FLOAT/DOUBLE results back to floating immediately, making compaction
redundant, and UNKNOWN result types would retain value-dependent carriers.
The unexported evaluator's int64 expectations are an implementation detail,
not a public contract. Update those expectations to the coherent floating
carrier, including finite and range-boundary tests. Do not change expected
numeric values or errors. Special-casing a downstream reciprocal would miss
casts, folding and other consumers.

UNKNOWN-typed Value operands are directly constructible. For FLOOR/CEIL/ROUND,
which derive their type from their first argument, the result now stays
float64 even when it is integral. POWER always has DOUBLE result type, even
for UNKNOWN operands. SQL bound parameters do not prove UNKNOWN reachability:
QueryContext and ExecContext in connection.go substitute arguments into SQL
before planning. The parameter tests revealed the separate defect below.

GROUP BY and DISTINCT distinguish the two signed zeros in the existing
engine. Therefore CEIL over {-0.25, +0} now gives two groups/distinct rows,
not one; test this visible correction with explicit cardinalities and signs.
Pin unknown-typed Value arguments directly and parameterized SQL independently;
a successful SQL query alone does not prove its compiled argument type.

SIGN is a different contract: it classifies a number into -1, +0 or +1, not
Java Math.signum (Java's SQL catalog does not register SIGN either). Its
existing +0 for either zero sign is retained and pinned; it does not call the
floating compaction removed here. This RFC makes no new NaN-domain policy.

## Parameter transport finding and decision

The bound-parameter control failed independently of scalar compaction.
`substituteParams` renders finite doubles with `%g`, so float64(-0) becomes
`-0` (an integral literal) and float64(3) becomes `3`. The literal decoder
returns int64, losing -0's sign and the bound floating type. The real-FDB
`parameter arithmetic and storage` subtest reproduces SELECT ? returning
int64(3), `? / 2` returning int64(1) instead of float64(1.5), and INSERT of
bound negative zero persisting positive zero. Negative-three division also
reproduces. This is not an UNKNOWN Value escape: literal syntax determines
the type before planning. The unit `TestSubstituteParamsFiniteFloat` runs
under Bazel and fails for both signed zeros and +/-3 in its ten-value corpus;
fractional, large, subnormal and maximum-finite controls pass.

Read Java `EmbeddedRelationalPreparedStatement.java` in full: setDouble stores
the boxed Double in its parameters map, and createPlanContext carries it via
PreparedParams.of. Java does not stringify it into an integral literal.

Keep the existing Go text transport, but format finite float64 arguments with
`strconv.FormatFloat(value, 'e', -1, 64)`. Exponent syntax always denotes a
floating literal, including -0e+00 and 3e+00, and precision -1 preserves all
finite bits. This removes the value-dependent loss of type without inspecting
SQL text or adding a negative-zero-only exception. int64, strings, bytes,
NULL and the existing non-finite CAST/payload admission are unchanged. Update
the direct formatter's expected spelling for 3.14 (3.14e+00), not its value.

All finite floating arguments now plan as floating expressions. That corrects
integer division and overload admission for whole-valued doubles; integer-only
SQL positions should reject a floating parameter just as a floating literal,
not become conditionally admissible because its decimal rendering lacks a dot.
Records/indexes retain the same format; bound -0 now stores the supplied sign.
The round-trip unit/fuzzer exercises BOTH the actual lexer plus expr resolver
(asserting DOUBLE type and exact bits) and the evalConstant literal decoder.
SQL projection/arithmetic/storage assertions independently pin full execution.

Measured before/after with literal 3.0 controls: bound float64(3) in BIGINT
INSERT/UPDATE changes from acceptance to 22000, matching the literal;
LIMIT/OFFSET change from acceptance to 42601, matching the literal. Integer
parameters remain admitted. ROUND's precision argument still accepts an exact
whole DOUBLE and returns int64(1234) in both forms. ORDER BY float64(2) no longer
errors as an out-of-range SELECT-list position; it is a constant ordering key,
matching 2.0. The test checks row membership, not an unspecified tie order.
An out-of-range int64(2) ORDER BY control still raises 22023, distinguishing
integer position interpretation from the floating constant key. Six real-FDB
EXPLAIN-plus-row controls check a secondary BIGINT index with equality and
IN predicates, each with floating parameters, floating literals, and integer
parameters: all retain `IndexScan(IDX_INTS_K, [=]` equality bounds, reject
unbounded `[*]` scans, and return the expected rows. Within each predicate
family the floating-bind, floating-literal and integer-bind plan strings must
also match. A `k + 0 = ? ORDER BY k` negative control must use
`IndexScan(IDX_INTS_K, [*]` and fail the point-lookup check; merely finding
the index name would not rule out a filtered full index scan. Literal full-scan
and mixed point/full-scan plan strings independently pin both guard clauses.
Verified mutations to name-only matching and to allowing a full-scan sibling
both built and failed this test before restoring the final guard.
These are visible changes for callers using JSON-decoded float64 numbers in
integer-only positions; bind int64 there instead. Java parity for assignment
is source-derived; LIMIT/OFFSET are Go extensions, not Java conformance claims.

The driver still uses text substitution, not the executor's WithParams path.
DIVERGENCES.md's bound-parameter section records this boundary and the existing
NaN-payload, midnight time.Time
and integer ORDER BY interpretation limits. Moving the driver onto runtime
binding changes statement-global ordinals, cache keys, literal specialization
and DDL/view admission; it is not required to encode finite DOUBLEs exactly.
Both design reviewers ACKed this distinction after reading the live paths.

## Reproduction and review record

Virtual Graefe and Torvalds both ACKed deletion of compaction before
values.go changed, and exponent rendering before utilities.go changed.

The scalar YAML scenario failed 4/5 queries before the scalar edit; the finite,
NULL and explicit integer-CAST control passed. It now passes 5/5. The matcher
pin `TestScalarSignedZeroExpectation` ran under Bazel and rejects a flipped
Infinity sign in every reciprocal slot of queries 0, 1, 3 and 4 (13 slots
across six rows). GROUP BY and DISTINCT now also project reciprocal strings
in the YAML, rather than relying on numeric equality for signed-zero rows.
The FDB cases independently assert both cardinality and exact floating bits.

After adding parser/resolver coverage, the parameter unit still failed on
+0, -0 and +/-3, now naming the incorrect INT type as well as the lost bits.
Restoring `%g` temporarily after the fix (one verified mutation, restored by
an EXIT trap) made the parameter unit and FDB targets fail again, including
assignment, LIMIT/OFFSET, ORDER BY, division and stored sign. The mutation
built and executed both targets; this was not a build failure or empty run.
The first full `just test` run exposed a missing plan-golden update for this
new scenario. Regenerating it changed the corpus totals and added the five new
query entries; no existing corpus query's plan changed. The uncached golden
target then passed.

## Acceptance

- Design ACK from virtual Graefe and Torvalds before production edits;
  implementation review from both plus Codex at completion.
- Keep red-to-green unit cases for all six spellings; FLOAT/DOUBLE catalog
  result typing; runtime evaluation and constant folding; negative-zero
  inputs, rounding negative fractions, positive/negative/extreme precision,
  POWER odd/even exponents and underflow; positive-zero and finite controls.
- Real-FDB SQL tests for column and constant expressions, explicit bitwise
  sign assertions, and reciprocal arithmetic with an explicit -Infinity
  expectation. Fresh per-subtest deadlines after t.Parallel and distinct
  catalog fixture names across repetitions.
- Unit-test floating result boundaries, NaN/Infinity, fractions, explicit
  integer casts of negative zero, UNKNOWN-typed operands, and NULL propagation.
  Assert folding produces a ConstantValue, not merely an equivalent evaluator.
  Preserve existing errors.
- Yamsql uses reciprocal-to-STRING assertions so numerical equality cannot
  confuse negative and positive zero. Include a matcher mutation pin.
- Gazelle and module tidy; affected targets uncached; existing arithmetic
  simplification fuzzer; `just test`; source hashes checked after verification.
- Sequential million-row stress twice per state at the merge-base and final
  source, on the same filesystem. /home is 99% full, so use one comparison
  worktree on /var/tmp (59% full) for both states. Record source identities,
  row/test populations, durations and load; no cross-filesystem timing claim.

## Verification evidence

- Ten uncached repetitions of the scalar, parameter and real-FDB regressions
  passed under Bazel: values/embedded/sqldriver reported respectively
  300/110/370 RUN lines including subtests (30/11/37 per repetition).
  The same filtered tests passed with `--@rules_go//go/config:race`, with
  30/11/37 RUN lines. The FDB fixture actually ran; it was not Docker-skipped.
- Bazel fuzzing reported 4,401,821 executions of FuzzScalarFloatingMath in
  15 seconds, 9,287,919 of FuzzSubstituteParamsFiniteFloat in 60 seconds, and
  3,934,141 of FuzzSimplifyValue_ArithmeticTree in 15 seconds. All passed.
  These binaries explicitly warned that coverage instrumentation was absent:
  these are **unguided fuzz runs**, not coverage-guided exploration claims.
- The final scalar-compaction mutation restored the original values.go and
  verified both compaction sites were present before building. All 21 scalar
  cases and four of the final five YAML queries failed; the finite/NULL/CAST
  control stayed green. Production was restored to its checked blob afterward.
- The six sequential million-row stress runs passed. Full source identities,
  row/plan populations, all query timings and before/after load readings are
  in TODO.md, **Stress test 1M baseline — RFC-254**. Restoring the original
  code reproduced the raised aggregate timings, so no speedup or regression
  is attributed to this fix from those wall-clock measurements.
- The repository-wide command
  `bazelisk test //... --test_tag_filters=-stress --nocache_test_results`
  executed **92/92 targets**, all passing (909.807 seconds). Task-file hashes
  were unchanged across that execution. The values/embedded/sqldriver/yamsql/
  explaindiff logs respectively contained 2233/1921/6639/431/53 RUN lines,
  including subtests; the signed-zero YAML reported 5/5 passed.
- Subsequent changes only strengthened the index test and its documentation;
  production remains at the two blobs named in the stress record. The final
  index test again passed ten repetitions and race detection, including the
  real full-index-scan negative control. `just test` is the final incremental
  gate; cached targets are not counted as fresh executions of the full suite.
