# WS-D design gate v10

TREE `f90241712769c8d5f92498c51c2183a4320ed9b0`; design SHA256 `b097a37ba40dd4adb0fe741b98d77282c8c2826be94cd8ebcffa1ab7f4daf096` (1365 lines). Answers the four v9 NAKs:
the time budget withdrawn for sampled refits (oracle item 20), consumerOutcome in
Java's statement order with four outcome values, positioned-RNG bounce hand-off,
serializable-side and delete conflict fixtures, named poison keys, merge-side n<k,
reconcile replica reads, retry owners, the heartbeat key-parse fix, evidence rebound
(items 19 corrected, 20-22). Lenses: Graefe, Torvalds, storage/wire, SPFresh.

Frozen-tree records: the frozen tree is the working tree at snapshot, byte-identical before and after (content manifests of every tracked and untracked file, /var/tmp/fdb-upgrade-recovery/v10b-manifest-before.txt and -after.txt, identical, and the tree re-hashed equal after). just test on it: /var/tmp/fdb-upgrade-recovery/just-test-v10b.log, Executed 7 out of 94 tests: 94 tests pass; the other 87 were served from Bazel's action cache, keyed on byte-identical inputs, from just-test-v10.log earlier the same session (that run was red on four targets: the stale DST seam allowlist key, an unlisted new test file, a Go test pinning a disproven Java ClassCastException claim, and fromless JSON renders that predated RowSet.Nullability; each fixed before v10b). Ginkgo summaries from the v10b executions (bazel-testlogs, timestamps 15:17-15:21): conformance_test Ran 1405 of 1524, 1405 Passed, 119 Skipped (inherited from master, TODO.md); rfc257_oracle_test Ran 22 of 22 Passed; recordlayer_test Ran 3615 of 3616 Passed, 1 Skipped (the RUN_MILLION_RECORD_TEST env gate, on master). ws-d-oracle/evidence-run.txt comes from two further uncached 21-spec runs (rfc257-v10-1.log, -2.log) before the WS-J non-integer-operand spec was added as the 22nd.
