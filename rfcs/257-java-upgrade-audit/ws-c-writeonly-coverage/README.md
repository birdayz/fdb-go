# WS-C state-transition prerequisite: preserve built coverage

Tracking: TODO.md, “WS-C readable-to-write-only coverage preservation”.
Discovered while tracing state transitions for the accepted shared maintenance
gate design. This increment does NOT implement that gate or queued state4.

Tagged Java FDBRecordStore.markIndexNotReadable (around lines 3690–3730) checks
whether the previous state is absent/READABLE. If its range set is empty, Java
inserts the full built range before marking it not-readable. Readable indexes
normally erase their completed range tracking, so a state-only transition must
restore that knowledge rather than scheduling a needless rebuild or rejecting a
later checked-readable transition. Existing nonempty range tracking is retained.
Go MarkIndexWriteOnly omitted this behavior; it now matches Java. ClearAndMark
remains the explicit fresh-build operation and still clears range coverage.

Retained proofs:
- Real-FDB regression was red (whole keyspace incorrectly missing), then green.
  It covers empty range-set restoration, READABLE round-trip, and preserving an
  existing partial range set without broadening its coverage.
- Live JVM oracle exercises Java-written and Go-written WRITE_ONLY transitions,
  checks persisted full coverage with both engines, and returns to READABLE using
  Go's checked transition without rebuilding.
- Full recordlayer run exposed four test fixtures relying on the old defect.
  Error-path, forced-stamp-restart, and chunked replacement tests now explicitly
  start a fresh build using ClearAndMarkIndexWriteOnly. Matching-stamp resume
  deliberately clears range tracking while retaining entries, modeling a worker
  that wrote entries before recording progress. All previous failure/row-count/
  retirement assertions remain unchanged. The full target passed after correction.

Verification, source hashes unchanged after full suite and race:
- just test: 93/93 targets pass, 42 executed / 51 cached (700.972 seconds).
- Actual --@rules_go//go/config:race, both targets uncached: 9 selected recordlayer
  specs and 1 conformance spec pass; ordinary Go tests also execute.
- Logs adjacent: initial red, full recordlayer failures, corrected full target,
  JVM oracle, full ordinary suite, selected race.

No completed WS-C implementation ACK is claimed. Shared maintenance gating,
maintainer replay, state/policy and drain/session/cleanup integration remain open;
WS-D–K and actual CI remain. No publication or operational changes performed.
