---
name: fdb-client-engineer
description: End-to-end engineering workflow for the pure-Go FoundationDB client (pkg/fdbgo — transport, transaction, commit path, RYW, key selectors, retry/ctx, GRV, wire encoding, the libfdb_c differential). Use for ANY client/wire change: drives RFC → review (FDB C++ maintainer + Torvalds + /code-review) → implement → review again → PR gauntlet (codex + @claude) → merge. The client analog of todo-worker: the FDB C++ dev substitutes for Graefe, C++ (libfdb_c 7.3.77) is the spec, wire compatibility is the hard line. Composes fdb-client-review (the review gate) and hunt-divergences (the differential method).
---

# FDB Client Engineering (pure-Go `pkg/fdbgo`)

You are engineering on the from-scratch pure-Go FoundationDB client. **Go, C, and Java apps
share one FDB cluster and read/write each other's bytes through it.** That makes every change
here wire- and behavior-load-bearing.

**C++ (`libfdb_c`) is the spec.** Any place the Go client behaves differently from the C++
client is a **bug in Go**, not a "Go choice" — until proven otherwise by *reading the C++
source* (CLAUDE.md design principle #2). This is the client/wire analog of "Graefe is final on
Cascades": here the **FDB C++ client developer** is final on client/wire correctness.

This skill is the **entry point and orchestrator** for client work. It owns the workflow
(RFC → impl → merge) and the discipline. It composes two narrower skills:
- **`fdb-client-review`** — the mandatory review gate (FDB C++ dev + Torvalds). Invoke it for
  every review cycle.
- **`hunt-divergences`** — the differential + fuzz method for finding/proving divergences vs
  `libfdb_c` (red→green in `pkg/fdbgo/bench`). Invoke it when the change is a divergence fix.

---

## 0. Before you touch anything: verify the work is real

The tracking docs (`TODO.md`, `TODO_client.md`, `TODO-production.md`, `shifts/*-fdbgo-client-*.md`)
**go stale fast** — client work moves in tight RFC cycles and the checkboxes lag. Treating a
stale TODO as open wastes a full RFC. **Before scoping anything:**

1. **Check the RFC ledger, not the TODO mark.** Each `rfcs/NNN-*.md` has a `**Status:**` line
   (`Implemented` / `Accepted` / `Draft`). Grep it: `head -12 rfcs/056*.md`.
2. **Check git log:** `git log --oneline -40 -- pkg/fdbgo/` — what actually landed.
3. **Check the code + its test.** A claimed gap that has a `segPhantom`/`segUnreadable`/named
   regression test is closed regardless of what the TODO says.

If the item is already done, **say so and flip the stale TODO mark** instead of inventing work.
(Real example: TODO.md listed RFC-056's "phantom slot" sub-edges as deferred long after RFC-058
shipped + differential-proved them.)

---

## 1. The spec: C++ source at 7.3.77 — read it FIRST, every time

The oracle is the FoundationDB C++ source at tag **7.3.77** (the `foundationdb` pin in
`MODULE.bazel`; exactly what `libfdb_c` and the cgo binding in the differential are built from).
**Do NOT confuse with `4.11.1.0`** (the *Java* record-layer version — different project).

Checkout is usually present at `/tmp/fdbsrc`; clone if missing:
```bash
ls /tmp/fdbsrc || git clone --branch 7.3.77 https://github.com/apple/foundationdb /tmp/fdbsrc
( cd /tmp/fdbsrc && git describe --tags )   # MUST print 7.3.77
```
If `/tmp/fdbsrc` is unavailable, fetch raw files (the source is NOT vendored):
`https://raw.githubusercontent.com/apple/foundationdb/release-7.3/<path>` and quote them.

| C++ file | What it specs |
|----------|---------------|
| `fdbclient/NativeAPI.actor.cpp` | `commit`/`tryCommit`, GRV (`getReadVersion`), `getValue`/`getKeyValues`/`getKey` retry loops, `commitDummyTransaction`, raw_access resolution |
| `fdbclient/ReadYourWrites.actor.cpp` | RYW read/write overlay; `resolveKeySelectorFromCache`; `addConflictRange`/`updateConflictMap`; `is_unreadable` stickiness |
| `fdbclient/RYWIterator.cpp` + `include/fdbclient/{RYWIterator,WriteMap}.h` | the RYW write-map segment model (`is_kv`/`is_unreadable`/independent/dependent/cleared) — the basis for phantom slots & conflict filtering |
| `fdbrpc/include/fdbrpc/LoadBalance.actor.h` | `loadBalance` — note it has **no per-read client reply timeout**; it re-sends until reply or read-version aging |
| `fdbclient/include/fdbclient/Atomic.h` | atomic-op semantics (`doMinV2`/`doAndV2` gate on `Optional.present()`) |
| `fdbclient/ClientKnobs.cpp` | retry limits, backoff, timeouts, `USE_GRV_CACHE` — the defaults Go must match |
| `fdbserver/storageserver.actor.cpp` | `sendErrorWithPenalty` (inline read error channel); `transaction_too_old` from version aging |
| `flow/error_definitions.h` | the authoritative error-code numbers |

Read the *actual* function before porting/verifying. Never guess wire layout or an error code.

---

## 2. Wire-format truths you must internalize (these have bitten us)

- **Two error channels for reads.** A read reply carries an error two ways: (a) the `ErrorOr<T>`
  **root** union, and (b) the **inline** `LoadBalancedReply.error` inside a "successful" reply.
  The storage server uses **(b)** for read wrong_shard / future_version / process_behind
  (`storageserver.actor.cpp` `sendErrorWithPenalty`), NOT the root. Read parsers check both.
- **`ErrorOr` is a `union_like`:** decide success/error by the **uint8 tag at slot 0**
  (1=Error, 2=value, 0=NONE), **never by field count** (a 1-field success is indistinguishable
  from a 1-field Error by count). `wire.ReadErrorOr` parses the tag.
- **`Optional<Error>` is a nested Error TABLE** (uint16 code at the table's slot 0) via a
  RelativeOffset — NOT a length-prefixed string. Use `wire.ReadInlineReplyError`; the generated
  `.Error` field mis-decodes it (schema-extractor bug — fix the extractor, don't hand-edit
  generated code).
- **Error codes are `uint16`.** Read with `ReadUint16`.
- Codes that matter: **1001** wrong_shard_server, **1006** all_alternatives_failed (read-path
  *absorbed* — must NEVER surface to the app), **1007** transaction_too_old (retryable),
  **1009** future_version, **1025** transaction_cancelled, **1036** accessed_unreadable,
  **1037** process_behind, **1062** change_feed_cancelled (NOT wrong-shard), **1079**
  blob_granule_request_failed. `OnError` ≠ `fdb_error_predicate` — they retry different sets;
  see `TestOnError_*`.
- **Continuations / records / index entries / split records are byte-exact vs Java.** A
  data-plane byte difference is a wire-compat bug, never a tolerance.

---

## 3. The workflow — RFC → review → implement → review → merge

One branch, one PR. Drive every non-trivial client change through this. (For a one-line obvious
fix you may collapse the RFC into the PR description — but the review gates are never skipped.)

### Step 1 — RFC
Write `rfcs/NNN-short-title.md`. Number = next free. Include: **Status: Draft**, the **Item** it
closes, the **problem** (with the C++ behavior cited `file:line` from `/tmp/fdbsrc`), the
**proposed Go change**, the **executable spec** (exactly what the test/differential will prove),
and **wire-compat impact** (bytes? conflict ranges? error codes?). The RFC's load-bearing claim
must be a C++ citation, not an assertion.

### Step 2 — review the RFC (all three, in parallel)
- **FDB C++ maintainer** + **Torvalds** — invoke the **`fdb-client-review`** skill (its prompts
  anchor on `/tmp/fdbsrc` 7.3.77 and demand `file:line` citations).
- **`/code-review`** — run it on the RFC diff for a third lens.

Iterate until the FDB C++ maintainer ACKs the approach (no wire divergence, no invented
semantics the C++ lacks) and Torvalds ACKs the plan. Set **Status: Accepted**. **Never start
implementing on a NAK.**

### Step 3 — implement (read C++ first, one logical change at a time)
Port 1:1. Write the test BEFORE assuming the code is right. `just test` (or the targeted
`bazelisk test //pkg/fdbgo/client:client_test`) green on every commit. For a divergence fix,
follow **`hunt-divergences`**: a red→green differential in `pkg/fdbgo/bench` IS the proof.

### Step 4 — review the implementation (same three, again)
Re-invoke **`fdb-client-review`** (FDB C++ maintainer + Torvalds) and **`/code-review`** on the
implementation diff. **An ACK only covers the HEAD it reviewed** — re-request after every new
commit. Fix → add the regression that should have caught it → re-review the delta.

### Step 5 — PR gauntlet (the published gates)
- **codex** — via the **`codex-review`** skill (`gpt-5.5 xhigh`). It repeatedly catches
  storage-shadow / cleared-base / sticky-unreadable / RYW edges the others miss. Re-review the
  delta after each fix; its ACK must be on the final HEAD.
- **@claude** — `@claude` in a PR comment (self-hosted runner; it's queued behind CI on the
  single box, be patient). Its **clean LGTM must be the LAST comment on the PR, on the current
  HEAD.** Every push re-stales it → re-request. Don't comment after the LGTM.
- **CI green** on the final HEAD before merge — no exceptions. A red or flaky CI is a real bug
  (see §5), root-cause it now.

Merge only when **FDB C++ maintainer + Torvalds + code-review + codex + @claude are all green on
the same final HEAD and CI is green.** Update the RFC to **Status: Implemented** and flip the
TODO mark.

---

## 4. Methods

- **Differential vs `libfdb_c`** (`pkg/fdbgo/bench`) — the byte-level oracle: run the same ops
  through both clients against one testcontainer FDB. **Compare at the DATA plane, never the
  wire** (request frames differ per client: reply-promise UIDs, versions, trace IDs, GRV
  batching, chunk boundaries). Compare **persisted bytes byte-exact** (keys, records, index
  entries, version at `pk+\xff`, split chunks, continuation tokens) and **reads semantically**
  (value / merged KV set / error CODE). A data-plane byte diff is a real bug. See
  `hunt-divergences`.
- **Deterministic fault injection** — the client/wire analog of FDB's Sim2/BUGGIFY. Extend the
  frame-proxying dialers in `pkg/fdbgo/client/fault_test.go`:
  - `wrongShardConn` — replaces the next reply frame with a crafted error body.
  - `dropReplyConn` — silently drops replies so the client's RPC timer fires deterministically.
  - `faultConn` / handshake-stall / cancel-while-blocked dialers for transport-teardown races.
- **Ride FDB's own oracles** against a testcontainer cluster the Go client mutated: `C1`
  ConsistencyCheck after Go writes (`pkg/fdbgo/conformance`), `C2` the libfdb_c differential.
  You **cannot** run inside Sim2 (hermetic, no external socket) — that's why deterministic fault
  injection at the wire boundary is our substitute.

---

## 5. Testing discipline (non-negotiable)

- **Real FDB via testcontainers, never mocks.** `t.Parallel()`, unique key prefixes, container
  timeouts (`context.WithTimeout(..., 2*time.Minute)` around `Run()`).
- **Deterministic > flaky — always.** Do NOT pin a transient race with a timing-dependent test;
  that violates the no-flakes rule. Make the race *structurally impossible to lose*:
  - *Case study (#288):* the read-timeout regression first used `rpcTimeoutOverride = 1ns` over a
    real connection and raced the timer against a real reply. It flaked in CI (the real getKey
    reply won every time on the CI box). The fix: a `dropReplyConn` that **drops the reply** so
    the timer always fires — no race. Same lesson as deferring race coverage to a deterministic
    harness rather than shipping a flaky integration repro.
- **t.Parallel + `defer cancel()` footgun:** a parent test with `t.Parallel()` and
  `defer cancel()` cancels its ctx *before* parallel subtests run. Don't blindly add
  `t.Parallel()` to table subtests that share the parent's ctx — it cancels them mid-run.
- **Anti-self-confirming tests:** a fault-injection test must inject the **canonical literal**
  (e.g. `1001`), never the code-under-test's own constant (`ErrWrongShardServer`) — injecting
  the constant passes for any value it holds (exactly how the 1062 wrong_shard bug stayed green
  across shifts). See `fault_test.go` P6 note.
- **Revert-prove every regression:** back out the fix, confirm the test goes red, restore. A
  green suite with the bug still latent is the danger.
- **`-race` the touched package and loop it:** client concurrency bugs hide behind timing.
  `bazelisk test //pkg/fdbgo/client:client_test --@rules_go//go/config:race
  --test_arg=--test.run=TestName$ --nocache_test_results` (and `--runs_per_test=10` to prove a
  determinism fix).
- **`bazelisk`, never `bazel`. Never `--no-verify`** — investigate hook failures.

---

## 6. Code map (Go side)

| File | What |
|------|------|
| `client/database.go` | `Run`/`runTransactCtx`, retry loop, commit dispatch, bootstrap timeout |
| `client/transaction.go` | `Transaction`, `Commit` (fast path, `ensureReadVersion`), `OnError`, `backoffSleep`, RYW, `GetKey`/`Snapshot` |
| `client/commitpath.go` | `tx.commit` = commit RPC + `commit_unknown_result` barrier; tenant prefix (`applyTenantPrefix`); `isRetryable` |
| `client/readpath.go` | `getValue`/`getKey`/`getRange` retry loops, the 3 reply parsers, reply-timeout handling, `sendWatch` |
| `client/ryw.go` + `client/ryw_getkey.go` | RYW overlay; op-type write-map segments (`segKV`/`segPhantom`/`segUnreadable`); `getKeyRYW`, `rywSegmentIterator`/lazy cursor |
| `client/hedge.go` | `sendFrameWithHedge`/`raceReplies`/`waitForReply`, `hedgeResult.others` |
| `client/grv.go` | GRV path, batchers, GRV cache (note: always-on vs C++ `USE_GRV_CACHE` opt-in) |
| `client/rpc.go` | `waitReply` (read path → `errReplyTimeout`), `waitReplyOrProxiesChanged` (commit path → `DeadlineExceeded`) |
| `client/fault_test.go` | fault-injection dialers (`wrongShardConn`, `dropReplyConn`, …) |
| `transport/conn.go` | connection, write/read loops, `failConnection`, `connectionMonitor`, handshake deadline |
| `fdb/error.go` | `IsRetryable` (the high-level predicate — keep in sync with `client.isRetryable`) |
| `fdb/database.go` + `fdb/transaction.go` | Apple-compatible facade (functional options, `Must*` boundary, `panicToError`) |
| `wire/reader.go` + `wire/types/` | `ReadErrorOr`/`ReadInlineReplyError`; generated reply structs (fix the extractor in `cmd/fdb-schema-extract`, don't hand-edit generated) |
| `bench/` | dual-client differential harness (Go vs `libfdb_c`) |

---

## 7. Where the current state lives

- **Authoritative & current:** the `rfcs/NNN-*.md` `Status:` lines + `git log -- pkg/fdbgo/`.
  Trust these over the TODO checkboxes.
- **`TODO.md`** "Native fdbgo client" section + "Known gaps"; **`TODO_client.md`** (the original
  15-finding audit — mostly closed, treat as history); **`TODO-production.md`** P0–P3 (the SaaS
  control-plane roadmap; client items: P1.6/P1.8/P1.9/P2.2/P3.3 are the live ones).
- **`pkg/fdbgo/client/CRASH_BUG.md`** (resolved — keep for the methodology), `fdb/API_PARITY.md`,
  `bench/PERFORMANCE.md`.

When you finish an item, update the RFC `Status:` AND the TODO mark in the same PR — that's how
the next engineer avoids re-auditing closed work.

---

## Hard rules (from CLAUDE.md — non-negotiable)

- **C++ is the spec.** Read `/tmp/fdbsrc` (7.3.77) first; port 1:1; no invented shortcuts. Go
  divergence from C++ is a Go bug — fix Go, never skip a divergence test.
- **Wire compat is the hard line.** Bytes written to FDB (keys, records, index entries,
  continuations, split records, atomic mutations) MUST be byte-identical to Java/C++.
- **No mocks** (real FDB via testcontainers). **No `t.Skip`** except the Docker check. **No red
  CI, no flakes** — a flake is a real concurrency/ordering bug; root-cause it now.
- **Every fix gets a revert-proven regression.** **`-race`** the touched client package.
- **Never paper over** at the surface (string check, tolerance gate) — fix the path the C++ uses.
- The query-engine gate (Graefe) does **not** apply to client work; the **FDB C++ maintainer**
  is the substitute. Don't merge a client/wire change without their ACK on the final HEAD.
