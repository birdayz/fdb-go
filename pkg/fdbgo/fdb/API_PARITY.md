# FDB Go Facade — API Parity with Apple Go Binding

This package (`pkg/fdbgo/fdb`) provides a drop-in compatible API with the
[Apple FoundationDB Go binding](https://github.com/apple/foundationdb/tree/main/bindings/go).

## Parity Status (verified 2026-04-11)

| Type | Apple Methods | Our Methods | Status |
|---|---|---|---|
| `Transaction` | All | All | ✅ Full parity |
| `Snapshot` | All | All | ✅ Full parity |
| `Database` | All | All + `InvalidateGRVCache`, `OpenTenantById` | ✅ Superset |
| `TransactionOptions` | 49 | 49 | ✅ Full parity |
| `DatabaseOptions` | 19 | 20 | ✅ Superset |
| `directory` package | All | All (ported) | ✅ Full parity |
| Key selectors | All 4 | All 4 | ✅ Full parity |
| `subspace` package | All | All | ✅ Full parity |
| `tuple` package | All | All | ✅ Full parity |

## Key Difference

The Apple binding uses the CGo `libfdb_c` library. Our facade wraps a
**pure Go FDB client** — no C dependencies, no CGo overhead, simpler
deployment.

## Cross-Client Compatibility

Verified via 15 interop tests that our facade produces identical results
to the Apple CGo binding for:
- Read/write operations
- Atomic operations (ADD, AND, OR, XOR, etc.)
- Key selector resolution (all 4 types)
- Reverse range scans
- Versionstamp operations
- Conflict detection
- Directory layer operations
