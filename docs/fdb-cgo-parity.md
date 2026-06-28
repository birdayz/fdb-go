# FDB client cgo-parity audit

`pkg/fdbgo/fdb` is the only public FoundationDB client (the build-tag backend
selector moved to `pkg/internal/fdbclient`). That makes any gap versus Apple's C
binding a user-visible gap, so this audit compares our surface to Apple's Go
binding and records what matches, what is intentionally different, and what was
closed.

## Method

Diff of `go doc` output for both packages:

- ours: `fdb.dev/pkg/fdbgo/fdb`
- reference: `github.com/apple/foundationdb/bindings/go/src/fdb` (pinned
  `v0.0.0-20250702211439-37fcf1c8ce08`, in the module graph via the libfdbc backend)

Compared package functions, constructors, and the method sets of `Database`,
`Transaction`, `ReadTransaction`, `Snapshot`, plus `KeySelector` helpers and the
supporting types.

## Result: high parity

The core read/write surface is at full parity or a superset of Apple's binding.

| Surface | Status |
|---|---|
| Constructors (`OpenDatabase`, `OpenDefault`, `Open`, `MustOpen*`, `OpenWithConnectionString`) | Full parity (plus `OpenDatabaseFromConfig`, `WrapDatabase`) |
| `Database` methods | Superset (every Apple method, plus `TransactCtx`/`ReadTransactCtx`, `IsValid`, hedge options, `OpenTenantById`) |
| `Transaction` methods | Superset (every Apple method, plus `*Bytes` key-convenience variants) |
| `Snapshot` methods | Exact parity |
| `KeySelector` helpers (`FirstGreaterThan`, `FirstGreaterOrEqual`, `LastLessThan`, `LastLessOrEqual`) | Full parity |
| Futures (`Get`/`MustGet` across byte-slice/key/int64/nil/array) | Full parity |
| Atomic ops (Add, And, Or, Xor, Bit*, Byte*, Max, Min, AppendIfFits, CompareAndClear, SetVersionstamped*) | Full parity |
| Tenants, locality, conflict ranges, range split points, estimated sizes | Full parity |

## Gaps closed in this pass

Four trivial package functions Apple had and we lacked:

- `IsAPIVersionSelected() bool`
- `MustGetAPIVersion() int`
- `StartNetwork() error`, `StopNetwork() error` (no-ops for source compatibility;
  the pure-Go client has no global network thread, see `network.go`)

## Intentional differences (kept, not bugs)

- **Write transactions are an interface because there are two backends.**
  `Transact`'s callback receives `WritableTransaction` (interface) where Apple
  passes the concrete `Transaction`. This is the seam that lets the Record Layer
  and SQL engine run over either client: `pkg/fdbgo/libfdbc` implements
  `fdb.WritableTransaction`, and the layers hold the interface (about 290 call
  sites in `pkg/recordlayer` alone), so `-tags libfdbc` swaps the concrete client
  underneath with no source change. Apple's binding has a single backend, so a
  struct is enough for it; we have two, so a concrete type would weld the layers
  to the pure-Go client and break libfdb_c support. The only user-facing cost is
  the closure parameter type (`fdb.WritableTransaction` vs `fdb.Transaction`); the
  calls inside (`tx.Set`, `tx.Get`, ...) are identical. `ReadTransact` uses
  `ReadTransaction` in both.
- **A few supporting types are interfaces for the same reason:**
  `TransactionOptions`, `RangeResult`, `RangeIterator` are produced by both
  backends, so they are interfaces rather than structs. The method sets match.
- **Absent by design:** `NetworkOptions` (no global network thread to configure),
  `Cluster` (legacy, gone from modern Apple too), `ErrorPredicate` (we expose
  `IsRetryable` / `IsOnErrorRetryable` functions instead of the predicate type).

## Verdict

Apple-binding code ports with minimal changes: select the API version, open a
database, and use the same transaction methods. The only routine edit is the
`Transact` callback parameter type. No missing core capability was found.
