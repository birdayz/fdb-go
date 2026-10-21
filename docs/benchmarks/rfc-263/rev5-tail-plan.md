# Rev5 range-tail blocker: diagnosis plan

**Historical plan.** The later [fixed confirmation](rev6-confirm-results.md) cleared
the 8w p999 blocker under the owner's rule. The old screen and diagnosis plan below
are not current gate status or retrospective attribution of the newer result.

The RFC at `c6e4cc97bb43c9606674cc28f6fee638f43ea8e5` records a merge blocker, not an accepted CPU/latency trade-off. Its fresh three-pair 32-worker range screen has p99 +19.44% against the original baseline in every pair; incremental R4→R5 8-worker range tails are also adverse in every pair. Cause is unresolved until the trace and intervention evidence are in.

## Retained data first

The retained R4 selected unprofiled phase (`rev4-range4-w32`, 3×500k measured) already has a weaker p99 warning: pair changes +7.447%, +1.496%, −1.907%; ratio of run medians 2170.342→2316.308 µs (+6.725%), not the same statistic as median paired change (+1.496%). Median p50/p90/p999 changes are −0.278%/−0.302%/−1.466%. Do not pool phases or treat historical runs as contemporaneous controls.

Phase 1 has only separate mutex-profiled 32-worker range populations: 100k p99 +27.434% (candidate sampled idle .899 versus baseline .981 is a confound); 500k draining p99 +0.806%, p999 +11.277%, each one pair. R4's separate mutex 500k pair has p99 +6.380%. These diagnostics are not an unprofiled timing population.

## Frozen new timing

Raw destination: `/tmp/rfc263-rev5-tail.I5smMO`. In-tree runner: `tools/benchcampaign/campaigns/rfc-263/rev5-tail-campaign.py`; analyzer: `rev5-tail-analyze.py`.

Six balanced three-arm blocks at 32 workers (500k measured + 2k warmup), followed by the same six blocks at 8 workers (20k + 2k). The six permutations are baseline/R4/R5, R5/R4/baseline, R4/baseline/R5, R5/baseline/R4, baseline/R5/R4, R4/R5/baseline. Each arm occupies each position twice. All three archived executables/source identities are frozen; no rebuild or source change is part of this batch. Hold `flock --close ${FDB_PERF_HOME}/validation/fast.lock` for the **whole 36-run timing batch**. Record uptime before and after every run plus exact argv/environment, binary SHA, source and harness identity, UTC bounds, load and idle. No builds/tests/profiles/CPU-heavy analysis during timing.

After the batch, report p50/p90/p99/p999 from every measured sample using the lower order statistic `floor((n−1)p)`. Preserve full samples and complete count histograms with 1 µs bins, lower-inclusive/upper-exclusive, no dropped tail or overflow bucket. Separate timing and diagnostic phases in all summaries.

## Diagnostic and causal sequence

After releasing the timing lock, run separate baseline/R4/R5 block, mutex and execution-trace populations, 32-worker range 500k. The unchanged harness already supports these modes. Trace is started after warmup, includes worker creation and before/after stats around measurement, and stops before forced drain. Instrumented latency is not qualification timing.

Pinned Go 1.26.9 source provides a **hypothesis**, not attribution: `runtime_makeStrongFromWeak` can park even a strongly held receiver on the global GC completion gate, reason `GC weak to strong wait` (`runtime/mheap.go:2418–2428,2480–2499`; gate `mgc.go:1036–1068,1153–1156`). R5 invokes this from `armTimeoutLocked`; R4 does not on ordinary arms. This direct park need not appear in a block profile. Find the trace reason **and caller stack**, align with slow CSV intervals and GC marking, and count subsequent runnable delay separately. Sweep/yield and referent shading are other possible costs, not automatically exercised by every promotion.

Only after that evidence, compare R5 against an otherwise-identical disposable prototype using `weak.Make(tx) != n.tx` as the detector. Go 1.26.9 guarantees same-owner canonical identity and a distinct identity after resurrection (`weak/pointer.go:26–37`); registration does not use the strong-conversion GC gate but still sweeps and takes a span-specials lock/list lookup. Verify controlled resurrection probes and normal allocation pins, freeze exact source/binary identities, then run a separate alternating A/B tail batch under the same lock. No attribution or fix claim before this intervention. Do not weaken first-rearm liveness through a drainer-only dead flag without a new contract review.
