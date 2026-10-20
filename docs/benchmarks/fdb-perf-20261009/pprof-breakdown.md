# batchset50 CPU breakdown: Go (old, affef937) vs CGo (libfdb_c 7.3.77)

Workload: 50 distinct 100-byte Sets plus Commit per transaction, 1 worker, GOMAXPROCS 8.

## Sources

- **Timing (R3):** 6 counterbalanced blocks of 3000 transactions, unprofiled. Archive `fdb-perf-20261009/old-batch-r3`.

  | | CPU µs/tx | user | sys |
  |---|---:|---:|---:|
  | Go | 905 | 515 | 391 |
  | CGo | 847 | 475 | 381 |
  | **gap** | **+58** | **+40** | **+10** |

  Paired ratio old/CGo is 1.073. Go's CPU is higher in all 6 blocks.
- **Go buckets:** R2 CPU profile of the same binary (`old-batch-attribution-r2`): 231 samples at 100 Hz over 3000 transactions.
  - Every sample goes into exactly one bucket by its stack (`/tmp/perf-lead/classify2.py`).
  - Samples whose leaf frame is in the kernel (syscall or futex) are scaled to R3 sys (391). The rest are scaled to R3 user (515).
  - One sample is worth about 3.6 µs (user) or 4.4 µs (sys). A bucket of n samples is uncertain by about ±√n samples.
- **Syscalls:** R3 `strace -f -c`, whole process divided by 3000. This includes fixture setup and warmup.
- **Allocations:** R2 heap profile with fixture stacks removed, normalized to the exact measured 80,404 B/tx and 299.6 allocs/tx.
- **CGo buckets** need `perf record` on libfdb_c. That is requested as R4; until it arrives, the CGo column has only the syscall census and Go-wrapper GC.

## STEP 1: Go CPU per transaction by bucket (sums to R3 Go = 905 µs)

| bucket | samples | user µs/tx | sys µs/tx | total µs/tx | Go-only, or does CGo pay it too? |
|---|---:|---:|---:|---:|---|
| syscall: socket write (GRV + commit frames over loopback) | 38 | 4 | 164 | 168 | Both. Go 2.34 `write`/tx vs CGo 2.18 `sendmsg`/tx |
| sched: wake idle thread (futex wake) | 24 | 0 | 107 | 107 | Go **16.3 futex/tx vs CGo 7.4** |
| **GC: mark / sweep** (54 % idle mark workers on idle Ps) | 28 | 101 | 0 | **101** | **Go-only.** 0.0028 GC/tx vs CGo's Go wrapper 0.0002 |
| **allocation: mallocgc** (memclr, heap bits) | 22 | 79 | 0 | **79** | Go. C++ uses arenas |
| transaction lifecycle / leases / GRV client / commit glue | 18 | 65 | 0 | 65 | partly |
| sched: idle thread sleep (futex wait) | 14 | 11 | 49 | 60 | partly |
| **wire encode: `keyRangeEqualsKeyAfter` byte loop** | 13 | 47 | 0 | **47** | **Go-only.** C++ `equalsKeyAfter` uses memcmp |
| sched: goroutine spawn + stack growth (`go grvBatcher.flush`) | 12 | 29 | 18 | 47 | Go-only |
| sched: run queue / chan / select / spinning | 12 | 40 | 4 | 44 | Go. CGo has 15.9 `epoll_wait`/tx instead |
| commit capture / coalesce / validate | 10 | 36 | 0 | 36 | partly |
| wire encode: other (marshal, frame, xxh3, bufio) | 10 | 36 | 0 | 36 | both |
| syscall: socket read (3 reads + 1 EAGAIN per frame, unbuffered) | 7 | 4 | 27 | 30 | Go 8.96 reads/tx vs CGo 4.65 |
| sched: netpoll (epoll) | 6 | 4 | 22 | 26 | both |
| conflict ranges (add / coalesce / finalize) | 7 | 25 | 0 | 25 | partly |
| Set mutation log (`stateSet` / `ownMutationLocked`) | 4 | 14 | 0 | 14 | partly |
| read-your-writes map (`rywCache.set` CPU; its allocations are in the GC/alloc rows) | 3 | 11 | 0 | 11 | partly |
| reply decode | 2 | 7 | 0 | 7 | both |
| harness | 1 | 4 | 0 | 4 | both |
| **total** | 231 | **515** | **391** | **905** | CGo: 475 / 381 / 847 |

### How this accounts for the +58 µs gap

**sys, +10 µs.** Go makes fewer syscalls than CGo per transaction (about 47 vs 63), but its mix is more expensive.

| syscall per tx | Go | CGo | difference |
|---|---:|---:|---:|
| read (Go unbuffered `FrameReader`) | 8.96 | 4.65 | Go +4.3 |
| futex (goroutine handoffs) | 16.3 | 7.4 | Go +8.9 |
| epoll_wait (network thread) | 4.45 | 15.9 | CGo +11.4 |
| rt_sigprocmask (cgo transitions) | — | 13.5 | CGo only |
| epoll_ctl | — | 2.4 | CGo only |
| mprotect | — | 1.1 | CGo only |
| nanosleep | 15.2 | 15.1 | equal |

Futex wakes are the expensive calls because they trigger cross-CPU wake-ups. Go has about 8 goroutine wake-ups per transaction:
- GRV: caller → spawned flush goroutine → writeLoop → flush goroutine → readLoop → flush goroutine → caller.
- Commit: caller → writeLoop → caller → readLoop → caller.

**user, +40 µs.** The Go-only user costs that can be named add up to far more than the gap:

| Go-only user cost | µs/tx |
|---|---:|
| GC | 101 |
| mallocgc | 79 |
| `keyRangeEqualsKeyAfter` byte loop | 47 |
| goroutine spawn, stack growth, run-queue spinning | ~70 |

So libfdb_c must spend user time of its own that Go does not: C++ arena work, actor scheduling, and cgo transitions (13.5 sigprocmask/tx). R4 (`perf record` of both binaries) will put numbers on CGo's side.

`perf stat` instruction counts are almost equal: 1.187 M (Go) vs 1.182 M (CGo) instructions per transaction, whole process. The perf task-clock does not reconcile with rusage, so I am not using it.

**VM speed.** On my box the same 150 × 88-byte `keyRangeEqualsKeyAfter` calls take 8.9 µs; the VM profile puts them at 47 µs. Every profile share therefore costs about 5× more absolute µs on the VM than locally. The shares are what carry over.

## Top 10 allocation sites (per transaction; 80,404 B and 299.6 allocs in total)

| # | site | B/tx | % bytes | allocs/tx | % objects | what it is |
|---|---|---:|---:|---:|---:|---|
| 1 | `client.(*Transaction).captureCommit` | 24,061 | 29.9 | 4.7 | 1.6 | full re-copy of every operand and conflict bound into a fresh arena at commit, plus new vectors |
| 2 | `client.(*rywCache).set` | 18,281 | 22.7 | 85.0 | 28.4 | per-Set value copy, `string(key)` map key, map growth |
| 3 | `client.(*Transaction).addWriteConflictForKeyLocked` | 8,799 | 10.9 | 5.9 | 2.0 | `writeConflicts` vector doubling |
| 4 | `client.(*Transaction).stateSet` | 8,632 | 10.7 | 6.9 | 2.3 | `mutations` vector doubling |
| 5 | `client.coalesceWriteConflicts` | 5,349 | 6.7 | 1.9 | 0.6 | copy and sort of the conflict vector |
| 6 | `client.coalescePlainSetMutations` | 2,758 | 3.4 | 0.9 | 0.3 | output mutation vector |
| 7 | `client.(*Transaction).enterState` | 2,244 | 2.8 | 90.6 | 30.2 | heap `executionLease` on every public call |
| 8 | `client.finalizeShipConflicts` | 2,242 | 2.8 | 0.8 | 0.3 | conflict vector copied and coalesced again with the self-conflict range |
| 9 | `main.transaction.Set` (facade `fdb.Key` boxed into an interface) | 1,032 | 1.3 | 41.8 | 14.0 | harness/API boxing; the CGo binding pays the same |
| 10 | `client.(*Transaction).opContext` | 696 | 0.9 | 6.6 | 2.2 | per-operation context |

Inside the client, each 100-byte Set copies its key 4 times and its value 3 times:
- the mutation log;
- the conflict range's begin and end;
- the read-your-writes value and its map key.

At commit, all of it is copied again (#1), and the conflict vectors are copied twice more (#5, #8).

## STEP 2: the biggest removable bucket and the change that removes it

**The largest Go-only bucket is GC + allocation: 180 µs/tx** of Go's 515 µs user. The single change that removes the most of it is site #1.

**Change.** `pkg/fdbgo/client/commit_input.go:captureCommit`: stop copying the operands into a fresh arena.
- Instead, the `commitInput` takes a reference on the transaction's pooled conflict slab (`conflictBuf` owner). Mutation keys and values and the conflict bounds already live in that slab, and it is append-only.
- The slab returns to its `sync.Pool` only when the last owner releases it. The owners are the commit's input and the transaction's reset or turnover:
  - `transaction.go` `postCommitResetFields`;
  - `reset`: a retry reuses the slab in place only if it is the sole owner;
  - `Cancel`.
- This is the same last-owner pattern as `transport/writeCompletion`.
- What stays copied: the small Mutation and KeyRange vectors, and anything not stored in the slab (empty operands, the self-conflict range).

This keeps the reason the copy exists today: a concurrent Reset or Cancel while the commit RPC is in flight still cannot overwrite bytes the commit is shipping. Bytes on the wire are identical.

**Expected saving: about −55 µs/tx (range −40 to −70).**
- About −36 µs from captureCommit's own CPU: 10 of its 13 samples are the copy loop, memclr and memmove.
- About −23 µs of GC, because the 18 KB/tx arena is 23 % of allocated bytes.
- Net gap to CGo: about +3 µs, i.e. at parity.

**Cheaper alternative, which could go first as its own PR:** `wire/types/keyrangeref_custom.go:keyRangeEqualsKeyAfter`.
- Replace the byte loop with `bytes.Equal(begin, end[:len(begin)])`.
- Compute the result once per range in `writeToBuffer`; it is currently evaluated 3 times per range across size precompute and the write.
- Wire-identical: same predicate, same output. Existing encoder golden tests cover it.
- Measured locally: 8.9 → 1.75 µs per transaction (5×). On the VM that is about **−38 µs/tx** (range −27 to −48), roughly two thirds of the gap, at near-zero risk.

Next, if approved: a buffered `readLoop` (−4.3 read syscalls/tx), then fewer GRV/write handoffs (−9 futex/tx).

## Addendum: Go vs CGo side by side (R4 `perf record`, both binaries)

**Method.**
- `perf record -F 1999 -g` (frame pointers) on the old Go binary and on the CGo binary: batchset50, 3000 transactions, 1 worker.
- Samples are period-weighted. Stacks from the fixture and validation phases are excluded (about 4.8% of each profile).
- Each arm's total is scaled to its R3 CPU: Go 905, CGo 847 µs/tx.
- Classifier: `/tmp/perf-lead/r4bucket.py`.
- Whole-process scope; throttling was reported, so treat these as shares, not exact µs.

**Kernel time (µs/tx):**

| bucket | Go | CGo | Go−CGo |
|---|---:|---:|---:|
| futex (thread wake/sleep) | 106 | 40 | **+66** |
| socket send | 101 | 66 | +35 |
| nanosleep | 66 | 40 | +26 |
| socket recv | 29 | 15 | +14 |
| scheduler/irq | 22 | 9 | +13 |
| epoll | 39 | 75 | −36 |
| memory (faults/mmap/mprotect) | 43 | 132 | −90 |

**User time (µs/tx):**

| Go | µs | CGo | µs |
|---|---:|---|---:|
| scheduler / chan / sync | 136 | network loop + actor scheduling | 67 |
| | | Go runtime + cgo boundary | 46 |
| GC | 80 | FastAllocator / Arena / malloc | 100 |
| mallocgc | 65 | | |
| serialize / frame / checksum | 63 | serialize | 22 |
| | | memcpy / StringRef | 29 |
| client logic | 104 | NativeAPI logic | 20 |
| | | other native | 93 |
| RYW write map | 27 | RYW write map (PTree) | 46 |

**Conclusion.** Go's excess is in cross-thread wake-ups: +66 futex, +26 nanosleep (spinning Ms), and 136 µs of user-side scheduler work against CGo's 113. GC and malloc together cost about 45 µs more than C++'s arena allocator. That is the reasoning behind the follow-up PRs:

| PR | change | targets |
|---|---|---|
| #816 | buffered reads | read syscalls |
| PR-4 | lone GRV runs inline on its caller | one spawn and two hand-offs per GRV |
| PR-5 | direct socket write when the writer is idle | two hand-offs per send |
| #818 | borrowed commit slab | 23% of bytes per transaction |
| #812 | memcmp in `keyRangeEqualsKeyAfter` | encode CPU |
