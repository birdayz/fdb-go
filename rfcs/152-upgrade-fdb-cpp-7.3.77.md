# RFC-152 — Upgrade FDB C++ wire-protocol baseline 7.3.75 → 7.3.77

**Status:** Accepted (FDB C++ maintainer ACK + Torvalds ACK + /code-review addressed; impl-phase
re-review on final HEAD per skill)
**Item:** FDB C++ baseline bump (procedure B of the `upgrade-versions` skill). The pure-Go client
(`pkg/fdbgo/`) matches the C++ `libfdb_c` wire protocol; the spec version it is validated against
moves 7.3.75 → 7.3.77.
**Reviewers:** FDB C++ maintainer (client/wire spec, `/tmp/fdbsrc`) + Torvalds (code/process
quality) + `/code-review` + codex + @claude.

**Gate scope.** This RFC's deliverable is the **mechanical baseline bump plus one regression test
proving the single behavioural delta is already covered in Go**. It touches no client algorithm: the
entire 7.3.75 → 7.3.77 C++ delta that is even *adjacent* to the client is a single upstream fix (PR
#12935) whose behaviour the Go transport already has structurally (§3). No Go production-code changes;
the only new code is one deterministic fault-injection test (§5).

---

## 1. Problem (verified)

The client pins the FDB C++ wire protocol at **7.3.75** in `MODULE.bazel`
(`bazel_dep(name = "foundationdb", version = "7.3.75")`, plus `module(...)`, `strip_prefix`, and the
archive `urls`), in `.bazelrc` (`FDB_VERSION=7.3.75`), in the schema-extractor version string, and
across living docs / generated-file headers / test comments. Upstream's current `7.3.x` patch line is
**7.3.77**. We want the pinned wire-protocol spec, the testcontainer image, the schema-extractor
source, and the docs to track 7.3.77 so the Go client is validated against the version teams actually
run, and so future client work reads the current C++ as its oracle.

This is a **patch bump within the 7.3 line** (no major/minor move): no serialization-format change, no
new request/reply types, no error-code changes. Wire compatibility is preserved by construction
(§2/§4). The testcontainer image `foundationdb/foundationdb:7.3.77` and the source tag both exist
(archive returns 200; tag fetched).

## 2. Investigation — the complete C++ delta (7.3.75 .. 7.3.77)

Read the full tree diff (`git diff 7.3.75 7.3.77` in `/tmp/fdbsrc`, all 44 changed files) and the
release notes. The release notes are decisive:

- **7.3.77** — *"Same as 7.3.76 release with AVX enabled."* A compiler/ISA flag only; **zero source
  change** vs 7.3.76.
- **7.3.76** — three entries: (1) **peer-disconnect detection fix in `waitValueOrSignal`** (PR
  #12935); (2) `getTeamByServers` made O(1) (PR #12938) — **server-side Data Distributor**, not the
  client; (3) miscellaneous **observability** improvements for CPU/memory/DD-startup/S3 (PRs #12937,
  #12913, #12997).

Mapping every changed file to a category:

| Category | Files | Client impact |
|---|---|---|
| Build / ISA | `CMakeLists.txt`, `cmake/CompileRocksDB.cmake`, `.gitignore` | none (RocksDB pin, AVX) |
| Server-internal | all `fdbserver/*` (DataDistribution, DDTeamCollection, DDTxnProcessor, DDShardTracker, BackupWorker, VersionedBTree, fdbserver.actor.cpp, workloads, `ServerKnobs.{cpp,h}`, `FDBRocksDBVersion.h`) | none (DD, storage engine, backup, restore — not on the client wire) |
| Observability refactor | `flow/SimpleCounter.{cpp,h}` (new), `flow/Net2.actor.cpp`, `fdbrpc/FlowTransport.actor.cpp`, `flow/Trace.cpp`, `flow/SystemMonitor.{cpp,h}`, `flow/Platform.actor.cpp`, `flow/Arena.{cpp,h}`, `flow/FastAlloc.{cpp,h}`, `flow/Trace.h` | none — `Int64MetricHandle` → `SimpleCounter` migration + trace plumbing; **no framing/behaviour change**. `Net2.actor.cpp`/`FlowTransport.actor.cpp` are 100% counter renames + a cosmetic slow-loop cleanup (verified line-by-line). |
| S3 backup | `fdbclient/S3BlobStore.actor.cpp` | none (Go client implements no S3 blob store) |
| Tracing-only client | `fdbclient/NativeAPI.actor.cpp` | none — the only change is in `waitStorageMetrics`: SevDebug→SevWarn after 60s + added `.detail(...)`; retry semantics (`invalidateCache` + `delay(WRONG_SHARD_SERVER_DELAY)`) **unchanged** |
| Compiler pedantry | `flow/include/flow/IndexedSet.h` | none (`this->template` qualification for the AVX build) |
| **Behavioural, client-adjacent** | `fdbrpc/include/fdbrpc/genericactors.actor.h` | **PR #12935** — the only delta worth analysing (§3) |
| Tests / docs | `fdbrpc/FlowTests.actor.cpp`, `flow/include/flow/UnitTest.h`, `tests/*`, release notes | none |

**Wire-format / error-code / knob invariants — confirmed unchanged** (empty diff across the tags):
`flow/include/flow/error_definitions.h`, `fdbclient/ClientKnobs.{cpp,h}`,
`fdbclient/ReadYourWrites.actor.cpp`, `fdbclient/RYWIterator.cpp`,
`flow/include/flow/{flat_buffers.h,serialize.h,ObjectSerializer.h}`, and **every header the schema
extractor reads** (`StorageServerInterface.h`, `CommitProxyInterface.h`, `GrvProxyInterface.h`,
`CoordinationInterface.h`, `ClusterInterface.h`, `FDBTypes.h`, `GlobalConfig.actor.h`, `Tenant.h`,
`TenantInfo.h`, `FlowTransport.h`). ⇒ Regenerating wire types is a **no-op** except the version
string in each generated file's header comment.

## 3. The one behavioural delta — PR #12935 — and why Go already has it

**C++ change** (`fdbrpc/include/fdbrpc/genericactors.actor.h`, `waitValueOrSignal`): added a new
`when` arm
```cpp
when(wait(peer.isValid() ? peer->disconnect.getFuture() : Never())) {
    return ErrorOr<X>(request_maybe_delivered());
}
```
`waitValueOrSignal` is the per-alternative wait inside `loadBalance` (`LoadBalance.actor.h`). Before
#12935 it waited on the reply OR the `IFailureMonitor` signalling the endpoint failed. The failure
monitor has a **detection lag** (ping interval / CONNECTION_MONITOR_TIMEOUT); during it, a request to
a storage server whose TCP connection had actually dropped would *hang*. #12935 wires the `Peer`'s
own `disconnect` promise into the wait so `loadBalance` returns `request_maybe_delivered` (retryable)
**the instant the connection drops** and immediately tries the next replica.

**Why Go is unaffected — architectural, verified in code:** the pure-Go client has **no separate
failure-monitor with a lag to bridge.** The connection's `readLoop` *owns* the socket
(`transport/conn.go`). The moment `fr.Read` returns EOF/RST (peer disconnect), the deferred
`failConnection(err)` runs `failAllPending(err)` (conn.go:707, 757-769), which delivers the error to
**every** in-flight reply channel immediately. The code comment already states it "Matches C++
connectionKeeper's disconnect promise that wakes all in-flight `deliver()` actors." Downstream:
- `processReply` turns any `Response.Err` into `hedgeResult{err, connErr:true}` (hedge.go:184-193);
- `sendGetValue` on a non-nil result error **falls back to the remaining replicas sequentially**
  (readpath.go:527-559) — the Go analog of `loadBalance` trying the next alternative — then flattens a
  no-reachable-server outcome to `all_alternatives_failed` (1006);
- `getValueImpl` absorbs 1006 (invalidate cache + retry, then a bounded exhaust to the retryable
  `transaction_too_old` 1007) and **never surfaces it to the app** (readpath.go:434, 444-449),
  exactly as "libfdb_c never propagates all_alternatives_failed to the application."

So Go detects peer disconnect *immediately* (no lag) and retries transparently — the behaviour
#12935 *added* to C++. Go never had the bug #12935 fixed. **No production-code change is required.**

## 4. The bump (this RFC's deliverable)

1. **`MODULE.bazel`** — `7.3.75` → `7.3.77` in all four spots (`bazel_dep`, `module(...)`,
   `strip_prefix`, archive `urls`).
2. **`.bazelrc`** — `FDB_VERSION=7.3.77`.
3. **Schema extractor** — `cmd/fdb-schema-extract/main.cpp` generated-header version string →
   `7.3.77`; run `just generate-wire-types` + `just gazelle`. Expected diff: **only** the
   `// Code generated by fdb-schema-extract v5 from FDB 7.3.77.` header line in each
   `pkg/fdbgo/wire/types/*_generated.go` (struct layouts byte-identical — §2).
4. **Binding tester / infra** — `cmd/fdb-stacktester/bindingtester/Dockerfile` (`ARG FDB_VERSION`),
   `cmd/fdb-stacktester/bindingtester/binding_test.go` version helper, `infra/main.tf`,
   `infra/cloud-init.yaml`, `infra/README.md`.
5. **Living docs (forced by the `pkg/docscheck` guard)** — `TestLivingDocsCiteCurrentFDBVersion`
   requires every FDB-keyword-adjacent `7.x.y` in README.md, PRODUCTION_READINESS.md, TODO.md,
   DIVERGENCES.md, CHANGELOG.md, RELEASE.md to equal the pin. Flip them; add a CHANGELOG Unreleased
   Compatibility note recording the bump and the #12935 finding.
6. **Other prose / comments / source permalinks (sweep ALL non-historical refs).** The bump is only
   "complete" if every live `7.3.75` reference flips — `grep -rn "7\.3\.75"` and triage each. Beyond
   the obvious docs (`pkg/fdbgo/wire/serializer.go`, `pkg/fdbgo/README.md`,
   `pkg/fdbgo/client/CRASH_BUG.md`, `pkg/fdbgo/bench/PERFORMANCE.md`, `pkg/fdbgo/libfdbc/backend.go`,
   `docs/wire-format-static-vs-logic.md`) this MUST include the clusters a first pass misses (codex/
   code-review finding):
   - **C++ source permalinks** `github.com/apple/foundationdb/blob/7.3.75/...` — `c_binding_port_test.go`
     (~45 refs), `cycle_workload_test.go`, `atomicops_workload_test.go` etc. §1's goal ("future client
     work reads the *current* C++ as its oracle") is undercut if these still point at 7.3.75 source.
   - **Docker-image quickstarts** users copy-paste: `cmd/frl/demo/README.md`, `cmd/fdb-stacktester/README.md`
     (`foundationdb/foundationdb:7.3.75`), plus the `7.3.75 server` comment in `cmd/frl/internal/cmd/store.go`.
   - **Spec-version comments in tests:** `differential_errorcode_test.go`, the `@ tag 7.3.75` /
     `7.3.75 testcontainer` comments, `transport_test.go`, `correctness_test.go`, `setget_test.go`.
   - **Non-living-doc trackers** the `pkg/docscheck` guard does NOT cover: `TODO-production.md`
     (lines ~622/745-749, which currently *assert* "all FDB references reconcile on 7.3.75" — a claim
     that goes false after the bump), `TODO_client.md:308`, `DIVERGENCES.md` heading.
   - **Skill files** that cite the spec version (`.claude/skills/{fdb-client-engineer,fdb-client-review,
     hunt-divergences,todo-worker,upgrade-versions}/SKILL.md`) — these define "the spec is 7.3.x" for
     future work; flip to 7.3.77 so the oracle stays current.

   (Genuinely historical/snapshot refs — `shifts/*`, tagged CHANGELOG/RFC entries, prod-readiness audit
   dated snapshots — are left as-is.)

## 5. Executable spec (the test this RFC adds) — **landed & proven**

`TestPeerDisconnect_FailsInFlightReplyImmediately` (`pkg/fdbgo/client/fault_test.go`) pins §3's claim
that Go already has the #12935 behaviour, so the bump is provably a no-op. It isolates the load-bearing
guarantee at the transport boundary (not the full read-retry loop — see the design note below):

- Real FDB testcontainer; seed + warm so `getOrDial(storageAddr)` returns a live, handshook conn.
- Register an in-flight reply token via `PrepareReply()` but send **no request**, so the server can
  never answer it — the *only* path that can wake `replyCh` is the connection teardown
  (`failConnection → failAllPending`). This removes the timer-vs-real-reply race (#288 lesson).
- Close the underlying socket — a faithful peer disconnect: the storage conn's `readLoop` discovers it
  via EOF (the §3 mechanism), not an explicit local `Close()`.
- Assert `replyCh` wakes with a connection error in **< 2s** (a generous CI margin; real teardown is
  sub-millisecond and far below `DefaultRPCTimeout` 5s, so a regression to "wait for the timeout" is red).

**Result: the in-flight reply fails in ~721ns (~56µs under `-race`)** — sub-millisecond, conclusively
fail-fast. **Revert-proven:** stubbing out `failConnection`'s `failAllPending(err)` call makes
`replyCh` never wake → the 2s arm fires → red (confirmed), then restored.

*Design note (why transport-boundary, not end-to-end through `tx.Get`):* an initial e2e variant drove a
real read whose storage reply was dropped + socket closed on every attempt. It surfaced a **test
artifact**, not a bug: on a single-node testcontainer the storage server and the commit/GRV proxy share
one network address, so arming that address also faulted the `locate` (GetKeyServerLocations) path; the
cache-invalidating read-retry loop then re-located through the same killed connection until the test ctx
deadline. The run still *confirmed* fast detection (163 disconnects in 55s ⇒ ~0.34s/cycle, never the 5s
RPC timeout), but the transport-boundary test proves the same guarantee deterministically and without the
single-node address conflation. The end-to-end read-path retry/absorption behaviour (replica fallback,
1006 absorbed to retryable 1007, never surfaced) is already covered by `TestWrongShardServer_*`.

## 6. Wire / behaviour impact

**None.** No persisted-byte change, no error-code change, no knob/retry/RYW change, no new wire types
(§2). The regenerated `*_generated.go` differ only in the header version comment. The 7.3.77 image is
wire-compatible with 7.3.75 (patch bump within 7.3). The client behaviour the only code-adjacent
upstream fix targets is already present in Go (§3) and now pinned (§5).

## 7. Test plan

- `just generate-wire-types && just gazelle` — diff is header-comment-only across
  `pkg/fdbgo/wire/types/*_generated.go` (confirms §2 "no struct change").
- `TestPeerDisconnect_FailsInFlightReplyImmediately` green (and revert-proven red).
- `//pkg/fdbgo/client:client_test` (+ `-race` on the new test) green against the 7.3.77 container.
- `just binding-stress 1 100` smoke (wire-format drift surfaces here) green.
- `pkg/docscheck` doc-consistency guard green (all living docs track the 7.3.77 pin).
- `just test` full suite green.

## 8. Scope

**One PR:** MODULE.bazel + .bazelrc + regenerated `*_generated.go` + schema-extractor version string +
binding/infra pins + living-doc flips + the §4.6 full `7.3.75`→`7.3.77` sweep (prose, test-comment
spec-version refs, C++ source permalinks, docker quickstarts, non-living-doc trackers, skill files) +
the new regression test + this RFC. No production-code behaviour change.

**Completeness note (review finding):** only the six living docs + the functional pins are guarded by
`pkg/docscheck`, so a half-swept bump would still go *green*. The §4.6 sweep is therefore a manual
discipline, not a guard-enforced one — the closing checklist (`grep -rn "7\.3\.75"` returns only
intentional historical refs) is the gate. The end state: every non-historical `7.3.75` reference is
`7.3.77`.
