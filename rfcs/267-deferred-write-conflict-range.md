# RFC-267: Explicit write-conflict-range errors are deferred

**Status:** Draft
**Item:** TODO.md §0, "Explicit write-conflict-range errors are immediate."
**Spec:** FoundationDB 7.3.77 (`/tmp/fdbsrc`); Go line numbers at `cab2e9d50`.

## Problem

`Transaction.AddWriteConflictRange` (`pkg/fdbgo/client/transaction_state_entry.go:407`,
`stateAddWriteConflictRange` at `transaction.go:3218`) returns `key_outside_legal_range`
(2004) from the call when an endpoint is past `maxWriteKey`. The `fdb` facade returns it
to the application (`fdb/transaction.go:381-384`) and records nothing. libfdb_c returns
success from the call and fails the transaction's next operation instead. The error site
differs, and a caller that discards the return (as `pkg/recordlayer/bunched_map.go:485`
does for its user-key ranges) commits successfully under Go while libfdb_c fails the
commit with 2004.

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

Out of scope, recorded as a new TODO.md entry: the read forms have the same C++ path
(`ThreadSafeTransaction.cpp:589-594`, RYW `:1950-1955`), but Go's `AddReadConflictRange`
returns 2004 immediately and `AddReadConflictKey` has no legal-range check at all.

## Wire and data compatibility

No bytes change. A legal range adds the same conflict range as today; an illegal range
added none before and adds none now. The visible change is where 2004 surfaces: the next
read, `GetApproximateSize`, `GetVersionstamp` or `Commit` instead of the call. The commit
is refused before anything is sent, as before when callers propagated the error.

## Performance

One extra atomic load per call on the legal path. The illegal path scans buffered
mutations once, as the key form already does.

## Test plan

- Fast lane (`ryw_pin_unit_test`): `AddWriteConflictRange` past `maxWriteKey` in the
  user, read-system and access-system modes returns `nil`, adds no write-map node, and
  the next `GetKey` and `Commit` fail 2004; a legal range does not poison. An inverted
  range returns 2005 even with a deferred error already recorded. An earlier oversized
  `Set` keeps its 2102 verdict over a later illegal range, and a range added after a
  deferred error changes nothing.
- `client_test`: `TestAddWriteConflictRange_Validation` asserts the immediate code (0 or
  2005) and the deferred code (2004) separately.
- Differential (`bench_test`, against libfdb_c): `TestDifferential_ConflictRangeEdges`
  gains a `write_past_max_key` shape compared on both the immediate code and the next
  `Get`'s code; red before the fix (go=2004, cgo=0 at the call).
- Revert-proof: restoring the immediate return turns the fast-lane test and the
  differential red.
