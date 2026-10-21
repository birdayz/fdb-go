# RFC-263: Transaction timeout queue and lazy read incarnations

**Status:** Implemented — revision 6 merged in #973 (`ee05cf5b6`).
Revision 3's design was accepted (FDB C++ maintainer
and Torvalds ACK at `2638d7dcf`), but its Phase 1 checkpoint `0a57d49cc` was rejected
for merge on measured CPU and queue contention. Revision 4 received the FDB C++ design
ACK but a code-quality NAK for resurrection/rearm liveness. Revision 5 specifies immutable
same-shard renewal, but its ordinary weak promotion introduces a proven GC-gate stall.
Revision 6 selects a non-promoting canonical-identity check and preserves every retained
future-positive rearm's recovery contract. Both official design reviewers ACKed
`d364c1a5e` before the owner authorized production implementation. The selected code,
20 canonical regression tests and compiled mutation proofs are implemented;
CPU qualification and separate queue-wait diagnostics pass the owner-approved amended
rules. The owner's final race (157/157), full (210/210, including `client_test`),
all five final-HEAD reviews and CI (7/7) passed before merge.
B3a/B3b remain separate later work.
**8-worker p999 blocker: cleared by the preregistered rule (owner decision).**
The complete fixed-population confirmation gives get100: 0.952 [0.902,1.005] and
set100: 1.008 [0.977,1.041], with both 97.5% confidence-interval upper endpoints strictly below 1.05 and all four
A/A intervals containing 1. Get's A/A log SD 0.25–0.27 exceeds the planned 0.15: planned
power was not established, although the interval cleared anyway. Set's epoch ratios
0.965→1.054 include an epoch 2 point above 1.05; order/serial effects are retained.
No post-hoc reinterpretation in either direction is applied. Full results and sealed
provenance are in `docs/benchmarks/rfc-263/rev6-confirm-results.md`; this is not merge
approval or universal tail stability. The historical range non-reproduction and earlier
get/set +9% screen remain separate evidence.
**Item:** TODO.md §0, "Client CPU per transaction"; RFC-261 Part B items B1 and B3, which
that RFC's review sent to their own design review (`perf/rfc-261`, "Review outcome").
**Spec:** FoundationDB 7.3.77, checkout at `${FDB_CPP_SOURCE}`:
`fdbclient/ReadYourWrites.actor.cpp` (RYW), `fdbclient/NativeAPI.actor.cpp` (NAPI),
`fdbclient/ClientKnobs.cpp`, `flow/Net2.actor.cpp`, `flow/FastAlloc.cpp`,
`flow/include/flow/{TaskQueue.h,flow.h,network.h}`. Go line numbers are at `eecd839d9`.

Revision 6 amends revision 4's lazy physical cancellation, lower-bound heap stubs and
one globally ordered drainer without weakening C7. A changed weak association is detected
without promotion and renewed on the next future-positive arm; it is never changed in place. The contract, controlled
finalizer proofs and incremental pricing precede revision 4's retained measurements and
state machine. The later B1 section records revision 3's historical checkpoint;
generation-scoped delivery and the B3 boundaries remain unchanged.

Revision 3 delivers due timeouts as a ready batch in enqueue order, as C++'s task queue
does (C7, B1 `fire`); makes a disarm leave `timeoutArm` alone; and corrects three
validation claims (stale-delivery revert proofs, the removal-cost test, the delivery
allocation pin) and the versionstamp observer tests.

Revision 2 changed the design in three places. A timeout is now armed for the
transaction's current read generation, not for an incarnation object; only delivery
records 1031, so an unused successor can stay unmaterialized without turning deadline
passage into a cause (B3a). The queue has an explicit single-drainer state machine and an
injectable timer (B1). The queue is a separate allocation with no database reference, and
removed heap slots are cleared. The C++ cost comparison, the retention bound and the
savings are restated as hypotheses to measure.

## Revision 6 production checkpoint

The selected immutable same-shard identity renewal, sixteen producer shards and global
C7 drainer are implemented. Whole fast-lane race/opt and exact-restoration runs pass;
removing renewal, eager publication locking, fixed frontier, staggered-exit `again`, or
cleanup registration compiles and triggers each exact canonical regression. The new
20-test source is in the ordinary fast target; its four real-finalizer scenarios and
bounded failure-path harness are retained, not left in disposable scratch code.

Complete populations, actual/cached counts, invalid proof-runner attempt, durable source/
SDK/binary/log hashes and source-preflight scope are recorded in
[`rev6-production-evidence.md`](../docs/benchmarks/rfc-263/rev6-production-evidence.md).
These correctness proofs do not replace the owner's full/full-race and final-HEAD
review gates. The subsequent exclusive production screen and actual
8-worker traces are recorded in
[`rev6-production-tail-results.md`](../docs/benchmarks/rfc-263/rev6-production-tail-results.md);
range's historical all-adverse pattern does not reproduce there, while optional get/set
p999 findings motivated the fixed confirmation below. Neither exploratory observation
cleared the blocker on its own; the later preregistered upper-bound rule did.
The actual production 8w trace has no weak-promotion waits, queue-path sync spans or
arm-path parks; its largest direct tail spans are GRV/reply select waits and runnable
time. R4 has one approximately 1.6 µs queue sync, not broad queue-lock parking. These are
observations from one separately instrumented run per arm, not proof of the historical
cause or of detector cost being absent. There are no get/set traces in that population.
Earlier revision sections below retain their historical pending-approval language;
this checkpoint supersedes those implementation-status claims.

## Preregistered get/set 8w non-inferiority plan — completed

The plan below was fixed in `7bf58d7bf` before collection and is retained rather than
retrofitted to the outcome. Both separately authorized epochs completed 1152/1152 runs
without interim comparisons or block replacement. The owner accepted clearance under
the recorded 5% rule; see [the result](../docs/benchmarks/rfc-263/rev6-confirm-results.md).
The original baseline/master guard returned only documentation and standalone Python
analysis scripts from #970; the owner explicitly ACKed that exception before any sample.
Frozen fa/c2 executables were not rebuilt. Any new measurement still needs separate
authorization; the fixed plan below is not permission to rerun until green.

### Question, identities and fixed population

Test `get100` and `set100` at 8 workers, frozen master
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` versus selected production
`c2e52f46ab02df0295f272f421b87c3bdb3a3515`. Use the already qualified Go 1.26.9 opt
executables, baseline SHA256 `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e`
and production `ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de`.
A moved master, production-code change, rebuilt executable or changed harness requires a
new preregistration before execution; do not silently call a historical revision current
master. Documentation-only commits do not substitute a different production binary.

Preserve the historical client configuration: CPUs 2,3,14,15; GOMAXPROCS 4; GOGC 100;
GOMEMLIMIT=off; empty GODEBUG; timeout 30000 ms; retry limit 10; warmup 2000; unique 68-byte
prefix; same native three-process cluster and frozen harness. Every process measures
**20000 operations**, profile=none. Keep the short-lived process regime: increasing
iterations to improve a single percentile estimate could enable the first 10-second
queue callback and change the phenomenon being confirmed. Instead increase independent
process/block replication. The p999 estimator remains the existing empirical lower order
statistic, zero-based index `floor((N-1)*0.999)`; at N = 20000 only 20 samples lie above it.
Do not pool all samples into a percentile and pretend they are independent trials.

For **each** workload collect:

- **96 master/production paired blocks**: 192 processes, balanced 48 master-first and 48
  production-first. These are the only primary treatment comparisons.
- **96 master/master A/A pairs** and **96 production/production A/A pairs**: 384 control
  processes. Scale each control population to the same 96-pair replication as treatment,
  rather than retaining the earlier 16-pair noise estimate. Both members of each A/A
  pair must have identical binary, source revision, configuration and warmup; only
  opaque run labels/prefixes differ. Estimate noise separately for both implementations
  and report actual interval widths; equal replication does not guarantee equal variance.

Total: **1152 processes / 23,040,000 measured operations**, plus 2,304,000 warmup
operations. Collect **two separately scheduled, balanced quiet epochs**; each contains
48 treatment pairs plus 48 A/A pairs of each binary **per workload**: 576 processes,
11,520,000 measured operations and 1,152,000 warmup operations per epoch. Both epochs
are mandatory even if the first looks decisive. No interim p999 comparison, sample-size
change or optional control dropping is allowed.

Estimated exclusive machine time is **105–115 minutes total**, approximately 52–58 minutes
per epoch; reserve **60 minutes separately for each epoch**, rather than one uninterrupted
slot exceeding an hour. The estimate uses the observed approximately 1-second get and
approximately 3-second set processes, the existing 3-second per-process gate, per-run
integrity/logging overhead, each epoch's 60-second quiet settle and final validation/
sealing. The 1152 per-process gates alone cost 57.6 minutes. Prebuilds happen before the
exclusive epochs; no build time is hidden in the estimate. If preflight forecasting
cannot fit the complete frozen epoch inside its owner deadline, do not start it or
trim controls. Any actual incomplete epoch remains explicit, never a reduced-N pass.

Use schedule seed `263006`; materialize the balanced schedule and freeze its hash before
the first process. Each group of 6 superblocks contains 2 treatment, 2 master-A/A and 2
production-A/A contrasts; balance treatment/alias order within each contrast and use
three get-first/three set-first superblocks. There are 48 groups, 24 per epoch: exactly
96/96/96 contrast pairs per workload overall and 48/48/48 per epoch. Each superblock
contains one get pair and one set pair. Randomize within these constraints using the
frozen seed, not operator preference. This intersperses A/A with treatment under the
same time/cluster conditions. Keep outcomes write-only until both epochs complete;
inspect only technical health, remaining time and success counts during collection.

### Estimand, power assumption and noise-floor control

For each treatment block compute `d = log(p999_production / p999_master)`. The primary
estimand is the mean paired log ratio, reported as `exp(mean(d))`; it is **not** the
previous exploratory ratio of medians. Treat each process pair as the replication unit,
not its 20000 operations. Exclude all exploratory data from confirmatory estimates.

**Non-inferiority planning assumption:** independent paired block log-ratio SD at
most 0.15, approximately normal block means, no unhandled serial/epoch effect, and a true
production/master ratio of 1.00. The clearance margin is `log(1.05) = 0.04879`, not a
9%-versus-zero detection target. Bonferroni adjustment across the two primary workloads
uses two-sided 97.5% intervals for each, giving at least 95% simultaneous coverage. Here
"upper 97.5% bound" means the exponentiated upper endpoint of that specified interval.
The normal approximation for 80% clearance power per workload is
`n = ((z_0.9875 + z_0.80) * 0.15 / log(1.05))^2`, approximately 90 pairs; 96 allow for
the small-sample t critical value. This is approximately 80% power **per workload** at a
true zero effect, not 80% probability that both workloads clear. If the two clearance
outcomes were independent, joint clearance power would be about two-thirds. A real
positive effect below 5%, higher variance or dependence lowers clearance power. These
assumptions and the actual interval widths must be reported; no distribution-free or
joint 80% guarantee is claimed.

Use the equal-weight mean of the two epoch means, with the within-epoch paired variances
and Welch-Satterthwaite degrees of freedom for its 97.5% t interval. Report each epoch
separately, all 96 paired ratios, empirical SD, temporal/order plots and a prespecified
whole-pair bootstrap sensitivity analysis. The t interval is primary; do not choose
whichever analysis produces the preferred outcome. Large skew, serial dependence or
epoch disagreement limits the planned power/interpretation and must be surfaced.

For each of the four A/A populations report all 96 paired log ratios, SD with uncertainty,
mean-ratio interval, empirical absolute-change distribution and epoch/order behavior.
Use 98.75% two-sided A/A mean intervals for a conservative four-control family gate.
An A/A interval excluding 1, or any actual binary/configuration mismatch, prevents a
measurement-qualified clearance claim and requires investigation. Non-significant A/A
means do **not** prove zero bias or equivalence. Compare the observed A/A noise SDs and
realized treatment SDs with the 0.15 planning assumption; if it fails, report the wider
actual intervals and that the intended power was not established. Do not discard noisy
blocks or append runs until a preferred interval appears. A larger follow-up, if needed,
must have a new fixed budget/preregistration before new data are examined.

### Gates and interpretation fixed before execution

1. **Machine/input gate:** prebuild all tools/binaries, stop Bazel actions and test
   containers, then record at least 60 seconds of quiet settling with all sampled loads
   at or below 7.5 before each epoch. Retain process/container/load censuses before/after
   every process; require load at or below 7.5 before launching each next process. Waiting
   for load is recorded, not a reason to drop an already measured block. Any forbidden
   action/container overlap invalidates measurement qualification for that epoch. Retain
   unchanged native-cluster identities/knobs, exact binary/SDK/compiler/harness hashes,
   complete tracked/untracked/config freezes and post-run verification.
   No other timing, build, test or CPU analysis may overlap a measurement epoch.
2. **Population gate:** all 1152 processes, both epochs, prescribed order balance and
   A/A controls must exist; each has exactly 20000 unique measured indices, expected
   workers, zero errors and no clipping, censoring or missing samples. Retain raw CSVs,
   complete histograms and every failed/incomplete attempt. An error or missing block
   is not a passing percentile result; never replace a block by a convenient retry.
3. **Regime/deadline gate:** require each process's conservative lifetime bound below
   the first 10-second queue wake. A breach is retained and invalidates a short-regime
   confirmation claim; do not selectively remove that slow block. Supervise each epoch
   to its owner's absolute deadline. If completion cannot fit, stop with an explicit
   incomplete population, not a reduced-N success. Analysis waits until measurement
   ownership ends and the next owner's quiet slot permits CPU work.
4. **Owner-ACKed statistical/blocker rule:** after the technical, population and
   control gates pass, evaluate each workload's exponentiated mean paired log ratio
   and its upper 97.5% interval endpoint. The p999 blocker clears **only if that upper
   bound is strictly below 1.05 for BOTH get100 and set100**. If a point estimate is
   below 1.05 but its upper bound is at or above 1.05, that workload is **unresolved**;
   non-significance against 1.00 is not a pass. A point estimate **at or above 1.05** in
   either workload is the owner's **regression/design-must-change** decision, even
   if its interval crosses 1.05. Distinguish this engineering decision rule from a
   statistical claim that the true effect is confidently above 5%. No lower-bound test
   or exclusion of 9% substitutes for the required two upper bounds. A changed design/
   executable needs a new preregistration, not repricing under this fixed population.
5. **Scope gate:** CPU, means, p50/p99, allocations, GC and session/order effects remain
   reported safety/context metrics, not alternative primary endpoints selected after
   collection. A favorable median, A/A result or one passing workload cannot clear the
   p999 blocker. Even satisfying both upper-bound conditions does not replace broader
   CPU qualification, full/full-race, final-HEAD owner/review or other merge gates.

## Revision 6: detect the association without promotion

**Selected detector:** with `tx.readErrMu` held and the receiver strong, compare
`weak.Make(tx)` with the current node's immutable `n.tx`. Equal means the same live
association; unequal means a new post-finalization association. In that case retain
revision 5's retirement, same-shard fresh-node/cleanup registration, publication and
normal scheduling protocol below. Never rewrite `n.tx` in place. Zero timeout, sticky
cause and synchronous C3 branches remain ahead of the check. **Every retained
future-positive rearm recovers**, including positive-default Reset; idle/no-rearm
continuity is still the explicitly inherited boundary, not a new promise.

This is the detector-only prototype: add the `weak` import, replace
`n.tx.Value()==nil` with `weak.Make(tx)!=n.tx`, and update its comment. Its constructor
is intentionally unchanged, so a rare renewal does a second Make lookup of the same
new association without a second handle allocation. Node/shard/root/ready sizes,
cleanup authority, ticket/frontier/owner protocol and generation behavior are unchanged.

**Runtime basis, pinned Go 1.26.9.** Finalization sweep unlinks the old weak special
(`runtime/mgcsweep.go:560-584`) and clears its retained handle box
(`runtime/mheap.go:2788-2799`). A subsequent `getWeakHandle` synchronizes with sweep and
searches the current span-specials list (`:2610-2627`); Make creates a new association
(`:2527-2551`) or returns a newer canonical one (`:2577-2598`), never that unlinked old
record. Immutable `n.tx` keeps the old box allocated, so address reuse cannot fake
equality. The public identity guarantee is `weak/pointer.go:26-37`.

Registration avoids the strong-conversion GC gate and referent-shading step, but still
calls `ensureSwept`, takes the span-specials lock and searches its list. It is **not free**.
In six balanced R5/identity/original-baseline blocks at 32 and 8 workers, identity costs
**+1.24% / +1.64% median client CPU versus R5**, although −0.58% / −1.54% versus the
contemporaneous baseline on range. These are program-level costs, not exclusive attribution
of every extra cycle to the lookup. Other workloads still need CPU qualification.

**Causal evidence, not a complete tail fix.** R5's separate trace has 5,671 GC weak-to-strong
parks, all at `armTimeoutLocked`, beginning during GC marking. Every full park plus
subsequent runnable interval lies unambiguously inside its same-worker CSV operation;
clock/epoch bounds are approximately 2 µs. Baseline, R4 and identity traces have zero such
parks. The detector-only intervention improves p99 against R5 in **all six pairs at both
concurrencies**, and p999 in all six 32-worker pairs. This pins the new R5 promotion gate
as a cause, not merely a correlation. Full traces/profiles, bounded alignments, raw hashes
and complete histograms are indexed in `docs/benchmarks/rfc-263/rev5-tail-results.md`.

**Historical scratch gate, before the fixed confirmation:** the 8-worker base-design
residual was a merge blocker. The later owner-accepted confirmation above clears the
8w p999 gate without reinterpreting this older population. In the contemporaneous
fa/R4/R5 batch, R4 itself is adverse against fa in five of six pairs for both p99 and
p999: median p99 1003.796 → 1023.364 µs (+1.95%); p999 1466.032 → 1566.328 µs (+6.84%).
At 32 workers R4 p999 is mixed (two adverse/four favorable, median +0.62%). Thus changing
the detector alone cannot repair the base-design contribution. Identity's separate
8-worker A/B remains adverse against baseline in all six p99/p999 pairs. Its 32-worker
p99 is favorable in five pairs, adverse in one; p999 is mixed. All four 32-worker traces'
slowest 500 operations overlap GC marking, which does **not** identify the residual wait.
At that scratch freeze no 8-worker execution trace had been collected; do not extrapolate
its cause from 32-worker traces or blame the detector without an isolating experiment.
The later production checkpoint's own 8-worker traces are a separate population. Historical R4
8-worker primary tails were mixed and remain separate evidence.

The identity prototype passes all 20 top-level probes plus two subtests in actual race
and opt runs. Removing renewal compiles and makes all four controlled-finalizer scenarios
named-red; exact restoration reruns green. Normal live rearm remains zero allocations.
Frozen source tree `bd0408b1e0c3828e5d587c51f4161b82033586c2`, source archive SHA256
`04258b4d4980f3c773e8d383da8140e47d78f46ee0baa68c3a6b6ba881c9fb59`, executable SHA256
`9067e97f4f9d15535964ca7dc37f7b808189bc65d2c639e09a296ba9f0db1a3e`.
Those scratch preflight ACKs were not official design ACKs or production validation.
The later accepted-design/production checkpoint above records the current gate status.

**Unselected alternative to measure:** prototype dispatch-observed flag renewal with
correctness probes only, preserving the identity candidate as the selected revision 6
design. It would promise recovery only after the drainer observes the old weak-nil arm;
a pre-observation rearm can reuse the dead handle. That is weaker than revision 6 and
must not become the supported contract without new Linus and FDB C++ ACKs. No flag timing
has run. Do not call an atomic flag literally cost-free before measurement.

The next owner-reserved slot, only after the campaign's local analysis and an explicit
ping, is at most 30 minutes: 8-worker traces for fa/R4/identity/flag, then balanced
fa/identity/flag timing at 8 and 32 workers, with separate diagnostic populations and the
same hash/uptime/fast-lock discipline. Source-only hypotheses and trace falsification
criteria are in `docs/benchmarks/rfc-263/rev6-tail-hypotheses.md`.

## Revision 5 (historical promotion detector): renew after resurrection

Revision 4 received the FDB C++ maintainer's design ACK, but the code-quality review
found one liveness blocker: permanent node reuse also permanently reuses its weak handle.
In the pinned **Go 1.26.9** runtime, `weak.Pointer.Value` becomes nil when a finalizer is
queued, before the finalizer can resurrect the object (`src/weak/pointer.go:80-84`). That
old association never becomes live again; a post-resurrection weak identity is distinct
(`:35-37`). A retained resurrected transaction can therefore
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
its worker weakly promotes `versionstampCompletion` before publication (`:129-159`).
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
sees nil. Synchronized retirement guards additionally prevent old identity admission. A collector paused after pop but
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

Fresh pricing ran alone **2026-10-20 12:19:32–12:24:11 UTC**, with Bazel shut down,
before the owner's 17:00 cutoff. There are **42 valid completed runs**, not pooled with
revision 4: three alternating 8-worker pairs of get/set/range against the exact archived
revision 4 binary; another three pairs/workload against original `fa8725f1d`; then three
unprofiled 32-worker original-baseline range pairs. Use 20,000 measured + 2,000 warmup
transactions at 8 workers and 500,000 + 2,000 at 32, unchanged harness/cluster/settings
from the evidence section below. Record `uptime` before/after **every run**, plus argv,
environment, hashes, UTC bounds and load/idle. Load averages span **5.44–7.97**, sampled
idle fractions **0.9727–0.9909**; background services were not claimed absent.

| Incremental 8-worker CPU | Revision 4 µs/tx | Revision 5 µs/tx | Median change | Paired changes |
|---|---:|---:|---:|---|
| get100 | 48.52285 | 47.73655 | −1.62% | −0.06%, −2.36%, −1.62% |
| set100 | 56.14595 | 56.56075 | +0.74% | +2.95%, −2.88%, +0.74% |
| range100 | 95.91125 | 94.93190 | −1.02% | −1.27%, +0.46%, −2.01% |

All incremental arm ranges overlap. This includes the additional ordinary-path weak
promotion, not just the rare renewal branch. Set/range are mixed; the result does **not**
establish that the weak check is free or that revision 5 is faster than revision 4.

| Fresh original-baseline comparison | Baseline µs/tx | Revision 5 µs/tx | Median change | Paired changes |
|---|---:|---:|---:|---|
| get100, 8 workers | 50.31775 | 49.65190 | −1.32% | −2.56%, −0.55%, −2.21% |
| set100, 8 workers | 57.15865 | 56.78540 | −0.65% | −0.47%, −2.85%, −1.46% |
| range100, 8 workers | 95.26725 | 93.83655 | −1.50% | −0.16%, −2.14%, −3.84% |
| range100, 32 workers | 61.312968 | 59.894066 | −2.31% | −1.72%, −1.79%, −2.61% |

All twelve original-baseline pairs favor revision 5 CPU. Get ranges overlap; set/range
ranges in this screen do not. The 32-worker ranges are baseline 60.714448–61.497640,
revision 5 59.670614–60.215792 µs/tx. This is a CPU screen, **not latency approval**:
**Historical Rev5 merge blocker, unresolved at that screen:** 32-worker range median mean latency is
782.144 → 786.766 µs (+0.59%), and median p99 is
**2,311.419 → 2,760.679 µs (+19.44%)**, adverse in all three pairs. Incremental revision
4→5 8-worker range mean/p99 are also adverse in all three pairs (median mean
475.573 → 482.178 µs; p99 950.351 → 1,050.510 µs). Preserve this unfavorable result;
these runs alone do not identify its cause or authorize an end-to-end performance claim.
Revision 4's mutex/GC/retention profiles were not rerun for revision 5. Production still
needs current design approval, its complete correctness/race lanes, current contention
and lifetime measurements, and an evidence-backed explanation/resolution of the latency
blocker. First compare retained revision 4/Phase 1 tails without fresh timing; then run
at least six alternating 32-worker range comparisons across baseline/revision 4/revision 5,
recording p50/p90/p99/p999 and full per-run histograms. Keep timing separate from block,
mutex and execution-trace diagnostics. Hold the owner's fast-validation lock for each
whole timing batch and finish local work before the reserved analysis window.

The in-tree evidence index is `docs/benchmarks/rfc-263/rev5-evidence.md`; scripts are in
`tools/benchcampaign/campaigns/rfc-263/rev5-*.py`. Raw runs/logs/binaries/source archives
remain outside the tree at `/tmp/rfc263-rev5.QgGUbU`, with SHA256 manifests. Frozen
source tree is `3f800737de95d937cd1f12babc60b5588814d781`; source archive SHA256 is
`8e67955510f465f6e3e898afc8d2b975e62ad3ccf18e7e0f7dadfde7787599c4`;
opt executable SHA256 is
`d2b9f82132acd6d4c10d3a694003c0c00094181b90f648d70f38f5b7f18d965e`.
Both race and opt builds actually execute all 20 top-level probes plus two subtests green.
Scratch preflight reviewers ACK the renewal code/proofs; those are not the official
revision 5 design ACKs and do not approve production implementation.

## Revision 4: reduce contention without weakening dispatch order

**Historical revision 4 proposal: variant (b), amended by revisions 5–6 above.** Use 16 stable transaction producer
heaps, one globally ordered drainer, lower-bound wake stubs, and GC-unreachability unlink.
Disarm only changes generation state; a later re-arm reuses an earlier queued stub without
a producer lock. Weak resolution stays outside shard locks. Sample one clock and global
ticket frontier before scanning all shards; sort actual resumptions globally. A cleanup
on the transaction unlinks its node after proven unreachability, never merely after commit.
This replaces B1 below; B3a/B3b remain unimplemented. The selected scratch variant meets
the screening bar, but production implementation still waits for design approval and its
full correctness/validation gates.

The Phase 1 checkpoint is `0a57d49cc`, **not accepted for merge on performance**. The
correctness-reviewed measured tree was `50432211dc325d7854dbdf72b24c986f424ccbd5`;
the checkpoint only corrects allocation-count diagnostics. Against merge-base
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`, three alternating 8-worker pairs per
workload gave these median measured-window client CPU costs:

| Workload | Merge-base µs/tx | Phase 1 µs/tx | Change |
|---|---:|---:|---:|
| get100 | 46.3905 | 47.6435 | +2.70% |
| set100 | 56.44865 | 54.8111 | −2.90% |
| range100 | 95.84595 | 96.6283 | +0.82% |

All three get pairs were adverse (+0.82%, +4.45%, +3.33%); observed ranges overlap.
This is no general CPU win. Long 32-worker runs, including actual 10 s interval dispatch,
reported queue cumulative mutex delay / measured-window client CPU of **5.59%, 7.90%,
4.21%** for get/set/range. That is a wait/CPU ratio, not a fraction of CPU consumed;
concurrent blocked goroutines can accumulate wait simultaneously. It exceeds revision
3's 1% redesign trigger. The numerator includes queue mutex delay attributed to caller and drainer Unlock-holder contexts,
excludes runtime-internal locks, and uses the whole mutex profile over the measured CPU
window as a conservative attribution scope.

The Phase 1 measured allocation saving was **4**, not the original hypothesis of 3:
`CreateTransaction` is 11 → 7 allocations in the opt build. The old first arm costs 6
(timebomb, weak handle, AfterFunc closure, timer, and two AddCleanup allocations), versus
2 for the new node and weak handle. A timeout-zero control removes this saving. The
unchanged harness rejected zero, so that control alone used the same explicitly labelled
one-line validator change in both trees; neither the primary comparison nor production
contains it. Positive-timeout successors remain eager. The measured harness uses `Transact` for
get100/set100/range100 (`workload.go:264`); it uses `ReadTransact` only for read1000/GRV.
Thus the older claim below that get100/range100 use ReadTransact is superseded. The
get/set census finds two WithCancelCause constructions and one Done channel per
transaction: the unwaited context is the eager successor, not evidence to start B3b
before B3a.

### Alternatives and their boundaries

| Option | Queue work and retention | Dispatch/order result | Decision |
|---|---|---|---|
| Keep indexed removal on every disarm/re-arm | Three queue-lock acquisitions for ordinary successful Transact; immediate unlink on disarm, abandoned nodes still wait for GC + dispatch | Phase 1 preserves C7 | Rejected by measured wait and get CPU |
| 16 stable transaction shards, independent timers/drainers | Spreads producer locks; up to 16 runtime timers and heap capacities per DB; node stays on its first shard; abandoned weak nodes still await dispatch | Only per-shard FIFO; globally wrong | Reject, even if a workload is faster |
| Per-P queues | Would require unsupported runtime pinning/hooks to identify a stable P, or migration/handoff machinery; retaining per-P capacity is another cost | Independent dispatch has the same C7 hole | Reject |
| Sharded producers, one coordinated global batch, no cleanup | Stable shards avoid per-P hooks; collect at one clock/ticket frontier and merge resumptions; weak nodes wait for GC, requested wake and dispatch; no hard 10 s reclamation bound | Preserves C7 with the frontier protocol below | Fixes contention but fails range CPU/GC; rejected without reclamation |
| Logical-arm state + lower-bound stub, one heap | One producer queue-lock acquisition on ordinary Transact; tombstones survive until their existing stub is processed; one global sequence atomic | Globally sorted actual resumptions preserve C7 | Corrected v2 still fails the wait target; retain its mechanism inside the sharded candidate |
| Sharded global batch + GC-unreachability unlink | Restores two cleanup allocations per transaction; leaf shard removal after GC/cleanup dispatch, preserving live successors | Same C7 and generation rules; no slot reuse/ABA | Selected (b): all CPU/wait gates pass in the scratch screen |
| Noscan value heap + slab | A pointer-free heap can remove heap-array scanning, but slab state still owns weak handles; capacity and recycling costs need measurement | Recycling needs independent owner identity, not only heap/ready generation guards | Parked (a), unbuilt/untimed, with a finalizer-resurrection owner-alias hazard |
| Pure lazy registration | Zero queue work until later use loses autonomous delivery and pending-arm identity | Violates C3/C4 and can shift C7; horizon staging must register logical state immediately | Reject pure (c); staging is a separate unmeasured representation |
| Immutable descriptor per arm | Simplifies concurrent publication, but adds an allocation and potentially multiple retained descriptors per transaction | Could preserve C7 with a global merge | Reject extra allocation/retention; two reusable slots do not cover arbitrarily rapid re-arms while an old reader is paused |
| Coarse wheel | Cheap bucket append is attractive; bucket/producer/drainer collisions and exact bucket minima still need design | Rounding to e.g. 10 ms silently changes C2's requested wake; bucket FIFO alone does not order unequal overdue wakes | Not selected; an exact-min wheel is a new design, not a free quantization shortcut |

**Why raw sharding is observably wrong.** A is enqueued before B; both are due at one
sampled cutoff, but their independent shard callbacks may record B's 1031 first while
A remains pending. An observer can then see B return 1031 while A's new
`GetVersionstamp` still returns its committed stamp (or 2015 if no stamp is ready).
This getter checks the recorded incarnation cause, not elapsed deadline
(`transaction.go:2175-2205` at the checkpoint), so the existing Go read-entry elapsed
publication does not mask the counterexample. C++ also races `getVersionstamp` with
`resetPromise` (RYW:2515-2520). C7 requires A's due resumption first. Allowing arbitrary
callback delay does not authorize reversing two timers already due in the same cohort.

**Exploratory screen, not an acceptance result.** Three rotating-order blocks, baseline /
lower-bound v1 / independent shards, each 20,000 measured + 2,000 warmup transactions at
8 workers:

| Workload | Baseline µs/tx | Lower-bound v1 µs/tx | 16 shards µs/tx |
|---|---:|---:|---:|
| get100 | 49.06445 | 47.7132 | 48.9026 |
| set100 | 57.3947 | 54.3563 | 55.7040 |
| range100 | 94.3542 | 94.8920 | 96.3462 |

V1 get/set improve in all three pairs; range is mixed (+0.57% median). Shards regress
range in all three pairs (+2.1% median). Long 32-worker queue wait/CPU ratios, classified
by exact queue-unlock source line, are **1.50% / 0.93% / 2.49%** for v1 and
**0.22% / 0.19% / 2.30%** for shards. Neither meets the goal on all three workloads.
The range drainer is still material; simply adding shard locks is not the demonstrated
solution. The lower-bound v1 arm also reserved eager FIFO tickets too early, a later
review finding below; shards independently violate cross-shard C7. These numbers locate
costs rather than validate a usable arm.

The corrected lock-release v2 moves `weak.Value()` outside `q.mu`, releases the lock
after each pop (including dead weak pointers), yields after 16 pops, and separates the
global sequence atomic from the mutex/heap-header cache line. Its fresh post-release
rerun used 18 primary runs plus six long profiled runs (both arms):

| Workload | Baseline µs/tx | Corrected v2 µs/tx | Median change | Paired changes | Queue wait/CPU at 32 |
|---|---:|---:|---:|---|---:|
| get100 | 49.8939 | 48.1390 | −3.52% | −9.91%, −2.24%, +0.78% | 1.952% |
| set100 | 56.8553 | 55.00495 | −3.25% | −3.36%, −3.64%, −3.25% | 0.915% |
| range100 | 96.11355 | 95.4392 | −0.70% | +4.11%, +0.57%, −0.70% | 0.794% |

Range primary CPU is mixed, not a demonstrated win. The one 32-worker profiled range
pair is 60.953688 → 64.101166 µs/tx (+5.16%). Queue mutex delay totals attributed to caller/drainer Unlock-holder contexts are
348.894/268.711 ms (get), 117.871/24.642 ms (set), 197.221/57.130 ms (range). Root's
acceptance requires under 1% on every workload; v2 fails get. This motivates combining
producer sharding with the shorter drainer critical sections, not accepting v2 alone.
Per-run load average was 5.28–7.36, idle fraction 0.899–0.993. Raw rows and all
unfavorable pairs remain included.

### Why fixing contention alone failed

The combined no-cleanup arm passes the mutex criterion but fails CPU:

| Workload, 8 workers | Baseline µs/tx | Combined µs/tx | Median change | Paired changes | 32-worker queue wait/CPU |
|---|---:|---:|---:|---|---:|
| get100 | 49.79425 | 48.89865 | −1.80% | −3.32%, −3.23%, +2.58% | 0.3782% |
| set100 | 57.9325 | 54.89895 | −5.24% | −5.62%, −4.19%, −5.29% | 0.0390% |
| range100 | 94.1853 | 96.58925 | +2.55% | +2.94%, +2.01%, +3.61% | 0.1577% |

Range arm ranges do not overlap at 8 workers. At 32 workers, three predeclared alternating
unprofiled 500,000-transaction range pairs give 59.342842 → 65.34993 µs/tx medians
(**+10.12%**), paired +11.04%, +8.69%, +10.92%, again nonoverlapping arm ranges. The
single profiled range pair also regresses (60.144206 → 66.205292 µs/tx). It is a real
CPU failure, not rescued by low queue wait or four fewer allocations.

**Same-binary attribution, separate from acceptance timing.** A diagnostic 32-worker
range CPU-profile pair, 500,000 measured + 2,000 warmup each, uses the exact archived
baseline and combined executables and the same settings. The profiled harness runs report
60.382224 → 66.3054 µs/tx rusage; `pprof -diff_base` compares the separate sampled
profiles, which total 30.12 → 33.09 CPU-s.
An exclusive stack partition assigns every sample exactly once, in this precedence:
GC mark/assist/sweep → write barriers → timeout/cleanup administration → other allocation
→ remainder. It accounts for all 1,844/2,025 sample records:

| Exclusive group | Baseline CPU-s | Combined CPU-s | Delta CPU-s | Delta µs/tx |
|---|---:|---:|---:|---:|
| GC mark/assist/sweep | 4.00 | 5.84 | +1.84 | +3.68 |
| Mutator write barriers, excluding above | 1.14 | 2.04 | +0.90 | +1.80 |
| Timeout/cleanup administration, excluding above | 1.17 | 0.95 | −0.22 | −0.44 |
| Other allocation stacks | 4.14 | 4.76 | +0.62 | +1.24 |
| Remainder, including unknown stacks | 19.67 | 19.50 | −0.17 | −0.34 |
| Total | 30.12 | 33.09 | +2.97 | +5.94 |

The +5.94 sampled µs/tx agrees with +5.923 rusage. `gcDrain` adds 1.93 s cumulative;
its nested `tryDeferToSpanScan` (+1.70 s), `scanSpan` (+1.32 s) and scanner helpers are
not independent costs to add. Flat changes include `tryDeferToSpanScan` +0.99 s,
`spanClass.sizeclass` +0.67 s and `scanObjectsSmall` +0.45 s. Only 0.02 s of the 0.90 s
barrier increase is on queue stacks; 0.88 s is elsewhere in mutator work. Removing heap
pointer-write barriers alone is not the six-microsecond answer. Retained pointer-bearing
state increases marking work and the period during which range's allocation/copy work
runs with write barriers active. The short 8-worker range failure occurs before the first
10 s interval, so interval callback churn cannot be its sole cause.

**Population and GC confirmation.** A separate diagnostic-only build reads runtime
metrics once per second and samples physical shard lengths under each shard lock. It
never scans/promotes transaction weak pointers. The same generic sampler is used in a
baseline diagnostic build. These builds support population/GC observations, **not CPU
performance claims**. Counts are weak queue entries, not strongly reachable transactions;
totals are sequential per-shard snapshots, and 1 Hz peaks are lower bounds.

| Seconds | Combined created nodes | Physical queued nodes |
|---:|---:|---:|
| 1 | 30,007 | 30,007 |
| 5 | 193,870 | 193,870 |
| 10 | 386,648 | 386,648 |
| 11 | 426,022 | 396,016 |
| 12 | 465,598 | 395,211 |
| 13 | 502,002 | 389,877 |

By 13 s the collector has observed 112,124 nil weak pointers and **zero non-nil ones**.
It retains roughly 10 s of completed transaction history, not 32 in-flight handles.
The sampled peak is about 31.7 MB of 64-byte nodes + 16-byte weak handles, plus 3.80 MB
of heap-pointer payload (475,136 slots). An independent archived-binary heap profile,
forced GC at measurement end, estimates 393k retained nodes and 426k weak handles; those
are sampled estimates, not an exact in-run census. Full transactions are not retained.

Archived-binary `gctrace=1` runs select cycles ending in the measured window using the
harness's before/after NumGC snapshots:

| 32-worker range, 500k | Baseline | Combined without cleanup |
|---|---:|---:|
| GC cycles | 216 | 179 |
| Total GC CPU, including idle marking | 3.568 s | 6.110 s |
| Mean GC CPU/cycle | 16.52 ms | 34.13 ms |
| Largest post-GC live-heap report | 120 MiB | 174 MiB |

Fewer cycles did not mean less GC CPU. The last sampled cumulative diagnostic
available-CPU GC fraction is about 5.6% baseline versus 8.0% combined. Its numerator excludes idle marking, as
`GCCPUFraction` does; total GC CPU includes idle marking because that is still real
process CPU. Gctrace figures are rounded and the first selected cycle may start before
the measured window. RSS high-water fields were flat even at identity, so they provide
no useful retention comparison. Raw traces, profiles and conservation-checked analysis
scripts are retained (`pprof/exclusive-cpu-analysis.*`, `gctrace-analysis.json`,
`population-analysis.json`).

### Selected variant (b): reclaim at proven unreachability

The revision 4 arm adds one GC cleanup registration per transaction object, not per arm,
to the sharded lower-bound/frontier design below. Cleanup unlinks the node by index under
its leaf shard lock. It does not cancel a retained handle or recycle storage into a new
owner. This restores two cleanup allocations; creation is **11 → 9**, rather than the
no-cleanup arm's 11 → 7. The performance objective is CPU, not maximum allocation cuts.

Three fresh alternating primary pairs per workload, same opt toolchain/harness/settings:

| Workload, 8 workers | Baseline µs/tx | Selected µs/tx | Median change | Paired changes | 32-worker queue+root wait/CPU |
|---|---:|---:|---:|---|---:|
| get100 | 50.0313 | 48.3165 | −3.43% | −6.50%, −1.50%, −3.43% | 0.01725% |
| set100 | 57.3486 | 55.13825 | −3.85% | −3.85%, −4.36%, −3.69% | 0.000511% |
| range100 | 95.66025 | 94.1891 | −1.54% | −1.54%, −0.93%, −3.62% | 0.01760% |

All nine primary pairs are favorable. Three alternating unprofiled 32-worker range pairs,
500,000 measured + 2,000 warmup each, give **59.855936 → 58.953094 µs/tx**, −1.51%;
paired −1.07%, −1.51%, −0.70%. Arm ranges overlap (baseline 59.172364–60.04102,
selected 58.760324–59.39971), so this is not a claim beyond the observed screen. The
single mutex-profiled 32-worker range pair is also favorable (60.662518 → 59.122064).
The wait numerator counts producer schedule, cleanup `remove`, shard drainer and root
mutexes at exact Unlock source lines, excluding transaction/runtime locks. No mutex-delay
samples are attributed to drainer Unlock sites in get/set. That valid zero attribution
category does not establish that drainers never waited; completed populations and total
identified queue samples are nonempty. All 30 runs,
including unfavorable spreads elsewhere, remain retained. Selected-run load average is
6.36–8.49 and idle snapshots 0.969–0.993; no overlapping build is accepted.

GC and population validation is separate from these acceptance timings:

| Archived-binary range diagnostics | Baseline | Selected |
|---|---:|---:|
| 8 workers / 20k: GC cycles | 189 | 191 |
| 8 workers: total GC CPU | 0.2694 s | 0.2719 s |
| 8 workers: mean GC CPU/cycle | 1.425 ms | 1.423 ms |
| 32 workers / 500k: GC cycles | 214 | 213 |
| 32 workers: total GC CPU | 3.555 s | 3.470 s |
| 32 workers: mean GC CPU/cycle | 16.613 ms | 16.291 ms |
| 32 workers: largest post-GC live report | 122 MiB | 120 MiB |

The separate diagnostic sampler observes **741–2,646 queued nodes**, not the previous
~396k plateau; heap-pointer capacity is 4,592 rather than 475,136. Peak sampled node +
weak class bytes are 211,680 plus 36,736 bytes of heap-pointer payload. This tracks
in-flight transactions **plus a GC/async-cleanup cohort**, not literally only 32 active
transactions; cleanup provides no immediate-reclamation guarantee. The independent
available-CPU GC fraction over each sampler's first-to-last interval is 5.757% baseline
and 5.718% selected. Total GC CPU counter deltas are 3.386 and 3.396 s—near baseline,
not a claim of a diagnostic CPU win. Per-cycle GC cost is back near baseline at both
worker counts. `memory4-gctrace-analysis.json` and `reclaim-population-analysis.json`
retain the exact interval/rounding/snapshot scope and all sampled curves.

### Rejected ownership/lazy shortcuts and parked noscan option

**Callback return is not an ownership endpoint.** `Database.Transact` passes an actual
`*Transaction` to user code and returns its arbitrary result unchanged after commit
(`database.go:974-1003`); the callback can return or capture the handle. After its due
successor delivery, a new `GetVersionstamp` must see 1031 ahead of the committed stamp
(`transaction.go:2175-2201`). Disarming/recycling merely on successful commit or callback
return changes that result. This preserves the Go post-commit extension, not an assertion
that modern C++ creates that successor. The cleanup uses proven unreachability instead.

**Pure lazy-on-use registration is invalid.** A live idle handle or already blocked
operation still needs autonomous delivery (RYW:1567-1578). Making its first late admission
an expired arm records 1031 synchronously where a pending, undispatched timeout could
still be replaced without a cause (C3/C4, revision 1's rejected boundary). A staging
horizon can be legal only if it immediately retains the original logical deadline,
requested wake, replacement identity and FIFO ticket; promotion is bookkeeping, not a
new arm. Preserve the interval phase and fresh tickets on actual continuations. Such
staging may change representation, but cannot justify zero registration/state.

**Noscan heap/slab (a) is parked, not a measured competitor.** A value heap such as
`{wake, slotIndex, slotGeneration}` can remove scanning of heap-array pointers; slab state
can reduce separate pointer-bearing node objects. Weak handles still need a safe GC root,
and slab/free-list high-water capacity and larger value entries must be counted. Do not
hide weak pointers in uintptrs. The partial source uses 24-byte heap values, 64-byte state
with one weak-handle pointer, and larger immutable ready records. Its 16 top-level probes
were **not executed**; no binary or performance claim exists.

It has an unresolved owner-alias hazard: Go 1.26.9 weak `Value` becomes nil when a finalizer
is queued (`src/weak/pointer.go:80-84`), before an old owner is irretrievably gone. A
finalizer can resurrect A after A's slab slot has been reused for B. A's old interior
`timeoutNode` pointer can then mutate B's state under A's lock; heap/ready generation
checks do not protect that producer access. Safe recycling requires cleanup-authorized
ownership release, or an independent immutable owner identity on the transaction checked
before every access, with a sound renewal/exclusion protocol. A generation read inside
the recycled state alone is insufficient. Go cleanup waits for finalization and another
unreachability (`runtime/mcleanup.go:43-44`), and selected (b) does not recycle nodes.
The partial source archive/patch and hazard are retained, verified byte-for-byte, and its
worktree was removed. Reopen only as a separately reviewed future option if measurements
justify its complexity; do not add it to the selected implementation for speculative gains.

### Proposed lower-bound state

Keep `Transaction.timeoutArmed`, `timeoutArm` and the generation-scoped delivery rules.
They are guarded by `readErrMu` exactly as in Phase 1. Each transaction has one current
weak node, reused across ordinary arms; revision 5 replaces it only after weak-association
clearing, retiring the old node without recycling it. There is no ordinary per-arm
allocation or strong queue → transaction reference. The node stays in the 64-byte class.

- **Physical, `q.mu`:** `wake`, heap `index`, and revision 5's `retired` bit. The heap compares only physical wake.
  `queued` is atomic and says the node is currently in the heap, not that it is armed.
  Every write to `wake` also holds its transaction's `readErrMu`, so an arming caller can
  compare it under that lock without taking `q.mu`. Pop/remove changes `index` and
  `queued`, but does not write `wake`.
- **Logical, `tx.readErrMu`:** absolute `deadline`, requested `desiredWake`, and logical
  `seq`. A scheduled arm has a fresh sequence; disarm does not bump `timeoutArm`.
- **Shard:** one `q.mu`, indexed physical min-heap, and `running/requested/reqAt` timer
  state. A node takes a stable round-robin shard on its first allocation and never migrates;
  no per-P runtime hooks are used. Successor arms use `node.queue`.
- **Root:** one padded global `atomic.Uint64` sequence, an atomic first-node shard selector,
  a callback-only owner mutex with `running/again`, and reusable immutable `ready` records
  `{node, arm, seq}`. Each ready slot is 24 bytes instead of Phase 1's 8-byte pointer.
  Producers never take the root mutex. Clear every vacated heap and ready slot.

For every armed node either a queued stub has `wake <= desiredWake`, or the drainer has
claimed it and must inspect it under `readErrMu`, or it is in the immutable ready cohort.
Physical staleness is only early, never late.

**Arm, holding `readErrMu`:** retain C3's synchronous already-expired path. Otherwise
bump `timeoutArm`, set `timeoutArmed`, compute absolute `deadline` before acquiring the
queue lock, and compute `desiredWake = min(deadline + 100µs, armNow + 10s)`.
If the existing weak association is cleared, first execute revision 5's retirement and
same-shard replacement under the transaction lock; then schedule the fresh current node.

1. If `queued.Load()` and `desiredWake >= wake`, take a fresh atomic sequence and return
   without `q.mu`. The earlier stub will discover this arm. If a concurrent pop follows
   the load, its later transaction-lock handshake sees the updated logical state.
2. Otherwise lock `q.mu`, **then** take the fresh sequence at physical publication;
   insert or fix the node at `desiredWake`, and preserve Phase 1's no-lost-wake request
   protocol. Earlier requested wakes must always take this eager path.

Do not reserve an eager ticket before acquiring the publication lock. The rejected
prototype could pause A after reserving seq 1, publish D(seq 2, wake 2 s), publish
B(seq 3, wake 1 s), deliver B at 1.5 s while A is absent, then publish A and dispatch A, D at
3 s: B, A, D has no consistent C7 enqueue interpretation. The corrected test requires
B, D, A. Reverting only the eager-ticket placement compiles and makes the named
`EAGER_PUBLICATION_FIFO` assertion fail.

**Disarm:** clear `timeoutArmed` under `readErrMu`; no queue lock and no eager unlink.
An old immutable record is invalidated by that flag, or by the next arm/sequence. This
is lazy physical cancellation, not a recorded cause and not a lazy read incarnation.

**Collect, with one sampled cutoff `t` and ticket frontier `F`:** sample both before
scanning any shard. At each shard scan, set `q.running=true` under `q.mu`. Pop every
physical stub with `wake <= t` under that lock, setting `queued=false` and clearing
its heap slot. Release `q.mu` **before**
resolving the weak pointer. If dead, drop it. If live, take `readErrMu` then `q.mu`:

- Require exactly `!retired && index == -1 && timeoutArmed`. A producer's reinserted entry
  (`index >= 0`), an already claimed entry (`index == inReady`), or a disarm is skipped
  without changing that state.
- `desiredWake > t`: repair the lower bound by requeueing at `desiredWake` with the
  **same** logical sequence. This is an old cancelled delay's stub, not an actor
  resumption, so it must not consume a new FIFO ticket.
- Otherwise append immutable `{node, timeoutArm, seq}` to ready and mark `index=inReady`.

Release both locks between nodes; never acquire `readErrMu` while holding `q.mu`.
After collecting all shards, with no shard lock held, sort all ready records globally
by their captured logical sequence. The single drainer owns the ready slice, so sorting needs no producer
lock. For each record, take `readErrMu → q.mu`, and reject unless `!retired`,
`index=inReady`, still armed, and **both** `timeoutArm` and `seq` match. The sequence check also rejects duplicate
records made possible by collection yielding across a producer's re-arm.

For a valid record with `seq > F`, requeue at its logical `desiredWake` with the same
sequence; it belongs to the next cohort, not an actor resumption now. Do this only after
all shard scans, so a due deferred record cannot be popped endlessly during collection.
For `seq <= F`, at that sorted resumption: if `t < deadline`, requeue with a fresh wake and fresh sequence;
else capture the arm, release all locks, and call unchanged `fireTimeout(arm)`. It still
claims and records 1031 together under `readErrMu`, with both stale guards. Clear/truncate
ready and re-examine the heap before exiting. Per-shard request replacement follows the Phase 1 protocol; global ownership is the
additional state machine below.

**Order interval continuations too.** Reachable trace: B arms at 0 (physical wake 10), A
at 2 (wake 12); A re-arms at 3 (desired 13, seq 3), B at 4 (desired 14, seq 4); dispatch
at 15. Physical pop order is B, A, but the actual continuations must run A, B and receive
seq 5,6 in that order. Refreshing tickets while popping, or sorting final deliveries
only, silently violates C7 on their next shared wake. Stub repair alone takes no ticket.

**Clock/cohort boundary:** keep `t` and `F` fixed through collection and its sorted resumptions.
C++ `Net2::now()` returns cached `currentTime` (Net2:172), refreshed at the outer loop
(Net2:1575), before collecting timers (1586) and executing ready tasks (1599-1642).
A physical pause while collecting does not itself advance the C++ actor clock. Sampling
fresh wall/monotonic time for each actor while retaining the old cohort is not a parity
fix. A later cohort samples again and collects all newly due nodes before sorting.
The existing intentional Go `wake <= t` rather than C++ `t + TIME_EPS` (100 ns) remains
explicit; this revision neither widens it nor quantizes wakes into wheel buckets.

### Global frontier and wake ownership

The first-node atomic round-robin selector chooses one of 16 shard queues for the
transaction's lifetime. Each shard has its own lazy runtime timer, but every callback
enters the **same root drainer**. Sixteen timers are wake notifications, not sixteen
independent dispatchers. A producer takes its shard mutex only for first insertion,
earlier-key replacement or reinsertion after a claim; the usual successor reuses a stub.

At each cohort the owner samples `t` and global ticket frontier `F` before scanning any
shard. An eager ticket `<= F` is discoverable because its reservation and insertion
share that shard lock; the collector must acquire that lock. A fast-path ticket is
backed by a queued stub or the collector's later transaction-lock handshake. If a logical
arm changes after the frontier, its old record is withdrawn and its new `seq > F` record
is deferred unchanged until another cohort. Do not refresh `F` after scanning.

Counterexample without the frontier: A and B begin arming while the clock is 0 and pause
before eager publication. At `t=15`, sample `F=0`, scan shard 0, then publish A(seq 1)
into that already-scanned shard and B(seq 2) into a later shard. Collecting and delivering
B now would produce B, A. The frontier defers B; the next cohort sees both and runs A, B.
`TestRev4ShardCohortFrontier` pins this exact race. A second probe places a post-frontier
physical head ahead of eligible work in the same shard: the older logical arm runs in
cohort 1, the late head in cohort 2. Resampling `F` at delivery compiles and makes both
named assertions fail; the correct source is restored before timing.

**Owner state machine:** a callback first clears its own shard's `requested` under its
shard mutex, releases it, then takes `root.mu`. If an owner exists it sets `again=true`
and returns; otherwise it sets `root.running=true` and becomes the owner. The root lock
is never held across shard scans, weak resolution, transaction locks, sorting or delivery.
Only the owner accesses `root.ready`.

The owner collects and processes complete cohorts until no ready records remain. On
exit it visits each shard under that shard mutex, requests its current head if nonempty,
then sets `q.running=false` in that same critical section. A producer before that exit
relies on this request; one after it requests an earlier head itself. Finally, under
`root.mu`, the owner either clears `again` and retains ownership for another complete
scan, or sets `root.running=false`. A callback during staggered shard exit cannot fall
between observing no work and releasing ownership: it atomically marks `again` or becomes
the next owner. No producer takes `root.mu`.

The initial delivery-pause callback probe did not prove `again`: the unconditional inner
rescan already found its work. A later test-only tree added the exact exit proof: pause
a later shard's `timer.request` after shard 0 has cleared `running`, arm due work on
shard 0, invoke its callback, then resume exit. The owner must deliver without another
manual fire. Removing `root.again=true` compiles and makes `STAGGERED_EXIT_LOST_WAKE`
fail. The measured combined tree (12 probes) and later test-only tree (13 probes) are
preserved separately; the selected variant includes that proof among 15 passing probes.

### Lifetime and cost changes

Register one noncapturing cleanup when allocating each node. Ordinary re-arms reuse its
node and registration; revision 5 renewal adds a fresh registration while the old cleanup
argument can still retain its retired node:

```go
n := &timeoutNode{tx: weak.Make(tx), queue: q, index: -1}
runtime.AddCleanup(tx, func(n *timeoutNode) { n.queue.remove(n) }, n)
```

The argument reaches a queue/root and weak transaction only: no database or strong
transaction back-reference. Production hooks must be nil; a test hook capturing the
transaction would invalidate a collection proof. Cleanup takes only the leaf shard lock:
return if retired; otherwise indexed `removeAt` if queued, then `index=-1`,
`queued=false`, with vacated slots cleared.
It records no cause, does not change `timeoutArm`, and makes no timer request. Removing a
head leaves an earlier harmless wake. A successful weak promotion keeps the transaction
strong while the drainer examines it; otherwise its cleared weak handle or withdrawn
index rejects an old batch entry. No node is reused for another owner, so cleanup adds
no slot-reuse ABA.

Disarm/turnover themselves still take no queue lock. Cancelled physical stubs wait for
cleanup or their existing wake; there is at most one queued node per transaction, not
per arm. Repeated later re-arms reuse the earlier key; earlier re-arms lower it eagerly.
A retained post-commit handle keeps its pending successor arm: callback return/commit is
not proof of unreachability. Database Close still does not cancel a live timeout.

Cleanup is asynchronous, not guaranteed to execute before program exit, and does not
establish a latest reclamation bound. Keep weak-nil dispatch as the physical-unlink
fallback. Memory can be reclaimed only after queue/batch references are removed, the
cleanup argument is released, and a later GC runs; a dispatched node can remain rooted
by a delayed cleanup argument. Heap capacity does not shrink on unlink. Measured
populations track in-flight work plus a GC/cleanup cohort, not literally 32 handles.

The combined prototype measures 49 cold construction allocations (root plus 16 shard
queues, timer wrappers and bound callbacks), compared with Phase 1's 3. Actual runtime
timers are lazy, up to another 16 on first use, versus 1. Revision 4's `unsafe.Sizeof` reports root 312,
shard 112, node 64 and ready record 24 bytes on amd64; the corresponding root allocation
class is 320 bytes (revision 5's additional test hook makes root size 320, same class).
Padding is per database, not per transaction. Ready capacity costs
24 bytes per slot and the sum of shard heap capacities costs 8 bytes per slot. Measure
runtime timer/heap allocations, peak capacities and GC costs with the final implementation.
The cleanup restores two per-transaction allocations: creation is 9 rather than the
no-cleanup prototype's 7 (merge-base 11). First-arm bookkeeping therefore budgets 4,
not 2. This deliberately trades two speculative allocation savings for the measured
range CPU/GC improvement. Short primary runs and long interval/GC runs remain separate;
neither allocation counts nor reduced mutex wait establishes complete lifetime cost.

### Current gates (revisions 4–6)

The selected revision 6 production implementation received the design ACKs before
implementation. The statuses below supersede historical prototype-only and
pending-design-approval language; final owner verification and merge are recorded in item 6.

1. **Met in the ordinary fast lane:** lower-bound reuse and logical disarm complete
   while the shard lock is held; lowering a desired key inserts/fixes eagerly and
   advances the timer request.
2. **Met, with independent compiled named-red proofs:** stale-stub repair does not
   consume a sequence, logical continuations retain FIFO order despite physical
   collection order, and eager publication reserves its ticket under the shard lock.
   Separate stub-repair, reverse-logical-order and eager-publication mutants fail
   their exact assertions; exact restoration passes. Fixed-frontier ordering has its
   own compiled named-red proof as well.
3. **Met in recorded fast opt/race proofs and final owner full/race lanes:** pop/rearm,
   requeue/rearm, immutable ready records, batch withdrawal, overlapping callbacks and staggered-exit
   wake handling are pinned. Physical/logical assertions and synchronous C3 coverage
   remain. Generation-delivery barriers have bounded failure-path proof. A separate
   paired failure control shows deferred turnover publication prevents cleanup's
   Cancel from blocking after a fatal mid-turnover wait; the no-defer control records
   that blocked Cancel and repairs it with the same temporary watchdog.
4. **Measured, with retention limitations retained:** node 64 bytes, first arm 4
   allocations, live rearm 0, delivery 1 private error, creation 11 → 9, cold queue 49
   allocations (50 with its first runtime timer). Root/shard/ready sizes and lifecycle
   populations are in the evidence. Repeated resurrection has no fixed bound on old
   cleanup arguments before final owner death, cleanup execution and later GC.
5. **Met under the owner-approved amended CPU rule and separate queue-wait criterion:**
   the [complete results](../docs/benchmarks/rfc-263/rev6-cpu-results.md) retain A1's
   historical **UNRESOLVED** verdict solely on both grv-w8 upper 95% bounds. A2,
   sealed before its first sample, replaces only those two estimates without pooling;
   the other fourteen A1 bounds remain unchanged. All sixteen final CPU upper 95%
   bounds are ≤1.02. Both window B→C and lifecycle E−A show strict improvement
   (upper bounds <1.00) for grv at 8/32 workers and set100 at 8/32 workers. These are
   per-endpoint intervals, not simultaneous 95% coverage or literal neutrality.
   All eight separate production 32w workload/profile queue-delay/CPU cells are <1%,
   maximum 0.008781790%, with shard/root and caller/drainer splits. Mutex is
   Unlock-holder context; block is Lock-waiter context; never add them or call
   cleanup-holder attribution proof that cleanup itself waited. Earlier range 8w
   +1.66% and descriptive confirmation get 8w +1.69% remain ABOVE baseline; this
   amended 2% NI rule does not retrospectively neutralize those screens. Neither
   these gates nor the separate p999 clearance authorizes merge.
6. **Met; merged in #973 (`ee05cf5b6`):** exclusive timing windows, frozen harness and
   source/binary identities, complete raw rows/load records, invalid attempts and whole
   fast-lane validation are recorded. The preregistered 8-worker get/set p999 gate is
   met, with power/epoch/order caveats. The owner's final race passed 157/157 targets;
   full passed 210/210, including `client_test`; all five final-HEAD reviews and CI
   (7/7) passed before merge.
7. **Met within the stated rearm contract:** four real-finalizer weak-nil-dispatch,
   positive-default Reset, popped-before-weak-read and immutable-ready probes establish
   distinct weak identities, same-shard renewal, correct deadline and fresh delivery.
   Removing renewal compiles and makes all four fail. Normal live rearm remains zero
   allocation; fresh-association cost is separate from whole Reset. No idle/no-rearm
   armed-timeout continuity or fixed repeated-resurrection retention bound is claimed.

### Evidence identities

Phase 1 evidence is `/tmp/rfc263-phase1.owjNRR`, including raw rows, source archives,
allocation/retention/mutex profiles, mutations and `evidence-files.sha256`. Revision 4's
screen evidence is `/tmp/rfc263-rev4-screen.2KEIQu`, including patches, full source
archives, input SHA256 manifests, build/probe logs, exact run argv/environment and
load/idle snapshots, `samples.csv`, summaries and profiles. These are local retained
artifacts, not checked-in benchmark binaries.

| Arm | Source identity | Executable SHA256 |
|---|---|---|
| Merge-base | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e` |
| Phase 1 | tree `50432211dc325d7854dbdf72b24c986f424ccbd5` | `56a78c0d7454a0222db4d5f1f1de863cdb0b9d75672a4d741a376d90480a4e3c` |
| Lower-bound v1 | tree `3a77018532f6316a80c8da1b002110fd9861a2dd` | `2fbd500094b10c1d198f1108ae8670fa49057b55af0aab88556dbc3e82387efa` |
| Independent 16 shards | tree `d4ee9ccbe4332b5abc418b1824f1e61105b75cd5` | `7c83ccca43551b8179ab88f79afd237e506f9721ceecbb3fcfd6ad7719a74da0` |
| Corrected one-heap v2 | tree `3f2c701be9d36e1fd5860ce7dc9bfbcea96104b7` | `00b337a70a51601ad8b74529ff6a24a50119673f4bff4b4fdfa216acad72fc97` |
| Combined without cleanup, measured tree (12 probes) | tree `090363b16c2e16e640a9ce3557278aa00252aab7` | `46b2ebfe7f8dc572982ce6725845593a420773f83d5aac73b1c9278c9c330374` |
| Selected GC-unreachability unlink (15 probes) | tree `61d2597e0231dfbedcd898a27c078c3412efaf6a` | `2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95` |

All screens use the same Go 1.26.9 Bazel opt toolchain, pure-Go backend, cluster
`${FDB_PERF_HOME}/cpu-work/fdb4.cluster`, `GOMAXPROCS=4`, affinity `2,3,14,15`,
`GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`, and transaction timeout 30,000 ms. The
8-worker screens use three pairs/blocks with 20,000 measured + 2,000 warmup each; mutex
runs use get 1,000,000, set 400,000, range 500,000 measured at 32 workers. Client CPU is
the harness's getrusage measured-window user+system delta; whole-process CPU is retained
separately. No `perf stat` claim is made. Both combined screens additionally run three
alternating unprofiled range pairs at 32 workers, 500,000 measured + 2,000 warmup each.
Diagnostic CPU/heap profiles, `GODEBUG=gctrace=1` runs, and explicitly instrumented sampler
builds are separate populations and are never pooled into the acceptance timings.

The first lock-release timing attempt was conservatively invalidated in full when the
owner withdrew the quiet window for a local campaign rebuild: 8 completed runs and an
interrupted start remain under `discarded-local-rebuild-attempt1`. A second start was
stopped when the eager-publication sequencing flaw was found; all of that attempt remains
under `discarded-early-sequence-attempt2`. Neither supplies accepted v2 numbers. The
corrected arm is rebuilt and run from scratch only after the owner's explicit release.

## CPU lifecycle amendment and separate follow-up

The [CPU plan amendment A1](../docs/benchmarks/rfc-263/rev6-cpu-plan.md#amendment-a1-lifecycle-fixpoint-2026-10-20) preserves the first invalid CPU epoch and corrects only its CPU-specific lifecycle helper: a completed finalizer/cleanup fixpoint may queue cleanup during forced GC, provided executed=queued at exit and every other lifecycle/resource check passes. Original-fa and production historical counters both demonstrate this; no CPU endpoint look motivated the amendment. The fresh, separately authorized A1 batch used frozen disjoint prefix namespaces, unchanged seeded ordering and byte-identical production code/binary. Its original UNRESOLVED result remains retained; the later [A2 result](../docs/benchmarks/rfc-263/rev6-cpu-results.md) supersedes only the two grv-w8 bounds without pooling, yielding combined CPU PASS and separate queue-wait diagnostic PASS.

**Completed in follow-up:** `tools/clientbench/scripts/run.py:validate_lifecycle` now removes only the entry/exit cleanup-queued equality, retaining the producer's completed 2–16-round fixpoint, pending-queue rejection, counter/GC and all other lifecycle checks. The real e1-r000 lifecycle-only fixture accepts newly discovered cleanup; pending exit work, incomplete drains and >16 rounds remain rejected. The standalone A1 helper stays byte-identical for frozen replay and matches the corrected canonical guard; the original invalid epoch is not reclassified.

## Problem

Master clientbench (Go backend, tmpfs cluster, database timeout 30,000 ms, the harness
default `-transaction-timeout-ms`) allocates **55.8 / 51.8 / 58.0** objects per
transaction on `set100` / `get100` / `range100`. Two sites are the subject here.

**B1. The TIMEOUT timer** (`armTimebombLocked`, `read_incarnation.go:218-237`). The first
arm of each `Transaction` object builds a `timebomb` (`:227`), a weak handle (`:228`), the
callback closure and the `time.Timer` (`time.AfterFunc`, `:229`) and the cleanup's closure
(`runtime.AddCleanup`, `:234`): five sites by code reading; RFC-261's profile at `13580f1b7`
attributed 4 to the function. Every clientbench transaction pays it: `CreateTransaction`
applies the database timeout (`database.go:1032`).

**B3. Eager read incarnations** (`readIncarnationLocked`, `read_incarnation.go:111-121`):
a `readVersionState` when absent, the `readIncarnation`, `context.WithCancelCause`'s
`cancelCtx` and cancel closure, and later the `Done` channel.

- At creation, `SetTimeout`/`SetSizeLimit` admit through `enterState` → `admitState`
  (`transaction_lifetime.go:72`). By code reading every measured workload then waits on
  this incarnation (a `context.Background()` operation is "joined",
  `read_incarnation.go:83-88`, and GRV and storage waits select on it, e.g.
  `read_version_state.go:96`). Construction here is moved, not wasted; Phase 1's census
  checks this.
- After a successful commit, `turnoverToken.finish` builds the successor because the
  timeout is positive (`transaction_lifetime.go:248-251`). The measured benchmark discards
  its handle after `Database.Transact`, so that successor is unused; the public API does
  not prevent other callbacks from retaining the handle. `TestUnusedSuccessorAvoidsFourAllocations`
  (`lazy_successor_unit_test.go:230-259`) pins its cost at exactly 4; #892 made it lazy for
  timeout 0.

**Original hypotheses, superseded by revision 4's measurements:** B1 removes 3 allocations per `Transaction` object (5 → 2)
and adds per-database costs (one queue, one runtime timer, the heap's backing array,
amortized); B3a removes 4 per committed-and-dropped transaction. The measured harness
uses `Transact` for `get100`, `set100` and `range100`, not `ReadTransact`; all three have
an eager successor. The original read-only-harness assumption was incorrect. Allocation cuts alone have not moved
CPU measurably before (RFC-261, #951). Retained heap bytes and queue-lock contention are
new costs. All of this is measured (Measurement plan); allocation-only results are
reported as such.

## C++ behaviour (normative)

- **C1. Deadline.** `resetTimeout` arms `timebomb(options.timeoutInSeconds +
  creationTime, resetPromise)` when the timeout is nonzero (RYW:1576-1579). `creationTime =
  now()` at construction (RYW:1552) and on user `reset()` (RYW:2743), not on `onError`.
  `now()` is the run loop's cached monotonic clock (`Net2.actor.cpp:172, 204, 1575`).
- **C2. Wake policy.** `timebomb` loops `while (now() < endTime) wait(delayUntil(min(endTime
  + 0.0001, now() + TRANSACTION_TIMEOUT_DELAY_INTERVAL)))` and then sends
  `transaction_timed_out` (1031) if `resetPromise` is unset (RYW:1567-1574); the interval
  is 10 s (`ClientKnobs.cpp:94`). The earliest delivery is the deadline. There is no
  guaranteed latest delivery: the wake is a request to the run loop, which runs it when it
  gets to it. A wake chosen by the interval term that lands in `[endTime, endTime + 100 µs)`
  delivers with less than the full slack. Due means `at <= now + TIME_EPS` (100 ns,
  `network.h:139-140`, `TaskQueue.h:76-84`).
- **C3. Expired at arm.** `resetTimeout` with `now() >= endTime` runs the actor to
  completion inside `setOption`: 1031 is sent synchronously and no delay is scheduled.
  This applies only when *arming* an already-expired timeout: `setOption(TIMEOUT)`
  (RYW:2570-2573) and the re-arm in `resetRyow` → `applyPersistentOptions`
  (RYW:2678-2697).
- **C4. Recorded cause.** The first *recorded* cause is sticky: timebomb, `cancel()`
  (RYW:2730-2733) and `resetRyow`'s `transaction_cancelled` (RYW:2726-2727) all test
  `isSet()` first. Elapsed time is not a cause; only the actor's resumption records 1031.
  Timeout-sensitive operations test `resetPromise.isSet()` at entry (e.g. RYW:1583, 1679,
  1700) and race the promise while waiting (RYW:361, 1381). Option replacement does not:
  `setOption(TIMEOUT)` replaces `timeoutActor` without looking at the old deadline
  (RYW:2570-2573, 1576-1578), so a timeout whose deadline has passed but whose actor has not
  resumed is replaced without 1031. A completed future keeps its result.
- **C5. Replacement and reset.** `resetRyow` cancels `timeoutActor` (RYW:2703) and installs
  a new promise (RYW:2700-2701), then re-arms through `applyPersistentOptions`, TIMEOUT last.
  User `reset()` restamps `creationTime` and restores database defaults (RYW:2735-2754).
  Destruction fails the promise with `transaction_cancelled` (RYW:2771-2773) and cancels
  the actor.
- **C6. Cost.** Per arm, the `timebomb` actor state is one allocation (an actor is its
  future's SAV). Each delay the actor actually waits on allocates a `PromiseTask`
  (`Net2.actor.cpp:1856`) and, through `Promise<Void>()`, a separate SAV (`flow.h:1039`);
  that includes every 10 s continuation. An arm that is already expired allocates no delay
  (C3). The timer queue is a `std::priority_queue` over a `std::vector` (`TaskQueue.h:154`)
  whose growth is ordinary allocation. Small objects come from FastAllocator's thread-local
  freelists, whose refill takes a global mutex (`FastAlloc.cpp:576-594`). The queue itself
  is confined to the network thread (`TaskQueue.h:33`) and takes no lock. Cancelling the
  actor leaves its `PromiseTask` queued until dispatch (`Net2.actor.cpp:263-272,
  1851-1863`). The 10 s interval bounds how far ahead that wake was scheduled, not when
  the memory is reclaimed: reclamation also waits for the run loop to dispatch it.
- **C7. Dispatch order.** `processReadyTimers` moves *every* timer with `at <= now +
  TIME_EPS` to the ready queue (`TaskQueue.h:76-84`). The ready queue orders by priority
  and then by the FIFO sequence taken at each `addTimer` (`TaskQueue.h:41-43, 146-148`).
  So same-priority timers that are due in one dispatch run in enqueue order, whatever
  their wake times: A enqueued first with wake 20 and B later with wake 10, dispatched at
  25, run A then B. An interval continuation enqueues again and takes a fresh sequence.
  All timebombs use `TaskPriority::DefaultDelay` (`flow.h:1478-1480`).
- **C8. Commit and its observers.** Modern C++ does not reset after commit: RYW resets only
  below API 410 (RYW:1385-1387, 1405-1412), NativeAPI only below 700 (NAPI:6909-6922). After
  its setup read, an RYW watch waits only on the native watch future (RYW:1301,
  1330-1332), registered by `setupWatches` at the committed version (NAPI:6418-6435);
  `resetPromise` no longer reaches it. The Go post-commit auto-reset
  (`transaction.go:3312-3346`) is a Go extension. Its successor keeps C1's deadline, and
  the eager Go behaviour at the merge-base is the reference for it, judged by C3/C4's rules.

## Design

### Timeout state belongs to the read generation

At the merge-base the timer is armed for an incarnation object (`timebombInc`,
`read_incarnation.go:222`). A lazy successor would then have no armed timer. Constructing
it later and comparing its deadline (`armReadTimeoutLocked`, `:195-198`) turns deadline
passage into a cause, which C4 forbids. Revision 1's counterexample: an unused successor
with deadline d and delivery due at d + 100 µs; at d + 50 µs, `SetTimeout(0)`. Eagerly
(and in C++) the setter replaces a pending timeout and no 1031 is recorded. Revision 1's
lazy admission recorded 1031 first.

Revision 2 separates the two states:

- **Pending:** a positive timeout is armed for the current read generation `tx.readGen`
  from its arm point until delivery, replacement or turnover. That holds whether or not
  the generation's `readIncarnation` exists.
- **Delivered:** the queue's delivery (or a C3 arm, or an existing Go synchronous
  publication point, below) records 1031 into the generation's incarnation, constructing
  it if absent.

Fields of `Transaction`, under `readErrMu`, replacing the four `timebomb*` fields
(`transaction.go:465-471`):

```go
timeoutNode  *timeoutNode // allocated on the first arm, reused for the object's lifetime
timeoutArmed bool         // a pending timeout exists for tx.readGen
timeoutArm   uint64       // bumped by every arm that schedules; a popped node carrying another value is stale
```

`readIncarnation.timer` and `timerGen` (`read_incarnation.go:21-22`) are deleted.

**Arm points** (C3, C5):

- `configureReadTimeout` (`read_incarnation.go:161-187`, from `SetTimeout` and
  `resetFields`): unchanged early return during turnover (`:166-169`); then
  `tx.armTimeoutLocked()`. It no longer constructs an incarnation (`:170-174` deleted).
- `turnoverToken.finish` (`transaction_lifetime.go:243-267`): `tx.armTimeoutLocked()`
  for the published generation, whether or not a successor was captured.

With the timeout 0, `armTimeoutLocked` disarms. Else if the current
incarnation exists and has a cause, it does nothing more: a recorded cause is sticky and
the caller delivers it as today. Else, with `delay := time.Until(tx.deadlineTime())`
(unchanged, `:196`): if `delay <= 0` it is C3, and it records 1031 synchronously into the
current incarnation, constructing it. Otherwise it bumps `timeoutArm`, sets
`timeoutArmed` and schedules the node.

**Construction** (`readIncarnationLocked`) no longer arms and never looks at the deadline
(`:118` deleted). Construction is not an arm point in C++ or in the eager design.

**Disarm points:** turnover (`turnoverLocked`, replacing `stopTimebombLocked` at
`transaction_lifetime.go:219`: the old generation's pending timeout is cancelled, C5);
recording any cause into the current generation's incarnation (`recordReadCauseLocked`
becomes `tx.recordCauseLocked(inc, cause)`, which disarms only when `inc == tx.readLife`,
so a cause recorded into a retired, captured incarnation does not disarm its successor);
`SetTimeout(0)`. A disarm clears `timeoutArmed` without bumping `timeoutArm`. Revision 3
also removed its physical node; revision 4 leaves the stub for cleanup or dispatch and
takes no queue lock at logical disarm. Stale-delivery protection is therefore exactly two checks in
`fireTimeout`. `!timeoutArmed` rejects a delivery popped before a disarm that was not
followed by an arm, including one that resumes while turnover is in progress. `timeoutArm
!= arm` rejects one popped before a re-arm, including `finish`'s re-arm of the successor.
A bump at disarm would be redundant with both, so none is specified.

**Delivery** (`tx.fireTimeout(arm)`, replacing `fireTimebomb` and `fireReadTimeout`,
`read_incarnation.go:265-291`). One critical section under `readErrMu`:

```go
if !tx.timeoutArmed || tx.timeoutArm != arm { return }      // stale: disarmed (incl. mid-turnover) or re-armed
tx.timeoutArmed = false
inc := tx.readLife
if inc == nil { inc = tx.readIncarnationLocked() }           // deliver into the unmaterialized generation
tx.recordCauseLocked(inc, &wire.FDBError{Code: ErrTransactionTimedOut})
pending := tx.detachPendingLocked(inc); cause := inc.cause
// unlock, then: afterTimeoutRecord hook (tests), inc.cancel(cause), retirePending(pending, false)
```

Turnover sets `readReady`, clears `readLife` and disarms in one critical section
(`transaction_lifetime.go:211-227`), so delivery never constructs during turnover. With
claim and record in one critical section, the merge-base's second guard (`fireReadTimeout`'s
`readLife == inc` and `timerGen` checks, `:280`) has no window left to protect and is
deleted. Constructing on delivery costs the 4 allocations only when a live, idle,
unused successor actually times out. Once a transaction is unreachable, GC clears its
weak pointer. Revision 3 unlinked the node when dispatch found it dead; revision 4 also
permits GC cleanup to unlink it sooner. Neither unlink nor GC reachability alone means
memory is already reclaimed: queue/batch and cleanup-argument retention are specified
in revision 4's lifetime section.

**Go's synchronous publication points stay as they are:** `checkTimeout` at read entry
(`transaction.go:2752-2770`) and the `OnError` backoff check (`:2298`) publish an elapsed
deadline without waiting for delivery. This already differs from C4, and B3a neither
widens nor narrows it (Out of scope).

### B1. Revision 3 checkpoint (historical)

```go
// pkg/fdbgo/client/timeout_queue.go

const (
	timeoutSlack    = 100 * time.Microsecond // RYW:1569 endTime + 0.0001
	timeoutInterval = 10 * time.Second       // CLIENT_KNOBS->TRANSACTION_TIMEOUT_DELAY_INTERVAL
)

// timeoutTimer requests one callback to q.fire at or after d from now,
// replacing any earlier request. Callbacks may still overlap: a request made
// while a callback runs can start another (time.Timer.Reset on an AfterFunc timer).
type timeoutTimer interface{ request(d time.Duration) }

// timeoutQueue is allocated on its own and refers to no database or transaction.
// mu is a leaf inside Transaction.readErrMu.
type timeoutQueue struct {
	mu      sync.Mutex
	heap    []*timeoutNode // min-heap on (wake, seq); slots past len are nil
	ready   []*timeoutNode // drainer's due batch, reused; slots past len are nil
	seq     uint64
	running bool          // a drainer owns delivery (single-drainer state machine)
	reqAt   int64         // wake of the outstanding timer request; 0 if none
	timer   timeoutTimer  // realTimer (lazy time.AfterFunc) or a test timer
	now     func() int64  // monotonic ns since process start
	deliver func(*Transaction, uint64) // (*Transaction).fireTimeout; tests substitute a counter
	beforeDeliver func(*timeoutNode)    // test hook: drainer before a delivery, mu released
}

type timeoutNode struct {
	tx    weak.Pointer[Transaction] // immutable
	queue *timeoutQueue             // immutable
	// Guarded by queue.mu.
	deadline, wake int64
	seq   uint64
	index int    // heap position; inReady (-2) while in the drainer's batch; -1 otherwise
	arm   uint64 // Transaction.timeoutArm when scheduled
}
```

`database` gains `timeouts *timeoutQueue`, allocated at open. Transactions without a
`Database` (unit tests) use a package-level queue. The queue holds nodes, nodes hold the
queue and a weak transaction, and the runtime timer's callback holds the queue. Nothing
in the queue reaches the `database`, which revision 1's embedded field would have
retained through interior pointers.

**State machine.** States: *idle* (`running == false`, `reqAt` is the outstanding request
or 0) and *draining* (`running == true`, owned by exactly one `fire` invocation). All
transitions are under `q.mu`.

- `schedule(n, arm, deadline)` (under `readErrMu` → `q.mu`): remove `n` if in the heap
  (a node in the ready batch is withdrawn by the push itself, below); set
  `deadline`, `wake = min(deadline + timeoutSlack, now + timeoutInterval)`, a fresh `seq`,
  `arm`; push. If idle and (`reqAt == 0` or `wake < reqAt`): `timer.request`, `reqAt =
  wake`. While draining it never requests; the drainer does so on exit.
- `remove(n)`: if `index >= 0`, `heap.Remove(&q.heap, n.index)` (by index, no search) and
  clear the vacated slot; if `index == inReady`, withdraw it from the batch; then `index =
  -1`. No timer request; a request left for a removed head wakes once and finds nothing.
- `fire()` (every timer callback): lock. If draining, unlock and return: the drainer
  re-examines the heap before it exits. That covers callbacks scheduled before ownership
  changed and callbacks overlapping after `Reset`. Else set `running = true`, `reqAt = 0`
  and repeat:
  1. Sample `now` once. Pop every node with `wake <= now` into `q.ready`, clearing its heap
     slot and setting `index = inReady`; stop if there are none. Sort the batch by `seq`
     (`slices.SortFunc`, no allocation), C7's ready order.
  2. For each batch entry in order, under `q.mu`: skip it if `index != inReady` (removed
     or rescheduled since collection); else set `index = -1` and
     - if `tx.Value() == nil`, drop it;
     - else if `now < deadline`, it is C2's loop: requeue with a fresh wake and `seq`;
     - else capture `arm`, unlock, run `beforeDeliver`, call `q.deliver(tx, arm)`, lock.
  3. Clear the batch's slots and truncate it.

  On exit: if the heap is nonempty, `timer.request(head.wake - now)` and `reqAt =
  head.wake`; `running = false`; unlock. Batch boundaries depend on when the callback
  runs, as C++'s depend on when the run loop calls `processReadyTimers`.

**Invariants.** (I1) At most one `fire` delivers at a time; within a batch, deliveries
are in enqueue (`seq`) order (C7).
(I2) No lost wake: when `q.mu` is released, either a drainer is running and will
re-examine the heap before returning, or the heap is empty, or a timer request at or
before the head's wake is outstanding. `schedule` while idle lowers the request,
`schedule` while draining relies on the drainer's exit, and `remove`/`pop` only raise the
head. (I3) A node is in at most one of the heap and the batch, and a queued or batched
node belongs to the current arm: every arm and disarm runs `schedule`/`remove` under
`readErrMu`, and both withdraw a batched node. The only stale delivery is one whose arm
was captured before a disarm or re-arm; `fireTimeout`'s two checks reject it. (I4) `q.mu` is
never held while acquiring another lock or delivering; `fireTimeout` is called without it.

**Lock order:** `resetMu` → `readErrMu` → {`nativeCommit.mu`, `timeoutQueue.mu`};
`time.Timer.Reset` under `q.mu` takes only runtime-internal locks. Delivery can re-enter
`schedule` (e.g. a cancellation callback that sets another transaction's timeout) because
the drainer holds no lock while delivering.

**Lifetime and retention.** An abandoned armed transaction is collectable at the next
GC, as at the merge-base (`lease_alloc_unit_test.go:333-355`). Its node (64-byte size
class) and weak handle (16 bytes) are unlinked from the queue at the first wake for the
node after a GC has cleared the weak pointer, plus the callback's scheduling delay; the
memory is reclaimed by a later GC, not at the unlink. While the transaction
is reachable the node is requeued every interval. So the node outlives the transaction by
up to one GC cycle, plus at most `timeoutInterval + timeoutSlack` from its last
(re)schedule, plus dispatch delay. GC timing is not bounded, so neither is that sum. This
mirrors C6, where the `PromiseTask` waits for dispatch. Popped and removed slots are set
to nil, so the backing array retains no node. Strong references are rejected:
`ReadTransact` never resets, so with the 30 s default every read-only transaction would be
retained whole for 30 s. A per-transaction cleanup would cost back the allocation removed.
Expected queue length is (abandonment rate) × (GC interval + up to 10 s); it is measured.

**Database Close** does not change delivery: Close cancels the database context and joins
`db.wg` (`database.go:1169-1189`). Timeouts were never in that set, and a live transaction
armed before Close times out today. Once no live transaction is armed, the drainer finds
an empty heap and makes no request, and the queue becomes collectable with the database.

**Contention.** Per transaction: an arm at creation, plus a disarm and a re-arm at each
turnover, plus a requeue per 10 s of armed life, each one O(log n) heap operation under
`q.mu`. The drainer takes `q.mu` once per popped node. One queue per database was the revision 3 starting point.
Its measured `q.mu` wait exceeded the 1% of client CPU trigger at 32 workers, prompting
revision 4 above. Independent shard drainers would weaken C7 and are rejected; sharded
producers would need a coordinated globally ordered dispatch.

**Timing change.** The merge-base fires at the deadline (`time.AfterFunc(delay)`). B1
requests the wake at `deadline + 100 µs`, or at the interval, as C2 does. The earliest
delivery is still the deadline. There is no latest-delivery guarantee in either design.

**Deleted:** `timebomb`, `armTimebombLocked`, `disarmTimebombLocked`, `stopTimebombLocked`,
`fireTimebomb`, `fireReadTimeout` (`read_incarnation.go:205-291`), the `timebomb*` fields,
`readIncarnation.timer`/`timerGen`, and the `runtime`/`weak` imports of
`read_incarnation.go`.

### B3a. No eager post-commit or turnover incarnation

1. `finish` constructs only a captured successor (`transaction_lifetime.go:249`: `if inc
   != nil || tx.timeoutNs.Load() > 0` becomes `if inc != nil`), and arms the generation in
   either case. A C3 arm (expired budget on `OnError`) still records 1031 synchronously,
   constructing the successor, as C5/C3 do via `applyPersistentOptions`.
2. `beginTurnover`/`startTurnover` (`:187, :205`) do not construct an incarnation only to
   retire it. `turnoverLocked` accepts `old == nil`: nothing to detach, cancel or drain; it
   still bumps `readGen`, clears `readErr`, installs `readReady` and disarms. With
   `expected != nil` and no current incarnation, `beginTurnover` returns its existing
   mismatch error.

**Boundary semantics (decided):** a pending timeout replaced or disabled before delivery
records nothing (C4). The merge-base already behaves this way for an existing incarnation,
and B3a now gives it to an unmaterialized one. A delivered timeout is sticky. Construction
is not delivery.

**Savings accounting.** The unused post-commit successor's 4 allocations are removed. Its
timeout arm remains (a zero-allocation reschedule after B1). `Reset()` (`transaction.go:
1977`, `reset` at `:3353`) turns over without admission, so on an unmaterialized successor
it also saves 4. `OnError` admits first (`opContextWithLease`, `transaction.go:2322`), which
constructs the successor; no saving there.

**Observers of an unused successor** (each a regression in the executable spec):

| Observer | Binding at the merge-base | Effect of the successor's pending or delivered timeout |
|---|---|---|
| Production `PendingGet` | built with the operation's context (`transaction.go:933`); `ResolveOrAbort` takes `op.inc` (`pending_get.go:32-36`); turnover detaches and retires it (`transaction_lifetime.go:212, 235-239`) | none; resolves to its retired/memoized outcome; does not construct the successor |
| `PendingGet` without an operation | fallback constructs the current generation (`pending_get.go:34-39`); `PendingGet{` occurs once in non-test code (`rg 'PendingGet\{' --glob '!*_test.go'`), at `transaction.go:933`, always with an operation | unreachable in production; unchanged |
| Committed watch | `WatchSetup` captures context, version, tenant and activation (`readpath.go:1409`, `transaction.go:2185-2198`, `transaction_lifetime.go:280-298`); `AwaitWatchCommit`/`WatchPoll` use only those (`transaction.go:2151-2165`, `readpath.go:1487-1559`, no admission); post-commit reset does not cancel watches (`transaction.go:3323-3346`) | none: the watch context is not the incarnation's (C8) |
| Old versionstamp future | obtained before commit; the completion is bound to its incarnation and retained as `lastVersionstamp` (`commit_input.go:101-103`, `versionstamp.go:153-161`) | none: resolved after the successor's delivery, it returns the committed stamp |
| New `GetVersionstamp` call | admits the successor; `stateGetVersionstamp` checks the admitted incarnation's cause before `lastVersionstamp` (`transaction.go:2225-2235`) | pending timeout: the committed stamp; delivered: 1031, as at the merge-base with an eager successor |
| Native commit | registered on its incarnation (`native_commit.go:50-70`) | none; it completed before publication |
| First admission | constructs the successor | sees 1031 iff delivered; a pending timeout keeps running |

### B3b. Lazy incarnation context (conditional, separate implementation review)

For incarnations that are admitted but never waited on, the `cancelCtx` and closure could
be deferred. On the three benchmark workloads every incarnation is expected to be waited
on, so B3b runs only if Phase 1's census shows `readIncarnationLocked`'s `WithCancelCause`
without a matching `Done` channel. Its implementation gets its own review before merge.

- `readIncarnation` keeps `ctxv atomic.Value` (a `context.Context` stored once) and
  `cancel context.CancelCauseFunc` (nil until created), both written under `readErrMu`.
  `contextLocked` builds the context, cancels it with the recorded cause if there is one,
  and only then stores it and `cancel`. A context is never visible uncancelled after a
  cause exists. A fresh context has no children or `AfterFunc`s, so cancelling it under
  `readErrMu` delivers nothing else.
- Readers: `readOperation.Done/Err/Value` (`read_incarnation.go:83-108`) and
  `opContextWithLease`'s `AfterFunc` (`:377`, which uses `inc.cause` instead of
  `context.Cause(inc.ctx)`). `rg 'inc\.ctx' pkg/fdbgo/client --glob '!*_test.go'` finds
  only `:85, :92, :106, :377`.
- Cancel sites become cancel-if-created, capturing `cancel` in the same critical section
  that records or observes the cause: `read_incarnation.go:154, 184, 367, 382`, the new
  delivery, `transaction_lifetime.go:234, 265`.
- Every mutable `cause` access must hold `readErrMu`. At `eecd839d9` the accesses outside
  `read_incarnation.go`/`transaction_lifetime.go` are `versionstamp.go:160, 224`,
  `transaction.go:676, 942, 2228, 2399`, `range_issue.go:121`, `pending_get.go:140` and
  `native_commit.go:55`. Each was read and holds the lock; the implementation review
  re-audits them.
- A custom `context.Context` incarnation remains rejected: derived contexts would lose
  `context.Cause`, and without an `AfterFunc` method each would start a goroutine.

## Interaction between B1 and B3

- B3a depends on the generation-scoped timeout (above). That change is part of Phase 1:
  with eager construction still in place it changes no behaviour.
- A benchmark-discarded transaction keeps its successor's logical arm until it becomes
  unreachable. Revision 4 physically unlinks through cleanup or weak-nil dispatch, while
  the merge-base stops its runtime timer through cleanup. The retention measurement covers `set100` as well as `get100`.
- Delivery into an unmaterialized generation constructs it (4 allocations), only for a
  live handle whose successor times out unused.
- With B3b, delivery into an incarnation without a context records the cause and cancels
  nothing; a later `Done()` returns a context created already cancelled.

## Executable spec

The queue under test is built with a manual `timeoutTimer` that records requests and
never calls back. Tests call `q.fire()` themselves, including concurrently, so no real
callback races them. Removal by index (`heap.Remove(&q.heap, n.index)`, no lookup) is
a review-checklist item checked by inspection; the comparison test counts heap work only.
Transaction-level tests attach such a queue to a test `database`
(or to the `Database`-less fallback through a test-only setter). The clock is a test
function. Races are sequenced with hooks:
`beforeDeliver` (drainer, `q.mu` released), `afterTimeoutRecord` (after delivery's critical
section, before cancellation), the existing `beforeReadFailureDelivery`, and for B3b
`beforeIncarnationContext`. Allocation pins join the `lease_alloc_unit_test` target; new
files are declared with `just gazelle`. Each test names the mutation that must turn it red.
The revert proof is that mutation applied, compiled, and the named assertion failing,
recorded in the PR.

### B1 queue

| Test | Asserts | Revert proof |
|---|---|---|
| `TestTimeoutQueueWake` | wake = min(deadline + 100 µs, now + 10 s); an interval wake in `[deadline, deadline+100 µs)` delivers; `now < deadline` requeues; never delivers before the deadline | slack removed; interval removed; `<=` for `<` on the deadline |
| `TestTimeoutQueueFIFO` | equal wakes deliver in scheduling order; a requeued node goes behind one scheduled earlier for the same wake (C7) | `seq` not refreshed on requeue (dropping `seq` from the heap's `less` is not a failing mutation: the ready batch is sorted by `seq`, which `TestTimeoutQueueBatchOrder` pins) |
| `TestTimeoutQueueBatchOrder` | A scheduled first with wake 20, B later with wake 10, `fire()` at 25: A then B (C7's example); B alone at 15: B | batch not sorted by `seq` (heap order) |
| `TestTimeoutQueueBatchWithdrawal` | while the drainer is paused in `beforeDeliver` on X, disarm batched Y and reschedule batched Z: Y is neither delivered nor requeued, Z is in the heap exactly once and delivered at its new wake | `index != inReady` skip removed |
| `TestTimeoutQueueHeapInvariant` | random schedule/reschedule/remove of 10,000 nodes keeps heap order, `index` and nil slots past `len` | `index` not updated in swap; slot not cleared |
| `TestTimeoutQueueSiftComparisons` | a counting `less` (test field) shows ≤ 2⌈log₂n⌉ + 2 heap comparisons per push and remove for n up to 65,536 | re-heapify (`heap.Init`) after each change |
| `TestTimeoutQueueSingleDrainer` | drainer A paused in `beforeDeliver` on X; schedule Y due; call `q.fire()` as B: B returns without delivering; resume A: A delivers X, collects and delivers Y, and on exit requests the head's wake | `running` check removed |
| `TestTimeoutQueueStaleCallback` | `fire()` with nothing due delivers nothing and requests the head's wake | exit path skips the request |
| `TestTimeoutQueueScheduleDuringDelivery` | while A delivers, schedule a future Z: no request is made during draining; after A's exit the outstanding request is Z's wake | `schedule` requests while draining (double request) or the exit omits it (lost wake) |
| `TestTimeoutPopThenRearm` | pop X, pause; `SetTimeout` re-arms; resume: no 1031; the re-arm delivers later | `fireTimeout` ignores `arm` |
| `TestTimeoutPopThenReset/mid-turnover` | X claimed for delivery, pause; begin turnover; resume while `readReady` is set: nothing recorded, no successor constructed | `fireTimeout` ignores `timeoutArmed` |
| `TestTimeoutPopThenReset/after-publication` | X claimed, pause; `Reset` completes and `finish` re-arms; resume: successor cause nil, its arm intact and later delivered | `fireTimeout` ignores `timeoutArm` |
| `TestTimeoutRecordThenReset` | pause at `afterTimeoutRecord`; `Reset`; old context cancelled with 1031, successor clean and armed | turnover overwrites the recorded cause |
| `TestTimeoutPopThenCancel` / `TestCancelAfterTimeout` | first recorded cause wins (1025, resp. 1031) | `recordCauseLocked` overwrites |
| `TestCauseOnRetiredIncarnationKeepsSuccessorArm`; `TestCauseOnCleanRetiredIncarnationGuardSynthetic` | real retirement already records a sticky cause, so its control keeps the successor armed; a synthetic clean-retired state separately pins the identity guard | unconditional disarm makes the **synthetic** `SYNTHETIC_RETIRED_GUARD` assertion red; removing only that guard is not end-to-end reachable after real retirement |
| `TestAbandonedTransactionTimeoutDoesNotRetainIt` (rewrites `lease_alloc_unit_test.go:333`) | an armed dropped transaction is collected before its deadline; the next `fire` after GC drops its node | node holds `*Transaction` |
| `TestTimeoutQueueDoesNotRetainDatabase` | a database and its transactions dropped while armed are collected; the queue too once drained | queue embedded in `database`; node holds `*database` |
| `TestTimeoutAfterDatabaseClose` | a live transaction armed before `Close` gets 1031 at its deadline | `Close` stops the queue |
| `TestTimeoutQueueConcurrent` | 32 goroutines arming, re-arming and disarming plus concurrent `fire()` calls, under `-race` in the CI race lane; invariants checked after | `q.mu` released inside `schedule` |
| Existing `TestTimebombCallbackIdentity`, `TestTimebombAdmittedCallbackLosesToRearm`, `TestLiveTransactionTimebombFires` (`lease_alloc_unit_test.go:357-470`) | same properties over the queue (renamed) | as today |

### Generation-scoped timeout and B3a

| Test | Asserts | Revert proof |
|---|---|---|
| `TestPendingTimeoutDisabledBeforeDelivery` | unused successor, deadline passed, wake not run; `SetTimeout(0)`; `fire()`; a Get succeeds, no 1031 | construction compares the deadline |
| `TestPendingTimeoutExtendedBeforeDelivery` | as above with `SetTimeout(2×)`: no 1031 until the new deadline, then 1031 | same |
| `TestDeliveredTimeoutIsSticky` (control) | `fire()` delivers into the unmaterialized successor; then `SetTimeout(0)`; Get fails 1031 | delivery skips construction |
| `TestExpiredArmIsSynchronous` (C3) | `SetTimeout` to an already-expired budget, and `OnError` past the budget: 1031 without `fire()` | C3 branch removed |
| `TestLazySuccessorPublication` (updated, `lazy_successor_unit_test.go:98-228`) | commit, user reset and retry × capture none/before/after × timeout zero/future/expired: unused successors are absent; captured successors exist; the generation is armed for future, recorded for expired | `finish` restores the `timeoutNs > 0` branch |
| `TestTurnoverWithoutIncarnation` | `Reset` on an unmaterialized successor constructs none; `readGen` advances; a stale `expected` is refused | turnover constructs `old` |
| `TestLazySuccessorPendingGet` (checkpoint `TestTimeoutSuccessorPendingGet`) | successful Commit has already drained and memoized the pipelined Get; later resolution keeps that old outcome after successor delivery | poison the settled memo; the later `op.inc` selection branch is bypassed and is not a reachable post-successful-Commit revert proof |
| `TestLazySuccessorHeldWatch` (sim, real FDB) | watch set before commit, held across the successor's deadline and delivery: still fires on change with no error | watch context derived from the incarnation |
| `TestLazySuccessorFirstAdmissionAfterWatch` | after that delivery, the first successor admission gets 1031 and the watch is unaffected | — (pins both bindings together) |
| `TestLazySuccessorOldVersionstampFuture` | a stamp future obtained before commit, resolved after the successor's delivery, returns the committed stamp | delivery selects `lastVersionstamp` with the cause |
| `TestLazySuccessorNewGetVersionstamp` | a new `GetVersionstamp` returns the committed stamp while the successor's timeout is pending and 1031 after its delivery | delivery skips construction (stamp returned instead of 1031) |
| `TestLazySuccessorBlockedOpTimesOut` (sim) | after commit, a GRV held by the intercept is interrupted with 1031 at the deadline | first admission does not see the pending arm |

### B3b (if run)

`TestLazyContextDoneBeforeCancel`, `TestLazyContextCancelBeforeDone` (later `Done()` is
closed, `Err` is `Canceled`, `context.Cause` gives 1031), `TestLazyContextPreCancelledOnPublish`
(pause in `beforeIncarnationContext` after a cause is recorded; no observer sees the
context uncancelled), `TestLazyContextResetBetweenCreateAndCancel`,
`TestLazyContextConcurrentDone` (one channel). Each with the matching revert.

### Allocation pins

These record the historical Phase 1 checkpoint. Revision 4's selected cleanup variant
changes first-arm bookkeeping from 2 to 4 and transaction creation from 7 to 9; its own
gates above supersede those two pins. The private-error and warmed-bookkeeping rules stay.

`testing.AllocsPerRun`:

- first arm of a fresh transaction ≤ 2;
- queue bookkeeping, with `q.deliver` replaced by a counting function: re-arm, disarm,
  requeue, batch collection and sorting, a stale `fire`, and a `fire` that delivers: 0
  allocations (the batch and heap slices at steady-state capacity);
- real delivery (`fireTimeout` into an existing incarnation) is measured separately and
  pinned at its measured count. It allocates at least the 1031 error, a fresh
  `*wire.FDBError` that escapes into the incarnation; sharing a mutable exported error
  value was rejected in #966 and is not used to reach zero;
- `TestUnusedSuccessorAvoidsFourAllocations` extended to a positive timeout: exactly 4
  between discarded and materialized, and the discarded path's arm allocates nothing;
- `CreateTransaction` with a database timeout: the measured merge-base/candidate counts
  are 11 → 7; retain the observed difference of 4, not the original hypothesis of 3;
- B3b: an admitted, never-waited incarnation allocates no `cancelCtx`.

### Timing against C++

The differentials `TestSetTimeout_CPort` and `TestDatabaseLevelTimeout_CPort`
(`c_binding_port_test.go:1330, 3157`) and `TestSetTimeout_OverallBudget`,
`_ResetRestartsTimer`, `_Disabled`, `_CommitCheck` (`:1469-1608`) pass unchanged.
`TestTimeoutNoEarlierThanDeadline` (real timer, 50 ms, held GRV) asserts only the lower
bound; the wake values are pinned by `TestTimeoutQueueWake`.

## Original measurement plan (revision 3)

Trees: the merge-base and each phase's head, same toolchain (recorded), the Go
`clientbench` built with Bazel per `tools/clientbench/README.md`, the three-process tmpfs
cluster, sequential runs under comparable load, `GOMAXPROCS=8`, client on CPUs 0-7.
Binaries' hashes, commands and data go under `docs/benchmarks/rfc-263/`.

1. **Allocations:** `set100`, `get100`, `range100`, 8 workers, 7,000 transactions,
   `GODEBUG=memprofilerate=1`; allocations and bytes per transaction, attributed by site.
   Control: `-transaction-timeout-ms 0`, where B1's saving must vanish. Per-database
   costs are reported separately: queue, timer, peak heap capacity in bytes.
2. **CPU:** `perf stat` task-clock per transaction, median of five; 8 and 32 closed-loop
   workers for all three workloads, paced `set100` 2,000/s with 64 workers; RFC-260's
   iteration counts.
3. **Contention:** mutex profile for `timeoutQueue.mu` at 8 and 32 workers, split into
   callers (schedule/remove) and the drainer.
4. **Retention and GC:** `get100` and `set100` at 32 workers with the 30 s timeout: queue
   length over time, live heap (`/memory/classes/heap/objects:bytes`), GC CPU
   (`/cpu/classes/gc/total:cpu-seconds`), against the merge-base.
5. **Delivery latency** (informational; no bound is claimed): deadline-to-delivery
   distribution for a synthetic 1 s-timeout population at 32 workers.
6. **Census:** Phase 1's profile attribution of `WithCancelCause` and `Done` channels in
   `readIncarnationLocked` decides B3b.

## Phases

Phase 1 is checkpointed for correctness, not accepted for merge on performance. The
next implementation, only after revision 6's design approval, replaces its B1 queue with
the selected contention design and reruns its gates. B3a/B3b remain separate later phases.

1. **B1 and the generation-scoped timeout.** The queue with its ready batch, `armTimeoutLocked`/`fireTimeout`,
   construction no longer arms, deletion of `timebomb`; queue and generation tests, pins,
   contention and retention runs. No behaviour change except the wake slack.
2. **B3a.** `finish` and turnover; the boundary and observer regressions; allocation and
   CPU runs.
3. **B3b**, only if Phase 1's census finds a population, after its own implementation
   review.

## Out of scope

- Go's elapsed-deadline publication at read entry and in `OnError` (`transaction.go:2298,
  2752-2770`), which C4 does not have. It is existing behaviour, left unchanged here, and
  belongs in TODO.md as a separate divergence item.
- The monotonic deadline (C1: Go converts a wall-clock `deadlineNs` once per arm) and the
  interval's BUGGIFY value.
- Implementing the unselected sharding/wheel alternatives, node pooling, embedding the
  creation incarnation, and a custom `context.Context` incarnation.

## Questions for review

Revision 6: does the canonical-identity detector preserve every retained rearm's recovery
without promotion, in-place weak mutation, stale-work interference or hidden retention?
Are the controlled finalizer/revert proofs and source/runtime identity arguments sufficient?
The GC-gate cause is pinned. The historical 8-worker finding did not reproduce in the
quiet production range screen; the fixed get/set confirmation cleared the 8w p999
blocker by the owner's preregistered rule, with power/epoch/order caveats retained.
Revision 4's shard-publication/frontier and wake handshakes
remain unchanged. Current design ACKs do not replace production validation. The retained
revision 3 questions concern the unchanged generation/B3 design:

1. Is constructing an unmaterialized successor at delivery (4 allocations for a live idle
   handle that times out) preferable to a separate per-generation cause slot that every
   cause reader would have to consult?
2. Should the elapsed-deadline publication at read entry (Out of scope) be filed as a C4
   divergence, or is it an accepted Go behaviour?

## Review resolution index

| Point | Resolution |
|---|---|
| C++ 1, Torvalds 3: B3a turned deadline passage into a cause | "Timeout state belongs to the read generation"; B3a "Boundary semantics"; tests `TestPendingTimeoutDisabledBeforeDelivery`, `…ExtendedBeforeDelivery`, control `TestDeliveredTimeoutIsSticky`; C3 limited to arming (C3, `TestExpiredArmIsSynchronous`) |
| C++ 2, Torvalds 1: no single drainer; overlapping callbacks | B1 "State machine", invariants I1-I2; `TestTimeoutQueueSingleDrainer`, `…StaleCallback`, `…ScheduleDuringDelivery` |
| C++ 2: refresh `seq` on requeue | B1 `fire` requeue; C7; `TestTimeoutQueueFIFO` |
| C++ audit C2: no latest delivery; partial slack at interval boundary | C2; B1 "Timing change"; `TestTimeoutQueueWake` |
| C++ audit C4: first recorded cause; elapsed time is not a cause; qualify "every operation" | C4; Out of scope (Go read-entry publication) |
| C++ audit C6: retention until dispatch; 10 s bounds wake distance, not reclamation; Go retention components | C6; B1 "Lifetime and retention"; Measurement 4 |
| C++ addendum C6: PromiseTask + SAV per delay incl. continuations; none for expired arm; vector growth; FastAllocator refill lock | C6 |
| C++ audit C7: FIFO among equal-priority due tasks, fresh seq | C7; `TestTimeoutQueueFIFO` |
| C++ audit C8: no modern auto-reset; watches on native future; pending gets keep their incarnation | C8; B3a observer table; `TestLazySuccessorHeldWatch`, `…FirstAdmissionAfterWatch`, `…PendingGet` |
| C++ audit: sharding needs evidence and explicit ordering relaxation | B1 "Contention" |
| C++ audit B3b: pre-cancel before publishing; synchronize every cause access | B3b bullets 1 and 4; `TestLazyContextPreCancelledOnPublish` |
| Torvalds 2: embedded queue retains the database; clear slots; retention must include GC and scheduling delay | B1 types (`*timeoutQueue`, no back-reference), `remove`/`fire` clear slots, "Lifetime and retention"; `TestTimeoutQueueDoesNotRetainDatabase`, `…HeapInvariant` |
| Torvalds 4: controllable tests | Executable spec preamble (manual `timeoutTimer`, `q.fire()` by tests) |
| Torvalds 4: masked `TestTimeoutPopThenReset` mutation | delivery claim and record are one critical section, inner guard deleted ("Delivery"); `TestTimeoutPopThenReset` (outer guard) and `TestTimeoutRecordThenReset` (post-record window) |
| Torvalds 4: heap invariants do not show O(log n) | `TestTimeoutQueueHeapInvariant` (renamed) and `TestTimeoutQueueSiftComparisons` (revision 3) |
| Torvalds: observer table with regressions | B3a observer table and its tests |
| Torvalds: OnError admission builds an incarnation | B3a "Savings accounting" |
| Torvalds: savings are hypotheses; queue init, retained bytes, callback contention | Problem "Hypotheses"; Measurement 1, 3, 4 |
| Torvalds: B3b conditional with its own implementation review | B3b heading and Phase 3 |
| Rev 2, Torvalds 1: `TestTimeoutPopThenReset`'s mutation masked by `finish`'s bump and `!timeoutArmed` | disarm no longer bumps (Disarm points: two checks, the bump classified redundant); `TestTimeoutPopThenReset/mid-turnover` (mutation: ignore `timeoutArmed`) and `/after-publication` (mutation: ignore `timeoutArm`) |
| Rev 2, Torvalds 2: counting `less` cannot detect a linear lookup | test narrowed to heap comparisons (`TestTimeoutQueueSiftComparisons`, mutation: re-heapify); index-based removal is specified in `remove` and checked by inspection (Executable spec preamble) |
| Rev 2, Torvalds 3: zero-allocation real delivery contradicts the escaping `*wire.FDBError` | Allocation pins: zero for queue bookkeeping with a counting `deliver`; real delivery measured and pinned at its count; no shared mutable error (#966) |
| Rev 2, C++ 4: old versionstamp future vs new `GetVersionstamp` after 1031 | observer table split into two rows; `TestLazySuccessorOldVersionstampFuture`, `TestLazySuccessorNewGetVersionstamp` |
| Rev 2, C++ 5: delivery order differs from C++ for unequal overdue wakes | matched: ready batch sorted by enqueue `seq` (C7, B1 `fire` steps 1-3, I1); `TestTimeoutQueueBatchOrder` with the reviewer's example, `TestTimeoutQueueBatchWithdrawal` |
