# RFC-171: Transaction.reset() must clear non-persistent options (port C++ reset semantics)

**Status:** Draft — needs FDB-C-dev design ACK before implementation (the OnError-retry path is the risky part).
**Item:** FDB client bug-hunt (2026-06-30) re-run, findings: `txn-options-lifecycle` (HIGH) +
`snapshot-ryw` (MEDIUM, same root). See `shifts/2026-06-30-fdbgo-client-bughunt.md`.
**Spec:** libfdb_c 7.3.77 (`/tmp/fdbsrc`).

## Problem

Go's `Transaction.reset()` (`transaction.go:2758-2810`) **preserves** transaction-level options
across a reset — its own comment says "Preserved across reset … retryCount, backoff, timeout,
retryLimit, priority, causalReadRisky, lockAware, readLockAware, sizeLimit, maxRetryDelay,
rywDisabled, snapshotRYWDisableCount, tenantId, creationTime, tags". The doc claims this "matches
C++ persistent option re-application" — it is the **opposite**.

C++ `ReadYourWritesTransaction::reset()` (`ReadYourWrites.actor.cpp:2735-2755`):
1. `persistentOptions.clear()` + `sensitivePersistentOptions.clear()` (`:2744-2745`)
2. `options.reset(tr)` — a `memset` of `ReadYourWritesTransactionOptions` (`:2077-2082`): so
   `readSystemKeys = writeSystemKeys = readYourWritesDisabled = false`, `sizeLimit`/`priority`
   back to default.
3. re-copy **`getTransactionDefaults()`** (the **Database**-level defaults) into `persistentOptions`
   (`:2749-2751`), then `resetRyow()` (`:2752`) re-applies only those.

Only **`timeout` / `retry_limit` / `max_retry_delay` / `auth_token`** are marked `persistent="true"`
in `vexillographer/fdb.options` (`:294/298/302/351`). `read_system_keys` (302→wait, 302 is the
code; persistence flag is separate), `access_system_keys`, `read_your_writes_disable` (51),
`size_limit` (503), `priority`, `lock_aware`, `snapshot_ryw_disable` (601) are **not** persistent →
cleared by reset. And even the persistent ones revert to the **Database** defaults, not the per-txn
overrides (the per-txn `setOption` entries were cleared).

Crucially, C++ clears these on **BOTH** paths: user `reset()` AND the `OnError` retry
(`RYWImpl::onError :1521 → resetRyow() → options.reset(tr)`). Go's single shared `reset()`
preserves them on both.

### Concrete divergences
- **Over-permission (safety):** `SetReadSystemKeys(); Reset(); Get(\xff"foo")` → Go keeps
  `readSystemKeys` → `maxReadKey()=\xff\xff` → the read passes the legal-range gate (returns the
  system value / nil). libfdb_c cleared `read_system_keys` → `getMaxReadKey()=\xff` →
  `key_outside_legal_range` (2004). A reused Go handle can read/write `\xff` system keys a libfdb_c
  handle rejects after reset.
- **Divergent commit acceptance:** `SetSizeLimit(100); Reset();` commit a >100-byte txn → Go
  rejects (`transaction_too_large`, limit preserved), libfdb_c accepts (reverted to 10 MB default).
- **Snapshot RYW:** `SetSnapshotRYWDisable(); …; reset()/retry` → Go keeps the disable so snapshot
  reads keep bypassing pending writes; libfdb_c re-enables (DB default) so they see pending writes.
- **Priority / lock_aware** likewise persist in Go, revert in C++.

## Why this needs design review (not a quick patch)
The faithful fix changes `reset()` to set the non-persistent option fields back to the **Database
defaults** (`db.TransactionDefaults`), keeping only the persistent set. But `reset()` is shared by
user `Reset()` AND the `OnError` retry loop, so this changes retry behavior: a per-transaction
`SetReadSystemKeys`/`SetSizeLimit`/`SetSnapshotRYWDisable`/priority will **no longer survive a
retry** (matching C++). Existing Go tests/behaviors may rely on the preservation. The exact
persistent-vs-non-persistent partition, and whether the per-txn `timeout`/`retry_limit` should
revert to DB defaults (C++ says yes — they re-apply DB defaults, not per-txn), need FDB-C-dev
sign-off.

## Proposed design
1. Enumerate the option fields by persistence (from `fdb.options`):
   - **Persistent (re-apply DB defaults on reset):** `timeout`, `retryLimit`, `maxRetryDelay`
     (+ `auth_token` when supported).
   - **Non-persistent (clear to DB default on reset):** `readSystemKeys`, `writeSystemKeys`,
     `rywDisabled`, `bypassUnreadable`, `lockAware`, `readLockAware`, `sizeLimit`, `priority`,
     `causalReadRisky`, `snapshotRYWDisableCount`, **and `tags` / `readTags`**. (`tenantId` is set at
     construction, not an option — keep.)
   - **CORRECTION (was backwards in the earlier draft):** `tags` is NON-persistent and C++ **clears** it
     on reset — `TransactionOptions::clear()` sets `tags = readTags = TagSet{}`
     (`NativeAPI.actor.cpp:6131-6144`), and `tag` is marked non-persistent in `fdb.options` (only
     `timeout`/`retry_limit`/`max_retry_delay`/`auth_token` are `persistent="true"`). So Go's
     preservation of `tags` across reset/retry — and its `reset()` comment claiming "C++ keeps tags across
     retries" — are THEMSELVES the divergence to FIX here, not to confirm.
2. `reset()` resets the non-persistent fields to the values derived from `db.TransactionDefaults`
   (the Database-level defaults the client already tracks: `SetDefaultReadSystemKeys`,
   `SetTransactionTimeout`, `SetTransactionSizeLimit`, …), and re-applies the persistent DB
   defaults — mirroring `options.reset(tr)` + the `getTransactionDefaults()` re-copy.
3. Both `Reset()` and the `OnError` retry get the cleared semantics (they already share `reset()`).

## Test plan
- **Differential (single handle reused via Reset):** the over-permission repro
  (`SetReadSystemKeys(); Reset(); Get(\xff"foo")` → cgo 2004, Go must be 2004), the `size_limit`
  repro (commit accept/reject), the snapshot-RYW repro, the priority repro — each red→green.
- **Regression:** confirm a Database-default option (e.g. `db.SetDefaultReadSystemKeys`) DOES
  survive reset (re-applied), while a per-txn `SetReadSystemKeys` does NOT.
- Audit existing tests for reliance on per-txn option preservation across reset/retry; fix or
  re-frame any that encode the divergent Go behavior.

## Status / next
Draft. Hold for FDB-C-dev ACK on the persistence partition + the retry-path impact. Torvalds +
codex-review on the impl. The fix closes both the `txn-options-lifecycle` (HIGH) and `snapshot-ryw`
(MEDIUM) findings.
