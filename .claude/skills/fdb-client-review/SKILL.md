---
name: fdb-client-review
description: Work on the pure-Go FoundationDB client (pkg/fdbgo — transport, transaction, commit path, RYW, key selectors, retry/ctx, wire encoding). Includes mandatory review by the FDB C++ client developer (validates Go against the 7.3.75 C++ source — the spec) and Torvalds (code quality). The client/wire analog of the query-engine skill: the FDB C++ dev substitutes for Graefe on client/wire items.
---

# FDB Client Work (pure-Go `pkg/fdbgo`)

You are working on the from-scratch pure-Go FoundationDB client. This is wire- and
behaviorally-load-bearing: Go, C, and Java apps share one FDB cluster and read/write each
other's data through it. **C++ (`libfdb_c`) is the spec.** Any place the Go client behaves
differently from the C++ client is a **bug in Go**, not a "Go choice" — until proven
otherwise by reading the C++ source (CLAUDE.md design principle #2). Every change here must
pass two virtual reviewers before shipping, plus the PR gates.

This is the **client/wire analog of the `query-engine` skill**. There, Graefe is final on
Cascades alignment; here, the **FDB C++ client developer** is final on client/wire
correctness. Load this skill for any change under `pkg/fdbgo/` (transport, transaction,
commit path, RYW, key selectors, retry/ctx, error mapping, wire encoding) and for any
record-layer code that touches the client contract (continuations, split records, atomic
mutations).

## The two reviewers

### FDB C++ client developer (substitute for Graefe on client/wire items)
- Evaluates the change **against the actual C++ source at tag 7.3.75** — `libfdb_c`
  semantics, not a remembered approximation. Their word is final on client/wire
  correctness, exactly as Graefe's is final on Cascades.
- Cites `file:line` in the C++ for every verdict. "Java/C++ does X here (`NativeAPI.actor.cpp:NNNN`),
  Go must do X" is the shape of an ACK condition.
- Key principle: **wire compat is the hard line.** A change that makes Go write different
  bytes, add/drop a conflict range, map an error code differently, or resolve a key
  selector differently than `libfdb_c` is a NAK regardless of how clean the Go reads.
- Key principle: **don't invent semantics the C++ doesn't have.** A "pragmatic" shortcut
  (merged GetRange + offset index for getKey-RYW, a non-zero default tx timeout, an
  unbounded retry that C++ bounds, a forced GRV on a path C++ skips) is a divergence even
  when it looks like an improvement. Match `libfdb_c`'s default exactly.
- Will catch: forced GRV on the no-op/read-only fast path; conflict-range add/drop;
  sticky-state (`is_unreadable`) mishandling; atomic-op behavior on empty/missing/
  present-empty values; retry-predicate drift from the C++ `IsRetryable`; ctx/cancellation
  semantics that don't match `libfdb_c`'s timeout/cancel model; `commit_unknown_result`
  idempotency-barrier regressions.

### Linus Torvalds (code reviewer)
- Evaluates **code quality**: dead code, logic holes, incomplete conversions, papered-over
  regressions, revert-proofs missing, scope honesty (does the test actually prove the fix).
- Blunt and specific — `file:line` references.
- Will catch: a "fix" with no red→green proof; a recover() that hides a real error path; a
  guard that duplicates an existing one instead of unifying; concurrency footguns
  (shared-field writes without atomics — the `hadRead` class); comments that lie about what
  the code does.

The PR-level gates that follow these two (see "Review gauntlet" below): **codex**
(repeatedly catches storage-shadow / cleared-base / sticky-unreadable edges the others
miss) and **@claude** on the GitHub PR (final gate; LGTM must be on the final HEAD).

## The spec: C++ source 7.3.75

The oracle is the **FoundationDB C++ source at tag 7.3.75** — the `foundationdb` pin in
`MODULE.bazel` (`bazel_dep(name = "foundationdb", version = "7.3.75")`), which is exactly
what `libfdb_c` (and the cgo binding the differential harness compares against) is built
from. **Do NOT confuse this with `4.11.1.0`**, the *Java* `fdb-record-layer-core` version —
a different project with a different spec.

Checkout (already present in this environment; clone if missing):
```bash
ls /tmp/fdbsrc || git clone --branch 7.3.75 https://github.com/apple/foundationdb /tmp/fdbsrc
( cd /tmp/fdbsrc && git describe --tags )   # must print 7.3.75
```

Key C++ files (read the **actual** function before porting/verifying):

| C++ file | What |
|----------|------|
| `fdbclient/NativeAPI.actor.cpp` | `Transaction::commit`/`tryCommit`, GRV (`getReadVersion`), `commitMutations`, `raw_access` resolution |
| `fdbclient/ReadYourWrites.actor.cpp` | RYW read/write overlay, `getRange`/`get`/`getKey` over pending |
| `fdbclient/WriteMap.cpp`, `RYWIterator.cpp` | RYW write map; `is_unreadable` stickiness |
| `fdbclient/include/fdbclient/Atomic.h` | atomic-op semantics (`doMinV2`/`doAndV2` gate on `Optional.present()`) |
| `fdbclient/ClientKnobs.cpp` | retry limits, backoff, timeouts — the defaults Go must match |
| `fdbclient/SystemData.cpp` | system keyspace, conflict-range construction |
| `flow/` (e.g. `genericactors.actor.cpp`) | cancellation / `WithoutCancel` analog, future teardown |

## Workflow

### 1. Read the C++ first

Before writing ANY client code, read the corresponding C++ function. Understand the
algorithm, the state machine, the exact branch the case hits. Then port it 1:1.

```bash
grep -rn "tryCommit\|getReadVersion" /tmp/fdbsrc/fdbclient/NativeAPI.actor.cpp
```

For the SQL→client correspondence, also confirm what the cgo binding (`libfdb_c`) does
observably — the differential harness in `pkg/fdbgo/bench/` is the byte-level oracle.

### 2. Implement with tests

One logical change at a time. Write the test BEFORE assuming the implementation is correct.
Run `just test` after every change. Commit on green. Two test shapes, pick by item type:

- **Divergence fix** (Go behaves differently from `libfdb_c`): use the **`hunt-divergences`
  skill** — a red→green differential in `pkg/fdbgo/bench/` (byte-compare both clients against
  one real FDB) plus a focused regression. The differential being red before and green after
  IS the proof.
- **Correctness/availability fix with no observable byte divergence** (ctx threading,
  cancellation, retry bounds, deadlock): a deterministic regression that fails pre-fix and
  passes post-fix. For commit-path / GRV / cancellation timing, extend the fault infra in
  `pkg/fdbgo/client/fault_test.go` (e.g. a frame-level dialer that delays a specific RPC
  reply on a `releaseCh`, arms after the request, then cancels the ctx while blocked).
  **No fake checkboxes** — the test must exercise the real path end-to-end, and you must
  revert-prove it (back out the fix, confirm red, restore).

Determinism: client concurrency bugs hide behind timing. Run the new regression and the
touched package under `-race`, and loop it:
```bash
bazelisk test //pkg/fdbgo/client:client_test \
  --@rules_go//go/config:race \
  --test_arg="--test.run=TestName$" --test_arg="--test.v" --nocache_test_results
```

### 3. Review cycle (MANDATORY)

After implementation passes all tests, launch BOTH reviewers in parallel as background
agents. The FDB C++ reviewer prompt MUST anchor on `/tmp/fdbsrc` (7.3.75) and demand
file:line citations:

```
Agent(description: "FDB C++ client review", prompt: "You are a senior FoundationDB C++ client developer. The spec is the C++ source at /tmp/fdbsrc, tag 7.3.75 (the `libfdb_c` this Go client must match byte- and behavior-for-behavior). Review the diff in /home/birdy/projects/fdb-record-layer-go — run `git diff master..HEAD`. [describe what changed and why]. For EVERY claim cite the C++ function and file:line in /tmp/fdbsrc that the Go code must match. Check: does Go now match libfdb_c's commit/GRV/retry/conflict-range/error-code/cancellation semantics exactly? Any forced GRV the C++ skips? Any wire-byte / conflict-range / error-code divergence? Under 300 words. ACK or NAK with specific C++ file:line reasons.", run_in_background: true)

Agent(description: "Torvalds code review", prompt: "You are Linus Torvalds. Review the diff in /home/birdy/projects/fdb-record-layer-go — run `git diff master..HEAD`. [describe what changed]. Focus on dead code, logic holes, incomplete conversions, papered-over regressions, missing red→green / revert-proof, concurrency footguns (shared-field writes without atomics). Under 300 words. ACK or NAK with file:line.", run_in_background: true)
```

### 4. Address findings

- **FDB C++ NAK**: client/wire divergence. The fix is "do what the C++ does" — read the
  cited function again and port the exact semantics. Never argue the divergence is
  acceptable without a C++ citation proving it is.
- **Torvalds NAK**: code quality. Usually concrete — delete dead code, add the revert-proof,
  unify the duplicate guard, atomic the shared field.
- **Both ACK**: proceed to PR.

Do NOT ship with a NAK from either reviewer. Iterate until both approve. A stale ACK does
not cover later commits — re-request after every new commit.

### 5. Review gauntlet on the PR (these are wire/client changes)

Client divergences are subtle and different reviewers catch different classes. On the PR,
after FDB C++ + Torvalds ACK the implementation:
- **codex** (`codex -s read-only -a never review --base <master-sha>`) — the gate that
  repeatedly caught storage-shadow / cleared-base / sticky-unreadable edges the others
  missed. Do not skip it; re-review the delta after each fix.
- **@claude** on the GitHub PR — final gate; the clean LGTM must be on the final HEAD
  (todo-worker Steps 10–11).

## When to use this skill

- Any change under `pkg/fdbgo/` (transport, `client/`, `fdb/`, wire encoding).
- A `TODO-production.md` "client robustness" item (commit-path GRV/ctx, handshake timeout,
  watch escape, retry bounds, the deadlock follow-ups).
- A continuation / split-record / atomic-mutation / index-entry change whose **bytes** must
  match Java/C++ (wire compat).
- A differential or fuzz mismatch in `pkg/fdbgo/bench/` → load `hunt-divergences` (the
  find-and-root-cause method) AND this skill (the review gate for the fix).

If the item is purely SQL/planner with no client contract, use `query-engine` instead.

## Key files (Go side)

| File | What |
|------|------|
| `pkg/fdbgo/client/database.go` | `FDBDatabase.Run`/`runTransactCtx`, retry loop, commit dispatch (`tx.Commit(...)`), bootstrap timeout |
| `pkg/fdbgo/client/transaction.go` | `Transaction.Commit` (fast-path return, `ensureReadVersion`, `tx.commit`), `OnError`, `backoffSleep`, RYW |
| `pkg/fdbgo/client/commitpath.go` | `tx.commit` = commit RPC + `commit_unknown_result` barrier (`commitDummyTransaction`); RFC-090 idempotency |
| `pkg/fdbgo/client/readpath.go` | `sendWatch` long-poll, read dispatch, GRV cache |
| `pkg/fdbgo/transport/conn.go` | `dialWith` handshake (deadline + cancellation watcher), read/write loops, `failAllPending`, `connMu` |
| `pkg/fdbgo/fdb/error.go` | `IsRetryable` — must match the C++ predicate exactly |
| `pkg/fdbgo/client/fault_test.go` | fault-injection dialer (`wrongShardConn`) — extend for RPC-reply-blocking / cancel-while-blocked tests |
| `pkg/fdbgo/bench/` | dual-client differential harness (Go vs `libfdb_c`) — the byte-level oracle |

## Hard rules (from CLAUDE.md — non-negotiable)

- **C++ is the spec.** Read `/tmp/fdbsrc` (7.3.75) first; port 1:1; no invented shortcuts.
  Go divergence from C++ is a bug in Go — never skip a divergence test, fix Go.
- **Wire compat is the hard line.** Bytes written to FDB (keys, records, index entries,
  continuations, split records, atomic mutations) MUST be byte-identical to Java/C++.
- **No mocks** — real FDB via testcontainers. **No `t.Skip`** except the Docker check.
- **No red CI, no unrelated flakes** — a flake is a real concurrency/ordering bug
  (transaction-conflict 1020, timeout 1007, watch-not-firing); root-cause it now.
- **Every fix gets a regression that revert-proves** (red before, green after). A green
  suite with the bug still latent is the danger.
- **`-race` on touched client packages** — the `hadRead` `atomic.Bool` fix (P1.1) is the
  template: a shared field written from pipelined-read goroutines is a data race.
- **Never paper over** at the surface (string check, tolerance gate). Fix the path the C++
  uses. **`bazelisk`, never `bazel`; never `--no-verify`.**
