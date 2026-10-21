# RFC-263: fixed-population get/set 8w p999 confirmation

**Owner decision: the 8w p999 blocker clears by the preregistered rule.** Both
primary upper 97.5% interval endpoints are strictly below 1.05, both combined point
estimates are below 1.05, and all four A/A control intervals contain 1. This is the
predefined decision over the complete fixed population—not a favorable median or an
alternative analysis selected after inspection. It does **not** establish the planned
power, absence of temporal effects, all-workload CPU qualification, full-suite/race
verification or merge approval. **Prominent limitations:** get's A/A log SD 0.25–0.27
exceeds planned 0.15, so planned power was not established although the interval cleared;
set's epoch estimates 0.965→1.054 include epoch 2's point above 1.05. Order/serial effects
are retained, with no post-hoc reinterpretation in either direction.

## Population, identities and collection integrity

Preregistration: `7bf58d7bf2bba737d8087d261b19f0e30bc274b5`, RFC-263's owner-ACKed
5% rule. Schedule seed 263006; full frozen schedule SHA256
`26641bf7a50514e051c08c62859091df971f99b25ef8b91cd70e68045f327fe3`.

| Role | Frozen source | Executable SHA256 |
|---|---|---|
| Frozen baseline—not current master | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e` |
| Selected production | `c2e52f46ab02df0295f272f421b87c3bdb3a3515` | `ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de` |

The owner explicitly ACKed the #970 provenance exception before any sample: the exact
`git diff --stat fa8725f1d..8aed7009450b4b7373f90761c955a8bb6b814c8a -- pkg/ cmd/ tools/clientbench/`
guard was **nonempty**, containing only `tools/clientbench/README.md` and standalone
`wire_batch_timeline.py`/`wire_grv_rounds.py` analysis scripts (3 files, 64 insertions).
There were no pkg/, cmd/, .go, BUILD or go.mod changes in that comparison. No new-master
binary was built or substituted. The guard output and exception ACK are frozen with
inputs; an initial unavailable-object check and subsequent fetch are also recorded.

**1152/1152 successful processes, 23,040,000 measured operations**, plus 2,304,000 warmup
operations. Each epoch has 576 processes; each workload has 48 treatment pairs,
48 baseline/baseline A/A pairs and 48 production/production A/A pairs per epoch. Thus each
of the six workload/contrast populations has **96 independent process pairs**, 48 per
epoch. Treatment/alias first-position and workload-order balance match the frozen
schedule. No process was replaced, no block was discarded, and no interim outcome
comparison was inspected. Epoch 1 was integrity-checked and sealed before epoch 2 began;
analysis started only after epoch 2's complete integrity gate passed.

Historical client settings are unchanged: Go 1.26.9 opt binaries; client CPUs 2,3,14,15;
GOMAXPROCS 4; GOGC 100; GOMEMLIMIT=off; empty GODEBUG; 20000 measured operations/process;
warmup 2000; timeout 30000 ms; retry limit 10; profile=none; unique 68-byte prefixes; unchanged
native three-process cluster and harness. Collection controllers used nice 19 and the
reserved CPU mask. There were no builds, Go/Bazel tests, testcontainers or other timing
in either measurement epoch. Raw outcomes stayed write-only until both epochs completed.

| Integrity item | Epoch 1 | Epoch 2 |
|---|---:|---:|
| Collector start UTC, 2026-10-20 | 17:23:27.970 | 18:43:17.013 |
| Collector complete UTC | 18:14:02.985 | 19:33:53.578 |
| Successful processes / measured rows | 576 /11,520,000 | 576 /11,520,000 |
| Nonvacuous machine gate records / forbidden records | 1740 /0 | 1740 /0 |
| Quiet settle before first launch | 63.854s | 63.855s |
| Largest conservative child-lifetime bound | 2.868289s | 2.868279s |
| Per-run artifact hashes independently reverified | 3456 | 3456 |
| Raw files sealed and reverified | 4036 | 4036 |

All accepted prelaunch loads were ≤7.5; complete tracked/untracked/config manifests and
all frozen scripts, binaries, schedules and cluster files matched after collection.
The collection source had 7415 entries. All child lifetimes are below 10 seconds, preserving
the short-process regime and excluding the queue's first interval callback from these
measurements. Registration/retention/GC costs are not excluded by that bound.

## Primary method and recorded decision

For each process, p999 is the exact empirical lower order statistic at zero-based
rank `floor((20000-1)*0.999) = 19979`. The replication unit is the process pair, not an
individual operation. Treatment ratios are production/frozen-baseline regardless of
launch order. No exploratory samples, R4 prototype, traced runs or pooled raw-operation
percentile enter this confirmation.

For each workload and epoch, compute the mean and sample variance of paired log ratios.
The estimate is the equal-weight mean of the two epoch means. Its standard error and
degrees of freedom are the preregistered Welch-Satterthwaite values. Exponentiate the
point estimate and two-sided 97.5% interval. These are the only primary decision intervals.

| Treatment | Mean paired-log ratio, exponentiated |97.5% ratio interval | Change and interval | Upper <1.05? |
|---|---:|---:|---:|---|
| get100,8w |0.951989616 |[0.901781407,1.004993251] |−4.801%;[−9.822%,+0.499%] |Yes |
| set100,8w |1.008473212 |[0.976863672,1.041105579] |+0.847%;[−2.314%,+4.111%] |Yes |

Neither combined point estimate reaches 1.05, so the owner's point-estimate
regression/design-change rule does not trigger. Both upper bounds satisfy the strict
clearance condition and the A/A mean gates below pass. The **8w p999 blocker therefore
clears by the preregistered rule**, as accepted by the owner. Other owner/CPU/full/full-race/final-HEAD review
and merge gates are not replaced by this result.

## A/A noise floors

Controls are oriented alias B/alias A independently of execution order. Each row has 96
pairs, 48 per epoch, with identical binary/source/configuration within each pair.
Intervals are the prescribed **two-sided 98.75%**, not the treatment confidence level.
All four intervals contain 1; no control mean interval triggers the calibration gate.
A nonsignificant A/A mean does not prove zero bias or equivalence.

| A/A population | Mean ratio |98.75% ratio interval | Paired-log SD |Median absolute pair change |95th percentile absolute pair change |
|---|---:|---:|---:|---:|---:|
| get100, frozen baseline |1.001351464 |[0.938201244,1.068752319] |0.252614 |14.44% |49.70% |
| get100, production |0.989784007 |[0.921837301,1.062738923] |0.273441 |14.25% |54.27% |
| set100, frozen baseline |1.006232083 |[0.973810742,1.039732836] |0.125521 |7.83% |22.80% |
| set100, production |1.014051294 |[0.979775068,1.049526631] |0.131623 |7.47% |25.17% |

These are substantial per-pair p999 fluctuations even for identical executables. They
are not confidence bounds on a treatment effect or a license to drop noisy blocks.
Get treatment SD is 0.233051; get A/A SDs 0.252614/0.273441 also exceed the 0.15 planning
assumption. The planned approximately 80% per-workload power was **not established**;
no joint 80% power claim is made. Actual variances were used in the prescribed intervals.
This does not introduce a new post-hoc hard gate or replace the observed decision rule.

Raw pooled chi-square SD confidence bands are normal/homogeneous-iid-model descriptions,
not exact uncertainty statements under arbitrary epoch heterogeneity. The raw report
also supplies each epoch's SD; the primary Welch interval does not force equal variance.

## Epoch, order and sensitivity limits

| Treatment | Epoch 1 ratio | Epoch 2 ratio | Master-first ratio | Production-first ratio | Epoch lag 1 correlations |
|---|---:|---:|---:|---:|---:|
| get100 |0.929651 |0.974865 |0.980809 |0.924017 |−0.0585,+0.0187 |
| set100 |0.964966 |1.053942 |0.984839 |1.032675 |+0.2767,+0.1422 |

The set epoch shift is material to interpretation: epoch 2 alone has a point estimate
above 1.05 while epoch 1 favors production. The fixed primary estimand is the equal-weight
combined epoch mean; neither selecting the favorable epoch nor applying a new worst-
epoch decision is allowed. Order differences and positive set lag 1 correlations also
limit an iid/generalization interpretation. They are reported, not removed by a new
endpoint, variance estimator or extra runs after inspection. The AA get epoch/order
series and all paired observations are retained in the report/plot.

The prespecified whole-pair bootstrap sensitivity preserves epoch strata and shared
get/set superblocks. With 20000 resamples, treatment 97.5% percentile intervals are
get [0.903840,1.003477] and set [0.977299,1.040014]. Both also have upper bounds below 1.05,
but **the bootstrap was not used instead of the primary t interval**. Implementation
uses base seed 263006: AB 263006, baseline-A/A 263007, production-A/A 263008. Source, seed,
replicate count and method were frozen before the analyzer read outcome CSVs.

Context-only treatment-run medians: client CPU get 46.021→46.801 µs/operation; set 55.451→
54.541 µs. Allocation counts get 51.8165→49.8128 and set 55.8684→53.8626 per operation.
These descriptive medians are not the primary estimand or new CPU qualification gates.

## Reproduction, independent check and artifacts

Durable root:
`${FDB_PERF_HOME}/validation/rfc263-production/confirmation/batch-20261020T1722Z/`.

Historical as-run collection: `tools/benchcampaign/campaigns/rfc-263/rev6-confirm.py`, SHA256
`7a66c34cafa0bc5e0245fd2ad2ec65860cb0da0599af11ad0ef1643edd61bf32`.
It has positive/negative schedule, AA-role, vacuity, deadline, CSV and manifest guards;
it computes no outcome metrics during collection. Epoch 2 reuses the original full
schedule and inputs and requires complete epoch 1. Collection never resumes or replaces
a run. The first source guard stopped before samples until the owner ACKed #970's
explicit documentation/analysis-script exception.

As-run and unchanged canonical analyzer: `rev6-confirm-analyze.py`, SHA256
`d3a64c062a02bb062ee3dc7e49863d070c43cb7c01febe53a535533304765543`.
Six synthetic guard groups passed before CSV analysis: exact ranks/vacuity, analytical
Welch SE/df, Student/normal positive controls, zero-variance intervals, launch-order-
independent pairing and strict owner decision boundaries/control failures. Actual raw
CSVs are hash-bound to each closed epoch seal. Every 1152-run/576-pair/six 96-pair population
is nonempty and checked. Full 1 µs histograms retain all observations with no overflow bin.
Python 3.12.15, NumPy 2.3.4 and SciPy 1.16.3 are recorded; analysis used nice 19/reserved mask,
with no Go/Bazel execution or additional measurement.

An independent reviewer derived all six mean/interval/SD results directly from the raw
23,040,000 CSV rows **without reading the canonical analyzer**. Results agree to
approximately 1e-12. Its separately archived source and guards are retained; independent
JSON SHA256 `ddbe66dbeec399a49ad4690cfac21d236ad450d49b4ec5e631ca5cb8a149eb1f`.
A separate read-only implementation review ACKed rank, orientation, counts, Welch
formula, critical values, AA gates and decision rules. This is scoped method/evidence
review, not an official implementation/merge approval.

The complete confirmation bundle contains **9261 files / 1,420,220,129 bytes**, sealed
and rehashed at 19:55:23 UTC on 2026-10-20. Its recursive index excludes itself; both closed
epoch indexes and the independent review remain unchanged. Write-up bytes are also
retained in the real-hook commit's frozen source archive. Future reanalysis must use a
new output directory rather than mutate these closed evidence directories.

| Artifact relative to durable root |SHA256 |
|---|---|
| `confirmation-artifact-sha256.json` |`11154ff9f414683639a8a67f232a9853f23d656d7a674cb0601f9e70395d7309` |
| `epoch1/artifact-sha256.json` |`a6d8c9165f656d8046708395180303676a25a84605225b2f9bf6dfa4f31334db` |
| `epoch2/artifact-sha256.json` |`df0d17dd6be72df72d050e8e8b2b740d47ecaabc2ec28ffc318e1e753d95fd9b` |
| `analysis-v1/results.json` |`66fabbc19b9c2500fd0f4020e92df1aa09ccfeb52338265f7d607736b2e4fb50` |
| `analysis-v1/artifact-sha256.json` |`f8f6215e9c65332ac43ab8430da139bfa8f46ea2879f9f484cc1cd0c46186d14` |

The exploratory 6-block +9% findings remain in `rev6-production-tail-results.md`; they
are not erased or pooled with this fixed-population confirmation. Likewise, the earlier
range traces remain observations, not retrospective attribution of this confirmation.

Portable collection source differs only as documented in [reproduction and source identities](reproduction.md); the sealed as-run collector and all raw indexes are unchanged. That index also records the independent crosscheck SHA256 and exact analyzer/crosscheck invocations.
