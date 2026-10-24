# RFC-270: Tuple-range index probes and SUM removal of MinInt64

**Status:** Draft
**Item:** TODO.md "Quality review 2026-10-24", Record layer: the unique-index false
violation, the RANK orphaned score and the SUM `math.MinInt64` removal (High, High, Medium),
plus the same range bug at the other sites it reaches.
**Spec:** Java Record Layer 4.14.2.0 (`fdb-record-layer/`, tag `4.14.2.0`).

## Problem

Several record-layer probes and clears read `fdb.PrefixRange(sub.Pack(t))`, which is every
key that starts with the bytes of `t`: `[pack, strinc(pack))`. Java reads
`sub.range(t)` (or `sub.subspace(t).range()`), which is `[pack+00, pack+ff)`. The two
differ when the last element of `t` is bytes, a string or a nested tuple. A packing ends in
the terminator `00`, and a value with a NUL at that position continues with `ff`: `b"\x01"`
packs to `01 01 00`, which is a byte prefix of `b"\x01\x00\x05"` (`01 01 00 ff 05 00`), and
`"a"` is likewise a prefix of `"a\x00b"`. The prefix range therefore also covers every key of
the longer value.

The effects:

1. **Unique index** (`checkUniqueness`, `index_maintainer.go`). Saving `b"\x01"` after
   `b"\x01\x00\x05"` is refused with a uniqueness violation. While the index is WRITE_ONLY,
   Go instead persists violation entries for both records, and Java would not write them.
2. **RANK** (`updateRankedSet`, `rank_index_maintainer.go`), without duplicate counting.
   Removing score `X` is skipped while a record holds `X ++ \x00…`. The stale score stays in
   the ranked set, so every rank above it is off by one.
3. **TIME_WINDOW_LEADERBOARD** (`updateRankedSet`, `time_window_leaderboard_maintainer.go`).
   This is the same probe as item 2, written separately.
4. **`ScanUniquenessViolationsForValue`** (`store_api.go`). The scan also returns the
   violations of extending values. `ResolveUniquenessViolationByDeletion` then deletes
   records that belong to a different value.
5. **`DeleteRecordsWhere`** (`store_delete_where.go`). The records clear, the legacy
   version clear (`RecordVersionKey`, format < 6), the removal of pending version mutations,
   and the record-count clear (prefix shorter than the count key) also remove the data of an
   extending prefix: `("a")` deletes the records of `("a\x00b")`.
6. **PERMUTED_MIN/MAX `DeleteWhere`** (`permuted_min_max_index_maintainer.go`). The permuted
   (secondary) entries of an extending group are cleared.
7. **`DeleteIndexEntriesInRange`** (`store.go`, a Go-only API). Its doc says it clears the
   entries whose leading value is the prefix. It also clears `"alice\x00…"`.

A separate defect: **SUM cannot remove `math.MinInt64`.** `sumMutation.applyMutation`
(`atomic_mutation.go`) returns an overflow error. Java adds `-lval`
(`AtomicMutation.Standard.getMutationParam`, `AtomicMutation.java:181-188`). For
`Long.MIN_VALUE` that negation wraps to itself, and FDB's ADD is modulo 2^64, so adding
`MinInt64` a second time undoes the first addition exactly. Go refuses every update and
delete of such a record, while Java stores the same record and maintains it. The test that
added the guard (`byte_limit_bug_test.go`) only asserts Go arithmetic.

## Investigation

Java ranges, by site:

| Go site | Java | Java range |
|---|---|---|
| `checkUniqueness` | `StandardIndexMaintainer.java:490` | `indexSubspace.range(valueKey)` |
| RANK, leaderboard `updateRankedSet` | `RankedSetIndexHelper.java:264` (both call it; leaderboard at `TimeWindowLeaderboardIndexMaintainer.java:396`) | `indexSubspace.range(valueKey)` |
| `ScanUniquenessViolationsForValue` | `FDBRecordStoreBase.java:1473`, `FDBRecordStore.java:1700` | `TupleRange.allOf(valueKey)` = `[pack, pack+ff)` |
| `DeleteRecordsWhere` records, legacy versions, counts | `FDBRecordStore.java:2292,2295,2309` | `subspace(prefix).range()`; `context.clear` drops version mutations over the same range |
| PERMUTED `DeleteWhere` secondary | `PermutedMinMaxIndexMaintainer.java:363` | `permutedSubspace.subspace(prefix).range()` |

The survey also covered every other `PrefixRange`, `Strinc` and `startsWith`-style range in
`pkg/recordlayer` (subpackages included). These ones already match Java, which uses
`[key, strinc(key))` or `Range.startsWith` too, so they stay as they are:

- `StandardIndexMaintainer.deleteWhere` (`:783`) and the RANK, leaderboard, sliding-window,
  TEXT, vector and multidimensional `DeleteWhere` paths.
- `removeUniquenessViolations` (`StandardIndexMaintainer.java:556`).
- the record-exists check (`SplitHelper.java:548`).
- clearing index data and former-index data.
- the pending-writes queues and the HNSW storage adapters.

The GuardiANN and SPFresh keys (Go-only) always end in an int64, a UUID or a constant, so
they cannot overlap.

Two of the matched Java ranges reach an extending prefix themselves, and Go keeps that
behavior so the bytes of a shared store agree:

- `deleteRecordsWhere(("a"))` clears the standard index entries of `("a\x00b")` but keeps its
  records.
- removing the second-to-last violation of `"a"` clears the violations of `"a\x00b"`.

## Fix

- Items 1-6 use the tuple range Java uses. A `subspace.Subspace` is an `fdb.ExactRange` over
  `[p+00, p+ff)`, so each site reads or clears `sub.Sub(t...)`. In `DeleteRecordsWhere` the
  version-mutation removal takes the range that was actually cleared:
  - the tuple range for records and legacy versions;
  - `[key, strinc(key))` for index subspaces, the range the maintainer cleared, which is
    unchanged.

  `clearPrefixRange` goes away, and `removeVersionDataInPrefixRange` becomes
  `removeVersionDataInRange(fdb.ExactRange)`.
- Item 7 clears `[pack, pack+ff)`. That range is the tuple range plus the key at `pack`
  itself, where a grouped aggregate stores its value.
- SUM drops the guard and adds the wrapped negation, as Java does.

## Performance

None of these changes adds a read or a round trip. Each site still packs one tuple and
issues one range read or clear; only the end key changes. The SUM change removes a branch.

## Test plan

Real-FDB integration specs (`pkg/recordlayer/tuple_range_probe_test.go`, fast lane), with
bytes and strings where one packing extends another:

- **Unique (READABLE).** `b"\x01"` and `"a"` save after the longer values. A true duplicate on
  either index is still refused.
- **Unique (WRITE_ONLY).** No violation entries are written for the extending value. A true
  duplicate still records both sides.
- **`ScanUniquenessViolationsForValue`.** It returns only the value's own violations.
  `ResolveUniquenessViolationByDeletion` keeps the records of the extending value.
- **RANK** (string and bytes) and **TIME_WINDOW_LEADERBOARD.** After the shorter score's
  record is deleted, the score is gone and the remaining record is at rank 0.
- **SUM.** A record holding `math.MinInt64` is updated, re-saved and deleted, and the sum is
  checked at each step.
- **`DeleteRecordsWhere`** at the default format and at format 5 (legacy versions). The
  extending prefix keeps its records, its versions (including versions still pending in the
  clearing transaction), its counts and its permuted entries.
- **`DeleteIndexEntriesInRange`.** The extending value's entry is kept. A grouped COUNT key
  stored at the prefix itself is cleared.

Cross-engine (`conformance/tuple_range_probe_conformance_test.go`, `conformance_java`): Java
and Go save the same records under unique, RANK and SUM indexes, including a RANK removal of
`"a"` and a SUM removal of `MinInt64`. The verdicts and the index key-value pairs must be
equal.

Each spec is checked red against the code before its fix, then green.
