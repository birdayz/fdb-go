# RFC-271: Connection-failure identity and the location cache range map

**Status:** Implemented

**Items.** Go FDB client findings of the 2026-10-24 quality review (TODO.md section
"Quality review 2026-10-24", added by PR #994, not yet on master; the owner asked for
the closed items to be listed in this PR until #994 merges):

1. High: `handleConnError` closes the pooled connection, not the failed one.
2. Medium: the location cache keeps overlapping stale ranges.
3. Low: malformed location replies are absorbed; `ReadVectorCount` truncates an
   oversized count.

## 1. Connection failures name the connection that failed

**Problem.** `handleConnError(addr)` (`client/topology.go`) closed and evicted
whatever `connPool[addr]` held, then marked `addr` failed after releasing `connMu`.
Its callers passed only the address: the commit and `PendingGet` arms,
`handleDialError`, and `handleReadConnError(addr, err)`, which the read, GRV,
location, metrics, watch and hedged-read arms call (20 call sites,
`git grep -n 'handleReadConnError(' -- 'pkg/fdbgo/client/*.go' ':!*_test.go'` less
the definition). A report can come after the pool replaced the connection it is
about: A dies, `getOrDialConnFor` drops it and pools B (`markAlive`), then a
goroutine still holding A's failed send or reply reports. It closed B, failing
every RPC on B (a commit on B returns `commit_unknown_result`, 1021), and marked
`addr` failed although B had just proved it reachable, excluding a healthy server
from reads for the 4 s re-admission window. Sends wait for their write, and a
pipelined `PendingGet` reports only when resolved, so late reports are ordinary.
Even a timely report could mark `addr` failed after a redial's `markAlive`.

**C++.** Each `Peer`'s `connectionKeeper` holds its connection in a local `conn`;
on failure it closes exactly that object (`fdbrpc/FlowTransport.actor.cpp:989-990`),
publishes the failure (`:924-930`, `:1001`), and only then reconnects; the client
marks the peer available after the new handshake (`:831-833`). Failure and recovery
of one peer are serialized, and NativeAPI never closes connections.

**Change.** `handleConnError(addr string, failed *transport.Conn)` takes the
connection the RPC used (nil for a dial failure, which has none), and
`handleReadConnError(addr, conn, err)` forwards it; the hedged path carries it in
`inFlightRPC`, `hedgeResult` and `rpcAccount`, so a loser's failure names the loser's
connection.

- If the pool holds a different, open connection, the report is superseded:
  nothing is closed and `addr` is not marked failed. The failure counter still
  ticks; the Warn, edge-triggered on the alive-to-failed transition, does not.
- Otherwise the pooled entry (the failed connection, or a closed one) is evicted
  and closed, and `addr` is marked failed.

The decision and `markFailed` share one `connMu` section with `dialAndPool`'s
pool-and-`markAlive`, so neither can overtake the other (`failureMonitor` takes only
its own lock, as `dialAndPool` already relies on). `recordConnFailure` becomes
`reportConnFailure(addr, newlyFailed)`, the counter and Warn, run after the unlock;
`handleConnError` stays the single failure sink. A test seam,
`beforeConnFailureMark` (nil in production), runs just before the mark.

Passing nil instead of the connection would behave the same in every reachable
state, because a transport failure closes its connection before any send or reply
sees the error; passing the pooled replacement would not, and the per-arm tests
catch that. A connection leaves the pool only closed, so a non-pooled `failed`
connection needs no close of its own.

## 2. The location cache is a range map

**Problem.** `insertSorted` (`client/locality.go`) replaced an entry only when its
`begin` matched; other overlaps stayed. After a merge (`[a,b)→S1, [b,c)→S2` refreshed
as `[a,c)→S3`) a lookup of `bb` returned the stale S2. After a split learned from its
upper half (`[a,c)→S1` refreshed with `[b,c)→S2`) a lookup of `a5` reported shard end
`c`, so a range read over `[a,c)` went to S1 whole. `lookupLocked` inspects only the
entry before the search index, which suffices only for disjoint entries.

**C++.** The cache is a `CoalescedKeyRangeMap<Reference<LocationInfo>>`
(`DatabaseContext.h:545`); `setCachedLocation` (`NativeAPI.actor.cpp:2001-2020`) calls
`insert(range, loc)`, which overwrites the range, keeps the value of the range
containing `range.end` from there on, and ignores an empty range
(`KeyRangeMap.h:206-253`, `:209-210`).

**Change.** `insertSorted` keeps a tenant's entries disjoint: an overlapping entry
is trimmed to its parts before `begin` and from `end` on (keeping its servers; a nil
end stays unbounded) or dropped, then the new entry is inserted. Touching entries
keep their bounds; a range with `begin >= end` inserts nothing. Lookups and
invalidations are unchanged.

## 3. Malformed location replies are not cached

**Problem.** `parseGetKeyServerLocationsReply` absorbed decode failures: a pair with
no key range decoded as `begin=nil, end=nil`, which the cache reads as the whole
keyspace; unreadable pairs and servers were skipped; inverted ranges were accepted;
a single-key range without its trailing `\x00` decoded as a wider range.
`ReadVectorCount` capped an oversized count without an error, so parsers read the
bytes after a vector as element offsets.

**C++.** An absent FlatBuffers field loads as its default
(`flat_buffers.h:1041-1046`): an absent key range is `KeyRangeRef()`, empty, and
caching it does nothing (`getKeyRangeLocations_internal` passes every pair to
`setCachedLocation`, `NativeAPI.actor.cpp:3209-3215`). `KeyRangeRef::serialize`
throws `inverted_range` for `begin > end` and asserts the single-key form ends in
`\x00` (`FDBTypes.h:384-399`). Loading does no bounds checks
(`flat_buffers.h:1140-1153`), so out-of-bounds input has no defined result. A reply
that throws while deserializing is never delivered
(`FlowTransport.actor.cpp:1195-1207`).

**Change.**

- `ReadVectorCount` returns a typed `*wire.VectorCountError` for a count that cannot
  fit in the buffer (four bytes per element offset).
- The parser returns a typed `*locationsReplyError` and nothing is cached when the
  results vector, a result, a present key-range slot, a present key's bytes, a
  server vector or a server element does not decode (`locationsUnreadable`), or a
  range is inverted (`locationsInvertedRange`). Comparison uses `bytes.Compare`, so
  an absent end is the empty key, as in C++: `["a", absent)` is inverted.
  `queryLocations` asks the next proxy and does not mark this one failed.
- A result whose range is absent or empty is skipped, as C++ caches nothing for it;
  it covers no key, so no hole results. Results without servers and servers without
  a valid endpoint are skipped as before. A reply left with nothing usable is
  `locationsNoShards`. Range lookups re-ask for a reply with a hole
  (`locateRangeWith`); a single-key reply carries one result (C++ asserts it,
  `NativeAPI.actor.cpp:3043`), so a skipped result leaves none and is re-asked.
- `KeyRangeRef.UnmarshalFromReader` swaps the single-key form only when the key ends
  in `\x00`; otherwise the range stays inverted and is rejected.
- After `stripTenantPrefix`, a reply with an entry left inverted (a boundary without
  the tenant prefix, which the strip keeps where C++ `toPrefixRelativeRange` clamps,
  `FDBTypes.cpp:26-34`) is not used either, so no inverted range is cached or
  returned.

Both decoder changes are shared. The hand-written parsers that call
`ReadVectorCount` (locations, `ClientDBInfo` proxy lists, split points) return its
error; the generated decoders treat it as an absent vector, so an oversized vector
decodes as empty. The client decodes `KeyRangeRef` from a reply only here; the other
carriers are requests it encodes, and the `tools/clientbench` tracers decode
well-formed frames as before.

Divergence, recorded in DIVERGENCES.md: C++ never delivers an undecodable reply, so
its request waits for a proxy change; Go discards it and asks the next proxy.

## Wire compatibility and performance

No bytes change; requests, conflict ranges and error codes are untouched. Every reply
a 7.3.77 server sends decodes as before (ground-truth vector
`GetKeyServerLocationsReply_one_range`). `handleConnError` adds one comparison and
holds `connMu` across `markFailed`; `insertSorted` stays O(log N + K) for K
overlapped entries.

## Tests

Each regression fails with its fix reverted; error cases assert the typed error and
defect.

- `conn_location_unit_test` (fast lane, pipe-backed fake peers):
  - Location then commit: a location query on A sees A die; in the
    `afterLocationSend` seam a commit dials B; the late report must leave B open,
    so the commit gets B's not_committed (1020), not 1021.
  - Every failure arm through its production path (commit, GRV, location,
    WaitMetrics, SplitRange, getValue to one server, load-balanced getValue, getKey,
    getRange, watch, pipelined get), as a reply failure (the request reaches A, the
    pool gets B, A dies) and as a send failure (A's peer reads nothing; when the
    client's second `Write` starts, the pool gets B and A dies under it). B stays
    pooled and open, the address alive, and the failure counter ticks exactly once.
  - `handleConnError` arms (pooled, replaced, dial failure after a redial, pooled
    closed, nothing pooled, report-redial-report) with counter and Warn counts;
    hedge results and losers carrying their own connection; `endOther`.
  - Ordering: the seam asks `connMu.TryLock()`; if the lock is free (mark outside
    it) it completes a redial before the mark, which then fails an open pooled
    connection's address; if held, the redial waits for the lock. No timers.
  - Cache: merge, split, nested, unbounded, touching, empty and inverted inserts,
    tenant isolation, and a randomized check against a per-key model of `insert`.
  - Parser: ground-truth mutations for every defect above, the C++ single-key form,
    and `queryLocations` over two proxies where the first reply is inverted, or
    inverted only after the tenant strip.
  - Oversized counts in the `ClientDBInfo` and split-point parsers.
- `wire_test`, `types_test`: `ReadVectorCount` bounds; a generated decoder
  (`GetReadVersionReply` tag throttles) decodes an oversized vector as empty.
- `client_test` (test-full, real FDB): a `PendingGet` resolved after A died and the
  pool redialed B, for the flush and reply arms.
