# RFC-281: Read hedging second-request budget

**Status:** Draft
**Item:** TODO.md, "Read hedging has no second-request budget" (from #1000).

## Problem

libfdb_c sends a storage read's second request (a hedge) only while its
database-wide `QueueModel` has a second-request budget. A fresh database has
none, so its first 20 reads never hedge, and in steady state at most one read in
21 does. The Go client has no budget: `sendFrameWithHedge` hedges every read that
has a second replica once the reply outlasts max(10 ms, 2 × the primary's last
latency). Nothing bounds how many reads Go hedges: under load, every read whose
reply outlasts the delay sends a duplicate, up to one per read, where libfdb_c's
budget caps duplicates at about one read in 21. Every hedge is storage-server CPU
spent on a duplicate read, which TODO.md §0 ranks first.

## The C++ rules (7.3.77, `/tmp/fdbsrc`)

All in `loadBalance` (`fdbrpc/include/fdbrpc/LoadBalance.actor.h`) with the
`QueueModel` fields `secondMultiplier` (initially 1.0) and `secondBudget`
(initially 0) (`fdbrpc/include/fdbrpc/QueueModel.h:103-104,121`). The knobs are
`FLOW_KNOBS` with no BUGGIFY override (`flow/Knobs.cpp:278-283`):
`INSTANT_SECOND_REQUEST_MULTIPLIER` 2.0, `BASE_SECOND_REQUEST_TIME` 0.0005 s,
`SECOND_REQUEST_MULTIPLIER_GROWTH` 0.01, `SECOND_REQUEST_MULTIPLIER_DECAY`
0.00025, `SECOND_REQUEST_BUDGET_GROWTH` 0.05, `SECOND_REQUEST_MAX_BUDGET` 100.

1. **Delay** (`:554-563`), computed once per `loadBalance` call before the first
   send. `bestTime` and `nextTime` are the last latencies of the chosen first and
   second alternatives. If there is no second alternative (`nextTime` stays 1e9:
   one replica, or every other one failed or in `failedUntil`), there is no second
   request. Otherwise the delay is `secondMultiplier × nextTime +
   BASE_SECOND_REQUEST_TIME`, or zero when `bestTime` exceeds
   `INSTANT_SECOND_REQUEST_MULTIPLIER` times that.
2. **Accrual** (`:692-701`). Whenever a request's reply arrives while it is the
   only one outstanding, success or error, `secondMultiplier = max(m − DECAY,
   1.0)` and `secondBudget = min(b + GROWTH, MAX)`. A transport failure is such a
   reply: `tryGetReply` completes with `request_maybe_delivered`
   (`fdbrpc/include/fdbrpc/fdbrpc.h:775-794`). Every later attempt in the same
   call is a lone request too (`firstRequestData` is reset by
   `checkAndProcessResult`, `:384-398`), so its reply accrues as well.
3. **Spend** (`:710-716`). When the delay fires first, the second request is sent
   only if `secondBudget >= 1.0`; it then costs 1.0 and raises
   `secondMultiplier` by GROWTH. Without budget the call keeps waiting for the
   first reply, which then accrues. Replies that arrive while two requests are
   outstanding change neither field (`:663-689`). The delay is reset after the
   first attempt (`:732`): a call hedges at most once.
4. **Which reads.** The model is passed only when `enableLocalityLoadBalance`,
   which `Database::createDatabase` sets (`fdbclient/NativeAPI.actor.cpp:2391,2403`):
   `getValue` (`:3673-3687`), `getKey` (`:3817-3824`), `getExactRange` (`:4328-4335`)
   and `getRange`, whose request stream also carries `getMappedKeyValues`
   (`:4722-4728`). `watchValue` (`:3962-3972`), `waitMetrics`, `getReadHotRanges`,
   `getRangeSplitPoints` and `splitMetrics` (`:7795-8890`) pass no model and so
   never hedge and never touch the model. Location lookups and GRVs go to proxies
   through `basicLoadBalance`, which has no second request (`LoadBalance.actor.h:746`).
5. **TSS** does not gate hedging. `maybeDuplicateTSSRequest` (`:262-287`) copies
   each request to a storage server's TSS pair independently of the budget. The Go
   client has no TSS support; nothing changes there.

## Go today

`sendGetValue`, `sendGetKey` and `sendRangeRPC` (`getRange`, the exact-range
fallback, mapped ranges and mapped selectors) pick a primary and a backup with
`chooseTopTwo` and call `sendFrameWithHedge` with `secondDelay` = max(10 ms, 2 ×
the primary's latency). Watches, metrics and split points never hedge. The
watch path does record a watch's duration as the server's latency, which C++
does not, and which the C++ delay formula below would turn into instant hedges.

## Fix

- `QueueModel` gains `secondMultiplier` and `secondBudget` under its mutex, with
  the six knobs as constants, and three methods:
  `secondDelay(best, next)` (rule 1), `firstReplied()` (rule 2) and
  `spendSecond() bool` (rule 3).
- `sendFrameWithHedge` takes the model. It calls `firstReplied` for every reply,
  or transport failure that is not the caller's cancellation, of a request that is
  alone in flight: the primary before the delay, the primary after a delay that
  found no budget, a single-replica read, and each fail-over request. When the
  delay fires it first takes a primary reply that is already queued (C++ `choose`
  checks the reply first), then sends the hedge only if `spendSecond` allows it.
  A Go reply timeout or a cancelled caller is not a reply and accrues nothing.
- `secondDelay` is computed only when there is a backup. `hedgeResult` reports
  whether the backup was tried, so `getValue`'s sequential fallback no longer
  skips a replica that was never sent the read.
- The `getValue` fallback's single-server read accrues on its reply (rule 2).
- `sendWatch` stops updating the `QueueModel` (rule 4).
- `SetHedgeEnabled`/`HedgeEnabled` stay, default on. On, the client hedges as
  libfdb_c does. Off, no read sends a second request; the budget still accrues,
  so turning hedging back on resumes from it.

Out of scope, recorded in TODO.md: Go picks the primary by best-of-two random
choices, where C++ takes the lowest `smoothOutstanding` and the next-lowest as
the second alternative (`:469-552`). The delay uses the latencies of the
replicas Go picked.

## Performance

- A fresh database sends no hedges until 20 lone replies have accrued a budget of
  1.0. Afterwards hedges are at most 0.05 per lone reply, about 4.8% of reads,
  with a burst allowance of 100 accrued by quiet periods. Each hedge raises the
  multiplier by 0.01, which the decay of 0.00025 per lone reply returns, so when
  replies often outlast the delay the multiplier settles where about 2.4% of reads
  hedge. Go today has no bound: every read slower than its delay hedges.
- The C++ delay (about 1.5 ms at a fresh model) is shorter than Go's 10 ms floor,
  so a multi-replica cluster with budget hedges a slow read sooner than Go did,
  but never more often than the budget allows.
- The §0 benchmark cluster (`configure new single`, one storage process) has one
  replica per shard; neither client hedges there, so §0 measurements do not move.
- Client CPU: one short critical section on the model mutex per read for
  `firstReplied`; single-replica reads no longer compute a delay.

## Wire compatibility

No new bytes; the requests are unchanged. Only how many duplicates are sent
changes.

## Test plan

- Unit (synctest, the `hedge_timer` seam): accrual of 0.05 and multiplier decay
  on a lone reply, on a transport failure and on each fail-over reply; no accrual
  on a reply timeout, on cancellation or in a race; the cap at 100 and the floor
  at 1.0; spend at the delay with budget 1.0 and none with 0.95; the delay and
  instant arm values from `secondDelay`; the reply-first order when the delay and
  the reply are both ready.
- Request transcript (real FDB, sim dialer, two aliases of the storage server, the
  first reply held past the delay): a fresh database sends one `GetValueRequest`,
  `GetKeyRequest` and `GetKeyValuesRequest` per read, where the previous code sent
  two; with a budget of 1.0 each read sends the hedge and the budget returns to 0.
- `getValue` fallback: with no budget and the primary's reply lost, the fallback
  tries the backup.
- Existing hedge tests (`TestReadTagSamplingOneDrawPerRequest`,
  `TestNativeReadTagSampling`, the hedge unit tests) set up the budget they need
  instead of relying on a fresh database hedging. The fallback spec's budget of
  exactly its two planned hedges bounds its sends structurally.
- The libfdb_c differential and the wire goldens run against one-replica clusters,
  where neither client can hedge, and compare data or idle-cluster requests; a
  hedge count needs two replicas and a reply slower than the delay, which only the
  in-process transcript test produces. libfdb_c's side is the C++ source above.
