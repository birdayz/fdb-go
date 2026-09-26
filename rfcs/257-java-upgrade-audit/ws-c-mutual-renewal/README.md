# WS-C implementation finding: mutual renewal conflicts

Addresses mutual heartbeat contention in the NAK of tree
`2a4d33046f8640673643ea989709665f425cbbc5`; no final-tree ACK or CI claim.

Java 4.14.2.0 `IndexingHeartbeat.checkAndUpdateHeartbeat` updates only the
caller's UUID key in mutual mode. The Go compatibility scan belongs at
admission, not on every disjoint-fragment transaction.

`OnlineIndexer.admittedHeartbeat` is published only after `markWriteOnly` commits
and reset on preparation/build entry and terminal cleanup. Periodic renewal
writes only its own UUID when the mutual heartbeat is exactly the admitted
session. Unadmitted internal callers and exclusive sessions still use the
fail-closed `CheckAndUpdate`; that public method's compatibility checks are not
weakened. The mutual batch no longer renews the same shared session twice.
Drain/merger renewal uses the same helper. Operators must still fence legacy
workers before migration; admission is not ongoing detection of rogue workers
introduced after admission.

## Evidence

Raw logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

* Two new real-FDB specs in `Mutual heartbeat renewal concurrency`.
  One blocks **both transaction bodies before either commits** for two disjoint
  fragments of the same index. Both must commit on their first attempt, build
  two records each, cover the whole range, and produce four index rows.
  The other introduces an incompatible legacy heartbeat after preparation's
  body, forcing an actual conflict and retry. Rejected admission cannot publish
  the fast-path token, and subsequent unadmitted renewal still refuses the key.
* `mutual-renewal-mutation.log`: restoring peer-range reads compiled and executed
  both specs; the overlapping-fragment test failed (1 failed/1 passed).
* `mutual-admission-mutation.log`: publishing admission before commit compiled
  and failed the selected rejected-preparation regression (1 executed).
  Mutations verified present and source restored.
* `mutual-renewal-full.log`: `just test`, **93/93**, **6 executed/87 cached**,
  343.370 seconds.
* `mutual-renewal-race.log`: actual `--@rules_go//go/config:race`, uncached,
  **10 executions of 63/3596 Ginkgo specs**, all passing. Focus covers mutual
  concurrency, IndexingHeartbeat, and preparation admission. Not an all-suite
  race claim.
* Six edited-source hashes checked unchanged after full and race runs:
  `mutual-renewal-source.sha256`.

Multi-target drain/merger follow-up liveness remains open.
No commit, push, publication, CI trigger, or merge performed.
