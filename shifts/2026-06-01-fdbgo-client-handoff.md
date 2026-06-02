# Handoff — native fdbgo client (RFC-010), 2026-06-01

For a fresh agent picking up the **pure-Go FDB client** (`pkg/fdbgo/*`). Two parts:
(A) where the work stands + what's next, (B) **how to work on this client** — the
methodology that found and fixed these bugs. Read part B before touching wire code.

---

## A. State

### What landed — PR #223, merged to master (merge `84c511de`)

**RFC-010 Phase 0** — five wire-correctness fixes to the read/commit path, each
verified against the FDB 7.3 C client and pinned by regression tests. RFC:
`rfcs/010-native-client-correctness.md`. Source audit: `TODO_client.md` (Codex).

| # | Fix | One-line why | Commit |
|---|-----|--------------|--------|
| #2 | `ErrWrongShardServer` 1062 → **1001** | 1062 is `change_feed_cancelled`; wrong-shard reads never retried. Fault test was self-confirming. | `e63cbd3d` |
| #8 | `ReadErrorOr` parses the **union tag**, not field count | a 1-field success (`SplitRangeReply`) was misread as an Error → silent data loss | `5ddb2760` |
| #5 | release hedge-**loser** QueueModel delta | loser/timeout/cancel deltas leaked → biased server selection | `7185f904` |
| #1 | decode the **inline** `LoadBalancedReply.error` on reads | the SS delivers wrong-shard for reads through this field, not the ErrorOr root — Go ignored it | `c0d2491b` |
| #3 | pipelined `Get` shares the full classify→invalidate→retry | `Resolve` skipped wrong-shard retry; `fdb.Get` fell back on the wrong errors | `274d28f3`, `4425e3ea` |

Plus error/edge coverage (every `Resolve` arm, both accounting paths, GetKey/GetRange
wrong-shard, four ErrorOr edge states) and a Phase-1 roadmap in `TODO.md` →
section **"Native fdbgo client — conformance & differential testing"** (C1–C4).

### What's next (prioritized)

The audit had 15 findings; Phase 0 closed 5. Remaining, by severity:

- **HIGH — do next:**
  - **#4 tenant commit mutation** (`commitpath.go:267`): the no-tenant unsafe-cast aliases
    `tx.mutations`, and the tenant prefix loop rewrites `m.Param1`/`Param2` + versionstamp
    offset **in place** → double-prefix on a rebuild. Fix: build a scratch `[]MutationRef`
    with copied headers for the tenant path; keep the alias only for no-tenant. **Fully
    testable now** (build-twice → no double-prefix, `tx.mutations` unmodified). Clean, no new infra. **Recommended next item.**
  - **#6 connection shutdown** (`transport/conn.go`): two bugs — (1) a `Close`/cancel race
    strands a `SendFrame`/`Flush` caller on `errCh` forever (writeLoop exits on `ctx.Done`
    without notifying queued waiters); (2) `connectionMonitor` declares a conn dead via
    `cancel()` but never closes the socket or fails pending replies → fd+goroutine leak,
    pending reads only wake on RPC timeout. Fix: one `failConnection(err)` path
    (cancel + close socket + fail pending + drain queued writers), used by Close, readLoop
    error, and the monitor. **The failure is a race — test it on SimTransport (below), not
    real network.**
- **MEDIUM:** #10 (decouple `ACCESS_SYSTEM_KEYS` from `LOCK_AWARE` — the conformance-corrected
  direction; the facade currently couples them, which diverges from C), #11 (wire TLS through
  the dial path or drop the README claim), #7 (honor or narrow the "concurrent transaction
  methods" contract — snapshot mutations/conflicts under `conflictMu` in Commit/marshal/reset).
- **LOW:** #13 (reply-channel never pooled on success — hot-path alloc), #15 (range-iterator
  `append([]byte(lastKey),0)` aliasing — mirror the reverse path's copy), #9 (`isSystemKey`
  → `isSpecialKey` rename; behavior is correct), #14 (monitor ping under a full writeCh).
  #12 was a **false positive** (locality never panics — invariant guarantees non-empty); add a
  defensive guard at most.

- **INFRA / levers (high leverage):**
  - **SimTransport** — a seeded in-process fake cluster behind `transport.DialFunc`, extending
    the existing `wrongShardConn`. This is the unblocker for: #6's race test, the **C4** deferred
    gaps (inline-error on the GetKey/GetKeyValues *parsers*, `Resolve` flush-error arm, range
    wrong-shard across a partial continuation, future_version/process_behind backoff), and
    deterministic fault injection generally. Build this before/with #6.
  - **C1** ConsistencyCheck-after-Go-writes; **C2** differential vs `libfdb_c` (DATA-plane only —
    see B); **P1** one generated error taxonomy (kills the #2 class permanently).

### Open formality
The Codex re-confirm of the one-line container-ctx fix (`ee4cd77f`) was quota-deferred
(5h limit) and is now **moot** — PR merged, fix verified by Torvalds + @claude.

---

## B. How to work on the fdb client

### 1. C++ is the spec — and it is NOT vendored. Fetch it.
Go divergence from the FDB C/C++ client is a Go bug. The wire types are generated from
**FDB 7.3.75**; review against the `apple/foundationdb` **release-7.3** branch. The C++
source is not in the repo — fetch raw files when a wire question is non-obvious:
`https://raw.githubusercontent.com/apple/foundationdb/release-7.3/<path>` and quote them.
Key files: `flow/error_definitions.h`, `flow/flat_buffers.h`, `flow/include/flow/{Error.h,flow.h,Arena.h}`,
`fdbrpc/{LoadBalance.actor.h,QueueModel.cpp,include/fdbrpc/Smoother.h}`,
`fdbserver/storageserver.actor.cpp`, `fdbclient/NativeAPI.actor.cpp`,
`fdbclient/include/fdbclient/StorageServerInterface.h`. **Never guess wire layout.**

### 2. Wire-format truths you must internalize (these bit us)
- **Two error channels for reads.** A read reply can carry an error two ways: (a) the
  `ErrorOr<T>` *root* union (transport/`sendError`), and (b) the **inline** `LoadBalancedReply.error`
  inside a "successful" reply. **The SS uses (b) for read wrong_shard/future_version/process_behind**
  (`storageserver.actor.cpp` `sendErrorWithPenalty`), NOT the root. Read parsers must check both.
- **ErrorOr is a `union_like`:** root object vtable `{8,9,8,4}` — uint8 tag at slot 0
  (**1=Error, 2=value, 0=NONE**), value RelativeOffset at slot 1. Decide success/error by the
  **tag**, never by field count (a 1-field success is indistinguishable from a 1-field Error).
- **`Optional<Error>` is a nested Error TABLE** (uint16 code at the table's slot 0), reached by a
  RelativeOffset — **not** a length-prefixed byte string. The **generated reply structs
  mis-decode it** (`m.Error = r.ReadBytes(...)`) — a schema-extractor bug; use
  `wire.ReadInlineReplyError` and ignore the generated `.Error` field.
- **Error codes are `uint16`.** Read with `ReadUint16`, not `ReadInt32`.
- Error codes that matter: 1001 wrong_shard_server, 1006 all_alternatives_failed, 1009
  future_version, 1037 process_behind, 1062 change_feed_cancelled (NOT wrong-shard).
- `OnError` ≠ `fdb_error_predicate`: 1006 is read-path-absorbed (NOT OnError-retryable), so it
  must never surface from the pipelined path; 1039 is predicate-retryable but not OnError-retried;
  1079 the reverse. (See `TestOnError_NonRetryable`.)

### 3. The review discipline — every change passes independent gates before merge
This is the core of the workflow. Run them in parallel; never merge with a NAK or a stale LGTM.
- **Torvalds** (subagent) — code quality: dead code, logic holes, papered-over regressions,
  and **non-vacuous tests** (would it fail if the fix were reverted?).
- **FDB-C-programmer** (subagent) — **wire conformance vs release-7.3**. This is the client's
  equivalent of Graefe-for-the-planner: Graefe reviews the Cascades planner; the FDB-C-programmer
  reviews wire/client correctness. Skip it (with cause) only when a delta has zero wire content.
- **Codex** (external, `/codex-review` skill) — independent second engine. It earned its keep:
  caught the #3 1006 fallback regression *and* the CI container leak that Torvalds + @claude
  both passed. Treat its findings as real until disproven.
- **@claude** (the GitHub bot, `@claude` in a PR comment) — the authoritative published gate.
  Its **clean LGTM must be the LAST comment on the PR, on the CURRENT HEAD.** Every push
  re-stales it → re-request. Don't post anything after the LGTM (it dethrones it).

Findings → fix → add the regression test that should have caught it → re-review the delta.
Drive via the `/todo-worker` skill (RFC → reviewers → implement → codex → @claude → merge),
**but skip Graefe and use the FDB-C-programmer reviewer instead** for client work.

### 4. Testing
- Real FDB via **testcontainers, never mocks**; `t.Parallel()`, unique key prefixes, container
  timeouts. Pre-commit runs full `just test` (gofmt + lint + build + tests) — it blocks on
  formatting too; run `gofmt -w` after `cat >>`-appending test bodies.
- **Deterministic > flaky.** Do NOT write a flaky test for a transient race — that violates the
  no-flakes rule. If a path is only reproducible via a race (e.g. drop-between-dial-and-send),
  pin the *deterministic* sub-fact (a unit test of the decision) and build SimTransport to make
  the race testable. This is why several real bugs are pinned by wire-unit tests + a TODO-C4
  deferral rather than a flaky integration repro.
- **Anti-self-confirming tests (the #2 lesson):** a fault-injection test must inject the
  **canonical literal** (e.g. `1001`), never the code-under-test's own constant
  (`ErrWrongShardServer`) — injecting the constant means the test passes for any value it holds,
  which is exactly how the 1062 bug stayed green across shifts. See `fault_test.go` + the P6 note.
- Hand-constructing internal types in tests is legitimate (e.g. `PendingGet{flushed:true,...}` with
  a real tx + a pre-loaded `replyCh` to exercise a `Resolve` arm) — see `fault_test.go`,
  `hedge_test.go`. Wire decode tests build buffers from `types.ErrorOrError`/`VoidReply` and poke
  the union tag at known offsets (`erroror_test.go`).

### 5. Codex CLI gotchas (now documented in the codex-review skill)
- Global flags (`-s`, `-a`, `-m`, `-c`) go **before** the subcommand.
- `review --base`/`--commit` **cannot** take a custom `[PROMPT]` (mutually exclusive). For a
  steered analysis use `codex exec "PROMPT" < /dev/null` — the `< /dev/null` is **mandatory**
  (exec reads stdin to append; without it it hangs printing "Reading additional input from stdin").
- **codex exits 0 with EMPTY output when rate-limited** (5h/weekly quota). An empty
  `/tmp/codex-review.md` is a FAILED run, not a clean pass — check stderr for quota/auth.

### 6. "Riding along" on FDB's own testing — what's possible
You **cannot** run our client inside Sim2 (hermetic single-threaded Flow event loop, in-memory
network, no external socket; BUGGIFY is sim-only). You CAN ride three real artifacts against a
testcontainer cluster our client mutated: **(C1)** FDB's `ConsistencyCheck` oracle, **(C2)** the
official `libfdb_c` binding via a differential, **(C3)** their workload *designs* (Cycle, etc.)
re-implemented as scenario+invariant tests. **SimTransport** is our analog of Sim2/BUGGIFY for the
*client* layer — deterministic fault injection at the wire boundary.

The **C2 differential must compare at the DATA plane, never the wire**: request frames differ per
client (reply-promise UIDs, read/committed versions, trace/span IDs, GRV batching, mutation/conflict
ordering, range chunk boundaries). Compare: **persisted bytes byte-exact** (key/tuple encoding,
record/index format, version at `pk+\xff`, split chunking, continuation-token bytes) and **reads
semantically** (value / merged KV set / error CODE), with control-plane fields excluded. A
data-plane byte difference is a real wire-compat bug, not a tolerance.

### 7. Map of the code
- `pkg/fdbgo/client/transaction.go` — Transaction, `GetPipelined`/`PendingGet.Resolve`, error
  constants, `OnError`, `isSystemKey`, RYW legal-key checks.
- `pkg/fdbgo/client/readpath.go` — `getValue/getKey/getRange` retry loops, the 3 reply parsers,
  `isWrongShardServer`/`isAllAlternativesFailed`/`isFutureVersionOrProcessBehind`.
- `pkg/fdbgo/client/hedge.go` — `sendFrameWithHedge`/`raceReplies`/`waitForReply`, `hedgeResult.others`.
- `pkg/fdbgo/client/loadbalance.go` — `QueueModel`, `smoother`, server selection.
- `pkg/fdbgo/client/commitpath.go` — commit request build, `applyTenantPrefix` (the #4 site).
- `pkg/fdbgo/wire/reader.go` — `Reader`, `ReadErrorOr(Into)`, `ReadInlineReplyError`, `ReaderAtRootObject`, `readNestedInto`.
- `pkg/fdbgo/wire/types/erroror.go` + `*_generated.go` — ErrorOr/VoidReply writers + generated reply structs (DON'T hand-edit generated; fix the extractor in `cmd/fdb-schema-extract`).
- `pkg/fdbgo/transport/conn.go` — connection, write/read loops, monitor (the #6 site).
- `pkg/fdbgo/fdb/transaction.go` — Apple-compatible facade (`Get` → GetPipelined fallback).
- Tests: `fault_test.go` (fault dialers + `newWrongShardTestDB`), `hedge_test.go`,
  `readpath_unit_test.go`, `wire/types/erroror_test.go`.

### Quick commands
- Targeted test: `bazelisk test //pkg/fdbgo/client:client_test --test_output=errors --test_filter='TestName'`
- Fast unit iteration: `go test ./pkg/fdbgo/wire/... -run 'TestX' -count=1` (note: `go test` on
  `wire/types` shows a pre-existing `ground_truth_test` failure due to `testdata.json` not being
  generated outside bazel — bazel is the gate, and it's green).
- Fuzz: `go test ./pkg/fdbgo/wire/ -run '^$' -fuzz '^FuzzReadErrorOrInto$' -fuzztime=15s`
