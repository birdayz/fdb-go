# RFC-271: Connection-failure identity and the location cache range map

**Status:** Implemented

**Items** (TODO.md, "Quality review 2026-10-24", Go FDB client): `handleConnError`
closes the pooled connection, not the failed one (High); the location cache keeps
overlapping stale ranges (Medium); malformed location replies are absorbed and
`ReadVectorCount` truncates an oversized count (Low).

## 1. Connection failures name the connection that failed

**Problem.** `handleConnError(addr)` (`client/topology.go`) closed whatever
`connPool[addr]` held and marked `addr` failed after releasing `connMu`. Its callers
(commit, `PendingGet`, `handleDialError`, and the 20 `handleReadConnError` call
sites of the read, GRV, location, metrics, watch and hedged arms) passed only the
address. A report can come after the pool replaced its connection: A dies,
`getOrDialConnFor` pools B (`markAlive`), then a goroutine holding A's failed send
or reply reports, closing B. Every RPC on B fails (a commit returns 1021) and the
reachable address is excluded from reads for 4 s. Sends wait for their write and a
`PendingGet` reports only when resolved, so late reports are ordinary.

**C++.** A `Peer`'s `connectionKeeper` holds its connection in a local `conn`,
closes exactly that object on failure (`fdbrpc/FlowTransport.actor.cpp:989-990`),
publishes the failure (`:924-930`, `:1001`) and only then reconnects; the client
marks the peer available after the new handshake (`:831-833`).

**Change.** `handleConnError(addr, failed *transport.Conn)` takes the connection the
RPC used (nil for a dial failure) and `handleReadConnError(addr, conn, err)`
forwards it; the hedged path carries it in `inFlightRPC`, `hedgeResult` and
`rpcAccount`. If the pool holds a different open connection, the report is
superseded: nothing closes, `addr` is not marked failed, only the counter ticks.
Otherwise the pooled entry is evicted and closed and `addr` marked failed. The
decision and `markFailed` share a `connMu` section with `dialAndPool`'s
pool-and-`markAlive`; the counter and edge-triggered Warn (`reportConnFailure`) run
after the unlock. A seam, `beforeConnFailureMark` (nil in production), runs just
before the mark. Passing nil at a call site would behave the same, because a
transport failure closes its connection before anyone sees the error; passing the
pooled replacement would not, which the per-arm tests catch. The eager sends
(commit, pipelined get) return once their frame is queued, so their send-failure
reports fire only for a connection closed before they send; no test reaches that
instant without a seam, and both sites pass the `conn` they sent on.

## 2. The location cache is a range map

**Problem.** `insertSorted` (`client/locality.go`) replaced only an entry with the
same `begin`. After a merge (`[a,b)→S1, [b,c)→S2` refreshed as `[a,c)→S3`) a lookup
of `bb` returned the stale S2; after a split learned from its upper half
(`[a,c)→S1` then `[b,c)→S2`) a lookup of `a5` reported shard end `c`.
`lookupLocked` inspects one neighbour, which suffices only for disjoint entries.

**C++.** `setCachedLocation` (`NativeAPI.actor.cpp:2001-2020`) calls
`CoalescedKeyRangeMap::insert` (`DatabaseContext.h:545`, `KeyRangeMap.h:206-253`):
the new range overwrites what it overlaps, the rest keeps its value, an empty range
is ignored (`:209-210`).

**Change.** `insertSorted` trims an overlapping entry to its parts before `begin`
and from `end` on (keeping its servers; a nil end stays unbounded) or drops it, then
inserts. Touching entries keep their bounds; `begin >= end` inserts nothing.

## 3. Malformed location replies are not cached

**Problem.** `parseGetKeyServerLocationsReply` decoded a pair with no key range as
`[nil, nil)`, which the cache reads as the whole keyspace, skipped unreadable
elements, and accepted inverted ranges and a single-key form without its trailing
`\x00`. `ReadVectorCount` capped an oversized count without an error.

**C++.** An absent field loads as its default (`flat_buffers.h:1041-1046`), so an
absent range is the empty `KeyRangeRef()`, which caching ignores
(`NativeAPI.actor.cpp:3209-3215`). `KeyRangeRef::serialize` asserts the single-key
form's `\x00` and throws `inverted_range` for `begin > end` (`FDBTypes.h:384-399`).
Loading has no bounds checks (`flat_buffers.h:1140-1153`). A reply that throws
while deserializing is never delivered (`FlowTransport.actor.cpp:1195-1207`).

**Change.**

- `ReadVectorCount` returns `*wire.VectorCountError` for a count the buffer cannot
  hold (four bytes per offset). Hand-written parsers return it; generated decoders
  treat it as an absent vector, so an oversized vector decodes as empty.
- The parser returns a typed `*locationsReplyError` (`locationsUnreadable`,
  `locationsInvertedRange`, `locationsNoShards`) and nothing is cached when a
  vector, element, present key-range slot or present key does not decode, or a
  range is inverted (an absent end compares as the empty key, as in C++).
  `queryLocations` logs it at Debug and asks the next proxy, without marking this
  one failed.
- Results with an absent or empty range, results without servers and servers
  without an endpoint are skipped; an empty range covers no key. Range lookups
  re-ask for a hole (`locateRangeWith`); a single-key reply has one result
  (`NativeAPI.actor.cpp:3043`), so a skip leaves none and is re-asked.
- `KeyRangeRef.UnmarshalFromReader` swaps the single-key form only when the key ends
  in `\x00`. The client decodes `KeyRangeRef` from no other reply.
- A reply that `stripTenantPrefix` leaves with an inverted entry (a boundary without
  the tenant prefix, which C++ `toPrefixRelativeRange` clamps, `FDBTypes.cpp:26-34`)
  is not used.

Three divergences are recorded in DIVERGENCES.md: Go asks the next proxy where C++
never delivers an undecodable reply, and keeps an unprefixed tenant boundary where
C++ clamps it (only malformed replies reach these two); and Go skips a shard with no
servers, which C++ caches as an empty location (unchanged by this RFC).

## Wire compatibility and performance

No bytes change. Every reply a 7.3.77 server sends decodes as before (ground-truth
vector `GetKeyServerLocationsReply_one_range`). `handleConnError` adds a comparison
and holds `connMu` across `markFailed`; `insertSorted` stays O(log N + K).

## Tests

Each regression fails with its fix reverted.

- `conn_location_unit_test` (fast lane, pipe-backed peers): a late location report
  leaves the commit on B its 1020; every failure arm (commit, GRV, location,
  WaitMetrics, SplitRange, getValue, load-balanced getValue, getKey, getRange,
  watch, pipelined get) as a reply failure, and every waiting send (all but commit
  and pipelined get) as a send failure blocked on A, with B kept, the address alive
  and exactly one report counted; every
  `handleConnError` arm; the mark-versus-redial ordering through the seam
  (`connMu.TryLock` decides, no timers); hedge connections and `endOther`; cache
  merge, split, nested, unbounded, touching, empty, inverted and tenant cases plus a
  randomized model of `insert`; every parser defect on mutations of the C++ vector;
  `queryLocations` skipping a bad proxy, including a tenant-strip inversion; the
  `ClientDBInfo` and split-point count errors.
- `wire_test`, `types_test`: `ReadVectorCount` bounds; a generated decoder drops an
  oversized vector.
- `client_test` (test-full, real FDB): a `PendingGet` resolved after A died and the
  pool redialed B, flush and reply arms.
