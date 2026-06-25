# RFC-136 — Metadata-evolution field renames (Java 4.12 parity, RFC-135 §4 R1)

**Status:** **Implemented & merged** (commit `2095a4a7b`, PR #336 — the RFC-135 4.12 upgrade landed the
R1 port in the same change). Torvalds + codex + @claude reviewed PR #336. This RFC was never flipped from
Draft and TODO.md R1 was never checked; the §1 "Problem" below is preserved as the *original* (now-closed)
gap analysis. **Residual follow-up: a test-depth gap vs Java's `RenameFieldsVisitorTest` — see §8.**
**Item:** RFC-135 §4 **R1** — port Java 4.12's `MetaDataEvolutionValidator` field-rename support +
`RenameFieldsVisitor`.
**Reviewers:** Torvalds (code/test quality) + codex + @claude. **Not a query-engine change** — this is
record-layer metadata-evolution validation (no planner/cost/matching/executor), so **no Graefe gate**.

---

## 1. Problem (verified real — NOW CLOSED, kept as history)

> **Closed:** every piece prescribed below exists in the current tree (commit `2095a4a7b`). Struct +
> builder + flags at `metadata_evolution_validator.go:30-32` / setters `:104-122` / `allowsAnyFieldRenames()`
> `:38-40`; the `validateField` rename predicate at `:1097` (was `:992` in this RFC's original draft);
> `comparePrimaryKeys` rewrite at `:387` (was `:342`); index rewrite via `expectedRenamedIndexExpression`
> (was "index validation `:421`"); and the `RenameFieldsVisitor` port in the new file
> `pkg/recordlayer/rename_fields_visitor.go`. The original gap text follows for reference.

Java added configurable field-rename support to `MetaDataEvolutionValidator` *between* 4.11.1.0 and
4.12.11.0 (`8b03a10ea` #4034 "Support field renames", `05f622297` #4119 "ignore field renames only on
deprecated fields"). The 4.12 validator has three new options — `allowFieldRenames`,
`allowDeprecatedFieldRenames`, `allowUndeprecatingFields` — and uses `RenameFieldsVisitor.renameFields`
to rewrite primary-key and index key expressions across a rename before comparing them.

Go's `MetaDataEvolutionValidator` *(at the time this RFC was drafted)* had **none** of
this: `validateField` unconditionally rejected any field-name change ("field %q renamed to %q
in message %q"), and `comparePrimaryKeys` / index validation compared key expressions
with `proto.Equal` of the *un-rewritten* expressions. So Go could not validate a metadata evolution that
renames a field even when the rename is semantically safe — a genuine 4.12 parity gap (real feature
Java supports, not a Go-extension). **This is now implemented (see Status).**

## 2. Investigation (Java spec + Go infra)

**Java `validateField` rename logic** (`:282-299`): if old/new field names differ, throw unless
`allowFieldRenames || (allowDeprecatedFieldRenames && (oldDeprecated || newDeprecated))`. Separately, if
`!allowUndeprecatingFields && oldDeprecated && !newDeprecated` → "field is no longer deprecated".
**`allowsAnyFieldRenames()`** = `allowFieldRenames || allowDeprecatedFieldRenames`. When true,
`validateRecordTypes` (`:397-416`) and index validation (`:695`) compute the *expected* key expression
via `RenameFieldsVisitor.renameFields(old, oldDescriptor, newDescriptor)` and compare it to the new one,
with a two-message split (key "changed" vs "does not match required").

**`RenameFieldsVisitor`** rewrites a `KeyExpression`'s field references by **field number**: look up the
source field by name → get its number → find the target field with the same number → use its (possibly
new) name. It recurses through Nesting (re-deriving child source/target descriptors via the parent
field's message type), Then, List, Grouping, Function, Split, Dimensions, KeyWithValue; Literal /
Version / RecordType / Empty are rename-invariant; anything else throws "field renaming not supported".

**Go infra** (Explore-mapped): all key-expression types live in `pkg/recordlayer/key_expression.go`
(+ `split_/list_/dimensions_key_expression.go`). Go's `FieldKeyExpression` is `{fieldName, fanType}` —
**no null-standin to preserve** (simpler than Java). Go has **no `KeyExpressionVisitor`** — it walks key
expressions with inline type-switch recursion (e.g. `metadata.go:bindRecordTypeKeyExpressions`).
Descriptors are `protoreflect.MessageDescriptor`; rename-by-number uses `Fields().ByName` /
`ByNumber`; deprecation is `fd.Options().(*descriptorpb.FieldOptions).GetDeprecated()`.

## 3. Fix

1. **Builder + struct** — add `allowFieldRenames`, `allowDeprecatedFieldRenames`,
   `allowUndeprecatingFields` (default `false`, matching Java) + `SetAllow*` chaining setters + an
   `allowsAnyFieldRenames()` helper.
2. **`validateField`** — replace the unconditional rename rejection with Java's exact predicate, and add
   the undeprecating check, reading `Deprecated` from each field's options. Error messages match Java's
   ("field renamed" old/new names; "field is no longer deprecated").
3. **`renameFields(expr KeyExpression, sourceDesc, targetDesc protoreflect.MessageDescriptor)
   (KeyExpression, error)`** — new file `rename_fields_visitor.go`. A recursive type-switch with
   **identical semantics** to Java's `RenameFieldsVisitor` (rename by field number; recurse into nesting
   re-deriving child descriptors; invariant leaves returned as-is; unsupported → error). **Divergence,
   documented:** Go has no `KeyExpressionVisitor` class/`expand()` dispatch, so this is a recursive
   function (the established Go pattern for key-expression walks), not a visitor object — same algorithm,
   no Java-only architecture imported. `sourceDesc == targetDesc` short-circuits to the input.
4. **`comparePrimaryKeys` + index validation** — when `allowsAnyFieldRenames()`, rewrite the OLD key
   expression via `renameFields(old, oldDesc, newDesc)` before the `proto.Equal` compare; reproduce
   Java's two-message split (changed vs does-not-match-required).

## 4. Performance

Validation is a metadata-evolution-time (schema-change) operation, not a hot path. `renameFields` runs
only when a rename option is enabled and is O(key-expression size). No effect on read/write paths.

## 5. Wire / behaviour impact

**None on persisted bytes.** This is validation logic: it *gates* which `RecordMetaData` evolutions are
accepted. A renamed field is a proto change the descriptor already encodes (rename = same field number,
new name) — the validator only decides whether to allow it and rewrites key expressions for the
*comparison*, exactly as Java does. Default behaviour is **unchanged** (all three flags default false →
renames still rejected), so no existing store/evolution is affected.

## 6. Test plan

- Port Java's `validateField` cases: default rejects rename; `allowFieldRenames` accepts; deprecated
  rename accepted under `allowDeprecatedFieldRenames` iff old- or new-deprecated, rejected otherwise;
  `allowUndeprecatingFields` gate; type/required/repeated changes still validated across a rename.
- `renameFields` unit cases (port `RenameFieldsVisitorTest` shapes): Field, Nesting (incl. child
  descriptor re-derivation), Then/Composite, List, Grouping, Function, Split, KeyWithValue; invariant
  leaves unchanged; unsupported → error; `source==target` identity; field-not-found errors.
- Integration: a primary-key and an index key expression over a renamed field validate clean when the
  rename is allowed and the renamed key expression matches, and fail with the right message otherwise.
- Update the existing `rejects field renamed` spec to assert the default (still rejects).

## 7. Scope

One commit on the RFC-135 branch (PR #336): the validator changes + `rename_fields_visitor.go` + tests.
No proto/wire change. R2–R8 remain separate.

## 8. Residual follow-up — port the missing `RenameFieldsVisitorTest` shapes (test-depth gap)

The functional port is complete and merged, but the unit coverage of `RenameFieldsVisitor` is shallower
than Java's. Java's `RenameFieldsVisitorTest.java` is parameterized across 11 shapes
(`renameMySimpleRecord`, `renameOuterRecord`, `renameMiddleRecord`, `renameMergedRecord`,
`renameSplitRecord`, `fieldMissingInSource`, `fieldMissingInTarget`, `fieldNotNestedInSource`,
`fieldNotNestedInTarget`, `avoidRewritingIfDescriptorIsTheSame`,
`avoidRewritingIfNestedDescriptorIsTheSame`). Go's `renameFields` unit block
(`metadata_evolution_validator_test.go:1637-1710`) directly exercises only top-level Field, source==target
identity, two Nesting shapes, Composite(Then), and the target-field-missing error.

**Not directly unit-tested in Go** (only indirectly via PK/index integration): the `Grouping`, `Function`,
`Split`, `Dimensions`, `KeyWithValue`, `List`, nested `RecordType`, and `Version`/`Literal`/`Empty`
branches, plus the "field not found in source" error path (`rename_fields_visitor.go:196`) and the
"parent field is not of message type" error path (`:221`). These branches are structurally trivial
(recurse + rebuild), but per CLAUDE.md's "100% Java alignment on tests" they should be pinned per
node-type. ~150-250 LOC of pure unit tests, no FDB. The one non-trivial axis is the nested-descriptor
re-derivation (`messageTypeForField`) at depth ≥ 2 / merged-record shapes — exactly Java's
`renameMiddleRecord` / `renameMergedRecord` cases.

This is a small standalone follow-up (Torvalds + codex gate), tracked here so the R1 box can be checked
with the gap recorded rather than hidden.
