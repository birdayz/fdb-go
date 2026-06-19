# RFC-127 — SQL pagination must not treat a non-terminal StartContinuation as end-of-results

**Status:** Draft
**Item:** prod-readiness-audit-2026-06-19.md **P0 (release-blocking)** — SQL pagination can silently drop
results.
**Spec:** Java fdb-record-layer @ 4.11.1.0 — `RecordLayerIterator.java:84-97`, `RecordCursorResult.java`,
`RecordCursor.NoNextReason` (`RecordCursor.java:109-194`), `RecordCursorStartContinuation.java`,
`ContinuationImpl.java:161-187`.
**Reviewers:** Graefe (Cascades/execution alignment — the architecture question in §5 is for him) +
Torvalds (code quality). Query-engine change → Graefe ACK required before merge.

---

## 1. The bug (confirmed)

`paginatingRows.fetchPage` (`embedded/cascades_generator.go:1342-1356`) drains a query's result by
re-executing the plan from `r.continuation` page-by-page and buffering rows for `Next()`. After each
page it inspects the cursor continuation:

```go
cont := rs.GetContinuation()
if cont == nil || cont.IsEnd() {
    r.exhausted = true; r.continuation = nil
} else {
    contBytes, _ := cont.ToBytes()
    if contBytes == nil {
        r.exhausted = true        // ← BUG
    } else {
        r.continuation = contBytes
    }
}
```

`StartContinuation` (`recordlayer/cursor.go:88-101`) is **non-terminal** — `IsEnd()==false`,
`ToBytes()==nil` — and is returned when an operator hits a row/scan/time limit **before any resumable
position exists** (`cursor_combinators.go` RowLimitedCursor with no prior result;
`key_value_cursor.go:645-652` `limitContinuation` with `c.continuation==nil`). The `else`-branch
`contBytes==nil → r.exhausted=true` makes the internal drain **stop early and declare the query done**,
so the client receives a **silently truncated** result set with no continuation to resume. An operator
that consumes input before emitting its first row, then hits a limit before that first row, triggers it.

**Root cause:** Go infers exhaustion from `ToBytes()==nil`. That is wrong: a `StartContinuation`'s nil
bytes are byte-identical to an `EndContinuation`'s nil bytes — the two are distinguishable **only** via
`IsEnd()` (cursor.go contract; Java `RecordCursorContinuation.java:40-44` states the same: `isEnd()==true
⟹ toBytes()==null`, but the converse does **not** hold).

## 2. The Java spec — exhaustion is `SOURCE_EXHAUSTED`, never bytes

Java's relational iterator (`RecordLayerIterator.java:84-97`, `fetchNextResult`):

```java
result = recordCursor.getNext();
if (!result.hasNext()) {
    noNextReason = result.getNoNextReason();
    if (noNextReason == NoNextReason.SOURCE_EXHAUSTED) { continuation = ContinuationImpl.END; }
    else { continuation = ContinuationImpl.fromUnderlyingBytes(result.getContinuation().toBytes()); }
}
```

- **Iteration terminates** on `result.hasNext()==false` (value presence) — never bytes.
- **End-of-whole-result-set** = `NoNextReason == SOURCE_EXHAUSTED` (≡ `continuation.isEnd()`) — **never
  `toBytes()==null`/empty**.
- `NoNextReason` (`RecordCursor.java:109-194`): `SOURCE_EXHAUSTED` is the *only* exhausted reason;
  `RETURN_LIMIT_REACHED` is in-band but **not** exhausted; `SCAN/TIME/BYTE_LIMIT_REACHED` are out-of-band.
  `RecordCursorResult.withoutNextValue` (`:251-263`) enforces both ways: a non-`SOURCE_EXHAUSTED` reason
  *must* carry a non-end continuation, and `SOURCE_EXHAUSTED` *must* carry the end continuation. So the
  reason and `isEnd()` are equivalent signals — and bytes are never the exhaustion signal.
- For **any** limit reason, Java returns the partial page with a non-END continuation;
  `fromUnderlyingBytes(null)` (a START's bytes) → `ContinuationImpl.BEGIN` (resume from the beginning),
  **not** END, **not** an error (`ContinuationImpl.java:161-168`). Java's relational layer has **no
  no-progress guard** — it returns each page to the client and lets the client drive resumption.

So Go's exhaustion decision must be `IsEnd()` (≡ `SOURCE_EXHAUSTED`) **only**.

## 3. Why Go cannot mirror Java 1:1 here — the architecture difference (the load-bearing point)

Java's relational iterator returns **one page per client request**; a non-end START becomes a
`BEGIN` continuation handed back to the **client**, who may resume (Java accepts the resulting
restart-from-beginning, unguarded — it is the client's choice). **Go's `paginatingRows` drains the whole
query internally** — `fetchPage` re-executes the plan from `r.continuation` in a loop
(`cascades_generator.go:1121-1143`, kept fetching for blocking operators that emit 0 rows/page while
accumulating). Go therefore **cannot** adopt Java's "resume from BEGIN" for a non-end START: resuming
the internal drain from a nil/START position re-executes the plan from row 0 → **re-buffers the same
rows and never terminates** (an infinite loop in the fetch loop). Go's internal-drain architecture has
no client to hand the page to.

So within the existing architecture Go has exactly three options for a **non-end, no-resumable-bytes**
result: (a) treat as exhausted — the current bug (data loss); (b) resume from BEGIN — infinite loop /
duplicate rows; (c) surface the limit as a typed error. Only (c) is both data-loss-free and
loop-free.

## 4. Proposed Go change

### 4.1 Expose the authoritative signal

`RecordLayerResultSet` (`executor/resultset.go`) captures `lastContinuation` but not the reason. Capture
and expose it: `rs.lastNoNextReason = result.GetNoNextReason()` (alongside `:70`) + a
`GetNoNextReason() NoNextReason` accessor (the cursor result already has `GetNoNextReason()`).

### 4.2 Fix `paginatingRows.fetchPage` — exhaustion by `IsEnd()`, branch the un-resumable case by reason

```go
cont := rs.GetContinuation()
if cont == nil || cont.IsEnd() {            // SOURCE_EXHAUSTED — the ONLY exhaustion signal
    r.exhausted = true
    r.continuation = nil
} else {                                     // a limit was hit; the result set is NOT exhausted
    contBytes, contErr := cont.ToBytes()
    if contErr != nil { return contErr }
    if contBytes != nil {
        r.continuation = contBytes           // resumable position → keep draining
    } else {
        // Non-end StartContinuation: no resumable position. Go's internal drain cannot resume-from-BEGIN
        // like Java's client-driven iterator (it would re-buffer / loop). Branch by reason:
        switch rs.GetNoNextReason() {
        case recordlayer.ReturnLimitReached:
            // In-band row limit reached with zero rows produced ⟹ the row limit was 0 (LIMIT 0): no rows
            // were ever wanted. Done — no data is lost (there were 0 rows to lose).
            r.exhausted = true
            r.continuation = nil
        default: // ScanLimitReached / TimeLimitReached (out-of-band): a resource limit was hit before any
            // resumable progress. Surfacing it (54F01) avoids dropping the rest of the result set AND the
            // re-execute-from-BEGIN loop. Reuses the RFC-106a ScanLimitReachedError → 54F01 mapping.
            return &recordlayer.ScanLimitReachedError{Reason: rs.GetNoNextReason()}
        }
    }
}
```

`ReturnLimitReached`+START is only reachable when a row limit of 0 is set (`LIMIT 0`); the internal drain
otherwise uses a positive page limit, which yields a real `BytesContinuation` after the first row. The
out-of-band+START case is the genuine "hit a resource budget before making resumable progress" — already
the failure mode `FailOnScanLimitReached`/54F01 exists for (`connection.go:144`); here it is unconditional
because the internal drain *cannot* paginate past a no-progress stop regardless of that opt-in.

### 4.3 Cursor-contract assertion (audit recommendation)

Add a unit assertion that every continuation consumer checks `IsEnd()` before interpreting nil bytes
(the bug was a consumer that did not). Concretely: a focused test that a `StartContinuation` and an
`EndContinuation` (both nil bytes) are classified by `IsEnd()`, not bytes.

## 5. Open question for Graefe (architecture)

The §3 mismatch — Go's **internal-drain** `paginatingRows` vs Java's **client-driven** `RecordLayerIterator`
— is the crux. Two paths:
1. **Scoped fix (this RFC):** keep the internal-drain architecture; correct the exhaustion signal and
   surface the un-resumable out-of-band stop as 54F01. Minimal, release-unblocking, no data loss.
2. **Rearchitect to client-driven pagination** (return page+continuation to the client, mirroring Java
   exactly, including resume-from-BEGIN). Much larger; arguably the "Java is the reference" endgame, but
   out of scope for a P0 correctness fix.

Recommendation: (1) now (it removes the data-loss bug faithfully on the exhaustion axis), (2) as a tracked
follow-up if Graefe deems the internal-drain model itself a divergence to close.

## 6. Executable spec (regression tests)

1. **The bug repro (SQL):** a query whose plan consumes input before its first output row (e.g. a filter
   over a scan, or a blocking operator) under a scan/time limit small enough to stop **before the first
   row** → asserts the statement returns **either the full result on resume OR a 54F01** — never a
   silently truncated/short result. Revert-proven: with the old `contBytes==nil → exhausted` the test
   sees a short result.
2. **LIMIT 0:** returns exactly 0 rows, cleanly exhausted (no error) — confirms the `ReturnLimitReached`
   branch.
3. **Normal pagination unaffected:** a multi-page scan (real `BytesContinuation`) drains the full result
   set (regression guard that the change didn't alter the resumable path).
4. **Cursor-contract unit test** (§4.3): `IsEnd()`-not-bytes classification.

## 7. Wire-compat impact

**None** — no persisted bytes change; continuation token encoding is unchanged (the fix is in how the
*consumer* interprets `IsEnd()` vs nil bytes). Behavior-visible only in that a previously-silently-dropped
result is now either fully returned or a 54F01 (both strictly better than data loss).
