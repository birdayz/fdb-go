# RFC-169: Thread isBackward through key-location so backward selectors resolve at shard boundaries

**Status:** Draft — needs FDB-C-dev design ACK + a multi-SS / SimTransport proof strategy.
**Item:** FDB client bug-hunt (2026-06-30), finding #10 (MEDIUM). See `shifts/2026-06-30-fdbgo-client-bughunt.md`.
**Spec:** libfdb_c 7.3.77 (`/tmp/fdbsrc`).

## Problem

A `getKey` with a **backward** selector (`lastLessThan` / `lastLessOrEqual`, `KeySelector.isBackward()
== true`) whose key sits exactly on a **cross-server shard boundary** can **livelock** to
`transaction_too_old (1007)` on the rywDisabled path, where libfdb_c resolves it directly.

C++ `getKey` passes `Reverse{ k.isBackward() }` to `getKeyLocation` (`NativeAPI.actor.cpp:3787-3788`).
For a backward selector at a boundary, `getCachedLocation`/`getKeyLocation_internal` use
`isBackward → rangeContainingKeyBefore` (`:1955`), and the proxy request carries `reverse=isBackward`
(`:3037`); on `wrong_shard_server` it re-invalidates with `Reverse{isBackward}` (`:2022-2029`). So
C++ locates the shard **ending at** the key (the shard that actually owns the resolution).

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
3. Define `isBackward` to match `KeySelectorRef::isBackward()` (offset <= 0).

### Open questions for FDB-C-dev
- Does the Go location cache key on the resolved shard range in a way that lets a backward lookup
  hit `rangeContainingKeyBefore` cleanly, or does the cache need a separate reverse lookup path?
- Is the `GetKeyServerLocations` `reverse` field already on the wire request struct (and just unset),
  or does it need wiring through `buildGetKeyServerLocationsRequest`?
- Confirm the forward path is byte-identical after the change (the `backward=false` default must be
  a no-op).

## Test plan
- **Multi-SS / SimTransport:** stand up ≥2 SS with a boundary at `K` split across servers; write
  data so `K` is an exact boundary. `tr.Options().SetReadYourWritesDisable(); tr.GetKey(lastLessThan(K))`
  → libfdb_c routes to SS_A and returns the greatest key `< K`; pre-fix Go livelocks to 1007,
  post-fix Go resolves correctly. Red→green.
- **Negative control:** the SAME GetKey WITHOUT `SetReadYourWritesDisable` resolves correctly today
  (it goes through the reverse-aware getRange path) — proves the rywDisabled scoping.
- **Forward regression:** the existing single-container getKey differentials stay green
  (`backward=false` is a no-op).

## Status / next
Draft. The livelock is real but requires the multi-SS topology to reproduce, so the proof strategy
(SimTransport vs a real multi-SS testcontainer) is the first thing to settle with FDB-C-dev. Hold
implementation until the locate-infra change + proof are ACKed.
