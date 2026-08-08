# RFC-216 — The projected-EXISTS fold must read the sort key's resolved path, not a re-derived name

**Status:** DRAFT — awaiting Graefe + Torvalds
**Origin:** RFC-197 ratchet entry `cascades_translator.go # sortKeyFieldRef`, whose stated
mechanisms were measured false; the live bug behind it is this one
**Scope:** `pkg/relational/core/query/cascades_translator.go` (the `sortSource` helpers), plus
one negative pin outside it

---

## 0. What this RFC is not

It is not the conversion the ratchet entry prescribed. That entry ruled CONVERT — correctly —
but justified it with two mechanisms, and both are false:

- **"Two keys that render alike collapse to one appended column."** They do, and it is
  harmless. `sortKeySourceValue` depends on the key *only* through `sortKeyFieldRef(k)`, so
  alike-rendering keys carry an identical source value by construction. Measured over five key
  shapes against both source classifications.
- **"`pullUpSortKeyValue` recovers them by scanning the folded projection's field names."** It
  does not. It recovers them at the **value** match. Measured by renaming an appended column to
  a string no name scan could find; resolution succeeded unchanged.

Carrying the append index `len(fields)+i` fixes neither, because neither is broken. The real
defect is one level down and this RFC is about that.

---

## 1. The bug, in two shapes, both user-facing

Schema: `CREATE TYPE AS STRUCT nst (sk BIGINT)`, `CREATE TABLE t1(id BIGINT, n nst, PRIMARY KEY(id))`.

```
SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY n.sk
  -> ERROR values: no ordering defined between *dynamicpb.Message and *dynamicpb.Message

SELECT id, EXISTS (SELECT 1 FROM t2 WHERE t2.t1_id = t1.id) AS has_t2 FROM t1 ORDER BY t1.n.sk
  -> ERROR ordinal resolution: field "SK" not resolvable in the runtime row
     (ordinal -1, row columns [ID N]) — malformed plan
```

Both reach `sortKeySourceValue` — confirmed by instrumenting it and first verifying the
instrument was live (7 hits on `TestFDB_ProjectedExists_Round4`). The printed state:

```
field="N"       Expr="N.SK"    Bare="SK" Qual="N"    Qualified=true Value=&FieldValue{Field:"N", Resolved:<non-nil>}
field="T1.N.SK" Expr="T1.N.SK" Bare="SK" Qual="T1.N" Qualified=true Value=<nil>
```

These are **two different bugs** that happen to share a symptom, and conflating them is how the
ratchet entry went wrong. They are separated in §2 and §3.

---

## 2. Shape A — `ORDER BY n.sk`: fold-local, and the fix is confined to one arm

The resolver does the right thing. `ResolveIdentifier` recognises `N` as a struct column
(`semantic/scope.go` Rule 5) and `fuseNestedAccessors` (`query/expr/expr.go`) fuses the
reference into **one** `FieldValue` whose `Field` is the struct root `N` and whose
`Resolved.Accessors` is the full `[N, SK]` path, with `Typ` the leaf type. The `.SK` is present
throughout — in `Resolved`, not in `Field`.

**The ordinary, non-fold sort path is correct, and correct for the right reason:**
`translateSort` never renders a key whose `Value` is set, and `bakeFlatRefsAgainstColumns`
returns early on `fv.Resolved != nil`, passing the resolved path through to the executor
untouched. It is correct *structurally*, not by consulting parse-tree segments.

The fold then throws that away, at `cascades_translator.go:4947`:

```go
if fv, ok := k.Value.(*values.FieldValue); ok {
    if fv.Child == nil {
        return strings.ToUpper(fv.Field)   // <-- "N". fv.Resolved.Accessors ignored.
    }
    return strings.ToUpper(values.ColumnNameValue(fv))
}
```

`fv.Child == nil` holds for a single-table nested reference, so the function returns the struct
root. Every fold decision flows from that one string — `sortKeyName`, `sortKeySourceValue`,
`collectExtraSortColumns` — so the fold appends the whole struct as the hidden column and sorts
by it. `sortKeySourceValue` compounds it by re-minting a **fresh, path-less** `FieldValue` from
the flat name, discarding the resolver's work a second time.

The other arm was already right: `values.ColumnNameValue` renders a multi-accessor path
dot-joined. The information was in hand; the fold did not ask.

**Decision.** The fold stops re-deriving a name from a Value that already carries the answer.
`sortKeySourceValue` returns `k.Value` itself when it is a `*FieldValue` with a non-nil
`Resolved` — the same early-out `bakeFlatRefsAgainstColumns` already uses, for the same reason.
The rendered name survives only where a name is genuinely wanted (the hidden column's label and
the nameability guard), and is derived from the full path rather than the root.

**Why not the alternatives.** Making `sortKeyFieldRef` render the full path
(`ColumnNameValue` in both arms) fixes the symptom and keeps the round trip: the name would
then be `N.SK`, which `stripSortQualifier`'s last-dot rule turns straight back into `SK`. That
is the defect restated. Declining the fold for nested keys is the other tempting move and it is
a capability regression — Java plans this shape, so declining would be a Go-only narrowing
dressed as a fix.

---

## 3. Shape B — `ORDER BY t1.n.sk`: not a fold bug, and it does not convert here

`walkColumnRef` rejects a 3-segment FullId outright
(`query/expr/walk.go`, `UnsupportedExpressionShapeError{"FullId with 3 segments"}`), and
`upgradeSortKeyValues` swallows the walk error with `continue`, leaving `Value` nil and only the
text `T1.N.SK`. So a table-qualified nested path is unresolved **everywhere**, not just in the
fold — the ordinary path has the same hole.

**Decision.** Out of scope for this RFC, and it must not be fixed by teaching the fold to split
`T1.N.SK`. The last-dot rule at `stripSortQualifier` yields `SK`; `splitQualifier`'s own doc
already concedes it manufactures qualifier `T1.N` for `A.B.C`. Any fold-side handling of this
shape is guessing at a segmentation the resolver declined to make. The correct fix is in
`walkColumnRef` — resolve the 3-segment FullId — and it is a separate item. What this RFC owes
it is the negative pin in §5 so the hole cannot close silently and re-arm the fold.

---

## 4. The last-dot survey, and what it does and does not license

Every `strings.LastIndex` / `splitQualifier` site in `core/query` and `core/embedded` was
surveyed. Two can demonstrably receive a >2-segment input today, and both are the fold's:
`stripSortQualifier` and `splitQualifier`, reached only from the `sortSource` helpers.

Several others (`expressionOutputColumns`, `stripColumnQualifier` for GROUP BY,
`derived_unnest.go`'s projection-output naming) carry the *same* last-dot assumption over
projection/group-key renderings, and I could not establish from reading whether a nested
projection reaches them. **That is recorded as unknown, not as safe.** They are not touched by
this RFC and no claim is made about them; a `SELECT n.sk` / `GROUP BY n.sk` probe is the way to
settle it and is not a precondition for this change, because this change does not alter what
those sites receive.

---

## 5. Deliverables

1. The fix in §2, in `cascades_translator.go` only.
2. **The earning test on the unprobed dimension.** There is no test anywhere under
   `pkg/relational` that orders by a nested struct field — on either path. The one
   `ORDER BY <qual>.<col>` conformance case uses a table alias, not a struct. So the dimension
   is not thin, it is absent, which is why this shipped green. The test asserts correct row
   order for `ORDER BY n.sk` in the fold, and mutation-verifies red against the reverted fix.
3. **A companion test on the non-fold path** asserting `SELECT id FROM t1 ORDER BY n.sk` is
   correct. This is currently a code-path *reading*, not a measurement, and an unmeasured
   "the other path is fine" is exactly the corollary-inherits-authority error.
4. **A negative pin for §3** asserting the 3-segment shape is refused, with a failure message
   naming what re-arms if `walkColumnRef` learns to resolve it.
5. The corpus plan-shape comparison — see §6.

---

## 6. Precondition: the plan-shape run is not runnable at present disk

This is translator surgery in the RFC-141 fold and it has plan-shape exposure, so the corpus
comparison is a required deliverable, not an optional one. **It cannot be run right now and was
not run.** `df -h .` reports the filesystem at 100% with 8.8G free, and the repo's own rule is
that ext4 above ~95% degrades point-lookup latency sharply and reports as a planner regression.
A run taken now would measure the disk.

Stating it here rather than skipping it silently: the comparison is owed before implementation
merges, and if goldens move, every changed record gets reviewed individually rather than
re-blessed in bulk. "Nothing moved" is a result to state, not a step to skip.

---

## 7. Ratchet accounting

Unchanged at **46 escapes / 33 authorities** — `TestFieldNameNeverDecides` and
`fieldDebtAuthorityTotal` green. The `sortKeyFieldRef` entry was rewritten, not retired;
nothing was implemented, so nothing retires. The companion `pullUpSortKeyValue` entry records
that its stated blocker is removable by this conversion (measured) and that it, too, retires
nothing yet.

Implementing §2 is expected to retire neither escape on its own: `sortKeyFieldRef` keeps
returning a string for the naming and guard paths. Retiring it is a follow-on that this RFC
deliberately does not claim.
