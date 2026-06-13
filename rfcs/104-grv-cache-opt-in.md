# RFC-104: GRV cache is opt-in (`USE_GRV_CACHE`, default off) — match libfdb_c

**Status:** Accepted — FDB C++ dev + Torvalds + codex ACK on r2 (2026-06-13). §4 resolved to
option (a) (match C++: cached path fail-opens, commit advances `lastTime`, `lastLocked` removed).
**Item:** Client launch-readiness #1 (TODO.md). Closes the "GRV cache is ALWAYS-ON in Go;
opt-in in C++" divergence filed by RFC-096. Gate: `fdb-client-engineer` (FDB C++ dev + Torvalds
+ codex), C++ (libfdb_c 7.3.75) is the spec.

## Problem (a real correctness bug, demonstrated differentially)

The pure-Go client's GRV (get-read-version) cache is **always-on**: `grvBatcher.getReadVersion`
(`grv.go:238-251`) serves `grvCache.tryCache` for every non-`SYSTEM_IMMEDIATE` transaction, and a
background refresher keeps it perpetually warm. The public option that is *supposed* to control
this — `TransactionOptions.SetUseGrvCache()` (`fdb/options.go:120`) — is a **no-op stub**
(`return nil`). So a Go app gets cache behavior it cannot turn off and never asked for.

libfdb_c is the opposite: it serves a cached read version **only** when the app opts in via the
`USE_GRV_CACHE` transaction option, which **defaults off** (`NativeAPI.actor.cpp:6148`; gate at
`:7504-7518`). By default **every libfdb_c transaction issues a fresh proxy GRV.**

**Demonstrated wrong answer** (RFC-098 differential, full-suite run): a Go transaction served a
cached read version *older* than a version a libfdb_c client had already committed → the
committed seed keys were **invisible** to the Go reader (GetKey resolved past them; a limited
GetRange saw 0 of 2 rows). libfdb_c's default (a real GRV per transaction) guarantees external
causality; Go's always-on cache silently does not. The differential currently works around this
by seeding through the Go client (`bench/differential_unreadable_test.go`, getkey subtest
comment) — a workaround that masks the divergence and should be removable when this closes.

This is the whole point of wire/behavioral compat: a Go app and a C app on the same cluster must
see the same data. Today they don't, at the default settings.

## C++ spec (libfdb_c 7.3.75, `/tmp/fdbsrc`)

1. **Options are transaction-scope, default off** (`vexillographer/fdb.options:343/345`):
   - `use_grv_cache` = **1101**: "Allows this transaction to use cached GRV from the database
     context. Defaults to off. Upon first usage, starts a background updater to periodically
     update the cache to avoid stale read versions."
   - `skip_grv_cache` = **1102** (hidden): forces this transaction to bypass the cache; used by
     the background updater itself.
   There is **no database-option variant** and **no propagation** to new transactions
   (`TransactionOptions::clear()` sets `useGrvCache=false`, `NativeAPI.actor.cpp:6148-6149`;
   `reset(Database const&)` does not touch it, `:6157`).
2. **The gate** (`TransactionState::getReadVersion`, `:7504-7518`): the cached version is served
   iff
   ```
   !FORCE_GRV_CACHE_OFF && !options.skipGrvCache
     && (random01() <= DEBUG_USE_GRV_CACHE_CHANCE || options.useGrvCache)
     && rkThrottlingCooledDown(cx, options.priority)
   ```
   then serve `getCachedReadVersion()` **only if** `now() - lastGrvTime <= MAX_VERSION_CACHE_LAG
   (0.1s)` and `rv != 0`; otherwise fall through to the normal proxy GRV path.
3. **Cache population is UNCONDITIONAL** (not gated on the option): every real GRV reply
   (`extractReadVersion`, `:7409`) and **every successful commit** (`:6657`, `t=now()`,
   `v=ci.version`) advance the cache, with `v >= cached` / `t > lastGrvTime` monotonic guards
   (`updateCachedReadVersion`, `:363-383`).
4. **The refresher is lazy and opt-in-driven**: `backgroundGrvUpdater` (`:1283-1320`) is started
   the first time a transaction actually takes the cached path, runs GRVs with `SKIP_GRV_CACHE`
   set, and is cancelled in the `DatabaseContext` dtor (`:1924`). If no transaction ever sets
   `useGrvCache`, **the updater never starts.**
5. **`rkThrottlingCooledDown`** (`:7483-7499`): `IMMEDIATE` is always allowed (never
   cooldown-blocked) but `SYSTEM_IMMEDIATE` bypasses the cache entirely upstream; `BATCH`/`DEFAULT`
   take the cached path only if `now() - lastRk{Batch,Default}ThrottleTime > 60s`.
6. **No lock-aware / `locked` check on the cached path** — `lockAware` never appears in the
   GRV-cache branch. A cached version is served fail-open; the `locked` enforcement
   (`extractReadVersion`, `:7425`) lives only on the fresh-GRV fall-through, which every default
   (cache-off) transaction takes.
7. C++ also requires API version ≥ 720 + shared (multi-version) state to even *accept* the option
   (`:7145-7158`). Go has no multi-version client state; we relax that acceptance gate but match
   the **semantic** (option-gated, default off).

The Go knobs already match (`grv.go:16-21`): `MAX_VERSION_CACHE_LAG=0.1s`,
`MAX_PROXY_CONTACT_LAG=0.2s`, `GRV_CACHE_RK_COOLDOWN=60s`.

## Proposed Go change

1. **Per-transaction option state.** Add `useGrvCache bool` and `skipGrvCache bool` to
   `Transaction` (`transaction.go:153`), default false. Wire the existing stub
   `TransactionOptions.SetUseGrvCache()` to set `useGrvCache=true` on the inner client tx; add
   `SetSkipGrvCache()` (option 1102) setting `skipGrvCache=true` (used by the refresher).
   `SetUseGrvCache()` must **ACCEPT** the option (set the flag, return nil) — never silently no-op
   again, and never error. (C++ throws `invalid_option` only because it requires api≥720 +
   multi-version shared state, `:7147-7149`; Go has no shared-state precondition, so it accepts
   unconditionally while matching the *semantic* — option-gated, default off.) The
   `disable_client_bypass` co-requirement named in the 1101 option doc (`fdb.options:344`) is a
   RYW-layer concern, NOT enforced in the native GRV gate; it is **intentionally not ported** here.
2. **Gate the cache read.** The fast path in `grvBatcher.getReadVersion` (`grv.go:242-251`) must
   consult `tryCache` **only when the calling transaction opted in**:
   `useGrvCache && !skipGrvCache` (in addition to the existing `!isImmediate`, freshness, and
   rk-cooldown checks). Thread the two per-tx bools into `getReadVersion` as **SEPARATE
   parameters, NOT as bits in `flags`** (codex P2): `flags` is serialized onto the wire — `flush`
   ORs `r.flags &^ grvPriorityMask` and `buildGetReadVersionRequest` writes it into
   `GetReadVersionRequest.Flags`. `useGrvCache`/`skipGrvCache` are LOCAL client options, not GRV
   wire flags (unlike `causalReadRisky`/priority, which ARE wire flags); riding them on `flags`
   would put undefined bytes on the wire and break the "wire-compat: none" guarantee below. Pass
   them as their own arguments. When the gate is false → fall through to the proxy GRV batch (the
   current "slow path"). **Default transactions now always issue a fresh GRV**, matching libfdb_c —
   which fixes the demonstrated wrong answer.
3. **Refresher stays lazy** (already `refreshOnce` on first cache hit, `grv.go:245-248`). With the
   gate added, a cache hit only happens for an opted-in transaction, so the refresher naturally
   never starts unless an app opts in — matching C++ `:1283`. Confirm it is joined on
   `db.Close()` via `db.wg` (already `db.wg.Add(1)`); add a stop signal if missing.
4. **Revisit RFC-096 — RESOLVED to option (a), match C++ exactly (FDB C++ dev ruling).** RFC-096
   added `lastLocked` ride-along + a lock-check on the cached path, and deliberately did NOT advance
   `lastTime` on commit, **specifically because the cache was always-on and enforcement-carrying**
   (`grv.go:36-45, 85-96`). With the cache opt-in/default-off, that hazard disappears: every default
   transaction takes the fresh-GRV path and hits the normal `locked` check at the consumption site
   (`transaction.go:561`). The FDB C++ dev ruled to match libfdb_c exactly:
   - **Remove the `lastLocked` lock-check from the cached path** — C++ fail-opens there by contract
     (`:7514-7516` returns `rv` with zero lock inspection; `lockAware` appears only on the fresh
     fall-through at `:7425`). An opted-in transaction accepts the documented staleness, locked flag
     included — exactly as a libfdb_c `USE_GRV_CACHE` app does. Keeping the Go-only enforcement (b)
     would be a behavioral divergence on the opt-in path with no wire benefit.
   - **Restore C++'s commit-advances-`lastTime`** (`:6657`). **Cache POPULATION stays UNCONDITIONAL
     for ALL transactions** (codex P3) — both the GRV-reply update (`:7409`) and the commit update
     (`:6657`) run regardless of the option; only cache *READS* are gated on the opt-in. So a default
     transaction's commit still warms the cache (which a later opted-in transaction may consume).
     Do NOT gate the update path on `useGrvCache`.
   The `lastLocked` field is removed entirely (no consumer after the cached-path check goes away).

## Wire-compat impact

**None.** No bytes change on the wire — keys, records, GRV request/reply frames, conflict ranges,
continuations are all untouched. Option codes 1101/1102 match libfdb_c exactly. The
`useGrvCache`/`skipGrvCache` flags are **local client state, passed as their own parameters and
NEVER serialized into `GetReadVersionRequest.Flags`** (codex P2) — a GRV request frame from an
opted-in Go transaction is byte-identical to one from a default transaction. This is purely a
client read-version-freshness *behavior* + option *semantic* change. The observable effect is that
a default Go transaction now reads a version at least as fresh as libfdb_c's default — strictly
*toward* parity, never away from it.

## Test plan (executable spec)

1. **Differential (the load-bearing proof), `pkg/fdbgo/bench`:** seed a key via **libfdb_c**, then
   read it via a **default** Go transaction — it must be visible immediately (fresh GRV). Pre-fix
   (always-on cache) this can miss the seed; post-fix it cannot. Remove the "seed through the Go
   client" workaround in `differential_unreadable_test.go` and assert cross-client visibility
   directly. Revert-prove (re-enable always-on → differential red).
   **The deterministic seam (Torvalds):** add a `grvCacheHits atomic.Int64` counter, incremented
   exactly once each time `tryCache` returns a hit (the cached path is taken), exposed on the
   `Metrics()` snapshot beside the existing `transactionReadVersionsCompleted` (which already counts
   *real* GRV replies, cache hits excluded — `clientmetrics.go:20,76`). This pair distinguishes the
   three states unambiguously, with NO goroutine-count flakiness: cache gated OFF →
   `grvCacheHits == 0` and `transactionReadVersionsCompleted` advances every txn; cache hit →
   `grvCacheHits` increments; cache stale-but-on → `grvCacheHits == 0` **but** the refresher started
   (distinguishable from gated-off, which never starts it).
2. **Opt-in serves cache vs default does not (FDB integration), via the seam:** two back-to-back
   `SetUseGrvCache()` reads within `MAX_VERSION_CACHE_LAG` → `grvCacheHits >= 1` (the second is a
   hit); N **default** transactions issued **strictly serially** (each committed/closed before the
   next starts, so the batcher cannot coalesce them into one GRV reply — Torvalds) →
   `grvCacheHits == 0` AND `transactionReadVersionsCompleted` advanced by **exactly N** (each took
   its own real GRV) — proving the gate is OFF, not merely that the cache was stale.
3. **Refresher is opt-in:** assert the refresher's `refreshOnce` has NOT fired (a `started
   atomic.Bool` seam on the batcher, not a goroutine count) for a process that only ran default
   transactions; it flips after the first opted-in cached hit.
4. **rk-cooldown + IMMEDIATE bypass** preserved (port the existing `tryCache` throttle tests under
   the new gate).
5. **Locked path:** a default transaction against a locked DB still gets `database_locked` (1038)
   via the fresh-GRV `extractReadVersion` check; the opted-in cached path behaves per the reviewer's
   decision in §4 (pin exactly that). Reuse `TestFDB_DatabaseLocked_ReadPathEnforcement`.
6. `-race` on `//pkg/fdbgo/client`.

## Risks / notes

- **Perf:** today's always-on cache is why Go GRV latency is flat under load. Making it default-off
  means default apps pay a proxy GRV per transaction — *exactly libfdb_c's default cost*. Apps that
  want the old behavior set `SetUseGrvCache()` (now functional). This is correct: we match the C++
  default, and the optimization is available opt-in. Call this out in the PR.
- **RFC-096 interaction** is the one place a reviewer must rule; everything else is a faithful port.
