# RFC-281: Read hedging second-request budget

**Status:** Implemented
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
2. **Accrual** (`:687-719`). The loop's last branch sends a request when none of
   the call's requests is outstanding and waits for it alone. When that reply
   arrives before a second request was sent, success or error,
   `secondMultiplier = max(m − DECAY, 1.0)` and `secondBudget = min(b + GROWTH,
   MAX)` (`:694-701`). A transport failure is such a reply: `tryGetReply` completes
   with `request_maybe_delivered` (`fdbrpc/include/fdbrpc/fdbrpc.h:775-794`). A
   failed reply resets `firstRequestData` (`:384-398`), so the next alternative is
   sent through the same branch and its reply accrues too. Nothing else accrues:
   not the replies of a race (`:648-686`), and not the first request's reply when
   the delay spent the budget but no other alternative was available, which the
   `!stream` branch waits for alone (`:640-647`).
3. **Spend** (`:710-716`). When the delay fires first, the second request is sent
   only if `secondBudget >= 1.0`; it then costs 1.0 and raises
   `secondMultiplier` by GROWTH. Without budget the call keeps waiting for the
   first reply, which then accrues. The delay is reset after the first attempt
   (`:732`), so a call spends at most once. (After a paid hedge fails while the
   first request is still outstanding, C++ sends unpaid further second requests
   to the remaining alternatives, `:648-686`. Go waits for the primary only when
   the hedge fails to send; a hedge reply that is a failure Go returns, see Out of
   scope.)
4. **Which reads.** The model is passed only when `enableLocalityLoadBalance`,
   which `Database::createDatabase` sets (`fdbclient/NativeAPI.actor.cpp:2391,2403`):
   `getValue` (`:3673-3687`), `getKey` (`:3817-3824`), `getExactRange` (`:4328-4335`)
   `getRange`, whose request stream also carries `getMappedKeyValues`
   (`:4722-4728`), and `getOverlappingChangeFeeds` (`:11021-11027`), which the Go
   client does not implement. `watchValue` (`:3962-3972`), `waitMetrics`, `getReadHotRanges`,
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
  `spendSecond() bool` (rule 3). The Go-only `backupRequestDelay` (10 ms) and
  `secondMultiplier` (2.0) constants go.
- `sendFrameWithHedge` takes the model. It calls `firstReplied` exactly where
  C++'s last branch accrues: on the reply, or a transport failure that is not the
  caller's cancellation, of the primary before the delay or after a delay that
  found no budget, of a single-replica read, and of the fail-over request sent
  after the primary failed. A lone synchronous send failure also accrues, with
  or without a secondary; a send cancelled by the caller does not. It does not
  accrue on race replies, nor when a paid hedge fails to send and Go waits for
  the primary alone (the `!stream` case).
  When the delay fires it first takes a primary reply that is already queued (C++
  `choose` checks the reply first), then sends the hedge only if `spendSecond`
  allows it. A Go reply timeout or a cancelled caller is not a reply and accrues
  nothing.
- `secondDelay` is computed only when there is a backup. `hedgeResult` reports
  whether the backup was tried, so `getValue`'s sequential fallback no longer
  skips a replica that was never sent the read.
- The `getValue` fallback's single-server read is a lone request; it accrues on
  its reply (rule 2).
- `sendWatch` stops updating the `QueueModel` (rule 4).
- `SetHedgeEnabled`/`HedgeEnabled` stay, default on. On, the client hedges as
  libfdb_c does. Off, no read sends a second request; the budget still accrues,
  so turning hedging back on resumes from it.
- The legacy read-path copy in `request_transcript_legacy_test.go` takes the same
  signatures. Its comparison runs with hedging off, so the shared budget cannot
  make the two paths' requests differ.
- Docs and comments that describe the unbudgeted hedge are corrected: the
  `QueueModel`, `sendFrameWithHedge`, `hedgeEnabled` and `SetHedgeEnabled` comments
  (client and facade), the `pkg/fdbgo/README.md` parity row, the TODO.md budget and
  native-tag fallback flake items (checked off), and the test comments and
  `loadbalance_test.go` expectations built on the 10 ms floor.

Out of scope, recorded in TODO.md by this change:
- Go picks the primary by best-of-two random choices, where C++ takes the lowest
  `smoothOutstanding` and the next-lowest as the second alternative (`:469-552`).
  The delay uses the latencies of the replicas Go picked.
- In a race, Go takes the first reply even when it is a failure C++ retries
  (`process_behind`, `server_overloaded`, a transport failure); C++ keeps waiting
  for the other request and sends unpaid second requests to the remaining
  alternatives (`:648-686`, `checkAndProcessResultImpl` `:325-378`). Lone inline
  retryable errors likewise decode after the hedge helper returns, so they do not
  enter its transport-error fail-over branch. This pre-existing retry gap remains
  tracked separately from second-request budgeting.

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
  on the primary's reply before the delay, on its reply after a delay that found
  budget 0.95 (which then reaches 1.0), on a read with no backup, on a transport
  failure and on the fail-over reply after a failed primary, and on synchronous
  primary/secondary send failures; none on a reply timeout, on cancellation
  (including a cancelled send), in a race, or while
  waiting for the primary after a paid hedge failed to send; the cap at 100 and the
  floor at 1.0; spend at the delay with budget 1.0 (cost 1.0, multiplier +0.01) and
  none with 0.95; with budget 2.0 and both replies slow, exactly one hedge; the
  delay and instant arm values from `secondDelay`; the reply-first order when the
  delay and the reply are both ready.
- Request transcript (real FDB, sim dialer, two aliases of the storage server):
  for `GetValueRequest`, `GetKeyRequest`, `GetKeyValuesRequest` and
  `GetMappedKeyValuesRequest`,
  - a fresh database sends one request per read, with the first reply held
    100 ms, far past the 1.5 ms delay, where the previous code sent two. That no
    hedge is sent does not depend on the hold: the budget is 0. The hold makes the
    delay fire first, the arm the unit tests pin deterministically;
  - with a budget of 1.0 the read sends the hedge, the budget returns to 0 and the
    multiplier to 1.01. The first reply is held until the hedge is recorded, so
    the delay always fires first;
  - with hedging off and a budget of 1.0 the read sends one request and the budget
    grows; turned back on, the next read spends it.
- `getValue` fallback: with no budget and the primary's reply lost, the fallback
  tries the backup, and the backup's reply accrues 0.05.
- Watch: after a watch fires, the server's `QueueModel` entry (latency,
  outstanding) and the budget are unchanged.
- Existing hedge tests (`TestReadTagSamplingOneDrawPerRequest`,
  `TestNativeReadTagSampling`, the hedge unit tests) set up the budget they need
  instead of relying on a fresh database hedging. The fallback spec's budget of
  exactly its two planned hedges bounds its sends structurally.
- The libfdb_c differential and the wire goldens run against one-replica clusters,
  where neither client can hedge, and compare data or idle-cluster requests; a
  hedge count needs two replicas and a reply slower than the delay, which only the
  in-process transcript test produces. libfdb_c's side is the C++ source above.
