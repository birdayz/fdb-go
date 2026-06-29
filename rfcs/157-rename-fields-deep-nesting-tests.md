# RFC-157: Close the `RenameFieldsVisitor` depth-≥2 test gap (RFC-136 §8 residual)

**Status:** Implemented
**Gate:** Torvalds + codex + @claude (record-layer metadata; no query-engine surface → no Graefe)
**RFC review:** Torvalds ACK (verified the staleness claim against the test block + visitor; suggested
adding the asymmetric merge/split type-follow case, included below as the 6th spec).

## Problem

RFC-136 §8 tracked a "test-depth gap" residual: Java's `RenameFieldsVisitorTest` is parameterized
across 11 shapes, and Go's unit block was said to exercise only a subset, leaving Grouping, Function,
Split, Dimensions, KeyWithValue, List, nested RecordType, Version/Literal/Empty, and the two
source-side error paths "not directly unit-tested."

## Investigation — most of §8 is already done (stale TODO)

Reading `pkg/recordlayer/metadata_evolution_validator_test.go` (the `Describe("renameFields
(RenameFieldsVisitor port)")` block) shows the per-node-type and error-path shapes §8 listed as
missing were **already ported** after §8 was written:

- Per node type: List, Function, Split, Grouping, KeyWithValue, Dimensions, nested RecordType,
  bare RecordType, Version, Literal, Empty, fan-type preservation — all present.
- Error paths: field-not-found-in-**source**, field-not-found-in-**target**,
  nesting-into-a-non-message parent, and the unsupported-type default arm — all present.

So the bulk of §8 is **stale**. The one axis §8 itself flagged as non-trivial is genuinely still
uncovered:

> "The one non-trivial axis is the nested-descriptor re-derivation (`messageTypeForField`) at
> depth ≥ 2 / merged-record shapes — exactly Java's `renameMiddleRecord` / `renameMergedRecord` cases."

The deepest existing Go test is a single `Nest(...)` (one `NestingKeyExpression`). Nothing exercises:

1. **Recursion through `messageTypeForField` at depth ≥ 2** — a 3-level `Nest("middle",
   Nest("inner", Field("foo")))` where the descriptor pair must be re-derived twice. Java's
   `renameOuterRecord` (expr 3: `other.outer.middle.inner.(foo,bar)`) and `renameMiddleRecord`.
2. **Rename follows the descriptor TYPE, not the global field name** — the same field name living in
   two different message types; renaming it in one must not touch the other. Java's
   `renameMiddleRecord` (`other_int` defined in both `MiddleRecord` and `OuterRecord.MiddleRecord`).
3. **A shared nested descriptor reached by two parents** — both top fields point at the same nested
   type; one nested rename rewrites both paths. Java's `renameMergedRecord` (`a` and `b` both →
   `OneTrueNested`).
4. **The `childSource == childTarget` short-circuit** (`rename_fields_visitor.go:58-63`,
   Java's `avoidRewritingIfNestedDescriptorIsTheSame`) — when the parent field is renamed but its
   message type is the *same descriptor object* on both sides, the recursion is skipped. Across two
   independently-built files this never fires, so it is currently **dead in tests**. It is reachable
   only when both metadata versions share the *same imported dependency* descriptor.

These are not cosmetic: (1)–(3) are the only checks that the per-level descriptor re-derivation is
correct (a wrong-type lookup would silently rename the wrong field and corrupt a primary-key/index
expression during metadata evolution — a wire-relevant bug), and (4) is the single untested branch
in the visitor.

## Fix

Test-only. Add to the existing `renameFields` `Describe` block (reusing its `buildFile`/`msg`/
`messageField`/`strField` helpers), no FDB:

- **Depth-3, rename innermost** — `Outer{middle→Middle}`, `Middle{inner→Inner}`, `Inner{foo,bar}`;
  rename `Inner.foo→foo_z`; assert `middle.inner.foo` → `middle.inner.foo_z`.
- **Depth-3, rename a mid-chain parent** — rename `Middle.inner→inner_2b`; assert
  `middle.inner.foo` → `middle.inner_2b.foo`.
- **Same name, different type** — `A{val}`, `B{val}`, `Top{a→A, b→B}`; rename `A.val→val_a`; assert
  `concat(a.val, b.val)` → `concat(a.val_a, b.val)` (B untouched).
- **Shared nested descriptor** — `Nested{x,y}`, `My{a→Nested, b→Nested}`; rename `Nested.x→x_2`;
  assert `concat(a.x, b.x)` → `concat(a.x_2, b.x_2)` (both paths rewritten).
- **Asymmetric merge/split (the strongest type-follow probe; Java `renameSplitRecord`)** — the name
  `x` lives at a *different number* in each source type (`NestedA.x=1`, `NestedB.x=2`) and both
  parents merge into one target `OneTrueNested{p=1, q=2}`; assert `concat(a.x, b.x)` →
  `concat(a.p, b.q)` (same name, different result, because the rename follows source-type → number
  → target-type-by-number at each parent independently). A name-keyed implementation cannot pass.
- **`childSource==childTarget` short-circuit** — a shared dependency file (`Shared{foo,bar}`)
  imported by both the old and new outer files; rename only the parent (`inner→wrapper`); assert
  `inner.foo` → `wrapper.foo` with the child returned unchanged (the branch is taken because the
  nested descriptor is the same object on both sides). Branch-coverage for the otherwise-dead arm.

Then mark RFC-136 §8 and the TODO.md R1 residual **done**, recording the staleness correction
(the per-node/error shapes were already covered; this PR closes the depth axis).

## Performance

None — unit tests, no FDB, no production code change.

## Test plan

- The six new specs pass; `renameFields` block is 27/27 (`bazelisk test
  //pkg/recordlayer:recordlayer_test --test_arg="--ginkgo.focus=renameFields"`).
- Revert-proof, verified with two deliberate reverts:
  - **Drop the per-level `messageTypeForField` re-derivation** (recurse the child against the
    top-level descriptors): fails all six new specs *and* the pre-existing nested specs (the child
    field is no longer found / the wrong error fires) — proving the six exercise the re-derivation
    the single-`Nest` tests never reached.
  - **Remove only the `childSource==childTarget` short-circuit** (always recurse): everything stays
    green, because line-28's `sourceDesc==targetDesc` guard already covers that case. So the sixth
    spec is *branch-coverage* for that otherwise-dead arm, not a behavioral revert-probe — stated
    as such, not over-claimed.
- Full `just test` green (pre-commit).
