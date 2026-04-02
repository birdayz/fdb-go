# RFC 015: Connection Lifecycle and Error Recovery

**Status**: Proposed  
**Author**: birdy  
**Date**: 2026-04-02  
**Depends on**: RFC 014 (DatabaseContext refactor)  
**Reviews**: Round 1 (5 FDB maintainers), Round 2 (5 FDB maintainers)

## Problem

From the outside, the Go FDB client must behave identically to the C++ client for connection failures. If a user opens a client, does queries, a server dies, and the user does another query — it should transparently reconnect and succeed. Currently it returns a fatal error.

### Concrete failure scenario

1. User calls `OpenDatabase()` → connects to coordinator, gets proxy list, caches connections
2. User calls `Transact(Get)` several times → works fine
3. GRV proxy process restarts (or network blip)
4. User calls `Transact(Get)` → **should transparently retry with new proxy**
5. Currently: `getOrDial` tries dead address → dial fails → raw Go error → `OnError` doesn't recognize it → **returns error to user**

C++ behavior: `basicLoadBalance` tries all alternative proxies sequentially → if all fail, waits via `quorum(ok,1)` for any to recover → `onProxiesChanged()` races → outer loop retries with fresh proxy list → succeeds. User sees nothing.

## C++ reference architecture

**Source**: `fdbclient/NativeAPI.actor.cpp`, `fdbrpc/FlowTransport.actor.cpp`, `fdbrpc/LoadBalance.actor.h`

### Critical insight: `broken_promise` NEVER reaches `onError`

Connection errors are resolved at **Layer 2** (loadBalance / RPC retry loops), not Layer 3 (`Transaction::onError`). The user never sees `broken_promise`, `connection_failed`, or `request_maybe_delivered` directly. These codes are internal to the retry machinery.

### C++ GRV path — `basicLoadBalance` with `AtMostOnce::False`

```
getConsistentReadVersion (NativeAPI.actor.cpp:7231)
  └── loop {
        choose {
          when(basicLoadBalance(grvProxies, AtMostOnce::False, ...)) → return
          when(onProxiesChanged()) → continue loop with fresh proxies
        }
      }
```

`basicLoadBalance` (LoadBalance.actor.h:752-842):
1. Pick proxy, send RPC — **strictly sequential** (no speculative second request — that's `loadBalance`, not `basicLoadBalance`)
2. On `broken_promise` → try next alternative proxy immediately (line 823)
3. Cycle through ALL proxies
4. If all down → `wait(quorum(ok, 1))` — **block indefinitely** until FailureMonitor marks any proxy as alive (line 796-806)
5. `onProxiesChanged()` races in outer loop — if topology updates, cancel `basicLoadBalance` and restart

**Note**: Because `GrvProxyInterface` has `alwaysFresh()=true`, `basicLoadBalance` uses `quorum(ok,1)` directly, NOT `allAlternativesFailedDelay`.

**Result**: single proxy death = sub-millisecond failover to next proxy. All proxies down = block until recovery. Zero user-visible errors.

### C++ commit path — `basicLoadBalance` with `AtMostOnce::True`

```
tryCommit (NativeAPI.actor.cpp:6628)
  └── choose {
        when(basicLoadBalance(commitProxies, AtMostOnce::True, ...)) → parse reply
        when(onProxiesChanged()) → throw request_maybe_delivered
      }
```

1. Pick one commit proxy, send RPC
2. On `broken_promise` → **do NOT try another proxy** (at-most-once!)
3. Convert `broken_promise` → `request_maybe_delivered` (line 828-829)
4. `tryCommit` catches `request_maybe_delivered` (line 6731) → `commitDummyTransaction` → `commit_unknown_result`
5. `commit_unknown_result` reaches `onError` → retryable with self-conflicting

**`commitDummyTransaction`** (line 6306): Creates a new transaction with overlapping conflict ranges, commits it. If it succeeds, the original definitely didn't commit (or committed first and the dummy conflicts). Establishes that the original is no longer in-flight before retrying.

**`not_committed` (1020) semantics**: In C++, `not_committed` is ONLY thrown when the commit proxy **successfully evaluates** the transaction and **rejects it due to MVCC conflict** (line 6726, `ci.version == invalidVersion`). It is NEVER used for connection-level failures. C++ makes **zero distinction** between "couldn't connect" and "connection died after send" — both are `broken_promise` → `request_maybe_delivered` → `commit_unknown_result`.

### C++ storage server reads — `loadBalance` (not `basicLoadBalance`)

```
getValue (NativeAPI.actor.cpp:3700)
  └── loop {
        loadBalance(locationInfo, ...) → tries all replicas with speculative 2nd
        catch(wrong_shard_server | all_alternatives_failed) → invalidate cache, retry
      }
```

`loadBalance` (LoadBalance.actor.h:442-734) — MORE sophisticated than `basicLoadBalance`:
1. Try best replica, race speculative second request after ~0.5ms (`BASE_SECOND_REQUEST_TIME`)
2. On `broken_promise` → try next replica
3. All replicas down → `allAlternativesFailedDelay` (50ms-1s jittered, `wait(quorum(ok,1))`)
4. `all_alternatives_failed` (error code **1006**) → invalidate location cache → outer loop retries

### C++ error codes — where they're handled

| Error | Code | Handled at | Reaches `onError`? |
|---|---|---|---|
| `broken_promise` | 1100 | `basicLoadBalance` / `loadBalance` | **Never** |
| `connection_failed` | 1026 | Read retry loops | **Never** |
| `request_maybe_delivered` | 1015 | `tryCommit` → `commitDummyTransaction` → 1021 | As 1021 |
| `all_alternatives_failed` | 1006 | Read retry loops | **Never** |
| `commit_unknown_result` | 1021 | `onError` | **Yes** |
| `not_committed` | 1020 | `onError` | **Yes** (server-side conflict ONLY) |
| `transaction_too_old` | 1007 | `onError` | **Yes** |
| `wrong_shard_server` | 1062 | Read retry loops | **Never** |

## Design: Go connection lifecycle

### Principle

From the user's perspective, `Transact()` transparently survives proxy restarts, storage server restarts, network blips, and topology changes. Connection errors are NEVER returned to the user — they are resolved internally, matching C++.

### Architecture: three layers of retry

```
Layer 3: Transact retry loop (OnError)
  │  Only sees FDB errors: not_committed (conflict), commit_unknown_result,
  │  transaction_too_old, future_version, etc.
  │  NEVER sees: broken_promise, connection_failed, all_alternatives_failed.
  │
  └── Layer 2: RPC retry (GRV, commit, location, reads)
  │     GRV: try all proxies; all fail → retryable FDB error
  │     Commit: one proxy; conn failure → commit_unknown_result
  │     Reads: try all replicas; all fail → all_alternatives_failed → cache invalidation + retry
  │     Connection errors resolved HERE, not propagated to user.
  │
  └── Layer 1: Connection pool (getOrDial)
        Detects dead connections (IsClosed), evicts, dials new.
        No retry — just lazy replacement.
```

### Shutdown sequence (implemented in conn.go)

**Path A — Client calls Close():**
1. `cancel()` — cancels connection context
2. `conn.Close()` — closes TCP socket, unblocks `readLoop`
3. `readLoop` → `failAllPending(err)` → `cancel()` (no-op) → `c.conn.Close()` (no-op, already closed) → `loopWG.Done()`
4. `Close()` returns after `loopWG.Wait()`

**Path B — Server dies:**
1. `readLoop` gets EOF → `failAllPending(err)` → `cancel()` (marks dead) → `c.conn.Close()` (close socket) → `loopWG.Done()`
2. `IsClosed()` returns true → pool evicts on next `getOrDial`
3. If pool later calls `Close()`, it's safe (idempotent cancel + already-closed conn)

**Critical fix from Round 2**: `readLoop` must call `c.conn.Close()` on exit to prevent TCP socket fd leak. Without this, Path B leaves the socket open if nobody calls `Close()`. The `loopWG.Wait()` deadlock concern doesn't apply — `readLoop` calls `c.conn.Close()`, not `c.Close()`.

### Safety invariants

1. **`Close()` MUST NOT be called from `readLoop`** — self-deadlock on `loopWG.Wait()`. readLoop calls `c.conn.Close()` (the raw socket) and `cancel()`, never `c.Close()` (the Conn method).
2. **All commit connection failures → `commit_unknown_result`** — matches C++ `AtMostOnce::True`. Never try another proxy. Never use `not_committed` for connection errors (`not_committed` = server-side conflict only).
3. **GRV/location failures → try all alternatives first** — matches C++ `AtMostOnce::False`.
4. **"All unreachable" errors must be typed `*wire.FDBError`** — never bare `fmt.Errorf`. This ensures `isAllAlternativesFailed()` works via `errors.As`.
5. **`handlePing` write deadline inside `mu`** — prevents cross-contamination with concurrent `SendFrame`.

### GRV path: try all proxies (Layer 2)

Match C++ `basicLoadBalance` with `AtMostOnce::False`:

```go
// ErrAllProxiesUnreachable is returned when every proxy in the current
// topology failed. Retryable at Layer 3 — topology monitor will refresh
// the proxy list in the background.
const ErrAllProxiesUnreachable = 1200 // Go-internal, not a C++ error code

func (b *grvBatcher) sendGRVRequest(db *database) (int64, bool, bool, error) {
    proxies := db.getGRVProxies()
    if len(proxies) == 0 {
        return 0, false, false, &wire.FDBError{Code: ErrAllProxiesUnreachable}
    }

    for _, proxy := range proxies {
        conn, err := db.getOrDial(db.ctx, proxy.Address)
        if err != nil {
            db.handleConnError(proxy.Address)
            continue // try next proxy
        }

        replyToken, replyCh := conn.PrepareReply()
        body := buildGetReadVersionRequest(replyToken)
        if err := conn.SendFrame(proxy.Token, body); err != nil {
            db.handleConnError(proxy.Address)
            continue
        }

        ctx, cancel := context.WithTimeout(db.ctx, DefaultRPCTimeout)
        select {
        case resp := <-replyCh:
            cancel()
            if resp.Err != nil {
                db.handleConnError(proxy.Address)
                continue
            }
            return parseGetReadVersionReply(resp.Body)
        case <-ctx.Done():
            cancel()
            continue
        }
    }

    // All proxies failed. Kick topology so background refresh gets new list.
    // Return retryable FDB error — Transact will retry after OnError backoff.
    db.kickTopology()
    return 0, false, false, &wire.FDBError{Code: ErrAllProxiesUnreachable}
}
```

**OnError must handle `ErrAllProxiesUnreachable`** — added to the retryable set with exponential backoff. This is a Go-internal code (not C++), needed because Go can't block indefinitely like C++ `quorum(ok,1)`. Instead: return retryable error → `Transact` retries → topology monitor has refreshed proxy list → next attempt succeeds.

**C++ divergence**: C++ `basicLoadBalance` blocks via `wait(quorum(ok,1))` until a proxy recovers. We return immediately and rely on `Transact` retry + topology monitor. Slightly higher latency (one `Transact` retry cycle) but avoids blocking a goroutine indefinitely. The topology kick ensures the proxy list refreshes before the next retry.

### Commit path: connection failure → always `commit_unknown_result` (Layer 2)

Match C++ `AtMostOnce::True`. ALL connection errors → `commit_unknown_result`. No distinction between dial/send/response failure — matching C++ which makes zero distinction (all are `broken_promise` → `request_maybe_delivered` → `commit_unknown_result`).

```go
func (tx *Transaction) commit(ctx context.Context) error {
    proxy, err := tx.db.getCommitProxy()
    if err != nil {
        return &wire.FDBError{Code: ErrAllProxiesUnreachable}
    }

    conn, err := tx.db.getOrDial(ctx, proxy.Address)
    if err != nil {
        // C++: broken_promise → request_maybe_delivered → commit_unknown_result.
        // C++ makes no distinction between "couldn't connect" and "died after send."
        // Even if we know we never sent, C++ treats it the same way.
        tx.db.handleConnError(proxy.Address)
        tx.db.kickTopology()
        return &wire.FDBError{Code: ErrCommitUnknownResult}
    }

    replyToken, replyCh := conn.PrepareReply()
    body := buildCommitTransactionRequest(tx, replyToken)

    if err := conn.SendFrame(proxy.Token, body); err != nil {
        tx.db.handleConnError(proxy.Address)
        tx.db.kickTopology()
        return &wire.FDBError{Code: ErrCommitUnknownResult}
    }

    rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
    defer cancel()

    select {
    case resp := <-replyCh:
        if resp.Err != nil {
            tx.db.handleConnError(proxy.Address)
            tx.db.kickTopology()
            return &wire.FDBError{Code: ErrCommitUnknownResult}
        }
        return tx.parseCommitReply(resp.Body)
    case <-rctx.Done():
        // Timeout — server may still be processing. Treat as unknown.
        return &wire.FDBError{Code: ErrCommitUnknownResult}
    }
}
```

**`commitDummyTransaction` deliberately skipped.** C++ uses it to confirm whether the original committed before throwing `commit_unknown_result`. We skip it because `OnError`'s self-conflicting mechanism (`makeSelfConflicting` equivalent — copy write conflicts to read conflicts) provides the same safety: if the original committed, the retry conflicts. This is a deliberate simplification. The self-conflicting mechanism is the actual safety net in both C++ and Go.

**Commit timeout → `commit_unknown_result`**: If the commit proxy is slow (not dead), the timeout fires. The server may still commit. Treating this as `commit_unknown_result` is correct and matches C++ behavior (timeouts in Flow produce `timed_out` which is handled similarly to `broken_promise` in the commit path).

### Storage server reads: retry replicas + `all_alternatives_failed` (Layer 2)

```go
// ErrAllAlternativesFailed matches C++ error code 1006.
const ErrAllAlternativesFailed = 1006

func (tx *Transaction) sendGetValue(ctx context.Context, key []byte, servers []ServerInfo) ([]byte, error) {
    for _, server := range servers {
        conn, err := tx.db.getOrDial(ctx, server.Address)
        if err != nil {
            tx.db.handleConnError(server.Address) // evict dead conn
            continue
        }

        replyToken, replyCh := conn.PrepareReply()
        body := buildGetValueRequest(key, tx.readVersion, replyToken, server.Token)
        if err := conn.SendFrame(server.Token, body); err != nil {
            tx.db.handleConnError(server.Address)
            continue
        }

        rctx, cancel := context.WithTimeout(ctx, DefaultRPCTimeout)
        select {
        case resp := <-replyCh:
            cancel()
            if resp.Err != nil {
                tx.db.handleConnError(server.Address)
                continue
            }
            return parseGetValueReply(resp.Body)
        case <-rctx.Done():
            cancel()
            continue
        }
    }
    // All replicas failed — typed FDB error for cache invalidation.
    return nil, &wire.FDBError{Code: ErrAllAlternativesFailed}
}

func (tx *Transaction) getValue(ctx context.Context, key []byte) ([]byte, error) {
    for attempts := 0; attempts < MaxWrongShardRetries; attempts++ {
        servers, err := tx.db.locCache.locate(tx.db, ctx, key)
        if err != nil { return nil, err }

        val, err := tx.sendGetValue(ctx, key, servers)
        if err == nil { return val, nil }

        // C++ catches both wrong_shard_server AND all_alternatives_failed
        // in the same handler — both invalidate cache + retry.
        if isWrongShardServer(err) || isAllAlternativesFailed(err) {
            tx.db.locCache.invalidate(key)
            time.Sleep(wrongShardRetryDelay)
            continue
        }
        return nil, err // other FDB error → propagate to Transact
    }
    return nil, fmt.Errorf("getValue: exhausted retries")
}

func isAllAlternativesFailed(err error) bool {
    var fdbErr *wire.FDBError
    return errors.As(err, &fdbErr) && fdbErr.Code == ErrAllAlternativesFailed
}
```

### Location cache refresh: try all commit proxies (Layer 2)

Same as GRV — cycle all commit proxies, return typed error on total failure:

```go
func (lc *locationCache) refresh(db *database, ctx context.Context, key []byte) ([]ServerInfo, error) {
    proxies := db.getCommitProxies()
    for _, proxy := range proxies {
        conn, err := db.getOrDial(ctx, proxy.Address)
        if err != nil {
            db.handleConnError(proxy.Address)
            continue
        }
        // ... send request, get response ...
        if err != nil {
            db.handleConnError(proxy.Address)
            continue
        }
        return servers, nil
    }
    db.kickTopology()
    return nil, &wire.FDBError{Code: ErrAllProxiesUnreachable}
}
```

### OnError additions

Only one new code: `ErrAllProxiesUnreachable` (Go-internal, for total proxy failure at Layer 2):

```go
case ErrNotCommitted, ErrDatabaseLocked, ErrProxyMemoryLimitExceeded,
     ErrGrvProxyMemoryLimit, ErrProcessBehind, ErrBatchTransactionThrottled,
     ErrAllProxiesUnreachable:
    tx.retryCount++
    time.Sleep(tx.nextBackoff())
    tx.reset()
    return nil
```

No `broken_promise`, `connection_failed`, or `all_alternatives_failed` in OnError — they are resolved at Layer 2. `commit_unknown_result` already handled with self-conflicting.

### handlePing: non-blocking write (deadline inside mu)

```go
func (c *Conn) handlePing(body []byte) {
    replyToken, ok := extractPingReplyToken(body)
    if !ok { return }
    replyBody := buildVoidReply()

    c.mu.Lock()
    c.conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
    _ = WriteFrame(c.conn, replyToken, replyBody, c.tls)
    c.conn.SetWriteDeadline(time.Time{}) // clear
    c.mu.Unlock()
}
```

Deadline set and cleared inside `mu` — no cross-contamination with concurrent `SendFrame`.

### readLoop: close socket on exit

```go
func (c *Conn) readLoop() {
    defer c.loopWG.Done()
    defer c.cancel()       // mark connection dead
    defer c.conn.Close()   // close TCP socket (prevents fd leak)
    // ... read loop ...
}
```

Three defers in LIFO order: `conn.Close()` first (close socket), then `cancel()` (mark dead), then `loopWG.Done()` (signal completion). This prevents TCP socket fd leaks when the server dies and nobody explicitly calls `Close()`.

### PrepareReply cleanup

Return a cancel function to prevent token leak if caller never sends:

```go
func (c *Conn) PrepareReply() (UID, <-chan Response, func()) {
    token := NewUID()
    ch := make(chan Response, 1)
    c.pending.Store(token, ch)
    cancel := func() { c.pending.Delete(token) }
    return token, ch, cancel
}
```

Callers: `defer cancel()` after creating, remove defer if send succeeds. In practice, most callers don't fail between PrepareReply and SendFrame, so the cancel function is rarely invoked.

**API change**: `PrepareReply` now returns 3 values. All call sites must be updated.

### Proxy accessor methods

```go
func (db *database) getGRVProxies() []ProxyInfo {
    info := db.dbInfo.Load()
    if info == nil { return nil }
    return info.GRVProxies
}

func (db *database) getCommitProxies() []ProxyInfo {
    info := db.dbInfo.Load()
    if info == nil { return nil }
    return info.CommitProxies
}
```

## Deliberate divergences from C++

| # | C++ feature | Go approach | Rationale | Phase |
|---|---|---|---|---|
| 1 | `basicLoadBalance` blocks via `quorum(ok,1)` when all proxies down | Return `ErrAllProxiesUnreachable`, let `Transact` retry | Go can't block goroutine indefinitely; topology kick + Transact retry achieves same result | 1 |
| 2 | `commitDummyTransaction` confirms original before retrying | Skip; self-conflicting in OnError is the safety net | Both C++ and Go rely on self-conflicting as the actual mechanism; dummy tx is defense-in-depth | 1 |
| 3 | `loadBalance` speculative second request after ~0.5ms | Sequential replica iteration | p99 optimization, not correctness | 2 |
| 4 | `allAlternativesFailedDelay` (50ms-1s) for storage reads | Immediate retry via `Transact` | Graceful rolling restart optimization | 2 |
| 5 | `connectionMonitor` outbound PING | No outbound PING; dead conns detected on next RPC | Detection latency 5s vs 2s | 2 |
| 6 | `onProxiesChanged()` races every proxy RPC via `choose` | Topology kick after total failure; stale list completed first | One extra retry cycle on topology change during cycling | 2 |

## User-visible behavior (must match C++)

| Scenario | C++ | Go (after this RFC) | Match? |
|---|---|---|---|
| Single proxy dies (3 total) | `basicLoadBalance` → next proxy (<1ms) | Cycle all proxies → next succeeds | **Yes** |
| All proxies die | `quorum(ok,1)` blocks → proxy recovers | `ErrAllProxiesUnreachable` → Transact retries → topology refreshes | **Yes** (higher latency) |
| Commit during conn failure | `commit_unknown_result` + self-conflicting | Same | **Yes** |
| Storage server dies (replicated) | `loadBalance` → next replica | Try all replicas → next succeeds | **Yes** |
| All replicas down | `allAlternativesFailed` delay → retry | Cache invalidation → Transact retries | **Yes** (no delay, Phase 2) |
| Proxy restart during idle | Transparent | Dead conn evicted, new conn dialed | **Yes** |
| Rapid proxy rotation | `onProxiesChanged` restarts each RPC | Cycle stale list → kick → next retry uses fresh | **Yes** (1 extra cycle) |
| Cluster down then recovers | `Transact` retries indefinitely | Same: retries until `ErrAllProxiesUnreachable` resolves | **Yes** |

## Implementation plan

### Phase 1: Layer 2 retry + conn safety (this RFC)

1. Add error codes: `ErrAllAlternativesFailed` (1006), `ErrAllProxiesUnreachable` (1200)
2. Add `ErrAllProxiesUnreachable` to `OnError` retryable set (backoff)
3. Add `getGRVProxies()` / `getCommitProxies()` returning all proxies
4. Rewrite `sendGRVRequest`: cycle all GRV proxies, evict dead conns, kick topology, return typed error
5. Rewrite `commit`: ALL connection errors → `commit_unknown_result`, evict, kick topology. Timeout → `commit_unknown_result`.
6. Rewrite `locationCache.refresh`: cycle all commit proxies, return typed error
7. Storage read path: `handleConnError` on dead replicas, `all_alternatives_failed` typed error, cache invalidation on `all_alternatives_failed`
8. `readLoop`: add `defer c.conn.Close()` to prevent socket fd leak
9. `handlePing`: write deadline inside `mu`
10. `PrepareReply`: return cancel function (3-value return)

### Phase 2: Performance + availability

- Speculative second request (storage reads)
- `allAlternativesFailedDelay` (wait for replica recovery)
- Outbound PING connection monitor
- Per-proxy backoff tracking
- `onProxiesChanged` race equivalent (cancel in-flight proxy cycling on topology change)

## Files affected

| File | Change |
|---|---|
| `client/transaction.go` | Add `ErrAllAlternativesFailed`, `ErrAllProxiesUnreachable`; add to OnError |
| `client/database.go` | Add `getGRVProxies()`, `getCommitProxies()` |
| `client/grv.go` | Rewrite `sendGRVRequest`: cycle all proxies, typed error |
| `client/commitpath.go` | Rewrite `commit`: all conn errors → `commit_unknown_result` |
| `client/locality.go` | Rewrite `refresh`: cycle all proxies, typed error |
| `client/readpath.go` | `handleConnError` on replicas, typed `all_alternatives_failed`, cache invalidation |
| `transport/conn.go` | `readLoop` defer `conn.Close()`, `handlePing` deadline inside `mu`, `PrepareReply` 3-value |

## Success criteria

1. `Transact(Get)` succeeds after single proxy restart — zero user-visible errors
2. `Transact(Set+Commit)` succeeds after proxy restart — `commit_unknown_result` → self-conflicting → transparent
3. All commit connection failures → `commit_unknown_result` (never `not_committed` for conn errors)
4. All-proxies-down → `Transact` retries (not fatal) until topology refreshes
5. Storage server death → replicas tried, dead conns evicted, `all_alternatives_failed` → cache invalidation
6. No TCP socket fd leaks (readLoop closes socket on exit)
7. No goroutine leaks (`Close()` blocks until readLoop exits)
8. No deadlocks (`Close()` never called from readLoop)
9. `handlePing` never blocks readLoop (write deadline)

## Review findings incorporated

### Round 1

| # | Finding | Resolution |
|---|---|---|
| 1 | `broken_promise` never reaches C++ `onError` | Resolved at Layer 2, not OnError |
| 2 | GRV vs commit different retry semantics | GRV cycles all; commit uses one + `commit_unknown_result` |
| 3 | `basicLoadBalance` cycles ALL alternatives | GRV and location refresh cycle all |
| 4 | Commit `broken_promise` → always `request_maybe_delivered` | All commit conn errors → `commit_unknown_result` |
| 5 | `handlePing` blocks readLoop | Write deadline inside `mu` |
| 6 | `Close()` from readLoop → deadlock | readLoop uses `c.conn.Close()` + `cancel()`, not `c.Close()` |
| 7 | Dead storage conns never evicted | `handleConnError` in read path |
| 8 | `all_alternatives_failed` → cache invalidation | Added alongside `wrong_shard_server` |

### Round 2

| # | Finding | Resolution |
|---|---|---|
| C1 | "All proxies unreachable" must be retryable, not fatal | `ErrAllProxiesUnreachable` (1200) added to OnError retryable set |
| C2 | `not_committed` wrong for conn errors — server-side conflict only | All commit conn errors → `commit_unknown_result` (no `not_committed` for conn) |
| C3 | `commitDummyTransaction` skipped | Documented as deliberate divergence; self-conflicting is the safety net |
| C4 | "All servers unreachable" must be `*wire.FDBError` not `fmt.Errorf` | `ErrAllAlternativesFailed` (1006) as typed FDB error |
| C5 | TCP socket fd leak if readLoop exits without `Close()` | `readLoop` defers `c.conn.Close()` |
| H1 | Per-replica 5s timeout too long | Phase 2: shorter timeout + speculative second |
| H2 | `handlePing` write deadline must be inside `mu` | Fixed: both `SetWriteDeadline` calls inside `mu` |
| H3 | `PrepareReply` token leak | Returns cancel function (3-value) |
| H4 | RFC header wrongly said GRV uses speculative 2nd | Corrected: `basicLoadBalance` is sequential |
| H5 | User context timeout returns non-retryable error | Commit timeout → `commit_unknown_result` (retryable). GRV batcher respects user ctx via select. |
