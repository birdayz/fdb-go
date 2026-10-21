# Historical pre-pricing Rev5 review draft

This is the scratch preflight draft reviewed before pricing, retained as history. The
canonical current contract, results and **open merge-blocking range-tail regression** are
in `rfcs/263-transaction-timer-and-incarnation.md` and `rev5-evidence.md`. This draft is
not design approval or performance acceptance.

## Revision 5: renew a cleared weak association after resurrection

Revision 4 received the FDB C++ maintainer's design ACK, but the code-quality review
found one liveness blocker: permanent node reuse also permanently reuses its weak handle.
In the pinned **Go 1.26.9** runtime, `weak.Pointer.Value` becomes nil when a finalizer is
queued, before the finalizer can resurrect the object (`src/weak/pointer.go:80-84`). That
old association never becomes live again. A retained resurrected transaction can therefore
re-arm a revision 4 node that every later dispatcher will discard. This is a Phase 1 /
revision 4 permanent-reuse regression, not a consequence unique to adding cleanup.

### Supported contract and inherited boundary

Caller-installed finalizers are not excluded from the supported retained-handle surface.
After resurrection, every subsequent **future-positive arm** must work: `SetTimeout`, a
user `Reset` restoring a positive database default, and other paths through
`armTimeoutLocked` use a fresh association when the old one is cleared. Renewal itself
must not reset creation time, extend the deadline, clear a recorded cause, or bypass C3's
synchronous already-expired path. User `Reset` still intentionally restarts creation time
and restores database options; that is Reset's existing C1 behavior, not renewal's.
C++ replaces the pending timebomb at `ReadYourWrites.actor.cpp:1576-1578,2570-2572`;
the actor checks the creation-based deadline before waiting at `:1567-1573`.

This is **rearm liveness**, not a claim of automatic continuity for an already-armed idle
timeout across finalization. At merge-base `fa8725f1d`, the old AfterFunc also holds one
weak handle (`read_incarnation.go:228-231`), so a weak-nil callback cannot record the old
pending timeout after resurrection. That idle/no-rearm loss is inherited. The baseline
can recover on a later arm **after the old timer has fired**: `Stop()==false` clears the
old timebomb (`:241-248`), and the next arm creates a fresh weak handle (`:218-234`).
Before the timer fires, baseline Reset/rearm can instead reuse its dead handle; recovery
is conditional. Permanent node reuse removed even the after-fire recovery. Revision 5
restores subsequent-arm liveness without falsely claiming to repair idle continuity.

The repository does not itself install a transaction finalizer. An independent census at
`6585b2919` covers **169 production Go files** under `pkg/fdbgo`, including **26 facade
files** under `pkg/fdbgo/fdb`: zero `SetFinalizer` and zero `go:linkname` occurrences.
The sole production `AddCleanup` is `native_commit.go:68`, which cancels a native commit;
its worker weakly promotes `versionstampCompletion` before publication (`:133-159`).
Neither that cleanup nor the facade resurrects a transaction. This explains why ordinary
use does not trigger renewal, but does not justify excluding caller resurrection.

### Immutable same-shard renewal protocol

Keep every node's weak association immutable. Do not replace `n.tx` in place while a
collector can resolve it outside the shard lock.

1. Hold `tx.readErrMu` and a strong receiver through detection, retirement, replacement
   and publication. Keep the existing zero-timeout, sticky-cause and synchronous C3
   branches ahead of node renewal. For an actual future-positive arm with an existing
   node, check `n.tx.Value()` **outside the shard mutex**.
2. If it is nil, lock the old node's shard. Set `old.retired=true`; indexed-remove it if
   physically queued; set `index=-1` and `queued=false`, also withdrawing `inReady`.
   Do not rewrite the old weak handle, logical sequence, or ownership. The `retired` bit
   is read/written only under this shard mutex and fits the node's existing padding.
3. Release the shard mutex **before** allocation or cleanup registration. Allocate a new
   node with `weak.Make(tx)` and a new noncapturing cleanup bound to that new node.
   Explicitly retain `old.queue`: do not consume another round-robin shard selection.
4. Publish `tx.timeoutNode=fresh` under `readErrMu`, then use normal scheduling for the
   already-selected arm/deadline and its normal fresh publication ticket. The fresh node
   starts unqueued, so it takes the eager insertion path. There is no extra actor
   resumption or extra ticket for retirement.
5. Cleanup takes only its node's leaf shard mutex. A retired node is a no-op. Collection
   and immutable-ready processing reject `retired` under that same mutex. Old cleanup
   arguments and old ready records never address the replacement node.

Lock order remains transaction → shard; neither retirement nor cleanup takes the root
owner mutex. Retirement leaves a possibly earlier harmless requested wake rather than
requesting/cancelling timers. Normal live-node arms reuse their association and retain the
lower-bound fast path, but now pay one additional `weak.Value()` check.

**Concurrency argument.** A collector that successfully promotes the old association
keeps the receiver strong while it examines it. Finalization cannot queue while that
strong receiver remains needed. After it releases the receiver, finalization can clear
the old association even if an immutable ready record still exists. Renewal then retires
and withdraws that exact old identity under the leaf lock; the later ready resolution
sees nil, or its synchronized identity guard rejects it. A collector paused after pop but
before weak resolution likewise sees the permanently nil old handle after renewal. No
reader races an in-place weak write, and neither stale work nor eventual old cleanup can
unlink the fresh arm. The existing arm/generation/cause checks remain unchanged.

### Controlled probes, mutation and costs

The disposable scratch has **20 top-level probes plus two collected-node subtests**
passing under Bazel's race build, with actual finalizer execution and controlled strong
retention rather than a forged nil weak pointer:

- `TestRev5ResurrectionAfterWeakNilDispatch`: release the last strong keeper, run GC,
  receive the caller finalizer's resurrected pointer, establish the old weak identity is
  permanently nil, and dispatch/drop that old physical node. Assert the inherited idle
  cause remains nil; public `SetTimeout` must renew and later deliver 1031 through public
  `GetVersionstamp`.
- `TestRev5ResurrectionReset`: the same real finalizer boundary, then public `Reset`
  with a positive database default. Require Reset's creation-time restart and restored
  30 s budget, a distinct live node/weak identity on the old shard, and actual delivery.
- `TestRev5ResurrectionRenewalAgainstCollectedNode`: pause first after pop but before
  weak resolution; separately pause with the old immutable ready record present and
  `index=inReady`. Finalize/resurrect/rearm before releasing the drainer. In both cases
  the fresh arm survives stale work and delivers. Invoke the old cleanup **body directly**
  as an explicitly synthetic additional check; real old cleanup waits for the retained
  receiver to become unreachable again.

All renewal probes require distinct node **and weak identity**, unchanged shard, old
weak identity still nil, old retirement/withdrawal, and fresh-arm delivery. SetTimeout
also requires an unchanged creation-based deadline. Removing the entire renewal branch
compiles; all **four resurrection scenarios** fail the named
`RESURRECTION_NODE_NOT_RENEWED` assertion. Restoring the exact source reruns all probes
green. The mutation patch, input hashes, nonzero status and raw red/green logs are kept.

The ordinary creation pin remains **9 allocations** (merge-base 11); live existing-node
rearm is **0 allocations**. A synthetic fresh-association cost probe measures **5 minus
1 fresh Transaction carrier = 4 allocations**: node, fresh weak handle, and two cleanup
registration allocations. This prices the helper used for rare renewal; it is not a
claim that a full public Reset costs four allocations. An earlier invalid repeated-owner
probe measured 3: after warmup, `weak.Make` reused that live owner's current weak handle.
Its source and failing output are retained, not counted as resurrection cost evidence.
Node/shard/ready sizes remain **64/112/24 bytes**; the extra after-pop test hook makes root
`unsafe.Sizeof` 320 rather than 312, still the same 320-byte allocation class. Cold root
construction remains 49 allocations, with up to 16 additional lazy runtime timers.

One node/cleanup per transaction is only the **ordinary non-resurrection case**. Each
renewal registers another cleanup on the same owner. The old cleanup argument can retain
its retired 64-byte node and weak handle until final post-finalization unreachability,
cleanup dispatch and later GC (`runtime/mcleanup.go:43-44`), even though it was physically
unlinked already. Repeated resurrection can accumulate these old registrations; there
is no fixed one-node lifetime bound or hard reclamation deadline. Nodes are never recycled
into another owner, and cleanup still takes no authority from callback return or commit.

### Revision 5 evidence and performance qualification

Pricing was pending when this historical draft was reviewed. See the current RFC and
`rev5-evidence.md` for the completed 42-run screen and its then-unresolved range-tail
blocker; [the later fixed confirmation](rev6-confirm-results.md) records current gate status.
