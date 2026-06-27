# RFC-147 — Remove vestigial `EmbeddedConnection` scope state (RFC-145 Phase-3 follow-up)

**Status:** Draft
**Item:** TODO.md "Known gaps" / RFC-145 Phase-3 follow-up (Torvalds, non-blocking) — residual
read-but-never-written connection state left after the embedded-interpreter island was deleted.
**Reviewers:** Torvalds (code quality) + codex + @claude. **Not a Cascades change** — this is the SQL
expression evaluator for INFORMATION_SCHEMA WHERE / INSERT-VALUES folding, not the planner/cost/matching/
executor; **no Graefe gate** (route Graefe-aware as a courtesy since it is executor-adjacent, but his
surface is untouched).
**Classification:** pure dead-state cleanup, behavior-preserving, net-negative LOC.

---

## 1. Problem (verified real)

RFC-145 deleted the legacy embedded SQL interpreter island. The island was the only writer of two
`EmbeddedConnection` fields, so they are now **read but never written** (always nil):

- `validQualifiers map[string]bool` — `connection.go:79` (doc 73-79 already notes "After RFC-145 removed
  the legacy executor nothing populates it").
- `outerScopes []outerScope` — `connection.go:86` (doc 81-86, same note).

Dead state that reads as live is a trap: the next reader assumes the qualifier/outer-scope machinery
works, when in fact every branch gated on these fields is unreachable.

## 2. Investigation — confirm dead, confirm removal is behavior-preserving

**All write sites (production):** only two nil-resets in `ResetSession` —
`connection.go:567` `c.outerScopes = nil`, `connection.go:568` `c.validQualifiers = nil`. No non-nil
production writer exists (grepped `\.X =` and struct-literal `X:` forms). The struct-literal hits at
`plan_visitor.go:705`, `logical_predicate.go:1401,3050,5010` are a **different** field —
`outerScopes map[string]semantic.ScopeSource` on the cascades logical-builder (`logical_predicate.go:4804`)
— do not conflate. The only non-nil writer of the `EmbeddedConnection` field is a **test**:
`scope_test.go:21,39,56` constructs `&EmbeddedConnection{outerScopes: …}` to exercise the read helpers.

**All read sites:**
- `eval_map.go:57` — `conn.validQualifiers != nil && !conn.validQualifiers[qualUpper]` (always-nil → dead)
- `eval_map.go:58,65,81` — `outerScopesContainQualifier` / `len(conn.outerScopes) > 0` (always false/0 → dead)
- `eval_proto.go:147` — `len(conn.outerScopes) > 0` (always 0 → dead correlated fallback)
- `scope.go:50-135` — the read helpers `outerScopesContainQualifier` + `resolveOuterColumn`, called
  only from the dead blocks above.

**Behavior-preserving collapse.** In `eval_map.go` (lines 44-91, the qualified-reference block): with
both fields always nil, the `if` at 57 and `else if` at 65 are false, so the `else` at 75-77
(`v, found = row[ref.bare()]`) always runs, and the 81-89 outer-scope fallback is unreachable. The block
reduces, behavior-identical, to:

```go
v, found := row[name]
if !found && ref.isQualified() { v, found = row[ref.bare()] }
if !found { return nil, ErrUndefinedColumn(name) }
```

`eval_proto.go`'s 145-155 correlated fallback is likewise unreachable and deletes cleanly. The two live
consumers after RFC-145 — INFORMATION_SCHEMA single-source WHERE and constant INSERT-VALUES folding —
never set a correlated/qualifier scope, so "always nil" holds on every live path.

**Third orphan already gone.** No `cteData`/`ctes`/`cteScope` field exists on `EmbeddedConnection`
(matches the TODO note "removed in Phase 2"). The remaining `ctes` hits are unrelated Cascades-builder
loop variables.

## 3. Fix (mechanical, net-negative)

1. `connection.go`: delete fields `validQualifiers` (73-79) and `outerScopes` (86) + the two nil-resets
   (567-568). **Keep `currentSourceAliases`** — separate, still-live field (read `eval_proto.go:129`,
   reset `connection.go:569`).
2. `eval_map.go`: collapse the 44-91 block to the 3-line form above; drop the now-unused `strings`
   import if `strings.ToUpper(ref.table)` was its only use.
3. `eval_proto.go`: delete the 145-155 outer-scope fallback.
4. `scope.go`: the whole file becomes dead — delete `outerScope` type (44-48),
   `outerScopesContainQualifier` (50-62), `resolveOuterColumn` (64-135). File goes.
5. `scope_test.go`: delete — it is the only non-nil writer and tests the removed helpers.

## 4. Wire / behaviour impact

None. Behavior-preserving deletion of unreachable code. No wire, no plan, no executor-output change.

## 5. Test plan

- `just test` green (the deleted `scope_test.go` loses, the kept INFORMATION_SCHEMA-WHERE /
  INSERT-VALUES tests pin the surviving path).
- **Add the missing pin** (per "every dimension gets a test"): a small test of the kept qualified-fallback
  (`row[name]` → `row[ref.bare()]`) for the system-table path, independent of the now-deleted helpers, so
  the collapse is pinned and a future regression that re-introduces a qualifier branch is caught.

## 6. Gate & risk

Torvalds + codex + @claude. Risk: trivial — the one subtlety is that the removed field has a **test-only**
writer (`scope_test.go`), so the test goes with the field; confirm no reflection/`FieldByName` writer
exists (none found). Re-run the full `embedded` suite.

## 7. Scope

In: the five-file deletion + the one pinning test. Out: `currentSourceAliases` (live, keep); any
restructuring of the kept INFORMATION_SCHEMA / INSERT-VALUES eval paths beyond the mechanical collapse.
