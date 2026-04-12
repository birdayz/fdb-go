# Nightshift-5 Handover

**Date:** 2026-04-11 23:00 — 2026-04-12 07:30 CEST
**PR:** #33
**Branch:** `nightshift-5`

## Objective

Rewrite FDB testcontainer module — eliminate socat proxy, add auto-init, chaos primitives, proper options.

## What was done

### 1. Testcontainer rewrite — single container, no socat

Replaced the 2-container socat proxy architecture with a single FDB container. Eliminates socat container, custom entrypoint scripts (`fdb-entrypoint.sh`, `socat-entrypoint.sh`), and the `embed.go` dependency.

**Architecture:**
- External clients: `localhost:mappedPort` via Docker port mapping (Bazel sandbox safe)
- Internal clients: `containerIP:4500` via Docker bridge IP (cross-container)
- `WithDirectIP()`: bypasses Docker DNAT entirely for high-connection-churn tests

### 2. Docker exec multiplexing bug fix

**Root cause found:** testcontainers-go `Exec()` returns Docker multiplexed stream with 8-byte binary frame headers per stdout/stderr chunk. Without `tcexec.Multiplexed()`, raw headers corrupt text output. This silently broke cluster file content (prepended `\x01\x00\x00\x00\x00\x00\x00\x1e` to the string), causing the FDB client to fail coordinator handshake with a 60-second timeout. Extremely hard to diagnose — corrupted string looks fine when printed.

**Fix:** All `Exec()` calls now use `tcexec.Multiplexed()`.

### 3. DNAT assertion spam fix

**Root cause found:** Docker port mapping causes `canonicalRemotePort` assertion flood in FDB server (`FlowTransport.actor.cpp:1545`). The Apple CGo FDB client (used by Java conformance server) and tests with high connection churn are affected. Our pure Go client has mitigations (`CanonicalRemotePort: 0`, `SetLinger(0)`).

**Fix:** `WithDirectIP()` option bypasses DNAT. Applied to all fdbgo test suites and conformance tests.
- `client_test`: 60-min timeout → 93 seconds
- `conformance_test`: 15-min timeout → 60 seconds

### 4. Auto-init + proper options

- `Run()` auto-initializes database (no more separate `InitializeDatabase()` call)
- `WithStorageEngine("ssd")`, `WithRedundancyMode`, `WithTenantMode` options
- `WithDirectIP()`, `WithNetwork()`, `WithStartupTimeout()`, `WithoutInit()`
- All fdbgo test callers migrated from manual `fdbcli configure` to options

### 5. Chaos testing primitives

- `Pause(ctx)` / `Unpause(ctx)` via Docker container pause API
- `FDBCLIExec(ctx, command)` helper for running fdbcli commands
- `Status(ctx)` convenience method

### 6. Vollkonti process update

Added review loop to shift process: push → request `@claude review` → wait for feedback → address → re-request → iterate until clean.

## Current state

- **Branch:** `nightshift-5` (5 commits ahead of master)
- **PR:** #33 — reviewed (4 rounds, 9 issues found and fixed, reviewer says "ready to merge")
- **All 13 Bazel test targets pass**
- **17 testcontainer tests** (unit + integration + multi-instance + chaos + version selection)
- **2307 Ginkgo specs** pass (record layer)
- **430 conformance specs** pass
- **50 chaos tests** pass

## Known issues

- **`time.Sleep(1s)` in `InitializeDatabase`** — timing-based stabilization after "FDBD joined cluster". Works in practice since `fdbcli configure` retries internally. Could be replaced with health polling if startup time becomes a bottleneck.
- **Port mapping still causes minor DNAT assertion noise** for tests using default mode (not `WithDirectIP`). Acceptable for low-connection-churn tests (record layer suite, chaos tests).

## What to work on next

### High priority
- **Pool frame read buffers** — `ReadFrame` allocates `make([]byte, payloadLen)` per response. Pool via `sync.Pool`.
- **DatabaseContext refactor** — consolidate Database/GRVBatcher/LocationCache/Cluster

### Medium priority
- **FDBReverseDirectoryCache** — reverse prefix→name caching (~496 lines Java)
- **KeySpace/KeySpacePath** — enterprise key management

### Low priority
- **Multi-node testcontainer** — multiple FDB processes for multi-shard testing
- **Version vector support** — causal consistency optimization
