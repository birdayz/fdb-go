# WS-B implementation NAK follow-up — intermediate verification

Historical intermediate checkpoint. The state-path obligations and ~8.7%
measurement below are superseded by [state-path closure](../ws-b-state-path-closure/).
That follow-up also changes SPFresh's direct state gate and requires its review.

Parent: [implementation reviews](../ws-b-implementation-review/).
Execution tracking: TODO.md, “WS-B implementation NAK follow-up — TEXT,
deletion acceptance, build-state cancellation”. This is not a milestone ACK.
Published HEAD remains `71ccd8cf8b3fd0dbafe283e91171818e36af555e`.
No commit, push, merge, PR-state change or new review lap was performed.

## Multi-type descriptor validation

The metadata validator now visits both per-type and multi-type associations.
Pinned Java MetaDataValidator.validateIndexForRecordTypes validates every covered
record descriptor. Four retained Go cases exercise valid string bodies and
numeric, repeated and missing bodies. All three invalid cases failed before the
fix by returning nil. The corresponding four live-Java cases now pass.

The first Java run failed only because its missing-field error wording differs
from Go's pre-existing wording. The test now checks the JavaError type,
InvalidExpressionException class and exact `Descriptor Beta does not have field:
payload` message, alongside Go's field-specific rejection. This is not an
accept-either or success-on-error assertion.

The first full run exposed three invalid PK-dedup fixtures: their multi-type
Order/Customer index referenced order_id, which Customer does not declare.
The fixtures now use the shared price field plus a literal non-PK component,
with price as the overlapping PK. Single-type positive controls and full exact
stored-key assertions remain. The focused 51-spec population passes. An initial
fixture edit incorrectly assumed Order had customer_id; the retained failed
build is not credited as test execution.

## Deletion acceptance

Fourteen new specs complete the requested shapes in store_api_test.go:
- Creation versus deletion: both commit orders, same pinned read version.
- Cacheability enable/disable versus deletion: both commit orders, cold and
  pre-opened cache. Exact first-attempt 1020 on the loser; both fresh-cache and
  reused-cache reopen check the winning header/deletion. A noncacheable header
  is intentionally not admitted, so its pre-open is not claimed as a cache hit.
- Valid unknown header fields, with and without cacheability, preserve the
  conditional metadata-stamp invalidation decision.
- Actual versioned SaveRecord then DeleteStore in formats 5 and 14. The tests
  establish incomplete versions and nonempty deferred/local-version state
  before deletion, empty queues afterward, then commit and inspect every key
  in the store subspace and cold-open absence. No manual mutation injection.

All 41 DeleteStore-focused specs passed. Existing range-boundary and injected
mutation tests remain; they are not substitutes for the new save-path tests.

## Online build state error propagation

The production caller audit found LoadIndexBuildState could report a successful
state after cancellation, including a lazy Build handle. Six retained cases
cover lazy/open handles and READABLE/WRITE_ONLY/DISABLED states. The red run
fails; all six pass after using the error-returning transactional state helper.
The other three online-indexer operational decisions (resume detection, source
scannability, already-readable completion) now use the same helper. Their
Java counterparts are IndexBuildState.loadIndexBuildStateAsync and
IndexingBase/IndexingByIndex state checks. The focused build/indexer population
ran 101 specs successfully.

## Verification and provenance

After pinned gofumpt, just gazelle and bazelisk mod tidy, 6712 tracked/untracked
nonignored files were frozen by SHA256. The exact file set and hashes were
verified unchanged after both final runs, before this evidence booking:
- `review-followup-full.log`: just test, 93 targets pass; 6 executed, 87 cached.
  This is NOT an all-uncached final gate.
- `review-followup-race.log`: actual rules_go race instrumentation, 147/3385
  focused Ginkgo specs pass, plus the target's unfiltered Go unit tests.
  The filter covers DeleteStore, build-state cancellation, OnlineIndexer,
  IndexBuildState, multi-type TEXT and concurrent warmed-cache publication.
  This is NOT the full race suite.
- `review-followup-instrumented-aquery.log` shows linux_amd64_race and -race in
  both GoCompilePkg actions. The earlier --features=race invocation did NOT
  instrument Go; it is retained as review-followup-uninstrumented.log and is
  not credited as race evidence. Its aquery is retained separately.
- Four live-Java TEXT cases ran in text-multitype-java-green.log. The subsequent
  just test runs also executed the full conformance target.
- git diff --check passed. HEAD was rechecked unchanged.

The initial full test command timed out at 240 seconds after reporting the
three invalid fixtures; its log is retained as incomplete, not a completed
suite. Later full runs completed with an adequate foreground timeout.
No background verification jobs remain. Evidence hashes are recorded in
`evidence-sha256.json`; the freeze predates these evidence files and TODO booking.

## Still-open milestone obligations

The three implementation verdicts remain NAK until final-tree confirmation.
The context-shared state projection, lazy initialization, retirement metadata
selection and cache-publication fixes from the preceding work are still subject
to the full final review and mutation campaign. Remaining acceptance includes
real executor routes, function-selection negative conflict matrix, independently
warmed lazy-transition invalidation/header-read errors, same-context bulk
deletion, and completion of the operational state audit (including SPFresh's
direct search API). No SPFresh source was changed in this follow-up.

The measured post-state-view write overhead remains approximately 8.7% against
the earlier true-merge-base samples; it is not parity, not final-tree evidence,
and still requires profiling and controlled remeasurement. Whole-tree uncached
verification, full race/Java/stress and final actual reviewer ACKs remain open.
WS-C–K and final CI are not completed by this work. Publication still requires
the user's explicit authorization.
