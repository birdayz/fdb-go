# Draft upstream report — not published

Tracked in `TODO.md`, “Upstream Java follow-up — deferred retirement after store deletion”.

Title: Deferred replacement retirement recreates index-state data after deleteStoreAsync

Affected reference: fdb-record-layer-core 4.14.2.0,
fdacd162a9c8acfadc49082b89185c823ab8ae4a.

Reproducer retained as probeDeleteWithPendingReplacementRetirement in
conformance/index_state_conformance.java, invoked by the retained Go conformance
spec “pins Java deletion with a pending replacement retirement callback”.

1. Define original VALUE(price) with replacedBy0=replacement and a replacement
   VALUE(price) index. Create/open the store in one context.
2. Mark replacement WRITE_ONLY, rebuild original, rebuild replacement.
3. Call FDBRecordStore.deleteStoreAsync(context, subspace).join().
4. Commit that context, then read the raw store range in a fresh transaction.

Observed: one row remains, the original's DISABLED (2) state key. The store header
is absent. Expected: no rows in the deleted store range.

Cause: addRemoveReplacedIndexesCommitCheckIfChanged registers a closure capturing
this store under removeReplacedIndexes_<subspace hex>. deleteStoreAsync clears
the store range without withdrawing that closure. At precommit the closure sees
readable replacements and markIndexDisabled writes a state key after the clear.

The Go boundary correction cancels the subspace's pending retirement check before
clearing; later registrations belong to recreated-store metadata. Canceled
registrations retain identity so an earlier precommit callback can delete the
store without a snapshotted later callback resurrecting it. Cancellation does
not stop a callback already executing; callers must not concurrently mutate a
context during commit. No Java library source was patched and no issue was filed.
