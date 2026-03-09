# Go Style/Idiom Audit — `pkg/recordlayer/`

**Date**: 2026-03-09
**Scope**: All 29 non-test `.go` files in `pkg/recordlayer/` (~26,400 LOC)
**Focus**: Public API surface, Go idiom compliance, Java-ism detection
**Note**: This is a research-only audit. No source code was modified.

---

## Severity Guide

| Level | Meaning |
|-------|---------|
| **HIGH** | Violates Go best practices in ways that affect correctness, safety, or composability. Fix before v1. |
| **MEDIUM** | Non-idiomatic but functional. Creates friction for Go developers consuming the API. |
| **STYLE** | Cosmetic naming/convention issues. Low priority but worth aligning over time. |

---

## 1. Naming Conventions

### 1.1 `Get` Prefix on Simple Accessors (STYLE)

Go convention: simple field accessors drop the `Get` prefix. `GetX()` implies the operation is non-trivial (computation, I/O, etc.). The codebase has ~30 methods that use `Get` for trivial field reads.

**File**: `pkg/recordlayer/metadata.go`
```go
// Line 540
func (m *RecordMetaData) GetRecordType(name string) *RecordType { return m.recordTypes[name] }
// Line 556
func (m *RecordMetaData) GetRecordCountKey() KeyExpression { return m.recordCountKey }
// Line 571
func (rt *RecordType) GetRecordTypeIndex() int { return rt.RecordTypeIndex }
// Line 577
func (rt *RecordType) GetRecordTypeKey() interface{} { ... }
// Line 588
func (m *RecordMetaData) GetIndexesForRecordType(name string) []*Index { ... }
// Line 603
func (m *RecordMetaData) GetUniversalIndexes() []*Index { return m.universalIndexes }
// Line 614
func (m *RecordMetaData) GetIndex(name string) *Index { return m.indexes[name] }
// Line 619
func (m *RecordMetaData) GetAllIndexes() map[string]*Index { return m.indexes }
// Line 625
func (m *RecordMetaData) GetFormerIndexes() []*FormerIndex { return m.formerIndexes }
// Line 633
func (m *RecordMetaData) GetIndexesToBuildSince(version int) []*Index { ... }
```

**File**: `pkg/recordlayer/store.go`
```go
// Line 495
func (store *FDBRecordStore) GetMetaData() *RecordMetaData { return store.metaData }
// Line 501
func (store *FDBRecordStore) GetIndexMaintainer(index *Index) IndexMaintainer { ... }
// Line 1077
func (store *FDBRecordStore) GetFormatVersion() int32 { ... }
// Line 1086
func (store *FDBRecordStore) GetUserVersion() int32 { ... }
// Line 1106
func (store *FDBRecordStore) GetMetaDataVersion() int32 { ... }
// Line 1122
func (store *FDBRecordStore) GetRecordStoreState() *RecordStoreState { ... }
// Line 1715
func (ts *TypedFDBRecordStore[T]) GetRecordCount() (int64, error) { ... }
// Line 1720
func (ts *TypedFDBRecordStore[T]) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) { ... }
```

**File**: `pkg/recordlayer/record_version.go`
```go
// Line 83
func (v *FDBRecordVersion) GetLocalVersion() int { ... }
// Line 89
func (v *FDBRecordVersion) GetGlobalVersion() []byte { ... }
// Line 99
func (v *FDBRecordVersion) GetDBVersion() int64 { ... }
```

**File**: `pkg/recordlayer/cursor.go`
```go
// Line 115
func (r RecordCursorResult[T]) GetValue() T { ... }
// Line 123
func (r RecordCursorResult[T]) GetContinuation() RecordCursorContinuation { ... }
// Line 128
func (r RecordCursorResult[T]) GetNoNextReason() NoNextReason { ... }
```

**File**: `pkg/recordlayer/database.go`
```go
// Line 237
func (rc *FDBRecordContext) GetLocalVersion(versionKey []byte) (int, bool) { ... }
// Line 308
func (rc *FDBRecordContext) GetReadVersion() (int64, error) { ... }
// Line 336
func (rc *FDBRecordContext) GetConflictingKeys() []fdb.KeyRange { ... }
```

**File**: `pkg/recordlayer/scan_properties.go`
```go
// Line 217
func (s ScanProperties) GetExecuteProperties() ExecuteProperties { ... }
```

**File**: `pkg/recordlayer/key_expression.go`
```go
// Line 567
func (g *GroupingKeyExpression) GetWholeKey() KeyExpression { ... }
// Line 572
func (g *GroupingKeyExpression) GetGroupedCount() int { ... }
// Line 577
func (g *GroupingKeyExpression) GetGroupingCount() int { ... }
```

**File**: `pkg/recordlayer/record_count.go`
```go
// Line 95
func (store *FDBRecordStore) GetSnapshotRecordCount(countKey tuple.Tuple) (int64, error) { ... }
// Line 117
func (store *FDBRecordStore) GetRecordCount() (int64, error) { ... }
// Line 124
func (store *FDBRecordStore) GetSnapshotRecordCountForRecordType(recordTypeName string) (int64, error) { ... }
```

**Suggested alternative**: Drop the `Get` prefix. `MetaData()`, `RecordType(name)`, `Index(name)`, `Value()`, `Continuation()`, etc. Some names like `GetRecordCount()` are borderline acceptable since they perform I/O (FDB read), but pure field accessors should definitely drop it.

Note: `GetRecordCount()` and `GetSnapshotRecordCount()` perform FDB reads (atomic read of count key), so the `Get` prefix is arguably justified there. The pure field-read accessors are the real offenders.

---

### 1.2 `Is` Prefix on Boolean Methods — Mixed (STYLE)

Some `Is` prefixes are fine Go (`IsComplete`, `IsUnique`). But others stutter with the type:

**File**: `pkg/recordlayer/metadata.go`
```go
// Line 561 — "IsStoreRecordVersions" reads awkwardly. "StoresRecordVersions()" or just "RecordVersionsEnabled()" is cleaner.
func (m *RecordMetaData) IsStoreRecordVersions() bool { ... }
// Line 566 — Same: "SplitsLongRecords()" reads better.
func (m *RecordMetaData) IsSplitLongRecords() bool { ... }
```

**File**: `pkg/recordlayer/index_state.go`
```go
// Lines 90-106 — These are fine individually but create stutter:
// store.IsIndexReadable("foo")    — reads as "is index readable" which is fine
// store.IsIndexWriteOnly("foo")   — same, acceptable
```

No action needed on the `index_state.go` ones. The `metadata.go` ones read more like Java verb forms.

---

### 1.3 Package-Prefixed Enum Constants (STYLE)

The `RecordExistenceCheck` constants repeat the type name, creating painful stutter when used:

**File**: `pkg/recordlayer/existence_check.go`
```go
// Lines 15-44
RecordExistenceCheckNone
RecordExistenceCheckErrorIfExists
RecordExistenceCheckErrorIfNotExists
RecordExistenceCheckErrorIfTypeChanged
RecordExistenceCheckErrorIfNotExistsOrTypeChanged  // 50 characters!
```

**Suggested alternative**:
```go
type ExistenceCheck int
const (
    ExistenceNone ExistenceCheck = iota
    ExistenceErrorIfExists
    ExistenceErrorIfNotExists
    ExistenceErrorIfTypeChanged
    ExistenceErrorIfNotExistsOrTypeChanged
)
```
Or, since this is inside `package recordlayer`:
```go
const (
    CheckNone ExistenceCheck = iota
    CheckErrorIfExists
    // ...
)
```

---

### 1.4 `RecordMetaDataBuilder` / `StoreBuilder` — Verbose Names (STYLE)

**File**: `pkg/recordlayer/metadata.go`, line 116
```go
type RecordMetaDataBuilder struct { ... }
```

**File**: `pkg/recordlayer/store.go`, line 1318
```go
type StoreBuilder struct { ... }
```

The `RecordMetaDataBuilder` name is extremely Java. In Go, types inside a package don't need to repeat the package context. Since it's in `package recordlayer`, callers write `recordlayer.RecordMetaDataBuilder` -- `recordlayer.MetaDataBuilder` would suffice.

---

## 2. Error Handling

### 2.1 Panics in Library Code Instead of Returning Errors (HIGH)

Go library code must never panic. These should all return `(T, error)`:

**File**: `pkg/recordlayer/record_version.go`
```go
// Line 89-92 — GetGlobalVersion panics on incomplete version
func (v *FDBRecordVersion) GetGlobalVersion() []byte {
    if !v.complete {
        panic("cannot get global version of incomplete FDBRecordVersion")
    }

// Line 99-102 — GetDBVersion panics on incomplete version
func (v *FDBRecordVersion) GetDBVersion() int64 {
    if !v.complete {
        panic("cannot get DB version of incomplete FDBRecordVersion")
    }

// Line 217-221 — Next() panics at max local version
func (v *FDBRecordVersion) Next() *FDBRecordVersion {
    local := v.GetLocalVersion()
    if local >= 0xFFFF {
        panic("cannot get next version: already at max local version")
    }

// Line 230-234 — Prev() panics at min local version
func (v *FDBRecordVersion) Prev() *FDBRecordVersion {
    local := v.GetLocalVersion()
    if local <= 0 {
        panic("cannot get prev version: already at min local version")
    }

// Line 253-256 — ToVersionstamp() panics on incomplete version
func (v *FDBRecordVersion) ToVersionstamp() tuple.Versionstamp {
    if !v.complete {
        panic("cannot convert incomplete FDBRecordVersion to Versionstamp")
    }
```

That is **5 panics** in a single type. Any caller that forgets to check `IsComplete()` crashes the process.

**Suggested alternative**: Return `([]byte, error)`, `(int64, error)`, `(*FDBRecordVersion, error)`, `(tuple.Versionstamp, error)` respectively. The caller can choose to panic if they want -- the library should never make that decision.

---

### 2.2 GetValue() Panics on No-Value Result (HIGH)

**File**: `pkg/recordlayer/cursor.go`, line 115-119
```go
func (r RecordCursorResult[T]) GetValue() T {
    if !r.hasNext {
        panic("GetValue called on RecordCursorResult with no value (check HasNext() first)")
    }
    return *r.value
}
```

This is the most dangerous panic in the codebase because `RecordCursorResult` is the core return type used everywhere. The comment says "Matches Java's behavior of throwing IllegalResultValueAccessException" -- but Java exceptions are recoverable. Go panics are not (without `recover()`).

**Suggested alternative**: Return `(T, bool)` or `(T, error)`. Or use the existing `HasNext()` + value field pattern but return `(T, ok bool)` to make the API impossible to misuse.

---

### 2.3 `recover()` to Swallow FDB Panics (MEDIUM)

**File**: `pkg/recordlayer/key_value_cursor.go`, line 392-398
```go
// Defend against FDB RangeIterator.Get() panicking when the internal
// batch state is inconsistent (e.g., empty kvs slice after Advance returns true).
defer func() {
    if r := recover(); r != nil {
        ok = false
    }
}()
```

This silently swallows *all* panics -- including nil pointer dereferences, out-of-bounds indexing, etc. -- not just the specific FDB bug it's defending against. A bug anywhere in the call stack becomes invisible.

**Suggested alternative**: If FDB's `RangeIterator.Get()` can panic, file a bug upstream. In the meantime, at least log the recovered panic value, or check its type to only swallow the specific panic you're defending against.

---

### 2.4 Silent Error Swallowing (MEDIUM)

**File**: `pkg/recordlayer/record_count.go`, line 64-69
```go
subkeys, err := countKey.Evaluate(record)
if err != nil {
    // Silently skip counting on evaluation errors (matches Java behavior
    // where this is logged but doesn't fail the operation)
    return
}
```

The comment says "matches Java behavior where this is logged" -- but Go doesn't log it either. The error is completely swallowed. At minimum, this should use a logger or return the error and let the caller decide.

---

### 2.5 Sentinel Errors Defined with `fmt.Errorf` Instead of `errors.New` (STYLE)

**File**: `pkg/recordlayer/index_state.go`, lines 71-74
```go
var ErrIndexNotReadable = fmt.Errorf("index is not readable")
var ErrIndexNotFound = fmt.Errorf("index not found in metadata")
```

`fmt.Errorf` without `%w` is functionally identical to `errors.New` but signals "this might need formatting" to readers. Since these are plain sentinel errors, use `errors.New` for clarity.

---

### 2.6 `fmt.Errorf` with `%v` Instead of `%w` (MEDIUM)

Scattered instances where errors are formatted with `%v` (loses the error chain for `errors.Is`/`errors.As`) instead of `%w`.

Grep did not find remaining instances in the current codebase (most were fixed), but worth noting the pattern: always use `%w` when wrapping errors so callers can unwrap.

---

## 3. Interface Design

### 3.1 RecordCursor Interface Is Too Wide (HIGH)

**File**: `pkg/recordlayer/cursor.go`, lines 140-155
```go
type RecordCursor[T any] interface {
    OnNext(ctx context.Context) (RecordCursorResult[T], error)
    Close() error
    Seq(ctx context.Context) iter.Seq[T]
    Seq2(ctx context.Context) iter.Seq2[T, error]
    SeqWithContinuation(ctx context.Context) iter.Seq2[T, RecordCursorContinuation]
}
```

This is a 5-method interface. In Go, interfaces should be small (1-2 methods). The problem: `Seq`, `Seq2`, and `SeqWithContinuation` have **identical implementations** across every cursor type. There are at least 10 cursor implementations that all copy-paste the same 30+ lines of boilerplate for these three methods:

- `emptyCursor` (cursor.go:172-181)
- `errorCursor` (cursor.go:196-208)
- `listCursor` (cursor.go:256-302)
- `filterCursor` (cursor.go:444-490)
- `skipCursor` (cursor.go:578-600+)
- `limitCursor` (cursor.go -- similar)
- `keyValueCursor` (key_value_cursor.go)
- `indexCursor` (index_scan.go)
- `mergeCursor` (merge_cursor.go)
- `dedupCursor` (dedup_cursor.go)
- `chainedCursor` (chained_cursor.go)
- `fallbackCursor` (fallback_cursor.go)
- `concatCursor` (cursor.go)
- `flatMapCursor` (cursor.go)

That is roughly **400-500 lines of duplicated boilerplate** across the codebase.

**Suggested alternative**: The interface should only require `OnNext` and `Close`. The `Seq`/`Seq2`/`SeqWithContinuation` methods should be free functions or provided via a wrapper struct:

```go
type RecordCursor[T any] interface {
    OnNext(ctx context.Context) (RecordCursorResult[T], error)
    Close() error
}

// Free functions that work with any cursor
func Seq[T any](ctx context.Context, c RecordCursor[T]) iter.Seq[T] { ... }
func Seq2[T any](ctx context.Context, c RecordCursor[T]) iter.Seq2[T, error] { ... }
```

This eliminates ~500 lines of boilerplate and follows the Go pattern of small interfaces + free functions (like `io.Reader` + `io.ReadAll`).

---

### 3.2 `ToKeyExpression()` Pollutes the Core KeyExpression Interface (MEDIUM)

**File**: `pkg/recordlayer/key_expression.go` (via metadata.go, line 98-112)
```go
type KeyExpression interface {
    Evaluate(msg proto.Message) ([][]interface{}, error)
    FieldNames() []string
    ToKeyExpression() *gen.KeyExpression  // proto serialization concern
}
```

The `ToKeyExpression()` method is a serialization concern that has nothing to do with evaluating keys. It forces every `KeyExpression` implementation to know about protobuf serialization. This violates the single-responsibility principle.

**Suggested alternative**: Move proto serialization to a separate interface or a standalone function:
```go
type KeyExpressionSerializer interface {
    ToProto() *gen.KeyExpression
}
```
Or use a type switch in the serialization code.

---

## 4. API Design

### 4.1 Builder Pattern vs. Functional Options (MEDIUM)

The `StoreBuilder` and `RecordMetaDataBuilder` use Java-style builder patterns with `SetX()` chains:

**File**: `pkg/recordlayer/store.go`, lines 1316-1355
```go
type StoreBuilder struct { ... }
func NewStoreBuilder() *StoreBuilder { ... }
func (b *StoreBuilder) SetContext(ctx *FDBRecordContext) *StoreBuilder { ... }
func (b *StoreBuilder) SetMetaDataProvider(metaData *RecordMetaData) *StoreBuilder { ... }
func (b *StoreBuilder) SetSubspace(subspace subspace.Subspace) *StoreBuilder { ... }
func (b *StoreBuilder) SetIndexRebuildPolicy(policy IndexRebuildPolicy) *StoreBuilder { ... }
```

**File**: `pkg/recordlayer/runner.go`, lines 55-76
```go
func (r *FDBDatabaseRunner) SetMaxAttempts(n int) *FDBDatabaseRunner { ... }
func (r *FDBDatabaseRunner) SetInitialDelay(d time.Duration) *FDBDatabaseRunner { ... }
func (r *FDBDatabaseRunner) SetMaxDelay(d time.Duration) *FDBDatabaseRunner { ... }
func (r *FDBDatabaseRunner) SetContextConfig(config *RecordContextConfig) *FDBDatabaseRunner { ... }
```

**Counterpoint**: The `StoreBuilder` has a strong argument for staying as-is: it mirrors Java's API exactly, which is a project requirement. The builder also has meaningful lifecycle methods (`Create()`, `Open()`, `CreateOrOpen()`, `Build()`) that go beyond simple construction. The `RecordMetaDataBuilder` is similarly complex with validation.

The `FDBDatabaseRunner` setter chain is a weaker case -- it's just configuring retry parameters. Functional options would be more idiomatic:
```go
func NewFDBDatabaseRunner(db *FDBDatabase, opts ...RunnerOption) *FDBDatabaseRunner { ... }
```

**Verdict**: Keep the builders for `StoreBuilder` and `RecordMetaDataBuilder` (justified by lifecycle complexity and Java compatibility). Consider functional options for simpler types like `FDBDatabaseRunner`.

---

### 4.2 `NewRecordMetaData` Silently Discards Errors (MEDIUM)

**File**: `pkg/recordlayer/metadata.go`, lines 534-537
```go
func NewRecordMetaData(fd protoreflect.FileDescriptor) *RecordMetaData {
    md, _ := NewRecordMetaDataBuilder().SetRecords(fd).Build()
    return md
}
```

The `_ =` error discard is dangerous. If the schema is invalid (missing primary key), this returns `nil` with no indication of what went wrong. The caller gets a nil pointer dereference later.

**Suggested alternative**: Remove this function entirely and force callers to use the builder with proper error handling. Or at minimum, make it return `(*RecordMetaData, error)`.

---

### 4.3 `LoadRecord` Returns `nil, nil` for Not Found (MEDIUM)

**File**: `pkg/recordlayer/store.go`, lines 94-95
```go
if value == nil {
    return nil, nil // Record not found
}
```

Returning `(nil, nil)` is a Go anti-pattern. The caller must check `result == nil` separately from `err != nil`, and forgetting the nil check leads to nil pointer panics. This pattern is repeated in `LoadRecordVersion` (line 891-893).

**Counterpoint**: This is a common pattern in database libraries (e.g., `sql.ErrNoRows`). The project might benefit from a sentinel error like `ErrRecordNotFound`, but this is a well-established Go idiom for "optional" return values in database contexts.

**Verdict**: This is borderline. Document the `(nil, nil)` contract prominently or switch to a sentinel error.

---

## 5. Code Organization

### 5.1 `store.go` Is 1,736 Lines — Too Large (MEDIUM)

**File**: `pkg/recordlayer/store.go` — 1,736 lines

This single file contains:
- `FDBRecordStore` struct + CRUD methods (Load, Save, Delete, Scan)
- `StoreBuilder` + Create/Open/CreateOrOpen/Build lifecycle
- `TypedFDBRecordStore[T]` generic wrapper
- Version management (saveRecordVersion, packVersion, unpackVersion, LoadRecordVersion)
- Union descriptor wrapping/unwrapping
- Index rebuild logic (checkPossiblyRebuild, RebuildIndex)
- Record existence checking
- Index subspace management
- Error types (StoreIsLockedForRecordUpdatesError, RecordAlreadyExistsError, etc.)
- Store header read/write
- Delete all records

**Suggested split**:
- `store.go` — FDBRecordStore struct + CRUD (Load, Save, Delete)
- `store_builder.go` — StoreBuilder + Create/Open/CreateOrOpen lifecycle
- `store_typed.go` — TypedFDBRecordStore[T]
- `store_version.go` — Version management methods
- `store_scan.go` — Scan methods (already partially in other files)

---

### 5.2 `cursor.go` Is 1,514 Lines — Too Large (MEDIUM)

**File**: `pkg/recordlayer/cursor.go` — 1,514 lines

Contains the `RecordCursor` interface, `RecordCursorResult`, 6+ cursor implementations (`emptyCursor`, `errorCursor`, `listCursor`, `filterCursor`, `skipCursor`, `limitCursor`, `orElseCursor`, `concatCursor`, `flatMapCursor`), plus free functions (`ForEach`, `AsList`, `Filter`, `Map`, etc.).

**Suggested split**:
- `cursor.go` — Interface definitions, `RecordCursorResult`, `RecordCursorContinuation`
- `cursor_combinators.go` — `filterCursor`, `skipCursor`, `limitCursor`, `orElseCursor`
- `cursor_concat.go` — `concatCursor` (already complex with its own continuation type)
- `cursor_flatmap.go` — `flatMapCursor` (already complex)
- `cursor_util.go` — Free functions (`ForEach`, `AsList`, `Filter`, `Map`, `Reduce`, etc.)

---

## 6. Concurrency & Resource Management

### 6.1 `sync.Map` for Single-Transaction Maps (MEDIUM)

**File**: `pkg/recordlayer/database.go`, lines 183-184
```go
localVersionCache sync.Map     // key (string) -> local version (int)
versionMutations  sync.Map     // key (string) -> value ([]byte) for SET_VERSIONSTAMPED_VALUE
```

`FDBRecordContext` wraps a single FDB transaction. FDB transactions are single-threaded (you can't use them from multiple goroutines safely). So these maps will only ever be accessed from one goroutine. Using `sync.Map` here is:
1. Slower than a plain `map` (sync.Map has significant overhead)
2. Misleading (implies concurrent access is expected/safe)
3. Creates an awkward API for `HasVersionMutations()`:

**File**: `pkg/recordlayer/database.go`, lines 354-361
```go
func (rc *FDBRecordContext) HasVersionMutations() bool {
    has := false
    rc.versionMutations.Range(func(_, _ any) bool {
        has = true
        return false // stop after first
    })
    return has
}
```

Iterating an entire sync.Map just to check if it's non-empty, because `sync.Map` has no `Len()` method. With a plain `map`, this is just `return len(m) > 0`.

**Suggested alternative**: Replace with `map[string]int` / `map[string][]byte` and a simple counter or `len()` check. If there's a legitimate concurrent access pattern, document it. If not, don't pay for synchronization you don't need.

---

## 7. Type System

### 7.1 `interface{}` for Tuple Elements and Subspace Keys (STYLE)

**File**: `pkg/recordlayer/index.go`, line 30
```go
subspaceKey    interface{}
```

**File**: `pkg/recordlayer/metadata.go`, line 93
```go
explicitRecordTypeKey interface{}
```

**File**: `pkg/recordlayer/metadata.go`, line 58
```go
SubspaceKey    interface{}
```

Go 1.18+ has `any` as an alias for `interface{}`. While functionally identical, `any` is the idiomatic spelling in modern Go.

More importantly, these `interface{}`/`any` values could be constrained with a union type if the set of valid types is known (e.g., `string | int64`). This isn't possible with Go's type system for interface fields, but it's worth noting that the lack of type safety here means invalid values (e.g., a `[]byte` subspace key) would only be caught at runtime.

**Suggested alternative**: Use `any` instead of `interface{}` throughout. Consider a `SubspaceKey` type alias with documented valid types.

---

### 7.2 Return `[][]interface{}` for Key Evaluation (STYLE)

**File**: `pkg/recordlayer/key_expression.go`, line 104 (via metadata.go)
```go
Evaluate(msg proto.Message) ([][]interface{}, error)
```

The return type `[][]interface{}` is hard to reason about. A named type would help:

```go
type KeyTuple []any
type KeyTuples []KeyTuple

Evaluate(msg proto.Message) (KeyTuples, error)
```

This is a minor readability improvement but affects every key expression implementation.

---

## Summary

### Findings by Severity

| Severity | Count | Key Items |
|----------|-------|-----------|
| **HIGH** | 3 | Panics in FDBRecordVersion (5 sites), GetValue() panic, RecordCursor interface too wide (500 LOC boilerplate) |
| **MEDIUM** | 8 | sync.Map misuse, silent error swallowing, recover() swallowing all panics, NewRecordMetaData discards error, store.go/cursor.go too large, ToKeyExpression in core interface, LoadRecord nil/nil |
| **STYLE** | 5 | Get prefix on ~30 accessors, package-prefixed enum stuttering, interface{} vs any, verbose type names, [][]interface{} return |

### Top 3 Highest-Impact Fixes

1. **Slim down `RecordCursor` to 2 methods** (`OnNext` + `Close`) and make `Seq`/`Seq2`/`SeqWithContinuation` free functions. Eliminates ~500 lines of duplicated boilerplate across 10+ cursor types and makes implementing new cursors trivial.

2. **Replace panics with errors** in `FDBRecordVersion` and `RecordCursorResult.GetValue()`. These are ticking time bombs in library code -- any caller who forgets a precondition check crashes the process.

3. **Replace `sync.Map` with plain `map`** in `FDBRecordContext`. Removes unnecessary synchronization overhead and enables simple `len()` checks instead of the `Range()` hack in `HasVersionMutations()`.
