# RFC-263 revision 6: unresolved 8-worker tail hypotheses

**Historical status: merge blocker; source-only hypotheses, not attribution.** The
later [fixed confirmation](rev6-confirm-results.md) cleared the 8w p999 blocker under
the owner's recorded rule; it does not retroactively attribute the earlier residual.
Revision 6
selects the identity detector and preserves every retained future-positive rearm
across resurrection. The weaker flag detector is a diagnostic/review control,
not an approved alternative contract. No performance run or production change
was made for this note; source-reading commands ran at nice 15.

Review input from ROOT #4355: fresh baseline/R4 comparisons have adverse 8-worker
p99 and p99.9 in five of six pairs, while 32-worker p99.9 is mixed. The later
identity A/B still has adverse 8-worker tails against its contemporaneous
baseline. These are separate populations, not a paired R4-versus-identity
comparison. See `rev5-tail-results.md` for frozen identities and campaign scope.

**Historical freeze: no 8-worker execution trace had yet been captured.** The later
production checkpoint has its own exclusive 8-worker traces and paired timing in
[rev6-production-tail-results.md](rev6-production-tail-results.md); those results do not
retroactively attribute this earlier residual. The existing four-arm
32-worker/500k traces establish the R5 promotion-gate mechanism and its removal,
but cannot diagnose this 8-worker/20k residual. GC overlap in the 32-worker traces
must not be presented as an 8-worker observation.

## Source constraints before hypotheses

Source references below use R4 tree
`61d2597e0231dfbedcd898a27c078c3412efaf6a`, identity tree
`bd0408b1e0c3828e5d587c51f4161b82033586c2`, original baseline
`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`, and the pinned Go 1.26.9 SDK.
Queue references are `pkg/fdbgo/client/timeout_queue.go`; benchmark references
are under `tools/clientbench/`.

### Existing process bounds exclude queue callbacks in the 8-worker timing runs

- R4's slack is 100 µs and interval is 10 s (`timeout_queue.go:14–17`). Each
  future arm sets `desiredWake=min(deadline+slack, now+10s)` (`:109–115`). With the
  configured 30 s transaction budget and young transactions, first wakes are
  approximately ten seconds **after scheduling**, not after database creation.
- `monoStart` only supplies a common monotonic coordinate (`:36–38`); queues are
  constructed without armed timers (`:86–94`). An old process/DB clock does not
  make a newly installed `now+10s` wake immediately due. Transaction creation
  records a fresh `time.Now()` (`database.go:1029–1041`).
- A shard requests a timer only when not draining and either no request exists
  or the inserted wake precedes `reqAt` (`timeout_queue.go:130–133`). A normal
  later successor uses the lower-bound fast path. Cleanup `remove` does **not**
  cancel or replace the requested timer (`:136–143`). There is no per-arm wake
  or per-arm `Timer.Reset` in that ordinary path. The timer wrapper uses
  AfterFunc once, Reset only for later requests (`:28–33`).
- `Database.Transact` creates a transaction per invocation (`database.go:974–975`).
  Round-robin first-node selection (`timeout_queue.go:98–104`) means the 2,000
  successful warmups have already exercised all 16 shard timers before measured
  work, provided the pre-first-callback conditions above hold. Cold queue
  construction and the 16 lazy timer installations are not recurring measured
  first-arm costs.
- Metadata/executable hashing precedes opening the client (`cmd/clientbench/main.go:97–113`).
  Backend open sets the positive default before `Run`
  (`cmd/clientbench/backend_go.go:41–54`). `Run` then prepares the entire fixture,
  seeds and validates it, performs warmup, and only then profiles/measures
  (`internal/bench/run.go:379–415`). Seed is one transaction; seed validation is
  another (`internal/bench/workload.go:137–159`). **The queue is not reset between
  seed, warmup and measurement.**

The measured 20k window alone would not prove zero callbacks. A stronger existing
negative control is available: each `runs/*w8*.command.json` records
`start_utc` before launching a **new process** and `end_utc` after it exits. The
first queue schedule cannot precede exec. A lightweight scan of the existing
metadata checked all 36 eight-worker invocations: workers 8, iterations 20k,
timeout 30000 ms, profile none, successful return. Their recorded whole-process
spans are only 1–2 s; adding 1 s uncertainty for second-resolution timestamp
rounding gives a conservative upper bound of **3 s, still below 10 s**.

| Existing eight-worker population | Runs | Maximum recorded process span | With +1 s timestamp uncertainty |
|---|---:|---:|---:|
| Baseline (both batches) | 12 | 2 s | 3 s |
| R4 | 6 | 2 s | 3 s |
| R5 (both batches) | 12 | 2 s | 3 s |
| Identity | 6 | 2 s | 3 s |

Subject to that declared timestamp bound, **ordinary queue callbacks cannot have
executed anywhere in these processes**, including seed, validation, warmup and
measurement. No Seed-start marker or new trace is needed for this negative
control. Therefore drainer scans/frontiers/sorting/yields and callback-driven
Reset cannot explain these eight-worker timing tails. The baseline uses its
actual remaining 30 s deadline, not R4's ten-second scan cadence
(`read_incarnation.go:195–200,218–234`). Future traced invocations must be checked
afresh: tracing/setup can extend them across ten seconds. Preserve that exposure
difference rather than silently treating them as the same population.

### Worker count is confounded with fixture size and GC cadence

The current budgets change **both** workers and iterations: 8/20k versus
32/500k. `Prepare` allocates one 100-byte payload per requested measurement even
for range100 (`internal/bench/workload.go:50–69`), and the measurement closure
continues indexing `d.Measure[i]` (`internal/bench/run.go:415`). `runPhase` also
allocates the full `[]Sample` before measurement (`:130–131`). These fixture and
result-array populations differ by roughly 25×, before allocator rounding,
capacity slack, roots and other live data. The 100-row range fixture itself is
unchanged; range operations materialize and check their results (`workload.go:219–228`).

Historical observed cadence is about 190 GCs per 20k operations versus about
215 per 500k, not the same exposure per transaction. These approximate counts
are not a fresh 8-worker trace result. More frequent marking, different live
heap/heap goal, warmup-to-measurement ratio, and short-run startup effects are
all confounds. Do not call any resulting contrast a pure concurrency effect.
A fixed-fixture worker-count experiment requires a separate predeclared design;
the next slot below intentionally reproduces the existing budgets first.

## Falsifiable hypotheses and required observations

### H0 — Drainer wake, shard scans, frontier/sort or drainer timer Reset

**Source:** R4 `fire`/root loop `:148–203`, `collect :205–237`,
`deliverReady :240–280`. `collect` yields every 16 pops (`:230–234`). These paths
are callback-driven; producers do not take the root owner mutex.

**Confirm exposure:** establish a due wake during measurement and identify an
actual queue-callback goroutine/stack, not just any `time.AfterFunc`. Then align
its scan/sort/yield interval or its induced mutex/scheduler delay with slow
samples. A post-ten-second diagnostic cannot establish exposure in the original
sub-ten-second population.

**Refuted for the existing eight-worker timing population:** the whole-process
bound above, unchanged 30 s budgets and no earlier-key arm exclude these ordinary
callbacks, frontier scans, ready sorting and callback-induced Reset. For the next
trace population verify exposure anew. If trace stacks are ambiguous, absence of
sampled CPU frames alone is not proof of zero calls. No causal story about
repeated per-arm wakes is supported by this source.

### H1 — Producer/cleanup shard-mutex interference, without any drainer

**Source:** R4 eager schedule locks `q.mu` around publication and heap operations
(`:120–133`); cleanup locks the same shard and indexed-removes (`:136–143`).
Ordinary queued successors can skip this lock (`:113–115`). First associations
still insert under it. Cleanup can run well before a timeout callback.

**Trace confirmation:** repeated worker waits with `schedule -> Mutex.Lock`,
woken by the cleanup/remove path, aligned with slow operations; alternatively,
cleanup blocked behind a producer holding the shard through allocation/heap work.
Use unblock stacks where available. Quantify blocking and subsequent runnable
delay separately, by caller class. A generic mutex wait is not automatically a
queue wait or proof of the holder's identity.

**Refutation/limit:** no queue-mutex waits around affected operations refutes
parked queue contention as their direct delay. It does not exclude short
contention, uncontended lock cost, spin, or cache-line traffic. A separate mutex
profile/diagnostic lock-site measurement may be needed; summed blocked time is
not CPU attribution.

### H2 — Bursts of cleanup work and cleanup/producer scheduling interference

**Source:** both designs already register timeout cleanup: baseline
`read_incarnation.go:234` calls `timer.Stop`; R4 `timeout_queue.go:103` calls
`queue.remove`. There is no baseline-zero-cleanups control. Go's sweeper batches
cleanup work (`runtime/mcleanup.go:458–487`), wakes workers (`:588–642`) and
executes batches (`:715–756`). Crucially, `maxCleanupGs=max(GOMAXPROCS/4,1)`
(`:707–710`): at the fixed GOMAXPROCS=4 there is **at most one cleanup worker**,
not an explosion of cleanup goroutines.

**Trace confirmation:** the cleanup worker wakes/runs in bursts, performs the
candidate remove work, and overlaps worker ready-to-running delays or H1 lock
waits. Compare work duty/batch timing against baseline's timer-stop cleanup,
not merely whether a cleanup goroutine exists. Capture cleanup queue/executed
counter deltas only as approximate cohort evidence, not exact queue length.

**Refutation/limit:** affected samples have no contemporaneous cleanup execution
or induced wait, and delays do not follow its wakeups. This excludes direct burst
interference for those samples, not registration cost or later GC tracing of
cleanup arguments. Forced drain occurs after profile stop
(`internal/bench/run.go:422,429–431`); it must not be included as measured cleanup.

### H3 — GC assists, STW/mark scheduling and a changed scanned object graph

**Source:** queue roots retain heap storage/node/weak-handle identities; cleanup
arguments retain their nodes until dispatch and later GC. Weak edges do **not**
strongly retain the transaction. Go scans weak-handle and cleanup special roots
(`runtime/mgcmark.go:451–464,489–492`). Fewer allocations is not proof of less
scan work or shorter pauses.

**Trace confirmation:** slow 8-worker samples repeatedly overlap worker GC mark
assists, GC STW intervals, or runnable delay while mark workers run; compare
per-operation assist incidence/duration, GC cadence and phase shape across the
four 8-worker controls with bounded CSV alignment. Distinguish active assist,
parked assist wait and post-wake scheduling.

**Refutation/limit:** many adverse samples are wholly outside those intervals and
have no GC-induced wait, weakening a direct assist/pause explanation. GC overlap
alone is not sufficient: the baseline can also overlap GC. A trace cannot name
the extra live object type, prove the queue graph caused extra tracing, or
attribute assists to the allocation that originally created the debt. That needs
separate CPU plus allocation/live-population evidence and fixture control.

### H4 — Heap backing-array growth/copy and retained capacity

**Source:** insertion appends a pointer under `q.mu` (`timeout_queue.go:282–287`);
removal nils the old slot and shortens length but does not shrink capacity
(`:339–351`). Thus old transaction pointers are not silently left in unused
slots, yet backing capacity can persist at a cohort high-water mark. Ready-slice
growth is H0-gated and should not execute in a verified callback-free run.

**Trace confirmation:** candidate slow worker intervals show allocation assists
or descheduling at eager heap insertion, with a contemporaneous population/
capacity increase if separately observed. H1 can expose a producer holding the
shard across this work. Compare initial/warmup capacity effects with later work.

**Refutation/limit:** verified stable sufficient capacity through affected samples
excludes growth/copy events there, but not scanning the retained backing store.
Execution trace alone has no reliable heap-capacity or object-type census;
without counters/CPU allocation frames it cannot distinguish append growth from
other allocation under the same operation. Do not invent a capacity-growth event
from a long Running span.

### H5 — Ordinary identity lookup, runtime specials lock and synchronous sweep

**Source:** identity uses `weak.Make(tx)!=n.tx`
(`read_incarnation.go:218`), while R4 has no ordinary existing-node detector.
`getWeakHandle` disables preemption, ensures sweep, locks a span-specials list and
searches it (`runtime/mheap.go:2601–2632`). It has no strong-conversion GC park.
`ensureSwept` may sweep/yield (`runtime/mgcsweep.go:465–497`).

**Trace confirmation:** a separately resolved stack at ordinary lookup coincides
with long producer execution or a P stall; identity-versus-flag timing and CPU
profiles can test incremental lookup cost if their remaining differences are
sealed. The flag control cannot by itself validate identity's retained-rearm
contract. The already removed R5 promotion wait is not presumed to recur here.

**Refutation/limit:** a shared R4/baseline tail regression cannot be explained by
an identity-only lookup. No `GC weak to strong wait` does not exclude a runtime
lock/sweep stall: these may not appear as ordinary Go blocked intervals or sweep
regions. CPU/runtime-lock evidence is needed before naming that cost.

### H6 — Scheduler handoff, transport completion or shared response bursts

**Source:** measured latency includes retries, materialization and exact checks
(`internal/bench/run.go:185–190`), not only timeout bookkeeping. Cleanup/GC may
compete with transport and operation workers on four Ps. The same 100-row result
must still be delivered and checked in every arm.

**Trace confirmation:** separate Waiting→Runnable from Runnable→Running. Classify
network/select/channel completion, queue mutex, GC assist and cleanup-triggered
wakeups by stack. Look for delayed transport readers/writers, or workers made
runnable together but delayed on the run queue, in the same slow sample interval.
Compare baseline's pattern rather than treating normal `selectgo` totals as a bug.

**Refutation/limit:** affected workers resume promptly and spend the interval
Running instead; then handoff delay is not the main direct component. Trace
cannot attribute a remote FDB response delay to this client change or prove an
OS scheduling/cgroup/frequency cause; server/network and OS evidence are separate.

### H7 — Shared atomics/cache locality or other on-CPU cost invisible to trace

**Source:** R4 has root round-robin/ticket atomics on first association and every
logical arm (`timeout_queue.go:100,114,121`), shared by producers despite leaf
sharding. Heap/node layout and pointer chasing differ from baseline timers.

**Trace-compatible prediction:** affected operations stay Running rather than
park, with no corresponding GC/cleanup/transport wait; cost may change when
fixture size or producer concurrency changes. This is only a compatible pattern,
not confirmation of cache/coherence contention.

**Falsification/requirements:** an accounted off-CPU wait covering the excess
excludes this as that sample's direct dominant delay. Otherwise require a
separate CPU profile and, for cache-line/coherence/memory-bandwidth or frequency
claims, appropriate hardware/OS counters. Neither trace state nor worker-count
contrast identifies an atomic cache-line bottleneck. The existing fixture-size
confound prevents reading the 8/32 contrast as a scaling curve.

## What the first 8-worker traces cannot decide

One trace per arm is diagnostic, not enough replications to price a tail. Trace
perturbs scheduling/allocation/GC and may extend the run across the ten-second
callback boundary. Any sampled CPU stacks in a trace do not substitute for a
dedicated CPU profile's attribution. It cannot directly measure cache misses, cache-line
bouncing, bandwidth, frequency throttling, exact runtime-lock wait, queue backing
capacity, retained object types or remote server work. Missing a sampled frame
is not proof of absent execution. Temporal overlap and aggregate GC fractions
must not be promoted to causation.

Use the eight-worker count and actual budget from each manifest. The existing
trace scripts currently assert 32 workers and a fixed run-name pattern; adapt
and verify those checks explicitly for the new diagnostic population, rather
than reuse old worker mapping or silently relax assertions. Preserve the prior
32-worker extraction manifests and output summaries.

## Next owner-approved slot: at most 30 minutes around 17:30

Confirm the slot's timezone and exclusive host/cluster ownership; set a hard
wall-clock stop at slot start + 30 minutes. The following is a plan, not work
launched by this note. Prepare builds/correctness reviews **before** the quiet
window; no builds/tests/CPU-heavy analysis overlap either trace or timing runs.

| Elapsed budget | Work and stop conditions |
|---|---|
| 0–4 min | Verify frozen baseline/R4/identity/flag source+binary hashes, harness identity, fixture/budget, affinity, GOMAXPROCS 4, GOGC 100, timeout 30 s, retry policy, OS priority and clean host ownership. Record flag's actual diff and weaker contract. If preflight/correctness or lock acquisition fails, stop; do not consume the slot rebuilding. |
| 4–8 min | Separate **8-worker range100, 20k measured +2k warmup** traces for baseline, R4, identity, flag. Retain every sample and complete trace, UTC/monotonic process bounds, manifests, counter deltas and uptime. Check callback-age exposure; never pool traced quantiles with timing. No decoding concurrently. |
| 8–22 min | Unprofiled balanced timing of **baseline/identity/flag**, six permutations at 8w/20k and six at 32w/500k: 36 runs, 2k warmup each. Each arm occupies each position twice. Hold the campaign lock throughout the complete timing batch; no local profiling/builds/analysis. Seal all inputs before starting, record complete histograms/quantiles per run. Stop admitting new runs with enough margin for the current run and teardown; incomplete blocks stay labelled incomplete, never fill them selectively. |
| 22–28 min | Release timing ownership; decode the four small traces at nice 15, verify worker mapping/clock bounds and classify slow samples against H0–H7. Summarize per-run p50/p90/p99/p99.9 and all six paired signs, separately by budget. Analyze diagnostics separately. First answer whether H0 was exposed at all, then direct waits and runnable delays; leave unsupported causes unresolved. |
| 28–30 min | Preserve raw artifacts, original manifests and all failures, write a short handoff with exact remaining blockers, and release the host. If earlier phases overrun, defer analysis instead of overrunning or silently reducing replications. |

If the first traces leave Running-time, root-graph or cache hypotheses unresolved,
request a separate CPU/live-population/hardware-counter slot. Do not add profiles
to the timing budget or improvise production changes mid-campaign. An improved
weaker flag result is a diagnostic contrast, **not** permission to replace the
chosen identity contract. At this historical planning freeze, the 8-worker baseline tail
remained a blocker pending review. The later confirmation/gate status is linked above.
