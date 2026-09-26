# WS-C shared maintenance gate foundation

Tracking: TODO.md, “WS-C shared maintenance gate foundation”. Design authority:
../ws-c-design.md, accepted design reviews in ../ws-c-design-review-v3/.
This is ordinary-state infrastructure, NOT completed WS-C or queued-state proof.

The existing context-shared transactionIndexStateView now owns a maintenance
RWMutex. Single-record dispatch, the whole batch, and DELETE_WHERE take its read
side after the handle stateMu. Checked readability and other high-level index-state
transitions hold both exclusive locks from before reading authoritative state
through validation, publication and cleanup. A separate already-locked setter
avoids recursive acquisition. The low-level setter acquires the same shared gate.
Reload takes it before registry/view locks; initial loadStoreState now takes its
handle lock before the registry instead of waiting for the handle under registry.
Initial unpublished binding does not take the maintenance gate. Lower context
clear operations retain their existing locks and do not reacquire maintenance.

The Java source authority is FDBRecordStore's begin/end state-write scope around
markIndexReadable/checkAndUpdateBuiltIndexState and markIndexNotReadable, plus
state-read scope around index maintenance. The shared-context Go extension follows
the accepted lock order, using the already-existing shared state view rather than
adding another state registry.

Retained real-FDB barriers pause actual transaction writes/clears; no storage
results or mutations are mocked. Ten cases:
- writer-first single record and batch, one/two handles (four);
- writer-first DELETE_WHERE, one/two handles (two);
- setter-first READABLE and READABLE_UNIQUE_PENDING, one/two handles (four).
TryLock/TryRLock probes at the barrier establish exclusion without timing-only
negative assertions. Operations then finish and persisted state/data are checked.
Initial single-handle writer cases were observed red before gate acquisition;
the two-handle fixture was corrected to bind both initially lazy state views before
comparing their identities. A compiled mutation removing the high-level setter's
shared gate fails all four setter-publication cases; restored byte-for-byte.

Verification before the restored-source booking run:
- Full just test: 93/93, 42 executed / 51 cached (728.350 seconds).
- Actual race: 57/3460 selected state/lifecycle specs passed; target uncached with
  --@rules_go//go/config:race. Ordinary Go tests also ran.
- Five source hashes remained unchanged after full/race verification.

Logs adjacent. Queued writer-before-buffering barriers, buffered/persisted queue
readability checks, capability/replay, and session/cleanup integration still need
the subsequent state4 implementation. Format15/state4 are not enabled. Completed
WS-C implementation review and WS-D–K remain open; no actual CI/publication claimed.
