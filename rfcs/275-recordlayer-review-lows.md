# RFC-275: Record-layer review lows and explicit-version saves

**Status:** Implemented
**Item:** TODO.md "Quality review 2026-10-24", Record layer (added by #994). This RFC
covers the four Low items and the Medium explicit-version/`VersionstampSaveBehavior` item:
- the transaction size warning;
- keyspace siblings, `PathFromTuple` and the resolver context;
- the two `%v` error chains and the `buildRange` read conflicts;
- the rank and TEXT edges;
- explicit-version saves.

**Spec:** Java Record Layer 4.14.2.0.

## Problem and Java behavior

1. **Transaction size warning.** `CheckTransactionSize` (`database.go:913`) returns a
   `TransactionSizeWarningError` once the Go-only `TransactionSizeWarnBytes` threshold is
   passed. `SaveRecord` and `DeleteRecord` return it after the write has already happened
   (`store.go:644,1032`). A caller's ordinary `if err != nil` then aborts a transaction that
   succeeded. Java has no user size threshold at all. The only size check is the online
   indexer's write limit (`IndexingBase.hadTransactionReachedLimits`), and FDB's own 2101 is
   counted and wrapped.
2. **Keyspace.**
   - Siblings: `AddSubdirectory` rejects only duplicate names, and does so by panicking.
     Java (`KeySpaceDirectory.java:349-385`) throws `RecordCoreArgumentException`:
     - `"Subdirectory already exists"` for a duplicate name;
     - `"Cannot add directory due to overlapping type"` for a same-type sibling when either
       directory is not constant;
     - `"... overlapping constant value"` for an equal constant;
     - `"... incompatibility with existing subdirectory"` when either directory's
       `isCompatible` refuses the other. Only a `DirectoryLayerDirectory` overrides
       `isCompatible`, and it refuses any sibling that is not one too.
   - Constant-first matching: without these checks, a tree can be ambiguous. Go resolves it
     by a constant-first rule in `PathFromTuple`, its `resolveRemaining` and
     `FindChildForValue`, and the constant-first test pins that. Java matches children in
     declaration order (`findChildForKey`, `:309-338`). Its sibling rules guarantee that at
     most one child holds any element.
   - Unmatched elements: an element that no child of a non-leaf directory matches is
     returned as a remainder. Java throws `"No subdirectory available to hold provided type"`
     (`:325-330`).
   - Context: `ResolverDirectory` resolves with `context.Background()` (`resolver.go:111`),
     because `ResolverFunc`, `KeySpace.Path` and `Path.Add` take no context.
3. **Error chains.** `store_builder.go:1037` and `store_state_cache.go:305` format the
   header-parse error with `%v`. Java throws `RecordCoreStorageException("Error reading
   version", cause)` (`FDBRecordStore.java:2758-2763`).
4. **`buildRange` read conflicts.** When every target index is idempotent, the build scans at
   snapshot isolation and adds a read conflict for each record
   (`online_indexer.go:2084-2085`). It adds the conflict before the per-index record-type
   filter. Java filters first: `getRecordIfTypeMatch` keeps only the target indexes' record
   types, and `addRecordReadConflict` follows (`IndexingBase.java:843-854`). So a concurrent
   write to a record of a type no target indexes fails Go's build transaction with 1020.
5. **Rank and TEXT edges.**
   - `GetNth` keeps the last key after drilling past level 0 even without duplicate
     counting. Java returns null there (`RankedSet.java:576-581`).
   - `extractRankValue` turns any non-`int` rank bound into an unbounded endpoint. Java
     casts the bound to `Number` and takes `longValue()`; a non-number throws
     `ClassCastException` and a null stays unbounded (`RankedSetIndexHelper.java:204-216`).
   - The TEXT maintainer silently skips a text column that is not a string, or a position
     past the key. Java's `Tuple.getString` throws `ClassCastException`, and the tuple index
     throws `IndexOutOfBoundsException` (`TextIndexMaintainer.java:288-293`). Null and `""`
     are skipped in Java too. Validation does not catch the non-string case: for
     `Concat(Literal(5), Field("name"))` it checks the first field it finds, `name`, which is
     a string.
6. **Explicit-version saves.** Go saves a version only when the metadata stores versions,
   and never takes one from the caller (`store.go:682`, "Phase 2").
   `SaveRecordWithOptions` nevertheless documents itself as Java's four-argument
   `saveRecordAsync`. Java's `saveRecord(rec, existenceCheck, version, behavior)`
   (`FDBRecordStore.java:503`) chooses the version in `recordVersionForSave` (`:621-633`):
   - `NO_VERSION` with a non-null version throws `RecordCoreException`.
   - `IF_PRESENT` stores exactly the supplied version, or none.
   - `WITH_VERSION`, or metadata that stores versions, assigns an incomplete version when
     none is supplied.
   - Otherwise the supplied version is stored, complete or incomplete.
   - The version is chosen before the existing record is loaded.

   Java's old format never reads back a version when the metadata stores none
   (`loadRecordVersionAsync`, `:1104`), even if a save wrote one.

## Fix

1. On the warning, `CheckTransactionSize` logs at WARN (`slog`) and increments a new
   Go-only count, `CountTransactionSizeWarning`, once per transaction, then returns nil.
   `TransactionSizeWarningError` is removed. `TransactionSizeExceededError` stays: it is an
   opt-in stop signal.
2. **Keyspace.**
   - `AddSubdirectory` and `AddSubdirectories` return an `*ArgumentError` under Java's rules.
     The error carries the parent, the directory name, the type and the value.
     `AddSubdirectories` stops at the first refusal, and the duplicate-name panic goes away.
   - A NULL directory counts as constant, as it does in Java: its only value is null.
   - Byte constants are compared by content. Java compares them by identity, so Go refuses
     equal byte siblings that Java would accept. Floats compare as Java's `Double.equals`:
     NaN equals NaN, and 0.0 is not -0.0.
   - The incompatibility rule cannot arise in Go: a resolver directory is always open, so the
     overlapping-type rule already refuses any same-type sibling.
   - Matching: `PathFromTuple`, which absorbs `resolveRemaining`, and `FindChildForValue`
     share one declaration-order matcher, `childForKey`. With the sibling rules the order is
     not observable, because at most one child can match.
   - `PathFromTuple` returns the `ArgumentError` for an element no child of a non-leaf
     directory holds, including a root without subdirectories. A remainder is left only past
     a leaf below the root.
   - `FindChildForValue` takes a stored value, as Java's `findChildForValue` does. A resolver
     directory is now found by its `int64` and no longer by its string name.
   - The test that pinned constant-first matching built a tree that is now refused. It is
     replaced by tests of the sibling rules.
   - `ResolverFunc`, `KeySpace.Path` and `Path.Add` take a `context.Context`, and
     `ResolverDirectory` passes it to `Resolve`.
   - A path still stores the resolved (stored) value. Java's resolved paths also carry the
     logical name. This is recorded in DIVERGENCES.md and as a TODO item.
3. `RecordCoreStorageError` gains a `Cause`, with `Unwrap`. Both header-parse sites return
   `&RecordCoreStorageError{Message: "Error reading version", Cause: err}`.
4. `buildRange` first collects the target indexes that apply to the record. A record that no
   target applies to is counted as scanned and skipped, with no conflict, as in Java. The
   idempotent-only condition on the conflict is unchanged. The per-record step is
   `indexScannedRecord`, which the mutual build's fragment loop (`buildFragmentRange`)
   shares; that loop had the same order. The loop body moves into `buildRangeInTransaction`,
   so a test can run it inside its own transaction.
5. **Rank and TEXT.**
   - `GetNth` returns nil after level 0 unless duplicates are counted.
   - `extractRankValue` converts tuple numbers with Java's `longValue()`: every Go integer
     carrier, `uint64`/`*big.Int` wrapping to their low 64 bits, and floats through
     `javaDoubleToLong` (NaN gives 0, saturation, truncation). A null element stays
     unbounded. Any other value is a `*ClassCastError` naming the Java class.
     `javaNumberLongValue` is shared with the key-expression functions' `nullableLong`. The
     SCORE_FOR_RANK aggregate, `ScoreForRank` and the leaderboard's SCORE_FOR_TIME_WINDOW_RANK
     take their rank the same way (`javaRank`); a null rank answers null, as Java's
     `scoreForRank` does.
   - The TEXT maintainer returns `*ClassCastError` for a non-string text column and
     `*IndexOutOfBoundsError` for a position past the key. Null and `""` are still skipped.
6. **Explicit-version saves.**
   - Add `VersionstampSaveBehavior` (`Default`, `NoVersion`, `WithVersion`, `IfPresent`), whose
     `String` gives Java's constant names, and `recordVersionForSave`.
   - Native version columns in a primary key are refused by Java's `RecordTypeBuilder`
     (`setPrimaryKey`, lines 86-90), and now by Go's metadata setter, validation and proto
     loader. A function expression declares no version columns by default, even if its
     arguments include one; lists sum and split/dimensions delegate as Java does.
   - `saveTargetFor` chooses the version once after resolving the record type, before
     evaluating the primary key and starting the existing-record read. Pipelined saves carry
     this chosen version in their precomputed target and reuse it in `saveRecordInternal`.
     Its callers:
     - `SaveRecordWithVersion` (new, the four-argument Java mapping);
     - `OverrideLockSaveRecordWithVersion` (new);
     - the typed store's `SaveRecordWithVersion` (new; its `SaveRecordWithOptions` delegates
       to it with the default);
     - `SaveRecordWithOptions`, `OverrideLockSaveRecord` and `SaveRecordsPipelined`, all with
       the default and no version.

     `SaveRecordWithOptions`'s doc now names the two-argument Java overload, and the
     "Phase 2" note is gone.
   - `SaveRecordBatch` chooses its default version before primary-key evaluation and keeps
     it for the save. `DryRunSaveRecord` uses the same default version-aware target as a real
     save, but stages no record, count, index or version mutation. Java's dry run keeps the
     chosen version on the preview record and omits version storage in the legacy format.
   - The version is written by the existing `saveRecordVersion`, which already handles
     complete versions, incomplete ones and the legacy format. VERSION index maintenance
     reads the stored record's version, and a complete version gives a plain index key.
7. **Records.** CHANGELOG `[Unreleased]` lists the breaking changes (the removed warning
   error, the keyspace signatures, the new errors) and adds the explicit-version API.
   DIVERGENCES.md records the Go-only size thresholds and the key-space path model.

## Performance

None of these paths gains a read or a round trip. `buildRange` saves conflict ranges for
records it does not index.

## Test plan

Each regression fails before its fix. For the new API, the red evidence is a mutation back
to the old behavior.

- **Transaction size:** a save and a delete past the warning threshold each return no error
  and count the warning once per transaction. The exceeded error is unchanged.
- **Keyspace:**
  - every sibling rule through `AddSubdirectory`, including two NULL siblings and equal byte
    constants, plus the valid cases;
  - `AddSubdirectories` stops at a refusal;
  - `PathFromTuple` rejects an unmatched element under a non-leaf directory, at the root and
    under a root without subdirectories, and keeps a remainder past a leaf;
  - `FindChildForValue` matches by stored value, including a resolver directory's `int64`;
  - a cancelled context reaches the resolver.
- **Error chains:** a corrupt header is a `*RecordCoreStorageError` that unwraps to the
  parse error, both for a direct open and through the store-state cache.
- **`buildRange` and the mutual fragment build,** with an idempotent VALUE index: a
  concurrent write to a record of a type no target indexes does not conflict the build's
  commit, while a write to an indexed record still does. The scanned count includes the
  skipped record.
- **Long-arithmetic key functions:** `uint64`, `uint` and a `*big.Int` past 2^64 are taken
  with `longValue()`, both at the evaluator and through a registered application function
  nested under `add` in a real-FDB VALUE index. Reopened index keys pin the numeric results.
- **Rank and TEXT:**
  - A ranked set written with duplicate counting and read without returns nil past level 0.
  - `extractRankValue` gives `longValue()` for `int64`, `uint64` above `MaxInt64`,
    `*big.Int` beyond the int64 range, float32, a negative double, NaN and ±Inf. A nil bound
    stays unbounded, and a bool is a `ClassCastError`.
  - `BY_RANK` with a double bound truncates, and with a string bound fails with
    `ClassCastError`. SCORE_FOR_RANK and SCORE_FOR_TIME_WINDOW_RANK accept int32, double and
    uint64 ranks, refuse a string, and answer a null rank with null.
  - A TEXT index over `Concat(Literal(int64(5)), Field("name"))` fails the save with a
    `ClassCastError` naming `java.lang.Long`. Null and `""` text save with no entries, and a
    position past the key is an `IndexOutOfBoundsError`.
- **Explicit versions,** on real FDB:
  - every behavior with no version and with a complete version, on metadata that stores
    versions and metadata that does not, at the default and the legacy format, read back in
    the saving transaction and after commit. A legacy-format store whose metadata stores no
    versions reads none back, as Java's does, so there the written legacy version key is
    checked instead. The exception is `NO_VERSION` with a version, which is refused and has its
    own spec;
  - a supplied incomplete version, completed at commit;
  - `NO_VERSION` with a version, refused before the primary key and the existence check, with
    Java's message;
  - an unknown `VersionstampSaveBehavior`, an `IllegalArgumentError` that writes nothing;
  - a re-save with no version clears the stored one;
  - a VERSION index key with a complete version;
  - maintained count keys grouped by a complete version on insert and delete, and incomplete
    count keys refused with `IncompleteVersionstampError` rather than a tuple-encoder panic;
  - an ordering-function primary key observes the supplied complete version, including
    distinct versions for the same record id; incomplete values are refused before
    encoding by real and dry-run saves. Native `VersionKey()` primary keys are not supported;
    their metadata is refused instead. A field-only dry run returns its version without writing;
  - the override-lock save on a locked store, a supplied complete version through the typed
    save, and the default behavior of pipelined and batch saves.
- **Explicit versions, cross-engine** (`conformance/version_save_conformance_test.go`): a new
  Java step saves the same records with the same complete versions and behaviors, including
  a `NO_VERSION` refusal and a clearing re-save. It runs on metadata with and without
  versions and a VERSION index, at the default and the legacy format. The verdicts and the
  record, index, maintained-count and legacy-version key-value pairs must be equal, with
  field-only and ordering-wrapped-version primary keys. Separate
  metadata cases pin native-version primary-key refusals for direct, composite, list, split
  and dimensions expressions, and Java's acceptance of a function-wrapped version expression.
