# RFC-139 — Typed-record build-range preset (Java 4.12, RFC-135 §4 R2c)

**Status:** Draft (v5 — v2 fixed the inclusive-high byte bound to `+0xff` (not strinc) after Torvalds NAK;
v3 makes the build loop *consume* that `+0xff` boundary after codex's P1; v4 gives up for non-integer
record-type keys; v5 normalizes int32 keys to int64 before packing — both after codex P2 rounds)
**Item:** RFC-135 §4 **R2 part (c)** — port Java 4.12's typed-records build-range preset optimization
(commit `4855124c9` #4244).
**Reviewers:** Torvalds + codex + @claude. Record-layer (online-indexer build optimization), **no Graefe
gate**.

---

## 1. Problem (verified real)

When an online build targets only a **subset** of the metadata's record types (via `SetRecordTypes`),
the records of those types live in a contiguous sub-range of the records space — keyed by their
record-type-key prefix. Java 4.12 preemptively marks the key ranges **outside** that sub-range as
already-indexed in each target index's `IndexingRangeSet`, so the build skips scanning keys that can't
hold any indexed-type record. Go has no equivalent (`computeRecordsRange` / `maybePresetRecordsRange`
are missing) — a real 4.12 optimization gap (correctness is unaffected; it's a build-speed win for
subset-typed builds).

## 2. Investigation (Java spec ↔ Go infra)

**Java `IndexingCommon.computeRecordsRange()`** (`:212-230`): over `getAllRecordTypes()` (the indexed
types), if **any** type `!primaryKeyHasRecordTypePrefix()` or `isSynthetic()` → return `null` (give up;
the whole records space may be relevant). Else accumulate the min (`low`) and max (`high`)
`getRecordTypeKeyTuple()` and return `TupleRange.betweenInclusive(low, high)`. `null` also when there are
no types.

**Java `IndexingBase.maybePresetRecordsRangeAsync()`** (`:596-613`): if the range is `null`, done. Else,
in one `buildCommitRetryAsync` transaction, insert into every target `IndexingRangeSet`, **sequentially**,
the two out-of-range gaps `[null, rangeStart)` then `[rangeEnd, null)` with `requireEmpty=true` (where
`rangeStart/rangeEnd` are the packed bounds of `tupleRange.toRange()`). Called **after markWriteOnly,
before the build loop** (`IndexingMultiTargetByRecords:120`, `IndexingMutuallyByRecords:283`).

**Go infra** (Explore-mapped): `IndexingRangeSet.InsertRange(tr, begin, end, requireEmpty)` already
exists (`indexing_range_set.go:42`), `[]byte` bounds. `OnlineIndexer.recordTypes []string` +
`SetRecordTypes` exist. `RecordType.GetRecordTypeKey() any` exists (wrap as `tuple.Tuple{key}.Pack()`).
**Two per-type predicates are MISSING:** `RecordType.PrimaryKeyHasRecordTypePrefix()` (only a
metadata-level `RecordMetaData.PrimaryKeyHasRecordTypePrefix()` exists; the helper
`primaryKeyStartsWithRecordType(keyExpr)` at `metadata.go:1135` is reusable per-type) and
`RecordType.IsSynthetic()` (Go has no synthetic record types — out of scope — so it is always `false`).

## 3. Fix

1. **`RecordType.PrimaryKeyHasRecordTypePrefix() bool`** — true iff the type's `PrimaryKey` key expression
   starts with a record-type-key component, reusing the existing `primaryKeyStartsWithRecordType` helper.
2. **`RecordType.IsSynthetic() bool`** — returns `false` with a comment: Go does not model synthetic
   record types (out of scope per CLAUDE.md). Kept as a method for 1:1 fidelity with Java's predicate and
   so the algorithm reads identically.
3. **`OnlineIndexer.computeRecordsRange() (begin, end []byte, ok bool)`** — the Go port of Java's method
   over the indexed record types (the ones named by `recordTypes`, or all if empty). Returns `ok=false`
   (whole space) when any type lacks the prefix / is synthetic / there are no types; else the packed
   `betweenInclusive(low, high).toRange()` bounds, matching Java `TupleRange.toRange`
   (`TupleRange.java:471-530`) **byte-for-byte** — wire compat is the hard line:
   - `begin = low.Pack()` **verbatim** (`RANGE_INCLUSIVE` low is a no-op in `toRange:479`; **not** strinc'd).
   - `end = append(high.Pack(), 0xff)` — `RANGE_INCLUSIVE` high appends a single `0xff` byte
     (`toRange:502-508`), **not** `strinc`. (`0xff` is the tuple escape-suffix; strings/bytes suffixed
     past the high tuple sort after it and are correctly excluded. `strinc(0x16 0x07) = 0x16 0x08` would
     write *different* range-set bytes than Java's `0x16 0x07 0xff` — a wire divergence even though both
     happen to cover the same real PKs for integer record-type keys.)
   - where `low`/`high` are the min/max `tuple.Tuple{recordType.GetRecordTypeKey()}`.
   - **Gives up for non-integer record-type keys, and normalizes integer keys to int64 before
     packing** (v4/v5, after codex P2). Go's `RecordTypeKeyExpression` only binds integer keys — **as
     int64** (`metadata.go:743-753`) — and silently encodes a string/bytes explicit `SetRecordTypeKey` as
     the message **type name** at save time (`key_expression.go` `Evaluate`). So (a) bounds from a
     non-integer key would not match where records live, and the preset could mark the real records built
     and skip them — incomplete index marked READABLE; and (b) the tuple encoder **panics** on a raw
     `int32` (it encodes only `int`/`int64`, `tuple.go:373-375`). `computeRecordsRange` therefore returns
     `ok=false` for any non-integer key, and normalizes `int`/`int32`/`int64` to `int64` before packing —
     matching `bindTypeKeys` exactly, so the bounds match record placement and the `int32` panic is
     avoided. (Java encodes every key type; the Go `RecordTypeKeyExpression` int-only limitation is a
     separate, pre-existing wire gap, tracked in TODO — not fixed here.)
4. **`OnlineIndexer.maybePresetRecordsRange(ctx) error`** — if `!ok`, no-op. Else, in one transaction,
   for each target index's `IndexingRangeSet`, insert `[nil, begin)` then `[end, nil)` with
   `requireEmpty=true`, sequentially (ordered mutations within the txn, per Java).
5. **Call it in `BuildIndex`** (and `buildIndexMutual`) after `markWriteOnly`, before the build loop.
6. **Make the build loop consume the `+0xff` end boundary** (v3, after codex P1). The preset makes the
   first missing range `[low.Pack(), high.Pack()+0xff)`. Go's `buildRange`/mutual scan previously did
   `fastUnpack(missing.End)` to build a `TupleRange` — and `high.Pack()+0xff` is **not** a valid tuple, so
   typed multi-target/mutual builds failed with `unpack range end`. Two changes (Java scans raw bytes; Go
   unpacks to tuples, so Go must bridge):
   - `unpackRangeEndBoundary([]byte) (tuple, EndpointType)` — a normal boundary unpacks as a tuple
     (EXCLUSIVE high); a `tuple+0xff` boundary (not a valid tuple) is the stripped tuple with an
     **INCLUSIVE** high endpoint, covering that tuple's whole sub-range exactly as the `0xff` does. The
     plain-tuple unpack is tried first, so an ordinary key whose pack ends in `0xff` (e.g. int 255 =
     `0x15 0xff`) is unaffected. Used by `buildRange` and the mutual fragment scan.
   - The **mark-built** step records progress using the *original* byte boundary `missing.End` (not
     `rangeEnd.Pack()`): the stripped pack drops the `0xff` and would mark an **inverted** range
     (`begin > end`) once records past the stripped tuple have been scanned. For a normal build
     `missing.End == rangeEnd.Pack()`, so this is a no-op there. (`buildRangeByIndex` — single-target
     BY_INDEX — never presets, so its scan path is untouched.)

## 4. Performance

Pure win: for subset-typed builds it marks the out-of-range gaps built up-front (one extra small
transaction), so the scan skips them. For all-types builds (`computeRecordsRange` → not-ok) it is a
no-op. No read/write hot-path effect.

## 5. Wire / behaviour impact

The preset writes to the **range set** (build-progress tracking), exactly as a normal build would as it
completes those ranges — so the post-build range-set state is identical to today; only the *scan* is
shorter. No record/index-entry persisted bytes change. `requireEmpty=true` keeps it safe on a resumed
build (it won't clobber already-recorded progress — it errors instead, matching Java).

## 6. Test plan

- Unit: `computeRecordsRange` returns not-ok when no `recordTypes` filter (all types) or a type lacks the
  prefix; returns the right bounds for a 2-type subset (and a single type). `IsSynthetic` false;
  `PrimaryKeyHasRecordTypePrefix` true/false on PK expressions with/without a record-type prefix.
- **Byte-exact bound test (pins the `+0xff` vs strinc divergence — REQUIRED).** The integration test
  ("highest type's records still indexed") passes under *both* the correct `+0xff` and a wrong `strinc`
  because they cover the same real PKs for integer keys, so it does NOT pin the boundary. Add a direct
  assertion that `end == append(tuple.Tuple{highKey}.Pack(), 0xff)` and `begin == tuple.Tuple{lowKey}.Pack()`
  (NOT strinc'd) — the byte the wire actually depends on. A `strinc` impl fails this.
- **FDB integration:** a store with ≥2 record-type-prefixed types; a multi-target build asserts the
  range set shows the out-of-range gaps as **built** after the preset (only the indexed type's range
  stays missing) — revert-proof vs no-preset. Confirm an all-types build is unaffected (no preset).
- **Full-build integration (REQUIRED — pins codex P1).** A typed multi-target build with a small
  `SetLimit` (multiple chunks across the preset range) must **complete** (no `unpack range end` / no
  inverted-range error) and index **all** records of the type, **including the highest** (which the
  `+0xff` inclusive-high bound must cover). The earlier preset-only test missed this because it never ran
  the build loop over the `[low, high]+0xff` range.
- **Unit (`unpackRangeEndBoundary`):** a normal tuple → EXCLUSIVE; a `tuple+0xff` → INCLUSIVE of the
  stripped tuple; int 255 (`0x15 0xff`, a valid tuple ending in `0xff`) → EXCLUSIVE of that key, not
  mistaken for a `+0xff` bound.
- **Unit (non-integer key give-up):** a type with a string explicit `SetRecordTypeKey` →
  `computeRecordsRange` returns `ok=false` (no preset), so the build scans everything and indexes all
  records (pins codex P2).
- **Unit (int32 normalization):** a type with an `int32` explicit `SetRecordTypeKey` → `ok=true` with
  bounds byte-equal to the `int64`-normalized pack (no tuple-encoder panic; matches `bindTypeKeys`).

## 7. Scope

One commit on the RFC-135 branch (PR #336): the two `RecordType` predicates + `computeRecordsRange` +
`maybePresetRecordsRange` + the call site + tests. R3–R8 remain separate.
