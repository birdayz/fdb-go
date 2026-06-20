# RFC-128 — Nested (derived-table) LIMIT/OFFSET is silently dropped and mis-hoisted → wrong results

**Status:** Draft
**Item:** prod-readiness-audit-2026-06-19.md — surfaced while verifying the **P1 "LIMIT/OFFSET plan
continuation"** finding. The audit's literal P1 (executor re-skip on resume) is **shielded/latent** (see
§6); this RFC fixes the *genuinely reachable, deterministic wrong-results* bug found in the same area.
**Reviewers:** Graefe (Cascades alignment) + Torvalds (code quality). Query-engine change → Graefe ACK
required before merge.

---

## 1. The bug (reproduced, deterministic)

A `LIMIT`/`OFFSET` on a **derived table** (subquery in `FROM`) that sits under any outer row-shaping
operator (WHERE / ORDER BY / another LIMIT / join / aggregate) is **silently dropped from the plan and
its limit/offset is mis-hoisted to the top-level pagination**, producing wrong rows with no error.

Reproduced against real FDB (10 rows, ids 1..10):

```sql
SELECT id FROM (SELECT id FROM t ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id
```
- Correct: inner `(ORDER BY id OFFSET 2 LIMIT 5)` = `[3,4,5,6,7]`; outer `WHERE id>4` = **`[5,6,7]`**.
- Actual: **`[7,8,9,10]`** — the inner `LIMIT 5 OFFSET 2` is hoisted to the top level and applied *after*
  `WHERE id>4` (`[5,6,7,8,9,10]` → OFFSET 2 → `[7,8,9,10]`), instead of at its inner position.

This is wire-compat-irrelevant (read-side query surface) but a **correctness defect on plain SQL**.

## 2. Root cause — two coordinated sites assume every `LogicalLimit` is top-level

The builder stacks the top-level query as (innermost→outermost, `logical_builder.go:522-559`):
`Scan → Filter → Join* → Aggregate → Sort → [postSortStrip Project] → LogicalLimit → [Project] →
[LogicalDistinct]`. So above the **top-level** LIMIT the builder can place a `LogicalProject` (SELECT list)
**and** a `LogicalDistinct` (`SELECT DISTINCT`). A LIMIT below a Filter/Sort/Join/Aggregate — or inside a
derived table (spliced in directly at `:217` with no boundary node) — is a *nested* LIMIT. (The original
"only Project above LIMIT" claim was wrong on both counts — Distinct, and the missing derived boundary —
which is why §3.1 identifies the top-level LIMIT by a build-time flag rather than a tree-walk.)

Two places treat *any* `LogicalLimit` as the top-level one:

1. **`extractLimitOffset` (`embedded/cascades_generator.go:544`)** walks `children[0]` from the root and
   returns the **first** `LogicalLimit` it finds — descending through Sort/Filter/Project indiscriminately.
   When the outer query has no LIMIT, it grabs the *nested* one and hoists it to `paginatingRows`
   (`sqlLimit`/`sqlOffset`, applied post-execution, `cascades_generator.go:373` / `pageRowBudget`).
2. **`translateOp` for `LogicalLimit` (`query/cascades_translator.go:611`)** unconditionally **skips** the
   wrapper (`return t.translateOp(o.Input)`), assuming pagination handles it — so a nested LIMIT is also
   *dropped from the Cascades plan*. (The correlated-scalar-subquery path,
   `cascades_translator.go:911-931`, already does the right thing: it peels the inner `LogicalLimit` and
   re-emits it as a `LogicalLimitExpression` so the inner side is row-capped. That is the pattern to
   generalize.)

Net: a nested LIMIT is both **dropped** (site 2) and **mis-applied at the wrong pipeline position**
(site 1).

## 3. Proposed fix — mark the genuine top-level LIMIT at build time; preserve nested LIMITs as operators; envelope the nested-LIMIT continuation

> **Revised after Graefe + Torvalds NAK.** The original "walk from root through only `LogicalProject`" rule
> was wrong twice: (a) the builder also places `LogicalDistinct` above the top-level LIMIT
> (`logical_builder.go:533→552→558`, `SELECT DISTINCT … LIMIT n`), so a Project-only walk regresses it; and
> (b) a tree-walk through transparent nodes **cannot** reliably stop at a derived-table boundary — the
> derived subtree is spliced in directly (`logical_builder.go:217`, `op = innerOp`, no boundary node), so a
> derived table whose outermost op is a `Project` would be walked into and its LIMIT re-grabbed. Both holes
> share a root cause: **a post-hoc tree-walk has lost the build-time scope that distinguishes the outer
> query's own LIMIT from a nested one.** So identify the top-level LIMIT at *build time*, not by walking.

### 3.1 Mark the top-level LIMIT at build time (robust; no tree-walk)

`buildLogicalPlanForSelect` is the outer-query builder; it is re-entered recursively for derived tables
(`logical_builder.go:200`), union branches (`:106`), and CTE bodies (`:70`). Thread an `isTopLevel bool`:
the generator's entry call passes `true`; every recursive call passes `false`. The `LogicalLimit` built at
`logical_builder.go:533` gets an `IsTopLevel` field set from that flag. Then:
- `extractLimitOffset` → returns the limit/offset of the `LogicalLimit` flagged `IsTopLevel` (there is at
  most one), found by a scoped descent that **stops at the first non-`IsTopLevel` `LogicalLimit` and at any
  derived/union/CTE boundary** — but since the flag is explicit, this is unambiguous regardless of how many
  Project/Distinct/derived nodes sit around it. No transparent-wrapper allowlist to get wrong.
- A query with no top-level LIMIT (the §1 repro: outer has none, the inner is `IsTopLevel=false`) hoists
  nothing.

This is correct for `SELECT DISTINCT … LIMIT n` (the flagged LIMIT is found through the Distinct), for
stacked Projects, and for derived tables (their LIMIT is `IsTopLevel=false`).

### 3.2 Preserve a nested (`IsTopLevel=false`) `LogicalLimit` as a `LogicalLimitExpression` (translator)

`translateOp` for `LogicalLimit` (`cascades_translator.go:611`) currently skips **every** LIMIT. Change it
to: skip only the `IsTopLevel` LIMIT (handled by `paginatingRows`); for a nested LIMIT, emit a
`LogicalLimitExpression` around the translated inner — exactly the pattern
`translateProjectWithCorrelatedScalar` (`:927-931`) already uses. Graefe's note: the current
"skip-at-top / peel-for-scalar-subquery" split *is itself* the smell; making nested LIMIT a uniform
first-class operator is the Cascades-correct generalization.

### 3.3 Envelope the nested-LIMIT continuation — MANDATORY (Graefe's blocker)

Graefe proved the §6 re-skip is **reachable** for the §1 repro: a derived-table LIMIT is a *streamed*
inner under the outer Filter/Sort (`logical_builder.go:217`), and `executeSort` decodes + resumes its inner
mid-window (`executor.go:1021`), `executeFilter` passes the continuation through (`:749`), so `executeLimit`
(`:818,:823`) is reached with a **non-nil** continuation and re-skips `offset` + resets `limit` the moment a
result spills a page (page/scan/time cap). So the "fresh-execution, safe" assumption is FALSE here, and the
continuation envelope is **required, not optional**:
- Add a `LimitContinuation` proto envelope `{ inner_continuation, skipped_so_far (or remaining_offset),
  emitted_so_far (or remaining_limit) }`. `executeLimit` decodes it: resume the child from
  `inner_continuation`, and reconstruct `SkipThenLimit` with the **remaining** offset/limit (so a resume
  past the skip does not re-skip, and a partially-consumed limit is not reset). On no/empty continuation,
  full `offset`/`limit` as today.
- This is the faithful fix for the audit's literal P1 too — it stops being shielded the moment §3.2 starts
  preserving streamed nested LIMITs, so it must ship together.

## 4. Executable spec (regression tests)

All revert-proven (fail before the fix — the repro already does):
1. **The repro:** `SELECT id FROM (… ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id` → `[5,6,7]`.
2. **Derived LIMIT under outer ORDER BY + outer LIMIT:** `SELECT id FROM (… LIMIT 5 OFFSET 2) AS s ORDER BY
   id LIMIT 3 OFFSET 1` → the inner caps to 5, the outer paginates that — pins that *both* limits apply.
3. **Derived LIMIT, no outer shaping:** `SELECT id FROM (… LIMIT 3) AS s` → still `[1,2,3]` (guards
   over-correction — a single derived LIMIT with no outer op still produces the right rows).
4. **Plain top-level LIMIT unaffected:** `SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2` → `[3,4,5]`.
5. **`SELECT DISTINCT … LIMIT n` (Graefe + Torvalds blocker 1):** e.g. `SELECT DISTINCT v FROM t2 ORDER BY v
   LIMIT 3 OFFSET 1` over a table with duplicate `v` → the top-level LIMIT is still hoisted correctly (the
   `IsTopLevel` flag is found through the Distinct). Revert-proven against a Project-only walk.
6. **Stacked-Project / computed top-level projection + LIMIT** → top-level LIMIT still hoisted.
7. **Forced-paging mid-window resume (Graefe blocker 2 — the merge gate):** the §1 repro run with a tiny
   page budget (small `MaxRows`/scan cap) so the inner derived LIMIT window is split across ≥2 pages →
   still `[5,6,7]`. Revert-proven: without the §3.3 continuation envelope this returns a wrong (re-skipped /
   over-returned) result. This is the test that proves the re-skip is fixed, not merely shielded.
8. **EXPLAIN:** the derived-table case shows a `Limit(` operator in the inner plan (no longer dropped).

## 5. Wire-compat impact

**None** — read-side query surface only; no persisted bytes, key encoding, or continuation-token format
change. (Java core has no SQL LIMIT plan node at all — `RecordQueryLimitPlan` is a Go-only read-side
extension; this RFC makes that extension *correct*, which CLAUDE.md requires of Go-only extensions with
deep test coverage.)

## 6. The audit's literal P1 (executor re-skip) is shielded — recorded, not fixed here

For the record (verified, mirrors the P0 outcome): the audit's P1 "physical LIMIT/OFFSET plan continuation
re-skips on resume" is a **real executor-level bug but unreachable from SQL today**. Java's
`SkipCursor`/`RowLimitedCursor` (`SkipCursor.java:54-72`, `RowLimitedCursor.java:54-73`) also do **not**
envelope skip/limit in the continuation — Go matches Java exactly — and Java never combines a non-zero skip
with a resume continuation (`QueryPlanCursorTest.java:110-144`). The top-level SQL LIMIT is handled by
`paginatingRows` (correct across pages); the only SQL-reachable nested `RecordQueryLimitPlan` (FlatMap
inner) is re-run fresh per outer row, never resumed mid-window. So it is **not** a Java divergence and not
a prod blocker. §3.3 is the only place this RFC must make sure it **stays** shielded after we start
preserving nested LIMITs.

## 7. Resolved (Graefe blocker 2): the continuation envelope is mandatory

The §3.3 open question is **closed**: Graefe traced that a derived-table LIMIT under an outer Filter/Sort is
a streamed inner that `executeSort` resumes mid-window (`executor.go:1021` → `:749` → `:818,:823`), so the
re-skip is reachable for the §1 repro the moment a page spills. The `LimitContinuation` envelope (§3.3) is
**required** and ships in this PR, gated by the §4.7 forced-paging regression. The audit's literal P1
re-skip is thereby fixed (not merely left shielded), because §3.2 makes the streamed nested LIMIT real.

This is now a three-part change — (1) build-time top-level marker, (2) translator preserves nested LIMIT,
(3) executor continuation envelope — that must land together: (1)+(2) without (3) would *introduce* a
reachable re-skip; (3) without (1)+(2) fixes nothing user-visible.
