# RFC-267: Explicit write-conflict-range errors are deferred

**Status:** Implemented
**Item:** TODO.md §0, "Explicit write-conflict-range errors are immediate."
**Spec:** FoundationDB 7.3.77 (`/tmp/fdbsrc`); Go line numbers at `cab2e9d50`.

## Problem

`Transaction.AddWriteConflictRange` (`pkg/fdbgo/client/transaction_state_entry.go:407`,
`stateAddWriteConflictRange` at `transaction.go:3218`) returns `key_outside_legal_range`
(2004) from the call when an endpoint is past `maxWriteKey`. The `fdb` facade returns it
to the application (`pkg/fdbgo/fdb/transaction.go:381-383`) and records nothing. libfdb_c returns
success from the call and fails the transaction's next operation instead. The error site
differs, and a caller that discards the return (as `pkg/recordlayer/bunched_map.go:485`
does for its user-key ranges) commits successfully under Go while libfdb_c fails the
commit with 2004.

RFC-126 §2 recorded libfdb_c as returning 2004 for `addWriteConflictRange(a, "\xff\xff\xff")`.
Its probe ran the call inside `Transact` and compared the transaction's final error
(`goErrCode`/`cgoErrCode`, `pkg/fdbgo/bench/differential_errorcode_test.go:30-43`), so
libfdb_c's 2004 came from the commit, not the call. The code is right; the site is not.
This RFC supersedes the write half of RFC-126 §2.2 on where the error surfaces, and the
implementation adds a note there.

## C++ behavior

1. `fdb_transaction_add_conflict_range` (`bindings/c/fdb_c.cpp:1216-1227`) builds a
   `KeyRangeRef` on the calling thread. Its constructor throws `inverted_range` (2005)
   when `begin > end` (`fdbclient/include/fdbclient/FDBTypes.h:318-323`);
   `CATCH_AND_RETURN` returns that code from the call. This is the only synchronous error.
2. `ThreadSafeTransaction::addWriteConflictRange` (`fdbclient/ThreadSafeTransaction.cpp:659-663`)
   enqueues RYW `addWriteConflictRange` with
   `onMainThreadVoid(..., tr, &ISingleThreadTransaction::deferredError)`.
3. `doOnMainThreadVoid` (`flow/include/flow/ThreadHelper.actor.h:50-61`) skips the functor
   when `deferredError` is already set; otherwise a throw is stored in `deferredError`.
4. RYW `addWriteConflictRange` (`fdbclient/ReadYourWrites.actor.cpp:2461-2500`) throws
   `key_outside_legal_range` when an endpoint exceeds `getMaxWriteKey()` (`:2466-2468`),
   before the size clamp and before touching the write map.
5. Every later future-returning operation re-throws it through `checkDeferredError`
   (for example `commit`, `ThreadSafeTransaction.cpp:666-672`); it clears only on reset.

Go's `AddWriteConflictKey` (`transaction.go:3248-3274`) already follows steps 2-5 for the
single-key form, including the rule that an earlier illegal mutation, which Go buffers
until commit, keeps its earlier verdict (C++ would have recorded it first).

## Fix

Split `stateAddWriteConflictRange` into the synchronous part and the network-thread part:

- `stateAddWriteConflictRange(begin, end) error`: return 2005 for `begin > end` (step 1,
  regardless of an existing deferred error, as in C++); otherwise run the deferred part
  and return `nil`.
- `stateAddWriteConflictKey(key)`: run the deferred part on `[key, keyAfter(key))`.
- The deferred part is today's `AddWriteConflictKey` body: return if `deferredErr` is
  set (step 3); run the legal-range check, clamp and conflict-map insert; on 2004 record
  the earliest illegal buffered mutation's code, else 2004, with `CompareAndSwap(nil, …)`.

The facade's `AddWriteConflictRange` return then carries only `inverted_range`, as with
the Apple binding over libfdb_c. Doc comments on both layers say so.

Out of scope, recorded as a new TODO.md entry in this change: the read forms have the same C++ path
(`ThreadSafeTransaction.cpp:589-594`, RYW `:1950-1955`), but Go's `AddReadConflictRange`
returns 2004 immediately and `AddReadConflictKey` has no legal-range check at all. RFC-268
fixes both.

## Wire and data compatibility

No bytes change. A legal range adds the same conflict range as today; an illegal range
added none before and adds none now. The visible change is where 2004 surfaces: the next
read, `GetApproximateSize`, `GetVersionstamp` or `Commit` instead of the call. The commit
is refused before anything is sent, as before when callers propagated the error.

## Performance

One extra atomic load per call on the legal path. The illegal path scans buffered
mutations once, as the key form already does.

## Test plan

Lanes: `ryw_pin_unit_test` is in `just test`; `client_test` and `bench_test` are tagged
`test-full`, so the change is verified with `just test-full` as well.

- `pkg/fdbgo/client/ryw_review_regression_test.go` (`ryw_pin_unit_test`, fast lane, built
  on `&Transaction{}` like the key-form tests there):
  - `TestAddWriteConflictRangeLegalRangeIsDeferred`: ranges in the user, read-system and
    access-system modes, including both endpoint edges at `maxWriteKey`. The call returns
    `nil`; an illegal range adds no write-map node and no write conflict, and the next
    `GetKey` and the `Commit` fail 2004; a legal range does not poison.
  - `TestAddWriteConflictRangeInvertedIsImmediate`: on a clean transaction 2005 from the
    call leaves no deferred error, write-map node or write conflict, and the next `GetKey`
    succeeds; after a deferred error 2005 still comes from the call and the deferred error
    is unchanged.
  - `TestAddWriteConflictRangePreservesDeferredOrder`: an earlier oversized `Set` keeps its
    2102 verdict over a later illegal range; a range added after a deferred error records
    nothing.
- `pkg/fdbgo/client/conflict_range_validation_test.go` (`client_test`, test-full):
  `TestAddWriteConflictRange_Validation` asserts the call's code (0 or 2005) and the
  deferred code (2004) separately.
- `pkg/fdbgo/bench/differential_conflict_range_test.go` (`bench_test`, test-full, against
  libfdb_c): `TestDifferential_WriteConflictRangeDeferred` compares the codes of the call,
  the next `Get` and the `Commit` for a legal, an end-past-max, a begin-past-max, a
  read-system-keys and an inverted range; red before the fix (go=2004, cgo=0 at the call).
  It is a separate write-only test: `TestDifferential_ConflictRangeEdges` runs its shapes
  through the read method too, which keeps the out-of-scope divergence. The commit step is
  the discard-then-commit shape: go committed (0) where libfdb_c failed (2004).
  `TestDifferential_ConflictRangeMaxKey` compares only the final `Transact` error, which
  is 2004 on both sides before and after; its comment is corrected to say where 2004
  surfaces.
- Revert-proof: restoring the immediate return turns the three fast-lane tests red
  (`just test`; the inverted-range test reaches its deferred-error half through an illegal
  range, which then fails at the call) and the differential red (`just test-full`).
