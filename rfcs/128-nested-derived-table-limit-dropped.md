# RFC-128 — SQL LIMIT/OFFSET: remove the post-execution hoist, make LIMIT a uniform plan operator (fixes nested derived-table / CTE / union LIMIT → wrong results)

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

## 3. Proposed fix — remove the post-execution hoist; make EVERY LIMIT a uniform `RecordQueryLimitPlan`; envelope its continuation

> **Revised twice.** v1 ("walk from root through only `LogicalProject`") missed `LogicalDistinct` and could
> not stop at a derived-table boundary. v2 (a build-time `IsTopLevel` flag) was wired to `buildSelectShell`
> — which the call-graph map showed is **Explain-only**; the live builder is the `PlanVisitor`
> (`cascades_generator.go:269`), whose LIMIT is born in `visitLimit` (`plan_visitor.go:1203`). More
> fundamentally, the flag would still leave `extractLimitOffset`'s `children[0]` walk broken for **two more
> shapes**: a top-level CTE (`LogicalCTE.Children()[0]` is the CTE *Body*, not `Main` — `operators.go:557` —
> so the outer LIMIT is missed and a CTE-body LIMIT can be wrongly hoisted) and a per-branch union LIMIT.
> The clean fix removes the ambiguity class entirely instead of patching the walk.

### 3.1 Delete the hoist (the root cause), keep every LIMIT in the plan

The hoist (`extractLimitOffset`, `cascades_generator.go:373/544`, → `paginatingRows.sqlLimit/sqlOffset`,
applied post-execution) exists **only** to apply SQL `LIMIT`/`OFFSET`. The physical operator that does the
same job already exists and is already used for nested limits: `LogicalLimitExpression` →
(`rule_implement_limit.go`) → `RecordQueryLimitPlan` → `executeLimit` (`executor.go:792-823`), which sets
`ReturnedRowLimit = offset+limit` for scan-bounding (`:811`) and applies `SkipThenLimit` (`:823`).
Push-down/merge rules already exist (`rule_push_limit_through_{projection,union}.go`, `rule_limit_merge.go`,
`rule_zero_limit.go`, `rule_noop_limit_elim.go`). So:
- **Remove `extractLimitOffset` and the `sqlLimit/sqlOffset` SQL-LIMIT path** in `paginatingRows`
  (`cascades_generator.go:373-393`, the `sqlLimit/sqlOffset` machinery at `:916-919/1042/1059/1188-1214`).
  Keep `maxRows`/`maxResultBytes`/`pageRowBudget`'s **non-LIMIT** caps — they are independent of SQL LIMIT.
- **No client-facing dependency is lost.** Verified: `r.continuation` is purely internal to one
  `paginatingRows` lifetime (`:937,1361,1389`) — never returned as a resumable SQL token — so nothing
  outside relies on the hoist.

This single deletion fixes the derived-table bug (§1), the CTE bug, and the union-branch bug at once,
because none of them can mis-hoist what is no longer hoisted.

### 3.2 Stop skipping the top-level LIMIT in the translator (uniform operator)

`translateOp` for `LogicalLimit` (`cascades_translator.go:611-617`) currently **skips** the wrapper. Change
it to translate **every** `LogicalLimit` to a `LogicalLimitExpression` (→ `RecordQueryLimitPlan`), exactly
as the correlated-scalar path already does (`:917-933`). The LIMIT is then applied at its correct pipeline
position by the operator, top-level and nested alike — **one** uniform treatment of LIMIT, matching the
CLAUDE.md "one query path" directive. (Graefe Q2 endorsed the uniform-operator direction.) EXPLAIN now
*shows* the top-level `Limit(...)` node (today it is invisible because skipped) — a correctness
improvement; affected EXPLAIN goldens are updated.

### 3.3 Envelope the LIMIT continuation — MANDATORY (Graefe + Torvalds confirmed)

With the LIMIT now a streamed operator, its skip/limit state must survive the **per-page transaction
rollover** `paginatingRows` does (`fetchPage` opens a fresh txn per page, `cascades_generator.go:1312`).
Both reviewers confirmed the re-skip is reachable: `executeSort` resumes its inner from a decoded
continuation (`executor.go:1021`) → reaches `executeLimit` (`:818`) which rebuilds `SkipThenLimit(offset,
limit)` with the **full** offset/limit (`:823`) → re-skips on resume. Fix:
- Add a `LimitContinuation` proto `{ inner_continuation, remaining_offset, remaining_limit }`. `executeLimit`
  decodes it: resume the child from `inner_continuation` and reconstruct `SkipThenLimit` with the
  **remaining** offset/limit. The `skipCursor`/`limitRowsCursor` (`cursor_combinators.go:56,93`) must emit
  this envelope as their continuation (they currently forward the inner's). On empty continuation, full
  `offset`/`limit` as today.
- This is the faithful fix for the audit's literal P1 re-skip — it must ship with §3.1/§3.2, since making
  LIMIT a streamed operator is exactly what makes the re-skip reachable.

## 4. Executable spec (regression tests)

All revert-proven (the §1 repro already fails pre-fix):
1. **The repro:** `SELECT id FROM (… ORDER BY id LIMIT 5 OFFSET 2) AS s WHERE id > 4 ORDER BY id` → `[5,6,7]`.
2. **Derived LIMIT under outer ORDER BY + outer LIMIT:** `SELECT id FROM (… LIMIT 5 OFFSET 2) AS s ORDER BY
   id LIMIT 3 OFFSET 1` → both limits apply.
3. **Derived LIMIT, no outer shaping:** `SELECT id FROM (… LIMIT 3) AS s` → `[1,2,3]` (guards over-correction).
4. **Plain top-level LIMIT unaffected:** `SELECT id FROM t ORDER BY id LIMIT 3 OFFSET 2` → `[3,4,5]`.
5. **`SELECT DISTINCT … LIMIT n`:** `SELECT DISTINCT v FROM t2 ORDER BY v LIMIT 3 OFFSET 1` over duplicate `v`
   → correct (the LIMIT operator applies after DISTINCT regardless of node stacking).
6. **Top-level CTE + LIMIT (the `LogicalCTE.Children()[0]==Body` mis-walk):** `WITH c AS (SELECT id FROM t)
   SELECT id FROM c ORDER BY id LIMIT 3` → `[1,2,3]`; and a LIMIT *inside* the CTE body
   (`WITH c AS (SELECT id FROM t LIMIT 2) SELECT id FROM c`) → `[1,2]` (not mis-hoisted). Revert-proven —
   pre-fix the outer LIMIT is missed / the body LIMIT mis-hoisted.
7. **Union-trailing + per-branch LIMIT:** `(SELECT id FROM t LIMIT 2) UNION ALL (SELECT id FROM t2 LIMIT 2)`
   and `(SELECT … UNION ALL SELECT …) ORDER BY id LIMIT 3` → each branch capped, trailing limit applied.
8. **Multi-page rollover (the re-skip merge gate, Graefe blocker 2):** the §1 repro and a plain
   `LIMIT k OFFSET m` run with a tiny page budget (small `MaxRows`/scan cap) so the LIMIT window splits
   across ≥2 `fetchPage` transactions → correct rows. Revert-proven: without the §3.3 envelope the resume
   re-skips `offset` / resets `limit` and returns wrong rows. This is the test that proves the re-skip is
   fixed, not merely shielded.
9. **EXPLAIN:** the top-level LIMIT now shows a `Limit(` node (previously invisible because skipped);
   affected EXPLAIN goldens updated.
10. **Determinism:** the planner-affecting tests run ≥10× (query-engine skill) — a uniform LIMIT operator
    must not introduce plan nondeterminism.

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

## 7. Scope, risk, and the three coupled parts

This is a planner/optimizer change (Graefe ACK required on RFC **and** implementation). Three parts land
together:
1. **Delete the hoist** (`extractLimitOffset` + `paginatingRows` SQL-LIMIT machinery) — removes the
   ambiguity class that caused the derived / CTE / union mis-hoists.
2. **Translate every LIMIT uniformly** to `RecordQueryLimitPlan` (stop skipping the top-level one).
3. **`LimitContinuation` envelope** — required because (1)+(2) make LIMIT a streamed operator resumed across
   per-page transaction rollover; without it the re-skip (audit P1) becomes reachable.

(3) without (1)+(2) fixes nothing user-visible; (1)+(2) without (3) would *introduce* a reachable re-skip —
so they are one PR.

**Risk surface to pin with tests (§4):** EXPLAIN goldens change (LIMIT now visible — a correctness
improvement); plan-cache keys on SQL text not the tree (`plan_cache.go:68`, Torvalds-verified) so caching is
unaffected; DML / `MAX_ROWS` / byte-cap paths are LIMIT-independent and stay in `paginatingRows`; the
existing push-down/merge rules already handle the new top-level `RecordQueryLimitPlan`. Determinism is
re-checked ≥10× on the affected planner tests.
