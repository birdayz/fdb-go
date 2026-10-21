# RFC-263 historical 32-worker range tail recheck

This is a retained-data comparison, not a new benchmark or attribution result. It
checks whether the selected Rev4 and Phase 1 records already contained adverse
range tails before the later Rev5 campaign. CPU gains do not establish acceptable
latency, and separate campaigns cannot establish that Rev5's regression is inherited.

**Result:** selected Rev4's unprofiled p99 is adverse in two of three pairs, with a
+6.725% ratio of run medians and a +1.496% median paired change. It is a warning, not
an all-three-adverse result. Phase 1 also has adverse p99 observations, but its only
retained 32-worker range populations are two separate single-pair mutex-profiled
comparisons. Do not pool them with one another or with unprofiled timing.

## Reproduce from sealed inputs

From the repository root, outside any reserved timing window:

```sh
python3 tools/benchcampaign/campaigns/rfc-263/rev5-historical-tail.py \
  --rev4-evidence /tmp/rfc263-rev4-screen.2KEIQu \
  --phase1-evidence /tmp/rfc263-phase1.owjNRR \
  --output /tmp/rfc263-rev5-tail.I5smMO/historical-tail.json
```

The standard-library script reads existing artifacts only and writes the requested
JSON outside the source checkout. It makes no network calls and starts no workload,
build, test, sampler, or profiler. It hashes the script, sealed source archives,
executables, metric indexes, and every consumed raw sample/summary/manifest file.
Expected source-archive and executable hashes are checked, not merely reported.

The recorded output is `/tmp/rfc263-rev5-tail.I5smMO/historical-tail.json`, SHA256
`dc12fdc0724328fc7408df8f80c487c8706a1d2aee56f331194ace7c221b1431`.
It contains all per-run values, identities, UTC starts, host load/idle observations,
and raw-file SHA256 values. Paths in that artifact refer to the retained local
archives; binaries and raw measurements are not checked into the repository.

The analysis validates **12 nonempty sample files and 5,200,000 measured rows**:
complete declared populations, all 32 workers represented, zero reported errors or
retries, and successful checks. Every run excludes 2,000 warmups. Recomputed p50/p99
match the retained metric rows exactly; mean and measured-window user+system CPU
also match. All use Go 1.26.9, GOMAXPROCS=4, GOGC=100, GOMEMLIMIT=off, empty GODEBUG,
range100, timeout 30,000 ms, retry limit 10, and harness identity
`cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400`.

For probability `p`, the quantile is:

```text
sorted_latency_ns[int((n - 1) * p)] / 1000
```

Compute quantiles independently for each run. The ratio of per-run medians and the
median of paired percentage changes are distinct statistics. Neither is a quantile
of pooled transaction samples. The script groups by the four explicit phases below;
it excludes diagnostic, discarded, other-workload, and other-prototype populations.

## Selected Rev4: unprofiled 32-worker range

Root: `/tmp/rfc263-rev4-screen.2KEIQu/timed/`.
Run names: `rev4-range4-w32-range100-p{0,1,2}-{baseline,reclaim}`.
Each run has **500,000 measured completions**, profile **none**. The three pairs
alternate arm order. Values are microseconds, baseline → selected Rev4.

| Pair | p50 | p90 | p99 | p99.9 |
|---|---:|---:|---:|---:|
| 0 | 688.033 → 685.338 | 1035.391 → 1032.266 | 2155.773 → 2316.308 | 4265.730 → 4673.723 |
| 1 | 682.864 → 686.120 | 1028.678 → 1031.413 | 2170.342 → 2202.813 | 4743.245 → 4475.408 |
| 2 | 700.117 → 692.121 | 1063.574 → 1038.116 | 2418.391 → 2372.274 | 5157.750 → 4842.293 |

| Pair | p50 change | p90 change | p99 change | p99.9 change |
|---|---:|---:|---:|---:|
| 0 | −0.392% | −0.302% | +7.447% | +9.564% |
| 1 | +0.477% | +0.266% | +1.496% | −5.647% |
| 2 | −1.142% | −2.394% | −1.907% | −6.116% |

| Statistic | Baseline median | Rev4 median | Ratio of medians | Median paired change |
|---|---:|---:|---:|---:|
| p50, µs | 688.033 | 686.120 | −0.278% | −0.392% |
| p90, µs | 1035.391 | 1032.266 | −0.302% | −0.302% |
| p99, µs | 2170.342 | 2316.308 | +6.725% | +1.496% |
| p99.9, µs | 4743.245 | 4673.723 | −1.466% | −5.647% |

p99 is adverse in two pairs and favorable in one; p99.9 is mixed. These records do
not demonstrate uniformly neutral tails, even though all three paired CPU changes
were favorable. They also do not show a consistent all-pairs tail regression.

## Separate mutex-profiled populations

These are **not** additional unprofiled acceptance pairs. Values are microseconds,
baseline → candidate; each row is its own single-pair population.

| Phase / run-name prefix | Measured per run | Profile | p50 | p90 | p99 | p99.9 |
|---|---:|---|---:|---:|---:|---:|
| Rev4 `rev4-mutex-draining4-w32-range100-p0-` | 500,000 | mutex | 688.444 → 694.155 | 1034.820 → 1041.543 | 2268.277 → 2413.001 | 4878.742 → 5253.101 |
| Phase 1 `mutex-w32-range100-p0-` | 100,000 | mutex | 735.683 → 756.663 | 1286.297 → 1392.948 | 2429.331 → 3095.794 | 5278.798 → 5156.076 |
| Phase 1 `mutex-draining-w32-range100-p0-` | 500,000 | mutex | 700.205 → 697.250 | 1065.217 → 1100.553 | 2485.153 → 2505.192 | 5017.437 → 5583.248 |

| Phase | p50 change | p90 change | p99 change | p99.9 change |
|---|---:|---:|---:|---:|
| Rev4 mutex draining | +0.830% | +0.650% | +6.380% | +7.673% |
| Phase 1 short mutex | +2.852% | +8.291% | +27.434% | −2.325% |
| Phase 1 mutex draining | −0.422% | +3.317% | +0.806% | +11.277% |

Rev4 uses the same root as the unprofiled table and arms `baseline` / `reclaim`.
Phase 1 uses `/tmp/rfc263-phase1.owjNRR/timed/` and arms `baseline` / `candidate`.
Its four matching 32-worker range directories are exactly the two pairs listed:
the script checks that nonempty directory census before reporting that no retained
unprofiled Phase 1 comparison exists. It fails if the population changes rather than
silently broadening the comparison.

The short Phase 1 candidate recorded host idle fraction 0.899 versus baseline 0.981.
That is an additional confound, not a reason to discard its adverse result. Single
profiled pairs cannot establish an unprofiled latency effect or its cause. Different
run lengths also expose different timer/GC histories; do not combine the 100k and
500k populations.

## Source and artifact identities

| Arm | Source identity | Executable SHA256 |
|---|---|---|
| Baseline | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e` |
| Selected Rev4 | tree `61d2597e0231dfbedcd898a27c078c3412efaf6a` | `2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95` |
| Phase 1 | tree `50432211dc325d7854dbdf72b24c986f424ccbd5` | `56a78c0d7454a0222db4d5f1f1de863cdb0b9d75672a4d741a376d90480a4e3c` |

| Retained source archive | SHA256 |
|---|---|
| Phase 1 root / `baseline-source.tar.gz` | `1dc00ff709355227c05c6f5b7317674c49e97f2274b68a72914a74436450e697` |
| Phase 1 root / `candidate-source.tar.gz` | `4ded661e094092314474e14b9020874b9a32011efbaf8fe6bd239b7c317861b9` |
| Rev4 root / `reclaim-source.tar.gz` | `4c3ec931e75bbece2e8e4556766e18360d785435f732d861ed6c1c4760b5f175` |

| Metric index | SHA256 |
|---|---|
| Rev4 root / `run-metrics.jsonl` | `1db9c9d9e002e18d0f2fcc49999ee011c1389edcefeb343f270acdf702a68e5a` |
| Phase 1 root / `run-metrics.jsonl` | `3a7cc5cb010e0730069a33a090e64170b5152e1faa772071d93ce35a3696590f` |

The baseline executable was independently hashed in both retained roots. Full raw
sample, summary, and manifest hashes are in `historical-tail.json`, not inferred
from run names. This historical comparison neither accepts the later Rev5 latency
trade-off nor attributes it to renewal, GC, scheduling, transport, or timer work.

## Separate eight-worker selected-Rev4 primary range check

This additional check reads only the six retained `run-metrics.jsonl` rows and their
summaries, not CSV samples. It does not change `historical-tail.json` or pool these
runs with the earlier 32-worker or profiled populations. The same executable/source
identities above apply. Run names under the Rev4 root are
`rev4-screen4-w8-range100-p{0,1,2}-{baseline,reclaim}`: 20,000 measured completions,
2,000 warmups excluded, eight workers, profile **none**, zero errors/retries per run.

| Pair | Mean µs, baseline → Rev4 | Mean change | p99 µs, baseline → Rev4 | p99 change |
|---|---:|---:|---:|---:|
| 0 | 474.450491 → 472.501216 | −0.411% | 949.188 → 934.009 | −1.599% |
| 1 | 472.021187 → 471.138536 | −0.187% | 921.726 → 919.151 | −0.279% |
| 2 | 474.988085 → 474.570036 | −0.088% | 939.990 → 984.695 | +4.756% |

Mean is favorable in all three pairs; p99 is favorable in two and adverse in one.
The ratio of run medians is mean −0.411% (474.450491 → 472.501216 µs), p50 −0.346%
(448.379 → 446.827 µs), and p99 −0.636% (939.990 → 934.009 µs). Thus the original
selected-Rev4 eight-worker screen did not have an all-pairs-adverse p99 pattern.
This is historical context only: it cannot establish neutrality in later six-block
campaigns or attribute their different result.

Reproduce this small metadata-only check separately:

```sh
python3 tools/benchcampaign/campaigns/rfc-263/rev5-historical-w8.py \
  --rev4-evidence /tmp/rfc263-rev4-screen.2KEIQu \
  --output /tmp/rfc263-rev5-tail.I5smMO/historical-tail-w8-metrics.json
```

The separate JSON records all six rows, source/executable identities, summary and
metric-index hashes, paired changes and medians. SHA256:
`e2caeb9b0b4dbf15b8ee2f5d9da842d231f5f534331ff4f63a372f373e5d0036`.
The earlier 32-worker JSON remains unchanged at
`dc12fdc0724328fc7408df8f80c487c8706a1d2aee56f331194ace7c221b1431`.
