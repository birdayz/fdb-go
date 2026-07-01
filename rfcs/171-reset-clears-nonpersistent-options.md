# RFC-171: Transaction.reset() must clear non-persistent options (port C++ reset semantics)

**Status:** DESIGN ACK (FDB-C-dev) — ready for implementation. Key resolved point: `onError` PRESERVES
per-txn persistent options; only user `Reset()` reverts them to DB defaults (see CORRECTION below).
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

### CORRECTION (FDB-C-dev design review): `reset()` and `onError` DIVERGE on the persistent set
The earlier draft claimed C++ clears **everything** on BOTH paths. That is wrong for the
**persistent** options. The two paths share `resetRyow()` (`ReadYourWrites.actor.cpp:2699-2727`:
`options.reset(tr)` memsets the NON-persistent RYW option fields, then `applyPersistentOptions()`
re-applies the `persistentOptions` vector) — but they set up that vector **differently**:

- **User `reset()`** (`:2735-2757`): `persistentOptions.clear()` + `sensitivePersistentOptions.clear()`,
  then re-copies **`getTransactionDefaults()`** (the DB-level defaults) into `persistentOptions`, then
  `resetRyow()`. → persistent options revert to **DB defaults**; per-txn `setOption` values are LOST.
- **`onError` retry** (`RYWImpl::onError :1521 → resetRyow()` only): does **NOT** clear
  `persistentOptions`. `applyPersistentOptions()` re-applies the **existing** vector, which still holds
  the user's per-txn persistent `setOption`s. → per-txn `timeout` / `retry_limit` / `max_retry_delay` /
  `auth_token` **SURVIVE** a retry.

So the correct partition is:
- **Non-persistent options** (readSystemKeys, writeSystemKeys, rywDisabled, sizeLimit, priority,
  lockAware, snapshotRYWDisable, tags, …): CLEARED on BOTH paths (via `options.reset(tr)` in
  `resetRyow`).
- **Persistent options** (timeout, retry_limit, max_retry_delay, auth_token): user `reset()` → DB
  defaults; `onError` retry → **preserved per-txn values**.

Go's single shared `reset()` preserves **everything** on both paths — so it is wrong on TWO axes: it
fails to clear the non-persistent set on either path, AND (once fixed) must NOT revert persistent
options to DB defaults on the `onError` path the way it does for user `reset()`.

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

## Why this needs design review (not a quick patch) — RESOLVED, DESIGN ACK
The faithful fix sets the non-persistent option fields back to the **Database defaults**
(`db.TransactionDefaults`) on BOTH paths, and — the subtle part — handles the persistent set
DIFFERENTLY per path (see the CORRECTION above): user `Reset()` reverts persistent options to DB
defaults; the `OnError` retry PRESERVES the per-txn persistent values. Go's single shared `reset()`
therefore must be **split / parameterized**: both paths clear the non-persistent set, but only the
user-`Reset()` path reseeds `persistentOptions` from `getTransactionDefaults()`; the `OnError` path
re-applies the un-cleared per-txn `persistentOptions`. A per-transaction
`SetReadSystemKeys`/`SetSizeLimit`/`SetSnapshotRYWDisable`/priority will **no longer survive a retry**
(non-persistent → cleared, matching C++), but a per-transaction `SetTimeout`/`SetRetryLimit`/
`SetMaxRetryDelay` **WILL** survive a retry (persistent, preserved on `onError`) and revert only on a
user `Reset()`. Existing Go tests/behaviors that rely on blanket preservation must be re-framed.

**Status:** FDB-C-dev DESIGN ACK obtained — the per-path persistent partition above is confirmed
against `ReadYourWrites.actor.cpp:2699-2757` (resetRyow / reset) and `:1521` (RYWImpl::onError →
resetRyow). Proceed to implementation behind the Torvalds + codex + @claude gauntlet.

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
2. **Both paths** reset the non-persistent fields to `db.TransactionDefaults` (the Database-level
   defaults the client already tracks: `SetDefaultReadSystemKeys`, `SetTransactionSizeLimit`, …) —
   mirroring `options.reset(tr)` in `resetRyow()`.
3. **The persistent set diverges per path** (the fix, not a shared behavior):
   - **user `Reset()`** reseeds the persistent options (`timeout`, `retryLimit`, `maxRetryDelay`,
     `auth_token`) from the **DB defaults** — mirroring `persistentOptions.clear()` +
     `getTransactionDefaults()` re-copy (`ReadYourWrites.actor.cpp:2744-2751`).
   - **`OnError` retry** PRESERVES the per-txn persistent values — mirroring `resetRyow()`'s
     `applyPersistentOptions()` over the un-cleared vector (`:2723`). So Go must thread a flag (or
     split into `resetForUser()` vs `resetForRetry()`) rather than route both through one `reset()`.

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
