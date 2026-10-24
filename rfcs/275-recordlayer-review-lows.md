# RFC-275: Record-layer review lows and explicit-version saves

**Status:** Draft
**Item:** TODO.md "Quality review 2026-10-24", Record layer (PR #994, unmerged). This RFC
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
   counted and wrapped. A warning must never reach the error result.
2. **Keyspace.**
   - Siblings: `AddSubdirectory` rejects only duplicate names. Java
     (`KeySpaceDirectory.java:349-385`) also throws `RecordCoreArgumentException` for a
     same-type sibling where either directory is open (`"Cannot add directory due to
     overlapping type"`), for an equal constant (`"... overlapping constant value"`), and for
     an incompatible directory class (`"... incompatibility with existing subdirectory"`).
     Only two `DirectoryLayerDirectory`s are compatible with each other.
   - Ambiguous trees: without these checks, `PathFromTuple` resolves an ambiguous tree by
     its own constant-first rule.
   - Unmatched elements: `PathFromTuple` returns an element no child of a non-leaf
     directory matches as a remainder. Java throws `"No subdirectory available to hold
     provided type"` (`:325-330`).
   - Context: `ResolverDirectory` resolves with `context.Background()` (`resolver.go:111`),
     because `ResolverFunc`, `KeySpace.Path` and `Path.Add` take no context.
3. **Error chains.** `store_builder.go:1037` and `store_state_cache.go:305` format the
   header-parse error with `%v`. Java throws `RecordCoreStorageException("Error reading
   version", cause)` (`FDBRecordStore.java:2758-2763`).
4. **`buildRange` read conflicts.** `online_indexer.go:2083` adds a record read conflict for
   every scanned record and only then filters by index. Java filters first:
   `getRecordIfTypeMatch` keeps only the target indexes' record types, and only then calls
   `addRecordReadConflict` (`IndexingBase.java:843-854`). The result is that a concurrent
   write to a record of a type no target indexes fails Go's build transaction with 1020.
5. **Rank and TEXT edges.**
   - `GetNth` keeps the last key after drilling past level 0 even without duplicate
     counting. Java returns null there (`RankedSet.java:576-581`).
   - `extractRankValue` turns any non-`int` rank bound into an unbounded endpoint. Java
     casts to `Number`: a long, BigInteger or floating value is taken with `longValue()`,
     and a non-number throws `ClassCastException`
     (`RankedSetIndexHelper.java:204-216`).
   - The TEXT maintainer silently skips a text column that is not a string, or a position
     past the key. Java's `Tuple.getString` throws `ClassCastException`, and the tuple
     index throws `IndexOutOfBoundsException` (`TextIndexMaintainer.java:288-293`).
     Validation does not catch this: for `Concat(Literal(5), Field("name"))` it checks the
     first field it finds, `name`, which is a string.
6. **Explicit-version saves.** Go saves a version only when the metadata stores versions,
   and never takes one from the caller (`store.go:681`, "Phase 2"). Java's
   `saveRecord(rec, existenceCheck, version, behavior)` (`FDBRecordStore.java:503,621-633`)
   works as follows:
   - `NO_VERSION` with a non-null version throws `RecordCoreException`.
   - `IF_PRESENT` stores exactly the supplied version, or none.
   - `WITH_VERSION`, or metadata that stores versions, assigns an incomplete version when
     none is supplied.
   - Otherwise the supplied version is stored, complete or incomplete.
   - The version is chosen before the existing record is loaded.

## Fix

1. On the warning, `CheckTransactionSize` logs at WARN (`slog`) and increments a new
   Go-only count, `CountTransactionSizeWarning`, once per transaction, then returns nil.
   `TransactionSizeWarningError` is removed. `TransactionSizeExceededError` stays: it is an
   opt-in stop signal. Both thresholds are documented as Go-only extensions.
2. **Keyspace.**
   - `AddSubdirectory` and `AddSubdirectories` return an error and apply Java's four sibling
     checks. A typed `*KeySpaceDirectoryError` carries the parent, name, type and value, and
     a resolver directory is compatible only with another resolver directory. This also
     replaces the duplicate-name panic.
   - `PathFromTuple` walks the children in declaration order, as Java does. It returns
     `"No subdirectory available to hold provided type"` for an element that no child of a
     non-leaf directory matches. A remainder is left only past a leaf.
   - `ResolverFunc`, `KeySpace.Path` and `Path.Add` take a `context.Context`, and
     `ResolverDirectory` passes it to `Resolve`.
   - A path still stores the resolved (stored) value. Java's resolved paths also carry the
     logical name, which Go does not model; reverse lookup stays out of scope.
3. `RecordCoreStorageError` gains a `Cause`, with `Unwrap`. Both header-parse sites return
   `&RecordCoreStorageError{Message: "Error reading version", Cause: err}`.
4. `buildRange` first collects the target indexes that apply to the record. A record that no
   target applies to is counted as scanned and skipped, with no conflict, as in Java. The
   loop body moves into `buildRangeInTransaction`, so a test can run it inside its own
   transaction.
5. **Rank and TEXT.**
   - `GetNth` returns nil after level 0 unless duplicates are counted.
   - `extractRankValue` converts every tuple number with Java's `longValue()` (two's-complement
     wrap for `uint64`/`*big.Int`, `javaDoubleToLong` for floats). A null element stays
     unbounded, and any other value is a `*ClassCastError`.
   - The TEXT maintainer returns `*ClassCastError` for a non-string text column and
     `*IndexOutOfBoundsError` for a missing one. Null and `""` are still skipped, as in Java.
6. **Explicit-version saves.**
   - Add `VersionstampSaveBehavior` (`Default`, `NoVersion`, `WithVersion`, `IfPresent`), and
     `SaveRecordWithVersion(rec, existenceCheck, version, behavior)` plus an override-lock
     twin.
   - `saveRecordInternal` takes the version and behavior and chooses the version first, as
     `recordVersionForSave` does. It writes the version through the existing
     `saveRecordVersion`, which already handles complete versions, incomplete ones and the
     legacy format.
   - VERSION index maintenance already reads the stored record's version, and a complete
     version gives a plain index key.
   - The batch and dry-run saves keep the default behavior, as Java's do.

## Performance

None of these paths gains a read or a round trip. `buildRange` saves conflict ranges for
records it does not index.

## Test plan

Each regression fails before its fix.

- **Transaction size:** a `SaveRecord` past the warning threshold returns no error. The count
  is incremented once per transaction, and the exceeded error is unchanged.
- **Keyspace:** each sibling rule and the valid cases; `PathFromTuple` rejects an unmatched
  element under a non-leaf directory; a cancelled context reaches the resolver.
- **Error chains:** a corrupt header is a `*RecordCoreStorageError` that unwraps to the
  parse error, for both the builder path and the cached path.
- **`buildRange`:** inside the build transaction, a concurrent write to a record of a type
  no target indexes no longer conflicts the build's commit. A record of the indexed type
  still conflicts.
- **Rank and TEXT:** a ranked set written with duplicate counting and read without returns
  nil past level 0. `BY_RANK` with a string bound fails; a double bound is truncated. A TEXT
  index over `Concat(Literal(int64(5)), Field("name"))` fails the save.
- **Explicit versions** (real FDB):
  - every behavior with no version, a complete version and an incomplete one, on metadata
    that stores versions and metadata that does not;
  - `NO_VERSION` with a version is refused before the existence check;
  - a VERSION index key with a complete version;
  - the legacy format.
- **Explicit versions, cross-engine:** a Java step saves the same records with the same
  versions and behaviors. The verdicts and the bytes of the record, index and legacy-version
  spaces must be equal. Complete versions make the bytes comparable; for assigned versions,
  only the keys are compared.
