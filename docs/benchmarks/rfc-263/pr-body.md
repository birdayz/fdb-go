## What / why

Implement [RFC-263 revision 6](https://github.com/birdayz/fdb-go/blob/master/rfcs/263-transaction-timer-and-incarnation.md): sixteen stable producer shards, lower-bound timer stubs and one globally ordered drainer. Logical disarm/later rearm avoids shard locking while preserving synchronous expiry and C7 ordering. Immutable same-shard canonical weak-identity renewal recovers every retained future-positive rearm after resurrection without R5's proven weak-promotion GC gate. Wire behavior is unchanged; B3a/B3b remain deferred.

**Design accepted; implementation complete, not merge approval.** The fixed non-inferiority confirmation clears the recorded 8-worker p999 blocker. The separate A1/A2 CPU qualification and queue-wait diagnostics also pass their recorded rules; full/full-race, all five final-HEAD reviews, CI and owner approval remain pending.

## Measurements and adverse findings

The earlier screen and p999 confirmation compare baseline `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` versus production `c2e52f46ab02df0295f272f421b87c3bdb3a3515`, not a claim about current master. The later CPU campaign has separate A1/A2 provenance and final sampling seal `989b186a2`, production executable SHA256 `02d86dcde2f70868c116aeed225c7ef0d8a626da9b8501fb0f06a2e668c41949`, actual Bazel SDK Go 1.26.9; the earlier executable identity must not be substituted for it. All identities are retained in the linked evidence. Exploratory timings, traces, p999 confirmation, A1, A2 and diagnostics are separate populations, never pooled.

- **Original six-block exclusive production screen:** range 8w client CPU **+1.66% (4/6 pairs adverse)**, ABOVE baseline; range 32w **+0.045% (3/6 adverse)**, effectively flat. Get/set 8w CPU **−3.43%/−2.50%**. These are workload-specific results, not an all-workload CPU win.
- **Original adverse tails remain recorded:** get 8w p999 **+9.30% (4/6 pairs adverse)** and set 8w p999 **+8.94% (5/6 adverse)**. Get mean was **+0.36%**; set mean was **+0.104%, with all 6/6 pairs adverse**. Range 8w p99/p999 improved in that unprofiled batch, so its historical all-adverse pattern did not reproduce there; that is not proof of equivalence or attribution of the earlier result.
- **Later confirmation's descriptive CPU medians:** get **+1.69%** (46.021 → 46.801 µs/operation), ABOVE baseline; set **−1.64%** (55.451 → 54.541 µs/operation). These are context-only treatment-run medians, not a CPU non-inferiority decision. **Those observations did not satisfy the original neutrality wording. The separate later A1/A2 campaign passes the owner-approved amended 2% rule without reinterpreting these earlier screens.** Passing p999 did not waive the CPU gate.
- Transaction creation allocations **11 → 9**; ordinary live-node rearm remains **0** allocations. Allocation savings do not establish CPU or latency acceptance.

### Actual production 8-worker range trace

The [separate instrumented diagnostic population](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/rfc-263/rev6-production-tail-results.md) is adverse for production: baseline → production p99 **1014.842 → 1254.405 µs**, p999 **1526.791 → 1944.371 µs**, and total latency of the slowest 1% **245.475 → 306.885 ms**. Each arm has only one instrumented run; this is descriptive evidence, not a balanced causal timing comparison.

GRV/reply select waits and subsequent runnable delay are the largest direct worker spans. Slowest-1% direct GC-assist waiting rises **0.790 → 3.773 ms** and STW overlap **22.612 → 37.787 ms**; these nonadditive overlaps do not prove a p999 cause. There are zero weak-promotion waits, production queue-path sync spans or production arm-path parks. Cleanup executor time is not dominant, and conservative process-age bounds exclude the first drainer callback in all 39 short 8-worker processes. None of those exclusions rules out registration, retention, GC or running-time costs. There are **no get/set traces** in this population.

## Fixed p999 confirmation: scoped clearance

The [preregistered confirmation](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/rfc-263/rev6-confirm-results.md) completed **1152/1152 successful processes and 23,040,000 measured operations** across two balanced epochs: 96 treatment pairs per workload plus 96 pairs of each A/A control per workload. The primary estimand is the exponentiated mean paired log ratio, not the exploratory ratio of medians.

- Get production/baseline **0.952 [0.902, 1.005]**; set **1.008 [0.977, 1.041]**, using the prescribed two-sided 97.5% intervals.
- Both upper endpoints are strictly below **1.05**, and all four A/A mean intervals contain **1**. The **8w p999 blocker clears by the owner's preregistered rule**.
- Get A/A log SD **0.25–0.27** exceeds the planned **0.15**: planned power was **not established**, despite clearance. Set epoch ratios **0.965 → 1.054** include epoch 2 alone above **1.05**; order/serial effects remain disclosed. No post-hoc endpoint or method changes were made.

This decision neither erases the original adverse screen and traces nor establishes universal tail stability, CPU neutrality or merge approval.

## CPU qualification and separate diagnostics

The [complete CPU results](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/rfc-263/rev6-cpu-results.md) retain all sixteen A1 bounds: A1 remains **UNRESOLVED** solely on both grv-w8 endpoints. The outcome-informed, presampling-sealed A2 replaces only those two entries, without pooling, giving window **0.974409096 [0.961230466, 0.987768407]** and lifecycle **0.975915653 [0.964107817, 0.987868104]**. The other fourteen A1 bounds are unchanged; all sixteen final upper 95% bounds are ≤1.02. Both endpoints have upper bounds strictly <1.00 for **grv-w8, grv-w32, set100-w8 and set100-w32**; other cells are NI, not improvement. Per-endpoint intervals are not simultaneous 95% coverage, and A/A controls are descriptive only.

The separate sixteen-process, 32-worker queue diagnostic also **PASSes**: each of eight production workload/profile cells has combined shard+root delay / same-process measured-window CPU **<1%**, maximum **0.008781790%**. Native profiles and positive controls validate all sixteen baseline/production profiles. Mutex means Unlock-holder context, block means Lock-waiter context; caller/drainer splits and baseline context are retained. Never add the two views or interpret cleanup-holder attribution as proof cleanup itself waited. These diagnostics never enter CPU inference. The sampled production binary is unchanged through A1/A2/profiles, not rebuilt from a later documentation tree.

## Correctness evidence and open gates

The [implementation checkpoint](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/rfc-263/rev6-production-evidence.md) records 20 canonical top-level tests plus two stage subtests, including four real-finalizer resurrection scenarios. Whole fast-lane nominal race/opt and exact-restoration runs passed. Removing renewal, eager publication locking, fixed frontier, `again` or cleanup compiled and produced exact uncached named-red regressions. The evidence distinguishes actual from cached targets and preserves the invalid first proof-runner attempt.

- [x] CPU qualification under the amended A1/A2 rule and separate queue-wait diagnostics: all sixteen final CPU upper 95% bounds ≤1.02; all eight production queue-wait cells <1%. This is not literal zero CPU regression or merge approval.
- [ ] Owner-run full/full-race verification; the recorded fast lanes are not those full lanes.
- [ ] All five official final-HEAD reviews, CI and owner merge approval.

Independent raw statistical derivation and scoped method review agree with the p999 result; neither substitutes for those open gates. B3a/B3b are separate follow-up work.
