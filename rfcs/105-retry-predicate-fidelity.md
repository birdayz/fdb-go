# RFC-105: Retry-predicate fidelity — pin each predicate to its C++ analog, kill drift

**Status:** Draft
**Item:** Client launch-readiness #2 (TODO.md) — TODO-production P3.3 ("de-duplicate the two retry
predicates"). Gate: `fdb-client-engineer` (FDB C++ dev + Torvalds + codex). C++ (libfdb_c 7.3.75)
is the spec.

## Problem

The Go client has **three** hand-maintained retry-decision sites, each with its own duplicated
code list:

1. **`fdb.IsRetryable`** (`fdb/error.go`) — the public facade predicate, the analog of C++
   `fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE)`.
2. **`Transaction.OnError`'s switch** (`transaction.go:1592`) — the main retry loop, the analog of
   C++ `Transaction::onError` (it also picks the backoff class per code).
3. **`client.isRetryable`** (`commitpath.go:216`, used at `:180`) — the `commitDummyTransaction`
   retry (the `commit_unknown_result` idempotency barrier, RFC-090), whose errors "route through
   `tr.onError`" (its own comment) — so it should equal #2's retryable set.

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

- **Q1 — `cluster_version_changed` (1039) in Go's `OnError`.** Go's `OnError` retries 1039 (as
  MAYBE_COMMITTED self-conflicting, `transaction.go:1644`); C++ `onError` does NOT (returns `e`).
  Is Go's retry a correct improvement (1039 *is* `fdb_error_predicate`-retryable, and the
  self-conflicting barrier makes a retry safe) or a divergence to remove? **FDB C++ dev rules.**
- **Q2 — the Go-only / forward-compat codes.** `OnError`/`client.isRetryable` retry
  `all_proxies_unreachable` (1200, Go-internal Layer-2 error — NOT C++ 1200),
  `transaction_throttled_hot_shard` (1235) and `transaction_rejected_range_locked` (1242)
  (both FDB 7.4+, absent in 7.3.75). These are documented Go extensions. Should `fdb.IsRetryable`
  also report them? **Decision: NO.** `fdb.IsRetryable` is the `fdb_error_predicate` analog and must
  match its FIXED C++ list exactly for cross-client parity (a Go app expecting libfdb_c's predicate
  must get libfdb_c's answer). The retry *loop* (`OnError`) legitimately retries more (C++ `onError`
  also retries more than the predicate, e.g. 1079); the *predicate* does not. This split is the
  point and must be pinned by tests so it isn't "fixed" into a divergence later.

## Proposed change (no behavior change — hardening + de-drift)

1. **Single source of truth for the per-code classification.** Add one table/helper (in
   `pkg/fdbgo/client`) that classifies each error code into its C++ retry attributes:
   its `fdb_error_predicate` membership (none / MAYBE_COMMITTED / RETRYABLE_NOT_COMMITTED) and its
   `onError` backoff class (none / version-delay / exp-backoff / resource-30s / maybe-committed-
   self-conflict), each row carrying the `/tmp/fdbsrc` citation. The Go-only/forward-compat codes
   (1039-in-onError, 1079, 1200, 1235, 1242) are rows with an explicit `goExtension` reason.
2. **Derive all three sites from it** so no code list is duplicated:
   - `OnError`'s switch dispatches on the `onError` backoff class (keeps the exact current behavior
     incl. the self-conflicting deep-copy for the maybe-committed class).
   - `client.isRetryable` = "the code's `onError` class is retryable" (it must equal `OnError`'s
     retryable set — the dummy uses `onError`; cross-checked by a test).
   - `fdb.IsRetryable` = "the code is in `fdb_error_predicate(RETRYABLE)`" — derived from the
     predicate column. (`fdb/error.go` calls a small exported helper; no list duplicated there.)
3. **Pin to C++ with regression tests** (the real drift sentinel):
   - `fdb.IsRetryable` returns true for EXACTLY the 12 `fdb_error_predicate` codes and false for a
     sampling of non-retryable ones (1006, 1031, 2000, …) — a table mirroring `fdb_c.cpp:78-94`.
   - `OnError`/`client.isRetryable` retryable set = the documented 16-code Go-onError set; a test
     asserts `client.isRetryable(c) == onErrorClassRetryable(c)` for all codes (they cannot drift
     apart).
   - The `goExtension` rows are asserted present with their reason, so a future reader can't delete
     them as "not in C++" without seeing why.

## Wire-compat impact

**None.** No bytes change; no observable retry behavior changes (the lists are already correct).
This is purely internal de-duplication + test pinning. The one *possible* behavior change is Q1
(1039 in `OnError`) — if the FDB C++ dev rules it should be removed, that is a deliberate,
separately-tested change; otherwise behavior is identical.

## Test plan

- The three pinning tables above (fail if any predicate drifts from its C++ analog).
- A cross-check test: `client.isRetryable` and `OnError`'s retryable set are identical for all codes.
- Revert-prove: flip one code in the source-of-truth table → the relevant pinning test goes red.
- `-race` on `//pkg/fdbgo/client` (the predicates are pure functions; trivially safe, but the
  package is touched).

## Open question for review

Q1 (retry 1039 in `OnError`): keep (Go-correct improvement, self-conflicting-safe) or remove (match
C++ `onError` literally)? FDB C++ dev decides; I'll implement the ruling.
