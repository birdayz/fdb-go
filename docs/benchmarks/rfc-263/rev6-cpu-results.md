# RFC-263 revision 6: CPU qualification and queue-wait results

**Result: CPU non-inferiority PASS; separate queue-wait diagnostic PASS.** These are scoped performance decisions, not merge approval. Owner-run full/full-race, five final-HEAD reviews, CI and owner approval remain pending. B3a/B3b and the broader client-CPU work remain separate.

The historical **A1 result remains UNRESOLVED**. The outcome-informed, owner-approved [A2 plan](rev6-cpu-a2-grv-confirmation.md), sealed before its first sample, replaces **only the two grv-w8 bounds**, without pooling. The other fourteen A1 bounds are retained unchanged. The combined sixteen-bound decision is PASS. The cells with upper 95% bounds **strictly below 1.00 for both endpoints** are **grv-w8 (A2), grv-w32, set100-w8 and set100-w32 (A1)**. No other cell is called an improvement.

## Populations, identities and endpoint scope

All ratios are production/baseline process CPU per completed measured transaction, not elapsed latency or server CPU. The frozen baseline is `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`; its executable SHA256 is `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e`. The production executable SHA256 is **`02d86dcde2f70868c116aeed225c7ef0d8a626da9b8501fb0f06a2e668c41949`**, reused byte-identically throughout A1, A2 and the diagnostics. It was not rebuilt from the later Python/documentation trees. Actual Bazel SDK: **Go 1.26.9**. Harness archive SHA256: `cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400`.

- **Window B→C:** `(stats_after−stats_before).(process_user_cpu_us+process_system_cpu_us)/N`.
- **Lifecycle E−A:** `(lifecycle.stats_after_shutdown−stats_after_identity).(process_user_cpu_us+process_system_cpu_us)/N`. This includes startup, fixture, warmup, validation, CSV and deferred cleanup/draining through shutdown after identity hashing. It excludes final summary output/process exit and is **not steady-state CPU**.

The [fixed CPU plan](rev6-cpu-plan.md) uses equal-weight epoch means of paired logs and Welch–Satterthwaite two-sided 95% intervals. A1 has six AB pairs per epoch/cell; A2 has forty per epoch for grv-w8 only. **FAIL has precedence if any point is ≥1.02; otherwise PASS requires every upper bound ≤1.02; otherwise UNRESOLVED.** Improvement requires upper bound strictly <1.00. These are per-endpoint intervals, not simultaneous 95% coverage. Decisions use unrounded results.

| Population | Processes | AB / AA pairs | Measured / warmup transactions | Status |
|---|---:|---:|---:|---|
| A1, eight cells, two complete epochs | 256 (128/epoch) | 96 / 32 | 66,560,000 / 512,000 | UNRESOLVED: only grv-w8's two upper bounds exceed 1.02 |
| A2, grv-w8 only, two complete epochs | 176 (88/epoch) | 80 / 8 | 3,520,000 / 352,000 | PASS; supersedes only A1 grv-w8 |
| Separate 32w diagnostic factorial | 16 | Not CPU inference | 8,000,000 / 32,000 | PASS; eight production workload/profile gates |

Eight-worker timing uses 20,000 measured + 2,000 warmup transactions/process; 32-worker timing and all profiles use 500,000 + 2,000. Every process is fresh, pair members are adjacent, prefixes are unique, and complete schedules are frozen before sampling. A2's 192 timing/profile prefixes are disjoint from all 544 reserved A1/invalid-predecessor prefixes. No samples, pairs or epochs were excluded, replaced, extended or pooled; no A2 outcomes were read before both complete epochs were sealed. A2's initial forty-pair proposal was superseded by the fixed eighty-pair plan **before any A2 sample**, not enlarged after a look.

Both arms use the existing real cluster and pure-Go backend, client affinity `2,3,14,15`, `GOMAXPROCS=4`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`, and no inherited network-option overrides. Collection/analysis use nice 19, the reserved controller affinity and the internally owned exclusive lock. Profiles are disabled for all CPU inference. The full source census (8,055 files for A1; 8,061 for A2/diagnostics, plus three external-configuration records in each) and frozen helper/binary/archive/configuration hashes were checked before and after each population and analysis. A1→A2 changed only approved Python/documentation inputs: compiled production Go, clientbench, BUILD/module/toolchain inputs and executable bytes remained identical.

## A1: all sixteen original bounds, retained as historical results

Each entry is **point ratio [95% lower, upper]**. There are twelve AB pairs/cell, six per epoch. Original verdict: **UNRESOLVED**, not FAIL and not PASS. No point reached 1.02; fourteen upper bounds met ≤1.02. The two grv-w8 entries below remain reported but are superseded for the combined decision.

| Cell | Window B→C | Lifecycle E−A | Final use |
|---|---|---|---|
| grv-w8 | **1.019867983 [0.980993752, 1.060282699]** | **1.013308117 [0.978249500, 1.049623169]** | Superseded by A2, never pooled |
| get100-w8 | 0.977881330 [0.946176308, 1.010648743] | 0.977403823 [0.950129542, 1.005461036] | Retained: NI, not improvement |
| set100-w8 | 0.983058079 [0.973027257, 0.993192307] | 0.987529204 [0.977806077, 0.997349015] | Retained: both endpoints improve |
| range100-w8 | 1.001595252 [0.990529388, 1.012784739] | 1.005493553 [0.991844292, 1.019330649] | Retained: NI, not improvement |
| grv-w32 | 0.965050263 [0.954602510, 0.975612363] | 0.964514158 [0.954721355, 0.974407407] | Retained: both endpoints improve |
| get100-w32 | 0.998436474 [0.988429754, 1.008544500] | 0.998030456 [0.988359930, 1.007795602] | Retained: NI, not improvement |
| set100-w32 | 0.985630697 [0.978754668, 0.992555033] | 0.985659935 [0.978807309, 0.992560535] | Retained: both endpoints improve |
| range100-w32 | 0.996070749 [0.988810270, 1.003384540] | 0.996186918 [0.989422009, 1.002998081] | Retained: NI, not improvement |

### A1 A/A observations: descriptive, not a gate

One pair per role/epoch; two pairs per role/cell overall. Each column lists **epoch 1 / epoch 2 / overall geometric mean**. There is no A/A CI, hard gate or equivalence claim. In particular, grv-w8 production-AA is visibly below 1; these small controls do not establish stable noise, erase A1's unresolved bounds or supply an alternative decision rule.

| Cell | Same-source role | Window B→C: E1 / E2 / GM | Lifecycle E−A: E1 / E2 / GM |
|---|---|---|---|
| grv-w8 | baseline | 0.956105 / 1.035563 / 0.995041 | 0.963476 / 1.031322 / 0.996822 |
| grv-w8 | production | 0.884159 / 0.998996 / 0.939825 | 0.900178 / 0.990817 / 0.944411 |
| get100-w8 | baseline | 1.033605 / 1.057946 / 1.045704 | 1.034428 / 1.044862 / 1.039632 |
| get100-w8 | production | 0.984500 / 0.996649 / 0.990556 | 0.984661 / 0.980702 / 0.982680 |
| set100-w8 | baseline | 0.963691 / 1.019284 / 0.991098 | 0.953131 / 1.010830 / 0.981556 |
| set100-w8 | production | 0.990625 / 0.970790 / 0.980657 | 0.979843 / 0.963560 / 0.971668 |
| range100-w8 | baseline | 1.005672 / 0.991565 / 0.998593 | 1.004157 / 0.993367 / 0.998748 |
| range100-w8 | production | 1.003296 / 1.012018 / 1.007648 | 0.997390 / 1.012691 / 1.005012 |
| grv-w32 | baseline | 0.989842 / 1.000653 / 0.995233 | 0.998230 / 0.995772 / 0.997000 |
| grv-w32 | production | 1.006733 / 1.020129 / 1.013409 | 1.004889 / 1.018772 / 1.011807 |
| get100-w32 | baseline | 0.993154 / 0.998957 / 0.996052 | 0.992827 / 0.999066 / 0.995942 |
| get100-w32 | production | 0.998348 / 0.993770 / 0.996056 | 0.997067 / 0.993328 / 0.995195 |
| set100-w32 | baseline | 1.002835 / 1.003319 / 1.003077 | 1.003046 / 1.002556 / 1.002801 |
| set100-w32 | production | 1.000779 / 0.987828 / 0.994282 | 1.001560 / 0.987746 / 0.994629 |
| range100-w32 | baseline | 1.003739 / 1.005076 / 1.004407 | 1.003228 / 1.005029 / 1.004128 |
| range100-w32 | production | 1.005719 / 0.991639 / 0.998654 | 1.004932 / 0.990824 / 0.997853 |

## A2: superseding grv-w8 confirmation

A2 is an **outcome-informed amendment following A1**, not part of A1's original preregistration. The owner fixed eighty AB pairs (forty/epoch; twenty of each execution order/epoch), plus four AA pairs/epoch, before sampling. Fixed seed: 263107. Both complete 88-process epochs passed all identity, lifecycle, measured/warmup CSV and source/hash checks. Epoch 1 ran 2026-10-21 01:17:44.752829–01:23:12.213627 UTC; epoch 2 ran 01:24:11.686898–01:29:39.148467 UTC. Each bounded light forensic tail finished about 0.11 seconds later; neither entered quarantine.

| Endpoint | A2 point [95% lower, upper] | Epoch 1 / epoch 2 point ratios | Decision |
|---|---|---|---|
| Window B→C | **0.974409096 [0.961230466, 0.987768407]** | 0.980515554 / 0.968340668 | NI and strict improvement |
| Lifecycle E−A | **0.975915653 [0.964107817, 0.987868104]** | 0.980356517 / 0.971494905 | NI and strict improvement |

A2 target verdict and combined sixteen-bound verdict are both **PASS**. Only these two grv-w8 entries replace A1. No additional timing is used or authorized by this result. Range100-w8 remains a slightly above-baseline point estimate in both endpoints; its bounds satisfy the declared **2% NI margin**, not literal neutrality or improvement.

### A2 A/A: every pair and epoch summary

Four pairs per role, two/epoch. Ratios follow the scheduled alias contrast, not arbitrary first/second execution position. `order` is the frozen 0/1 execution-order code. All values are descriptive; there are no CIs, exclusions, hard gate or equivalence/stationarity claims.

| Pair | Role | Order | Window B→C ratio | Lifecycle E−A ratio |
|---|---|---:|---:|---:|
| e1-v03-grv-w8 | baseline | 0 | 1.051084259 | 1.064038974 |
| e1-v09-grv-w8 | production | 0 | 1.011799935 | 1.004214679 |
| e1-v18-grv-w8 | baseline | 1 | 1.065730293 | 1.069646750 |
| e1-v42-grv-w8 | production | 1 | 1.029243911 | 1.017332069 |
| e2-v09-grv-w8 | production | 0 | 1.002225333 | 1.000631219 |
| e2-v10-grv-w8 | baseline | 1 | 0.979171577 | 0.974193480 |
| e2-v13-grv-w8 | production | 1 | 0.946778791 | 0.948170217 |
| e2-v22-grv-w8 | baseline | 0 | 0.953039492 | 0.952778282 |

| Endpoint / role | Epoch 1 GM | Epoch 2 GM | Overall equal-epoch GM |
|---|---:|---:|---:|
| Window / baseline | 1.058381942 | 0.966017175 | 1.011145456 |
| Window / production | 1.020484651 | 0.974107637 | 0.997026525 |
| Lifecycle / baseline | 1.066839177 | 0.963426380 | 1.013815075 |
| Lifecycle / production | 1.010752095 | 0.974047596 | 0.992230139 |

The A2 planning approximation assumed true ratio 1.00 and persistent pilot variances/covariances: marginal normal plug-in powers 84.8%/91.8%, joint 84.8% using pilot correlation 0.9902038993; independence sensitivity 77.8%. At A1's observed window ratio near 1.02, forecast PASS probability was only 2.4%. Pilot covariance used six pairs/epoch. These are conditional planning approximations, **not guaranteed power or post-hoc proof of stationarity**; the favorable true effect was not assumed from the original result.

### Secondary allocation/GC context

AB treatment-run means are context only, not acceptance endpoints. Each entry is baseline → production. GC is mean window cycle count per process, not per transaction; retries and warmup retries are zero throughout both CPU timing populations.

| Population / cell | Allocations/tx | Allocated bytes/tx | GC cycles/process |
|---|---|---|---|
| A1 grv-w8 | 27.750375 → 25.759625 | 2948.022 → 2835.575 | 11.667 → 11.917 |
| A1 get100-w8 | 51.815796 → 49.816933 | 5330.318 → 5202.296 | 21.583 → 21.500 |
| A1 set100-w8 | 55.872000 → 53.867271 | 6410.605 → 6281.993 | 26.000 → 25.917 |
| A1 range100-w8 | 58.336496 → 56.332438 | 43923.544 → 43793.838 | 191.083 → 190.167 |
| A1 grv-w32 | 27.262871 → 25.258303 | 2912.511 → 2795.639 | 14.000 → 14.000 |
| A1 get100-w32 | 51.472127 → 49.470438 | 5318.317 → 5188.137 | 26.000 → 25.917 |
| A1 set100-w32 | 55.586713 → 53.582588 | 6429.353 → 6299.097 | 31.917 → 31.833 |
| A1 range100-w32 | 57.004570 → 55.001704 | 43843.726 → 43715.313 | 213.833 → 211.667 |
| A2 grv-w8 | 27.758019 → 25.754943 | 2949.826 → 2834.672 | 11.963 → 11.963 |

## Separate queue-wait diagnostic: PASS

After both A2 epochs and their PASS analysis, a separate owner authorization admitted the frozen sixteen-process factorial: all four workloads × baseline/production × mutex/block, **32 workers only**. Sampling ran 2026-10-21 **01:33:56.537037–01:39:30.260456 UTC**, below its hard 02:03:56.537037 deadline. The forensic tail finished **01:39:30.484891 UTC**, within its separate five-minute cap. Exact `d-r000`…`d-r015` population, successful lifecycle/CSV/identity checks, input hashes verified after collection, proved teardown, no quarantine; all sixteen native profiles are nonempty and positive classifier controls pass.

Criterion: **(shard + root cumulative queue delay) / same profiled process's measured-window CPU <1% in EACH of eight production workload/profile cells.** Baseline's eight profiles are validated context, not extra gates. The exact-source-line classifier binds each profile to its matching frozen binary and archived queue source: mutex **Unlock sites attribute waiting to the unlocking holder context**; block **Lock sites identify the blocked waiter context**. Broad queue ancestors and descendant transaction-cancel/read-error/allocator waits are excluded.

All recorded queue delay is shard/caller context; **root caller, shard drainer and root drainer are zero sampled delay in every production profile**. The table gives the complete nonzero caller split. `schedule` and `cleanup-remove` mean holder context for mutex, waiter context for block. In particular, **mutex cleanup-remove attribution does not prove cleanup itself waited**.

| Run / workload | Profile | Shard+root delay (ns) | Same-process window CPU (ns) | Delay / CPU (%) | Caller schedule (ns) | Caller cleanup-remove (ns) |
|---|---|---:|---:|---:|---:|---:|
| d-r001 / grv | mutex | 414104 | 5668334000 | 0.007305568 | 352798 | 61306 |
| d-r002 / grv | block | 559829 | 6374885000 | **0.008781790** | 449481 | 110348 |
| d-r004 / get100 | block | 522459 | 16405731000 | 0.003184613 | 522459 | 0 |
| d-r007 / get100 | mutex | 1194516 | 15883143000 | 0.007520653 | 849218 | 345298 |
| d-r009 / range100 | mutex | 1294741 | 30034131000 | 0.004310899 | 420536 | 874205 |
| d-r010 / range100 | block | 1311924 | 31306596000 | 0.004190567 | 1301154 | 10770 |
| d-r012 / set100 | block | 16731 | 20059082000 | 0.000083409 | 16731 | 0 |
| d-r015 / set100 | mutex | 20037 | 19541198000 | 0.000102537 | 0 | 20037 |

All eight are strictly below 1%; maximum is **0.008781790%**, not 0.8781790%. Baseline has no queue source and all eight classified queue delays are zero, only accepted after nonempty native profiles, exact archived-source absence checks and decoder positive controls. Mutex/block are different views that can count the same contention: **never add them**, treat them as matched events or interpret cumulative goroutine wait as CPU consumed. Profile enable/disable brackets the measured phase slightly more broadly than the CPU snapshots. Sampled zero is not proof that a path never waits. These diagnostic timings never enter or alter CPU NI and are not eight-worker profile evidence.

## Retained adverse evidence, invalid attempts and limits

- The [earlier production screen](rev6-production-tail-results.md) still reports **range 8w CPU +1.66%**, above baseline, and the [p999 confirmation](rev6-confirm-results.md) reports descriptive **get 8w CPU +1.69%**, also above baseline. They are not rewritten as neutral. The later formal CPU campaign applies its explicit 2% NI rule to separate populations, rather than silently redefining those older results. It does not establish literal neutrality, a universal CPU win or attribution of the difference between batches.
- Original get/set 8w p999 **+9.30%/+8.94%**, set mean **+0.104% with 6/6 adverse pairs**, and the adverse one-run-per-arm range trace remain recorded. Historical range-tail non-reproduction is not equivalence or a causal explanation; there were no get/set traces in that population.
- The owner-cleared fixed p999 confirmation remains a separate 1,152-process, 23.04-million-operation population: get **0.951989616 [0.901781407, 1.004993251]**, set **1.008473212 [0.976863672, 1.041105579]**, prescribed 97.5% intervals with both uppers <1.05 and all four A/A intervals containing 1. Get A/A log SD **0.25–0.27 > planned 0.15** means planned power was **not established** despite clearance. Set epoch ratios **0.964966→1.053942**, and order/serial caveats, prohibit universal tail-stability claims. Neither CPU nor profiles erase those limitations.
- The invalid pre-A1 CPU epoch is preserved, not analyzed for CPU endpoints: its sole process passed the producer fixpoint, but the Python guard wrongly demanded cleanup queued-at-exit equal queued-at-entry. [A1's outcome-blind amendment](rev6-cpu-plan.md#amendment-a1-lifecycle-fixpoint-2026-10-20) removes **only** that predicate in the CPU-specific helper. Completed 2–16 rounds, executed=queued at exit, counters/GC and all other canonical lifecycle checks remain. Both baseline and production register cleanup. The shared clientbench validator is unchanged here and has a separate open TODO/PR follow-up. The original written prefix was cleared only under separate administrative authorization and verified empty; new populations use disjoint frozen namespaces.
- Earlier invalid/unlocked fixture and proof-runner attempts remain preserved in the existing evidence. A2 validation attempt 1 failed before fixtures on an off-tree import path; attempt 2 passed legacy guards but exposed only the planner's near-singular correlation fixture (`0.9999999999999999` versus 1). A four-ULP singular snap in power planning fixed that boundary without changing actual pilot correlation/power; the fixture was not weakened. Attempt 3 passed all **86 fixture groups**, plus n=6-in-A2 and unrestricted-source mutants red/restored green, and the actual real hook's **157 fast targets (1 actual, 156 cached)**. These are historical fast/fixture receipts, not full-suite or final-publication review evidence.
- No final-HEAD full/full-race lane, official five-review gauntlet, published CI or merge approval is claimed here. Historical correctness evidence, opt/race/mutation proofs and acceptance assumptions remain in [production evidence](rev6-production-evidence.md). Lower allocations, low sampled contention and CPU NI do not alone establish throughput, tail stability, bounded repeated-resurrection retention or deferred B3 behavior.

## Sealed receipts and portable replay

Raw CSVs, profiles, executables, source archives and operational launch receipts remain outside Git beneath `${FDB_PERF_HOME}/validation/rfc263-production/`. Public reports contain no raw private machine paths or cluster contents. Each seal below is the SHA256 of that population's `artifact-sha256.json`, independently retained outside the sealed population.

| Artifact | SHA256 |
|---|---|
| A1 frozen manifest (`cpu-gate-a1-sealed-55860c9-1/frozen.json`) | `154a783499a59a5e26e3bdffbf19631b99cd2a41b67261c34556eb187493dd62` |
| A1 epoch 1 / epoch 2 seals | `da405065ee33e0da348a2c8b6044429183c7f4a6ade7ac8f64ae55fbbc22b2e2` / `dbde586f5313e3a635a8b8f306079384a38451f7c60c885c04699f3d56d90472` |
| A1 results / analysis index | `6c700352c97e190fbc1cadf7d0a597fa6eb3f16a10bf7fb06144c9a82377deeb` / `6c4cd85cd82589cc11dcc6d11721d5511b0d649a59e0b26331a8cf1b0964d8c5` |
| Original invalid epoch seal | `5d5f09a876ff4f3c09bf594929c141c1377e913435ea3597cd3b61fafcb0117a` |
| A2 as-run plan / frozen manifest | `1d17ad5ed1c3644a3351b36568ef8d3fc47f362bb209c3a4877576aad51b47b3` / `3aba5ff3734df631842f776f4f2cd9932be0f3f8ed77658abcefced6e4cb4d2f` |
| A2 epoch 1 / epoch 2 seals | `d12cd8100884a12efb137546387206ecebb6f8f6085c4a63cad6a717243bc06a` / `85c2640a4aae9bd12c6e3e97c46792f64e9a44e96b3cbca9978dabdadc9212dd` |
| A2 results / analysis index | `797cfd9a7228f310ab7a9da5e5f774c17d025aa98719142633e9304249ffc205` / `694c1d6b754ccf1dd602c57df2ca92232a14228356f14f4b3837e7eb11589933` |
| A2 validation attempt 3 index | `c1c18f7021f1efde0d0abd0e9425083a2726c690ab5303864c0b157c26a36603` |
| Diagnostic population seal | `5be33f1e1be89e5574c1c663a604644952702483ebe51316adda46f5e0103fd6` |
| Queue-wait results / analysis index | `b02cbcc5c62a8b398dc7c44236ffa7fee5342b1bd7e635aeee00414cbcb18e0b` / `c6762ce01347fe33e62478de1a8224340655dc5a1cda4e733d7d337f86aa9f5a` |
| Prebuilt SDK pprof executable | `2d6c3b8020d92ad04649cbe8748ab39a12217b8d9f18e38e176f8b9c6af5e9a8` |

A2 and diagnostic `source_revision` is **`tree:e3891d15e100a731a4cec7b48fc417067bceda0a`**, sealed private commit `989b186a28a37aaf256fd79535c1e4700207cc5e`. Production queue-source SHA256 is `83e48f0993fbda349104ac9fcc4aaebbae6cc5f9b8804d881b776e0289ef26c6`. Baseline source archive SHA256 is `f6631905ffb8ec8b634ff9bb8a1fd26244d55ded8c3b77b1f7589e52bc8ab528`. A later results/publication tree is **not** claimed to have been sampled; header/status/link edits do not change the archived as-run plan.

Run offline analysis with explicit receipt pins and fresh output directories, under the recorded nice/affinity wrapper; each analyzer acquires its lock internally, so **no outer flock**. `cpu_analyze.py` consumes the complete A1 evidence and two seals; `cpu_a2_analyze.py` consumes the complete A2 evidence/frozen pin/two seals and verifies the independently pinned retained A1 result/index. Both use the recorded NumPy 2.3.4 / SciPy 1.16.3 environment. For diagnostics:

```sh
uv run --offline --no-project python "$P/rev6-cpu-queuewait.py" \
  --evidence "$A2_EVIDENCE" --output "$NEW_QUEUEWAIT_OUTPUT" \
  --collector-helper "$A2_EVIDENCE/inputs/cpu_collect.py" \
  --frozen-sha256 "$A2_FROZEN_SHA256" \
  --pprof-binary "$A2_EVIDENCE/bin/pprof" --pprof-sha256 "$PPROF_SHA256" \
  --seal1 "$A2_EPOCH1_SEAL" --seal2 "$A2_EPOCH2_SEAL" --seal "$DIAGNOSTIC_SEAL"
```

`P` is the portable campaign-tool directory from [reproduction.md](reproduction.md). Strict source/config guards require the archived as-run checkout for replay, not a later documentation or rebased publication checkout. No collection authorization follows from these replay commands.

## Results-only exception and publication boundary

After all sampling and analysis, the owner authorized one nonempty **documentation-only results commit** relative to the final sampling seal. Its nine changed paths are this report, `README.md`, `pr-body.md`, `reproduction.md`, `rev6-cpu-plan.md` and `rev6-cpu-a2-grv-confirmation.md` under `docs/benchmarks/rfc-263/`, plus `rfcs/263-transaction-timer-and-incarnation.md`, `TODO.md` and `docs/benchmarks/README.md`. The owner explicitly extended the allowlist by **exactly that last path for one RFC-263 summary-line correction only**, stating that CPU, p999 and queue-wait gates passed while merge gates remain pending. The complete census is restricted to that amended allowlist; production, tools, build/module inputs and external configuration are unchanged. The retained binary is still the exact sampled executable above.

Publication is a separate operation: first rebase onto the owner's updated current-master parent **`cc3e77a074709d7c6edd590e79b2d46c561f714c`** (#972), then form one separate-branch squash, without pushing. This supersedes the older A2 plan's `a39c3c6be` publication parent. Master contributes unrelated benchmark/website/script changes, so a direct sealed-tree→rebased-tree path list is **not falsely called doc-only**. The doc-only exception is proved before rebase; the publication proof separately attributes all additional paths to the exact master delta, checks the rebase range-diff and preserves every production/clientbench/build/module input byte. Final commit/tree, path census, privacy-grep, hook actual/cached counts and production-input proofs are retained in the off-tree publication receipt and reported to the owner. Those administrative checks do not change the performance populations or supply the still-open final review/CI gates.
