# RFC-263 revision 5: trace mechanism and bounded sample alignment

## Scope

These are separate **instrumented diagnostic runs**, not qualification timings.
They establish an observed blocking mechanism and temporal overlap, not the cause
of the unprofiled range-latency regression or a latency trade-off worth accepting.
The detector-only identity-comparison A/B intervention is a separate experiment.

Evidence root: `/tmp/rfc263-rev5-tail.I5smMO`.
Inputs are `runs/tail-diag-trace-range100-p0-{rev5,rev4,baseline}/trace.out`,
`samples.csv`, `manifest.json`, and `summary.json`. Each run has 32 workers,
500,000 measured range100 transactions, 2,000 warmups, and Go 1.26.9. The raw
traces are retained; repeated multi-gigabyte textual stack dumps are not needed.

## Observed ordinary-arm blocking

| Frozen diagnostic arm | Weak-to-strong parks | Parks with `armTimeoutLocked` stack |
|---|---:|---:|
| Revision 5 | 5,671 | 5,671 |
| Revision 4 | 0 | 0 |
| Original baseline | 0 | 0 |
| Identity detector | 0 | 0 |

Every observed Rev5 weak park completed. Their cumulative **goroutine parked
time**, not CPU or wall time, is 6,845,114,217 ns; maximum is 5,698,880 ns.
The corresponding post-unpark runnable delay totals 1,095,356,662 ns; maximum
is 898,624 ns. Summing parallel goroutine delays does not measure elapsed time.

A concrete first observed Rev5 event is at trace time `717481011481792` ns:
`Running -> Waiting`, reason **GC weak to strong wait**. Its transition stack is:

```text
runtime.gcParkStrongFromWeak                  runtime/mheap.go:2493
weak.runtime_makeStrongFromWeak               runtime/mheap.go:2428
weak.Pointer[...].Value                       weak/pointer.go:89
(*Transaction).armTimeoutLocked               client/read_incarnation.go:217
turnoverToken.finish                         client/transaction_lifetime.go:253
(*Transaction).publishCommit                 client/commit_input.go:106
(*Transaction).commitAdmitted                 client/transaction.go:1817
(*Transaction).Commit                        client/transaction.go:1648
(*Database).Transact                         client/database.go:996
... Dataset.Operation -> Run.func2 -> runPhase.func2 (run.go:188)
```

This is the ordinary successful-commit successor arm, not evidence of an actual
resurrection or node replacement. Raw full transition stacks, wait/unpark/run
instants, and worker IDs are retained in `trace-analysis/rev5.events.jsonl`.

## Runtime mechanism and diagnostic blind spots

In the pinned SDK, `runtime/mheap.go:2418–2428` tests the global weak-to-strong
block gate before loading the weak handle. Even an already strongly retained
receiver can park. `gcParkStrongFromWeak` records the explicit trace reason at
`:2480–2499`. The gate begins before the ragged barrier
(`runtime/mgc.go:1036–1059`), can remain set while additional mark work is
processed (`:1061–1068`), and is cleared at `:1153–1156`. It is not necessarily
bounded by the reported STW pause. Runnable delay after waking is separate.

Promotion also calls `ensureSwept` (`mheap.go:2453–2454`), which can sweep or
`osyield` (`mgcsweep.go:465–497`), and can shade during marking
(`mheap.go:2462–2470`). These source-level possibilities are not quantified by
this analysis. Promotion does not directly invoke `gcAssistAlloc`.

A weak park uses direct `goparkunlock`, not ordinary block-profile event
accounting. A clean block profile therefore cannot exclude it. Likewise,
`ensureSwept` has no sweep tracing brackets; `GCSweepSpan` emits no sweep event
without them (`traceruntime.go:367–382`). Missing sweep regions are not proof of
no synchronous sweep.

## Bounded epoch recovery

`runPhase` creates workers in worker-index order (`run.go:161–196`); creation
stack identifies them, without assuming contiguous goroutine IDs. Every selected
worker must demonstrably block at the initial `<-start` (`:167`). The extraction
checks there are exactly 32 such measured workers.

Let `E` be the unknown CSV epoch in trace nanoseconds, `R_w` the first Running
event **after** worker w's start-channel release, `s_w` its first CSV start
offset, and `U_w` its initial channel-unblock event. Since the worker's first
operation starts after Running, and main takes the epoch before closing start
(`:205–206`):

```text
max_w(R_w - s_w) <= E <= min_w(U_w)
```

Raw interval widths are 1,820 ns (Rev5), 1,323 ns (Rev4), and 1,946 ns (baseline).
The analyzer expands both epoch bounds by one 64 ns trace-clock quantum and
checks all recorded Go 1.26 clock snapshots against that margin. It additionally
allows ±64 ns on each later trace boundary used for CSV alignment. This is an
explicit clock-quantized alignment model, not an exact recovered timestamp or
proof of a universal clock-error bound from finitely many snapshots. Both ends,
clock snapshots, worker mapping, and assertions are retained in the output.

For a sample with CSV start/end offsets s/e, GC mark interval [a, b), and boundary
margin m, possible overlap means `E_low+s < b+m && E_high+e > a-m`. Guaranteed
overlap with that mark means `E_high+s < b-m && E_low+e > a+m`. These yield
conservative lower/upper counts under the stated clock model without choosing a
convenient epoch. A direct wait match additionally requires the **same worker's
entire park plus runnable span** to fit one sample at the same possible epoch,
with the same endpoint margins. Possible and guaranteed matches are separate.

## Alignment results

The final four-arm scan passed every input hash, frozen executable hash,
32-worker mapping, sequential sample, clock snapshot, complete GC interval,
completed weak wait, and sample-containment assertion. The epoch bounds below
include the ±64 ns epoch margin; later trace boundaries have a separate ±64 ns
margin as described above.

| Arm | Epoch bounds (ns) | Width (ns) | Complete GC mark intervals |
|---|---|---:|---:|
| Rev5 | 717480994781860–717480994783808 | 1,948 | 213 |
| Rev4 | 717445106337045–717445106338496 | 1,451 | 210 |
| Baseline | 717463016959014–717463016961088 | 2,074 | 209 |
| Identity | 718478396023010–718478396027840 | 4,830 | 210 |

**All 5,671 Rev5 weak waits begin inside an observed GC mark interval.** Every
entire park-plus-runnable span maps to exactly one same-worker CSV sample for
all allowed epochs, not merely under a best-fit epoch. In this run, 5,671 of
500,000 operations (1.1342%) contain that wait.

| Arm | All-sample GC overlap, guaranteed–possible / 500,000 | Slowest 1% GC overlap / 5,000 | Slowest 1% containing weak wait | Slowest 0.1% GC overlap / 500 | Slowest 0.1% containing weak wait |
|---|---:|---:|---:|---:|---:|
| Rev5 | 40,425–40,443 | 4,830 | 2,893 | 500 | 203 |
| Rev4 | 53,364–53,384 | 4,625 | 0 | 500 | 0 |
| Baseline | 54,211–54,243 | 4,516 | 0 | 500 | 0 |
| Identity | 52,856–52,892 | 4,736 | 0 | 500 | 0 |

For both slow-sample groups, guaranteed and possible counts coincide. **Every
arm's slowest 500 samples overlap GC marking**, including the baseline and the
identity detector without weak parks. Temporal overlap alone does not identify
which GC/runtime/application activity causes the remaining tail. Nor does the
smaller total overlap population in Rev5 establish less GC work; these are
closed-loop traced executions with different code paths, producer blocking, and
phase-dependent scheduling.

### Descriptive diagnostic quantiles only

These single traced runs are not paired pricing evidence. Values below are
microseconds, with quantiles selected by sorted order statistic
`floor((n-1)*p)`. They must not be pooled with the unprofiled A/B campaign.

| Arm | p50 | p90 | p99 | p99.9 | Maximum |
|---|---:|---:|---:|---:|---:|
| Rev5 | 716.748 | 1,062.443 | 2,644.840 | 5,360.374 | 12,274.622 |
| Rev4 | 735.232 | 1,102.568 | 2,429.993 | 4,778.171 | 12,983.404 |
| Baseline | 741.064 | 1,113.219 | 2,326.376 | 4,744.307 | 11,107.039 |
| Identity | 735.203 | 1,109.953 | 2,463.757 | 4,757.381 | 10,034.037 |

Rev5's weak-park median/p99 are 1,080.192/4,014.464 µs; subsequent runnable
median/p99 are 188.224/362.496 µs. The identity diagnostic confirms removal of
this explicit arm-path park mechanism, not elimination of all tail mechanisms
or proof of the uninstrumented campaign's entire latency delta.

The separate identity trace also has zero weak-to-strong parks. The first CSV
analysis pass conservatively rejected `RangeActive` events. Inspection showed
these are trace-generation carry-forward markers inside already open, fully
observed GC intervals, not missing GC starts. The final parser accepts them only
when an explicit begin is already active; it still rejects incomplete initial or
final intervals rather than inventing endpoints. No heavy CSV or trace work ran
during the A/B quiet window.

## Reproduction and artifacts

Scripts are in-tree; generated outputs stay under the evidence root:

```sh
SDK="${GO_SDK_ROOT:?set GO_SDK_ROOT to the Bazel Go 1.26.9 SDK}"
P=/tmp/rfc263-rev5-tail.I5smMO
uv run --no-project python tools/benchcampaign/campaigns/rfc-263/rev5-trace-extract.py "$P" --sdk "$SDK" --extract-only
uv run --no-project python tools/benchcampaign/campaigns/rfc-263/rev5-trace-analyze.py "$P"
# After the separate identity diagnostic finishes:
uv run --no-project python tools/benchcampaign/campaigns/rfc-263/rev5-trace-extract.py "$P" --sdk "$SDK" --extract-only --arms identity --manifest-name extraction-inputs.identity.json
uv run --no-project python tools/benchcampaign/campaigns/rfc-263/rev5-trace-analyze.py "$P" --arms rev5 rev4 baseline identity
```

The extractor compiles the pinned SDK's `cmd/trace` with a temporary overlay that
changes **only the debug printer**, retaining the SDK's own validated trace
reader. It never changes SDK files on disk or benchmark sources/binaries. It
emits only worker mapping, GC/STW boundaries, clock snapshots, and completed
weak waits. The first attempted full parsed debug dump was stopped and its
partial generated text removed; the original binary trace remains intact.

`trace-analysis/extraction-inputs.json` records trace, CSV, SDK debug-source,
overlay source, extractor script, and compact-output SHA256 hashes. The original
three-arm manifest is preserved byte-for-byte as
`extraction-inputs.original-three-arms.json`; identity-only extraction uses
`extraction-inputs.identity.json`, not an overwrite. The exact originally executed
extractor script is also retained and checked against the original manifest's
script SHA; subsequent script changes only add the output-manifest option.
The analyzer wrote `alignment-summary.json` and each arm's `.alignment.json`
with full per-wait and slowest-0.1%-sample tables, hashes, and epoch bounds.
`alignment-summary.json` SHA256:
`0c14e0d53841ef4b0d1ede8f27fe3e2512307c33df41e7bb0ad171ec5f965cd9`.
Scope remains instrumented mechanism evidence; no timing distributions are
pooled with it. Four-arm analysis completed at 13:08 UTC, after the A/B and
identity-trace quiet windows. No repository test lane or benchmark was run by
this analysis.
