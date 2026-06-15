# Native fdb-go Client Bug Findings

> **STATUS — HISTORICAL AUDIT, NEARLY ALL RESOLVED (do not read as the current bug list).**
> This is the **2026-06-01** source audit that *bootstrapped* the client's hardening. The
> overwhelming majority of its findings — including **every High-severity item (1–7)** — have since
> been fixed and pinned with regression tests (real-FDB / differential / fault-injection). For
> example: #1 embedded reply-errors, #2 `wrong_shard_server` = 1001, #3 pipelined-read semantics,
> #4 tenant-commit mutation aliasing, #5 hedge QueueModel accounting, #6 connection-shutdown
> stranding, #7 transaction concurrency, #8 shape-based `ErrorOr`, #11 TLS, #12 empty-location
> panic, #16 GRV-cache throttle cooldown — all addressed. The **authoritative current state** of the
> client is `rfcs/prod-readiness-go-client.md` (the production-readiness assessment) and its
> punch-list (RFC-111/RFC-112 etc.); a reader wanting "what's left" should start there, NOT here.
> The detailed findings below are retained as the historical record of the original audit.

Review date: 2026-06-01 (historical — see status banner above)

Scope: `pkg/fdbgo/client`, `pkg/fdbgo/fdb`, `pkg/fdbgo/transport`, and `pkg/fdbgo/wire`.

Severity guide:

- High: can return wrong data/errors, skip required retries, corrupt transaction state, or hang goroutines.
- Medium: breaks a public feature or important edge case, but usually needs a specific option/path.
- Low: performance leak, misleading behavior, or latent bug that should still be fixed.

## Findings

### 1. High - Read reply parsers ignore embedded storage-server errors

Evidence:

- `pkg/fdbgo/client/readpath.go:196` parses `GetKeyReply` manually and never checks `HasError`.
- `pkg/fdbgo/client/readpath.go:656` parses `GetKeyValuesReply` and returns `Data/More` without checking `HasError`.
- `pkg/fdbgo/client/readpath.go:708` parses `GetValueReply` and treats missing `HasValue` as a missing key, even if `HasError` is set.
- Generated reply structs explicitly contain optional error fields: `GetValueReply.HasError/Error`, `GetKeyReply.HasError/Error`, and `GetKeyValuesReply.HasError/Error`.

Impact:

Storage-server errors such as `wrong_shard_server`, `future_version`, or `process_behind` can be lost or converted into misleading results. Point reads can look like "key not found"; range reads can look empty; key selector reads can become generic `read KeySelector` errors. This also prevents the read path from invalidating the location cache and retrying the correct shard.

Fix notes:

- After unmarshalling each reply, check `HasError` before reading success fields.
- Decode the nested `Error` payload into `*wire.FDBError`.
- Add unit tests for embedded errors on Get/GetKey/GetRange, including wrong-shard and future-version cases.

### 2. High - `wrong_shard_server` is assigned the wrong error code

Evidence:

- `pkg/fdbgo/client/transaction.go:29` defines `ErrWrongShardServer = 1062`.
- `pkg/fdbgo/fdb/error.go:52` maps `1001` to `wrong_shard_server`.
- `pkg/fdbgo/fdb/error.go:101` and `pkg/fdbgo/wire/reader.go:532` map `1062` to `change_feed_cancelled`.
- `pkg/fdbgo/wire/fdberror_test.go:92` explicitly says `1062` was incorrectly used for `wrong_shard_server` and that the real code is `1001`.
- `pkg/fdbgo/client/readpath_unit_test.go:30` still tests the wrong `1062` assumption.

Impact:

Actual wrong-shard replies will not trigger location-cache invalidation and local retry. Conversely, a real `change_feed_cancelled` error would be treated as wrong-shard in client read paths.

Fix notes:

- Change `ErrWrongShardServer` to `1001`.
- Update read-path tests and fault injection tests to use `1001`.
- Keep a regression test proving `1062` is not considered wrong-shard.

### 3. High - Public `fdb.Transaction.Get` pipelining bypasses normal read semantics

Evidence:

- `pkg/fdbgo/fdb/transaction.go:39` always tries `inner.GetPipelined` first.
- `pkg/fdbgo/client/transaction.go:436` implements `GetPipelined` separately from the normal `getValue` path.
- `pkg/fdbgo/client/readpath.go:225` has the normal wrong-shard retry loop, but `GetPipelined` does not use it.
- `pkg/fdbgo/client/transaction.go:506` resolves a pipelined get without location-cache invalidation, hedging, QueueModel updates, or normal retry behavior.
- `pkg/fdbgo/fdb/transaction.go:53` says the fallback is for `ErrNeedFullRYW`, but the code falls back on every send-time error.

Impact:

The public API's most common point-read path can skip wrong-shard retry, skip storage-server load balancing/hedging, ignore `Flush()` errors, and return response-time storage errors directly to callers. It also lacks the legal key range check present in `Transaction.Get`, so illegal system/special keys can be sent before they are rejected.

Fix notes:

- Make `PendingGet.Resolve` share the same parse/retry/cache-invalidation behavior as `getValue`, or restrict pipelining to cases where it preserves those semantics.
- Check legal key range before sending.
- Fall back from `GetPipelined` only for `ErrNeedFullRYW`, not arbitrary locate/send failures.
- Add tests for response-time wrong-shard, illegal key, closed connection during `Flush`, and QueueModel accounting.

### 4. High - Tenant commit marshalling mutates buffered transaction mutations

Evidence:

- `pkg/fdbgo/client/commitpath.go:267` unsafe-casts `tx.mutations` to `[]types.MutationRef`.
- `pkg/fdbgo/client/commitpath.go:286` then rewrites `m.Param1` and sometimes `m.Param2` while applying the tenant prefix.
- For versionstamped keys, `pkg/fdbgo/client/commitpath.go:297` also adjusts the versionstamp offset in the same aliased mutation.

Impact:

Building a tenant commit request changes `tx.mutations` in place. Rebuilding the request can double-prefix keys and double-adjust versionstamp offsets. A send/proxy failure after marshalling leaves the transaction's local mutation buffer corrupted until reset.

Fix notes:

- Keep the unsafe cast only for the no-tenant path.
- For tenant commits, build a scratch `[]types.MutationRef` with copied/prefixed slice headers.
- Add a unit test that calls `buildCommitTransactionRequest` twice on the same tenant transaction and asserts that the serialized keys are not double-prefixed and `tx.mutations` remains unmodified.

### 5. High - Hedged reads leak QueueModel outstanding accounting

Evidence:

- Primary/secondary read senders call `queueModel.startRequest` in `pkg/fdbgo/client/readpath.go:145`, `pkg/fdbgo/client/readpath.go:280`, and `pkg/fdbgo/client/readpath.go:559`.
- `pkg/fdbgo/client/hedge.go:120` races two replies, but only returns the winner's `addr/delta/start`.
- `pkg/fdbgo/client/hedge.go:126` and `pkg/fdbgo/client/hedge.go:132` cancel the loser without returning enough information for `queueModel.endRequest`.
- Timeout/cancel paths in `pkg/fdbgo/client/hedge.go:135` and `pkg/fdbgo/client/hedge.go:141` return no address/delta for either in-flight request.
- QueueModel requires every start delta to be subtracted in `pkg/fdbgo/client/loadbalance.go:254`.

Impact:

Every hedged loser can permanently inflate `smoothOutstanding` for that server. Under load, server selection will become increasingly biased by phantom outstanding work.

Fix notes:

- Return per-RPC cleanup data for both winner and loser, or let `sendFrameWithHedge` call QueueModel cleanup through callbacks.
- On timeout/cancel, decrement both started requests.
- Add a deterministic hedge unit test that asserts outstanding returns to baseline for winner, loser, timeout, and context-cancel paths.

### 6. High - Connection shutdown can strand senders and leak goroutines

Evidence:

- `pkg/fdbgo/transport/conn.go:286` enqueues sync writes and waits on `errCh` at `pkg/fdbgo/transport/conn.go:293`.
- `pkg/fdbgo/transport/conn.go:319` does the same for `Flush`.
- `pkg/fdbgo/transport/conn.go:349` lets `writeLoop` exit immediately on `c.ctx.Done()` without notifying queued `errCh` channels.
- `pkg/fdbgo/transport/conn.go:670` has `connectionMonitor` call `c.cancel()` and return, but it does not close the socket.
- `pkg/fdbgo/transport/conn.go:473` can remain blocked in `ReadFrame` until the socket closes or bytes arrive.

Impact:

If close/cancel races with a queued `SendFrame` or `Flush`, the caller can block forever waiting for an `errCh` send that will never happen. When the monitor marks a connection dead, the read loop can remain blocked on the TCP connection and pending replies may only wake by RPC timeout.

Fix notes:

- Add a single `failConnection(err)` path that cancels context, closes the socket, fails all pending replies, and drains queued sync write requests with an error.
- Have `Close`, `readLoop` error handling, and `connectionMonitor` use that path.
- Add tests for `Close` racing `SendFrame`/`Flush` and monitor-driven dead connection cleanup.

### 7. High - Public transaction concurrency guarantee is not upheld internally

Evidence:

- `pkg/fdbgo/fdb/transaction.go:24` documents that individual transaction methods are safe for concurrent use.
- `pkg/fdbgo/client/transaction.go:164` says `conflictMu` protects mutations/conflicts.
- Mutators append under `conflictMu` at `pkg/fdbgo/client/transaction.go:631`, `pkg/fdbgo/client/transaction.go:646`, `pkg/fdbgo/client/transaction.go:667`, and `pkg/fdbgo/client/transaction.go:690`.
- Commit reads `tx.mutations` without that lock at `pkg/fdbgo/client/transaction.go:737` and `pkg/fdbgo/client/transaction.go:757`.
- `GetApproximateSize` reads mutations/conflicts without the lock at `pkg/fdbgo/client/transaction.go:1064`.
- `postCommitReset` and retry `reset` clear `tx.mutations` outside the lock at `pkg/fdbgo/client/transaction.go:1401` and `pkg/fdbgo/client/transaction.go:1435`.
- Commit marshalling reads mutation/conflict slices without taking the lock in `pkg/fdbgo/client/commitpath.go:267`.

Impact:

Concurrent `Set/Get/Commit/GetApproximateSize` on the same transaction can data race, see partial slice state, or clear a mutation buffer while another method appends. The RYW atomic path also unlocks during a server read and writes back the resolved atomic value later, which can overwrite a concurrent write to the same key.

Fix notes:

- Either narrow the public concurrency contract, or consistently snapshot mutation/conflict state under `conflictMu`.
- Keep `tx.mutations` clearing inside the same critical section as conflict clearing.
- Add race-detector tests for concurrent `Set` plus `Commit`, `Set` plus `GetApproximateSize`, and atomic resolution racing a later `Set`.

### 8. Medium - `ReadErrorOr` is shape-based and breaks one-field success replies

Evidence:

- `pkg/fdbgo/wire/reader.go:601` treats any decoded object with `nfields <= 1` and field 0 present as an error.
- `pkg/fdbgo/wire/types/splitrangereply_generated.go:29` defines `SplitRangeReply` with exactly one field, `SplitPoints`.
- `pkg/fdbgo/client/metrics.go:200` calls `ReadErrorOr` before unmarshalling `SplitRangeReply`.
- The generated `Error` type stores `ErrorCode` as `uint16`, but `ReadErrorOr` reads it with `ReadInt32` at `pkg/fdbgo/wire/reader.go:613`.

Impact:

Successful `GetRangeSplitPoints` replies can be mistaken for FDB errors by reading the split-points relative offset as an error code. Empty one-field success replies can become `empty ErrorOr response`. Error decoding also relies on adjacent padding being zero because it reads four bytes for a two-byte code.

Fix notes:

- Parse the actual `ErrorOr` union tag/value instead of inferring success/error from the nested value's field count.
- Decode `types.Error.ErrorCode` as `uint16`.
- Add unit tests for `SplitRangeReply` with nil and non-empty `SplitPoints`, plus `ErrorOrError` with non-zero padding around the code.

### 9. Medium - System-key detection only recognizes special keys

Evidence:

- `pkg/fdbgo/client/transaction.go:544` documents system key access using `\xff`.
- `pkg/fdbgo/client/transaction.go:593` implements `isSystemKey` as only `\xff\xff`.
- `pkg/fdbgo/client/transaction.go:418` and `pkg/fdbgo/client/transaction.go:618` skip read conflicts only when `isSystemKey` returns true.

Impact:

Reads of `\xff/...` after `SetReadSystemKeys` can add resolver conflict ranges in system keyspace even though comments say system keys are handled internally without resolver conflicts. Mixed transactions that read system keys and write user keys can diverge from C++ behavior.

Fix notes:

- Decide whether `isSystemKey` should mean `key[0] == 0xff` and add a separate `isSpecialKey` helper for `\xff\xff`.
- Add tests for `SetReadSystemKeys` plus a user write after reading `\xff/...`.

### 10. Medium - Database-level access-system-keys does not set the lock-aware commit flag

Evidence:

- Database defaults call only `tx.SetAccessSystemKeys()` at `pkg/fdbgo/client/database.go:540`.
- `pkg/fdbgo/client/transaction.go:587` sets read/write system key booleans, but not `lockAware`.
- The public transaction option does set lock aware at `pkg/fdbgo/fdb/options.go:58`.
- Commit flags are derived from `tx.lockAware` in `pkg/fdbgo/client/commitpath.go:326`.

Impact:

A transaction created through database-level `SetDefaultAccessSystemKeys` can pass client-side key-range checks for system-key writes while omitting the commit request's lock-aware bit. This is inconsistent with per-transaction `SetAccessSystemKeys` through the public facade.

Fix notes:

- Either make `client.Transaction.SetAccessSystemKeys` also set `lockAware`, or have database defaults apply both flags.
- Add a commit-request unit test proving database-level access-system-keys sets `FLAG_IS_LOCK_AWARE`.

### 11. Medium - TLS support is advertised but not wired into database connections — FIXED (RFC-051)

`ParseClusterString` now parses the `:tls` coordinator suffix (→ `ClusterFile.UseTLS`),
`resolveTLSConfig` loads `FDB_TLS_{CERTIFICATE,KEY,CA}_FILE` (C++ precedence) into a
standard `*crypto/tls.Config`, and `database.getOrDialConn` dials over TLS via
`transport.Dial(ctx, addr, *tls.Config, dialFn)`. The user-facing API is functional
options — `fdb.OpenDatabase(clusterFile, WithTLSConfig(*tls.Config), WithDialFunc(...))`
(bradfitz review). The plaintext-on-TLS-framing footgun is gone by construction: a
non-nil `*tls.Config` is the *only* "use TLS" signal (nil = plaintext); the old
`useTLS bool` / `DialWith` / `DialWithTLS` / bespoke `transport.TLSConfig` are deleted.
`Conn.useTLS` survives only as the internal frame-checksum-omission flag, derived from
`tlsConfig != nil`. FDB-C + Torvalds + bradfitz ACK. Follow-ups: per-address TLS flag
(dual-listen), `FDB_TLS_VERIFY_PEERS` rule DSL, `FDB_TLS_PASSWORD`/encrypted keys,
FDB-TLS testcontainer e2e.

### 12. Medium - Location refresh can panic on empty location responses

Evidence:

- `pkg/fdbgo/client/locality.go:267` calls `queryLocations`.
- `pkg/fdbgo/client/locality.go:274` immediately indexes `entries[0]` without checking length.

Impact:

An empty or malformed `GetKeyServerLocationsReply` can panic the client instead of returning an FDB error and letting callers retry or reconnect.

Fix notes:

- Return `all_alternatives_failed`, `wrong_shard_server`, or a typed internal error when no entries are returned.
- Add a unit test with `queryLocations` returning an empty slice.

### 13. Low - Reply channels are not returned to the pool after successful replies

Evidence:

- `pkg/fdbgo/transport/conn.go:249` says callers should release reply handles after success.
- `pkg/fdbgo/transport/conn.go:47` returns a channel to the pool only on `Cancel`.
- `pkg/fdbgo/transport/conn.go:54` `Release` returns only the handle to the pool.
- `pkg/fdbgo/transport/conn.go:503` deletes successful replies from `pending` and sends on the channel; there is no later `putReplyChannel`.

Impact:

Every successful `PrepareReply`/`Send` consumes a fresh reply channel forever from the pool's perspective. This is a steady allocation/performance leak on the hot path.

Fix notes:

- Make successful reply consumption return the channel to the pool exactly once.
- Avoid double-put between `Cancel` and `Release`.
- The `Send` API may need a handle or wrapper, because it currently returns only `<-chan Response`.

### 14. Low - Full write queue makes connection monitor report a successful ping

Evidence:

- `pkg/fdbgo/transport/conn.go:696` tries to enqueue a ping.
- On a full write channel, `pkg/fdbgo/transport/conn.go:699` cancels the reply and closes `done`.
- `pkg/fdbgo/transport/conn.go:661` treats `<-replyCh` as a ping reply.

Impact:

When the write queue is full, the monitor can treat "ping was not sent" as "ping succeeded", delaying dead-connection detection under write pressure.

Fix notes:

- Return an explicit status from `sendPingWithReply`, or keep the monitor in the timeout/bytes-received path when the ping could not be enqueued.
- Add a unit test with a saturated `writeCh`.

### 15. Low - Range iterator forward boundary relies on non-obvious slice capacity behavior

Evidence:

- `pkg/fdbgo/fdb/range_result.go:234` uses `ri.begin = append([]byte(lastKey), 0)`.
- The current wire parser caps key slices to their length in `pkg/fdbgo/wire/types/keyvalueref_generated.go:136`, which prevents overwrite today.

Impact:

If a future parser or synthetic `KeyValue` supplies a key slice with spare capacity, advancing the iterator can append into the same backing array as the returned key. This is a latent aliasing bug; the reverse path already uses a defensive copy.

Fix notes:

- Change to `ri.begin = append(append([]byte(nil), lastKey...), 0)`.
- Add a unit test with a `Key` whose capacity exceeds its length.

### 16. Low - GRV cache gate omits ratekeeper throttle cooldown

Evidence:

- C++ `TransactionState::getReadVersion` gates the cache block on `rkThrottlingCooledDown(cx.getPtr(), options.priority)` (`/tmp/fdbsrc` 7.3.75, `fdbclient/NativeAPI.actor.cpp:7506`). When the ratekeeper has recently throttled this priority, C++ skips the whole block — it does NOT start `backgroundGrvUpdater` and does NOT serve a cached version, going straight to a real GRV.
- Go's gate (`pkg/fdbgo/client/grv.go` `getReadVersion`) is `!isImmediate && useGrvCache && !skipGrvCache`, with no throttle-cooldown condition. `tryCache` DOES recheck throttle on the serve path (`grv.go:64-71`), so a stale/throttled cache never serves — but the background refresher now starts under throttle where C++ would not.

Impact:

Correctness is unaffected (the serve path still throttle-gates). Only the timing of the background updater's launch diverges: Go may start it slightly earlier under active throttling. Surfaced by the FDB C++ reviewer on PR #291.

Fix notes:

- Port `rkThrottlingCooledDown` (C++ `NativeAPI.actor.cpp:7480-7499`) + the `GRV_CACHE_RK_COOLDOWN` knob, track `lastRkBatchThrottleTime`/`lastRkDefaultThrottleTime` (already partly modeled via `lastRkBatch`/`lastRkDefault`), and add the cooldown check to the cache gate.
- Regression: a transaction marked rk-throttled must NOT start the refresher until cooldown elapses.

## Systemic Prevention Plan

The root problem is not just individual bugs. The client has several places where protocol semantics, retry policy, parser behavior, and lifecycle cleanup are reimplemented by hand. Prevent recurrence by making those invariants centralized, testable, and mandatory in CI.

### A. Keep generated wire layouts, and pin the remaining protocol truth

The wire message layout files are already generated. The recurring risk is the handwritten semantic layer around those generated types: error-code tables, retry predicates, `ErrorOr` handling, and embedded reply-error checks.

Prevented bugs: wrong error codes, incorrect retry predicates, shape-based `ErrorOr` parsing, missing embedded error checks.

Actions:

- Generate or mechanically sync FDB error constants/descriptions/retry predicates from one checked-in source of truth.
- Keep `client`, `fdb`, and `wire` packages from defining conflicting error tables.
- Add a test that asserts all exported/reused error constants match the canonical table.
- Replace shape-based `ReadErrorOr` logic with real union tag/value parsing.
- Require every generated reply type with `HasError/Error` fields to use a shared helper before success fields can be read.

CI gate:

```sh
go test ./pkg/fdbgo/wire ./pkg/fdbgo/wire/types ./pkg/fdbgo/client -run 'Error|ErrorOr|ReplyParser|WrongShard' -count=1
```

### B. Centralize read semantics

Prevented bugs: pipelined reads skipping wrong-shard retry, embedded errors becoming nil results, load-balancing accounting differences.

Actions:

- Define one internal read-result state machine:
  - locate shard
  - send request
  - decode reply
  - classify embedded/root errors
  - update QueueModel
  - invalidate location cache
  - retry or return
- Make synchronous, pipelined, hedged, and fallback reads share that state machine.
- Keep scheduling differences separate from semantics. Pipelining may defer waiting; hedging may send a backup; neither may alter error handling or retry rules.

CI gate:

```sh
go test ./pkg/fdbgo/client -run 'GetValue|GetKey|GetRange|WrongShard|FutureVersion|ProcessBehind|Pipelined' -count=1
```

### C. Enforce resource-lifecycle accounting

Prevented bugs: QueueModel leaks, reply-channel leaks, stranded senders on close, monitor false-success pings.

Actions:

- Give every resource a single owner and exactly-once cleanup path:
  - every `startRequest` must end exactly once
  - every prepared reply must be delivered, canceled, or failed exactly once
  - every sync write waiter must receive exactly one result
  - every connection failure path must close the socket and wake pending waiters
- Introduce small scoped helper types where possible, for example `inFlightRead`, `replyRegistration`, and `queuedWrite`.
- Do not allow raw `ReplyHandle` plus raw QueueModel delta plumbing to spread through read paths.

CI gate:

```sh
go test ./pkg/fdbgo/client ./pkg/fdbgo/transport -run 'Hedge|QueueModel|Reply|Close|ConnectionMonitor|Flush' -count=1
```

### D. Require marshalling purity

Prevented bugs: tenant commit request building mutating transaction buffers, versionstamp offsets being adjusted twice.

Actions:

- Treat all request builders as pure functions over transaction snapshots.
- Never let a request builder mutate `tx.mutations`, conflict buffers, RYW cache, or transaction options.
- Snapshot mutable transaction state under lock before marshalling.
- Add repeated-build tests for every request builder that applies transformations such as tenant prefixes or versionstamp offset adjustment.

CI gate:

```sh
go test ./pkg/fdbgo/client -run 'Build.*Request|CommitTransaction|Tenant|Versionstamp|MutationLayout' -count=1
```

### E. Make concurrency promises executable

Prevented bugs: public facade promising concurrent transaction methods while internals race.

Actions:

- Pick one contract:
  - either transaction methods are truly concurrent-safe, or
  - documentation says transactions are single-goroutine except for explicitly listed methods.
- If concurrent-safe, snapshot or guard all mutation/conflict/read-version/commit fields consistently.
- Add race tests that exercise public API concurrency promises, not only internal helpers.
- Run race tests in CI for the native client packages.

CI gate:

```sh
go test -race ./pkg/fdbgo/client ./pkg/fdbgo/fdb ./pkg/fdbgo/transport -run 'Concurrent|Race|Transaction|Future|Close' -count=1
```

### F. Add fault-injection as a release blocker

Prevented bugs: happy-path tests passing while topology, shard, and transport failures are broken.

Actions:

- Keep a deterministic fake transport for unit fault injection.
- Cover:
  - wrong shard during point reads, key selectors, and ranges
  - future version and process behind
  - all alternatives failed
  - proxy change while commit is in flight
  - connection close during send, flush, and reply wait
  - malformed/empty location replies
  - hedged winner/loser/timeout/cancel cases
- Fail CI if any High-severity regression test is skipped without an explicit build tag.

Suggested CI target:

```sh
go test ./pkg/fdbgo/client ./pkg/fdbgo/transport -run 'Fault|WrongShard|ProxyChange|Close|Timeout|Malformed|Hedge' -count=1
```

### G. Differential and conformance testing

Prevented bugs: native behavior diverging from the official FDB binding without being noticed.

Actions:

- Run a nightly or pre-merge conformance suite against a real FDB cluster.
- Compare native Go behavior with the official C binding for:
  - error codes and retry behavior
  - point reads and key selectors
  - range reads in all streaming modes
  - tenants and tenant-prefixed commits
  - system/special key access
  - watches
  - commit unknown result handling
- Store minimal reproducer tests for any divergence before fixing it.

Suggested non-fast target:

```sh
go test ./pkg/fdbgo/client ./pkg/fdbgo/fdb -run 'CPort|Conformance|Correctness|Tenant|Watch|MultiShard' -count=1
```

### H. Code review checklist for native-client changes

Every PR touching `pkg/fdbgo/client`, `pkg/fdbgo/fdb`, `pkg/fdbgo/transport`, or `pkg/fdbgo/wire` should answer these before merge:

- Does this add a new protocol parser or request builder? If yes, where is the malformed/error test?
- Does this start a request, register a reply, enqueue a write, or acquire a pooled resource? If yes, where is exactly-once cleanup proven?
- Does this introduce a new read path? If yes, how does it share the centralized read semantics?
- Does this touch transaction state? If yes, what lock or snapshot protects it?
- Does this change error handling? If yes, which canonical error-code/retry-predicate test covers it?
- Does this optimize allocations with aliasing or unsafe? If yes, where is the purity/aliasing regression test?
- Does this claim C++/C-binding parity? If yes, which conformance test pins it?

### I. Definition of done for the current bug batch

Do not call the native client production-ready until all of these are true:

- All High findings above are fixed with regression tests.
- `wrong_shard_server` behavior is validated with the real code `1001`.
- Pipelined `Get` and normal `Get` pass the same injected error matrix.
- `go test -race` passes on `client`, `fdb`, and `transport`.
- Connection close tests prove no blocked `SendFrame`, `Flush`, or pending reply goroutines.
- Tenant commit request building is proven non-mutating.
- Hedged read tests prove QueueModel outstanding returns to baseline.
- At least one real-cluster conformance run passes after the fixes.

## Validation Performed

Targeted unit subset:

```sh
go test ./pkg/fdbgo/client ./pkg/fdbgo/fdb ./pkg/fdbgo/transport ./pkg/fdbgo/wire ./pkg/fdbgo/wire/types -run '^Test(ParseCommitReply_ErrorOrError|FDBError_Description_LatentBugFixes|IsWrongShardServer|IsAllAlternativesFailed|IsFutureVersionOrProcessBehind|BuildVoidReply|ParseKeyValueRefStringVector)$' -count=1
```

Result: passed. Note that the passing client wrong-shard tests currently pin the incorrect `1062` assumption described above.
