# RFC-268: Explicit read-conflict-range errors are deferred

**Status:** Implemented
**Item:** TODO.md §0, "Explicit read-conflict-range errors are immediate or missing."
**Spec:** FoundationDB 7.3.77 (`/tmp/fdbsrc`); Go line numbers at `54566f2a4`.

## Problem

Two divergences in the explicit read-conflict API, the read half of RFC-267:

1. `Transaction.AddReadConflictRange` (`pkg/fdbgo/client/transaction_state_entry.go:389`,
   `stateAddReadConflictRange` at `transaction.go:3171`) returns `key_outside_legal_range`
   (2004) from the call when an endpoint is past `maxReadKey` and the range is not exactly
   the metadataVersionKey range. The facade returns it to the application
   (`pkg/fdbgo/fdb/transaction.go:369-372`) and records nothing. libfdb_c returns success
   from the call and fails the transaction's next operation instead, so a caller that
   discards the return (`pkg/recordlayer/bunched_map.go:383`) commits under Go where
   libfdb_c fails the commit with 2004.
2. `Transaction.AddReadConflictKey` (`transaction_state_entry.go:399`,
   `stateAddReadConflictKey` at `transaction.go:3216`) runs no legal-range check: a system
   key without `READ_SYSTEM_KEYS` is recorded as a read conflict and the commit succeeds.
   It also skips the size clamp, so an oversized key ships its full bytes as a conflict
   range where libfdb_c records none. The Apple binding's `AddReadConflictKey` is
   `addConflictRange(KeyRange{key, key+"\x00"}, read)`
   (`bindings/go/src/fdb/transaction.go:548-554`): the same C function as the range form.

## C++ behavior

1. `fdb_transaction_add_conflict_range` (`bindings/c/fdb_c.cpp:1216-1227`) builds a
   `KeyRangeRef` on the calling thread; its constructor throws `inverted_range` (2005)
   for `begin > end` (`fdbclient/include/fdbclient/FDBTypes.h:318-323`), returned from
   the call. This is the only synchronous error.
2. `ThreadSafeTransaction::addReadConflictRange` (`fdbclient/ThreadSafeTransaction.cpp:589-594`)
   enqueues RYW `addReadConflictRange` with
   `onMainThreadVoid(..., tr, &ISingleThreadTransaction::deferredError)`.
3. `doOnMainThreadVoid` (`flow/include/flow/ThreadHelper.actor.h:50-61`) skips the functor
   when `deferredError` is already set; otherwise a throw is stored in `deferredError`.
4. RYW `addReadConflictRange` (`fdbclient/ReadYourWrites.actor.cpp:1945-1987`):
   - `:1950-1955`: throw `key_outside_legal_range` when either endpoint exceeds
     `getMaxReadKey()` (`\xff`, or `\xff\xff` under `READ_SYSTEM_KEYS`, `:2757-2762`),
     unless `begin == metadataVersionKey && end == metadataVersionKeyEnd`
     (`"\xff/metadataVersion"`, `"\xff/metadataVersion\x00"`, `SystemData.cpp:1385-1386`);
   - `:1957-1975`: clamp each endpoint to `getMaxReadKeySize + 1` bytes and return
     without a conflict when the clamped range is empty;
   - `:1977-1986`: add the range directly when RYW is disabled, else through
     `updateConflictMap`.
5. Every later future-returning operation re-throws the deferred error through
   `checkDeferredError` (for example `commit`, `ThreadSafeTransaction.cpp:666-672`); it
   clears only on reset.

For the key form `[k, k\x00)` this means: `k` past `maxReadKey` and `k == maxReadKey`
(its end `k\x00` is past it) both defer 2004; `k == metadataVersionKey` is the exempt
range; and a `k` longer than `getMaxReadKeySize(k)` clamps both endpoints to the same
`max+1` bytes, so nothing is recorded. A `k` within the limit is unchanged by the clamp.

## Fix

Mirror RFC-267's write-side split in `pkg/fdbgo/client/transaction.go`:

- `stateAddReadConflictRange(begin, end) error`: return 2005 for `begin > end` (step 1,
  regardless of a deferred error); otherwise, unless an error is deferred (step 3), run
  `addReadConflictRangeRYW` and record a 2004 it returns; return `nil`.
- `stateAddReadConflictKey(key)`: the same deferral around `addReadConflictKeyRYW`.
- `addReadConflictRangeRYW(begin, end)` is step 4 (today's range body after the inverted
  check): the legal-range check, the clamp, the RYW-disabled or filtered insert.
- `addReadConflictKeyRYW(key)` is step 4 on `[key, keyAfter(key))` without allocating
  `keyAfter(key)`: the end is past `maxReadKey` iff `key >= maxReadKey`; the range is the
  metadataVersionKey range iff `key == metadataVersionKey`; the clamp drops it iff
  `len(key) > getMaxClearKeySize(key)` (`getMaxReadKeySize == getMaxKeySize ==
  getMaxClearKeySize`, as the range form already assumes) and leaves it unchanged
  otherwise. The insert is then today's single-key `addReadConflictForKeyRYW`, which is
  `updateConflictMap` on `[key, keyAfter(key))` without the write map's range walk
  (`transaction.go:1060-1074` keeps it off the Get hot path's re-sort).
  `TestAddReadConflictKeyMatchesRangeForm` pins the two bodies to each other.
- Recording the deferred error is shared with the write form and `Atomic`: RFC-267's
  "earliest illegal buffered mutation wins" scan in `deferWriteConflictRange` and the same
  scan inline in `Atomic` (`transaction.go:1408-1426`) become one
  `recordDeferredError(poison)`, store-if-unset under `conflictMu`. Go buffers illegal
  mutations until commit; C++ would have recorded the earlier mutation's error first.

The facade's `AddReadConflictRange` return then carries only `inverted_range`, and
`AddReadConflictKey` keeps returning `nil`, as with the Apple binding over libfdb_c
(`[k, k\x00)` is never inverted). Doc comments on both layers say so, and the
`deferredErr` field comment lists the read form beside the write form.

Out of scope: `checkUsedDuringCommit` (`:1946-1948`, 2017). Go has no used-during-commit
state for any operation (RFC-068 records the auto-reset extension that replaces it).

## Wire and data compatibility

No mutation bytes change. Conflict ranges change only where Go diverged: an illegal range
or key adds no read conflict (before: the range form added none, the key form added one),
and an oversized key adds none (before: a full-length single-key range). The visible change
is where 2004 surfaces: the next read, `GetApproximateSize`, `GetVersionstamp` or `Commit`
instead of the call, and for the key form at all.

## Performance

The key form gains one atomic load, two byte comparisons against a 1-2 byte bound and a
length check on the legal path; the range form gains one atomic load. The illegal path
scans buffered mutations once, as the write forms do.

## Test plan

Lanes: `ryw_pin_unit_test` is in `just test`; `client_test` and `bench_test` are tagged
`test-full`, so the change is verified with `just test-full` as well.

- `pkg/fdbgo/client/ryw_review_regression_test.go` (`ryw_pin_unit_test`, fast lane, on
  `&Transaction{}` like the RFC-267 tests):
  - `TestAddReadConflictRangeLegalRangeIsDeferred`: the range and key forms in the user,
    read-system and access-system modes, over endpoints at and past `\xff` and `\xff\xff`
    and the metadataVersionKey range/key. The call returns `nil`; an illegal one adds no
    read conflict, and the next `GetKey` and the `Commit` fail 2004; a legal one does not
    poison and records exactly its range (none when empty), including the
    metadataVersionKey range and key and the system keys legal under each mode.
  - `TestAddReadConflictRangeInvertedIsImmediate`: 2005 from the call on a clean
    transaction, with no deferred error or conflict and a clean next `GetKey`; after a
    deferred error 2005 still comes from the call and the deferred error is unchanged.
  - `TestAddReadConflictRangePreservesDeferredOrder`: an earlier oversized `Set` keeps its
    2102 verdict over a later illegal range or key; a legal range or key added after a
    deferred error records no conflict.
  - `TestAddReadConflictKeyMatchesRangeForm`: for user, self-written, system,
    `maxReadKey`, metadataVersionKey, at-limit and over-limit (user and system) keys, in
    each mode with RYW on and off, `AddReadConflictKey(k)` leaves the same deferred error
    and read conflicts as `AddReadConflictRange(k, k\x00)`; an over-limit key records
    none and an at-limit key records one.
- `pkg/fdbgo/client/conflict_range_validation_test.go` (`client_test`, test-full):
  `TestAddReadConflictRange_Validation` asserts the call's code (0 or 2005) and the
  deferred code (0 or 2004) separately, plus key-form rows.
- `pkg/fdbgo/bench/differential_conflict_range_test.go` (`bench_test`, test-full, against
  libfdb_c): `TestDifferential_ReadConflictRangeDeferred` compares, per client, the call's
  code, `GetApproximateSize` after the call (code, and growth over its value before the
  call), the next snapshot `Get` and the `Commit`. Both clients count every recorded read
  conflict in the approximate size (C++ `ReadYourWrites.actor.cpp:346`, `:1978`; Go
  `transaction.go:2604-2606`), so its growth shows whether a legal call recorded a conflict
  and of what size, which the read-only commit cannot. Rows: the range form (legal, empty,
  end and begin past `\xff`, `READ_SYSTEM_KEYS` legal and past `\xff\xff`, the
  metadataVersionKey range and a non-exact one, inverted) and the key form (legal, `\xff`,
  `\xff\x05` with and without `READ_SYSTEM_KEYS`, `\xff\xff` under it,
  metadataVersionKey, and a user key at 10008 and 10009 bytes and, under `READ_SYSTEM_KEYS`,
  a `\xff\x05`-prefixed system key at 30000 and 30001 bytes, the `getMaxReadKeySize`
  boundaries of `NativeAPI.actor.cpp:11621-11633`; without the option a system key fails
  the legal-range check before the clamp. The at-limit keys grow the size, the over-limit
  keys do not, and all four return 0 everywhere).
  Red before the fix: the range form's illegal rows on every column (call go=2004 cgo=0;
  approximate size, `Get` and `Commit` go=0 cgo=2004), the key form's illegal rows on the
  later columns (go=0, cgo=2004), and the over-limit keys on the size growth. No
  wire-golden scenario covers oversized explicit read conflicts
  (`tools/clientbench/internal/wgscenarios/scenarios.go:176-185` uses short keys); the
  approximate size is the data-plane observable of the recorded range.
  `TestDifferential_ConflictRangeMaxKey`'s comment is updated: libfdb_c defers both forms,
  and Go now does too.
- `rfcs/126-fuzzapi-workload.md` §2.2/§3.2 notes point here for the read half. TODO.md:
  tick the §0 read-conflict item with a "Fixed in RFC-268" note, and repoint the
  "Panic-safe locks" item's read-conflict site to `addReadConflictRangeRYW`.
- `pkg/simfdb` (`simfdb_test`, fast lane): SimFDB's key forms gain the clamp its range forms
  already port (`clampConflictRange`). `TestOversizedConflictKeyDropped` checks both key
  forms at the user (10008) and system (30000) limits, where the conflict still aborts a
  reader with 1020, and one byte over, where nothing is recorded. The 2004 check stays
  unported there (`pkg/simfdb/conflict.go:52-57`).
- Revert-proof: restoring the immediate return turns the range rows of the fast-lane tests
  and the differential red; removing the key form's check turns its rows red.
