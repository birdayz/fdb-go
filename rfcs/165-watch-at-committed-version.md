# RFC-165: Register watches at the committed version, not the read version

**Status:** Draft — needs FDB-C-dev design ACK before implementation.
**Item:** FDB client bug-hunt (2026-06-30), finding #1 (HIGH). See `shifts/2026-06-30-fdbgo-client-bughunt.md`.
**Spec:** libfdb_c 7.3.77 (`/tmp/fdbsrc`).

## Problem

`Transaction.Watch` registers the storage-server watch at the transaction's **read version**,
not its **committed version**. When the watching transaction also WRITES the watched key, the
watch fires **spuriously and almost immediately** instead of staying pending until the next
*external* change.

- `WatchSetup` (`readpath.go`) captures `readVersion := tx.readVersion` and the to-watch `value`
  (the RYW-merged value, so for `Set(k,"B")` it captures `"B"`).
- `buildWatchValueRequest` stamps `WatchValueRequest.Version = readVersion` (RV).
- The facade `Watch` (`fdb/transaction.go`) spawns the poll goroutine **eagerly**, inside the
  user's `Transact` body, **before** the wrapper issues the commit — so the watch RPC is sent at
  RV, racing (and beating) the multi-hop commit.

The storage server fires a watch iff `reply.value != metadata->value && latest >= metadata->version`
(`storageserver.actor.cpp:2626`) after `waitForVersionNoTooOld(req.version)` (`:13217`). With Go's
`req.version = RV < CV`, the SS waits only to RV, reads the key at a version in `[RV, CV)` — i.e.
**before the watching txn's own write `B` is visible** — sees the OLD value `A != B`, and fires
immediately.

libfdb_c registers the watch **post-commit at the committed version**:
`Transaction::setupWatches` uses `watchVersion = getCommittedVersion() > 0 ? getCommittedVersion()
: getReadVersion()` (`NativeAPI.actor.cpp:6420`), and `commitAndWatch` runs `commitMutations()`
THEN `setupWatches()` (`NativeAPI.actor.cpp:6909-6918`). So the SS waits until `latest >= CV`,
reads the txn's own write `B == B`, and correctly stays pending until the NEXT external change.

### Blast radius
Only the **self-write-then-watch** pattern is affected: for a watch on a key the txn did NOT
write, value@RV == value@CV (the txn didn't change it), so RV-vs-CV is observationally identical
until an external change. The bug is narrow but real and silent (a watch that fires when it must
not), hence HIGH.

### Repro (single container, differential)
Seed `k="A"` in a SEPARATE committed txn. Then in ONE txn: `Set(k,"B"); w = Watch(k)`; let the
wrapper commit; make NO external change. EXPECTED libfdb_c (cgo): `w` stays **pending**. ACTUAL
Go: `w` fires almost immediately. (Absent-baseline variant: clear `k` first, then `{Set(k,"B");
Watch(k)}` — cgo pending, Go fires.) The differential is the proof: the cgo channel stays empty
for a window, the Go channel receives `nil` (fired).

## Why this is architectural (not a one-line fix)

The watch value `B` is captured correctly (the RYW read). The bug is purely the **registration
version** + **timing**: Go sends the watch RPC at RV before the commit, libfdb_c sends it at CV
after the commit. Matching libfdb_c requires the watch RPC to be **deferred until after the commit
succeeds** and stamped with the **committed version**. That changes the watch lifecycle and how
`Database.Transact` orders commit-vs-watch-activation (libfdb_c's `commitAndWatch`).

## Proposed design (port `commitAndWatch`)

1. **WatchSetup stays synchronous** — keep capturing the RYW value, read conflict, and span (all
   correct today). It no longer determines the final watch version.
2. **Defer the watch RPC to post-commit.** The poll goroutine must not send the watch at RV. It
   waits for a *commit-completion signal* carrying the committed version, then sends the watch at
   that version. Mirror C++ `setupWatches`: `watchVersion = committedVersion > 0 ? committedVersion
   : readVersion` (a watch on a never-committed / read-only txn falls back to RV — but note: in
   FDB a watch only activates on commit, so the read-only case is the empty-commit path).
3. **Commit fires the signal.** On a successful `Commit`, publish `committedVersion` to any pending
   watches set up in this transaction incarnation (a per-incarnation broadcast — reuse the
   `readGen` generation so a watch from a reset-away incarnation is not activated by a later
   commit). On `OnError`/`Reset`/`Cancel`, cancel pending watches (already handled by
   `cancelWatches`, now race-free per round-2).
4. **Facade `Database.Transact`** already commits after `fn`; the watch future's goroutine blocks
   on the commit-completion signal rather than sending eagerly. The `w := tr.Watch(k)` pattern
   inside `Transact` keeps working: `Watch` returns the future immediately, the goroutine activates
   the watch once `Transact` commits.

### Open questions for FDB-C-dev
- **Commit-completion plumbing.** What is the cleanest signal? A per-transaction
  `chan struct{ version int64 }` (or a `sync.Cond`/future) closed by `Commit` and consumed by the
  watch goroutine, keyed by `readGen` to drop stale-incarnation watches. Does this interact badly
  with the auto-reset-after-commit (`postCommitReset`) that already clears `readVersion`?
- **Never-committed watches.** If the user calls `Watch(k)` and then never commits / abandons the
  txn, the watch must not hang forever. C++ activates only via `commitAndWatch`; an abandoned watch
  never fires. Today Go's eager goroutine would send at RV. After the change, the goroutine blocks
  on the commit signal — it must also drain on `Cancel`/`Reset`/ctx-cancel (it does, via watchCtx).
  Confirm a never-committed `Watch` future resolves on the parent ctx, not hangs.
- **Direct `Transaction.Watch` (synchronous client API, `readpath.go:1044`).** `tx.Watch` calls
  WatchSetup then WatchPoll synchronously (no goroutine). For this path the caller is expected to
  commit before/around the watch — the synchronous `tx.Watch` cannot wait for a commit that hasn't
  happened. Likely this path should document that the watch is registered at RV unless a commit
  has already set committedVersion (matching `setupWatches`' ternary), or require commit-first.

## Test plan
- **Differential (single container):** `differential_watch_test.go` — `seed k=A (separate txn);
  {Set(k,B); Watch(k)}; commit; no external change` → assert cgo pending AND go pending for a
  window (the red→green sentinel). Absent-baseline variant. Plus a positive control: an EXTERNAL
  change after commit fires both.
- **Regression:** the existing separate-txn watch tests must stay green (non-self-write watches are
  unaffected). A new test for the self-write-watch + post-commit-external-change ordering.

## Status / next
Draft. Do NOT implement before FDB-C-dev ACKs the commit-completion design (the
`postCommitReset`/generation interaction is the risky part). Torvalds + codex-review on the impl.
