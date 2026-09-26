# WS-C commit-bound target retirement delta

Candidate tree is supplied in review prompts. Previous candidate tree
`5dc2a9cf465ff439ac75d1e5eb5682eecd0ca38b` received actual storage/SPFresh ACKs
and Graefe/Torvalds NAKs in `../ws-c-final-delta-review/`. The ACKs did not override
the NAKs: scannable-state skipping was not permanent ownership retirement.

Java `IndexingBase.markIndexReadableForIndex` independently completes each
target after its publication commits. Go's sequential all-target liveness
callbacks must likewise release completed targets permanently, without altering
the original target list used for stamp identity.

Implementation now records successful own closeout in `retiredBuildTargets`,
and records observed peer completion through the record context's post-commit
hook. Failed observation transactions cannot release targets. Follow-up
validation, drains, requested merges, and later closeout skip retired targets.
Fresh build preparation resets the set; standalone explicit merge also starts
fresh instead of inheriting a previous build's released targets.

Six added real-FDB specs in the existing Bazel-registered follow-up test file:
* Published A is disabled while B continues closeout.
* A new single-target builder is admitted after A's publication, while the old
  multi-target builder finishes B/C without touching A's new stamp/heartbeat.
* A declares B as replacement; publishing B disables completed A; queued C must
  still drain and become readable (no competing process needed).
* Peer completion observation commits: subsequent disablement of released A
  must not prevent B's drain or merger, nor re-enter A during closeout.
* Peer observation aborts: neither early retirement nor ignored disablement is
  allowed; B's drain and merger must still fence A.
* Reusing the same indexer for a fresh full BuildIndex starts new ownership and
  actually rebuilds/publishes all targets, not a stale-completion no-op.

Evidence root `/var/tmp/fdb-upgrade-recovery/ws-c/`:
`retirement-red.log`: all three concrete closeout reproductions failed before
production edits. `retirement-target.log`: all 12 follow-up specs pass.
`retirement-early-mutation.log`: publishing peer retirement before commit builds
and fails both observation cases. `retirement-reset-mutation.log`: retaining the
old session's set builds and fails the new-build regression. Mutations were
verified present and restored before final verification.

`retirement-full.log`: just test 93/93 passed, 6 executed / 87 cached,337.230s.
This is NOT a new 93-executed claim; the predecessor's full uncached run remains
historical. `retirement-race.log`: actual race, uncached,189/3610 Ginkgo specs
plus ordinary Go tests/fuzz seeds. `retirement-jvm-race.log`: actual race,
uncached,9/1523 JVM specs including nested Any and format15. The source manifest
`retirement-source.sha256` was checked after all final runs. git diff --check,
Gazelle and bazelisk mod tidy passed.

Read the complete exact tree-to-tree source delta and interactions with the
prior fixes, their reviews and immutable `../ws-c-design.md`. See prior scope
for the original five fixes. No previous ACK applies automatically to this tree.

Unchanged boundaries: max format15/default14, approved extensions, C++7.3.77,
SPFresh queue exclusion and unchanged algorithms/LIRE; HNSW synchronous. No
GuardiANN/Lucene deferred-child backend proof. WS-D–K and absent Lucene remain
upgrade-wide work. No CI, overall migration completion or merge-readiness claim.
Read only; no tests, edits, mutations, commits, publication or operational changes.
