# RFC-105: Retry-predicate fidelity — pin each predicate to its C++ analog, kill drift

**Status:** Accepted — FDB C++ dev ACK (Q1: keep 1039), Torvalds (table dropped → tests-only),
codex (dead 4th predicate deleted), all addressed on r2 (2026-06-13).
**Item:** Client launch-readiness #2 (TODO.md) — TODO-production P3.3 ("de-duplicate the two retry
predicates"). Gate: `fdb-client-engineer` (FDB C++ dev + Torvalds + codex). C++ (libfdb_c 7.3.75)
is the spec.

## Problem

The Go client has **four** hand-maintained retry-decision sites, each with its own duplicated
code list:

1. **`fdb.IsRetryable`** (`fdb/error.go`) — the public facade predicate, the analog of C++
   `fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE)`.
2. **`Transaction.OnError`'s switch** (`transaction.go:1592`) — the main retry loop, the analog of
   C++ `Transaction::onError` (it also picks the backoff class per code).
3. **`client.isRetryable`** (`commitpath.go:216`, used at `:180`) — the `commitDummyTransaction`
   retry (the `commit_unknown_result` idempotency barrier, RFC-090), whose errors "route through
   `tr.onError`" (its own comment) — so it should equal #2's retryable set.
4. **`wire.FDBError.Retryable()`** (`wire/reader.go`) — a FOURTH list (the predicate's 12 codes ∪
   {`1006` all_alternatives_failed, `1200`, `1235`, `1242`} — note it has `1006` and LACKS `1079`,
   so it equals neither #1 nor #2). **It has ZERO production callers** (only its own test) — dead
   code that nonetheless carries a divergent classification (`1006`/all_alternatives_failed is not
   retryable in either C++ predicate). Found by codex.

P3.3 flagged this as "drift risk; single source." Investigation (against `/tmp/fdbsrc` 7.3.75)
found **no current divergence** — the lists happen to be correct today — but:

- **`fdb.IsRetryable` ≠ `OnError`'s set, correctly** (they are different C++ predicates), so a naive
  "unify them into one" — P3.3's literal suggestion — would be **wrong**. `fdb_error_predicate`
  (12 codes) excludes `blob_granule_request_failed` (1079) and the Go extensions; `onError`
  (16 codes) includes them. This RFC does NOT unify the two semantics.
- The three lists are **hand-duplicated** → the real risk is future drift, and **none is pinned to
  the C++ source with a regression test**, so drift would land silently.
- Two consistency questions the duplication hides (below) need an explicit, tested answer.

### The C++ ground truth (`/tmp/fdbsrc` 7.3.75)

- **`fdb_error_predicate(RETRYABLE)`** = `MAYBE_COMMITTED ∪ RETRYABLE_NOT_COMMITTED`
  (`bindings/c/fdb_c.cpp:78-94`):
  - MAYBE_COMMITTED: `commit_unknown_result` (1021), `cluster_version_changed` (1039).
  - RETRYABLE_NOT_COMMITTED: `not_committed` (1020), `transaction_too_old` (1007),
    `future_version` (1009), `database_locked` (1038), `grv_proxy_memory_limit_exceeded` (1078),
    `commit_proxy_memory_limit_exceeded` (1042), `batch_transaction_throttled` (1051),
    `process_behind` (1037), `tag_throttled` (1213), `proxy_tag_throttled` (1223).
  - **= the 12 codes Go's `fdb.IsRetryable` already lists.** Verified equal.
- **`Transaction::onError`** (`NativeAPI.actor.cpp:7734-7780`) resets+retries: `not_committed`,
  `commit_unknown_result`, `database_locked`, `commit_proxy_memory_limit_exceeded`,
  `grv_proxy_memory_limit_exceeded`, `process_behind`, `batch_transaction_throttled`,
  `tag_throttled`, `blob_granule_request_failed` (1079), `proxy_tag_throttled`,
  `transaction_too_old`, `future_version` — else `return e`. **Note: `onError` retries 1079 but NOT
  1039; `fdb_error_predicate` is the reverse on both.** (This asymmetry is real C++ behavior, not a
  Go artifact — the source of the "1039 predicate-retryable-not-onError / 1079 the reverse" note.)

### Two questions the duplication hides

- **Q1 — `cluster_version_changed` (1039) in Go's `OnError`. RESOLVED: KEEP (FDB C++ dev ruling).**
  Go's `OnError` retries 1039; C++ `NativeAPI::onError` returns `e` for it — but that is NOT because
  libfdb_c gives up on 1039. C++ retries 1039 one layer up, in
  `MultiVersionTransaction::onError` (`MultiVersionTransaction.actor.cpp:1740-1761`): it swaps to
  the new-version connection (`updateTransaction(true)`) and returns `Void()` = retry, even catching
  a 1039 thrown by the inner onError and converting it to a retry. `NativeAPI::onError` returns `e`
  precisely because the MVC layer above it owns the version-switch retry. **Go has no separate MVC
  layer — `OnError` is the only retry site — so folding 1039 into `OnError` reproduces the
  *aggregate* libfdb_c behavior (1039 ⇒ retry), and the self-conflicting deep-copy is the correct
  idempotency barrier (1039 is MAYBE_COMMITTED).** Removing it would make Go *less* faithful. The
  test must annotate 1039 with the `MultiVersionTransaction.actor.cpp:1740` citation so no one
  "fixes" it to the literal NativeAPI behavior.
- **Q2 — the Go-only / forward-compat codes.** `OnError`/`client.isRetryable` retry
  `all_proxies_unreachable` (1200, Go-internal Layer-2 error — NOT C++ 1200),
  `transaction_throttled_hot_shard` (1235) and `transaction_rejected_range_locked` (1242)
  (both FDB 7.4+, absent in 7.3.75). These are documented Go extensions. Should `fdb.IsRetryable`
  also report them? **Decision: NO.** `fdb.IsRetryable` is the `fdb_error_predicate` analog and must
  match its FIXED C++ list exactly for cross-client parity (a Go app expecting libfdb_c's predicate
  must get libfdb_c's answer). The retry *loop* (`OnError`) legitimately retries more (C++ `onError`
  also retries more than the predicate, e.g. 1079); the *predicate* does not. This split is the
  point and must be pinned by tests so it isn't "fixed" into a divergence later.

## Proposed change (NO refactor — delete dead code + pin with tests)

Torvalds NAK'd the original "single source-of-truth table": `OnError` doesn't dispatch on a
boolean — its MAYBE_COMMITTED arm does a `conflictMu`-locked deep-copy of write→read conflicts
split around the backoff sleep (`transaction.go:1644-1673`), tied to `conflictBuf` lifetime. A
table "dispatching on backoff class" would force restructuring that subtle, working code (CLAUDE.md
#5/#10). The drift problem's actual cure is **tests**, not an abstraction. Revised plan:

1. **Delete the dead 4th predicate** `wire.FDBError.Retryable()` + its test (`wire/fdberror_test.go`).
   Zero production callers, and it carries a divergent classification (`1006`/all_alternatives_failed
   retryable — wrong per C++). Removing it cuts the drift surface 4→3 and resolves codex's
   import-cycle concern (no shared table needed). (If a reviewer wants it kept as a public
   convenience, the fallback is to pin it to `fdb_error_predicate` exactly and drop the `1006`
   wire-addition — but delete is cleaner for unused code.)
2. **Keep the three real switches AS-IS** (`fdb.IsRetryable`, `OnError`, `client.isRetryable`) —
   they are correct today and already carry inline C++ citations. No restructuring.
3. **Single-source the onError retryable set — DERIVE, don't mirror (Torvalds' binding fix).** Today
   `OnError`'s 4 `case` arms implicitly define the 16-code retryable set, AND `client.isRetryable`
   (`commitpath.go:216`) is a *second* hand-copy of those labels — already the drift this RFC kills.
   Make ONE function the source: `onErrorRetryable(code) bool` (the 16-code list, with the C++/MVC
   citations). Then **`OnError` calls it** as a guard so a code can't be retryable-here-but-not-there
   by construction:
   ```
   if !onErrorRetryable(code) { state = errored; return err }   // single source of WHETHER to retry
   switch code {                                                 // refine only the BACKOFF CLASS:
   case too_old, future_version:                version delay
   case proxy_mem, grv_proxy_mem, hot_shard, range_locked: resource (30s) backoff
   case commit_unknown_result, cluster_version_changed:    maybe-committed self-conflict (the
                                                           conflictMu deep-copy, UNCHANGED)
   default:                                     exp backoff   // the RETRYABLE_NOT_COMMITTED rest
   }
   ```
   The deep-copy block (`transaction.go:1644-1673`) is NOT restructured — it stays inline in its arm;
   only the *retryability decision* moves to the guard. `client.isRetryable` becomes
   `= onErrorRetryable` (one line). Now a code missing from `onErrorRetryable` is structurally
   un-retryable everywhere — no mirror to drift.
4. **Add C++-pinned regression tests** — EXHAUSTIVE, not sampled (codex): each test enumerates a
   fixed list of ALL known FDB error codes (the `Err*` constants 1000–1300 + 2xxx + the Go-internal
   ones) and asserts `predicate(code) == (code ∈ expectedSet)` for EVERY code. A sampled test would
   miss a spuriously-added retryable code (e.g. 1235 sneaking into `fdb.IsRetryable`); the set
   comparison catches an extra OR a missing code.
   - `fdb.IsRetryable`: `expectedSet` = the 12 `fdb_error_predicate(RETRYABLE)` codes
     (`bindings/c/fdb_c.cpp:78-94`). Asserted exhaustively.
   - `onErrorRetryable`: `expectedSet` = the documented 16-code Go-onError set
     (`NativeAPI.actor.cpp:7743-7768` + the Go-extension rows). Asserted exhaustively.
   - Because `OnError` CALLS `onErrorRetryable` (§3) and `client.isRetryable == onErrorRetryable`,
     pinning `onErrorRetryable` pins all three retry-loop sites at once — no separate switch can
     drift (codex P2: the sentinel asserts the function the real loop uses, not a parallel copy).
   - The Go-extension rows (1039, 1079, 1200, 1235, 1242) are asserted retryable-in-onError with
     their documented reason + citation, so a future reader can't "fix" them to a literal NativeAPI
     port and silently break (esp. 1039 — cite `MultiVersionTransaction.actor.cpp:1740`, see Q1).

## Wire-compat impact

**None.** No bytes change; no observable retry behavior changes — the three real predicates' lists
are already correct (verified vs C++) and stay as-is. The only code removed is the unused, never-
called `wire.FDBError.Retryable()` (dead code; no caller's behavior changes). Q1 resolved to KEEP
1039, so the retry loop is unchanged. This is purely dead-code removal + test pinning.

## Test plan

- The two pinning tables (§4): `fdb.IsRetryable` == the 12 `fdb_error_predicate` codes; and
  `onErrorRetryable` == the 16-code Go-onError set — each with non-retryable negatives. Fail if any
  predicate drifts from its C++ analog.
- `client.isRetryable == onErrorRetryable` is now guaranteed by construction (`client.isRetryable`
  *is* `onErrorRetryable`), so no cross-check test is needed — but a behavior test exercises a real
  `OnError` retry of one Go-extension code (e.g. drive a 1039 through `OnError`, assert it resets +
  the self-conflict ranges are added) so the guard+switch refactor is end-to-end covered.
- Revert-prove: flip one code in `onErrorRetryable` (or `fdb.IsRetryable`) → the relevant pinning
  test goes red; and removing 1039 makes the `OnError`-1039 behavior test red.
- Full `pkg/fdbgo/client` suite + `-race` (the `OnError` guard restructure touches the live retry
  path — verify no behavior regression, esp. the maybe-committed self-conflict path).

## Review status

- **FDB C++ dev: ACK.** Verified both predicate lists member-for-member vs `/tmp/fdbsrc` 7.3.75.
  Q1 ruled **KEEP 1039** (C++ retries it in the MVC layer, `MultiVersionTransaction.actor.cpp:1740`);
  Q2 agreed (forward-compat/Go-only codes stay out of `fdb.IsRetryable`). Nit: cite the MVC layer on
  the 1039 row — folded in above.
- **Torvalds: NAK (table) → ACK-conditional (r2) → addressed.** Dropped the table. r2 binding fix:
  the helper must be the SINGLE SOURCE, not a mirror — `OnError` now *calls* `onErrorRetryable` as a
  retryability guard (the backoff switch only refines the delay), and `client.isRetryable` becomes
  `= onErrorRetryable`. The self-conflict deep-copy is untouched. "Derive, don't mirror" satisfied.
- **codex: 3×P2 → addressed (r2 plan).** (1) The implementation will DELETE the dead 4th predicate
  `wire.FDBError.Retryable()` + its test (it still exists on this branch; removed in the impl
  commit, not yet — no shared table remains so the import-cycle concern is moot). (2) The drift
  sentinel asserts `onErrorRetryable`, the function `OnError` actually calls (§3) — not a parallel
  copy — so the real loop can't drift. (3) The pin tests are EXHAUSTIVE over all known codes
  (set comparison), not 12-true-plus-samples.
