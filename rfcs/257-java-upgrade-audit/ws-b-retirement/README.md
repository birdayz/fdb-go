# WS-B replacement lifecycle — intermediate implementation evidence

Java: 4.14.2.0 fdacd162a9c8acfadc49082b89185c823ab8ae4a.
Published Go HEAD remains 71ccd8cf8b3fd0dbafe283e91171818e36af555e.
Uncommitted intermediate sources are identified by retirement-sources.sha256;
all six hashes were checked after the full suite and full race target.

Implemented:
- All three explicit commit APIs share checks, deferred version flush, commit,
  versionstamp resolution and postcommit. The commit span starts before checks
  and ends before postcommit; failures are counted. Run variants still have their
  own retry loops and are not routed back through checks a second time.
- Private named context checks, ordered with anonymous checks, deduplicated by
  name and cancellable before invocation even in an existing snapshot. Callback
  execution is outside commitMu. Registration suppliers run under commitMu.
- Changed mark-readable / mark-readable-or-unique-pending schedules retirement,
  keyed by store subspace. Retirement requires all named replacements to exist
  and be exactly READABLE, using serializable transaction-visible state reads.
  Lifecycle transitions also use those reads rather than stale per-object maps.
- DeleteStore cancels the pending retirement registration after its successful
  header read and before clearing. This deliberately corrects the Java defect
  documented in upstream-report.md and DIVERGENCES.md.

Retained evidence:
- commit-red: six new specs ran; both plain-commit cases failed before the fix.
  Subsequent commit tests also pin transaction failure and timer behavior.
- retirement-red: all three explicit commit APIs failed to retire the original
  after completing the second replacement, without changing metadata version.
- retirement-delete-red: basic retirement passed, but both deletion cases failed
  with resurrected state keys (delete before commit and from an earlier check).
- retirement-green: 77 focused Ginkgo specs PASS. Includes multiple store objects
  with demonstrably stale informational caches, dedup, and WRITE_ONLY / DISABLED /
  READABLE_UNIQUE_PENDING readiness negatives.
- retirement-just-test: 93 targets PASS, 42 executed / 51 cached, 733.722s.
- retirement-race: uncached whole recordlayer target PASS; 3273 / 3274 Ginkgo
  specs passed, with the existing opt-in million-record spec not enabled.
- Six independently applied, compiled, killed and restored mutations: plain
  commit bypass, named dedup, cancellation, exact readiness, DeleteStore callback
  cancellation, and transaction-visible state reads. Runner and logs retained.
- Java probe: one focused live-Java spec PASS, observing RemainingRows=1,
  HeaderPresent=false, OriginalState=2 after deletion and commit. This is evidence
  of a Java defect, NOT evidence that Go should recreate deleted state.

Historical status: the initialization, cleanup, state-conflict and extended
lifecycle obligations listed as active at this initial checkpoint are now
implemented. Follow-on evidence and current remaining scope are recorded in
[lifecycle/README.md](lifecycle/README.md). Metadata evolution, rank values and
final milestone review remain open; the early greens above are not final ACKs.
