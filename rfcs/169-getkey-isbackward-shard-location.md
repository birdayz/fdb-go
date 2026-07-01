# RFC-169: Thread isBackward through key-location so backward selectors resolve at shard boundaries

**Status:** Design ACK'd by FDB-C-dev (2026-07-01) against 7.3.77. Implementation already landed
(commit `6b72427bc`, finding #10). Remaining: end-to-end livelock proof + this doc's corrections.
**Item:** FDB client bug-hunt (2026-06-30), finding #10 (MEDIUM). See `shifts/2026-06-30-fdbgo-client-bughunt.md`.
**Spec:** libfdb_c 7.3.77 (`/tmp/fdbsrc`).

## Problem

A `getKey` with a **backward** selector — `lastLessThan(k) = (k, orEqual=false, offset=0)`, the only
selector with `KeySelectorRef::isBackward() == true` — whose key sits exactly on a **cross-server
shard boundary** can **livelock** to `transaction_too_old (1007)` on the rywDisabled path, where
libfdb_c resolves it directly. (`isBackward() == !orEqual && offset <= 0`, FDBTypes.h:659-661.
`lastLessOrEqual(k) = (k, orEqual=true, offset=0)` is **NOT** backward — `orEqual=true` → false,
FDBTypes.h:683 — so it does not hit this path.)

C++ `getKey` passes `Reverse{ k.isBackward() }` to `getKeyLocation` (`NativeAPI.actor.cpp:3787-3788`).
For a backward selector at a boundary, `getCachedLocation` uses
`isBackward ? rangeContainingKeyBefore : rangeContaining` (`:1955`; cache check at `:3107`, refresh
`getKeyLocation_internal` at `:3008`), and the proxy `GetKeyServerLocationsRequest.reverse`
(`CommitProxyInterface.h:447`, serialized pos-4 `:472`) is set `= isBackward` (`:3037`); on
`wrong_shard_server` it re-invalidates with `invalidateCache(prefix, k.getKey(), Reverse{isBackward})`
(`:3845-3848`, `invalidateCache` def `:2022-2034`). So C++ locates the shard **ending at** the key
(the shard that actually owns the resolution).

Go's `getKeyImpl` locate (`readpath.go:179`) calls `locCache.locate(key, …)` with **no backward
flag**; `lookupLocked` and `buildGetKeyServerLocationsRequest` always use `rangeContaining(key)` /
`Reverse=false` (`locality.go`). So for key `K` on the boundary of shard `A=[P,K)` (on SS_A) and
`B=[K,Q)` (on SS_B), Go locates `B`; the SS owning `B` runs `getShardKeyRange` with
`sel.isBackward() → rangeContainingKeyBefore(K)`, which is **not readable on B's server**, and
returns `wrong_shard_server` (`storageserver.actor.cpp:4503-4507`). Go invalidates `K` and
re-locates `K` **without** reverse → `B` again → `wrong_shard` again, looping until
`shardRetries > MaxWrongShardRetries` → `ErrTransactionTooOld (1007)`, which the `Transact` loop
retries indefinitely (livelock). libfdb_c locates `A` directly and resolves correctly.

### Scope
Affects the **rywDisabled** getKey path (the RYW-enabled path resolves against the cache, not a
single storage locate). Only manifests with a **real multi-SS topology** where the boundary splits
ownership — NOT on a single-process testcontainer (one SS owns both shards, so
`rangeContainingKeyBefore` lands on the readable shard regardless of client routing).

## Why this needs design review
The fix threads an `isBackward`/`Reverse` flag through the location-cache + the
`GetKeyServerLocations` request, touching shared locate infrastructure (`locality.go`
`locate`/`lookupLocked`/`buildGetKeyServerLocationsRequest`, the location-cache lookup, and the
wrong-shard re-locate in `readpath.go`). It must not perturb the (correct) forward path. And it
cannot be proven on the single-container differential harness — it needs a multi-SS topology or a
`SimTransport` scenario that splits ownership at the boundary.

## Proposed design (port the C++ Reverse threading)
1. `locality.go`: `locate(db, ctx, key, …)` and `lookupLocked` take a `backward bool`; when true,
   resolve against `rangeContainingKeyBefore(key)` (the shard ending at `key`) instead of
   `rangeContaining(key)`. `buildGetKeyServerLocationsRequest` stamps `reverse = backward`
   (NativeAPI:3037).
2. `readpath.go getKeyImpl`: pass `isBackward(orEqual, offset)` (the selector's backward predicate)
   to `locate`, and on `wrong_shard_server` re-invalidate/re-locate **with the same backward flag**
   (NativeAPI:2022-2029) — not the current forward re-locate that loops.
3. Define `isBackward` to match `KeySelectorRef::isBackward()` = **`!orEqual && offset <= 0`**
   (FDBTypes.h:659-661) — NOT `offset <= 0` alone. Getting this wrong (tagging `lastLessOrEqual`
   backward) would MIRROR the bug: the client routes to `rangeContainingKeyBefore` but the SS, whose
   real `isBackward()` is false, uses `rangeContaining` (storageserver.actor.cpp:4504) → wrong_shard.
   The implementation already uses the correct predicate (`readpath.go:183`).

### Open questions for FDB-C-dev — RESOLVED (2026-07-01, against 7.3.77)
- **Cache lookup vs request stamp:** BOTH are required. C++ uses one `locationCache`; only the lookup
  selector differs (`isBackward ? rangeContainingKeyBefore : rangeContaining`, NativeAPI:1955).
  Stamping the request `reverse` alone is insufficient — the cache *lookup* must also pick the
  shard-ending-at-key, else a correctly-fetched shard-A entry is missed by `rangeContaining(K)`. The
  impl does both (`lookupLocked` backward branch + `buildGetKeyServerLocationsRequest` reverse).
- **Wire field:** `GetKeyServerLocationsRequest.reverse` is a real serialized field (CommitProxyInterface.h:447,
  pos-4 :472); it was always false pre-fix and is now stamped from the backward flag.
- **Forward no-op:** confirmed byte-identical — `backward=false` → `rangeContaining`, `reverse=false`
  request, forward invalidate; `getValue` hardcodes `Reverse::False`.

## Test plan — as-built (the deterministic proof) + why an end-to-end multi-SS test is rejected
The livelock's *routing effect is only observable on a multi-SS topology*, which cannot be made
deterministic (data-distribution decides shard→SS placement; there is no `moveKeys`/special-key hook to
pin a boundary at a chosen key; under double redundancy adjacent shards' replica sets always overlap, so
the forward-routed SS usually also owns the backward shard and never returns wrong_shard). The symptom
is a *wall-clock livelock* (50×`wrongShardRetryDelay` → 1007 → `Transact` retries). A real multi-SS
regression would therefore be **flaky** (forbidden) and could only be made to pass by **skipping** when
no cross-SS boundary is found (also forbidden — CLAUDE.md allows only the Docker skip). The single-SS
SimTransport can reproduce the 1007 *symptom* but not a valid red→green: it proxies one real SS, so the
post-fix backward locate returns that same SS and the fix has no distinct correct target to resolve
against. So there is **no faithful deterministic end-to-end test** — the proof is the decomposition:

- **Backward lookup routing** (`TestLookupLocked_BackwardSelectorOnBoundary`): backward `m` on
  `[a,m)@SSA + [m,z)@SSB` → SSA (shard ending at m); forward `m` → SSB. Revert-proven.
- **Backward invalidate** (`TestInvalidate_BackwardSelectorEvictsShardEndingAtKey`): wrong-shard
  invalidate for a backward selector evicts the shard ending at the boundary, so the retry doesn't
  re-hit the stale shard. Revert-proven.
- **System-key clamp** (`TestLocate_SystemKeyClampIgnoresBackward`, codex #10 P2): a backward selector
  on `allKeysEnd` clamps to the `0xff` sentinel and forces forward → routes to the SYS shard, not the
  last USER shard. Revert-proven.
- **Request stamp** (`TestBuildGetKeyServerLocationsRequest_ReverseField`): backward → `Reverse=true`.
- **The predicate** (`TestKeySelectorIsBackward`, RFC-169/FDB-C-dev): `keySelectorIsBackward` =
  `!orEqual && offset <= 0`; `lastLessThan` backward, `lastLessOrEqual` NOT. Revert-proven (dropping
  `!orEqual` reds the `lastLessOrEqual` case — the exact mirror-bug FDB-C-dev flagged).

Collectively these pin every C++-mandated decision the getKey livelock loop depends on
(`readpath.go` locate + invalidate with the backward flag). The `locate(…, isBackward)` /
`invalidate(…, isBackward)` call-site arguments in `readpath.go` are the one line whose *effect* is only
observable on multi-SS; it is gated by FDB-C-dev review (verified faithful vs 7.3.77), which is the
correct gate for a line no deterministic test can cover.

## Status / next
**Implemented (`6b72427bc` + review `0af735069`, finding #10) and design-ACK'd by FDB-C-dev against
7.3.77.** The predicate is now a named, unit-pinned `keySelectorIsBackward` seam. No open work — the
deterministic proof is the five decomposed unit tests above; a multi-SS end-to-end test is deliberately
NOT added (would be flaky / require a forbidden skip).
