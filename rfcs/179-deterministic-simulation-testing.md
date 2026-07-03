# RFC-179 — Deterministic Simulation Testing (DST) for the record & relational layers (Track A)

**Status:** Draft / exploration. No code yet. Seeking direction before committing to a build order.
**Scope decision (see §1a):** this RFC is **Track A** — *real* DST for the record + relational layers
(shared Tier 0 + SimFDB + workload/replay). The **client-transport** work (**Track B** — simulation &
fault injection, honestly *not* full DST) is documented here for context (Tier 3 + the alternatives
survey) but **ships separately**: its own RFC or an extension of RFC-118 (SimTransport).
**Relates to:** RFC-002 (chaos-testing), RFC-118 (SimTransport — Track B home), RFC-167/164 (Cascades plan determinism / NONDETERMINISM line-items).
**Reference spec:** FoundationDB C++ Sim2/Flow (`/home/birdy/projects/foundationdb`, tag 7.3.75).

---

## 1. What we're actually asking for

Real DST, the FoundationDB kind: **one seed drives the entire run** — every random choice, every
injected fault — against a **simulated clock** and a **simulated backend/network** you fully control,
so that a bug found once is reproducible *forever* by re-running the seed. Inject conflicts,
`commit_unknown_result`, `transaction_too_old`, network partitions, delays, reordering, dropped
replies, process death — at chosen or seeded points — and assert invariants hold. When something
breaks, print the seed; `go test -run X -seed=N` puts you back on the exact failure in a debugger.

This is a *superset* of what we have. Today `chaos` (RFC-002) and `SimTransport` (RFC-118) inject
faults, but both run against a **real** FDB cluster on **real** wall-clock time, so neither is
seed-replayable: the same injection can interleave differently and see different server-assigned
versions run to run. `chaos/fault.go:24-29` even concedes it "can't simulate true rollback at this
abstraction level" and fakes conflicts as double-commit-then-re-execute.

## 1a. Two tracks — and is this "real DST"?

Be honest about the name up front, because the two layers earn **different guarantees** and lumping
them under one word lets the weaker one borrow credibility. Real DST (FDB/Antithesis definition) =
simulated environment + seeded fault injection + controlled time + **single seed reproduces the run
identically, including concurrency**.

| DST property | Record/relational (SimFDB) | Client transport (SimTransport + synctest) |
|---|---|---|
| Simulated environment | ✅ in-mem FDB | ✅ sim network + sim server |
| Fault injection at seeded points | ✅ | ✅ |
| Time controlled | ✅ own logical clock | ✅ synctest fake clock |
| **Seed → identical run** | ✅ **bit-exact, incl. all interleavings** | ⚠️ **timing yes; memory-race interleaving no** |
| **Verdict** | **Real DST** | **Not full DST — "mostly" (sim + fault injection)** |

Why the asymmetry: the record/relational layers' correctness is **data-mediated** — FDB is serializable
transactions, so the interleavings that cause bugs are interleavings of *transactions* (read/write
conflict ranges vs. commit versions), which SimFDB models as **data** and the workload driver
interleaves **explicitly** in one goroutine. Single-*goroutine* execution still models a
massively-*concurrent* workload (exactly as FDB's own sim runs a whole cluster in one thread) — and
can explore interleavings the OS scheduler would rarely produce, reproducibly. The client transport's
correctness is instead **race-mediated** (goroutine timing + memory ordering); `synctest` makes the
*time-driven* majority deterministic, but raw runnable-goroutine interleaving is not seed-reproducible
in stock Go (see §3). So:

**Decision: split into two tracks over one shared foundation.**
- **Track A — DST (real):** record + relational, at the `fdb.BackendDatabase` seam. Tier 0 + Tier 1 +
  Tier 2 below. This is what "DST" means in these docs.
- **Track B — client simulation & fault injection (NOT "DST"):** transport, at the `DialFunc`/`net.Conn`
  seam. Tier 3 below. Keep it under the RFC-118 **SimTransport** name; do not rebrand it "client DST."
  When it is actually built it should graduate to its own RFC or **extend RFC-118**, with the
  "clock-deterministic, not interleaving-deterministic" caveat stated plainly.
- **Shared:** only Tier 0 (Clock + seeded RNG + Buggify) is common; the two superstructures share
  ~zero critical-path code.

This split is also an ROI/risk decision: Track A is medium-effort / high-ROI / low-risk and covers
where the bugs live — **ship it first**; Track B is large-effort (simulate proxy/resolver/storage),
lower-ROI (the client is smaller and already covered by SimTransport fault tests + `-race`), and even
when done does not yield bit-exact replay. Keeping them separate stops the hard, low-ROI client work
from blocking or discrediting the high-ROI record-layer win. It also matches how the codebase already
organically split these concerns: RFC-002 (chaos, record layer), RFC-118 (SimTransport, client),
RFC-167 (plan determinism, query engine).

**Coverage ceiling, stated once (the honest ledger):** DST here is *additive* — the existing real-FDB
integration/conformance/`-race` tests keep running, so nothing covered today becomes uncovered.
Track A gives **full, exhaustive, reproducible** coverage of transaction-level concurrency + fault
retry/idempotency. The only classes it does **not** cover: (a) genuine *data races inside* record-layer
goroutine fan-out (indexer/spfresh) — held to be order-independent, but keep `-race` as the guard; (b)
client memory-race interleavings (Track B ceiling); (c) SimFDB-vs-real-FDB model-fidelity gaps
(differential mode + conformance tests remain the guard). A hypervisor would add only (b) — the
smallest, least bug-dense slice — for ~1000× the cost.

## 2. The one insight that makes this tractable

**The record and relational layers reach FDB *only* through interfaces** — `fdb.Transactor`,
`fdb.ReadTransaction`, `fdb.WritableTransaction`, `fdb.BackendDatabase` (`pkg/fdbgo/fdb/interfaces.go`,
`backend.go:37`) — and **two backends already implement that exact contract**: the pure-Go client
(`pkg/fdbgo/client`) and the CGo `libfdbc` backend (`pkg/fdbgo/libfdbc/backend.go:877-891`). The
build-tag switch `fdbclient.Open() (fdb.BackendDatabase, error)` (`open_purego.go` / `open_libfdbc.go`)
picks one.

In FoundationDB, DST is enabled by two global vtable swaps: `g_network` (`INetwork`) and `g_simulator`
(`ISimulator`) — the sim replaces the real network/OS wholesale. **We already have that swap, in
interface shape, done.** A deterministic in-memory MVCC backend is simply a **third implementer** of
`fdb.BackendDatabase`. Every source of nondeterminism in the pure-Go client — GRV goroutines, failure
monitor, load-balancer `rand`, per-RPC timers, sockets — is **replaced, not tamed**. The determinism
problem for the record/relational layers collapses to: *write a single-goroutine sorted-map MVCC store
that mints versions deterministically*, behind a seam that is already proven substitutable
(`recordlayer.NewFDBDatabaseWithBackend`, `database.go:147`, takes the interface with zero layer
changes).

## 3. The Go reality: what determinism you can and cannot buy

FoundationDB's determinism is **structural**: the Flow actor compiler turns all concurrency into a
single-threaded event loop pulling from a priority `TaskQueue` (`flow/include/flow/TaskQueue.h`),
where "which coroutine runs next" is `(TaskPriority, ++issueCounter)` — a pure function of enqueue
order — and time only advances when the ready queue drains (`fdbrpc/sim2.actor.cpp:1386-1409`).

**Go has no equivalent and cannot get one bolted on.** Goroutines are preempted by the runtime;
`select` over ready channels picks pseudo-randomly; `map` range order is deliberately randomized. You
cannot make goroutine interleaving deterministic without rewriting the concurrency as an explicit
event loop.

The ecosystem's three honest options, in ascending cost:

| Tier | Mechanism | Determinism | Cost |
|---|---|---|---|
| **`testing/synctest`** (stdlib, in Go 1.25+; we're on **1.26.4**) | fake clock + quiescence (`synctest.Wait`) inside a "bubble"; no real net/OS allowed | time + quiescence deterministic; **not** raw goroutine interleaving | free, in-tree, no code rewrite |
| **Forked runtime** (Polar Signals: `GORANDSEED` + `-tags=faketime`, `GOOS=wasip1`) | seed the scheduler's RNG | "mostly" — they admit occasional non-repro | custom toolchain + WASM target |
| **gosim** (source-translates `go f()`→`gosimruntime.Go`) / **Antithesis** (hypervisor) | full sim runtime | full | gosim: experimental, Go-only, breaks on new linkname rules. Antithesis: ~$168k+/yr |

**`synctest` is the key stdlib primitive.** It gives a fake clock (advances only when every bubble
goroutine is durably blocked) and `synctest.Wait()` (block until all others are durably blocked). It
does **not** seed interleavings of simultaneously-runnable goroutines. Its hard rule — *no real
network/OS in the bubble* — is exactly satisfiable here because we have a pure-Go client + the
in-memory SimTransport/`net.Pipe` seam.

**Conclusion:** don't promise "one seed reproduces any interleaving" for the client — it won't survive
contact with the Go runtime. Split DST into tiers by how much determinism each layer can actually
deliver.

## 3a. Where to intercept nondeterminism (the layer ladder) — and why not gVisor

Every DST approach is a choice of *interception layer*. Higher = cheaper determinism, less of the
real stack exercised. The rule: intercept as **high** as you can while still covering the code you
care about.

| Layer | Tool | Determinism | Cost |
|---|---|---|---|
| Go interface (`fdb.BackendDatabase`) | **Tier 1 SimFDB (this RFC)** | Full — single goroutine, we own it | Low ← pick |
| `net.Conn` | SimTransport + `synctest` (Tier 3) | Mostly (time + quiescence) | Medium |
| Syscall (source-translate) | gosim (§8a) | Full incl. interleavings | High — unmaintained, fights Bazel |
| Syscall (userspace kernel) | **gVisor** | **None** — no determinism engine | High — vendor + fork a kernel |
| Below the OS (hypervisor) | Antithesis | Full, language-agnostic, no code changes | $$$ (~$168k/yr) |

**gVisor was evaluated and rejected as a platform.** It is a *security sandbox*, not a deterministic
hypervisor: the Sentry intercepts guest syscalls in userspace but multiplexes guest threads onto host
threads scheduled by the **host Go runtime** — the exact nondeterministic scheduler DST must
eliminate — and makes zero determinism guarantees. Making it deterministic means forking and
rewriting the Sentry's core scheduler (a massive, fast-moving Google codebase) and *still* facing "the
Sentry is itself a concurrent Go program on the nondeterministic host runtime" — the Go-scheduler
problem relocated one layer down into harder code. It is the one **strictly-dominated** row: it pays
the full cost of descending to the syscall layer without the determinism payoff (gosim at least tries
at that layer; Antithesis achieves it one layer below). Wrong fidelity axis, too — the pure-Go client
makes ordinary Go `net`/`time`/`rand` calls we need to control, not 200+ Linux syscalls we need to
faithfully emulate. No prior art exists of gVisor used for DST.

**One reusable component, though:** `gvisor.dev/gvisor/pkg/tcpip` (netstack) is a standalone userspace
TCP/IP stack with a **channel-based link endpoint** (real TCP over an in-memory Go channel) and an
injectable **`tcpip.Clock`**. If Tier 3 ever needs to exercise the client against realistic TCP
pathologies (partial segments, RST races, retransmit, zero-window), netstack-over-channels + fake
clock + link-layer fault injection is an option. Caveats keeping it "later, maybe": netstack is
internally concurrent (own goroutines/timers → not deterministic by itself), and the client already
abstracts the network as `DialFunc → net.Conn`, so SimTransport's channel-`net.Conn` is simpler and
sufficient unless we are specifically hunting TCP-layer bugs. Not worth vendoring gvisor into Bazel up
front.

## 4. Prior art we build on (don't rebuild)

| Asset | What it gives | Reuse verbatim? |
|---|---|---|
| `chaos.StoreModel` + `Verify()` (`chaos/model.go`, `verify*.go`) | 13 store↔model invariants (counts, VALUE/COUNT/SUM/MIN-MAX_EVER/RANK/VERSION/VECTOR/BITMAP/TEXT). The DST **oracle**. Backend-agnostic. | **Yes** — keep as the invariant suite. |
| `chaos` seed + failure-report discipline (`scenario.go`, PCG seed) | "violation at op N (seed=…)" → re-run with `--seed`. Exactly the Sim2 replay ergonomic. | **Yes** — extend. |
| `chaos.RunRandom` single-goroutine driver (`random.go`) | already synchronous, one goroutine, sorts map keys (`sortedModelPKs`) | **Yes** — retarget at the sim backend. |
| `chaos.RunConcurrent` (`concurrent.go`) | N goroutines, wall-clock pacing, snapshot-only verify | **No** — openly non-replayable; replace. |
| SimTransport / `fault_test.go` (`wrongShardConn`, `dropReplyConn`, `simConn`, `frameIntercept`) | faithful frame-level BUGGIFY (FDB-C-maintainer ACK); drop/reorder/`More`-flip/inline-error already exist | **Yes** — the `Sim2Conn` analog for Tier 3. |
| `DialFunc` + `WithDialFunc` (`transport/conn.go:144`, `client/options.go:44`) | production net.Conn factory seam; `net.Pipe()` plugs straight in | **Yes** — the client-network injection point. |
| RFC-167 `exprConcreteHash` + `FuzzPlanner_Determinism`/`Confluence` subprocess nets | plan-selection total order + "same seed → same plan" oracle | **Yes** — the planning-determinism gate. |
| Synchronous futures `newReadyFutureByteSlice` / `pendingFutureByteSlice` (`future.go:71,98`) | resolve with **no goroutine, no channel** | **Yes** — lets the sim backend stay single-goroutine. |

**Genuinely missing** (the build list): a virtual `Clock`, a threaded seeded RNG source, a
`Buggify(file,line,prob)` helper, the in-memory MVCC backend, true rollback, and — for the client
tier — simulated FDB server processes.

## 5. Proposed architecture — four tiers

Grouped by the §1a split: **Tier 0** is the shared foundation; **Tiers 1–2 are Track A (real DST)**;
**Tier 3 is Track B (client simulation & fault injection — not "DST", separate deliverable).**

### Tier 0 — the three seeded singletons (foundation, cheap) [SHARED]

Port the shape of FDB's determinism primitives:

1. **`Clock` interface** replacing `time.Now`/`time.After`/`time.NewTimer`/`time.Since`. There is
   **no clock seam anywhere in `pkg/fdbgo` or the record layer today** — this is the single largest
   missing piece. A sim clock is a scalar advanced by the driver.
2. **Seeded `rand.Source`** (the `math/rand/v2.NewPCG` pattern `chaos/fault.go:96` already uses),
   threaded through the offenders and **banning `crypto/rand`** on the sim path.
3. **`Buggify(file, line, prob) bool`** — direct port of FDB's `getSBVar` (`flow/flow.cpp:356-370`):
   two seeded gates per site — *activation* (decided once per run, cached per `(file,line)`) and
   *firing* (re-rolled per hit). This is how you sprinkle fault points without a central registry.

Wire Clock+RNG into the **persisted-byte** sites first, because those leak nondeterminism into stored
data and defeat replay:
- Store header `LastUpdateTime` (`store.go:1447`, `store_builder.go:298,486`), lock-state timestamp
  (`store.go:1596`), indexer heartbeat (`indexing_heartbeat.go:42,84`), lease TTLs
  (`online_indexer.go:1625`).
- R-tree node UUIDs (`rtree_types.go:27`, `crypto/rand` → written into keys — worst byte-determinism
  offender), HNSW sample keys (`hnsw.go:670`), indexer UUID (`indexing_heartbeat.go:40`), rebalancer
  nonce (`spfresh_rebalancer.go:46`), spfresh/build tokens (`spfresh_build.go:73`).
- SQL `CURRENT_*`: the one seam `Session.StatementNow()` (`session.go:99-105`) — replace its
  `time.Now()` fallback and every `CURRENT_TIMESTAMP`/`CURRENT_DATE` is deterministic.

### Tier 1 — SimFDB: deterministic in-memory MVCC backend (**the highest-leverage win**) [TRACK A — real DST]

A third `fdb.BackendDatabase` over a **sorted** in-memory keyspace (btree/sorted slice — never a Go
`map`). Wired in via `NewFDBDatabaseWithBackend`. Requirements, in fidelity-risk order:

1. **SSI conflict detection** over read/write conflict ranges (`AddReadConflictRange` etc.,
   `interfaces.go:95-98`, plus the implicit read-conflict every `Get`/`GetRange` adds) resolved
   against commit-version ordering → `not_committed (1020)`. **Highest risk**: get this wrong and the
   layer's retry/idempotency paths — the whole point of DST — are validated against a false model.
2. **Deterministic versions**: a monotonic logical counter → read version, commit version, and the
   12-byte versionstamp (10-byte tx version + 2-byte user version; overwrite the `0xFF×10` placeholder
   at commit — `fdb/tuple/tuple.go:124-147`). Need not match a real cluster (store is isolated) but
   must be monotonic and correctly stamped; the record layer stores versions inline at `pk + -1` and
   asserts ordering. Read-version/versionstamp are **backend-owned** — the record layer already defers
   entirely (`database.go:350,861`), so this is clean to introduce.
3. **RYW mutation buffer + full atomic-op merge set** (`Add`/`Min`/`Max`/`And`/`Or`/`Xor`/`ByteMin`/
   `ByteMax`/`AppendIfFits`/`CompareAndClear`/`SetVersionstamped{Key,Value}`, little-endian `Add` —
   `transaction.go:244-314`). Non-idempotent `Add` under a simulated `1021` retry is precisely the
   bug class DST must catch.
4. **True rollback** on conflict — fixes the `chaos/fault.go:27` limitation for free.
5. **The 5s window + `100KB` value / `10MB` tx / `~10KB` key limits** → `1007`/`2101`/`2103`, so
   split-record and continuation boundaries fire where a real cluster's would.
6. **Synchronous ready-futures** (reuse `future.go:71,98`) — zero goroutines.
7. **BUGGIFY commit points**: inject `1021`/`1020`/`1007` at seed-chosen commits.

Deferrable to v2: watches (concurrent long-poll, off the interface per RFC-109 — model as synchronous
callbacks fired at the committing mutation), `GetEstimatedRangeSizeBytes`/`GetRangeSplitPoints`
(trivial estimates), locality.

**Payoff:** the *entire record + relational stack runs single-goroutine, no cluster, no Docker,
seed-reproducible.* Validate against `chaos.StoreModel.Verify` and the existing
`libfdbc/differential_test.go` shapes. **Oracle to add** (see §8b): alongside `Verify()`'s
invariants, run an **Elle-style linearizability/isolation check** over the committed history — it
catches consistency-anomaly bugs (dirty reads, lost updates, G2 anti-dependency cycles) that an
invariant-only oracle can miss.

### Tier 2 — deterministic workload drivers + replay harness [TRACK A — real DST]

- **Record layer:** retarget `chaos.RunRandom` + `Verify()` at SimFDB under one seed. Subsumes all of
  `Scenario`/`RunRandom` and gains true rollback + version determinism.
- **SQL:** replace the goroutine-based stress driver (`stress_test.go:138-151`, `go func`+`WaitGroup`,
  wall-clock pacing) with a **serial** seed-reproducible SQL workload generator. Execution is already
  deterministic-by-construction (single-goroutine pull cursors, no rand — `executor.go`), so this is
  mostly a driver swap.
- **Continuation-under-fault replay** (net-new capability): mint a continuation, inject a
  conflict-retry or plan-cache invalidation, resume the token, assert rows. This catches a class no
  current harness can — a continuation minted against one commit version/plan resumed after the plan
  switches underneath it (RYW + plan-cache hit/miss path-dependence, `connection.go:265-267,566`).

### Tier 3 — client simulation & fault injection [TRACK B — *not* "DST"; own RFC / extends RFC-118]

For the pure-Go client's *own* concurrency (recovery, hedging, failure monitor, GRV): the dominant
nondeterminism is **time-driven** ("did the failure detector fire before the hedge timer?"), spread
over ~51 `time.*` sites with no clock seam, plus timer-vs-reply `select` races
(`conn.go:900`, `hedge.go:149`, `rpc.go:54`).

Approach — keep the multi-goroutine architecture, control the boundaries:
- Run inside a **`synctest` bubble** → fake clock makes all backoff/timeout/hedge/failure-window
  behavior deterministic, and `synctest.Wait()` steps the client to a stable state after each injected
  fault (no sleeps, no polling).
- Behind `DialFunc`, a **deterministic in-memory `net.Conn`** (the `Sim2Conn` analog; SimTransport's
  `simConn`/`frameIntercept` already does drop/reorder/rewrite) feeding **simulated FDB server
  processes** as deterministic state machines: coordinator (`OpenDatabaseCoordRequest` → ClientDBInfo),
  GRV proxy (seeded read versions), commit proxy + resolver (conflict detect, assign commit
  versions/versionstamps), storage (KV + shard map + `wrong_shard_server`/`future_version` replies).
  This is the one genuinely large from-scratch build.
- Thread the seeded RNG through `loadbalance` (best-of-two server pick, `loadbalance.go:145`), `span`,
  `locality`, `transport.NewUID`.
- Fault catalog to port from Sim2 (`sim2.actor.cpp:330-626`): latency, partial/reordered delivery,
  `rollRandomClose` drops, `clogPair`/`disconnectPair` partitions, swizzle (rolling partition —
  `RandomClogging.actor.cpp`), and the `KillType` ladder constrained by a `canKill`-style safety model
  (`sim2.actor.cpp:1609`).

**Determinism ceiling for Tier 3:** *mostly*-deterministic. Time + quiescence + network faults are
seed-reproducible; the residual — raw interleaving of simultaneously-runnable transport goroutines —
is **not** bit-exact. Cover that residue with `-race` + seeded stress. Reserve a true single-goroutine
event-loop rewrite (strategy 1) for one narrow, high-value client path only if a specific race bug
demands it. **Do not** attempt to reproduce Flow's scheduler generically.

**Buy-instead-of-build alternative for this tier (see §8b):** if bit-exact, interleaving-level client
DST turns out to matter, the pragmatic path is **not** a scheduler rewrite but **Antithesis** — a
deterministic hypervisor that runs our real Docker images unmodified and gives FDB-grade determinism +
autonomous fault exploration with zero code changes. It is the one strategy-② tool that actually works
against Go (it operates *below* the Go runtime). Budget-gated; evaluate only if Tier 3 client-race bugs
prove real and frequent.

## 6. What bugs this catches that today's tooling can't

- **Idempotency under *true* rollback+retry** (COUNT/SUM `Add`), not chaos's double-commit
  approximation.
- **Continuation resumed after conflict-retry / plan-switch → wrong rows** (Tier 2 replay).
- **Split-record / continuation boundary bugs** under deterministically-injected size limits.
- **Online-indexer correctness** under `commit_unknown` at fragment boundaries
  (`online_indexer.go` MUTUAL fragments).
- **Conflict-storm behavior**, deterministically replayable instead of a CI flake.
- **SQL correctness under fault**, bisected to a seed + `Verify()` oracle — the cross-engine and
  stress harnesses check plan-equality and throughput, never fault-triggered wrong-rows to a seed.

## 7. Phased roadmap

1. **Tier 0** — `Clock` + seeded `rand.Source` + `Buggify`; wire into persisted-byte sites +
   `Session.StatementNow`. (Unlocks byte-reproducibility; small, mechanical, high value.)
2. **Tier 1** — SimFDB minimal: sorted store, logical versions/versionstamps, RYW+atomics,
   SSI conflicts, 5s window/limits, ready-futures, commit BUGGIFY. Validate vs `Verify()` + differential
   tests. (**The keystone.**)
3. **Tier 2** — retarget `RunRandom`+`Verify` at SimFDB; serial SQL workload driver; continuation-
   under-fault replay. (Where bug-hunting starts paying.)
4. **Tier 3 [TRACK B — separate RFC / extends RFC-118, not this deliverable]** — SimTransport +
   simulated server processes under `synctest`; thread client Clock/RNG. (Largest build; do only if
   client-layer bugs justify it, and ship under its own RFC with the "not full DST" caveat.)

**Tier −1 (free, do immediately, orthogonal):** wire up **`rr` + Delve replay** (`dlv replay`) for
root-causing real-FDB / testcontainer flakes today — record a flaky run, replay it deterministically
with reverse-debugging. Zero build; it reproduces *captured* failures (it is not seeded exploration or
fault injection), so it complements DST rather than replacing it.

Land 0→1→2 before touching 3. Each tier is independently valuable and revert-safe.

## 8. Risks & non-goals

- **Fidelity of SimFDB is the whole ballgame.** MVCC read-version monotonicity, SSI conflict
  semantics, and versionstamp byte layout must match real FDB or `Verify()` produces false greens on
  the exact retry/idempotency class DST exists to catch. Same "C++ is the spec" bar the real client is
  held to. Mitigate with a differential mode: run the same seeded workload against SimFDB *and* a real
  container, diff observable results.
- **Residual planning nondeterminism** (RFC-167 Phases 1b/2/4 unlanded — Map/Filter/Distinct shells,
  intersection ordering, `physical_fetch_from_partial_record_wrapper.go:169` Fetch-only guard) is
  *masked* by the tie-break, not removed. Gate with the existing subprocess determinism nets, not
  in-process loops.
- **Non-goal:** bit-exact client-goroutine replay in stock Go. Explicitly out of scope; that's
  forked-runtime/gosim/Antithesis territory and not worth the rewrite for the bug classes we have.
- **Non-goal:** replacing real-FDB integration/conformance tests. DST is additive — a fast,
  container-free, fault-injecting inner loop, not a substitute for the wire-compat gate.

## 8a. Evaluated and rejected (for now): gosim

[gosim](https://github.com/jellevandenhooff/gosim) (MIT) is the closest existing OSS tool to what
Tier 3 wants, and its *design* is the right target — so it was evaluated seriously.

**What it does (and does better than we could cheaply build):** source-translates all Go (your code +
deps + stdlib) into a `translated/` tree running on its own deterministic runtime, intercepting at the
**syscall level** (`SysSocket`/`SysBind`/`SysRead`/…). Determinism is **total** — `rand`, time, *and
goroutine interleavings* — so a seed reproduces the run step-for-step. It models **machines**
(`gosim.NewMachine` with per-machine globals, `Crash()`/`Restart()`), a simulated **TCP network**
(`SetDelay`, partitions), a fault-injecting **filesystem** (lost writes), and simulated time
(fast-forwards when all goroutines await time). Ergonomics are excellent: `gosim test -seeds=1-3`,
`gosim debug -step=N` (drops into Delve at an exact step), `-simtrace=syscall`. It runs real code
(bbolt, a 3-node etcd cluster with partitioning). The machine/crash/restart model maps naturally onto
simulating a multi-process FDB cluster.

**Why it is not adoptable as a dependency right now:**
1. **Dormant upstream, hostile Go-version story.** Last upstream commit **Dec 2024**, `go.mod` pinned
   `go 1.23.2`; it breaks on Go 1.24+ (linkname restrictions). We're on **1.26.4**. The only fork that
   claims 1.26 (`glycerine/gosim`) is a one-person, AI-assisted revival whose own README says the
   **race detector is not fully happy** — i.e. you lose `-race`, a primary safety net, under sim.
   `cortalo/gosim` is actively fixing translator bugs (Jun 2026) but is still on 1.23. 79 stars; no
   maintained release.
2. **Fights the build system.** gosim requires its own `gosim test` CLI that translates the whole
   transitive module into a `translated/` tree and runs plain `go test`. This repo builds/tests under
   **Bazel 9 + gazelle + nogo**. gosim would need a parallel, non-Bazel test path and would have to
   translate a large dependency surface (789 non-test files across the three layers, plus ANTLR,
   protobuf, and the pure-Go client's `unsafe` sites) — every unsupported construct (assembly,
   linkname, cgo-in-deps) is a translation failure.
3. **Doesn't remove the big build item.** Even with gosim, Tier 3 still requires writing the simulated
   FDB *server* processes (as gosim machines) — gosim supplies the deterministic scheduler + net +
   machine model, not FDB's protocol. So gosim trades "build a sim network + use `synctest`" for
   "adopt an unmaintained translator," while the server-sim work is unchanged.

**Decision:** do **not** take a gosim dependency. Tiers 0–2 (SimFDB + `synctest`) need no source
translation and live inside Bazel cleanly. For Tier 3, **borrow gosim's design** — the
machine/crash/restart model, syscall-level net sim, and step-numbered time-travel debugging — rather
than the code. Revisit adoption only if (a) a maintained fork stabilizes on current Go with working
race detection, **and** (b) client-race bugs actually justify the integration cost. Cheap
de-risking option first: run `gosim test` against **only** the pure-Go client package (`pkg/fdbgo`,
the most self-contained, lightest-dep target) on a current fork to see whether translation even
succeeds — low cost, high signal, before any commitment.

## 8b. Full alternatives survey (the complete landscape)

There are only **three strategies** to obtain determinism; every tool is an instance of one. The rule
is *intercept as high as you can while still covering the code you care about* (see §3a's ladder).

- **① Seam it high** — replace the nondeterministic thing with a deterministic fake at an interface.
  Cheap; needs code you own and can seam. *→ this RFC's SimFDB (`fdb.BackendDatabase`), `synctest`
  (`net.Conn`), state-machine modeling.*
- **② Determinize the runtime from below** — make scheduler/syscalls deterministic under *unmodified*
  code. For running a binary you can't seam. **All ② tools fight Go's runtime** (signal-based async
  preemption, M:N scheduler, no pluggable runtime) — this is structural, not a tool-choice problem.
- **③ Model & check** — verify the design, or check outputs against a correctness oracle.
  Complementary; not a runtime.

| Tool | Strat. | What it would give | Verdict for this repo |
|---|---|---|---|
| **SimFDB** (this RFC) | ① | Full determinism for record + relational, no cluster | **Chosen keystone.** |
| **`synctest`** (stdlib, Go 1.26) | ① | Fake clock + quiescence for Tier 3 | **Chosen** for Tier 3 concurrency control. |
| **gosim** | ② | Full determinism incl. interleavings via source translation | **No** (§8a) — dormant, Go 1.24+ broken, fights Bazel. Borrow design. |
| **gVisor** | ② | Userspace kernel at syscall boundary | **No** (§3a) — a *sandbox*, no determinism engine; strictly dominated. `netstack` a minor optional component. |
| **Hermit (Meta)** | ② | "Deterministic gVisor done right" — ptrace/Reverie, purpose-built for determinism, chaos + replay, OSS | **No** — "no longer active development, maintenance mode"; long tail of unsupported syscalls; needs fixed FS + no net; Go runtime is a known-hard case. Same failure mode as gosim. |
| **rr + Delve** (`dlv replay`) | ② | Record/replay + reverse-debug, **free, today** | **Adopt as Tier −1** — a *debugger*, not DST. Reproduces captured flakes; no seeded exploration / fault injection. |
| **Antithesis** | ② (hypervisor) | FDB-grade DST on unmodified Docker images, autonomous exploration, time-travel; the **only ② tool that beats Go's runtime** (runs below it) | **The "buy" option** for Tier 3 (see §5 Tier 3). Budget-gated (~$168k+/yr); ship container images. Productized Sim2 by the FDB founders. |
| **State-machine modeling + property testing** | ①/③ | Design the concurrency out: seeded single-goroutine state machines + invariant checks | **Already our direction** — `chaos.StoreModel`+`Verify()` *is* this; SimFDB extends it. Validated by Polar Signals' Go→Rust pivot to exactly this. |
| **Jepsen / Elle** | ③ | Distributed chaos + **linearizability/isolation checking** | **Cherry-pick Elle** as an added SimFDB oracle (§5 Tier 1). Jepsen-the-harness is non-deterministic (we already have that shape via testcontainers + chaos). |
| **TLA+** | ③ | Model-check the *protocol* (SSI conflict, versionstamp ordering) | **Optional, high-value** for the trickiest invariants. FDB ships TLA+ specs. Verifies design, not the Go code. |
| **Go knobs** (`GOMAXPROCS=1`, `GODEBUG=asyncpreemptoff=1`, native `go test -fuzz`) | ① | Cheap partial determinism + seeded workload driver | **Free, use them.** Fuzzing is the workload driver that pairs with SimFDB (already used for `FuzzPlanner`). |
| **madsim** (Rust) / **Coyote** (.NET) | ①/② | Deterministic async-runtime / controlled scheduler | **N/A — wrong language.** Instructive: Rust/.NET let you swap the runtime; **Go cannot**, which is precisely why ① (seam high) beats ② here. |

**Why the survey confirms rather than changes the plan:** every ② tool except Antithesis is dormant,
Go-hostile, or both — because Go's runtime resists external determinization *structurally*. The only ②
tool that delivers (Antithesis) delivers by being a paid hypervisor you don't build. ③ tools are
oracles you complement with, not runtimes you build on. So: **seam high (SimFDB), complement with ③
oracles (Elle, TLA+), and hold Antithesis as the Tier-3 buy-option.**

## 9. Open questions

1. Build-tag `//go:build simfdb` third backend variant, or a plain runtime constructor passed to
   `NewFDBDatabaseWithBackend`? (Latter avoids a build matrix; former matches `open_libfdbc.go`.)
2. Is a differential SimFDB-vs-real mode worth building in Tier 1, or defer to Tier 2?
3. How faithful must the 5s-window/size-limit modeling be in v1 vs. stubbed?
4. Watches in v1 or v2? (Core save/load/query/index doesn't need them.)
