# RFC-271: Connection-failure identity and the location cache range map

**Status:** Draft

**Items** (TODO.md, "Quality review 2026-10-24", Go FDB client):

1. High: `handleConnError` closes the pooled connection, not the failed one.
2. Medium: the location cache keeps overlapping stale ranges.
3. Low: malformed location replies are absorbed; `ReadVectorCount` truncates an
   oversized count.

## 1. Connection failures name the connection that failed

### Problem

`handleConnError(addr)` (`client/topology.go`) closes and evicts whatever
`connPool[addr]` holds, then marks `addr` failed. Its callers (the read, GRV,
location, metrics, watch and commit arms, and `handleDialError`) pass only the
address. A report can arrive after the pool already replaced the connection it is
about: connection A dies; `getOrDialConnFor` sees `A.IsClosed()`, drops it, dials
B and pools it (`markAlive(addr)`); then a goroutine that was holding A's failed
reply runs its handler. It closes B, failing every RPC in flight on B: a commit on
B returns `commit_unknown_result` (1021), and `addr` is marked failed although B's
dial just proved it reachable, so reads exclude a healthy server for the 4 s
re-admission window. A pipelined `PendingGet` makes the late report ordinary: its
error reply is buffered when A dies and handled only when the caller resolves it.

### C++

There is no address-keyed close in C++. Each `Peer` owns one `connectionKeeper`
actor, which holds its connection in a local `conn`; when that connection fails it
closes exactly that object (`conn->close(); conn = Reference<IConnection>();`,
`fdbrpc/FlowTransport.actor.cpp:989-990`), publishes the failure status
(`:925-930`, `:1001`), and only then reconnects. The client marks the peer
available again after the new handshake (`:831-833`). Failure and recovery of one
peer are serialized in that actor, so a failure of the old connection can never be
recorded after the new connection's recovery. NativeAPI never closes connections.

### Change

`handleConnError(addr string, failed *transport.Conn)` takes the connection the
RPC ran on (nil for a dial failure, which has no connection):

- If the pool holds a different, open connection for `addr`, the report is
  superseded: nothing is closed and `addr` is not marked failed. The counter still
  ticks, since a connection did fail.
- Otherwise the pooled entry (the failed connection, or a closed one) is evicted
  and closed, and `addr` is marked failed, as today.

Every call site passes the connection it sent on; the hedged read path carries it
in `inFlightRPC`, `hedgeResult` and `rpcAccount`. `handleDialError` passes nil: a
dial failure with an open pooled connection means a later dial already succeeded.

A connection that leaves the pool is always closed (by `handleConnError`, by the
dial panic backstop, or because `getOrDialConnFor` drops only closed entries), so
a non-pooled `failed` connection needs no close of its own.

## 2. The location cache is a range map

### Problem

`insertSorted` (`client/locality.go`) replaces an entry only when its `begin`
equals the new one; any other overlap is kept. After a merge
(`[a,b)→S1, [b,c)→S2` refreshed as `[a,c)→S3`) the cache holds `[a,c)→S3` and the
stale `[b,c)→S2`, and a lookup of `bb` returns S2. After a split learned from its
upper half (`[a,c)→S1` refreshed with `[b,c)→S2`) a lookup of `a5` reports shard end
`c`, so a range read over `[a,c)` is sent to S1 whole. `lookupLocked` inspects only
the entry before the search index, which is enough only for disjoint entries.
Stale overlaps cost `wrong_shard_server` round trips and stay until invalidated or
evicted.

### C++

The cache is a `CoalescedKeyRangeMap<Reference<LocationInfo>>`
(`fdbclient/include/fdbclient/DatabaseContext.h:545`). `setCachedLocation`
(`NativeAPI.actor.cpp:2001-2020`) calls `insert(range, loc)`, which erases every
boundary inside the range, keeps the value of the range containing `range.end`
from `range.end` on, and starts the new value at `range.begin`
(`KeyRangeMap.h:206-253`). An empty range is a no-op (`:209-210`).

### Change

`insertSorted` keeps the entries of a tenant disjoint, as `insert` does: an entry
overlapping the new range is trimmed to the part before `begin` and the part from
`end` on (each keeps its servers), or dropped when it lies inside; the new entry
is then inserted in order. An empty range inserts nothing. Lookups and
invalidations are unchanged: on disjoint entries the containing entry is the one
they inspect.

## 3. Malformed location replies are rejected

### Problem

`parseGetKeyServerLocationsReply` absorbs decode failures. A pair whose key range
is absent decodes as `begin=nil, end=nil`, which the cache reads as the whole
keyspace, so every lookup routes to that pair's servers. Unreadable pairs and
server entries are skipped, leaving holes or shortened replica lists. Inverted
ranges are accepted. `wire.Reader.ReadVectorCount` caps a count larger than the
buffer can hold and returns no error, so the parser reads the following bytes as
element offsets.

### C++

`KeyRangeRef::serialize` throws `inverted_range` for `begin > end` and asserts the
single-key form ends in `\x00` (`FDBTypes.h:384-399`). FlatBuffers loading does no
bounds checking (`flow/include/flow/flat_buffers.h:1140-1153`), so C++ has no
defined result for a count past the buffer. A reply that fails to deserialize is
not delivered: `deliver` traces `ReceiverError` and rethrows
(`FlowTransport.actor.cpp:1195-1207`), and nothing is cached.

### Change

- `ReadVectorCount` returns a typed `*wire.VectorCountError` when the count cannot
  fit in the remaining buffer (four bytes per element offset).
- The parser returns a typed error, and caches nothing, when the results vector or
  a pair's server vector is unreadable, a pair or server element cannot be read, a
  pair has no key range, a range is empty or inverted, or a server has no valid
  endpoint. `queryLocations` already treats a parse error as a failed attempt and
  tries the next proxy.
- `KeyRangeRef.UnmarshalFromReader` performs the single-key swap only when the
  encoded key ends in `\x00`, as C++ asserts; otherwise the range stays inverted
  and is rejected.
- A pair with no servers is still skipped: `locateRangeWith` re-asks for a reply
  with a hole.

## Wire compatibility

No bytes change. Requests, conflict ranges and error codes are untouched; the
changes are which connection a failure closes, the cache's in-memory shape, and
which replies the client accepts. Every reply a 7.3.77 proxy sends decodes as
before (ground-truth vector `GetKeyServerLocationsReply_one_range`).

## Performance

`handleConnError` gains one comparison under `connMu`. `insertSorted` stays
O(log N + K) for K overlapped entries; the common disjoint insert does the same
search and copy as today.

## Test plan

Each regression fails on the pre-fix code and passes after it.

- Fast lane, `native_range_unit_test`: a location lookup on connection A
  (`afterLocationSend` seam) sees A fail; before its handler runs, a commit dials B
  and waits on B. The late handler must leave B open: the commit receives B's
  reply (not_committed, 1020) instead of 1021, and `addr` is not marked failed.
  Unit tests drive every arm of `handleConnError`.
- Fast lane, new `location_cache_unit_test`: merge and split inserts (lookups
  return the merged servers and the trimmed shard bounds), a range nested inside
  an entry, empty ranges, tenant isolation; malformed replies built by mutating
  the C++ ground-truth vector (absent key range, oversized pair and server counts,
  inverted range, single-key form without `\x00`) are rejected and cache nothing.
- `wire_test`: `ReadVectorCount` rejects an oversized count and accepts one that
  fits exactly.
