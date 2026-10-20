#003 API-compatible revision — pre-rebase, not a PR measurement source

Patch arena-003-api-compatible.patch SHA2563f2158699f0166049bb649b90a16a821c212e7bf1fd087757c23858cb441d831. Candidate757-file manifest SHA256d4a3f8c21d6cbececb601e7b15e1f0ff44997e8a929854d531a7c6ff368ce506.

The candidate directory still has the immutable measured replay002 WIP parent; the directory suffix `-pr` identifies API-adaptation work, NOT rebasing onto PR789. Root request430 requires the eventual003 comparison to use the forthcoming post-replay002 corrected PR789 export as baseline, then baseline+003 and immutable CGo. No prior WIP measurement may be relabeled as a PR result.

Production transaction.go is byte-identical (`cmp` verified) to initial003 patch e8162019ac2854d2f2dc46b2285e27fab685f5f1ef1d3537ef2f6916a79a83d6, which readorder-final semantically ACKed and root source-reviewed as consistent. See durable initial003 archive for source proposal and C++ citations. No timing is authorized before root alias review and PR export/rebase.

## Test delta

The initial BeginRead/issue-view test depended on read-order WIP APIs absent from PR789. Replaced it with real Get/Snapshot.Get, parked at the existing beforeReadVersionLock seam, plus a separate execution lease holding the mutation-header snapshot. Twelve cases cross user/retry/postcommit reset, Get/Snapshot.Get, and both lease-release orders. Incarnation cancellation occurs before releasing the parked read, so that read must report literal1025 before GRV/network I/O; no mock database or invented read transport is involved.

While leases remain, checks include active head, predecessor, complete bounded-chain count, current slab used length, and raw mutation bytes. After both release, reset retains only current bounded slab or postcommit releases all; independently owned commitInput bytes/conflicts remain stable through successor writes. Nonblocking completion checks are observations, not a standalone scheduler-proof of drain behavior. The lease drain is existing production infrastructure.

hn-codehealth NAKed the initial patch on a Medium: the dedicated conflictbuf_unit_test Bazel target does not include ownership/lifetime helper files. Fixed here with local helpers and built-in clear; no other test source was added to its target, avoiding duplicate execution. The runtime code received ACK. Revised test review requested; pending at this record's creation.

## Verification

Completed: gofmt on changed test file; candidate757 hashes; byte-for-byte unchanged production file versus initial003; grep confirmed no BeginRead/acquireView/op.issue or cross-target helper names in conflictbuf_unit_test.go, with positive test/helper-name census. Inspected immutable292b79226 APIs and BUILD declarations, not active worktree files.

Not performed: compile, main-module Bazel fast/full/race or regression execution, remote build, profiles, timing. Root owns normal Bazel validation and will provide the corrected PR export. No worktree modified. Existing001/002 results remain historical WIP results, not correctness approval or003 performance evidence.
