# RFC-263 CPU amendment A2: grv-w8 confirmation

**Status:** Completed: A2 and combined CPU PASS; separate queue-wait diagnostic PASS. See [full results](rev6-cpu-results.md). The fixed plan and Python inputs were sealed before sampling and each slot received separate owner authorization. A2 is an outcome-informed amendment after A1, not an original A1 preregistration. This later status annotation does not change the archived as-run plan or authorize more sampling/merge.

## Scope and supersession

The complete A1 campaign collected 256 processes and 128 pairs. Its exact decision was **UNRESOLVED**: no point estimate reached 1.02, but both `grv-w8` upper 95% bounds exceeded 1.02. The other **14 cell × endpoint bounds** met the declared CPU non-inferiority criterion. A2 confirms **only grv at 8 workers**, for both measured-window B→C and post-identity lifecycle E−A process CPU per transaction.

A2 **replaces, and never pools with, both A1 grv-w8 estimates** for the final CPU decision. The other 14 bounds remain the frozen A1 estimates. An A2 PASS therefore makes the combined CPU gate PASS; an A2 FAIL blocks that gate; an A2 UNRESOLVED parks RFC-263, with no extra pairs, replacement epoch, precision-driven extension or further confirmation. The original A1 verdict, all 16 A1 intervals, A/A observations, and the earlier invalid pre-A1 batch remain reported and immutable. This supersession does not retrospectively turn A1 into PASS or create a simultaneous 95% confidence claim.

Bind the retained A1 inputs by independently recorded hashes:

- A1 frozen manifest: `154a783499a59a5e26e3bdffbf19631b99cd2a41b67261c34556eb187493dd62`.
- A1 epoch 1 seal: `da405065ee33e0da348a2c8b6044429183c7f4a6ade7ac8f64ae55fbbc22b2e2`.
- A1 epoch 2 seal: `dbde586f5313e3a635a8b8f306079384a38451f7c60c885c04699f3d56d90472`.
- A1 `results.json`: `6c700352c97e190fbc1cadf7d0a597fa6eb3f16a10bf7fb06144c9a82377deeb`.
- A1 analysis artifact index: `6c4cd85cd82589cc11dcc6d11721d5511b0d649a59e0b26331a8cf1b0964d8c5`.

Preparation and analysis must check the retained result and artifact-index pins, the **actual A1 frozen-manifest pin above** (not an arbitrary same-identity prior manifest), the original baseline identity, identical production binary bytes, and all 14 retained bounds. Frozen copies must be rebound to the independent pins after copying and when verified; computing a new hash after a validation/copy race is not sufficient.

## Executables and historical configuration

- Frozen baseline source: `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`; binary SHA256 `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e`.
- Production binary SHA256: **`02d86dcde2f70868c116aeed225c7ef0d8a626da9b8501fb0f06a2e668c41949`**, unchanged from A1. Only documentation/Python changes are authorized for A2. Preparation must compare the current source/config census against the independently pinned A1 census: the nonempty changed-path population may contain only authorized documentation/Python paths, and external configurations must be identical. Prove no Go, clientbench harness, BUILD/module/toolchain/dependency changes; record that source-input equivalence alongside the new sealed tree, rather than checking only the executable hash. Do not imply the unchanged executable was rebuilt from a later documentation tree.
- Actual Bazel SDK: **Go 1.26.9**, unchanged. Harness archive SHA256: `cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400`. Preparation also requires equality with the pinned A1 manifest's primary/tail/confirmation helper hashes, corrected lifecycle-helper hash, prebuilt pprof hash, cluster-content hash and baseline source-archive hash. Artifact paths may differ; these unchanged bytes may not. The new plan, Python tooling and production documentation/source archive have their own new pins.
- Existing real tmpfs FoundationDB cluster, pinned cluster identity; pure-Go backend in both arms; no inherited `FDB_NETWORK_OPTION_*`.
- Workload **grv**, **8 workers**, **20,000 measured + 2,000 warmup transactions per process**. Client affinity `2,3,14,15`; `GOMAXPROCS=4`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`; transaction timeout 30,000 ms, retry limit 10. Keep fixture, warmup, barrier, validation, shutdown and profiling-disabled behavior unchanged.
- Preserve the selected-fa baseline exception from A1: the nonempty fa→publication-parent clientbench delta is README/two standalone analysis scripts only; compiled Go/BUILD harness inputs are unchanged.

## Fixed population and randomization

**80 AB pairs total: 40 in each of two epochs.** Every epoch has exactly **20 baseline-first and 20 production-first AB pairs**. Add **four A/A pairs per epoch**: two baseline-AA and two production-AA, with one of each execution order per role per epoch. Shuffle these fixed pair slots with **seed 263107**; order is fixed before the first sample and never changed by an outcome. Pair members are adjacent, fresh processes.

Each epoch has **44 pairs / 88 processes**; the complete confirmation has **88 pairs / 176 processes**. This is **3,520,000 measured transactions plus 352,000 warmup transactions**. AB is the sole inferential population; A/A never enters its mean, variance, degrees of freedom or decision. No interim endpoint estimates, early success/failure looks, exclusions, replacements, resumed epochs, optional stopping or pooling with A1 are allowed. A technical failure preserves every artifact and leaves A2 incomplete; it does not authorize an automatic restart.

Choose a fresh frozen 32-hex batch namespace. Keep the A1 transformation: `clientbench/rfc263/` + first **48** hex digits of `SHA256(namespace + NUL + original_prefix)` + `/`, exactly 68 bytes. All 176 timing prefixes and the 16 reserved diagnostic prefixes below must be unique and disjoint from **all 544 prefixes reserved by A1 and its invalid predecessor**, not merely from written keys. Freeze complete schedules, namespace and independently pinned prefix ancestry before sampling. Namespace changes transform only prefixes, not the fixed randomization or sizes.

## Precision sizing and power assumption

A1 supplies **six pilot AB log ratios per epoch**. Use its separate epoch variances, without pooling epochs as interchangeable samples:

| Endpoint | Epoch 1 log variance | Epoch 2 log variance | Within-epoch pooled log SD | A1 point ratio |
|---|---:|---:|---:|---:|
| Window B→C | 0.0050649265637420044 | 0.0017945565759612456 | 0.058564 | 1.019868 |
| Lifecycle E−A | 0.004322715874684138 | 0.001128599168129425 | 0.052208 | 1.013308 |

For projected `n` AB pairs per epoch, use `SE = 0.5 × sqrt((s1²+s2²)/n)` and Welch df `(s1²+s2²)² / ((s1⁴+s2⁴)/(n−1))`. The two-sided 95% log half-width is `h=t(.975,df)×SE`; the ratio half-width `(upper−lower)/2` at point ratio `R` is `R×sinh(h)`.

The owner selected **80 AB pairs, 40/epoch**, for approximately 80% NI power for both endpoints under the **true-ratio-1.00 alternative**, rather than stopping at a width-only design. Freeze that N before sampling. With the pilot variances, the projected standard errors are below the approximate `0.02/2.8 ≈ 0.0071` power target:

| Quantity at 40 AB pairs/epoch | Window B→C | Lifecycle E−A |
|---|---:|---:|
| Projected log SE | 0.006547654 | 0.005837013 |
| Welch df | 63.553830 | 58.065096 |
| `t(.975,df)` | 1.997999794 | 2.001669705 |
| Ratio half-width at R=1.02 | 0.013344236 | 0.011917718 |
| Normal plug-in NI power at true R=1.00 | **84.8%** | **91.8%** |
| Univariate noncentral-t approximation | 84.6% | 91.6% |

The normal plug-in power is `Φ(log(1.02)/SE − t(.975,df))`, with fixed pilot variances. The noncentral-t approximation uses projected Welch df and noncentrality `log(1.02)/SE`; it is not an exact joint Welch calculation. The within-epoch pilot cross-covariances are `0.004650606137888858` and `0.0014044889756557417`, implying aggregate correlation **0.9902038993** between the two endpoint mean estimates. Assuming those covariances also persist, deterministic bivariate-normal quadrature forecasts **84.8% joint power** for both endpoints to pass. As a sensitivity check, treating endpoint statistics as independent would give **77.8% joint power**. Marginal powers must not simply be called joint power.

Assumptions are independent approximately normal paired log ratios within each epoch and future within-epoch variances/cross-covariances equal to the A1 pilot estimates; epochs retain equal weights and may have different variances. Each pilot variance/covariance uses only **six pairs per epoch**, so these are planning approximations, **not guaranteed precision or exact joint power**. Both forecast half-widths remain below 0.02, but width is not an additional decision rule or a reason to extend the sample. Replay the calculation with the explicit-input, internally locked `tools/benchcampaign/campaigns/rfc-263/cpu_a2_power.py` and the pinned A1 results/index/pair rows; A1 `pair-rows.json` SHA256 is `827b3eaadf79db84d93a3605953f1dc70594a0cc2c191267ab5301e2b1c9bdf8`. The qualified local planning receipt records SciPy 1.16.3 / NumPy 2.3.4; an independent stdlib calculation agreed.

The favorable power assumption is **true ratio 1.00**, not the A1 observed effect. If the window's true ratio is its observed **1.019868**, the normal plug-in PASS probability is only **2.4%**; at the 1.02 margin the upper-95%-bound test has nominal 2.5% rejection probability. Keeping the A1 centers with the projected 80-pair precision would yield approximately window `[1.00661, 1.03330]` and lifecycle `[1.00154, 1.02522]`, not NI PASS. No favorable true effect or universal stationarity is asserted from A1. No additional pairs or further confirmation follow an unresolved A2.

## Endpoints, inference and exact decision

For each process `N=completed=20,000`, with full measured/warmup populations:

1. **Window B→C:** `(stats_after − stats_before).(process_user_cpu_us + process_system_cpu_us) / N`.
2. **Lifecycle E−A:** `(lifecycle.stats_after_shutdown − stats_after_identity).(process_user_cpu_us + process_system_cpu_us) / N`.

Both are process CPU, not elapsed latency. Lifecycle includes startup, fixture, warmup, validation, CSV and deferred cleanup/drain through shutdown after the identity snapshot; it is not steady-state CPU. Retain A1's corrected lifecycle guard: completed 2–16-round producer fixpoint, executed=queued for finalizers and cleanups at exit, valid/nondecreasing counters, sufficient GC and every other barrier/closure/resource check. Newly queued cleanup during forced GC remains allowed. Missing/nonpositive endpoint deltas, source/config/identity drift or incomplete populations invalidate the campaign.

For each endpoint, form AB paired logs by alias/source identity, never by execution position: `d=log(production CPU/tx)−log(baseline CPU/tx)`. With `n1=n2=40`, equal-epoch mean is `m=(m1+m2)/2`; `SE=0.5×sqrt(s1²/40+s2²/40)`. Use Welch–Satterthwaite df `(s1²/40+s2²/40)² / ((s1²/40)²/39+(s2²/40)²/39)` and two-sided 95% interval `exp(m ± t(.975,df)×SE)`. If both variances are zero, the interval collapses to the point. All 80 AB pairs must be present. No repeated-measures or simultaneous-confidence claim is made.

Apply this order using full precision:

1. **FAIL** if either A2 point estimate is **≥1.02**. FAIL has precedence, including equality/degenerate intervals.
2. Otherwise **PASS** only if **both upper 95% bounds are ≤1.02**.
3. Otherwise **UNRESOLVED**; **park RFC-263**. No extra pairs or follow-on confirmation.

For final CPU reporting, replace only the two retained A1 grv-w8 entries with A2's entries, preserve the other 14 unchanged, and apply the same full 16-bound rule. A2 PASS therefore implies combined CPU PASS, not correctness, throughput, tail latency, queue-wait or merge approval. Improvement may be claimed for either A2 endpoint only if its upper bound is **strictly <1.00**.

A/A has **four pairs per role overall**, two per epoch. Report each pair ratio, each epoch's geometric mean and the overall equal-epoch geometric mean separately for baseline/production and both endpoints. No A/A CI, hard gate, exclusion or claim of equivalence follows from these small descriptive populations. Retain allocations, bytes, GC and retry counts as secondary context, not alternative acceptance endpoints.

## Sampling slots, preservation and time estimate

The controller stays nice 19 on `{0,1,6,7,11,12,13,18,19,23}`. Sampling requires exclusive owner approval for each epoch, a hard absolute UTC deadline **≤30 minutes from launch**, the internally owned fast lock, existing nonempty process/container/load gates and 60-second quiet settle. Keep the two-second pre-process quiet check. Each child remains bounded by `min(120 seconds, remaining authorized deadline−5 seconds)`. No duration-based exclusions are allowed.

For scale, A1's 32 grv-w8 process receipts span approximately **0.665–0.715 seconds**, mean **0.713 seconds**, through process exit. Eighty-eight such processes plus the existing two-second pre-process settle and 60-second initial quiet period project about **299 seconds per epoch before gate/hash overhead or stalls**. Conservative operational estimate: **10–12 minutes per epoch**, **20–24 minutes total sampling slots** under comparable quiet conditions. Use a **15-minute admission forecast per epoch**, not an empirical worst-case guarantee; each authorized sampling hard stop still remains at most 30 minutes. No new timing probe is authorized to refine this estimate.

Carry over A1's supervisor-owned process group, inherited validated lock descriptor, subreaper, unconditional KILL after TERM grace, teardown proof and single authoritative seal. After proven teardown, allow only **≤300 seconds of light administrative forensics**, chunk-cap hashing at 295 seconds with five seconds reserved for terminal status. Record UTC start/deadline/end. If teardown cannot be proved, enter unsealed **QUARANTINE**, retain the lock passively, immediately notify the owner, and perform no hashes/new work. Only the owner's exact ACK/release decision can end quarantine; disappearance alone is insufficient. Runtime job timeouts must not silently kill the guardian. These controls are not relaxed for the smaller population.

Seal both complete 88-process epochs before any endpoint/quantile/comparative analysis. Preserve frozen manifest hash, both external epoch seals, argv/environment, full CSVs, summaries, manifests and all failed attempts. Analysis runs offline with its own lane lock and the same controller resources; verify source/config/input bytes before and after.

## Reserved queue-wait stage and publication

Freeze the unchanged **16-process** diagnostic factorial and its fresh A2 namespace before confirmation sampling: all four workloads × baseline/production × mutex/block, 32 workers, 500,000 measured + 2,000 warmup, using the existing diagnostic seed/order and prebuilt SDK pprof. This is separate from A2's 176 timing processes and never enters its estimates. The new A2 frozen source tree is used for this later stage because A1's live-root source guard cannot truthfully describe subsequent authorized Python/documentation amendments.

**Do not collect these profiles until A2 PASS and a separate owner go.** A2 diagnostic admission additionally requires explicit `--confirmation-results` and `--confirmation-results-sha256` arguments for the completed analysis. Verify the exact buffered receipt against that independent SHA, current frozen-manifest hash, both complete 88-process epoch seals, identities, both new grv-w8 point/upper-bound criteria and the unchanged retained 14 bounds. Copy/rebind that derived PASS receipt into the diagnostic population for its final seal; recheck drift. This post-timing output cannot exist at initial preparation: it is an additional bound admission receipt, not permission to change the initial frozen inputs, source or schedules. Owner authorization remains separately mandatory. Preserve A1's profile validation, exact archived source/binary binding, native pprof positive controls and strict production gate: combined shard+root cumulative queue wait divided by the same profiled process's measured-window CPU **<1% in each of eight production workload/profile cells**. Baseline's eight cells are fully validated context. Mutex means Unlock-holder context; block means Lock-waiter context. Never add their delays or call cleanup-holder attribution proof that cleanup itself waited. Diagnostic chronology must follow both complete A2 timing epochs; population guards must require exactly 176 A2 timing receipts plus 16 profiles, not weaken to arbitrary counts.

Only after confirmation/diagnostic collection and analysis, follow the owner's existing **one documentation-only results commit** exception, with a nonempty allowlisted path census and zero production/tool/build changes relative to the final sampling seal. The original publication parent was `a39c3c6bed42ccf5a81f022044d0fb2560bd4735`. After sampling, the owner superseded it with current master `cc3e77a074709d7c6edd590e79b2d46c561f714c` (#972): rebase first, check range-diff and production-input bytes, then form one separate-branch squash with the rebased final tree and rerun the privacy proof. Distinguish the allowlisted results delta from unrelated new-master changes; preserve the original measured source/binary identities. Await all remaining review/CI/owner gates. No push is authorized.
