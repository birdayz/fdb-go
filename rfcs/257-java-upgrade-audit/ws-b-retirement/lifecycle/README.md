# WS-B retirement lifecycle follow-on evidence

This supersedes the early retirement coverage/status in the parent README.
Java reference: 4.14.2.0 fdacd162a9c8acfadc49082b89185c823ab8ae4a.
Published Go HEAD remains 71ccd8cf8b3fd0dbafe283e91171818e36af555e;
retirement-lifecycle-freeze.json identifies the 4530 uncommitted source/build
files verified unchanged after the final full and race runs.

Implemented beyond the initial commit/check mechanism:
- Fresh and reconciled replaced originals initialize DISABLED before policy;
  changed-index enumeration remains unfiltered, build eligibility is filtered.
  Immediate reconciliation retirement and inline/chunked online completion work.
- Serializable transaction-visible state drives maintenance and all three scan
  entrypoints, including negative disabled/unreadable decisions. Retained tests
  exercise stale objects, both conflict commit orders and explicit 1020 retries,
  overlapping builders, state-read failures, delete/recreate and missing names.
- Context range clears cancel deferred version writes and cached local versions.
  Retirement erases Java build subkeys 1–9 (including exact prefixes), preserving
  lock 0 and future 10+. Former-index cleanup uses FormerName for state, not the
  independent subspace key; Java's surviving build data is preserved.
- Unique-pending retirement exposed wire/cleanup defects: uniqueness violation
  keys now append the FULL primary key, not trimmed index-entry components;
  deleting a duplicate clears the final survivor; strict readable-transition
  errors preserve ExistingKey. Public low-level one-entry removal is unchanged.
- Named-check lifecycle fuzz covers deduplication, cancellation, ordering and
  re-registration against a model; no new public partial commit-check API.

Verification (no final milestone ACK claimed):
- retirement-current-focused.log: 146 focused Ginkgo specs PASS out of 3320.
- retirement-lifecycle-just-test.log: 93 targets PASS, 2 executed / 91 cached.
- retirement-lifecycle-race.log: uncached full recordlayer race target PASS,
  3319 of 3320 Ginkgo specs. Existing opt-in million-record case not enabled.
- retirement-final-java.log: three focused live-Java specs PASS: the deletion
  resurrection probe and both Go-written/Java-written composite-PK uniqueness
  fixtures. Exact raw keys, full conflicting PK values and counts [5,4,2] pinned.
  Java async uniqueness completion can choose different valid cross-references;
  tests require a different PK in the same value group, not identical choices.
- retirement-uniqueness-java-premature-read.log retains the initial probe failure:
  reading in the seeding transaction preceded Java precommit uniqueness futures.
  The corrected fixture commits seeding before inspection/deletion.
- retirement-commit-fuzz.log: 6,208,485 executions, 30 seconds, four workers,
  PASS without coverage guidance.
- Eight initialization/cleanup and nine state-conflict/uniqueness mutations were
  independently applied, compiled, killed and restored. Scripts, JSON outcomes
  and individual logs are retained here. These and the parent's six mutations
  were measured on intermediate test populations, NOT the final 3320-spec tree.
  The two separately named build-failure logs are uncredited attempts.

WS-B metadata evolution, rank-valued scans, remaining interoperability and final
performance/implementation-review gates are still open. No extra state overlay
was introduced; existing client RYW serverCache services repeated reads. No
performance parity claim. No commit, push, merge or PR-state changes performed.
