# RFC-279: Mutual indexing fragments, limits and the FULL phase; lessen-work backoff

**Status:** Implemented
**Item:** TODO.md "Quality review 2026-10-24", Record layer (PR #994, unmerged). This RFC
covers four Medium items:
- mutual-indexing fragment edges are raw shard keys;
- the mutual build loop never checks the transaction limits;
- the mutual FULL phase builds one chunk per fragment;
- lessen-work retries do not back off.

**Spec:** Java Record Layer 4.14.2.0 (`IndexingMutuallyByRecords`, `IndexingThrottle`,
`FDBRecordStore.getPrimaryKeyBoundaries`).

## Problem and Java behavior

Go line numbers are at the base commit, `da73f1db1`.

1. **Fragment edges.** Go's `computeFragments` (`indexing_mutual.go:105-120`) asks
   `LocalityGetBoundaryKeys` for the whole records subspace, outside any transaction and at a
   fresh read version. It keeps each raw key minus the records prefix as a fragment edge.
   Java's `getPrimaryKeyBoundaries` (`IndexingMutuallyByRecords.java:156-180`,
   `FDBRecordStore.java:5438-5462`) does this instead:
   - it covers only `computeRecordsRange()`, the indexed record types' records;
   - it reads the shard map at the first build transaction's read version;
   - for each boundary it reads the first key at or after it (a snapshot `getRange` with
     limit 1), drops the split suffix when the store has one
     (`isSplitLongRecords() || !omitUnsplitRecordSuffix`), and skips repeats.

   So every Java edge is a primary key. A shard boundary is usually an existing key, which in
   the default layout is `(pk, n)`. Go then starts a fragment inside a record:
   - the record's later chunks fail the scan with "Found split record without start";
   - `(pk, 0)` leaves the `(pk, -1)` version key in the previous fragment;
   - a boundary that is not a tuple fails the `Unpack` at `:368` or `:377`.
2. **Transaction limits.** `buildFragmentRange` (`:359-425`) stops only at the record limit.
   Java's `buildThisRangeOnly` runs `iterateRangeOnly`, which checks
   `hadTransactionReachedLimits` after each indexed record (`IndexingBase.java:866`). Go's
   `buildRange` already does (`online_indexer.go:2106`).
3. **FULL phase.** Java's `buildNextRangeOnly` picks a range: in FULL a fragment with nothing
   built, in ANY the first missing part of one (`:309-363`, `fullyUnBuiltRange`,
   `partlyUnBuiltRange`). It then builds that range chunk by chunk until nothing in it is
   missing (`iterateAllRanges(buildThisRangeOnly)`). Only then does it list again and walk
   on. Go's `buildMutual` builds one chunk and returns (`:283-294`). On the next call, FULL
   sees its own fragment as partly built and skips it (`:344-350`). So FULL builds one chunk
   per fragment and leaves the rest to ANY. Every peer then works in ANY over the same
   fragments. The partitioning is lost, and the peers conflict with 1020.
4. **Backoff.** `buildCommitRetryAsync` (`IndexingThrottle.java:345,391-398`) creates the
   runner's `ExponentialDelay` (10 ms initial, 1 s max) per call. After
   `mayRetryAfterHandlingException` lessens the limit, it waits `delay.delay()`, timed as
   `RETRY_DELAY`. Go's `buildRangeWithRetries` (`online_indexer.go:1605-1631`) retries at
   once. The runner's own backoff covers only the codes the runner retries. A 2101 reaches
   lessen-work with no delay at all.

## Fix

1. **`FDBRecordStore.GetPrimaryKeyBoundaries(TupleRange)`** is Java's method. It reads the
   shard map at the transaction's read version and looks up the first key after each
   boundary with snapshot reads, at most `DefaultPipelineSize` in flight. It unpacks each key
   with the records subspace, drops the last element when the store has a split suffix, and
   skips a repeat of the previous key. The mutual builder uses an internal form over raw
   bytes, the records prefix plus `computeRecordsRange`, else the whole records range.
   - Detected boundaries are read in the first claim transaction (item 3), as Java reads them
     in its first build transaction. A `BackendCapabilityError` (a tenant-backed or custom
     transactor) gives one fragment; any other error fails the transaction.
   - Preset boundaries (`SetMutualIndexingBoundaries`) are still set when the builder is made.
2. **Limits.** `buildFragmentRange` takes the transaction's start time and checks
   `transactionReachedLimits` after each indexed record, as `buildRangeInTransaction` does.
   The record after the limit ends the range. The function returns whether it stopped early.
3. **FULL phase.** The builder keeps a claimed range across calls. `buildMutual`:
   - with no claim, runs `claimNextRange`, a read-only transaction like Java's
     `buildRangeOnly`. It checks the index states, reads the boundaries if needed and lists
     the missing ranges. It reports done when nothing is missing. Otherwise `nextClaim` walks
     the fragments from the current one with ports of `fullyUnBuiltRange` and
     `partlyUnBuiltRange`.
     - A walk back to the first fragment moves FULL to ANY.
     - A walk back in ANY is Java's third-iteration failure, an `IndexingValidationError`
       "Mutual indexing failure - third iteration". A walk finds work whenever a range is
       missing, so Go reaches this only through a defect.
   - builds the first missing chunk of the claim in its own transaction
     (`buildClaimedRange`, Java's `buildThisRangeOnly`). The transaction validates the build
     session and renews the heartbeat as before. When the claim has nothing missing, the
     builder drops it and claims again. A chunk that reaches the claim's end drops it at once.

   The claim and the walk are kept only when the claim transaction commits. A failed chunk
   keeps the claim for the retry.
4. **Backoff.** `buildRangeWithRetries` makes an `exponentialDelay` from the database's
   initial and maximum delays at its first retry. Before each retry it waits through
   `waitRetryDelay`, which is the wait taken out of `attemptLoop`. Under a simulated
   environment the delay is recorded, not slept. A context that ends during the wait returns
   the context error wrapping the last attempt's error. This covers every build method, as
   Java's `buildCommitRetryAsync` does.

## Not ported

- **anyJumper** (`IndexingMutuallyByRecords.java:495-544`). Java records an FDB error that
  ends a claimed range, after the runner's retries, and returns quietly, skipping
  lessen-work. On the next walk, if the fragment and range are unchanged, it throws the error
  again. It never clears the recorded error after that throw, so every lessen-work retry
  throws it again and the build fails. A claimed range that is too large for one transaction
  (2101) therefore fails the mutual build in Java, with no smaller retry. Go keeps the runner
  retries and lessen-work on the claimed range. Peers that collide inside one fragment make
  progress by the runner's retries in both engines; Java's jump happens only after a range
  fails every runner attempt. DIVERGENCES.md records this.
- Java's `infiniteLoopProtection`, which fails after the same range is picked 1000 times in a
  row. A Go claim is built until nothing in it is missing, and a chunk that keeps failing ends
  the build after the lessen-work retries. DIVERGENCES.md records it too.
- A cross-engine check of the boundaries. It needs real shard splits: a multi-process cluster
  and data large enough to split, which is slow and timing-dependent. The boundaries are
  tested with an injected shard map instead.

## Compatibility

No stored format changes. The built ranges a mutual build writes now end at primary keys,
as Java's do; the range set's encoding is unchanged. `newMutualIndexBuilder` no longer
returns an error; it is unexported.

## Performance

A claim costs one read-only transaction per fragment, where Go used to list the missing
ranges of every fragment it walked inside each chunk's transaction. The chunk transaction
now reads only the claimed range of the range set, so a peer's progress elsewhere does not
conflict it. Boundary detection adds one snapshot read per shard boundary in the records
range, pipelined, once per build.

## Test plan

Each regression fails before its fix (mutations recorded on the PR).

- **Boundaries** (simulated database with an injected shard map):
  - `GetPrimaryKeyBoundaries` on a store with a split record maps a boundary before the
    first record, a non-tuple boundary, two boundaries inside the split record and one past
    the last record to `[(10), (50)]`, reading the shard map at the transaction's version. A
    format-4 store, without the suffix, keeps the whole key. Fifteen boundaries, more than
    the lookups in flight, with a repeat across the window's edge, give 14 sorted keys.
  - A mutual build with a shard boundary at `(50, 2)` inside a split record gets the edges
    `[\x00, (50), \xff]` and indexes every record.
  - With a record-type key prefix, the shard map is read over the indexed type's records
    only.
  - A `BackendCapabilityError` gives one fragment and a complete build; any other shard-map
    error fails the claim and sets no fragments.
- **Limits:** a mutual chunk with a 1-byte write limit indexes one record; with the default
  write limit the rest of the claim is one chunk.
- **FULL:** one builder over two preset fragments, 100 records and limit 10 stays in FULL
  until the build is done, visits each fragment once, and takes 11 chunks of at most 10.
- **Peers:** a peer that finishes the claimed fragment moves the builder to the next one; a
  peer that builds the middle of a claim leaves the builder on the rest of it, in FULL, until
  the claim has nothing missing.
- **No anyJumper:** a claimed chunk that fails with 2101 is retried at a lower limit and
  succeeds.
- **Claim publication:** a failed claim transaction leaves no boundaries and no claim. A
  failed chunk keeps the claim. A successful last chunk drops it, and the next call reports
  done.
- **Walk:** Java's `testFullyUnBuiltRange` and `testPartlyUnBuiltRange` vectors. A walk with
  a missing range outside every fragment fails with the third-iteration error.
- **Backoff:** two 2101 lessen-work retries record two `EventRetryDelay`s under a simulated
  environment, totalling what the exponential delay draws from the same seed. Cancelling the
  context during a minute-long wait ends the build at once with both errors.
- The existing mutual specs (concurrent builders, user writes during the build, claim loss,
  heartbeat renewal) run unchanged in intent. The heartbeat spec claims first, so its barrier
  holds the chunk transaction.
