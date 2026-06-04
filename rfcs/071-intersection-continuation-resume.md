# RFC-071: Intersection cursors resume mid-stream (P2.3)

**Status:** Implemented
**Area:** Cascades executor (intersection continuation decode)
**Reviewers:** Graefe (Cascades/cursor alignment — mandatory), Torvalds (code quality), codex, @claude

## Problem

Both intersection executors *emit* a correct per-child `IntersectionContinuation`
proto (shared `buildIntersectionContinuation`), but neither *decodes* it on
resume:

- `executeIntersection` (regular set-intersection) silently starts every child
  from a `nil` continuation — on a cross-transaction / limit-split resume it
  restarts each child from the beginning, **duplicating rows** (documented but
  unfixed).
- `executeMultiIntersection` (the RFC-069 multi-aggregate `GROUP BY`
  count+sum path) **errors loudly** on a non-nil incoming continuation rather
  than silently corrupting results (the guard added in RFC-069).

Benign for typical small-group aggregates (few groups, one scan pass, no
resume), but a correctness/availability gap for any intersection that spans a
transaction boundary or a returned-row limit. Surfaced by @claude + codex on
PR #249. TODO P2.3.

## Investigation

**Java** (`IntersectionCursorContinuation` / `KeyedMergeCursorState` /
`MergeCursorState`, tag 4.11.1.0): on resume, the parent `IntersectionContinuation`
proto is split into N per-child continuations via a 3-way classification:
- `!started` → `START` → child created fresh (begin continuation),
- `started` + bytes → mid-stream → child created from those bytes,
- `started` + no bytes → `END` → child is `empty()` (exhausted).

The Go producer (`buildIntersectionContinuation`) already encodes exactly this
shape: `childStarted := child.hasResult || cursor.started`; exhausted children
get empty bytes (→ END), mid-stream children get `result.GetContinuation()`
bytes (→ MID). So the decode is the precise inverse.

**Continuation nesting:** `applySkipLimit` wraps the intersection cursor with
`skipCursor`/`limitRowsCursor`, but those **delegate** the inner cursor's
continuation (no `buildContinuation` of their own — they return the inner
result directly). So the `continuation []byte` reaching the executor *is* the
raw `IntersectionContinuation` proto; no unwrapping required.

**Per-child `started` (the architectural property).** Java's `KeyedMergeCursorState`
carries a per-child `started` flag; the proto's `started` field is that property,
not a derived observable. Today's Go producer derives it as `child.hasResult ||
cursor.started`, which happens to be correct only because `intersectionCursor.OnNext`
sets the cursor-level `started=true` (line 306-313) before any checkpoint — a
fragile coupling to `OnNext`'s internal ordering that would break the moment that
ordering changes (CLAUDE.md principle #10). We instead track `started` **per child
in `mergeChildState`** (matching Java): set in `advance()` (a child that has been
advanced has started) and **seeded from the decoded continuation on resume** (MID/END
children → `true`, START → `false`). `buildIntersectionContinuation` then encodes
`childStarted := child.started` directly. This is robust regardless of checkpoint
timing — a resumed mid-stream child can never be re-encoded as START and restarted.

## Fix

1. **`merge_cursor.go`** —
   - `mergeChildState` gains a `started bool` field; `advance()` sets it `true`.
     `buildIntersectionContinuation` encodes `childStarted := child.started`
     (per-child, no behavior change for the non-resume path: a fresh child is
     `started` after its first `advance`).
   - Add `decodeIntersectionContinuation(data []byte, n int) ([]IntersectionChildResume,
     error)`, the exact inverse of `buildIntersectionContinuation` (reads
     `FirstContinuation`/`FirstStarted`, `SecondContinuation`/`SecondStarted`,
     `OtherChildState[]`). **`nil`/empty `data` → all-fresh** (each child START),
     distinct from a **hard error** when the proto is corrupt OR the child count
     ≠ `n` (mirrors Java `IntersectionCursorContinuation`'s
     `RecordCoreArgumentException`). Returns per-child `{Continuation []byte, Started bool}`.
   - Add resume-aware constructors `IntersectionResume` / `IntersectionMultiResume`
     that seed each child's `started` from the decoded flags.

2. **`executor.go` / `executor_new_plans.go`** — a shared helper builds the child
   cursors + per-child `started` seeds from the decode result:
   - `!Started` → `ExecutePlan(inner, nil)` (fresh), seed `started=false`,
   - `Started` && `len(Continuation) > 0` → `ExecutePlan(inner, Continuation)` (resume), seed `true`,
   - `Started` && empty → `recordlayer.Empty()` (exhausted; intersection is then
     immediately `END`, matching the semantics that any exhausted child ends the
     intersection), seed `true`.
   With no incoming continuation, all children start fresh (unchanged behavior).
   `executeMultiIntersection` drops its loud guard.

3. **`merge_cursor.go` (continuation-capture timing)** — a latent bug the paged
   resume test surfaced: both cursors captured the continuation *after* advancing
   all children past the matched key, so each child's saved position pointed one
   row too far and resume lost every other match (`[2,4,6]` → `[2,6]`). The
   matched result's own continuation already points to the row after the match,
   so the continuation is now captured **before** the post-match advance. Latent
   until now (resume was never wired), and the reason a resume fix needs its own
   regression test rather than trusting the existing producer.

## Performance

No steady-state change: the first-page path is identical (nil continuation → all
children fresh). On resume, children seek to their saved positions instead of
rescanning from the start — strictly *less* work than the (incorrect) full
restart. Decode is one proto unmarshal per page.

## Test plan

FDB integration tests (real cluster), both executors:
- **Regular intersection** (`SELECT ... WHERE a = x AND b = y` driving a
  two-index AND-intersection): page through with `ReturnedRowLimit = 1` (forces
  a resume after every row), assert the full result set equals the unpaged
  result with **no duplicates and no omissions** across continuation boundaries.
- **Multi-aggregate intersection** (`SELECT k, COUNT(*), SUM(v) ... GROUP BY k`):
  same paged-resume, assert per-group counts/sums match the single-pass result
  (this path previously errored on resume).
- Decode unit test: round-trip `buildIntersectionContinuation` →
  `decodeIntersectionContinuation` for 2-child and N-child (with exhausted /
  mid-stream / fresh mixes), plus a corrupt/short-proto error case.
