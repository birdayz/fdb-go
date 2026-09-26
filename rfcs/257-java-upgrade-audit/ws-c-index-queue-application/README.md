# WS-C index queue per-entry application

Tracking: TODO.md, “WS-C index queue per-entry application”. Accepted design:
../ws-c-design.md. This is an internal replay primitive, not the completed drain
runner, queued writer dispatch, or WS-C implementation acceptance.

Ported IndexingPendingWriteQueue.handleOneItem from Java 4.14.2.0: UPDATE delegates
to UpdateFromQueue; DELETE_WHERE unpacks its typed Any and uses the serialized
per-index prefix; successful application clears the entry within the transaction.
No successful clear occurs on failed application. The caller must still own commit,
retry, state validation, heartbeat and continuation publication. The factory uses
Java subspaces (9,indexKey,8) and (9,indexKey,9), asserted with independent literals.
No format or queued-state enablement is introduced by these helpers.

Three retained real-FDB specs in index_maintainer_queue_test.go:
- Commit two partitioned vector insert entries followed by DELETE_WHERE for one
  partition; replay in versionstamp order, check membership after each operation,
  retain the other partition, and assert persisted queue emptiness/counter zero.
- Failed UPDATE leaves its committed entry/counter intact.
- Failed DELETE_WHERE leaves its committed entry/counter intact.
Both failure cases deliberately COMMIT after observing the error and repeat in a
new transaction, distinguishing real retention from rollback-only retention.

Evidence in /var/tmp/fdb-upgrade-recovery/ws-c/:
- index-queue-application.log: 3 / 3468 selected specs pass.
- index-queue-application-full.log: 93/93 targets pass, 42 executed / 51 cached,
  765.261 seconds; both source hashes checked after completion.
- index-queue-application-race.log: 3 / 3468 selected specs pass with actual
  rules_go race configuration and no cached test result.
- index-queue-application-mutation.log: confirmed source mutation clearing failed
  UPDATE compiled and executed; UPDATE retention failed, the other two selected
  specs passed. Mutation restored byte-for-byte; source hashes checked.
- index-queue-application-restored.log: post-restore selected verification.
- index-queue-application-booking-full.log: full suite after restoration/booking.

No gate ACK is claimed. Queued state/dispatch/readability, policy and full drain/
session/cleanup are still open, followed by completed milestone review and WS-D–K.
No publication, PR changes or operational cleanup performed.
