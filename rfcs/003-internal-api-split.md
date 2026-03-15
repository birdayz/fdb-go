# RFC 003: Split Between Exported API and Internal Implementation

## Status: Draft

## Problem

`pkg/recordlayer/` is a single flat package with 70 implementation files exporting ~120 types. Users importing `github.com/birdayz/fdb-record-layer-go/pkg/recordlayer` see everything: the 15 types they actually need, plus 12 index maintainer structs, split helpers, range set internals, subspace constants, format version numbers, and implementation details they should never touch.

This creates three problems:

1. **No API contract.** Every exported type is a promise. If someone depends on `StandardIndexMaintainer` directly (which they shouldn't — it's an implementation detail), we can't refactor it without a breaking change. Right now we have zero external users so this is free to fix. Later it won't be.

2. **Discoverability.** `go doc github.com/birdayz/fdb-record-layer-go/pkg/recordlayer` is a wall of text. The actual user-facing API (Database, Store, MetaData, Index constructors, Cursors, Errors) drowns in implementation noise.

3. **Contributor confusion.** New contributors can't tell which types are public API vs internal machinery. There's no structural signal — you have to just know that `CountIndexMaintainer` is internal but `Index` is not.

## What the API surface actually looks like

After auditing all 70 files, conformance tests, and chaos tests:

### Must be exported (user-facing API) — ~40 types

| Category | Types |
|----------|-------|
| Database/Context | `FDBDatabase`, `FDBRecordContext`, `FDBDatabaseRunner`, `RecordContextConfig`, `TransactionPriority` |
| Store | `FDBRecordStore`, `FDBRecordStoreBuilder`, `TypedFDBRecordStore[T]`, `FDBStoredRecord[T]`, `FDBIndexedRecord` |
| Metadata | `RecordMetaData`, `RecordMetaDataBuilder`, `RecordType`, `FormerIndex` |
| Index | `Index`, `IndexState`, `IndexRebuildPolicy`, `IndexPredicate`, 14 constructor functions |
| Key expressions | `KeyExpression` interface, `Field()`, `FanOut()`, `Concat()`, `Nest()`, `GroupBy()`, `EmptyKey()`, `RecordTypeKey()`, `Version()`, `KeyWithValue()`, `Literal()`, `FunctionExpr()` |
| Cursors | `RecordCursor[T]`, `RecordCursorResult[T]`, `RecordCursorContinuation`, `NoNextReason`, combinators |
| Scanning | `ScanProperties`, `ExecuteProperties`, `TupleRange`, `IndexEntry`, `ForwardScan()`, `ReverseScan()` |
| Versioning | `FDBRecordVersion` |
| Online indexing | `OnlineIndexer`, `OnlineIndexerBuilder`, `IndexingRangeSet` |
| Validation | `MetaDataValidator`, `MetaDataEvolutionValidator` |
| Caching | `FDBRecordStoreStateCache` interface, `MetaDataVersionStampStoreStateCache`, `PassThroughStoreStateCache` |
| Errors | 20+ error types (`RecordAlreadyExistsError`, etc.) |
| Aggregates | `IndexAggregateFunction`, `IndexRecordFunction`, `EvaluateAggregateFunction()`, `EvaluateRecordFunction()` |

### Should be internal (~30 types) — never used outside the package

| Category | Types | Why internal |
|----------|-------|-------------|
| Index maintainers | `StandardIndexMaintainer`, `CountIndexMaintainer`, `SumIndexMaintainer`, `RankIndexMaintainer`, `VersionIndexMaintainer`, `MaxEverVersionIndexMaintainer`, `MinMaxEverIndexMaintainer`, `MinMaxEverTupleIndexMaintainer`, `CountNotNullIndexMaintainer`, `CountUpdatesIndexMaintainer`, `PermutedMinMaxIndexMaintainer` | Only accessed via `store.getIndexMaintainer()` dispatch. No external caller constructs these. |
| Index maintainer interface | `IndexMaintainer` | Only used for internal dispatch. Users interact via `store.ScanIndex()`, not maintainer methods. |
| RangeSet | `RangeSet` | Only used by `IndexingRangeSet` internally. Users use `IndexingRangeSet`. |
| Split helper | `SizeInfo` | Only constructed internally. Fields exposed on `FDBStoredRecord`. |
| Subspace constants | `StoreInfoKey`, `RecordKey`, `IndexKey`, etc. (the raw int constants) | Users never build subspace keys manually. Store methods handle this. |
| Format versions | `FormatVersionInfoAdded` through `FormatVersionFullStoreLock` | Implementation detail. Users set format version via builder if at all. |
| Store state | `FDBRecordStoreState` | Internal cache state object. |
| Ranked set | `RankedSet`, `RankedSetConfig` | RANK index implementation detail. |

### Borderline — currently undecided

| Type | Argument for export | Argument for internal |
|------|--------------------|-----------------------|
| `IndexMaintainer` interface | Power users might want custom index types | Java doesn't expose this to casual users either; we can export it later |
| `RangeSet` | General-purpose data structure | Only used for index building |
| Subspace constants | Debugging, raw FDB inspection | Exposing them implies we support raw key construction |

## How Java handles this

Java Record Layer uses **annotation-based visibility** instead of package-level hiding. Key findings:

### The `@API` annotation

Every public class/method is annotated with a stability level:

| Level | Meaning | Count (whole project) |
|-------|---------|----------------------|
| `STABLE` | Won't break until next major release | **1** (!) |
| `UNSTABLE` | May change in next minor release | 248 |
| `EXPERIMENTAL` | Under development, may vanish | 572 |
| `INTERNAL` | Public only for cross-package use within Record Layer | 238 |
| `DEPRECATED` | Avoid, removal imminent | 17 |

Notable: almost nothing is `STABLE`. Even `FDBRecordStore` is `UNSTABLE`. The project ships at 4.10.x without a single stable public API contract. They get away with this because they control the primary consumer (FoundationDB's own services).

### Package structure — NOT flat

Java splits across ~15 packages under `com.apple.foundationdb.record`:

```
record/                              # Core interfaces, enums, exceptions
  cursors/                           # Generic cursor combinators (MapCursor, FilterCursor)
  metadata/
    expressions/                     # KeyExpression types (Field, Nesting, Concat, etc.)
  provider/
    common/                          # Serialization (no FDB dependency)
    foundationdb/                    # FDB-specific implementation
      indexes/                       # ALL index maintainers (Standard, Count, Rank, etc.)
      cursors/                       # FDB-specific cursors (Union, Intersection, Merge)
      indexing/                      # OnlineIndexer
      runners/                       # FDBDatabaseRunner, retry logic
      storestate/                    # Store state caching
      keyspace/                      # Directory layer integration
  query/                             # Query planner (huge, 200+ classes)
```

### What Java marks `@INTERNAL` (36 classes in foundationdb/)

These are public classes that exist only because Java forces `public` for cross-package access:
- `SubspaceProvider`, `IndexingSubspaces` — plumbing between packages
- `LocatableResolver`, `ScopedValue` — directory layer internals
- `StringInterningLayer`, `HighContentionAllocator` — optimization internals

This is exactly the problem Go's `internal/` solves structurally. Java needs an annotation because it lacks `internal/`.

### What this tells us

1. **Java does NOT keep everything flat.** Index maintainers are in their own `indexes/` package. Cursors are split between generic (`cursors/`) and FDB-specific (`provider/foundationdb/cursors/`). Key expressions are in `metadata/expressions/`.

2. **But Java's packages are all public.** `StandardIndexMaintainer` is `public` and `@UNSTABLE`. Anyone can import it. They rely on the annotation to signal "don't depend on this" — there's no compiler enforcement. Sound familiar? That's exactly what Option B does, but with Go's unexport instead of Java's annotation.

3. **The `@INTERNAL` annotation exists because Java can't unexport.** In Java, if package A needs to call package B's class, that class must be `public`. Go doesn't have this problem — unexported types are accessible within the same package. This is why Go's single-package approach + unexport works better than Java's multi-package + annotation approach for our case.

4. **Java's package split is driven by the query planner**, not the record store. The `query/` subtree is 200+ classes and genuinely separate. We don't have a query planner and probably never will. Stripping that out, Java's core record store code (`provider/foundationdb/` minus query) is ~80 classes in one package — similar to our 70 files.

5. **Java keeps index maintainers public** (`@UNSTABLE`) because they're in a separate `indexes/` package and the store needs to instantiate them. If they were in the same package as `FDBRecordStore`, they could be package-private. In other words: Java's package split *forced* them to export maintainers. We should learn from this mistake, not repeat it.

## Options considered

### Option A: `internal/` subpackage (recommended)

```
pkg/recordlayer/
  store.go, database.go, metadata.go, ...     # exported API (~40 files)
  internal/
    maintainer/                                # index maintainers (~15 files)
      standard.go, count.go, sum.go, rank.go, version.go, ...
    split/                                     # split record logic
      split_helper.go, size_info.go
    rangeset/                                  # range set implementation
      range_set.go
    rankedset/                                 # ranked set (skip-list)
      ranked_set.go
    subspace/                                  # subspace constants + key building
      constants.go
```

**Pros:**
- Go's `internal/` enforces the boundary at the compiler level. No discipline needed.
- `go doc` on the top-level package shows only user-facing types.
- Clear signal to contributors: if it's in `internal/`, don't depend on it from outside.
- Can still refactor internals freely without semver concerns.

**Cons:**
- Significant churn: 30+ files move, all imports update, every test file touches.
- `internal/` subpackages need their own `BUILD.bazel` files (gazelle handles this).
- Cross-package boundaries add friction for internal code that currently just calls private functions.
- Types that are currently in the same package and share private fields would need to expose them (lowercase → uppercase within internal packages, or accessor methods).
- **The real cost**: index maintainers currently access `store` internals directly (subspaces, transaction, context). Moving them to a separate package means either:
  - Passing a fat interface/struct with all the store internals they need, or
  - Keeping them in the same package and just unexporting them (see Option B).

### Option B: Unexport in place (keep flat package, just lowercase the internals)

```
pkg/recordlayer/
  store.go, database.go, metadata.go, ...     # same files, same package
  index_maintainer.go                          # IndexMaintainer → indexMaintainer
  standard_index_maintainer.go                 # StandardIndexMaintainer → standardIndexMaintainer
  count_index_maintainer.go                    # CountIndexMaintainer → countIndexMaintainer
  ...
  range_set.go                                 # RangeSet → rangeSet
  ranked_set.go                                # RankedSet → rankedSet
  split_helper.go                              # SizeInfo → sizeInfo
```

**Pros:**
- Minimal churn — just rename exported types to unexported. No file moves, no import changes.
- No cross-package boundary problems. Maintainers keep direct access to store internals.
- `go doc` immediately improves (unexported types don't show).
- Tests stay in the same package (`_test.go` files still have access to unexported types).
- Chaos tests already construct maintainers indirectly (via store), so nothing breaks.

**Cons:**
- No compiler enforcement. Someone could re-export an internal type by accident.
- Still 70 files in one package. Doesn't address the "big flat package" concern.
- Contributors still need to know the convention (though unexported = obvious signal).

### Option C: Do nothing

**Pros:**
- Zero effort. Ship features instead.
- We have zero external users. The API surface doesn't matter yet.
- Java Record Layer itself is one giant package (`com.apple.foundationdb.record.provider.foundationdb`) with 100+ public classes. We're following the same pattern.

**Cons:**
- Every day we wait, more code accumulates and the eventual split gets harder.
- But also: every day we wait, we learn more about what the API should actually look like.

## Recommendation

**Option B (unexport in place), with a future path to Option A.**

Rationale:

1. **The index maintainer problem kills Option A today.** Maintainers like `StandardIndexMaintainer` directly access 10+ store internals: `store.indexSubspace()`, `store.indexSecondarySubspace()`, `store.context`, `store.metadata`, `store.recordStoreState`, etc. Extracting these into a clean interface is a real design project, not a mechanical refactor. And getting the interface wrong means redoing it. We're not ready for that yet.

2. **Java's experience validates this.** Java split maintainers into their own `indexes/` package, which *forced* them to be public. They then had to invent `@API(INTERNAL)` to say "public but please don't use this." That's a code smell — the package boundary created a visibility problem that didn't need to exist. Go's single-package + unexport avoids this entirely.

3. **Option B gives us 80% of the value at 5% of the cost.** The `go doc` cleanup, the contributor signal, the freedom-to-refactor — all of that comes from unexporting. The compiler enforcement of `internal/` is nice but not critical when we have zero external users.

4. **We're still actively adding index types and features.** The API is not stable. Doing a big structural refactor now means doing it again when we realize the boundaries were wrong. Unexporting is trivially reversible; package moves are not.

5. **The path to Option A is clear.** Once the API stabilizes and we want to publish a v1, we can do the `internal/` split then. By that point we'll know exactly what the maintainer interface should look like, because we'll have built all the maintainer types. Option B doesn't close any doors.

## Concrete changes (if we proceed with Option B)

### Phase 1: Unexport concrete index maintainers

These 11 types become unexported. Zero external usage confirmed.

| Current | New |
|---------|-----|
| `StandardIndexMaintainer` | `standardIndexMaintainer` |
| `CountIndexMaintainer` | `countIndexMaintainer` |
| `SumIndexMaintainer` | `sumIndexMaintainer` |
| `MinMaxEverIndexMaintainer` | `minMaxEverIndexMaintainer` |
| `MinMaxEverTupleIndexMaintainer` | `minMaxEverTupleIndexMaintainer` |
| `RankIndexMaintainer` | `rankIndexMaintainer` |
| `VersionIndexMaintainer` | `versionIndexMaintainer` |
| `MaxEverVersionIndexMaintainer` | `maxEverVersionIndexMaintainer` |
| `CountNotNullIndexMaintainer` | `countNotNullIndexMaintainer` |
| `CountUpdatesIndexMaintainer` | `countUpdatesIndexMaintainer` |
| `PermutedMinMaxIndexMaintainer` | already unexported |

Also unexport `IndexMaintainer` interface → `indexMaintainer`.

### Phase 2: Unexport implementation details

| Current | New | Reason |
|---------|-----|--------|
| `RangeSet` | `rangeSet` | Only used by `IndexingRangeSet` |
| `RankedSet`, `RankedSetConfig` | `rankedSet`, `rankedSetConfig` | RANK impl detail |
| `SizeInfo` | `sizeInfo` | Only constructed internally |
| `FDBRecordStoreState` | `fdbRecordStoreState` | Internal cache state |
| Format version constants (8 of them) | unexport | Impl detail |
| Subspace key constants (10 of them) | **keep exported** | Useful for debugging; low harm |

### Phase 3: Review key expression internals

Some key expression types might have exported fields that should be methods. Low priority — review after Phase 1-2.

### What NOT to change

- All error types stay exported (users match with `errors.As`).
- All constructor functions (`NewIndex`, `Field`, `Concat`, etc.) stay exported.
- `RecordCursor[T]` and all combinator functions stay exported.
- `IndexEntry`, `TupleRange`, `ScanProperties` stay exported.
- `OnlineIndexer`, `IndexingRangeSet` stay exported.
- Subspace constants stay exported (useful for debugging, low harm).

## Effort estimate

Phase 1: ~1 hour mechanical rename (11 types + their methods + test references).
Phase 2: ~1 hour more.
Total: half a day including test verification.

## Open questions

1. **Should `IndexMaintainer` interface stay exported?** It's the extension point for custom index types. But no one has asked for that, and we can always re-export it later.

2. **Should subspace constants be exported?** They're useful for people debugging raw FDB keys. But they also imply we support manual key construction, which we don't.

3. **Timing.** Do this now while we remember what's internal, or wait until we're closer to a v1 tag? The longer we wait, the more types accumulate, but also the more stable the API becomes.
