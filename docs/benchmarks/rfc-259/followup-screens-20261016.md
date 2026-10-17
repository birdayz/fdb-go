# Scratch follow-up: timed successors and native completion yields

Three scratch screens tested allocation and scheduling explanations for the remaining
server-CPU gap. None is included in the GRV observation implementation.
A clean-candidate comparison rechecked how much of the current gap is GC-related.

## Scope and method

Same three-process local cluster, core placement, Go 1.26.9, default `GOGC=100`, and
`servercost.py` method as the [GRV observation campaign](grv-observation-20261016.md).
The timed-successor and native-yield screens use **one identical scratch binary**, SHA-256
`377317fc4456e56dd06ac08a2afa16d889faf9d6adb4479bff707f06b97ff41b`.

That binary includes the smaller native GRV state, single-entry RYW map pooling,
conflict-vector allocation prototype, and GRV observation prototype with
`FDBGO_EXP_GRV_OBSERVE=yield`. Transport delay and memory profiling are disabled.
The scratch source was not committed; its artifact digest and raw switch settings
identify these comparisons, but are not a reproducible production source revision.
These are **within-prototype comparisons**, not improvements measured against master.

All configurations completed successfully:

- [Timed-successor screen](results-lazy-timeout-screen.jsonl): 113 configurations,
  51 baseline/variant pairs and 11 CGo anchors across eight workload/worker cells.
- [Native-yield screen](results-native-yield-screen.jsonl): 74 configurations,
  23 blocks of three Go variants and five CGo anchors across three worker counts.
- [Current GC confirmation](results-current-gc-confirm.jsonl): 44 configurations,
  10 default-GC/GC-off pairs at each of 8/32 workers and four CGo anchors. This uses
  the clean GRV candidate, not the scratch binary; details below.
- [Fresh-header reserve screen](results-header-reserve-screen.jsonl): 74 configurations,
  23 blocks of three Go variants and five CGo anchors. Its separate scratch artifact
  is identified below. Together the follow-ups retain 305 configurations.
- The scratch screens alternate or rotate variant order within blocks. Every raw row records
  its short/long budgets and environment. CGo anchors are sparse context, not matched
  full-size reference populations.

Tables give medians of server cycles per transaction, in thousands. Paired ratios use
`exp(mean(log(variant/base)))`. Intervals use the same 10,000-replicate paired bootstrap
as the main report, drawing the cell's pair count with `rng.randrange(n)`, resetting
`random.Random(20261016)` for each metric, and taking sorted indices 249 and 9749.
These nominal intervals do not adjust for multiple comparisons.

## Unused timed successors

The existing lazy-successor path still eagerly constructs a replacement incarnation
when a positive timeout is configured. The experiment skips that construction only
when no successor was captured during turnover. First admission would still check the
existing absolute deadline; that behavioral argument has not been established by new
end-to-end regressions, so this is not a correctness-qualified candidate.

`FDBGO_EXP_LAZY_TIMEOUT=0/1` is the only switch changed between the two Go arms;
native completion yields are disabled in both.

| Workload | Workers | Pairs | Base cycles | Lazy cycles | Paired cycle ratio | 95% interval | Base/lazy allocations per tx |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| get100 | 1 | 3 | 502.5 | 496.4 | 0.994 | 0.980–1.018 | 56.13 / 52.12 |
| get100 | 32 | 10 | 62.7 | 62.5 | 0.996 | 0.987–1.005 | 53.79 / 49.78 |
| get100 | 64 | 5 | 41.2 | 40.6 | 0.997 | 0.973–1.027 | 53.55 / 49.54 |
| range100 | 32 | 5 | 115.0 | 114.8 | 1.000 | 0.989–1.009 | 59.28 / 55.28 |
| grv | 32 | 5 | 20.0 | 18.3 | 0.947 | 0.915–0.981 | 28.52 / 28.51 |
| set100 | 1 | 3 | 3331.5 | 3178.5 | 0.967 | 0.953–0.976 | 60.37 / 56.36 |
| set100 | 8 | 10 | 434.6 | 461.3 | 1.030 | 0.880–1.220 | 58.66 / 54.65 |
| set100 | 32 | 10 | 254.0 | 255.4 | 1.004 | 0.960–1.052 | 58.17 / 54.13 |

The four-allocation reduction reproduces across reads, ranges and writes, but does
not materially reduce server cycles at 32/64 workers. Client CPU decreases modestly:
paired ratios are 0.982 for `get100/32`, 0.975 for `get100/64`, and 0.986 for
`range100/32`. Bytes per transaction were not collected by this screen.

`grv/32` does **not** show the four-allocation change, so its lower server-cost sample
cannot be attributed to removing a steady-state successor allocation. Startup effects
and measurement variability have not been separated for that row.

With only three 1-worker pairs, arithmetic mean latency is 191.3→188.7 µs for
`get100` and 9880.0→10016.5 µs for `set100`. This small screen does not establish
serial-latency noninferiority. The change is not promoted for the server-CPU goal;
any later production proposal needs deadline, first-cause, future and watch regressions.

## Native completion scheduling

`FDBGO_EXP_LAZY_TIMEOUT=0` in every Go arm. The only changing switch is
`FDBGO_EXP_COMMIT_YIELD`:

- `base`: no extra yield.
- `publish`: one `runtime.Gosched` after native I/O and operand release, before
  upgrading the weak completion owner or publishing the outcome.
- `deliver`: one yield just before closing the public native-commit wait, after
  outcome/cache publication and post-commit turnover.

| Workers | Blocks | Base cycles | Deliver cycles | Publish cycles | Deliver/base ratio, 95% interval | Publish/base ratio, 95% interval |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 3 | 3342.0 | 3260.6 | 3414.2 | 0.981 (0.971–0.992) | 1.019 (0.996–1.034) |
| 8 | 10 | 503.9 | 455.6 | 454.4 | 0.883 (0.737–1.020) | 0.834 (0.685–0.997) |
| 32 | 10 | 253.0 | 251.6 | 254.2 | 0.956 (0.896–1.012) | 0.984 (0.940–1.020) |

The publication yield's 8-worker result warrants a clean-tree confirmation if pursued,
but neither yield establishes a 32-worker server win. One-worker arithmetic mean
latency is 9765.0 µs for base, 9936.4 µs for deliver and 10005.4 µs for publish;
three blocks are not enough to establish the 5% guard. No native yield is promoted.

## Current GC contribution and fresh traces

The clean candidate binary (`78b3894b…`, production code at `12a2628db`) was run
with alternating `GOGC=100` and `GOGC=off`, 10 pairs per worker count. All 20 off
samples reported zero measured GC cycles. This is a bounded diagnostic, **not a
proposed production configuration**. CGo has only two anchors per cell.

| Workers | Default cycles | GC-off cycles | CGo cycles | Paired off/default ratio | 95% interval | Default/off client CPU, µs/tx |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 8 | 442.4 | 432.6 | 431.2 | 0.990 | 0.963–1.017 | 62.96 / 53.06 |
| 32 | 261.3 | 194.3 | 154.3 | 0.738 | 0.642–0.852 | 46.22 / 39.69 |

GC is still a material contributor at 32 workers, but disabling it does **not**
consistently match CGo: off samples range from 144.8k to 274.8k cycles/tx. At eight
workers the default samples stayed in the low-cost mode in this campaign, despite
roughly 38 GC cycles per long run. Allocation/GC counts alone do not identify a split.

Fresh ordinary runtime traces of the same clean binary, with `GOGC=100` and no
custom event-append instrumentation, completed 5,000 measured commits at eight
workers and 12,000 at 32. These diagnostic trace runs are not performance-comparison
samples. Publication wakeups were counted only when `runtime.chansend` appeared
beneath `transport.readLoop`, and recipients were classified from their immediately
preceding blocking stack:

- Eight workers: exactly 5,000 commit wakeups, 625 intact clusters of eight.
- 32 workers: exactly 12,000 commit wakeups, two intact clusters of 32 followed by
  373 alternating 10+22 pairs. Counts agree at 100 µs, 500 µs and 1 ms gap thresholds.
  These are client publication clusters, not proven server durability-batch counts.
- After the last intact wave at 12.752128–12.771776 ms, GC started at 12.967424 ms
  in `Database.CreateTransaction` (`database.go:1019`). The next commit clusters
  contain 10 wakeups at 18.305024 ms and 22 at 24.386432 ms. Times are relative to
  the trace's first Sync event.
- None of 31 captured allocation-triggered GC starts is in FrameReader/readLoop.
  Its 12 observed assists occur between commit clusters, not while draining one.
  Send-side `writeContext`/`bytes.Clone` and native/GRV work also incur assists;
  overlapping elapsed assist durations are not CPU totals.

The older trace's `context.AfterFunc` initiator predates #947 and is not an actionable
current finding: the current nil-Done caller path avoids that work. The fresh sequence
supports application-allocation-started GC preceding a persistent split, not the claim
that frame allocation interrupts a commit-reply wave. It does not isolate which
GC-side delay determines the split. Raw runtime traces remain local; their summaries
are qualified observations, not a committed deterministic regression.

## Fresh-header allocation reserve

A final scratch screen moved header allocation without recycling any used transaction.
Binary SHA-256:
`7ef05bc3e04f28c140a200bd6f82ef519051fb1c79e2004874fb29f9f47b5685`.
It shares the other allocation/GRV prototypes above, with lazy timed successors and
native completion yields disabled. The only changing switch is
`FDBGO_EXP_HEADER_RESERVE`:

- `base`: allocate at CreateTransaction as usual.
- `prefetch`: consume a reserve of up to 128 individually allocated, never-exposed
  zero headers. Refill after a 1 ms timer following native send; each refill burst is
  bounded to the missing capacity. Database, defaults and creation time are assigned
  only when the object is actually handed out. Nothing is returned after exposure.
- `pad`: fill and retain the same bounded reserve, without consuming it.

Pad controls retained-heap effects only approximately: its occupancy and allocation
schedule differ from the refilling arm. The timer does not guarantee allocation occurs
inside an I/O wait, and its process-global callback is not joined by database Close.
These are important limitations, not a production-approved allocator design.

| Workers | Blocks | Base cycles | Prefetch cycles | Pad cycles | Prefetch/base ratio, 95% interval | Prefetch/pad ratio, 95% interval |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 3 | 3280.1 | 3248.2 | 3392.8 | 0.985 (0.969–0.995) | 0.993 (0.957–1.056) |
| 8 | 10 | 456.5 | 455.1 | 673.8 | 0.994 (0.814–1.195) | 0.793 (0.680–0.968) |
| 32 | 10 | 250.8 | 257.1 | 251.8 | 1.017 (0.978–1.055) | 1.008 (0.991–1.024) |

A separate `MemProfileRate=1` positive control completed 2,000 measured commits plus
100 warmup. It recorded 2,149 header allocations in the refill callback and only one
in the foreground fallback, confirming that the path ran. That allocation-profile
run is not a performance sample and does not establish the previous runs' exact hit
rate. The screen establishes no 32-worker server benefit; the reserve is not promoted.

## Remaining investigation

Current GC still contributes to the 32-worker gap, but these allocation-count,
scheduler-yield and allocation-relocation screens do not solve it. Further changes
need current timing evidence and clean-tree comparisons, not extrapolation from a
heap census. Numeric-reply buffer reuse is not supported by the current split timing
evidence and has additional cancellation/borrowed-value lifetime hazards; no such
implementation was attempted. The overall server-CPU goal remains open.
