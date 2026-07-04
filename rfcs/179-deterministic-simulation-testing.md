# RFC-179 — Deterministic Simulation Testing (DST) for the record & relational layers (Track A)

**Status:** **Implemented** (Track A, Tiers 0→1→2). The four open questions are **resolved** (§9,
validated against the C++/Java/Go sources) and the build order is committed (§7). Not a query-engine
planner change, so the Graefe gate is **advisory** here, not mandatory. Track B (client transport) is a
separate deliverable (§1a) and out of scope for this RFC.

**Implementation status (this branch):**
- **Tier 0** ✅ — `pkg/dst`: `Clock` (real/sim), seeded `Randomness`, `Buggify` (faithful port of FDB
  `getSBVar`), `Env` bundle. Env seam threaded `FDBDatabase → FDBRecordContext`; every persisted-byte /
  nondeterminism site routed through it (production byte-identical): store-header `LastUpdateTime` +
  lock-state, `Session.StatementNow`/`BeginStatement`, indexer heartbeat time + UUID, online-indexer
  lease TTL, SPFresh token, and the R-tree / HNSW / SPFresh vector-index nonces (via an `Env()` method
  on the `indexStoreContext` seam).
- **Tier 1** ✅ — `pkg/simfdb`: the third `fdb.BackendDatabase` (MVCC sorted store, logical versions,
  RYW, all atomics, SSI with the strict-`>` rule + `1007` window + versionstamp server-role WCR re-add,
  size limits, commit BUGGIFY, synchronous ready-futures, idempotent commit, lazy versionstamp future).
  **Validated seven ways**: unit tests; the record layer saves/loads over SimFDB with no Docker;
  byte-reproducible across runs (incl. under injected faults); `chaos.Verify()`'s 14-invariant oracle;
  the full SQL/relational stack end-to-end; a conflict-outcome parity check against the known
  real-FDB/libfdb_c outcomes; and a **live differential** — 200 randomized conflict scenarios whose
  commit/abort outcome must equal the pure-Go client on a real FDB cluster (transitively libfdb_c, no
  cgo). Five real SimFDB bugs were found and fixed through these integrations.
- **Tier 2** ✅ — serial seed-reproducible record-layer workload driver + `Verify()` oracle;
  concurrent-open-transaction interleaving driver (fires real `1020` end-to-end through the record
  layer); byte-reproducibility **under injected faults**; SQL-over-SimFDB validation; a serial
  seed-reproducible SQL workload driver checked against a model; and continuation-under-fault replay
  (a scan resumes from a continuation across a concurrent modification, no dup/loss).
- **Documented follow-ups** (not core gaps): the SQL-level *plan-switch* continuation variant (a token
  minted against plan A resumed against plan B) needs a cross-request SQL continuation-resume hook the
  engine does not yet expose; a *live* SimFDB-vs-libfdb_c fuzz (the parity + live-vs-pure-Go differential
  already pin the semantics) would add the cgo bench harness as a third arm.
**Review:** the RFC-design review ACK'd (Torvalds, FDB C++ client dev, Graefe advisory); their findings
folded in (`SetVersionstampedKey` server-side WCR re-add, LRU-eviction continuation lever, versionstamp
anatomy). The *implementation* review then caught — and drove fixes for — two real **under-conflict**
SSI bugs the initially-rigged tests missed: GetRange under-conflicting on empty/gap reads (fixed by
porting the client's `rangeConflictExtent`) and GetKey inverting its conflict range for backward
selectors (fixed via `addGetKeyConflictRange`); the conflict-outcome + live differential tests are now
adversarial (sparse keyspace, gap/empty probes, GetKey selectors — verified red-first). Both
implementation reviewers **re-reviewed the fix and ACK'd** (Torvalds confirmed the new tests go red on
the pre-fix code; the FDB C++ dev confirmed the conflict-range ports are byte-identical to the
libfdb_c-validated client). @claude + codex review on the PR.
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
`backend.go:37`) — and **two backends already implement that exact contract**: the pure-Go
`fdb.Database` (`pkg/fdbgo/fdb`, wrapping the `pkg/fdbgo/client` transport; conformance asserted at
`check.go:7`) and the CGo `libfdbc` backend (`pkg/fdbgo/libfdbc/backend.go:877-891` — the
interface-conformance `var` block). The build-tag switch `fdbclient.Open() (fdb.BackendDatabase, error)`
(`pkg/internal/fdbclient/open_purego.go` / `open_libfdbc.go`) picks one — but that switch is only the
production convenience entry point; the actual substitution seam is the runtime constructor below.

In FoundationDB, DST is enabled by two global vtable swaps: `g_network` (`INetwork`) and `g_simulator`
(`ISimulator`) — the sim replaces the real network/OS wholesale. **We already have that swap, in
interface shape, done.** A deterministic in-memory MVCC backend is simply a **third implementer** of
`fdb.BackendDatabase`. Every source of nondeterminism in the pure-Go client — GRV goroutines, failure
monitor, load-balancer `rand`, per-RPC timers, sockets — is **replaced, not tamed**. The determinism
problem for the record/relational layers collapses to: *write a single-goroutine sorted-map MVCC store
that mints versions deterministically*, behind a seam that is already proven substitutable
(`recordlayer.NewFDBDatabaseWithBackend`, `database.go:147`, takes the interface with zero **gold-path**
changes).

One load-bearing caveat, verified against the constructor: `NewFDBDatabaseWithBackend` type-asserts
`backend.(fdb.Database)` (`database.go:157`) to keep the pure-Go concrete fast paths; a backend that is
not the concrete `fdb.Database` — libfdb_c today, and SimFDB — falls into the **degraded** capability
envelope (`d.db` stays empty; the concrete `CreateTransaction` fails fast with `BackendCapabilityError`).
So `Run`/`RunRead` work with zero changes, but for explicit SQL transactions (`BeginTx` →
`CreateWritableTransaction`, `connection.go:527`), the `FDBDatabaseRunner`, and online MUTUAL indexing,
SimFDB **must** implement the interface methods `CreateWritableTransaction()` and
`LocalityGetBoundaryKeys()` (`backend.go:42-45`). These are load-bearing in v1 — not the deferrable
locality of Tier 1's line "Deferrable to v2."

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
| **gosim** (source-translates `go f()`→`gosimruntime.Go`) / **Antithesis** (hypervisor) | full sim runtime | full | gosim: experimental, Go-only, breaks on new linkname rules. Antithesis: ~$100k+/yr (order-of-magnitude) |

**`synctest` is the key stdlib primitive.** It gives a fake clock (advances only when every bubble
goroutine is durably blocked) and `synctest.Wait()` (block until all others are durably blocked). It
does **not** seed interleavings of simultaneously-runnable goroutines. Its *no-real-network/OS*
requirement is not a hard panic — socket I/O and syscalls simply aren't "durably blocking," so a stray
real call **silently** breaks determinism (fake time advances / `Wait` returns early) rather than
failing loudly; only cross-bubble channel/timer/`WaitGroup`/`Cond` ops are fatal. It is satisfiable here
because we have a pure-Go client + the in-memory SimTransport/`net.Pipe` seam — but Track B must add a
`GODEBUG`/lint guard proving the client makes **zero** real `net`/syscall calls under sim, since the
compiler won't catch a stray one.

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
| Below the OS (hypervisor) | Antithesis | Full, language-agnostic, no code changes | $$$ (~$100k+/yr, order-of-magnitude) |

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
| `chaos.StoreModel` + `Verify()` (`chaos/model.go`, `verify*.go`) | 14 store↔model invariant families (counts, VALUE/COUNT/SUM/MIN-MAX_EVER/PERMUTED_MIN-MAX/RANK/MULTIDIMENSIONAL/VERSION/VECTOR/SPFresh/BITMAP/TEXT — `verify.go:158` numbers 1–13 plus the un-numbered SPFresh 11b). The DST **oracle**. Backend-agnostic. | **Yes** — keep as the invariant suite. |
| `chaos` seed + failure-report discipline (`scenario.go`, PCG seed) | "violation at op N (seed=…)" → re-run with `--seed`. Exactly the Sim2 replay ergonomic. | **Yes** — extend. |
| `chaos.RunRandom` single-goroutine driver (`random.go`) | already synchronous, one goroutine, sorts map keys (`sortedModelPKs`) | **Yes** — retarget at the sim backend. |
| `chaos.RunConcurrent` (`concurrent.go`) | N goroutines, wall-clock pacing, snapshot-only verify | **No** — openly non-replayable; replace. |
| SimTransport / `fault_test.go` (`wrongShardConn`, `dropReplyConn`, `simConn`, `frameIntercept`) | faithful frame-level BUGGIFY (FDB-C-maintainer ACK); drop/reorder/`More`-flip/inline-error already exist | **Yes** — the `Sim2Conn` analog for Tier 3. |
| `DialFunc` + `WithDialFunc` (`transport/conn.go:144`, `client/options.go:44`) | production net.Conn factory seam; `net.Pipe()` plugs straight in | **Yes** — the client-network injection point. |
| RFC-167 `exprConcreteHash` + `FuzzPlanner_Determinism`/`Confluence` subprocess nets | plan-selection total order + "same seed → same plan" oracle | **Yes** — the planning-determinism gate. |
| Synchronous futures `newReadyFutureByteSlice` / `pendingFutureByteSlice` (`pkg/fdbgo/fdb/future.go:71,98`) | resolve with **no goroutine** (the *pending* future is also channel-free; `newReadyFutureByteSlice` at :71 closes a channel). They're unexported, so SimFDB lives in a different package and can't call them. | **Pattern, not verbatim** — SimFDB writes its own ready-future type, as libfdbc did; the point is single-goroutine resolution. |

**Genuinely missing** (the build list): a virtual `Clock`, a threaded seeded RNG source, a
`Buggify(file,line,prob)` helper, the in-memory MVCC backend, true rollback, and — for the client
tier — simulated FDB server processes.

## 5. Proposed architecture — four tiers

Grouped by the §1a split: **Tier 0** is the shared foundation; **Tiers 1–2 are Track A (real DST)**;
**Tier 3 is Track B (client simulation & fault injection — not "DST", separate deliverable).**

### Tier 0 — the three seeded singletons (foundation, cheap) [SHARED]

Port the shape of FDB's determinism primitives:

1. **`Clock` interface** replacing `time.Now`/`time.After`/`time.NewTimer`/`time.Since`. There is
   **no clock seam anywhere in `pkg/fdbgo` or the record layer today** — the single largest *missing
   abstraction*. That is not a contradiction with "small, mechanical": **Track A** needs only the
   persisted-byte + `Session.StatementNow` subset seamed (a handful of sites, below), which is small;
   the full ~70-site client seam is **Track B** and is the large part. A sim clock is a scalar advanced
   by the driver.
2. **Seeded `rand.Source`** (the `math/rand/v2.NewPCG` pattern `chaos/fault.go:96` already uses),
   threaded through the offenders and **banning `crypto/rand`** on the sim path.
3. **`Buggify(file, line, prob) bool`** — direct port of FDB's `getSBVar` (`flow/flow.cpp:356-370`):
   two seeded gates per site — *activation* (decided once per run, cached per `(file,line)`) and
   *firing* (re-rolled per hit). This is how you sprinkle fault points without a central registry.

Wire Clock+RNG into the **persisted-byte** sites first, because those leak nondeterminism into stored
data and defeat replay:
- Store header `LastUpdateTime` (`store.go:1447`, `store_builder.go:298,486`), lock-state timestamp
  (`store.go:1596`), indexer heartbeat (`indexing_heartbeat.go:42` createTimeMs, `:135` the persisted
  `HeartbeatTimeMilliseconds`; note `:84` is a read-side staleness *comparison* — control-flow
  nondeterminism, a different class, not a persisted byte), lease TTLs (`online_indexer.go:1625`).
- R-tree node UUIDs (`rtree_types.go:27`, `crypto/rand` → written into keys — worst byte-determinism
  offender), HNSW sample keys (`hnsw.go:670`), indexer UUID (`indexing_heartbeat.go:40`), rebalancer
  nonce (`spfresh_rebalancer.go:46`), spfresh/build tokens (`spfresh_build.go:73`).
- SQL `CURRENT_*`: route both the `StatementTime` pinned by `BeginStatement` (`session.go:113`,
  `time.Now().UTC()` — the *primary* in-statement time source) **and** the `Session.StatementNow()`
  no-statement fallback (`session.go:101`) through the Clock; seaming only the fallback leaves the
  primary live, so deterministic SQL time needs both. Then every `CURRENT_TIMESTAMP`/`CURRENT_DATE` is
  deterministic.

### Tier 1 — SimFDB: deterministic in-memory MVCC backend (**the highest-leverage win**) [TRACK A — real DST]

A third `fdb.BackendDatabase` over a **sorted** in-memory keyspace (btree/sorted slice — never a Go
`map`). Wired in via `NewFDBDatabaseWithBackend`. Requirements, in fidelity-risk order:

1. **SSI conflict detection** over read/write conflict ranges (`AddReadConflictRange` etc.,
   `interfaces.go:95-98`). **Highest risk** — a loose rule produces false greens on the exact
   retry/idempotency class DST exists to catch, so state it precisely: a read conflict range read at
   the transaction's **read version RV** conflicts iff some committed write to that range has commit
   version **strictly greater than RV** → `not_committed (1020)`. Note the exact shape: strict `>`, and
   RV-vs-the-conflicting-write's-**commit**-version — *not* commit-vs-commit; get the direction (`>=`
   vs `>`) or the operands wrong and every boundary case flips (cf. C++ `SkipList.cpp` `CheckMax`,
   `Resolver.actor.cpp`). A read version below the 5s MVCC window (`commitVersion − 5,000,000`) yields
   `transaction_too_old (1007)` instead — a **distinct, earlier** verdict that never fires for
   write-only transactions, and which a too-old transaction resolves by applying **no** mutations (its
   assigned batch version is still consumed — harmless in the isolated store). Every mutation auto-adds
   a write conflict range **unless** `NEXT_WRITE_NO_WRITE_CONFLICT_RANGE` is set — **and mind the
   versionstamp path**: `SetVersionstampedKey` suppresses the *client* write-conflict range (the key
   isn't known yet), and the **server** re-adds it at the *stamped* key (C++
   `CommitProxyServer.actor.cpp:213`). SimFDB plays the server role, so it **must** re-add the write
   conflict range at the finished key *after* stamping — skip it and every versionstamped-key write
   gets no conflict range → systematic under-conflict, a false green on exactly the class DST exists to
   catch. The implicit
   read-conflict that `Get`/`GetRange` add covers **most** reads, not all — SimFDB reimplements the
   whole `WritableTransaction` surface, so replicating the exclusions is its responsibility, not free:
   snapshot reads add none (`client/transaction.go:499-602`), `\xff\xff` special keys add none (`:756-762`), a
   read already satisfied by the tx's own write adds none (RYW filter, `:1130-1146`), and `GetRange`
   clamps its read conflict to the data actually returned, not the requested span (`rangeConflictExtent`,
   `:1148-1172`). **Serialize commits** (assign each one monotonic version, resolve one at a time) to
   sidestep the C++ batch/`MiniConflictSet` intra-batch path — strictly simpler, still faithful. This
   is the claim to pin against the existing conflict-outcome differential (Tier 1 differential note, §8).
2. **Deterministic versions**: a monotonic logical counter → read version, commit version, and the
   12-byte versionstamp = **10-byte tx version** (8-byte commit version + 2-byte batch order, both
   big-endian, *server*-written) + **2-byte user version** (client tuple data, never server-written —
   don't conflate it with the batch order inside the 10 bytes). The `0xFF×10` placeholder layout is
   at `pkg/fdbgo/fdb/tuple/tuple.go:129-141`, but the **overwrite is not in the tuple codec** — in real
   FDB it is **server-side** (`transformVersionstampMutation`, C++ `Atomic.h`, invoked by the commit
   proxy); the pure-Go client only appends the 4-byte offset (`PackWithVersionstamp`) and reads the
   stamped value back from the commit reply (`client/transaction.go:2216-2233`). So SimFDB **plays the server
   role**: at commit it overwrites the placeholder with the 8-byte commit version + 2-byte batch order,
   strips exactly the 4 trailing offset bytes (apiVersion ≥ 520), and flips the mutation to a plain
   `SetValue`; the batch order stamped into the key MUST equal the value returned from `GetVersionstamp`
   (trivially 0 for one-txn-per-commit). Versions need not match a real cluster (store is isolated) but
   must be monotonic and correctly stamped; the record layer stores versions inline at `pk + -1`, gated
   on format version ≥ 6 (`recordVersionSuffix=-1` `constants.go:52-55`,
   `formatVersionSaveVersionWithRecord=6` `store.go:33`; also gated by `omitUnsplitRecordSuffix`), and
   asserts ordering. Read-version/versionstamp are **backend-owned** — the record layer already defers
   entirely (`database.go:350,861`), so this is clean to introduce.
3. **RYW mutation buffer + full atomic-op merge set** (`Add`/`Min`/`Max`/`And`/`Or`/`Xor`/`ByteMin`/
   `ByteMax`/`AppendIfFits`/`CompareAndClear`/`SetVersionstamped{Key,Value}`, little-endian `Add` —
   `client/transaction.go:244-314`). Non-idempotent `Add` under a simulated `1021` retry is precisely the
   bug class DST must catch.
4. **True rollback** on conflict — fixes the `chaos/fault.go:27` limitation for free.
5. **The 5s window + `100KB` value / `10MB` tx / `~10KB` key limits** → four verdicts, one per limit:
   5s MVCC window → `transaction_too_old (1007)`; `100KB` value → `value_too_large (2103)`; `10MB` tx →
   `transaction_too_large (2101)`; `~10KB` key → `key_too_large (2102)` (`client/transaction.go:1651` — the
   earlier draft dropped 2102; omitting the guard is a false-green on index-entry-size regressions).
   What the layer depends on is the **retryable-vs-terminal classification** (1007 retryable;
   2101/2102/2103 terminal — `fdb/error.go:436-456`, mirrored in `runner.go:243-256`), not byte-exact
   accounting — so the size guards are ~3-line constant-threshold checks kept as **regression
   sentinels** (an over-limit online-indexer / delete-where batch would otherwise pass silently in
   SimFDB while failing on a real cluster). Modeling these codes governs the **error paths**, *not* the
   firing of split-record/continuation **boundaries** — those are record-layer computations
   (`splitRecordSize=100_000`, proactive in `saveWithSplit` so 2103 is unreachable on the happy path;
   `ExecuteProperties` scan limiters, `text_cursor.go:85-106`) that fire regardless of the backend. The
   5s window is **server-authoritative** (the purego client enforces no client-side wall clock —
   `grv.go:182-185`), so it lives in SimFDB (the backend): model it as a fixed logical-version knob
   (default `5,000,000` = `MAX_WRITE_TRANSACTION_LIFE_VERSIONS`) evaluated as pure version arithmetic
   *before* the conflict check; in v1 do **not** GC the history (retaining it is strictly *safer* —
   SimFDB always proves conflict exactly, never a false "committed") and inject 1007/1009/1021 at
   seed-chosen points via BUGGIFY (req #7) rather than building a real MVCC-GC clock. (A client
   `SetTimeout` deadline surfaces as `transaction_timed_out (1031)`, a different "5s" code SimFDB need
   not model for continuation correctness.)
6. **Synchronous ready-futures** — zero goroutines. Reuse the *pattern* of `pkg/fdbgo/fdb/future.go:71,98`
   (the pending future is channel-free too), but those types are unexported, so SimFDB writes its own in
   its package, as libfdbc did.
7. **BUGGIFY commit points**: inject `1021`/`1020`/`1007` at seed-chosen commits.

Deferrable to v2 — **watches** deferral is *free* (Watch is off the `BackendDatabase`/`ReadTransaction`/
`WritableTransaction` interfaces per RFC-109, concrete-only on the pure-Go `Transaction`, and the
record/relational core uses **zero** watches, so a v1 SimFDB omits the method entirely with no coverage
loss). When built, the correct v2 model is a callback that fires when a *later* commit changes the
watched key to a value **different from the one pinned at the watch's read version**, armed at the
creating transaction's **committed** version (RFC-170) — *not* "fired at the committing mutation," which
would self-fire a txn's own `Set` and produce false wakeups; add `MAX_WATCHES`/`too_many_watches (1032)`
slot-accounting + permitted spurious fires only for a v2 that itself targets watch-limit behavior. Also
deferrable: `GetEstimatedRangeSizeBytes`/`GetRangeSplitPoints` (trivial estimates) and locality
**fidelity** — but note the interface method `LocalityGetBoundaryKeys()` must still *exist* (a trivial
whole-keyspace-as-one-shard stub suffices unless v1 exercises MUTUAL indexing or range-split
parallelism; real shard boundaries are the deferrable part — cf. §2's load-bearing caveat).

**Payoff:** the *entire record + relational stack runs single-goroutine, no cluster, no Docker,
seed-reproducible.* Validate against `chaos.StoreModel.Verify` and the existing
`libfdbc/differential_test.go` shapes.

**Differential note (Tier 1 — co-develop with the SSI detector, don't defer):** `Verify()` is
store-vs-model (`chaos/model.go:18-21`) and **cannot** self-catch a SimFDB conflict-semantics
divergence — a systematic under- or over-conflict is a false green. So the SSI detector (item 1) ships
*with* its own oracle: point the existing two-backend commit/abort-agreement harness
(`FuzzDifferential_ConflictOutcome`, `pkg/fdbgo/bench/differential_conflict_outcome_fuzz_test.go`) and a
masked-byte-parity diff (mask the inline `pk+-1` version + versionstamps, run serial-from-empty) at
SimFDB-vs-real as its red/green oracle. This is nearly free (the harness exists — §8) and is the only
check that catches both conflict directions. The full seeded-workload replay stays in Tier 2.

**Oracle to add** (see §8b): alongside `Verify()`'s invariants, run an **Elle-style
linearizability/isolation check** over the committed history — it catches consistency-anomaly bugs
(dirty reads, lost updates, G2 anti-dependency cycles) that an invariant-only oracle can miss (it
catches under-conflict only when the anomaly is sampled, so it complements — not replaces — the
differential).

### Tier 2 — deterministic workload drivers + replay harness [TRACK A — real DST]

- **Record layer:** retarget `chaos.RunRandom` + `Verify()` at SimFDB under one seed for the
  single-transaction paths (gains true rollback + version determinism, subsuming `Scenario`/`RunRandom`;
  widen the driver plumbing `NewScenario`/`RunRandom` from concrete `fdb.Database` to
  `fdb.BackendDatabase` first). **But `RunRandom` is serial and fakes conflicts** as
  double-commit-then-re-execute (`fault.go:24-29`), so retargeting it *unchanged* fires **zero** real
  SSI conflicts — Tier 1 item 1 would be an unexercised checkbox (violates NO-FAKE-CHECKBOXES). So Tier
  2 also adds an explicit **concurrent-open-transaction interleaving driver**: hold several transactions
  open, interleave their ops in one goroutine, commit through the serialized resolver. *This* is what
  makes `1020` and non-idempotent-`Add`-under-`1021` actually fire. Its hard prerequisite is MVCC
  **reads-at-read-version** (a latest-value map returns writes the reader must not see, corrupting both
  results and the conflict model) — non-stubbable per Tier 1 item 5.
- **SQL:** replace the goroutine-based stress driver (`stress_test.go:138-151`, `go func`+`WaitGroup`,
  wall-clock pacing) with a **serial** seed-reproducible SQL workload generator. Execution is already
  deterministic-by-construction (single-goroutine pull cursors, no rand — `executor.go`), so this is
  mostly a driver swap.
- **Continuation-under-fault replay** (net-new capability): mint a continuation, inject a
  conflict-retry or a plan switch, resume the token, assert rows. This catches a class no current
  harness can — a continuation is a serialized cursor tree keyed to the *original* plan's operator
  structure, but resume re-derives the plan via the cache (`cascades_generator.go:264`) and Go embeds
  **no plan-hash in the token and does no bind-validation on resume** (Java does), so a token minted
  against plan A resumed against a re-optimized plan B is decoded with zero guard → silently wrong rows
  or a decode error, not a clean rejection (RYW + plan-cache hit/miss path-dependence,
  `connection.go:265-267,566`). Drive the plan switch primarily via **LRU eviction** (`maxSize`) — the
  natural, frequent trigger that needs no DDL — not only `Invalidate()`.

#### Tier 2 oracles — intrinsic query-layer correctness (metamorphic + golden), no external reference

The record layer has a model oracle (`chaos.Verify`, 14 invariants). The **query layer has no cheap
model** for "is this result set correct?". The tempting answer — **differential vs a reference engine
(SQLite/Java)** — is **rejected**: it relocates the definition of "correct" to a system whose SQL
semantics differ, makes our suite parasitic, and cannot cover the read-side surface we intend to extend
*beyond* Java (query reach is not the wire line, §1a). Two **intrinsic** oracles — both newly enabled by
SimFDB's determinism, neither needing an external reference — cover it. They are complements, not
alternatives:

**(a) Metamorphic — catches WRONG.** The oracle is a web of invariants the engine must satisfy *against
itself*, no known-correct answer required; a single buggy rule/executor path cannot keep satisfying
diverse relations while producing a wrong result. Strongest first:
- **Plan-diversity equivalence** (the key one for Cascades): the *same data* queried through *different
  physical plans* must return identical rows. Force diversity via identical-data tables with/without an
  index (full-scan vs index-scan), algebraic rewrites the optimizer normalizes differently, or rules
  on/off; `EXPLAIN` confirms the plans actually differ so the check is non-trivial. **This directly tests
  that the transformation rules preserve semantics** — the exact property PR #201's 0-row bug violated.
- **Partition invariants:** `COUNT(*) == COUNT WHERE p + COUNT WHERE ¬p + COUNT WHERE p IS NULL`;
  `SUM(x) == Σ over GROUP BY g of SUM(x)`; a predicate and its complement partition the table.
- **Rewrite equivalence:** `x IN (a,b) ≡ x=a OR x=b`; De Morgan; predicate reorder/redundancy;
  `ORDER BY k LIMIT n` is a prefix of the full `ORDER BY k`.
Not circular — it is a constraint system, not "engine == engine". Its one blind spot (a bug *consistent
across all transformations*, e.g. every plan drops the same row) is covered by the simple independent Go
model already in `sqlhunt` (sql-query/sql-null).

**(b) Golden / characterization — catches CHANGED.** Capture the engine's behavior for a curated corpus —
**result rows + `EXPLAIN` plan** — as a committed baseline, and **diff on merge/release**: every behavior
delta becomes a reviewable artifact, and a silent cost-model/rule regression that alters a plan (or a
result) surfaces as a golden diff the author + Graefe must approve. **Newly feasible because SimFDB makes
engine output a deterministic function of (schema, data, query)** — pre-SimFDB the baseline was noise
(versions/timing/map-order). Precedent: RFC-109 already golden-gates the record-layer *wire bytes*; this
extends it to the *plan + result*.
- **Granularity:** result rows (locks correctness — a diff is almost always a bug, loud) + plan shape
  (locks the optimizer — catches silent regressions). Full execution traces are too fragile; omit them.
- **Honest limitation:** golden locks in *current* behavior, right or wrong — it catches CHANGED, not
  WRONG. A bug baked into the baseline stays green until noticed. It is the **complement** of metamorphic,
  never a substitute.
- **Noise / approval-fatigue mitigation:** a stable curated corpus, canonical serialization, and
  separating "**result** changed" (loud — almost always a bug) from "**plan** changed, result identical"
  (often a real optimization — reviewed with its cost delta, not rubber-stamped). Wire it into the
  query-engine merge gate: a plan-golden diff is precisely what Graefe reviews per PR.

**The four intrinsic layers** (zero external reference): independent Go model (absolute wrong answer) ·
metamorphic invariants (WRONG) · golden baseline (CHANGED) · planner-internal fuzz
(`FuzzPlanner_Determinism/_Idempotence/_MemoConsistency/_Invariants`, in-tree). Differential-vs-reference
is explicitly out.

| oracle | catches | needs | status |
|---|---|---|---|
| independent Go model | wrong absolute answer | a naive recompute | ✅ `sqlhunt` sql-query/null |
| metamorphic | wrong (inconsistent) | invariants | ⏳ build |
| golden / characterization | changed (regression) | determinism (✓ SimFDB) | ✅ `pkg/simfdb/hunt/golden` (result+`EXPLAIN` baseline, diff on merge) |
| planner-internal fuzz | memo/plan invariants | in-process | ✅ in-tree |

#### Tier 2 generators — LLM-guided adversarial input (the "clanker" analog)

**Motivation.** Random seeds explore a huge but *unstructured* slice of the input space; for a SQL engine
the interesting bugs hide in *semantically* structured inputs random generation almost never reaches
(correlated subqueries, empty-group aggregates, NULL/type-coercion corners, LIMIT/DISTINCT/ORDER-BY
interactions, adversarial join shapes). Greg KH's "clanker" (a local LLM fuzzing the kernel, human owns
the fixes) is the proof of pattern. Our determinism + oracle stack lets us do a *sounder* version: an LLM
(Claude, driven by a skill) proposes inputs; the deterministic oracles judge.

**The one hard rule — the LLM generates INPUTS, never the ORACLE.** The LLM is untrusted for correctness
(it hallucinates the "right answer"); it is trusted only to produce *valid, diverse, adversarial* input.
Ground truth stays with the deterministic oracles above. This is the line between a sound tool and "ask
the model if the output looks right" (which is not testing).

**The input level — valid *user* input, not bytes or internal state.** The LLM plays an adversarial
DBA + analyst: it emits well-formed SQL (schema DDL, seed `INSERT`s, queries) — the same shapes a user
hands the database, chosen adversarially — which then run through the real parser → planner → executor.
It is *not* a byte-fuzzer (random/mutated bytes for decode robustness — that is `FuzzParse`/`FuzzUnpack`)
and it does *not* fabricate plan trees or internal state (it hands the engine a query and lets it plan).
Pointed at the record layer instead of SQL, the same idea generates metadata + records + operation
sequences (the app-developer's input). Three input levels stack: raw bytes (existing byte-fuzzers) ·
**user-level SQL/schema/records (this)** · planner internals (existing `FuzzPlanner_*`).

**What it generates** — a structured, replayable corpus (not free text):
- Schemas + indexes + seed data (boundary values, NULLs, duplicates, empty sets).
- Queries aimed at historically-fragile semantics.
- **Metamorphic relations — the highest-value output:** families of queries the LLM *asserts* are
  equivalent (or predictably related), which the engine then checks against itself. The LLM's *semantic*
  knowledge writes the equivalence claim; the engine's execution is the judge (e.g. it emits `a IN (1,2,3)`
  beside `a=1 OR a=2 OR a=3`, or a query beside its predicate-pushed / join-reordered rewrite). This is
  what makes the LLM the natural *feeder* for the metamorphic oracle.
- Operation sequences for the fault-injection hunters.

**The loop** (generate → execute → observe → refine):
1. Claude (skill / subagent) emits a batch of candidate inputs as structured data.
2. The deterministic harness runs each over SimFDB; oracles return pass/violation; coverage is recorded
   (plan shapes, rules fired, operators, code paths).
3. The signal — *coverage gaps* ("no correlated subquery yet", "no plan used the RankedSet path"),
   *violations* (a finding to minimize + amplify), *novelty* (dedup vs the committed corpus) — feeds back.
4. Claude generates the next batch, steering at the frontier, within a token/time budget (like the
   overnight seed budget). Repeat.

**Corpus promotion — what makes it cumulative.** An input is *promoted* into the committed corpus only if
it (a) finds a bug, (b) increases coverage, or (c) exercises a new plan shape; the rest are ephemeral.
Promoted queries become golden entries; promoted+validated relations become permanent metamorphic checks;
promoted sequences become regression seeds. The corpus is thus a growing, curated, LLM-built asset — the
analog of clanker's accumulating patches, but accumulating *coverage*.

**Soundness guards:**
- *The LLM never judges correctness* — the §5 oracles do.
- *A proposed equivalence that fails is triaged, not trusted:* it is EITHER an engine bug OR a wrong LLM
  claim. A relation that holds across many data instances on the assumed-correct engine joins the
  permanent suite; the rest are surfaced for review. A hallucinated equivalence costs a triage, never a
  false green.
- *Determinism is preserved:* the *generator* is nondeterministic (explores differently each run), but
  every generated input runs over SimFDB, so every *finding* is a reproducible, committed artifact. The
  loop wanders; the results do not.
- *Human/Graefe ratifies promotions:* the LLM *proposes* corpus additions (golden baselines, metamorphic
  relations); a human reviews the diff before commit — mirroring clanker's "AI finds, human owns" and the
  query-engine merge gate.
- *Invalid SQL is filtered, not corpus'd* — a parse/plan error is logged; a query that *should* parse but
  errors is itself a finding.

**Mechanism & governance.** A Claude skill (e.g. `/dst-generate`) drives the loop on-demand or nightly,
locally (no cloud dependency, like clanker). Batches are structured JSON/Go so they replay
deterministically. A fix for a found bug carries a `Found-by: dst-generate (<corpus-entry>)` trailer — tool
attribution, not model co-authorship, disclosing AI-assisted *discovery* in the clanker spirit while the
human authors and owns the fix.

**Relationship to the rest.** This is a *generator that feeds the existing judges*, not a new judge. It
complements random fuzzing (random finds the unexpected; the LLM finds the semantically deep — additive)
and is the natural feeder for the metamorphic oracle. **Status:** ⏳ design (this section); build after the
metamorphic oracle it feeds. **Honest limits:** LLM token cost (budget-scoped); coverage bounded by what
the LLM "knows" (keep random fuzzing beside it); nondeterministic *exploration* (mitigated — findings are
deterministic, committed).

### Tier 3 — client simulation & fault injection [TRACK B — *not* "DST"; own RFC / extends RFC-118]

For the pure-Go client's *own* concurrency (recovery, hedging, failure monitor, GRV): the dominant
nondeterminism is **time-driven** ("did the failure detector fire before the hedge timer?"), spread
over ~70 `time.*` sites with no clock seam (77 across `pkg/fdbgo`; the "~51" of an earlier count
excludes the 26 `time.Since` duration sites), plus timer-vs-reply `select` races
(`conn.go:900`, `hedge.go:149`, `rpc.go:54`).

Approach — keep the multi-goroutine architecture, control the boundaries:
- Run inside a **`synctest` bubble** → fake clock makes all backoff/timeout/hedge/failure-window
  behavior deterministic, and `synctest.Wait()` steps the client to a stable state after each injected
  fault (no sleeps, no polling). The bubble controls *time*, not *RNG or I/O* — so a bit-reproducible
  run has two **hard preconditions** the net seam alone doesn't cover: seed the client's `crypto/rand`
  (`transport/conn.go:1049,1074`) and global `math/rand/v2` (`loadbalance.go:145`) first, and add the
  no-real-net/syscall guard (§3), since a stray real call silently breaks determinism rather than
  panicking.
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
- **Wrong query answers with no reference engine** — metamorphic invariants (plan-diversity, partition,
  rewrite equivalence) catch rule/executor bugs intrinsically (Tier 2 oracles §5).
- **Silent plan/result regressions across changes** — a golden result+`EXPLAIN` baseline over the
  deterministic SimFDB corpus turns every behavior delta into a reviewable diff at merge/release (would
  have caught PR #201's 0-row plan regression).

## 7. Phased roadmap

1. **Tier 0** — `Clock` + seeded `rand.Source` + `Buggify`; wire into persisted-byte sites +
   `Session.StatementNow`. (Unlocks byte-reproducibility; small, mechanical, high value.)
2. **Tier 1** — SimFDB minimal: sorted store, logical versions/versionstamps (server-role stamping),
   RYW+atomics, SSI conflicts (strict RV-vs-commit-version `>` rule + distinct `1007`), 5s window +
   all-three size codes (2101/2102/2103), ready-futures, commit BUGGIFY. Validate vs `Verify()` **and**
   the conflict-outcome differential co-developed as the SSI detector's red/green oracle (`Verify()`
   can't self-catch a conflict-semantics divergence). (**The keystone.**)
3. **Tier 2** — retarget `RunRandom`+`Verify` at SimFDB for single-tx paths **plus** a
   concurrent-open-transaction interleaving driver (the thing that actually fires SSI `1020` /
   non-idempotent-`Add`-under-`1021`; `RunRandom` is serial and fakes conflicts); serial SQL workload
   driver; continuation-under-fault replay. (Where bug-hunting starts paying.)
4. **Tier 3 [TRACK B — separate RFC / extends RFC-118, not this deliverable]** — SimTransport +
   simulated server processes under `synctest`; thread client Clock/RNG. (Largest build; do only if
   client-layer bugs justify it, and ship under its own RFC with the "not full DST" caveat.)

**Tier −1 (near-free, orthogonal):** wire up **`rr` + Delve reverse-replay** for root-causing real-FDB /
testcontainer flakes — record a flaky run, replay it deterministically with reverse-debugging. Small
setup, not zero: `dlv` is present but **`rr` is not installed**, and the literal `dlv replay <trace>`
is *not* a command in dlv 1.26.3 — the integration is `dlv test/exec --backend=rr` (or standalone `rr
record`/`rr replay`), which needs `rr` on PATH, a hardware PMU, and `perf_event_paranoid ≤ 1` (fails in
most containers). It reproduces *captured* failures (not seeded exploration or fault injection), so it
complements DST rather than replacing it.

Land 0→1→2 before touching 3. Each tier is independently valuable and revert-safe.

### 7a. Implementation status & the selected next build

- **Tier 0** ✅ `pkg/dst` (Clock/Randomness/Buggifier) wired into persisted-byte sites.
- **Tier 1** ✅ `pkg/simfdb` (SSI resolver, versions, RYW+atomics, size codes, commit BUGGIFY, targeted injection).
- **Tier 2:**
  - Record-layer + SQL hunt workloads ✅ `pkg/simfdb/hunt` (+ `sqlhunt`): brute-force loop-until-bug,
    shrink, `cmd/dst-hunt` overnight runner. Found/fixed the dynamic-record wire-serialization bug.
  - Golden / characterization oracle ✅ `pkg/simfdb/hunt/golden` (result + `EXPLAIN` baseline, diff on
    merge; determinism-proven across 160+ fresh processes).
  - Independent-model + metamorphic SQL oracles ✅ in `sqlhunt` and `pkg/simfdb/hunt/metamorphic`
    (`Check` runs each group of asserted-equivalent queries over SimFDB and diffs the multisets — the
    WRONG-catcher; `dst-generate` is its runner). The partition-invariant variant is deferred: the
    scalar-subquery arithmetic it needs (`SELECT (..)+(..)`) is unsupported SQL today.
  - **Concurrent-open-transaction interleaving driver ✅ BUILT** — `pkg/simfdb/hunt/interleave`. Holds
    N transactions open, interleaves RMW-increment / atomic-add ops through one deterministic goroutine,
    and commits through the serialized SSI resolver. Every prior workload is single-writer, so the
    resolver had **never fired a real `1020`** — an *unexercised checkbox* (NO-FAKE-CHECKBOXES). It now
    fires **5,317 real `1020`s across 2,000 pure-RMW seeds** (and **zero** on the pure-atomic-add
    profile — the commutativity contrast). Two intrinsic oracles judge each run: a **verdict** oracle
    (independently recompute the SSI verdict from point-key read/write sets + a lockstep model version
    counter — catches missed conflicts AND spurious aborts; the point-vs-byte-range representation gap is
    its teeth) and a **state** oracle (retry-drain every abort so each program applies exactly once, then
    assert the final keyspace equals the seed-derived sum of every program's effect — catches lost
    updates, rolled-back-write leaks, atomic-add miscounts). v1 is fault-free (the resolver itself is
    under test); a fault-enabled variant that teaches the state oracle about post-apply
    `commit_unknown`(1021) — the non-idempotent-`Add`-under-rollback surface — is the follow-up.
  - **LLM-guided adversarial input generator** (the "clanker" analog — Claude proposes inputs, oracles
    judge) ✅ v1 built + run: the `llm-metamorphic-generate` workflow proposes equivalence scenarios,
    `dst-generate` judges them over SimFDB. First run found **2 real query-engine bugs** (aggregate-index
    all-NULL-group drop; inconsistent NULL ordering — both in `TODO.md ## DST findings`).
  - **Range-conflict interleaving driver ✅ BUILT** — `pkg/simfdb/hunt/rangeconflict`. The interleave
    driver only makes point-width conflict ranges; this one interleaves `GetRange`/`ClearRange`/point ops
    to exercise the resolver's SPAN arithmetic (`rangesOverlap`, `keyAfter`, the GetRange conflict
    extent). Verdict oracle resolves over integer intervals (byte-range overlap ≡ interval overlap for
    ordered keys); state oracle replays blind Set/Clear writes in commit order. **9,238 real `1020`s +
    30,417 range ops across 3,000 hot seeds.** Its first clean sweep caught a model-fidelity bug — a
    read-only transaction takes FDB's client-side fast path (no conflict check, no version bump), which
    a verdict model must replicate.
  - **Continuation-under-fault replay ✅ BUILT** — `pkg/simfdb/hunt/continuation`. Paginates record +
    index scans (forward/reverse) at every page size, resuming from the continuation token each page in a
    FRESH transaction, and checks against an INDEPENDENT Go model (records in primary-key order, the VALUE
    index in `(indexed-value, pk)` order). Three airtight oracles: pagination equivalence (page size 1
    round-trips every row through a token), prefix-delete invariance, tail-delete reflected (both under a
    between-page version bump). The independent model — proven faithful by a one-shot-scan fidelity test —
    catches a wrong scan order that a metamorphic paginated-vs-one-shot check would miss.
  - **Selected next build:** fault-enabled interleave variant ⏳ — inject post-apply `commit_unknown`(1021)
    into the interleave driver and teach the state oracle that a 1021'd transaction's writes DID land
    (the non-idempotent-`Add`-under-true-rollback surface, now in a concurrent setting).
- **Tier 3** — Track B, separate RFC, not started (out of scope here).

## 8. Risks & non-goals

- **Fidelity of SimFDB is the whole ballgame.** MVCC read-version monotonicity, SSI conflict
  semantics, and versionstamp byte layout must match real FDB or `Verify()` produces false greens on
  the exact retry/idempotency class DST exists to catch. Same "C++ is the spec" bar the real client is
  held to. Mitigate with a **differential** — and note `Verify()` **cannot** self-catch this:
  `chaos.StoreModel` is a shadow driven by the same op stream (`model.go:18-21`), so it checks
  SimFDB-vs-model, never SimFDB-vs-real; a systematic conflict-detect divergence (under- *or*
  over-conflict) is a false green, and the Elle oracle catches under-conflict only when the anomaly is
  sampled. The two-backend harness is **not net-new** — `libfdbc/differential_test.go` already drives
  two `BackendDatabase`s against one container and diffs persisted bytes/cross-reads, and
  `FuzzDifferential_ConflictOutcome` (`pkg/fdbgo/bench/differential_conflict_outcome_fuzz_test.go`) is already the
  exact commit/abort-agreement oracle (it caught the RFC-121 conflict-set bugs). SimFDB is a third
  backend into that fixture. So the targeted conflict-outcome + masked-byte-parity slice is
  **co-developed with the SSI detector in Tier 1** as its red/green oracle; the full seeded-workload
  replay is **Tier 2** (needs the Tier-2 drivers, Docker in the loop, and version-byte masking —
  SimFDB's isolated version domain breaks the read-version pinning the existing differentials use, so
  it runs serial-from-empty with all version bytes masked, and fault-injected runs can only be diffed
  fault-free).
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
| **rr + Delve** (`dlv --backend=rr`) | ② | Record/replay + reverse-debug | **Adopt as Tier −1** — a *debugger*, not DST. Reproduces captured flakes; no seeded exploration / fault injection. Small setup: `dlv` present, but `rr` needs installing + a PMU (`dlv replay` is not a 1.26.3 command). |
| **Antithesis** | ② (hypervisor) | FDB-grade DST on unmodified Docker images, autonomous exploration, time-travel; the **only ② tool that beats Go's runtime** (runs below it) | **The "buy" option** for Tier 3 (see §5 Tier 3). Budget-gated: **~$100k+/yr** is an order-of-magnitude marker (a derived ~24-core figure, not a headline quote; public list is ~$0.80/core-hr), not a blocker on the product's health — it is very much alive ($105M Series A, Dec 2025). Ship container images. Productized Sim2 by the FDB founders. |
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

## 9. Resolved decisions (were open questions)

Each resolved against the C++/Java/Go sources; one-line rationale here, detail at the cited section.

1. **Backend wiring → plain runtime constructor, no build tag.** Construct SimFDB in test/harness code
   and pass it to `NewFDBDatabaseWithBackend(simBackend)` (the proven seam, `database.go:147`); do
   *not* add `//go:build simfdb`, and do *not* add it to `fdbclient.Open()`. The build-tag switch
   exists only to gate **CGo linkage** and libfdb_c's once-per-process unrecoverable network thread —
   SimFDB is pure Go with neither property, so a tag would copy the form without the justification, at
   zero benefit, and would stop one test binary from running both real-FDB and SimFDB (which the Tier 1
   differential needs). (§2)
2. **Differential in Tier 1 *and* Tier 2 — split, not either/or.** The targeted conflict-outcome +
   masked-byte-parity differential is **co-developed with the SSI detector in Tier 1** as its red/green
   oracle (the invariant/Elle oracle is structurally blind to conflict-semantics divergence, and the
   two-backend harness already exists); the full seeded-workload replay is **Tier 2** (needs the Tier-2
   drivers + Docker + version masking). (§8, Tier 1)
3. **5s-window/size-limits → per-limit fidelity, not one knob.** SSI/1020 is HIGH-fidelity and
   non-stubbable; the 5s window is a fixed logical-version knob (no real GC in v1, inject 1007/1009/1021
   via BUGGIFY); the three size limits are ~3-line constant guards kept as regression sentinels — and
   **all three codes ship** (2101/2102/2103; the earlier draft dropped `key_too_large 2102`). MVCC
   reads-at-read-version is the one hard non-stubbable requirement. (Tier 1 item 5)
4. **Watches → v2, and deferral is free.** Watch is off the `BackendDatabase` interface (RFC-109) and
   the record/relational core uses zero watches, so a v1 SimFDB omits the method entirely with no
   coverage loss; the v2 model is fire-on-value-**change** armed at the committed version (RFC-170), not
   "fired at the committing mutation." (Tier 1 deferrable)
