# RFC-221: Retire the compiled key-evaluator twin

## Summary

`pkg/recordlayer/key_expression_compiled.go` is a 239-line second implementation
of key encoding with zero production callers. Delete it. Its 40 tests are
re-pointed at the live encoding path, and the two divergences the exercise
surfaced — one that the dead file enshrined, one live on the write path — get
regression tests.

## The file

`compileKeyExpression` / `compiledKeyEvaluator` / `compiledStep` /
`tupleAppender` are referenced from nowhere but their own test file:

```
$ grep -rn "compileKeyExpression\|compiledKeyEvaluator\|compiledStep" \
    --include='*.go' pkg/ cmd/ conformance/ example/ \
    | grep -v _test.go | grep -v key_expression_compiled.go
$ echo $?
1
```

Its own doc comment states the purpose: it "eliminates all intermediate
allocations from EvaluateFlat" and "falls back to EvaluateFlat for unsupported
expressions". It is a performance fast path that nothing executes. Every key
evaluation in production goes through `Evaluate` / `EvaluateFlat` /
`EvaluateScalar` / `EvaluateInt64` / `PackDirect`.

## Why delete rather than wire up

**The fast path it claims to be already exists and is live.** `PackDirect`,
`EvaluateScalar` and `EvaluateInt64` are wired into `index_maintainer.go:136`
and `:147`, `atomic_mutation_index_maintainer.go:225`-`:323`, and
`key_expression.go`'s own composite path. They pack straight into a pooled
`tuple.Packer` with no `[]any` boxing — exactly the allocation win the dead
twin advertises.

**The live one is more careful about the same hazard.**
`RecordTypeKeyExpression.PackDirect` reports false on `err != nil || !ok`,
documented as routing the caller to the erroring path "rather than packing a
guess". The dead twin packs the guess.

**Java has one path.** `RecordTypeKeyExpression.java:73` is a single
`evaluateMessage`. Wiring the twin up would put a second key-encoding
implementation on the wire path, which is the hard line in CLAUDE.md, and
violates "No parallel pipelines".

**Its concurrency claim is unfalsifiable.** `fieldStep.cachedFD` is a plain
mutable `protoreflect.FieldDescriptor` guarded only by the comment
`// safe: fieldStep is per-batch, not shared` — an assertion about a caller
that does not exist. The live `FieldKeyExpression.fdCache` is an
`atomic.Pointer[fieldDescCache]` precisely because metadata is shared across
transactions.

## Divergence 1 — the dead twin corrupts where the live path errors

On a key expression naming a field the message lacks:

| path | result |
|---|---|
| `resolveFieldDescriptor` (live, `key_expression.go:110`) | `&KeyExpressionError{"field %s not found in message"}` |
| `fieldStep.packInto` (dead, `key_expression_compiled.go:101-103`) | `appendAny(nil)`, nil error → packs `0x00` |

Measured, on `Field("no_such_field")` against an `Order`:

```
Evaluate:       [] err=field no_such_field not found in message
EvaluateFlat:   [] err=field no_such_field not found in message
EvaluateScalar: <nil> err=field no_such_field not found in message
EvaluateInt64:  0 ok=false err=field no_such_field not found in message
PackDirect:     ok=false
DEAD compiled:  tuple=(<nil>) packed=00 err=<nil>
```

The twin's 692 lines of comparison tests could not catch this. Both harness
arms (`evalCompiled`, `evalStandard`) assert `Expect(err).NotTo(HaveOccurred())`,
so "one path errors, the other returns a value" is inexpressible. Worse than
inexpressible: `TestCompiledKeyEvaluator_NonexistentField` exercised only the
compiled arm and asserted `err` did NOT occur and the result was nil —
it *pinned the corruption as correct*.

Nothing anywhere in `pkg/recordlayer` pinned the live path's error; the string
`no_such_field` appeared in exactly one file, the dead one. That is now
`TestKeyExpressionFastPath_UnknownFieldErrorsEverywhere`.

## Divergence 2 — a live defect: nested record type keys lose columns

Re-pointing the twin's "nested RecordTypeKey should not compile" test forced
the question of what the *live* path does with `RecordTypeKey().Nest(...)`.
It truncates the key.

`RecordTypeKey().Nest(Field("order_id"))` on an `Order` with type key 7 and
`order_id` 42, `ColumnSize() == 2`:

```
Evaluate:       [[7 42]]   packed 1507152a   (correct, 2 columns)
EvaluateFlat:   [7]                          (nested column dropped)
EvaluateScalar: 7                            (nested column dropped)
PackDirect:     ok=true    packed 1507       (TRUNCATED key written)
```

`RecordTypeKeyExpression.EvaluateFlat`, `EvaluateScalar` and `PackDirect` never
consulted `r.nested`.

An earlier draft of this RFC claimed the consequence was orphaned index
entries, on the reasoning that inserts take `PackDirect` while deletes take
`Evaluate`. **That was inferred and it is wrong** — `evaluateIndex`
(`index_maintainer.go:394`) prefers `EvaluateFlat` for *both* the old and the
new record, so both sides truncated consistently and no orphan arises.

The measured consequence is worse. `store.go:564` and `store_batch.go:85`
compute every record's primary key with `evaluateKeyFlat`, which calls
`EvaluateFlat` and has **no fallback to Evaluate**. With
`RecordTypeKey().Nest(Field("order_id"))` as a primary key:

```
                        HEAD              with fix
order_id=42 -> pk       [7]               [7 42]
order_id=43 -> pk       [7]               [7 43]
```

Every record of the type collapses onto the single primary key `[7]`, so each
save overwrites the previous record. That is silent data loss, and it is the
exact shape `metadata_builder_test.go:262-264` installs as a primary key.
`primary_key_translation.go:61` also handles this shape.

Pinned by `TestKeyExpressionFastPath_NestedRecordTypeKeyPrimaryKeysStayDistinct`.

**Java is not the spec here, because Java does not have this at all.**
`RecordTypeKeyExpression` is `KeyExpressionWithoutChildren` with a private
constructor and `getColumnSize()` hard-coded to `return 1`
(`RecordTypeKeyExpression.java:59`, `:89`). Go's `Nest` on a record type key is
a Go-only extension. The extension is allowed (read-side reach is not capped by
Java), but it must be *correct*, and the single-element fast paths cannot
express it.

**Fix:** `EvaluateScalar` and `PackDirect` decline when `r.nested != nil`,
routing callers to `Evaluate`. `EvaluateFlat` cannot decline — `evaluateKeyFlat`
propagates its result with no fallback — so it delegates to `Evaluate` and
returns the full-width tuple.

## Test disposition — all 40, bucketed

The brief counted 28; that is the number of `compileKeyExpression` call sites.
The file holds **40 test functions** and 45 total dead-symbol references. All 40
are accounted for.

**A — re-pointed at the live path, same claim (38).**

The twin's core claim was "compiled path and standard path pack identical
bytes". The live analogue is the same claim on the live pair: every fast path
(`EvaluateFlat`, `EvaluateScalar`, `EvaluateInt64`, `PackDirect`) must pack
byte-identically to `Evaluate`, or decline. `assertFastPathAgreement` in
`key_expression_fastpath_test.go` is that harness, and unlike the twin's it can
express disagreement, because a fast path that *accepts and differs* fails.

- 18 scalar-kind and boundary cases (int64/string/float32/float64/enum,
  int32/sint32/sint64/sfixed32/sfixed64/bool×2/bytes, MaxInt64, MinInt64, zero,
  empty string, unicode) → `TestKeyExpressionFastPath_*` equivalents.
- 5 composite cases (two fields, mixed types, unset fields, all integer types,
  empty key).
- 3 record-type-key cases (with field, three-field composite, follows-the-record).
- 1 unresolved-type case — strengthened: it now also asserts `PackDirect`
  declines, which the twin's version never checked on the live type.
- 2 nil-message cases.
- 1 grouping-key delegation case.
- 4 "should not compile" cases → "the live fast path must DECLINE this shape"
  (fan-out field, fan-out inside a composite, nesting expression, nested record
  type key). This is a strictly stronger claim than the original: it pins that
  fan-out and nesting never reach a direct packer that would flatten them onto
  the wire.
- 2 field-descriptor-cache cases — the twin's single-goroutine cache test, plus
  a new concurrent one that actually exercises the property the twin's
  `// safe: per-batch, not shared` comment merely asserted.
- 2 packer cases (subspace prefix, reset residue) re-pointed from the dead
  `tupleAppender` onto the pooled `tuple.Packer` the maintainer really uses.

**B — the live path did NOT satisfy the claim (1).**

`TestCompiledKeyEvaluator_NestedRecordTypeKeyReturnsNil`. Re-pointing it
uncovered divergence 2 above. Fixed in `key_expression.go`, pinned by
`TestKeyExpressionFastPath_NestedRecordTypeKeyKeepsAllColumns` and
`TestKeyExpressionFastPath_NestedRecordTypeKeyPrimaryKeysStayDistinct`.

**C — the claim was always wrong (1).**

`TestCompiledKeyEvaluator_NonexistentField` asserted "nonexistent field should
produce nil". The live path errors, and erroring is correct — a nil there is a
`0x00` in an index key. Deleted and replaced by
`TestKeyExpressionFastPath_UnknownFieldErrorsEverywhere`, which asserts the
opposite.

`TestCompiledKeyEvaluator_FanOutFieldReturnsErrFanOut` is folded into the
fan-out A case rather than counted separately: it was a verbatim duplicate of
`TestCompiledKeyEvaluator_FanOutReturnsNil`, and its own comment says so
("Actually we can't -- FanOut won't compile. So just verify it returns nil").

Net: 40 dead tests out, 42 live tests in.

## Rejected alternative

*Wire the compiled evaluator into `index_maintainer.Update`.* Rejected: it puts
a second key encoder on the wire path, it is the less careful of the two on the
exact hazard that matters (packing a guess vs declining), its cache is not
goroutine-safe, and the allocation win it targets is already banked by
`PackDirect`.
