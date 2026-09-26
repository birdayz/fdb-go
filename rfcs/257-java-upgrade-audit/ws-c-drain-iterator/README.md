# WS-C drain iterator increment (not milestone acceptance)

Reference: Java 4.14.2.0 `runners/throttled/ThrottledRetryingIterator.java`
and `IndexingPendingWriteQueue.java`; WS-C's accepted design is unchanged.

Implemented a single-owner retry iterator using `Runner.OpenContext`, explicit
commit/cancel, commit-bound continuations, 100 retries, adaptive row limits,
rate waits, cancellation, and concurrent close. Queue draining is called after
record-building batches, including the exhausted batch. Each drain transaction
checks queued state and the expected/unblocked stamp at commit and refreshes a
stable build-owned heartbeat. All exits attempt bounded independent-context
cleanup of that session's target heartbeat keys; other sessions are untouched.

This increment did NOT complete the queued build lifecycle. Its then-open
pre-build-batch session validation, snapshot record conflicts, and readable
closeout retries are subsequently implemented and verified in
`../ws-c-session-closeout/README.md`. Deferred maintenance contracts and normal
format-15 opening remain unfinished. The
format ceiling remains 14. No implementation review ACK is claimed.

## Evidence

Logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

- `throttled-target.log`: seven real-FDB Ginkgo specs, plus the delay unit test.
- `throttled-full.log`: initial full run failed the unreferenced-production-function
  gate (iterator had not yet been wired). Fixed by adding the production drain
  path, not by exempting the constructor.
- `drain-target.log`: eight selected specs / 3503 registered, plus delay unit test.
- `drain-race.log`: same eight selected specs and delay unit test passed with
  `--@rules_go//go/config:race`, uncached.
- `drain-full.log`: `just test`, 93/93 targets green; 38 executed, 55 cached;
  761.767 seconds. Six source hashes checked unchanged after verification.

The focused tests pin commit-failure replay from the previous successful
continuation; a final empty heartbeat commit; exactly 101 failed attempts;
retry-allowance reset; adaptive increase after forty successes; active-context
cancellation and closure; concurrent closure during throttling; pre-closed
iteration; and persisted own-session-only heartbeat cleanup. The drain fixture
uses Java-permitted format-14 queued state through raw test setup; it does not
raise Go's checked setter/opening ceiling prematurely.

No commit, push, PR change, or CI publication was performed. These are local
verification results, not CI status or milestone gate acceptance.

Subsequent normal format-15 opening and lifecycle acceptance evidence:
`../ws-c-format15/README.md`. Earlier format-14 ceiling statements above describe
their measured increment, not the current implementation.
