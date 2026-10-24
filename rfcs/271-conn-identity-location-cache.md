# RFC-271: Connection-failure identity and the location cache range map

**Status:** Draft

**Items.** Go FDB client findings of the 2026-10-24 quality review (TODO.md section
"Quality review 2026-10-24", added by PR #994, not yet on master):

1. High: `handleConnError` closes the pooled connection, not the failed one; its
   call sites pass only an address, so a late handler can close a fresh connection
   and fail its RPCs (a commit becomes 1021).
2. Medium: the location cache keeps overlapping stale ranges: `insertSorted`
   replaces only an equal `begin`, where C++ `CoalescedKeyRangeMap::insert`
   overwrites the range, and `lookupLocked` checks only `idx-1`.
3. Low: malformed location replies are absorbed (a missing range covers the whole
   keyspace); `ReadVectorCount` truncates an oversized count instead of failing.

## 1. Connection failures name the connection that failed

### Problem

`handleConnError(addr)` (`client/topology.go`) closes and evicts whatever
`connPool[addr]` holds, then marks `addr` failed after releasing `connMu`. Its
callers pass only the address: the commit and `PendingGet` arms directly,
`handleDialError`, and `handleReadConnError(addr, err)`, the wrapper the read, GRV,
location, metrics, watch and hedged-read arms go through (20 call sites:
`git grep -n 'handleReadConnError(' -- 'pkg/fdbgo/client/*.go' ':!*_test.go'`, less
the definition). A report
can arrive after the pool replaced the connection it is about: connection A dies;
`getOrDialConnFor` sees `A.IsClosed()`, drops it, dials B and pools it
(`markAlive(addr)`); then a goroutine holding A's failed reply runs its handler. It
closes B, failing every RPC in flight on B: a commit on B returns
`commit_unknown_result` (1021), and `addr` is marked failed although B's dial just
proved it reachable, so reads exclude a healthy server for the 4 s re-admission
window. A pipelined `PendingGet` makes the late report ordinary: its error reply is
buffered when A dies and handled only when the caller resolves it. Even a report
that is not stale can mark `addr` failed after a redial's `markAlive`, because the
mark happens outside `connMu`.

### C++

There is no address-keyed close in C++. Each `Peer` owns one `connectionKeeper`
actor, which holds its connection in a local `conn`; when that connection fails it
closes exactly that object (`conn->close(); conn = Reference<IConnection>();`,
`fdbrpc/FlowTransport.actor.cpp:989-990`), publishes the failure status
(`:924-930`, `:1001`), and only then reconnects. The client marks the peer
available again after the new handshake (`:831-833`). Failure and recovery of one
peer are serialized in that actor, so a failure of the old connection can never be
recorded after the new connection's recovery. NativeAPI never closes connections.

### Change

`handleConnError(addr string, failed *transport.Conn)` takes the connection the
RPC ran on (nil for a dial failure, which has no connection):

- If the pool holds a different, open connection for `addr`, the report is
  superseded: nothing is closed and `addr` is not marked failed. The failure
  counter still ticks (a connection did fail); the Warn, which is edge-triggered on
  the alive-to-failed transition, does not fire.
- Otherwise the pooled entry (the failed connection, or a closed one) is evicted
  and closed, and `addr` is marked failed.

The decision and `markFailed` run in one `connMu` critical section, the lock under
which `dialAndPool` pools a new connection and calls `markAlive`, so a mark cannot
overtake a redial's recovery and a redial cannot slip between the decision and the
mark. (`failureMonitor` takes only its own lock; `dialAndPool` already nests it.)
`recordConnFailure`, which did the mark, the counter and the Warn after the
unlock, becomes `reportConnFailure(addr, newlyFailed)`: the counter and the
edge-triggered Warn, run after `connMu` is released, so no logging happens under
the pool lock. `handleConnError` stays the single failure sink and the comment
says so.

`handleReadConnError(addr, conn, err)` takes and forwards the connection. Every
call site passes the connection it sent on; the hedged read path carries it in
`inFlightRPC`, `hedgeResult` and `rpcAccount`, so a hedge loser's failure names the
loser's connection. `handleDialError` passes nil: a dial failure with an open
pooled connection means a later dial already succeeded.

A connection that leaves the pool is always closed (by `handleConnError`, by the
dial panic backstop, or because `getOrDialConnFor` drops only closed entries), so
a non-pooled `failed` connection needs no close of its own.

Send-error reports can be late too: a send waits for its write (and, on a cold
peer, for its place in the outbox), and the connection can die while it waits,
after which another caller may pool a replacement. Every report, send or reply,
names the connection its request used. Passing nil at a call site would behave
the same in every reachable state, because a transport failure closes its
connection before any send or reply sees the error; passing the pooled
replacement would not, and the per-arm tests below catch that.

A test seam, `beforeConnFailureMark` (nil in production, like `afterLocationSend`),
runs just before the mark so a test can attempt a redial at that instant.

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
`end` on (each keeps its servers, and a nil end stays unbounded), or dropped when
it lies inside; the new entry is then inserted in order. Entries that only touch
the new range keep their bounds. A range with `begin >= end` inserts nothing; that
covers an empty range and one the tenant prefix strip in `queryLocations` leaves
inverted (`stripTenantPrefix` keeps a boundary without the prefix, where C++
`toPrefixRelativeRange` clamps it, `FDBTypes.cpp:26-34`; only a malformed reply
reaches this). Lookups and
invalidations are unchanged: on disjoint entries the containing entry is the one
they inspect.

## 3. Malformed location replies are not cached

### Problem

`parseGetKeyServerLocationsReply` absorbs decode failures. A pair whose key range
is absent decodes as `begin=nil, end=nil`, which the cache reads as the whole
keyspace, so every lookup routes to that pair's servers. Unreadable pairs and
server entries are skipped silently. Inverted ranges are accepted, and a
single-key range without the trailing `\x00` decodes as a wider range.
`wire.Reader.ReadVectorCount` caps a count larger than the buffer can hold and
returns no error, so the parser reads the following bytes as element offsets.

### C++

FlatBuffers loading gives an absent field its default value
(`flow/include/flow/flat_buffers.h:1041-1046`): an absent key range is
`KeyRangeRef()`, the empty range, and caching an empty range does nothing
(`KeyRangeMap.h:209-210`; `getKeyRangeLocations_internal` passes every pair to
`setCachedLocation`, `NativeAPI.actor.cpp:3209-3215`). `KeyRangeRef::serialize`
throws `inverted_range` for `begin > end` and asserts the single-key form ends in
`\x00` (`FDBTypes.h:384-399`). Loading does no bounds checking (`flat_buffers.h:1140-1153`),
so C++ has no defined result for an offset or count past the buffer. A reply that
throws in deserialization is not delivered: `deliver` traces `ReceiverError` and
rethrows (`FlowTransport.actor.cpp:1195-1207`), and nothing is cached.

### Change

- `ReadVectorCount` returns a typed `*wire.VectorCountError` when the count cannot
  fit in the remaining buffer (four bytes per element offset).
- The parser returns a typed `*locationsReplyError` and caches nothing for a
  reply C++ could not deliver: an unreadable results vector, result, server vector
  or server element (`locationsUnreadable`), or an inverted range
  (`locationsInvertedRange`). `queryLocations` already treats a parse error as a
  failed attempt and asks the next proxy; it does not mark the proxy failed.
- A present key-range slot that does not decode (`ReadNestedReader` fails on a
  present slot: zero or out-of-bounds offset) is `locationsUnreadable`, and so is
  a present key whose bytes `ReadBytes` cannot return (it returns nil for a
  present field with a zero offset or with bytes outside the reply): decoding
  either as empty would widen the range. The range checks compare with `bytes.Compare`,
  so an absent end is the empty key, as C++ decodes it: `["a", absent)` is
  inverted, not unbounded.
- A result whose range is absent or empty is skipped, as C++ caches nothing for
  it; an empty range covers no key, so skipping it leaves no hole. Results without
  servers, and servers without a valid endpoint, are still skipped as today. A
  reply left with no usable shard is `locationsNoShards`, which `queryLocations`
  treats like any parse error. A range lookup re-asks for a reply with a hole
  (`locateRangeWith`). A single-key lookup takes the first usable result as
  before; its reply carries exactly one result (C++ asserts it,
  `NativeAPI.actor.cpp:3043`), so a skipped result leaves none and the lookup is
  asked again.
- `KeyRangeRef.UnmarshalFromReader` performs the single-key swap only when the
  encoded key ends in `\x00`, as C++ asserts; otherwise the range stays inverted
  and is rejected.

Both decoder changes are shared. `ReadVectorCount` is called by the hand-written
reply parsers (locations, `ClientDBInfo` proxy lists in `coordinator.go`, split
points in `metrics.go`), which already return its error, and by the generated
decoders, which treat an error as an absent vector: an oversized vector now
decodes as empty instead of as elements read from the bytes after it. The client
decodes `KeyRangeRef` from a reply only in the locations reply; the other
messages that carry one (`CommitTransactionRef`, `SplitRangeRequest`,
`WaitMetricsRequest`) are requests it encodes. The `tools/clientbench` wire
tracers (`wiretrace`, `wiregolden`) decode captured frames through the same
generated decoders; well-formed frames decode as before.

Divergence (recorded in DIVERGENCES.md): C++ never delivers a reply that fails to
deserialize, so its request waits until the proxies change; Go discards the reply
and asks the next proxy. Only a malformed reply reaches this.

## Wire compatibility

No bytes change. Requests, conflict ranges and error codes are untouched; the
changes are which connection a failure closes, the cache's in-memory shape, and
which replies the client accepts. Every reply a 7.3.77 server sends decodes as
before: a well-formed vector never declares more elements than it holds, and C++
writes the single-key form only with the trailing `\x00` (ground-truth vector
`GetKeyServerLocationsReply_one_range`).

## Performance

`handleConnError` gains one comparison and holds `connMu` across `markFailed`,
which takes only the failure monitor's own lock. `insertSorted` stays
O(log N + K) for K overlapped entries; the common disjoint insert does the same
search and copy as today.

## Test plan

Each regression fails on the pre-fix code and passes after it; the error cases
assert the typed error and its defect, not only that some error occurred.

- Fast lane, new `conn_location_unit_test` (pipe-backed fake peers):
  - Location arm: a location query on A (`afterLocationSend` seam) sees A fail; in
    the seam a commit dials B and the peer signals the commit frame's arrival; the
    seam then returns and the late handler runs. B's peer replies not_committed
    (1020) only after the location query has retried on B (handler done). The
    commit must get 1020, not 1021; B stays pooled and open; `addr` is not failed.
  - `handleConnError` arms: failed connection pooled; replaced by an open one;
    dial failure after a later successful dial; pooled connection closed too;
    nothing pooled; failure then redial then a second report. Each asserts the
    pooled connection, `isFailed`, the counter and the Warn count.
  - Ordering: the `beforeConnFailureMark` seam attempts a redial just before the
    mark. It asks `connMu.TryLock()` whether the pool lock is free: if it is (the
    mark runs outside `connMu`), the seam completes the redial before returning,
    and the mark then leaves an open pooled connection's address failed; if not,
    it starts the redial on a goroutine, which waits for the lock, so its
    `markAlive` lands last. No timer is involved. A repeated concurrent
    report-and-redial checks the same invariant.
  - Every failure arm through its production path (commit, GRV, location,
    WaitMetrics, SplitRange, getValue to one server, load-balanced getValue, getKey,
    getRange, watch, pipelined get with its resolve), twice. Reply arms: the
    request reaches A, the test gives the pool a replacement B, and A dies. Send
    arms: A's peer reads nothing after the handshake, so the request's write
    blocks; the client's `net.Conn` wrapper signals when its second `Write`
    starts (the connect packet is the first), and the test then pools B and kills
    A under the blocked write. Either way the arm's report must leave B pooled
    and open and the address alive, and the failure counter must have ticked
    exactly once, proving the arm reported A rather than never reporting.
  - Hedged path: `sendFrameWithHedge` results and losers carry the connection of
    their own request; `endOther` reports a loser against its connection.
  - Cache: merge, split (upper half and nested), unbounded entries, touching
    neighbours, empty ranges, tenant isolation, and a randomized check against a
    per-key model of `insert`.
  - Parser: mutations of the C++ ground-truth vector for an absent key range, an
    empty range and a server without an endpoint (skipped, `locationsNoShards`);
    a zero key-range offset, a result, server or range-key offset past the buffer,
    oversized result and server counts (`locationsUnreadable`, the last two with
    a `*wire.VectorCountError`); an inverted range, an absent range end and a
    single-key form without `\x00` (`locationsInvertedRange`); and the C++
    single-key form still decoding.
  - `queryLocations` over two proxies: the first answers with an inverted range,
    the second with the valid vector; the query returns and caches the second, and
    the first proxy is not marked failed.
- `wire_test`: `ReadVectorCount` rejects an oversized count and accepts one that
  fits exactly. `types_test`: a generated decoder (`GetReadVersionReply` tag
  throttles) decodes an oversized vector as empty and keeps the other fields.
  `conn_location_unit_test`: the split-point and `ClientDBInfo` proxy-list parsers
  return the `*wire.VectorCountError`.
- `client_test` (test-full, real FDB): a `PendingGet` resolved after its
  connection A died and the pool redialed B, for the flush arm and the
  transport-error reply arm: the value is returned, B stays open, `addr` is not
  failed.
