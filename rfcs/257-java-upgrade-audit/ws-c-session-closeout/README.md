# WS-C session fencing and readable-closeout increment

Reference: Java 4.14.2.0 `IndexingBase.iterateRangeOnly`,
`handleCursorResult`, `validateTypeStamp`, and `markIndexReadableForIndex`.
This is an implementation increment, not WS-C acceptance or a review ACK.

## Implemented

- Non-mutual BuildIndex owns a stable heartbeat across preparation, batches,
  drain retries, and final empty work. Independent bounded cleanup executes on
  exit for all target keys owned by this session, including ordinary builds.
- BY_RECORDS and BY_INDEX validate expected ordinary/queued state, stamp method,
  live blocks, and heartbeat ownership before each transaction's work, including
  an empty batch. Preparation admits the session in the state/stamp transaction.
- Idempotent snapshot batches explicitly conflict processed records. The
  BY_INDEX loader already adds serializable record reads; the explicit call
  now also follows Java's builder contract.
- Ongoing stamp validation matches Java: missing BY_RECORDS is legacy-compatible;
  method and live block decide rejection, not ancillary takeover fields. This
  corrects the preceding increment's overly strict drain stamp comparison.
- Readable closeout drains each queued target, attempts checked publication,
  and re-drains on pending writes / exhausted conflict retries within the policy
  budget. Explicit zero and one both permit one attempt, matching Java's
  `attemptsRemaining > 1` condition; two permits another drain and mark.
- Drain heartbeat checks use the store-scoped named callback family removed by
  DeleteStore, rather than an anonymous callback.

## Evidence

Full logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

- `session-target.log`: 101 selected Ginkgo specs / 3509 registered, green.
- `session-conflict-mutation.log`: compiled mutation substitutes an unrelated
  primary key for the BY_RECORDS conflict range. Of the two selected snapshot
  interleaving specs, BY_RECORDS fails and BY_INDEX passes (its record load is
  already serializable). Mutation text was observed before execution; source
  was restored before verification.
- `session-race.log`: 109 selected specs / 3509, actual race flag, green.
- `session-full.log`: 93/93 targets, 7 executed / 86 cached.
- `closeout-target.log`: 104 selected specs / 3512, including budgets 0/1/2.
- `closeout-full.log`: 93/93 targets, 6 executed / 87 cached, 333.964 seconds.
- Final `closeout-final-race.log`: 132 selected specs / 3512, plus
  `TestOnlineIndexerOngoingStampValidation` and its six subtests. Actual
  `--@rules_go//go/config:race`, uncached, all passed.
- Final `closeout-final-full.log`: 93/93 targets, 2 executed / 91 cached,
  116.237 seconds. Five source hashes checked unchanged afterwards.

The six new real-FDB batch-session specs cover snapshot deletion interleavings
for both strategies and empty-batch rejection after disablement, a permanent
block, a method change, or another live heartbeat. The three new closeout specs
commit a real competing enqueue after the mark transaction's body and before
commit; the backend supplies the conflict, not a fabricated error. Failure
budgets preserve queued state/data; the second-attempt case drains and publishes.

## Still-open milestone work

At this increment, deferred-maintenance control and IndexingMerger contracts
were not implemented; the subsequent port and mutual fencing fixes are recorded
in `../ws-c-merger-mutual/README.md`.
Their complete Java source (307 and 303 lines respectively at the pinned tag)
was read. HNSW's synchronous graph has no deferred work; this is not proof of
GuardiANN or Lucene backend callback consumption. Normal format-15 opening,
complete lifecycle/interoperability acceptance, and actual completed-milestone
review gates remain outstanding. The maximum format remains 14. No new review
ACK, CI-green claim, commit, push, or PR change is implied.

Subsequent normal format-15 opening and lifecycle acceptance evidence:
`../ws-c-format15/README.md`. Earlier format-14 ceiling statements above describe
their measured increment, not the current implementation.
