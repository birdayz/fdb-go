# WS-C queue storage and raw cursor — implementation in progress

Tracking: TODO.md, “WS-C queue storage and version conversion”. Design authority:
../ws-c-design.md and ../ws-c-design-review-v3/. These results are prerequisite
verification, NOT completed WS-C implementation acceptance or upgrade completion.

Implemented a typed versionstamped protobuf queue, snapshot capacity/counter reads,
serializable persisted emptiness, conflict-protected entry clears, and a raw split
cursor with physical-KV limit accounting. Java-written split cases compare exact
continuations, stop reasons, rows, resumed rows, terminal caching, and scan/byte
counts. Envelope cases include closed proto2 enum normalization. Skip behavior is
pinned in both writer directions: Java ignores queue Skip; Go preserves that.
Go-produced multi-chunk entries are read by Java's production queue reader.

DFS fixes in this increment:
- FDBRecordVersion conversions now preserve incomplete versionstamps in both
  directions, matching tagged Java FDBRecordVersion.fromVersionstamp/toVersionstamp.
  The old rejection caused five queue storage specs to fail. Retained unit and JVM
  regressions check locals 0, 5, and 65535 and the exact placeholder bytes.
- Public Go queue entries can bypass Java's package-private constructor. ClearEntry
  now enforces the same two-element key shape before clearing, preventing an empty
  key from clearing the entire queue prefix. A red-to-green real-FDB regression
  also checks that rejected keys leave the entry and counter unchanged.
- Raw split assembly uses Java's missing-start classification when a version is
  followed by a non-start segment or reverse assembly jumps over the start. The
  reverse suffix boundary and orphan-version rejection are pinned separately.
- Raw records overlay context-local versions as Java KeyValueUnsplitter does.
  Missing-start classification and the local overlay both have observed-red tests.
- Queue metric declarations are enrolled in the metric taxonomy source gate. The
  first full run failed that guard; registration fixes the failure without weakening
  the guard or changing assertions.

Verification of the eleven files in source.md5 (unchanged after both runs):
- Uncached full ordinary suite: 93/93 targets EXECUTED and passed (927.702 seconds).
  Command: bazelisk test //... --test_tag_filters=-stress --nocache_test_results
- Actual rules_go race: both selected targets executed and passed; recordlayer
  11/3447 selected specs, conformance 4/1515 selected specs. Ordinary Go tests also
  run in these targets. Command uses --@rules_go//go/config:race and
  --ginkgo.focus='PendingWritesQueue|pending queue|incomplete versionstamp conversions|incomplete ToVersionstamp'.
- Bidirectional skip oracle: six cases (Java/Go writer × skips 0/1/5).
- Previously retained eight split-limit and fifteen envelope oracle cases remain
  exercised by the selected conformance specs.

Logs adjacent: final full/race, selected green, bidirectional oracle, malformed-clear
red, and raw-cursor red. These are about the named tests, not every dimension of
queue correctness. Remaining WS-C work includes deeper malformed/time/cancellation
and structured-error coverage, maintenance gating, maintainer replay, policy/state
integration, drain/session/cleanup lifecycle, overflow behavior, and completed
implementation review. State4/format15 are not enabled. WS-D–K and actual CI remain.
No commit, push, merge, PR-state change, or operational heartbeat cleanup occurred.
