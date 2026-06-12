# RFC-096: Enforce database locks on the read path (GRV `locked`)

**Status:** ACCEPTED — FDB-C++-dev ACK (initial + delta after the cached-path redesign);
Torvalds NAK→ACK (the NAK forced the load-bearing redesign: C++'s cache is opt-in, Go's is
always-on, so `locked` must ride the cache or enforcement ~never fires on a warm handle;
his implementation condition — store `lastLocked` only on version-CAS acceptance — is
implemented in `grvCache.updateFromGRV`). Implemented; e2e revert-proven.
**Found by:** the RFC-095 reply ground-truth net — the `GetReadVersionReply_locked`
vector deliberately sets `locked = true` and the asserter could not check it through
the production parser because the parser discards the field.

## Problem

C++ enforces database locks **client-side, per transaction, on every real GRV
reply**:

```cpp
// NativeAPI.actor.cpp:7425-7426 (extractReadVersion)
if (rep.locked && !trState->options.lockAware)
    throw database_locked();
```

Both `LOCK_AWARE` and `READ_LOCK_AWARE` set `trState->options.lockAware`
(`NativeAPI.actor.cpp:7077-7091`), so either option exempts the transaction.

The Go client's `parseGetReadVersionReply` (`grv.go`) **discards `rep.locked`
entirely**. A database locked via the management API (the state used by
backup/restore/DR tooling: `\xff/dbLocked`, `SystemData.cpp:1383`) is silently
readable through the Go client, while C++ and Java refuse with `database_locked`
(1038). Writes are still rejected (the proxy enforces the lock on commit), so the
divergence is read-side only — exactly the half that's invisible without
client-side enforcement.

## C++ semantics to mirror (all verified at 7.3.75)

1. **The check is per-transaction, after the batched reply.** `extractReadVersion`
   runs per `trState`; a single batched GRV reply fans out to many transactions,
   each applying its own `lockAware` exemption.
2. **The shared cache is updated BEFORE the throw.**
   `updateCachedReadVersion(...)` at `:7409` precedes the locked check at `:7425` —
   a locked reply still feeds the version cache.
3. **Cache hits are never lock-checked in C++ — but the C++ cache is OPT-IN.**
   The cached-read-version path (`:7504-7518`) is gated on
   `options.useGrvCache` (`USE_GRV_CACHE`, default **false**, `:6148`), so a
   DEFAULT C++ transaction always goes through `extractReadVersion` and always
   gets lock-checked; the unchecked window exists only for transactions that
   explicitly opted into the cache. Go's `grvCache.tryCache` is **always-on**
   (a pre-existing divergence, filed by this RFC) and the background refresher
   keeps it perpetually warm — an unchecked Go cache path would mean
   enforcement fires roughly once per handle, ever. The emergent C++ property
   to reproduce is "default transactions are always lock-checked", so in Go
   **`locked` rides the cache**: `applyGRVReply` stores it, `tryCache` returns
   it, and the per-transaction check applies on both paths. When the
   cache-gating divergence is closed (cache becomes opt-in like C++), the
   cached-path check can be revisited against C++'s literal shape.
4. `database_locked` (1038) is OnError-retryable (`:7744`) — the Go retry sets
   already include `ErrDatabaseLocked`, so `Run`-loop behavior after the error
   surfaces is already C++-faithful: the retry refetches a GRV and throws again
   until the lock is released or the ctx/retry budget ends.

## Fix (Go, mirroring the structure 1:1)

1. `parseGetReadVersionReply` returns `locked` (a 7th return; the generated
   `GetReadVersionReply.Locked` field already decodes it — pinned by the RFC-095
   vector).
2. `sendGRVRequest` threads `locked` through; `grvResult` gains `locked bool`;
   `flush` fans it out to every waiter unchanged. `applyGRVReply` (the cache
   update) stays **unconditional** — C++ fact 2 — and additionally stores
   `locked` into the cache (`grvCache.lastLocked`, an `atomic.Bool` updated
   together with the version).
3. The per-transaction consumption site (`transaction.go:446`, the
   `extractReadVersion` analog) applies:
   `if locked && !(tx.lockAware || tx.readLockAware) { return ErrDatabaseLocked }`.
4. `grvCache.tryCache` returns the stored `locked` with the version, so cache
   hits flow through the SAME per-transaction check — see C++ fact 3 for why
   this deliberate, documented deviation from C++'s literal (opt-in-cache)
   shape is what actually reproduces C++'s default behavior. A code comment at
   the cache documents this; the always-on-cache divergence itself is filed in
   TODO.md as a separate item.
5. `backgroundRefresher` ignores `locked`. C++'s background GRV updater
   (`NativeAPI.actor.cpp:1283-1318`) is not check-free — it runs a
   non-lock-aware `tr.getReadVersion()` that THROWS 1038 on a locked DB,
   caught by its own `onError` + backoff loop; because the cache update
   (:7409) precedes the throw (:7425), the cache still refreshes each
   attempt. Go's refresher refreshing the cache and discarding `locked` is
   functionally equivalent (cache stays warm, nothing surfaces to users); the
   only delta is C++'s extra backoff pacing while locked, which affects
   refresh cadence, not correctness.

## Test plan

- **E2E (real FDB, DEDICATED container — this is load-bearing):** a database
  lock is GLOBAL, not key-prefix-scoped, and the package's `openTestDB` reuses
  one shared container across all `t.Parallel()` tests — locking it would fail
  every concurrently-running test. The e2e therefore spins its own container
  via the `tcfdb.Run(ctx, "", WithStorageEngine("ssd"), WithDirectIP())` +
  cluster-file pattern from `testmain_test.go` (extracted into a helper, with
  the CLAUDE.md-mandated 2-minute setup timeout). Lock the database exactly as
  C++ `lockDatabase` does (`ManagementAPI.actor.cpp:2471-2489`): under
  ACCESS_SYSTEM_KEYS + LOCK_AWARE, `SetVersionstampedValue` of
  `"0123456789" + UID + \x00\x00\x00\x00` at `\xff/dbLocked` + write-conflict
  range over the normal keyspace. Then, on a **fresh client handle** (empty GRV
  cache — determinism; a warm cache legitimately serves pre-lock versions, C++
  fact 3):
  - plain RAW transaction (no `Transact` retry loop — 1038 is retryable, so a
    run-loop would spin its retry budget; assert the FIRST error) →
    `database_locked` (1038) from the real fetch;
  - a SECOND plain raw transaction immediately after → 1038 again, this time
    via the warm cache (pins the cached-path check — the arm that would
    silently pass with an unchecked cache);
  - `LOCK_AWARE` transaction read → succeeds;
  - `READ_LOCK_AWARE` transaction read → succeeds;
  - unlock (clear `\xff/dbLocked` lock-aware) → POLL until a plain read
    succeeds (bounded): the warm cache legitimately still carries
    `locked=true` until the background refresher's next real fetch stores the
    unlocked reply — same eventual-consistency C++ accepts in the opposite
    direction for its opt-in cache.
- **Reply-vector:** the `GetReadVersionReply_locked` asserter upgrades from the
  wire-layer-only decode to asserting the production parser's new `locked`
  return.
- **Revert-proof:** the e2e's plain-read arm fails (read succeeds) with the
  check removed.

## Wire-compat statement

No bytes change. The GRV request already carries no lock-related flag in 7.3
(the proxy always reports `locked`; enforcement is purely client-side), and the
reply was already decoded — just discarded. This is an error-mapping/behavior
alignment: Go starts returning 1038 exactly where C++ throws it.

## Out of scope (intentionally)

- `metadataVersion` caching (`extractReadVersion` also fulfills a
  metadataVersion promise) — Go has no metadata-version cache at all;
  pre-existing, separate gap.
- Per-flags batcher keying (C++ keys version batchers by `(flags, priority)`,
  Go ORs option bits into one batch per priority) — pre-existing divergence,
  noted for a separate item; does not affect lock enforcement because the check
  is per-transaction, not per-batch.
