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

**Per-child cached continuation (Java `MergeCursorState`).** The proto's per-child
`(continuation, started)` is the projection of Java `MergeCursorState.continuation` —
a cached resume point, NOT a value derived from the cursor's current result. The
original Go producer derived it from `child.result.GetContinuation()` (the position
*after* the child's current row) and `cursor.started`, which is wrong in two ways:
1. A value that is merely **loaded but not yet matched** (held during the merge) would
   be encoded one row too far, so an out-of-band stop (a child hits a scan/time limit
   mid-merge) loses the held match on resume.
2. A child that hits a limit **before its first row** returns a `StartContinuation`
   (empty bytes, not end) — indistinguishable from an exhausted child's empty-end
   continuation under the naive encoding, so it would decode as END and silently
   terminate the whole intersection.

We port Java's model: `mergeChildState` carries a `continuation` initialized to the
child's start (or resume) point, updated to `result.GetContinuation()` **only** when
the child yields no value (`advance`: limit/exhausted) or when its value is **consumed**
(`consume()`: a merge result was emitted) — never for a merely-held value.
`buildIntersectionContinuation` encodes from this cached continuation, deriving
`started=false` for an empty-but-not-end (START) continuation and `started=true` for
end (END) or non-empty (MID) — exactly Java `MergeCursorContinuation`. On resume the
seeds come from `DecodeIntersectionContinuation` (START → `StartContinuation`, MID →
the bytes, END → `EndContinuation`). This is correct regardless of checkpoint timing,
preserves held matches across out-of-band stops, and keeps START distinct from END.

## Fix

1. **`merge_cursor.go`** —
   - `mergeChildState` gains a `continuation RecordCursorContinuation` (Java
     `MergeCursorState.continuation`): `advance()` updates it only on a no-value
     result; a new `consume()` updates it to the matched result's continuation.
     The match path calls `consume()` on all children, then captures the
     continuation **before** the in-memory advance (so it points to the row after
     the match — building it after the advance lost every other match,
     `[2,4,6]`→`[2,6]`, which the paged test caught).
   - `buildIntersectionContinuation` encodes from `child.continuation`: empty+end
     → END (`started=true`), non-empty → MID (`started=true`), empty+not-end →
     START (`started=false`) — exactly Java `MergeCursorContinuation`.
   - Add `DecodeIntersectionContinuation(data []byte, n int) ([]IntersectionChildResume,
     error)`, the exact inverse. **`nil`/empty `data` → all-fresh** (each child
     START), distinct from a **hard error** when the proto is corrupt OR the
     child count ≠ `n` (mirrors Java `IntersectionCursorContinuation`'s
     `RecordCoreArgumentException`). Returns per-child `{Continuation []byte, Started bool}`.
   - Add resume-aware constructors `IntersectionResume` / `IntersectionMultiResume`
     that seed each child's cached `continuation` from the decode (START →
     `StartContinuation`, MID → its bytes, END → `EndContinuation`).

2. **`executor.go` / `executor_new_plans.go`** — a shared helper builds the child
   cursors from the decode result and passes the resume states to the constructor:
   - `!Started` → `ExecutePlan(inner, nil)` (fresh),
   - `Started` && `len(Continuation) > 0` → `ExecutePlan(inner, Continuation)` (resume),
   - `Started` && empty → `recordlayer.Empty()` (exhausted; intersection is then
     immediately `END`, matching the semantics that any exhausted child ends the
     intersection).
   With no incoming continuation, all children start fresh (unchanged behavior).
   `executeMultiIntersection` drops its loud guard.

## Performance

No steady-state change: the first-page path is identical (nil continuation → all
children fresh). On resume, children seek to their saved positions instead of
rescanning from the start — strictly *less* work than the (incorrect) full
restart. Decode is one proto unmarshal per page.

## Test plan

White-box deterministic tests (`intersection_resume_test.go`). The SQL path
can't drive this: its pagination is time-based (`txPageTimeLimit`), so it can't
force a per-row continuation boundary in a fast/deterministic test. A
continuation-resumable in-memory cursor (`sliceResumeCursor`) does, and the
paging loop mirrors `buildIntersectionChildCursors` exactly (decode → rebuild
each child from its per-child continuation → `IntersectionResume`):
- **Paged resume, both cursors** (`IntersectionResume` + `IntersectionMultiResume`):
  one row per page, assert the full result equals the unpaged intersection with
  **no duplicates and no omissions** — cases: common, all-match, no-common,
  asymmetric exhaustion (one child END), 3-child. This is what surfaced the
  continuation-capture-timing bug (`[2,4,6]`→`[2,6]`).
- **Limit-before-first-row, no loss** (the case Graefe flagged): child B hits a
  scan limit before its first row while child A holds a match; on resume that
  held match is NOT lost (`[2,4,6]` intact) — proves the consume-based cached
  continuation captures the position *before* a held value, and that a
  `StartContinuation` child round-trips as START (re-read), not END (silent drop).
- **Decode**: round-trip (MID/END/START mix) asserting an exhausted child
  round-trips as END (not START); `nil` → all-fresh; corrupt proto and
  child-count mismatch → hard error.

The existing `TestFDB_MultiAggregateIntersection_Filtered` (real FDB) continues
to pin the non-resume multi-aggregate path end-to-end.
