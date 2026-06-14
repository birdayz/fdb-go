# RFC-109: libfdb_c escape hatch — a config-selectable battle-tested backend

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
4. **Config selects the backend at construction.** `fdb.OpenDatabaseWithBackend(BackendLibFDBC,
   clusterFile)` (default `BackendGo`), surfaced as a `recordlayer` factory field / `FDB_BACKEND`
   env. One `fdb.Database`/process = one backend (see lifecycle, below).

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
  switch** between backends within a live process. The config (`FDB_BACKEND`) is read once at
  database construction.
- The pure-Go client and libfdb_c **can coexist** in one process (separate stacks, no shared C
  state) — already proven by `bench_test.go:88-101`, which opens both against one cluster. The two
  "API versions" are independent in-process bookkeeping; only libfdb_c touches the C network.
- **Future resolution must not block an OS thread per in-flight read.** `cgofdb.FutureByteSlice.Get`
  calls `fdb_future_block_until_ready`, which parks the calling goroutine *on a cgo call* (pins the
  M, not just the G). For the record layer's fan-out reads that is thread-pool pressure, not just
  latency. The backend resolves futures via `fdb_future_set_callback` → channel (mirroring the
  pure-Go future), **not** naive `block_until_ready` in a thunk.

## Retry / errors — delegate to libfdb_c, map codes 1:1 (FDB C++ dev)

- **`OnError` is driven through `cgofdb.Transaction.OnError`** (libfdb_c's own retry state machine),
  **not** re-implemented by the Go retry loop. `commit_unknown_result` (1021) idempotency, the
  `transaction_too_old` (1007) / `not_committed` (1020) classification, and backoff are libfdb_c's
  job on that backend — the Go `runTransactCtx` loop calls the backend's `OnError` and trusts it.
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
  stamp read back via `GetVersionstamp()` matches what landed. (Most likely adapter-bug site.)
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
  TransactCtx` callbacks from `Transaction` → `WritableTransaction`; propagate the type through the
  ~86 record-layer callbacks (mechanical, compiler-enforced). **No new backend, pure-Go path
  runtime-unchanged** (the concrete `fdb.Transaction` still satisfies the interface; the pipelined
  `Get` is untouched). The whole existing suite is the regression — and because the hot path is
  byte-for-byte the same code, there is no perf slice to benchmark (unlike Plan B).
- **Phase B — the libfdb_c backend** (`backend_libfdb_c.go`, `//go:build cgo`): `libfdbcDatabase`/
  `libfdbcTxn` over `cgofdb`, with callback-based future resolution, `OnError` delegation, raw-int
  options, and 1:1 error mapping. A `//go:build !cgo` stub makes `OpenDatabaseWithBackend(
  BackendLibFDBC, …)` return a clear *"built without cgo / libfdb_c support"* error (Torvalds — the
  default build must compile and fail gracefully, not reference a missing type).
- **Phase C — config switch + differential.** Wire `FDB_BACKEND` / the factory; add the differential
  suite above (versionstamp-structure, conflict/RYW, snapshot/GRV, record-layer, cross-backend) + the
  operator runbook ("flip to libfdb_c and back").

Each phase merges before the next.

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
