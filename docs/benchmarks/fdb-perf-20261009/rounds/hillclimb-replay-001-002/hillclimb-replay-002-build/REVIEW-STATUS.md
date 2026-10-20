# Scratch replay review status (append-only evidence, not whole-tree approval)

Parent-project tests are owned by root. At this record's creation the measurer has no reported Bazel execution/revert/race result for the newly added tests. Both remote standalone harness versions passed15 top-level tests; those are not the main-module regression tests.

## hn-codehealth:001

Correctness ACK for all-plain-Set transformation and borrowed operand lifetime, but two Medium findings: unordered full-log stable sorting can regress asymptotically on duplicate-heavy logs; process-global AllocsPerRun can be contaminated by background allocations and test was nonparallel. Low: nil Value may remain nil instead of empty non-nil, but wire bytes identical.

## hn-codehealth:002

ACK, nothing Medium or above. Both Medium findings fixed. Ordered/type pre-scan is O(n), returns immediately on first non-Set or descending key; fallback keeps old map replay. Two-pass ordered dedupe uses O(distinct) output storage and no sort. Byte borrowing still safe. Parallel structural identity test distinguishes old replay because old replay copies operands. Ordered half of randomized oracle actually exercises fast path.

Low notes retained, not hidden: hot-key cap1 assertion protects against001full-log clone rather than old replay; unordered oracle half compares fallback with old replay; new table subtests lack their own t.Parallel though top-level test is parallel. Reviewer recommends unordered hot-key and mixed early-exit timing controls before a broad no-regression claim.

FDB C++ reviewer hn-client was asked to review002 with actual7.3.77 citations; response pending at this record's creation. Review applies only to the delta. Known read-order/watch/COW base issues remain outside correctness approval. No shipping or merging authorized by these performance results.

## readorder-final:002 — later completed semantic review

FDB C++ semantic ACK for exact patch SHA b313b48ba4ce1984c6102c9bb1449149d3dd5174c04d225126ae524da7540c97. Candidate ryw.go:494–520 rejects any non-Set or decreasing byte key before output, retains last adjacent duplicate, emits ascending distinct keys; equivalent to WriteMap.cpp:125–137 replacement and ReadYourWrites.actor.cpp:2025–2044 ordered Set emission. Borrowing operands remains safe: ownMutationLocked transaction.go:1456–1470 owns capped bytes; snapshot :1923–1926 is validated :1948–1952; captureCommit copies commit_input.go:57–68 before lease release transaction.go:2055–2060. Versionstamp/RYW-disable/raw conflicts and size gates unchanged. No new Medium+ semantic finding. ACK scoped only002 vs base; not baseline watch/COW. Reviewer performed no edits/builds/tests/remote calls. This supersedes the pending-review status above; hn-client failed compaction and was not reused.
