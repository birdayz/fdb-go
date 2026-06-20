# RFC-136 — Metadata-evolution field renames (Java 4.12 parity, RFC-135 §4 R1)

**Status:** Draft
**Item:** RFC-135 §4 **R1** — port Java 4.12's `MetaDataEvolutionValidator` field-rename support +
`RenameFieldsVisitor`.
**Reviewers:** Torvalds (code/test quality) + codex + @claude. **Not a query-engine change** — this is
record-layer metadata-evolution validation (no planner/cost/matching/executor), so **no Graefe gate**.

---

## 1. Problem (verified real)

Java added configurable field-rename support to `MetaDataEvolutionValidator` *between* 4.11.1.0 and
4.12.11.0 (`8b03a10ea` #4034 "Support field renames", `05f622297` #4119 "ignore field renames only on
deprecated fields"). The 4.12 validator has three new options — `allowFieldRenames`,
`allowDeprecatedFieldRenames`, `allowUndeprecatingFields` — and uses `RenameFieldsVisitor.renameFields`
to rewrite primary-key and index key expressions across a rename before comparing them.

Go's `MetaDataEvolutionValidator` (`pkg/recordlayer/metadata_evolution_validator.go`) has **none** of
this: `validateField` (`:992`) unconditionally rejects any field-name change ("field %q renamed to %q
in message %q"), and `comparePrimaryKeys` (`:342`) / index validation (`:421`) compare key expressions
with `proto.Equal` of the *un-rewritten* expressions. So Go cannot validate a metadata evolution that
renames a field even when the rename is semantically safe — a genuine 4.12 parity gap (real feature
Java supports, not a Go-extension).

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
