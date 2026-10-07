# Upstream report drafts: fdb-record-layer 4.14.2.0

Defects the Go port found in the Java target while porting it. Go corrects each one and
declares it in DIVERGENCES.md. These are drafts to file against
`FoundationDB/fdb-record-layer`; none has been filed yet.

## 1. deleteStore leaves a store's queued replacement-retirement commit check, which recreates index state after the delete

**Component:** `FDBRecordStore.deleteStoreAsync`, `addRemoveReplacedIndexesCommitCheckIfChanged`.

**What happens.** An index-state change that may retire replaced indexes registers a named commit
check, `"removeReplacedIndexes_" + hex(subspace)` (`FDBRecordStore.java:2951-2952`), bound to
`this::removeReplacedIndexes`. `deleteStoreAsync(context, subspace)` (`:1886-1908`) clears
`subspace.range()` and marks the store state dirty, but it does not remove that commit check. At
commit the check runs over the deleted store and writes the original index's DISABLED state key
back into the cleared range.

**Reproducer.** In one context: open a store whose metadata has an index replaced by another
(`IndexOptions.REPLACED_BY_OPTION_PREFIX`), mark the replacement readable (so the check is
registered), call `FDBRecordStore.deleteStore(context, subspace)`, commit. Measured on 4.14.2.0
(Go conformance probe `probeDeleteWithPendingReplacementRetirement`, spec "pins Java deletion with
a pending replacement retirement callback", `conformance/store_lifecycle_conformance_test.go`):
after the commit the subspace holds exactly one row, the original index's state key with state 2
(DISABLED), and no store header.

**Expected.** A deleted store's subspace is empty after the commit.

**Suggested fix.** In `deleteStoreAsync`, remove commit checks registered for the subspace
(`context.removeCommitChecks(...)` with a predicate on the check name's subspace part, or keep the
store-scoped check names in a set keyed by subspace), before clearing the range.

**Go:** `DeleteStore` cancels the subspace's retirement check and its pending-write checks
(DIVERGENCES.md "DeleteStore cancels pending replacement retirement").

## 2. The pending-write-queue overflow check is keyed by index name alone

**Component:** `IndexingPendingWriteQueue.registerDisableOnOverflowCommitCheck`.

**What happens.** The disable-on-overflow commit check is registered as
`DISABLE_INDEX_COMMIT_HOOK + index.getName()` (`IndexingPendingWriteQueue.java:212-214`) through
`getOrCreateCommitCheck`, with the store captured in the lambda. When one transaction writes to two
stores whose queued indexes share a name and both queues overflow, the second registration returns
the first store's check. Only the first store's index is disabled, and the second keeps a full
queue that refuses every later write until it is drained.

**Expected.** Each overflowing store's index is disabled.

**Suggested fix.** Include the store's subspace in the check name, as
`addRemoveReplacedIndexesCommitCheckIfChanged` does
(`"..._" + ByteArrayUtil2.toHexString(subspace.pack())`).

**Go:** the check is keyed by store subspace and index name (DIVERGENCES.md "Pending-queue overflow
disables every overflowing store's index").
