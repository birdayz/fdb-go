# WS-C implementation finding: destructive preparation before admission

The implementation review of tree `2a4d33046f8640673643ea989709665f425cbbc5`
returned NAK. This addresses its heartbeat-preflight finding, not the remaining
cleanup, nested-Any, mutual-renewal, or follow-up-liveness findings. No final-tree
review ACK or CI result is claimed.

`IndexingHeartbeat.checkAdmission` performs read-only, serializable admission.
`markWriteOnly` installs it before `StoreBuilder.Open` metadata reconciliation;
`prepareIndexingState` also runs it before any explicit clear. Every target is
checked before any target is cleared. Continued mutual builds permit UUID peers
only with unchanged metadata; fresh resets and metadata reconciliation require
peer quiescence. Legacy and malformed keys always refuse admission. Ordinary
store opening remains unchanged.

Java reference: tag `4.14.2.0`, `indexing/IndexingHeartbeat.java` and
`IndexingBase.handleStateAndDoBuildIndexAsync`. Fail-closed legacy-key admission
is the already accepted Go migration safety requirement; it must precede both
of the destructive lifecycle paths, not merely renewal after those paths.

## Regressions and verification

Raw logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

* `OnlineIndexer preparation heartbeat admission`: 48 real-FDB specs across
  public ordinary/queued/multi-target/mutual builds, direct preparation, and
  open-time metadata upgrades; legacy keys, malformed keys and live UUID owners;
  readable/disabled primaries where targets already exist. Multi-target cases
  place the owner on a write-only follower. Refusal must preserve **all store
  bytes**, including when a direct caller deliberately commits after refusal.
* Removing pre-open admission: all 6 selected metadata-upgrade cases failed
  (`admission-open-mutation.log`). Removing preparation admission: all 18
  selected direct-preparation cases failed
  (`admission-preparation-mutation.log`). Mutated source was asserted present;
  both runs built and executed tests before failing; originals were restored.
* Initial fixtures were corrected before banking mutation evidence: target names
  must survive the builder's sorting, and indexes added in version 2 need explicit
  version-2 index metadata. The first pre-open mutation survived because the
  initial fixture did not actually trigger index reconciliation.
* The full run exposed a deterministic DST fixture defect: advancing one second
  per clock read made extra admission reads expire SimFDB transactions. Its
  build-time-limit test now advances time between successful Run commits, with
  transaction time held stable. Anchor and elapsed-clock mutations still fail
  (`admission-clock-{anchor,elapsed}-mutation.log`). The existing latency-only
  DST allowlist entry follows `Open`'s extracted `openWithPreflight` implementation.
* `admission-final-full.log`: `just test`, 93/93 passing, **2 executed/91 cached**,
  117.801 seconds. The preceding run executed 38 targets and exposed the two
  failures fixed above; it was not a green result.
* `admission-race.log`: actual `--@rules_go//go/config:race`, uncached, **70/3570
  Ginkgo specs**, plus `TestBuildIndexTimeLimitUsesTheEnvClock` and both subtests,
  passing. Focus: preparation admission, heartbeat, batch session fencing and
  `Mutual Indexing` matches. This is not an all-suite race claim.
* Seven edited-source SHA256 values were checked unchanged after both the final
  full run and race run (`admission-source.sha256`).

No commit, push, PR change, CI-triggering publication, or merge performed.
