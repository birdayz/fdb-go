# RFC-127 — SQL pagination must not treat a non-terminal StartContinuation as end-of-results

**Status:** Accepted
**Item:** prod-readiness-audit-2026-06-19.md **P0** — SQL pagination can silently drop results.

> **Severity correction (post-implementation, from the `/code-review` reachability trace).** The audit
> rated this release-blocking, but the data-loss path is **latent, not live** in the current code. Every
> Go leaf cursor reports an out-of-band stop only after `scanned>0` (`key_value_cursor.go:164/174/181`,
> `record_key_cursor.go:64/69/78`), so its continuation is set → a `BytesContinuation` (which the OLD code
> resumed correctly); composite cursors carry a serialized `BytesContinuation` (merge/intersection) or
> error-first with 54F01 (`mergeSort`, RFC-106a). So **no current cursor emits a no-next
> out-of-band+StartContinuation**, and the only reachable nil-bytes+non-end case is `LIMIT 0`
> (`ReturnLimitReached`), which the old `nil→exhausted` happened to handle correctly. Even the audit's
> described trigger (filter + scan limit before the first row) produces a *BytesContinuation* (the leaf
> scanned rows), not a START. **The fix is still correct and worth landing**: the old logic was wrong *in
> principle* (it inferred exhaustion from bytes, violating Java's `SOURCE_EXHAUSTED`-only invariant) — a
> latent landmine the moment any future Go cursor emits the out-of-band+START state Java's
> Union/Intersection/MapWhile cursors legitimately produce. It is a correctness/invariant hardening + makes
> the `LIMIT 0` handling explicit, not an emergency data-loss patch.

> **Reviews.** Graefe **ACK** (verified vs Java 4.11.1.0): exhaustion off `IsEnd()`/`SOURCE_EXHAUSTED`,
> never bytes, is correct (`RecordLayerIterator.java:91`; the `withoutNextValue` invariant makes reason ⇔
> `isEnd()`); exposing `NoNextReason` as a first-class signal is right (Java carries it precisely because a
> START's nil bytes are ambiguous; `terminatedEarly()` reads the reason, not bytes). The §3 architecture
> mismatch is **real** and the out-of-band→54F01 adaptation is **faithful, not a masked cursor bug** — a
> non-end START with an out-of-band reason is a *legitimate* Java state (emitted by Union/Intersection/
> MapWhile/MemorySort cursors); Java survives it only because its iterator is client-driven (hands out
> `BEGIN`), which Go's internal drain has no analog for. Scoped fix NOW; rearchitect-to-client-driven is a
> tracked follow-up, not a P0. `ReturnLimitReached`→exhausted (LIMIT 0) is faithful (Java treats it
> in-band, not terminatedEarly). Torvalds **ACK** (scoped): root cause + infinite-loop claim verified;
> 54F01 is honest (exact precedent: `errIfDrainTruncated`, `cursor_util.go:17-22`). Both required fixes
> folded below: (1) drive the un-resumable branch off `IsOutOfBand()` (reuse the `errIfDrainTruncated`
> classifier), no bare `default`; (2) prove the LIMIT-0/`ReturnLimitReached` claim with continuation-type +
> LIMIT-N(>0) tests. Graefe clarifications folded: the invariant is "**a non-end continuation with nil
> bytes**" (the KVCursor *leaf* special-cases `lastKey==null ⟹ isEnd`, `KeyValueCursorBase.java:176`;
> composite cursors produce the non-end START), and §6 test 1 pins which branch each operator shape takes.
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
result: (a) treat as exhausted — the old logic (latent data loss — see the severity correction: no current
cursor reaches it, but it is wrong in principle); (b) resume from BEGIN — infinite loop / duplicate rows;
(c) surface the limit as a typed error. Only (c) is both data-loss-free and loop-free.

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
        // A non-end continuation with NO resumable bytes (e.g. StartContinuation). Go's internal drain
        // cannot resume-from-BEGIN like Java's client-driven iterator (it would re-buffer / loop). This is
        // exactly the value-only-drain situation `errIfDrainTruncated` (cursor_util.go:17-22) already
        // handles — classify by `IsOutOfBand()`, do NOT re-list reasons or use a bare default:
        reason := rs.GetNoNextReason()
        if reason.IsOutOfBand() {            // SCAN / TIME / BYTE limit before any resumable progress
            // Surfacing 54F01 avoids dropping the rest of the result set AND the re-execute-from-BEGIN
            // loop. Same ScanLimitReachedError → 54F01 path the value-only drains use.
            return &recordlayer.ScanLimitReachedError{Reason: reason}
        }
        // In-band (ReturnLimitReached) with zero rows ⟹ the row limit was 0 (LIMIT 0): no rows were ever
        // wanted, so this is a clean done — no data is lost (Java treats RETURN_LIMIT_REACHED as in-band /
        // not terminatedEarly). SourceExhausted+nil-bytes is impossible here (it has isEnd()==true, caught
        // by the first branch; the withoutNextValue invariant guarantees it).
        r.exhausted = true
        r.continuation = nil
    }
}
```

`ReturnLimitReached`+nil-bytes is only reachable when a row limit of 0 is set (`LIMIT 0`); the internal
drain otherwise uses a positive page limit, which yields a real `BytesContinuation` after the first row —
§6 tests 2+3 pin this (the load-bearing claim Torvalds flagged). The out-of-band case is the genuine "hit
a resource budget before making resumable progress"; here the 54F01 is unconditional (not gated on
`FailOnScanLimitReached`) because the internal drain *cannot* paginate past a no-progress stop regardless.

### 4.3 Cursor-contract assertion (audit recommendation)

Add a unit assertion pinning the invariant **"a non-end continuation may have nil bytes"** — i.e. nil
bytes is NOT an exhaustion signal; `IsEnd()` is. (Note the asymmetry Graefe flagged: the KVCursor *leaf*
special-cases `lastKey==null ⟹ isEnd==true` (`KeyValueCursorBase.java:176`), so a leaf scan's nil-bytes
continuation IS end; the non-end-with-nil-bytes case is produced by *composite* cursors — RowLimited,
Union, Sort, MapWhile — stopping on a limit before progress.) The test classifies a `StartContinuation`
(non-end, nil bytes) and an `EndContinuation` (end, nil bytes) by `IsEnd()`, never bytes.

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

1. **The out-of-band branch (unit, not SQL):** the out-of-band+StartContinuation state is **not reachable
   via SQL today** (see the severity correction — every leaf guards out-of-band with `scanned>0` →
   `BytesContinuation`; composites carry bytes or error-first). So the proof is the deterministic unit test
   `TestPageContinuationState`, which constructs a non-end `StartContinuation` paired with
   `ScanLimitReached`/`TimeLimitReached` and asserts the 54F01, plus the in-band/exhausted/resumable rows of
   the decision table. **Revert-proven:** restoring the old `contBytes==nil → exhausted` reds the
   `start_+_scan_limit` / `start_+_time_limit` cases (confirmed). A SQL-level repro is impossible without a
   cursor that emits the state (a future Union/MapWhile port would; the test guards that day).
2. **LIMIT 0:** returns exactly 0 rows, cleanly exhausted (no error). **Assert the continuation type** at
   the page boundary is a non-end nil-bytes continuation with `ReturnLimitReached` (the load-bearing
   precondition Torvalds flagged), so the test fails if the plan ever produced a different shape.
3. **LIMIT N (N>0), incl. a blocking operator (e.g. ORDER BY / aggregate) page boundary:** asserts the
   page never lands `ReturnLimitReached`+nil-bytes (the first row yields a real `BytesContinuation`), and
   the full N-row result is returned — proving the `ReturnLimitReached`→exhausted branch can never eat
   real rows.
4. **Normal pagination unaffected:** a multi-page scan (real `BytesContinuation`) drains the full result
   set (regression guard that the change didn't alter the resumable path).
5. **Cursor-contract unit test** (§4.3): non-end-with-nil-bytes is classified by `IsEnd()`, not bytes.

## 7. Wire-compat impact

**None** — no persisted bytes change; continuation token encoding is unchanged (the fix is in how the
*consumer* interprets `IsEnd()` vs nil bytes). Behavior-visible only in that a previously-silently-dropped
result is now either fully returned or a 54F01 (both strictly better than data loss).
