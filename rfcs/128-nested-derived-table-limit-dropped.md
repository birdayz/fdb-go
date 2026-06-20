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

The builder stacks the top-level query as (innermost→outermost, `logical_builder.go:145-152`):
`Scan → Filter → Join* → Aggregate → Sort → LogicalLimit → LogicalProject`. So the **top-level** LIMIT is
reachable from the root through **only a `LogicalProject`**. A LIMIT below a Filter/Sort/Join/Aggregate is
a *nested* (derived-table or set-op-branch) LIMIT.

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

## 3. Proposed fix — only hoist the genuine top-level LIMIT; preserve nested LIMITs as operators

### 3.1 Strip only the projects-reachable top-level LIMIT (replaces `extractLimitOffset`)

`stripTopLevelLimit(root) (newRoot LogicalOperator, limit, offset int64)`:
- Walk from `root` through **only** `LogicalProject` nodes (the lone operator the builder places above the
  top-level LIMIT). If a `LogicalLimit` is reached this way, it **is** the top-level LIMIT: splice it out
  (replace it with its `Input`) and return `(newRoot, lim.Limit, lim.Offset)`.
- Stop and return `(root, -1, 0)` at the first node that is **not** a `LogicalProject` and not the
  top-level `LogicalLimit` (Filter/Sort/Join/Aggregate/Union/nested Limit) — there is no top-level LIMIT;
  do **not** hoist.

So the hoist is byte-for-byte what it is today for genuine top-level `... ORDER BY ... LIMIT n`, but a
nested LIMIT under WHERE/ORDER/etc. is left in the tree.

### 3.2 Preserve a nested `LogicalLimit` as a `LogicalLimitExpression` (translator)

Since §3.1 removes the top-level LIMIT from the tree *before* translation, any `LogicalLimit` that
`translateOp` still encounters is **nested** → it must be preserved, not skipped. Change
`cascades_translator.go:611` from "skip" to "emit a `LogicalLimitExpression` around the translated inner"
— exactly what `translateProjectWithCorrelatedScalar` (`:927-931`) already does for the correlated-scalar
case. This makes the nested limit a real Cascades operator (`RecordQueryLimitPlan` after implementation),
applied at its correct position in the pipeline.

### 3.3 Continuation correctness of the now-preserved nested LIMIT

A preserved nested `RecordQueryLimitPlan` is executed by `executeLimit` (`executor.go:792`) →
`SkipThenLimit(offset, limit)`. The audit's P1 re-skip (§6) is that, on a mid-window resume, `executeLimit`
re-wraps `SkipThenLimit` fresh and re-skips `offset`. We must ensure the preserved nested LIMIT is **not
resumed mid-window**, or make its continuation envelope skip-consumed/limit-remaining. Investigation shows
the existing SQL-reachable nested `RecordQueryLimitPlan` (FlatMap inner side) is re-run **fresh** per outer
row (`nil` continuation) and so is safe; this RFC must confirm the derived-table case is executed the same
way (the inner is bounded by `ReturnedRowLimit = offset+limit` and, where it is a FlatMap/nested inner,
restarted fresh), **or** add the continuation envelope. This is the one open design point for Graefe — see
§7.

## 4. Executable spec (regression tests)

All revert-proven (fail before the fix — the repro already does):
1. **The repro:** `SELECT id FROM (… ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id` → `[5,6,7]`.
2. **Derived LIMIT under outer ORDER BY + outer LIMIT:** `SELECT id FROM (… LIMIT 5 OFFSET 2) AS s ORDER BY
   id LIMIT 3 OFFSET 1` → the inner caps to 5, the outer paginates that — pins that *both* limits apply.
3. **Derived LIMIT, no outer shaping:** `SELECT id FROM (… LIMIT 3) AS s` → still `[1,2,3]` (the top-level
   hoist of a single derived LIMIT with no outer op must remain correct — guards over-correction).
4. **Plain top-level LIMIT unaffected:** `SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2` → `[3,4,5]` (the
   hoist path is unchanged for genuine top-level LIMIT).
5. **EXPLAIN:** the derived-table case shows a `Limit(` operator in the inner plan (it is no longer dropped).

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

## 7. Open question for Graefe

After §3.2 starts preserving nested derived-table LIMITs as `RecordQueryLimitPlan`, can such a plan be
**resumed mid-window** by the internal drain (making the §6 re-skip reachable)? Two options:
1. **Confirm fresh-execution / bounded-in-page** (preferred if true): the nested LIMIT inner is bounded by
   `ReturnedRowLimit = offset+limit` and restarted fresh wherever it is a nested/inner side, so it never
   resumes mid-window — no envelope needed; add a paging regression to prove it.
2. **Add the continuation envelope** (`skip-consumed` / `limit-remaining` in a `LimitContinuation` proto)
   if (1) cannot be guaranteed — the faithful fix for the executor bug, making nested LIMIT page-correct.

Recommendation: implement §3.1 + §3.2 (the reachable wrong-results fix) and prove (1) with a paging test;
fall back to (2) only if a mid-window resume is demonstrable.
