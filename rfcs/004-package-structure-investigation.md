# RFC 004: Package Structure Investigation — Multi-Package Split

## Status: Rejected

Supersedes the multi-package direction considered in RFC 003. Confirms RFC 003's Option B (unexport in place) as correct. Adds nogo layering enforcement as a concrete next step.

## Motivation

58 `.go` files in one flat `pkg/recordlayer/` package. No compile-time enforcement of layering. Any file can call any function. The dependency graph between subsystems (store, indexes, cursors, key expressions, metadata) is invisible — violations are only caught by code review.

The question: should we split into multiple Go packages to get compiler-enforced boundaries?

## Investigation

Three independent review agents analyzed the proposal in parallel, examining cycle feasibility, user ergonomics, and alternative structures.

### What Java does

Java Record Layer splits across ~15 packages:

```
record/                              # Core interfaces, enums, exceptions
  cursors/                           # Generic cursor combinators
  metadata/
    expressions/                     # KeyExpression types (Field, Nesting, Concat, etc.)
  provider/
    foundationdb/                    # FDB-specific: FDBRecordStore, FDBDatabase
      indexes/                       # ALL index maintainers
      cursors/                       # FDB-specific cursors (Union, Intersection)
      indexing/                      # OnlineIndexer
      storestate/                    # Store state caching
  query/                             # Query planner (200+ classes, ~40% of codebase)
```

Key observations:
- The package split is driven by the **query planner** (200+ classes), not the record store
- Stripping `query/`, Java's core store code is ~80 classes in one package — similar to our 58 files
- Java's split **forced** index maintainers to be `public` (cross-package access). They then invented `@API(INTERNAL)` to say "public but don't use this" — a code smell caused by the package boundary
- Java has no import cycle restriction. A class in `indexes/` can freely import from `record/` while `record/` imports from `indexes/`. Go cannot do this.

### The proposed structure

```
pkg/recordlayer/                      # Root — FDBRecordStore, database, builder
  core/                               # Shared types (cycle-breaker)
  keyexpr/                            # Key expression types
  metadata/                           # Schema definition
  cursor/                             # Cursor combinators
  internal/
    index/                            # Index maintainer implementations
    split/                            # Split record helper
    rangeset/                         # RangeSet
    kvcursor/                         # Key-value cursor internals
    statecache/                       # Store state cache implementation
```

5 public packages + 5 internal packages. Dependency DAG flows strictly downward.

### Why it fails: the irreducible type cycle

The codebase has a mutual dependency loop that **cannot be split across Go packages**:

```
KeyExpression.Evaluate(*FDBStoredRecord, proto.Message)
         |
FDBStoredRecord.RecordType → *RecordType
         |
RecordType.PrimaryKey → KeyExpression
```

Plus:
- `FDBStoredRecord.Store → *FDBRecordStore` (needed by `FunctionKeyExpression` → `get_versionstamp_incarnation`)
- `FDBRecordStore.metaData → *RecordMetaData` (contains `KeyExpression` instances)

These 7 types form an irreducible kernel: `FDBStoredRecord`, `RecordType`, `KeyExpression`, `Index`, `IndexMaintainer`, `IndexEntry`, `RecordMetaData`. Any split that puts them in different packages creates an import cycle — a compile error in Go.

**To break the cycle**, you must either:
1. Extract all 7 types into a shared `core/` package, or
2. Replace concrete types with interfaces in `KeyExpression.Evaluate` signature

Both options have severe costs (see below).

### Why `core/` becomes "the package"

If you extract shared types to `core/` to break cycles, it absorbs ~80% of all types. The "root" package becomes a thin wrapper. You've just renamed the package and forced users to write two imports instead of one. `core/` is not a meaningful abstraction — it's "everything except the store."

### User ergonomics are bad

Popular Go libraries use ONE consumer-facing package:
- `database/sql` — one package
- `net/http` — one package
- `google.golang.org/grpc` — one package
- `go.etcd.io/etcd/client/v3` — one package

The proposed split requires 3-5 imports for basic CRUD:

```go
import (
    rl "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
    "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/core"
    "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/keyexpr"
    "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/metadata"
    "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/cursor"
)
```

This is a Go anti-pattern.

### Can `keyexpr/` go internal?

No. Users call `Field()`, `Concat()`, `Nest()`, `GroupBy()` etc. directly when building metadata. These are public API. Re-exporting 30+ symbols from the root package is tedious and defeats the purpose.

## Alternatives evaluated

| Alternative | Viable? | Trade-off |
|---|---|---|
| **A: Two packages** (`recordlayer/` + `recordlayer/internal/`) | No | Fatal import cycle: parent dispatches to internal, internal uses parent types |
| **B: core + internal** (`core/` breaks cycle) | Technically yes | `core/` absorbs 80% of types. Two imports always. Not a real boundary. |
| **C: 5+5 split** (the full proposal) | Technically yes | Over-engineered for 58 files. 3-5 imports. Massive migration. |
| **D: Flat + nogo linting** | Yes | Zero migration. Enforced via Bazel build. Not checked outside Bazel. |
| **E: schema/store split** (two packages) | Future option | Cleanest boundary but requires `Evaluate` interface change. Do later. |

## Decision

**Stay flat. Add nogo layering enforcement.**

This is RFC 003 Option B (unexport in place) plus compile-time lint rules.

### Rationale

1. **The cycle problem is expensive to fix.** Changing `KeyExpression.Evaluate(*FDBStoredRecord, proto.Message)` to use an interface touches every key expression implementation (15+ types), every index maintainer, every test. Massive disruption for marginal benefit.

2. **The real problem is navigability, not safety.** With one developer, 91 chaos tests, and 1640 test specs, the risk of "accidentally creating bad internal dependencies" is near zero. The issue is "67 files is getting hard to browse." That's a file organization problem, not a package boundary problem.

3. **Java doesn't have this split either.** `FDBRecordStore.java` is 5800+ lines. The core store package is ~80 public classes. They've maintained it for years without the package split causing problems.

4. **nogo rules ARE build errors.** Since this project uses Bazel exclusively (`just build`, `just test`), nogo analyzer violations are compile errors in practice.

5. **The API is still evolving.** Doing a big structural refactor now means redoing it when boundaries turn out wrong. Unexporting is trivially reversible; package moves are not.

### Concrete actions

**Done (RFC 003 Phase 1):** Unexported 11 concrete index maintainer types, `RankedSet`, `RankedSetConfig`, `SizeInfo`, format version constants, split/size constants. Added `RankQuerier` interface for chaos test access.

**Future (when codebase reaches ~100+ files):** Revisit Alternative E — split into `schema/` (metadata + key expressions, no FDB dependency) and `recordlayer/` (FDB-touching store code). This is the one clean abstraction boundary. Requires breaking the `Evaluate` cycle via an interface, which is justified at that scale.

## Appendix: dependency DAG (current, within the flat package)

```
Layer 5: store (CRUD, builder, online indexer, store functions)
    |
Layer 4: index maintainers (VALUE, COUNT, SUM, RANK, VERSION, ...)
    |
Layer 3: cursors (kv cursor, index cursor, combinators)
    |
Layer 2: metadata (RecordMetaData, Index, RecordType)
    |
Layer 1: key expressions (Field, Concat, Nest, Grouping, Version, ...)
    |
Layer 0: errors, constants, FDBRecordVersion, ScanProperties, FDBRecordContext
```

All dependencies flow downward. No layer reaches upward except through interfaces (`indexStoreContext`). The layering is clean — it just isn't compiler-enforced today.
