# RFC-253: Scalar MOD shares arithmetic remainder evaluation

Status: implemented and locally verified; design and implementation ACKed by both virtual reviewers; Codex final review has no actionable findings.

## Measured defect and reference

At `7a7d61cde`, SQL `MOD(7.0, 0.0)` raises 22012, whereas `7.0 % 0.0`
returns NaN. The initial `TestScalarModFloatZero` fails all nine unit cases;
`TestFDB_ModFloatZero` fails its nine MOD cases while both infix controls
pass against real FDB. FLOAT, DOUBLE, mixed integer/floating operands,
positive/negative zero divisors and a zero dividend reproduce.

Read in full: Java 4.12.11.0 `ArithmeticValue.java` and
`SqlFunctionCatalogImpl.java`. Java's SQL catalog maps `%` to the internal
`ModFn`; it does NOT register the function-call spelling `MOD(a,b)`. The
initial design incorrectly called the function spelling a Java parity fix.
It is a Go-only extension. Its intended semantics here are explicitly those
of the dialect's existing remainder operator, not CockroachDB's MOD function.
Java's integral MOD_II/IL/LI/LL throws for zero; floating MOD_* computes Java
`%`, giving NaN for a zero divisor. Go's infix ArithmeticValue follows this.
Pin Java's rejection of the function spelling and acceptance of `%` in the
cross-engine harness rather than implying Java runs the extension. Measured
on the live JVM: `%` returns NaN and MOD() errors `Unsupported operator MOD`.
The rejection happens in `SemanticAnalyzer.resolveScalarFunction:981-982`
(`containsFunction`), before `lookupFunction`; the first probe incorrectly
expected the later lookup's error wording and was corrected against this source.

## Decision

Use one arithmetic evaluator, not just delete the scalar's floating-zero
check. Extract the already-evaluated body of `ArithmeticValue.Evaluate` into
`(*ArithmeticValue).evaluateOperands(l, r any)`. Evaluate still reads its two
children once, propagates their errors, and delegates; move the NULL check
with the arithmetic body. The extraction preserves infix dispatch and math.
The later int8/int16 admission correction also enables these native carriers
in INT/LONG arithmetic, integer-to-string concatenation and StrictRankLimitValue.
`toFloat32Operand` now takes its direct integer arm for them rather than the
ToFloat64 fallback; both conversions are exact at these small integer widths.
SQL stored integer columns still use the unchanged int64 carrier.

The scalar MOD arm keeps its arity/NULL/nonnumeric admission: exactly two
non-NULL arguments, unsupported pairs decline to NULL. Use the shared
`toInt64ForArith` admission for losslessly LONG-representable integer carriers;
other numeric pairs convert with ToFloat64. This preserves integer precision
for native callers as well as the row domain's int64 carriers.
It then calls `(&ArithmeticValue{Op: OpMod}).evaluateOperands` on those
normalized operands. There is no separate remainder calculation or zero
check left in the scalar evaluator. Nil children intentionally mean unknown
static types, selecting ArithmeticValue's runtime-carrier lanes. Production
scalar calls have ALREADY had common-numeric promotion applied by
`ScalarFunctionValue.evaluateUncoerced`; FLOAT operands are rounded there
before the shared calculation and the result is narrowed by Evaluate.
Retain that existing catalog mechanism, including direct integer-to-float32
conversion. This preserves nonnumeric/arity compatibility of direct scalar
callers without broadening those contracts. The initial implementation kept
scalar MOD's int64-only integral admission. Review exposed a second defect:
`MOD(int64(9007199254740993), int32(2))` returned float64(0), not int64(1),
through lossy double conversion. `TestScalarModNativeIntegerCarriers` reproduced
it under Bazel, along with integral-zero misclassification. Use the common
integer helper for MOD and complete its int8/int16 cases (already numeric in
ToFloat64). Tests pin native int/int8/int16/int32/int64/uint/uint64 and mixed
large-long/small-integer cases through both scalar and arithmetic nodes.
Unsigned values above MaxInt64 remain outside the integral LONG lane; their
existing floating scalar fallback is not changed.

Retain ScalarFunctionValue's representation. A concrete consumer makes
parser lowering a separate behavior change: `core/query/ddl/generator.go`
`valueKeyExpression` accepts ArithmeticValue as a persisted function key
expression, but its ScalarFunctionValue arm permits only bit functions.
Lowering MOD() in the parser would silently admit a previously rejected index
expression. `values/semantic_hash.go` also gives the two nodes distinct memo
identities. No claim of serialized plan incompatibility is needed or made.
Keeping the node does not require keeping a duplicate arithmetic evaluator.
The cost is separate memo identities: the spellings cannot match each other
as expressions, so equivalent alternatives may not be found. This change
preserves that existing planning limitation rather than broadening index DDL.

Update the catalog's broad CockroachDB-reference comment with MOD's explicit
infix-semantics exception. Correct stale float-22012 assertions/comments in
`scalar_functions_errors_test.go`, `scalar_functions_extra2_test.go`, and the
scalar arm. Narrow RFC-087's MOD/0 contract to integral operands.

## Acceptance

- Design ACK from virtual Graefe and Torvalds before production edits.
- Keep red-to-green unit and real-FDB reproducers; assert exact lane types,
  INTEGER/BIGINT 22012, NULL propagation, finite remainders, infinities/NaNs,
  negative-zero dividends and remainders (bitwise), and FLOAT operand rounding
  of 16777217 in BOTH operand positions. Assert signed-zero operands before
  using them. Include `%` and infix `MOD` controls.
- Use the catalog result type in unit fixtures (the initial unknown-type
  fixture did not exercise FLOAT coercion). Explicit expected values, not
  only equality of two routes that might fail together.
- Sequential constant queries on one connection with zero/nonzero divisors
  rule out cached constant-folding contamination.
- Add yamsql with NaN rendered as STRING: its numeric matcher does not equate
  NaN to NaN. Verify the new scenario fails on pre-fix code and when a finite
  result substitutes for the expected NaN.
- Gazelle and module tidy; full affected Bazel targets uncached; `just test`.
- Implementation review by both virtual reviewers and Codex at completion.
- Hash the tested source and verify it unchanged after final verification.
- Sequential 1M stress twice per state, in one worktree on `/var/tmp`
  (58% occupied; `/home` is 99% occupied). Record source identities and test
  populations; do not infer timing changes from cross-filesystem comparisons.

No planner rule, property, matching, stored record/index or continuation
layout changes. This does not change non-finite value persistence policy.

## Review and reproduction record

Design ACKs preceded production edits:
- Graefe: `dd34614f-4dca-4165-a6ef-44230eb4711a`.
- Torvalds: `c3128d89-b5d2-493d-85ae-01455b180471`.

Implementation and final-delta ACKs:
- Graefe: `edad89f8-cd9a-45ba-b090-4be6a2fc4fdb`.
- Torvalds: `de066006-bb63-4f65-b2f6-1b11d9791727`.
- Codex: `01a09e62-7e15-79b2-95c5-09d6c90a5688`, no actionable findings
  after the delta. The final documentation condition (infix scope after
  int8/int16 admission) is incorporated above; no further review was requested.

The final unit/FDB/YAML reproducers were rerun against the original values.go
and failed on the floating-zero cases; the YAML scenario reported its first
query failing with 22012. Restoring the shared evaluator made them pass.
The native-carrier regression was separately seen red before changing integer
admission, including the explicit 9007199254740993 remainder failure.

Two fixture errors were diagnosed rather than hidden: Go and Java's grammar
accept negative numeric literals but not unary `-d`; the negative-column
controls use the admitted `0.0 - d` expression. The JVM rejection happens in
SemanticAnalyzer before lookupFunction, so the probe pins its actual
`Unsupported operator MOD` message. The live JVM and Go both return the string
`NaN` for `%` by floating zero. Go also returns it for MOD(), while Java rejects
that function spelling. Both probe arms assert rows/errors before reporting.

The yamsql NaN check uses string values deliberately and has its own unit pin:
substituting a finite string in any of its four output slots fails comparison.
Integer-error and finite/NULL controls remain separate scenario queries.

## Final verification

Final arithmetic source blob: `98b29581a04c61e6d0a8503d0ea6bbd7d4e48504`.
The values, sqldriver, yamsql and explaindiff targets all ran in full and
uncached after the native-carrier correction: four targets passed. Their
combined log contains 38 `TestScalarMod*` RUN lines, 30 RUN lines for the new
FDB modulo and DDL-boundary tests, the NaN-matcher unit test, and the new
scenario's `3/3 passed` report. Hashes of all changed code, tests, BUILD files,
corpus and generated ledgers remained unchanged through verification.

`FuzzSimplifyValue_ArithmeticTree` then ran for 15 seconds on the final source:
13,549,406 executions, no failure. `just test` passed all 92 test targets
(50 executed this invocation, 42 cached). The new JVM spec also ran in that
full conformance target; its earlier focused execution asserted both boundary
arms and reported 1/1 selected specs passed.

Sequential million-order stress passed twice per state with identical test
and row populations. The complete four-sample timing table, source identities,
loads, command and interpretation limits are in TODO.md section 11,
“Stress test 1M baseline — RFC-253 scalar remainder evaluation”. No performance
improvement is claimed from that correctness fixture. Temporary comparison
worktree removed; local verification logs remain in `/var/tmp/query-hunt-253`.
