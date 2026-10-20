# Arena003 scratch proposal and implementation — awaiting root review

Parent: immutable measured replay002 source manifest ef374bd348fc4d9539c6929e27dc78cb8622bf7436e29490f84ad8ecbf7e98f8. Experimental base is read-order WIP, not root's corrected ownership checkout. No worktree was modified. Root approved scratch implementation in request429; **no timing is authorized until root reviews this patch**.

Patch arena-003-from-002.patch SHA256 e8162019ac2854d2f2dc46b2285e27fab685f5f1ef1d3537ef2f6916a79a83d6. Candidate757-file manifest SHA256 e275eeec36466fc95672ae063dc48b46f16bf76180e9c4800df11097d783d6b9.

## Source/alias proposal

Transaction conflict/mutation allocation uses fixed512B/32KiB slabs. Once a slab is full, the next pool owner links the predecessor; no used bytes are copied and no predecessor is prematurely pooled. Requests over32KiB receive independent exact-size allocations without entering the owner chain or disturbing the current slab. Return slices have capped capacities. The existing captureCommit byte copy is unchanged.

Both reset paths first run turnover, which retires the incarnation and drains execution users/pending resolvers (transaction_lifetime.go). Under conflictMu they clear mutation/conflict vector elements, reset RYW, then retire pool owners. Clearing vector elements also drops references to unpooled oversized allocations. Ordinary/user reset retains only the current bounded slab, returning its predecessors. Postcommit returns every slab. The return loop saves and clears next before Put, since another goroutine may immediately Get and alter that owner.

Actual C++7.3.77 source read: flow/Arena.cpp349–371 allocates from the current block or creates another, without relocating live operands;469–475 links prior blocks;481–518 destroys according to reference ownership. ReadYourWrites.actor.cpp2349–2353 copies key/value into its arena;2699–2727 resets the arena and write/cache owners. Go uses lease-delimited ownership and detached commitInput rather than C++ reference-counted arenas. Pool size policy is Go implementation detail, not a semantic equivalence claim about C++ pool capacities.

## Regression coverage added/updated

- Fixed bounded slab chain with no copied prefix; prior conflict ranges remain intact after additional allocations.
-80writes spanning three slabs,40KB direct operand, then a small write; caller-buffer scribbling does not change mutation/RYW bytes.
- Actual BeginRead establishes a registered issue, write-map view and execution lease; an extra execution lease holds a raw mutation snapshot. Concurrent user Reset, retry reset and postcommit reset each block until both leases release, in both release orders. Snapshot/view bytes are checked while their lease is valid, not after releasing ownership.
- captureCommit pointer independence and bytes/conflicts across turnover plus successor writes. A raw transaction snapshot is deliberately not promised valid after its lease is released.
- Narrow point-read structural allocation check:512B slab, adjacent bumps, same bounded owner/backing reused after retry. No process-global AllocsPerRun.
- Existing oversized-allocation tests now assert exact direct storage and no oversized pool owner rather than old copy-growing capacity.

## Verification performed / not performed

Completed: gofmt on the three touched files; all757 candidate hashes; all757 immutable002 hashes. Patch restricted to transaction.go, conflictbuf_unit_test.go, transaction_unit_test.go. Source-based C++/codehealth review requested from existing agents.

**Not performed:** main-module compilation, Bazel fast/full/race lanes or new regression execution; root owns these. No003 binaries, timing, profiles, or performance claims exist. Existing001 heap measurements motivated this hypothesis but do not price003. Every future003 timed campaign/control must include immutable CGo plus previous-Go attribution and publish absolute mean/p99/CPU and paired Go/CGo tables; split workloads into1–3min campaigns as required.
