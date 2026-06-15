# RFC-109: libfdb_c escape hatch — a build-tag-selectable battle-tested backend

**Status:** DRAFT (v2 — reworked after FDB C++ dev + Torvalds NAK of the v1 "Plan B" inner
interface). Client launch-readiness #6 (TODO-production P2.2). **`· L` (large)**; phased.
Wire compatibility is the whole point and the hard line.

## Problem — 86 files bet on a young client with no fallback

`86` non-test files import `pkg/fdbgo/fdb`, the from-scratch pure-Go FDB client. It is young
and recently-churning — it once crashed the FDB *server* (fixed; `pkg/fdbgo/client/CRASH_BUG.md`),
and this very work-stream fixed two more client bugs (GRV refresher on opt-in miss, retry
predicates). The Apple **`libfdb_c`** CGo binding is the decade-hardened reference — but here it
is **test-only**: imported solely by `pkg/fdbgo/bench` as the differential oracle
(`cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"`, `bench_test.go:14`).

So there is **no production fallback**: if the pure-Go client regresses under a real workload, an
operator cannot flip to `libfdb_c` without a code change + redeploy. Torvalds on P2.2:
*"mandatory for any bet-the-company write path."* A serious adopter must be able to run the
record layer on `libfdb_c` by **config**, and switch back to the pure-Go client when they trust it.

## The seam does NOT already exist — and the v1 fix (Plan B) was wrong

It *looks* like `fdb.Transactor` + `recordlayer.NewFDBDatabaseWithTransactor(transactor, db)`
(`pkg/recordlayer/database.go:112`) abstracts the backend. It does not: the seam passes a
**concrete** type.

```go
// pkg/fdbgo/fdb/interfaces.go:7
type Transactor interface {
    Transact(func(Transaction) (any, error)) (any, error)   // ← concrete fdb.Transaction
    ReadTransactor
}
```

`fdb.Transaction` is a concrete struct whose every method is hard-wired to the pure-Go client
(`transaction.go:12` `inner *client.Transaction`; `Get` calls `inner.GetPipelined`). A
`cgofdb.Transaction` can't be poured into it. `ChaosTransactor` works only because it *delegates to
a real pure-Go `fdb.Transaction`*; it never substitutes the backend.

**v1 proposed "Plan B"** — keep `fdb.Transaction` concrete and make its *inner* a `backendTxn`
interface returning `([]byte, error)`. Both reviewers NAK'd, correctly:
- **Torvalds:** a `([]byte, error)` inner **cannot express the pipelined `Get` fast-path**
  (`transaction.go:55-82`: synchronous `GetPipelined` returning a future backed by a reply
  channel — the `pending != nil` branch). Flattening it to `([]byte,error)` would *degrade the
  pure-Go client*. And Plan B invents a *second, parallel* ~40-method abstraction of operations the
  existing `WritableTransaction` interface already describes — two surfaces to keep in sync.
- **FDB C++ dev:** the architecture was directionally right but missing the load-bearing libfdb_c
  lifecycle / onError / differential sections (below).

## Proposed design — Plan C: route the record layer through the EXISTING interfaces

The read side is **already** interface-based: `ReadTransactor.ReadTransact(func(ReadTransaction))`
(`interfaces.go:14`) takes the `ReadTransaction` *interface*. Only the **write** side passes the
concrete `Transaction`. So the change is small and reuses what exists:

1. **Widen two write-side callbacks from the concrete type to the interface** that already exists
   and that `fdb.Transaction` already satisfies exactly (`WritableTransaction`, `interfaces.go:54`):
   ```go
   type Transactor interface {
       Transact(func(WritableTransaction) (any, error)) (any, error)   // was func(Transaction)
       ReadTransactor
   }
   type CtxTransactor interface {
       TransactCtx(ctx context.Context, f func(WritableTransaction) (any, error)) (any, error)
   }
   ```
2. **The pure-Go path is RUNTIME-UNCHANGED.** `fdb.Transaction` keeps its exact concrete impl —
   the pipelined `Get`, RYW, everything. `Database.Transact` still constructs an `fdb.Transaction`
   and passes it to the callback; because `fdb.Transaction` already satisfies `WritableTransaction`,
   that is a **pure static-type change at the call boundary** — zero runtime cost, the pipeline is
   untouched. (This is the decisive advantage over Plan B: Plan C does **not** rewrite the hot
   read path, so there is no perf regression to benchmark away.)
3. **The cgo backend implements the same interfaces.** A `libfdbcDatabase` (`Transactor` +
   `CtxTransactor`) whose `Transact` builds a `libfdbcTxn` that satisfies `WritableTransaction` by
   forwarding to `cgofdb.Transaction`. The record layer calls `tr.Get(...)`/`tr.Set(...)` through
   the interface, blind to the backend.
4. **A build tag selects the backend.** Application source is backend-agnostic — it opens through
   `fdbclient.Open(clusterFile)` — and a build tag, NOT a runtime flag, picks the client:
   `go build` → pure-Go (default, no cgo); `CGO_ENABLED=1 go build -tags libfdbc` → libfdb_c. This is
   the standard-library netgo/netcgo idiom and the mattn-go-sqlite3-vs-modernc swap: exactly one
   client is linked, and a default build never pulls in cgo or the C library. One binary = one
   backend, which is also the physical reality (see lifecycle, below). *(v1 used a runtime
   `OpenDatabaseWithBackend(BackendLibFDBC, …)` enum + an `init()` registration; that was replaced
   with the build tag — since the choice is launch-time-static anyway, a build flag states it plainly
   and keeps the C dependency out of every build that does not ask for it. The idiom was vetted by
   stdlib-net and sqlite-driver reviewers + Torvalds.)*

**Why Plan C over Plan B (Torvalds' point, accepted):** one compiler-enforced interface that
already exists, not a second hand-maintained ~40-method abstraction. The cost is widening a
callback type across the ~86 importers — but that is a **mechanical, gofmt-able,
compiler-verified** `fdb.Transaction` → `fdb.WritableTransaction` substitution in the callback
position (Phase A), not 86 hand-edits. A blast radius the compiler keeps honest beats a small
clever seam it cannot.

## libfdb_c lifecycle — once per process, construction-time only (FDB C++ dev)

This is load-bearing and was missing in v1. `fdb_select_api_version` is **process-global, callable
exactly once** (`fdb_c.h`; `cgofdb.APIVersion` panics on a second call). On first `OpenDatabase`
the cgo binding calls `fdb_setup_network` + `fdb_run_network`, spinning **one** dedicated C network
thread that owns all libfdb_c futures/callbacks; `fdb_stop_network` is **one-shot and
unrecoverable**. Consequences the implementation MUST honor:

- The libfdb_c backend **lazily initializes the global network exactly once and never tears it
  down.** Backend selection is therefore a **process-launch-time** decision — there is **no runtime
  switch** between backends within a live process. This is exactly why selection is a **build tag**
  (`fdbclient`, `-tags libfdbc`) and not a runtime config: a binary physically runs one client for
  its whole life, so encoding the choice in the build is honest and keeps cgo/libfdb_c out of builds
  that do not opt in.
- The pure-Go client and libfdb_c **can coexist** in one process (separate stacks, no shared C
  state) — already proven by `bench_test.go:88-101`, which opens both against one cluster. The two
  "API versions" are independent in-process bookkeeping; only libfdb_c touches the C network.
- **Future resolution must not block an OS thread per in-flight read.** `cgofdb.FutureByteSlice.Get`
  calls `fdb_future_block_until_ready`, which parks the calling goroutine *on a cgo call* (pins the
  M, not just the G). For the record layer's fan-out reads that is thread-pool pressure, not just
  latency. The backend resolves futures via `fdb_future_set_callback` → channel (mirroring the
  pure-Go future), **not** naive `block_until_ready` in a thunk.

## Retry / errors — delegate to libfdb_c, map codes 1:1 (FDB C++ dev)

- **The retry DECISION and backoff are `cgofdb.Transaction.OnError`'s** (libfdb_c's own retry state
  machine) — `commit_unknown_result` (1021) idempotency, the `transaction_too_old` (1007) /
  `not_committed` (1020) classification, and the backoff schedule are libfdb_c's job; the adapter
  re-raises `fdb.Error` codes 1:1 and trusts `OnError`'s verdict (retryable ⇒ nil ⇒ reset; terminal
  ⇒ error). **The retry LOOP itself is Go-driven** (`runLoop`), NOT `cgofdb.Database.Transact`, for
  one reason: cgofdb's own `retryable()` runs `OnError(...).Get()` with no `ctx`, so a cancel/deadline
  during the inter-attempt backoff is not honored until the next attempt — violating the
  `CtxTransactor` contract `recordlayer.Run` relies on (pure-Go bounds the backoff via
  `transaction.go` `backoffSleep`). `runLoop` waits on the `OnError` future under `ctxBoundedWait`
  (ctx cancel ⇒ cancel the tx ⇒ unblock the future ⇒ return `ctx.Err()`); the backoff *algorithm*
  stays libfdb_c's, only the *wait* is made cancellable. The auto-commit is detached (no watcher, no
  FDB timeout), matching the pure-Go `WithoutCancel` commit.
- **FDB error codes map 1:1.** `cgofdb.Error.Code` and `fdb.Error.Code` are both the raw
  `fdb_error_t` int, so `errors.As`/retry on the numeric code is identical — *provided the adapter
  preserves the integer and synthesizes nothing*. The pure-Go client surfaces a few **client-side**
  conditions that libfdb_c expresses differently or absorbs internally — these have **no libfdb_c
  analog** and the adapter must NOT invent them: `ErrNeedFullRYW` (pure-Go RYW-merge signal,
  internal), and the layer-2 `all_alternatives_failed` (1006) the pure-Go read path synthesizes +
  absorbs (`transaction.go:64-75`). On the libfdb_c backend those paths simply don't exist; the
  differential must compare on FDB error *codes*, not on these Go-internal sentinels.
- **Options by raw integer.** The backend sets transaction/database/network options via
  `fdb_transaction_set_option(opt_int, val)` / `fdb_database_set_option` / `fdb_network_set_option`
  using the SAME integer codes both clients generate from `fdb.options` — NOT by re-deriving through
  `cgofdb`'s typed setters (a renumbered/missing typed setter would silently no-op). Network options
  (`SetKnob`, `SetTraceEnable`) and database options are plumbed, or the backend launches with
  default knobs.

## Wire compatibility — the differential plan (reworked, FDB C++ dev)

Both backends talk to the **same** cluster and MUST read/write byte-identical records, index
entries, continuations, split records, versionstamps, and conflict ranges. Byte-comparing disjoint
subspaces is necessary but **insufficient** — the gaps that actually break cross-engine are
transaction-internal or per-transaction:

- **Versionstamps** — the 10-byte stamp is assigned by the cluster at commit and differs per txn, so
  a raw byte-compare is wrong. Compare **structure**: the offset placement, the 2-byte LE position
  suffix the client appends, and `SetVersionstampedKey` vs `…Value` opcode; and assert the committed
  stamp read back via `GetVersionstamp()` matches what landed. (Most likely adapter-bug site.) Also
  pin the *resolve-after-commit* semantics: the pure-Go `GetVersionstamp()` blocks on the commit
  (`transaction.go:129`); cgofdb's future also resolves post-commit, but the differential asserts
  both surface the stamp only after `Commit` (FDB C++ dev).
- **Conflict ranges / RYW** — persisted bytes can't observe them. Add a **concurrent-conflict
  differential** (two txns; exactly one must get `not_committed` 1020 under each backend) and an
  **RYW-ordering differential** (set-then-get, clear-then-range, atomic-then-get — the exact
  `ErrNeedFullRYW` path the pure-Go client special-cases).
- **Snapshot reads & GRV** — include a snapshot read (no conflict added) under both backends;
  snapshot-vs-serializable is a per-read flag in libfdb_c, not a sub-transaction — easy to get wrong.
- **Record-layer differential** (the gold gate) — run `saveRecord`/`loadRecord`, index maintenance,
  a range scan with a continuation, a versionstamped write, an atomic counter through a store backed
  by each backend on disjoint subspaces; byte-compare the keyspace via a neutral reader.
- **Cross-backend read** — write with backend A, read with B (and vice-versa): the actual operator
  scenario (flip the flag; existing data still reads identically).
- The existing `pkg/fdbgo/bench` differential + the 23 client fuzz targets keep gating the pure-Go
  side.
- **Tenants** are out of scope for v1 (libfdb_c `fdb_database_open_tenant` the pure-Go client may not
  mirror) — declared explicitly; the escape hatch covers the non-tenant record-layer path.

## Phasing (`· L` — reviewable slices, each its own stacked PR)

- **Phase A — widen the seam to the interface.** Change `Transactor.Transact` / `CtxTransactor.
  TransactCtx` callbacks from `Transaction` → `WritableTransaction`. **The real surface is bigger
  than the callback (Torvalds — don't undercount it):**
  - **Widen `WritableTransaction` itself** to add the six `[]byte` overloads
    `SetBytes`/`ClearBytes`/`AddBytes`/`MaxBytes`/`MinBytes`/`CompareAndClearBytes`
    (`transaction.go:202-308`). They are NOT in the interface today, but **34** hot-path
    index-maintenance call sites invoke them through the bound `tx` (`atomic_mutation.go`, the
    version/rank index maintainers, …). Widening the interface (vs. rewriting those call sites to
    the `KeyConvertible` form) is preferred — the overloads exist to avoid boxing on that path. Both
    backends implement them.
  - **Cascade the type through the ~129 record-layer/relational helper functions** that take a
    plain `fdb.Transaction` parameter (`ranked_set.go`, `range_set.go`, `rtree.go`, the maintainers,
    …) → `WritableTransaction`, since under the escape hatch the `tx` handed in may be cgo-backed.
    Compiler-enforced, but a genuine ~129-signature sweep, not a one-line callback swap.
  - `Watch`/`Locality`/tenant concrete-only methods are NOT called through `tx` in the layer
    (verified), so they stay OFF the interface.
  **No new backend; pure-Go path runtime-unchanged** — `fdb.Transaction` still satisfies the
  (widened) interface (`check.go:12` `_ WritableTransaction = Transaction{}` stays green) and the
  pipelined `Get` (`transaction.go:50-83`) is byte-for-byte untouched, so there is no perf slice to
  benchmark (unlike Plan B). The whole existing suite is the regression.
- **Phase A.2 — interface-ize the concrete return types the cgo backend can't construct
  (DISCOVERED DURING IMPLEMENTATION).** Widening the `Transactor` callback (A.1) is necessary but
  NOT sufficient: `ReadTransaction.GetRange(...)` returns the concrete `fdb.RangeResult` struct and
  `ReadTransaction.Snapshot()` returns the concrete `fdb.Snapshot`, both of which wrap the pure-Go
  `*transaction` internal (`range_result.go:11`) — a cgo backend physically cannot build them. So
  `RangeResult` (10 non-test usages, all `var x fdb.RangeResult`), its `Iterator()`'s
  `*RangeIterator` (3 usages), and `Snapshot` (0 type-usages in the layer) must become INTERFACES,
  with the pure-Go structs as one impl and the cgo backend as the other. Tractable (the futures —
  `FutureByteSlice` etc. — are already interfaces, so they need no change), but it is real scope the
  v2 RFC under-counted; folded back here for the reviewers. Still a behavior-preserving refactor
  (suite is the regression), still no perf slice (pure-Go impls unchanged).
  **Two more return types surfaced the same way and are folded in:** `ReadTransaction.Options()`
  returned the concrete `TransactionOptions` struct (wraps `*transaction`, 52 option methods) → it
  becomes an INTERFACE so the cgo backend can forward each option independently (the pure-Go struct
  is renamed `goTransactionOptions`, one impl). And `ReadTransaction.GetDatabase()` returned the
  concrete pure-Go `fdb.Database` (a ~40-method handle no cgo backend can build) — it has **zero**
  production callers and is never invoked through the interface, so it comes **OFF** the
  `ReadTransaction` interface entirely (concrete-only on `Transaction`/`Snapshot`, same treatment as
  `Watch`/`Locality`/tenant). After A.2 every method on the read/write interfaces returns an
  interface, a value struct (`fdb.Error`), or a primitive — so the cgo backend can implement them.
- **Phase B — the libfdb_c backend** (a SEPARATE package `pkg/fdbgo/libfdbc`, not a `//go:build cgo`
  file inside `fdb`). Implementation findings that refine the v2 plan (each verified against the
  cgofdb source / fdb_c.h and re-reviewed):
  - **Separate package, not same-package.** A `//go:build cgo` file *inside* `pkg/fdbgo/fdb` would
    make the pure-Go client itself link `libfdb_c` whenever `CGO_ENABLED=1` (the auto `cgo` tag). A
    separate `pkg/fdbgo/libfdbc` is linked **only when the build asks for it**, so the pure-Go client
    stays cgo-free. Package `fdb` exposes only the backend-agnostic surface (`BackendDatabase` +
    `Database.IsValid`); it never imports the cgo package. The selector is the tiny `pkg/fdbgo/fdbclient`
    shim — `open_purego.go` (`//go:build !libfdbc`) returns the pure-Go client and `open_libfdbc.go`
    (`//go:build libfdbc`) returns `libfdbc.Open`, so a default build never even imports `libfdbc`. The
    `//go:build !cgo` stub still exists (in `libfdbc`), returning a clear *"built without cgo"* error
    for the nonsensical `-tags libfdbc` + `CGO_ENABLED=0` combination — the netgo `cgo_stub.go` pattern.
  - **Build on cgofdb's high-level API, not raw libfdb_c calls.** The v2 RFC mandated
    `fdb_future_set_callback`→channel because it believed `cgofdb.Future.Get` pins an OS thread per
    in-flight read. **Reading the binding refutes that** (`bindings/go/src/fdb/futures.go`):
    `BlockUntilReady` registers `fdb_future_set_callback` and then parks on a `sync.Mutex` — a
    Go-runtime park that frees the M while the C network thread fires the callback. So cgofdb already
    does the callback→channel design; forwarding to it inherits correct, non-thread-pinning
    resolution. The retry *decision* + *backoff* are libfdb_c's (`tr.OnError` → `fdb_transaction_on_error`),
    but the retry *loop* is Go-driven (`runLoop`, not `cgofdb.Database.Transact`) so the inter-attempt
    backoff WAIT is ctx-bounded — cgofdb's own `retryable()` waits with no ctx (see the Retry section);
    error codes map 1:1, nothing synthesized.
  - **Options via cgofdb's typed setters** (its raw `setOpt(code,param)` is unexported). 49 of the 52
    options map to an identically-named generated setter (same `fdb.options` code); the 3 cgofdb
    lacks a setter for or that have no libfdb_c analog (`SkipGrvCache`, `WriteConflictsDisabled`,
    `EnsureMutationCapacity`) are documented per-method no-ops — a known v1 limitation, not a silent
    divergence.
  - **Scope: the Transactor-driven gold path.** `BackendDatabase` is `Transactor + Close`; the cgo
    `database` drives `FDBDatabase.Run`/`RunRead` (record save/load, query, index maintenance). The
    pure-Go-only direct paths — `CreateTransaction`, the manual `FDBDatabaseRunner`, and
    `LocalityGetBoundaryKeys` (online mutual indexing) — return concrete pure-Go handles a cgo backend
    cannot build, so they stay pure-Go-only in v1 (fail-fast `BackendCapabilityError` / graceful
    single-fragment degradation), the same scope boundary the RFC already draws around tenants.
- **Phase C — build-tag switch + differential.** `fdbclient.Open` (build-tag-selected) +
  `recordlayer.NewFDBDatabaseWithBackend`. The differential gate is a record-layer test
  (`pkg/fdbgo/libfdbc/differential_test.go`, `//go:build cgo`) against one real FDB: **cross-backend
  round-trip** (save through one backend, read through the other on the same subspace — the operator
  flip), **byte-identical keyspace** (same records saved through each backend on disjoint subspaces;
  the record + index keyspaces compared byte-for-byte through a neutral reader), and **split-record
  wire compat** (a >100KB record split across keys, written by cgo, read by pure-Go, byte-compared).

One PR, multiple commits (phases as commits, not stacked PRs) — per the maintainer's call.

## Reviewers

- **FDB C++ dev** (final on wire/client correctness): the backend IS `libfdb_c`, so scrutiny is on
  the *translation* — futures resolved at the right point, error codes 1:1, options by raw int,
  `OnError` delegated, no forced GRV / conflict-range divergence, the network-thread lifecycle.
  Cite `cgofdb` + the C API.
- **Torvalds** (code quality): Phase A is a pure type-widening refactor — prove zero behavior change
  (suite green, hot path untouched); the cgo build tag doesn't bit-rot the default build; no dead
  code.
- PR gauntlet: codex + @claude per the client-review gauntlet.

## What this does NOT do

- Does **not** make libfdb_c the default — the pure-Go client stays default; libfdb_c is the opt-in
  escape hatch (the pure-Go client is the project's reason for existing).
- Does **not** add FDB functionality — both backends expose the exact same operations.
- Does **not** support runtime backend switching (the libfdb_c network thread is once-per-process) —
  it is a launch-time config.
- Does **not** cover tenants (v1) — declared out of scope.
