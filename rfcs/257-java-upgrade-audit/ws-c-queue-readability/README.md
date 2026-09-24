# WS-C checked readability queue guard

Tracking: TODO.md, “WS-C checked readability queue guard”. Authority:
../ws-c-design.md, section 4's explicit strengthening of Java checked publication.
Java FDBRecordStore.checkAndUpdateBuiltIndexState checks completed ranges; the
accepted design requires surviving buffered and persisted queued writes to block
publication as well. This is not a claim that tagged Java already has the guard.

checkIndexBuilt now rejects pending queue work after its existing range check.
It checks the actual shared context version-mutation registry under versionMu,
then a serializable limit-one entry-range read. Counter values never decide.
Existing high-level checked setters retain the shared maintenance write lock
through this check and publication. IndexNotBuiltError retains existing range
messages and adds PendingWrites to distinguish this refusal with index context.

Seven real-FDB specs in index_maintainer_queue_test.go:
- Buffered enqueue refuses checked publication through same/other handles and
  MarkIndexReadable/MarkIndexReadableOrUniquePending (four cases). Context clear
  removes the refusal while a DIFFERENT index still has buffered mutations.
- A persisted entry with a deliberately zeroed counter still refuses publication;
  applying/clearing it allows the transition (one case).
- Explicit independent transactions verify both commit orders: queued writer
  commits first and closeout gets 1020; closeout commits first and the writer's
  state-read conflict gets 1020 (two cases). No retry hides either outcome.

Scope: these tests enqueue through the internal queue and establish the state
read explicitly; queued-state store writer dispatch is not wired yet. They do
not claim that the entire state-4 writer/closeout protocol is enabled. The earlier
shared maintenance gate tests cover ordinary concurrent writer/publication lock
ordering; queued writer-before-buffering barriers still need production dispatch.

Evidence in /var/tmp/fdb-upgrade-recovery/ws-c/:
- queue-readability.log: 7 / 3475 selected specs passed.
- queue-readability-full.log: full suite 93/93 passed, 42 executed / 51 cached,
  740.322 seconds, five source hashes verified after completion.
- queue-readability-race.log: same seven specs passed under actual rules_go race,
  uncached test result.
- queue-readability-mutation.log: confirmed inverted buffered-range mutation
  compiled and executed. All four buffered refusal cases failed; the persisted
  and two transaction-order cases passed (population: seven selected specs).
  Restored byte-for-byte and five hashes checked.
- queue-readability-restored.log: uncached post-restoration selected run.
- queue-readability-booking-full.log: full suite after restoration and booking.

State-4 dispatch, checked queued setter, eligibility/overflow policy, full drain
retry/session/heartbeat/cleanup and completed implementation gate ACKs remain.
Maximum/default format is unchanged. WS-D–K and actual CI remain. No publication,
PR changes or operational cleanup performed.
