# WS-C deferred-maintenance contracts and mutual transaction fencing

This increment is not completed-milestone acceptance. Format-15 opening and
remaining lifecycle acceptance/reviews are still outstanding.

## Implementation

Ported the Java 4.14.2.0 IndexDeferredMaintenanceControl fields and feedback
semantics, and IndexingMerger's adaptive merge/time/repartition budgets,
second-chance precedence, abort classification, and outer failure allowance.
Controls are store-local; callbacks run outside their lock and register on the
actual transaction through the store-scoped callback family. Child-transaction
backends must register on every child transaction too. None exists in the HNSW
adapter, so these tests do not prove GuardiANN/Lucene callback consumption.

IndexMaintainer now has MergeIndex. Synchronous standard/HNSW maintainers report
no deferred work; sliding windows delegate; SPFresh keeps its separate lifecycle.
OnlineIndexer exposes explicit MergeIndexes and executes committed build/drain
requests before completion/throttling. Drain success notifications run only
after commit (including the final empty transaction); failed attempts publish
neither their continuation nor follow-up work.

Reading the remaining mutual path exposed staged index mutations being committed
after a failed follower range claim. Claims now fail the transaction with a typed
IndexRangeClaimLostError in ordinary, by-index, and mutual paths. Mutual phase and
fragment state publish only after commit. This is the accepted design's explicit
loss-of-range safety boundary, not Java anyJumper: the old Go branch misapplied
anyJumper to an uncommitted boolean claim failure instead of an aborted FDB error.

Mutual builds now share the outer session UUID, update all target heartbeats,
validate state/stamps, add snapshot record conflicts, process requested merges,
and use bounded independent all-target cleanup. Preset ranges validate session
state too. Single-target mutual stamps use MUTUAL_BY_RECORDS, as Java does.
Another worker's successful readability publication is handled before stamp
validation, since publication erases stamps. Partially published targets stop
only when every remaining write-only range is complete.

## Evidence

Logs in `/var/tmp/fdb-upgrade-recovery/ws-c/`:

- `merger-target.log`: a test initially mis-modeled precedence when both a renewed
  second chance and a repartition cap were present. Read Java's
  shouldGiveRepartitionSecondChance; retained its early-return precedence in the
  test rather than changing the implementation to the incorrect expectation.
- `merger-fuzz.log`: sandbox setup failed before execution because its writable
  cache directory did not exist. After creating it, `merger-fuzz-fixed.log`
  records 19,917,841 executions in 15 seconds, no failures.
- `merger-race.log`: 134 selected specs / 3514, plus feedback/control unit tests,
  green with actual race instrumentation. `merger-full.log`: 93/93 targets,
  42 executed / 51 cached, 762.444 seconds.
- `mutual-target.log`: three existing concurrent-builder tests exposed missing
  handling of a peer's completed publication. Fixed the completion/state boundary;
  their assertions were not weakened. `mutual-regression.log` is green.
- `range-claim-mutation.log`: compiled suppression of the claim guard fails both
  selected regression cases (ordinary and mutual). Source restored afterwards.
- `merger-mutual-race.log`: 142 selected specs / 3517, plus feedback/control/stamp
  unit tests, green under actual race instrumentation.
- `merger-mutual-full.log`: 93/93 targets, 29 executed / 64 cached, 615.150 seconds;
  ten source hashes unchanged after verification.
- `merge-followup-target.log`: subsequent committed-drain follow-up wiring green;
  the iterator regression now asserts success callbacks at attempts 1,3,4,5,
  never at failed attempt 2.

The new regressions use real FDB for callback commits, synchronous HNSW merger
invocation, both range-claim-loss paths, and commit-bound mutual phase state.
Pure feedback tests enumerate ten error/budget cases, timeout/permanent-error
classification, and success/second-chance/cap ordering, plus control defaults,
cumulative accounting, request deduplication, and UUID defensive copies.

No commit, push, PR-state change, CI publication, or implementation gate ACK.

Subsequent normal format-15 opening and lifecycle acceptance evidence:
`../ws-c-format15/README.md`. Earlier format-14 ceiling statements above describe
their measured increment, not the current implementation.
