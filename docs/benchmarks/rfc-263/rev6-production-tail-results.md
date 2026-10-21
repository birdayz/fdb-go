# RFC-263 production revision 6: exclusive quiet-slot tail screen

**Not merge approval.** This is the first fresh timing/8-worker trace population for
production commit `c2e52f46ab02df0295f272f421b87c3bdb3a3515`, not the archived identity
prototype. The historical all-adverse 8-worker range result **does not reproduce in this
quiet production batch**: p99/p999 medians improve and only two/one of six pairs are
adverse. That does not establish equivalence or explain the earlier residual. Optional
8-worker get/set measurements have new adverse p999 medians and remain qualification
findings. The owner must review these results, trace attribution, full/full-race and
final-HEAD gates before reconsidering the performance merge blocker. B3a/B3b remain
outside this implementation.

**Later gate status:** the [fixed confirmation](rev6-confirm-results.md) cleared the 8w
p999 blocker by the owner's preregistered rule. The exploratory screen and diagnostic
observations below remain historical evidence, not replaced or pooled with confirmation.
Power/epoch/order limitations and the remaining merge gates still apply.

## Identity and quiet-slot controls

Raw evidence is durable:
`${FDB_PERF_HOME}/validation/rfc263-production/quiet-slot-a-20261020T153639Z/`.
No flag alternative was run. The three separate diagnostic arms are:

| Arm | Source | Executable SHA256 |
|---|---|---|
| Original baseline | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | `2dd943fdc9d58ff136449a181189ba5843465e86e6ae696d616a9b1bac4a439e` |
| Historical R4 prototype | tree `61d2597e0231dfbedcd898a27c078c3412efaf6a` | `2f0993bd332a9d7085b1c1053659b68867286de95ad1cbe2bf91fe327f088e95` |
| Selected production Rev6 | `c2e52f46ab02df0295f272f421b87c3bdb3a3515` | `ff3b447e7449a1e3dc38d04da15e4fdec15cc4e0840fa808e95fb6544b5f37de` |

The production executable was built **after the real-hook commit**, with Bazel opt,
race=false and actual Go **1.26.9**. All three executables report Go 1.26.9. The complete
production source archive, build command/output, pinned SDK/compiler/linker identities
and decoder build/source/binary hashes are retained. The baseline and production
`tools/clientbench` trees are exactly equal (`1d6e88991000d7664ce5fb21ce8c41a25ee03747`,
**63 files** each), not merely similarly named harnesses. Frozen harness archive identity
remains `cfc3a33191ec10b4e0955b3f0f192b1ba3be554104ea61016c2a6e60aabbf400`.

The exclusive slot began at 15:36:39 UTC on 2026-10-20, including setup builds. Bazel was
shut down and the full-state decoder built **before** settling. Initial recorded load
8.28 was not accepted. Load first reached **7.50 at 15:37:54.459236 UTC**, followed by
at least **60 seconds of quiet settling with every recorded load sample at or below 7.5**.
The first diagnostic process began
15:38:58 UTC with load 5.92, idle fraction 0.989639 and zero steal ticks. There are
**135 process/container/load gate records**, with zero active Bazel action or forbidden
FDB/testcontainers container records. Substrate infrastructure containers were allowed;
the three-process native benchmark FDB cluster was unchanged. No benchmark ran while
the setup build or initial over-threshold settle was active.

Historical client settings are preserved: CPUs `2,3,14,15`, GOMAXPROCS 4, GOGC 100,
GOMEMLIMIT=off, empty GODEBUG, timeout 30000 ms, retry limit 10, warmup 2000, write permission
and a unique **68-byte** prefix. Controllers/builds used nice 19 and the booked
`0-1,6-7,11-13,18-19,23` mask; benchmark processes inherited nice 19. Commands, environment,
uptime before/after each invocation, raw samples and process CPU are retained. The whole
tracked/untracked/config input manifest and all frozen scripts/binaries were checked
unchanged after timing. No additional test container or test/build was started after
the quiet gate.

## Population and fresh paired results

**51/51 successful processes:** three separately instrumented 8-worker range traces,
then **48 unprofiled timing runs**, each with zero errors and exact requested completion.
Each timing population has six alternating blocks: AB, BA, AB, BA, AB, BA, where A is
baseline and B production. Range 8w/get 8w/set 8w each measure 20000 operations/run; range 32w
measures 500000/run. This is 6,720,000 measured timing operations plus 60000 diagnostic
operations, excluding warmup. The diagnostic order baseline/R4/production is not a
balanced timing comparison and is never pooled with unprofiled runs. There are **no
get/set traces** in this slot: range diagnostics cannot attribute the optional get/set
p999 findings.

Each cell below is the **median of six independent runs**, not a pooled percentile.
Percent changes are ratios of those medians; adverse counts use the six matched blocks.
CPU is measured client process CPU per completed operation, not exclusive detector cost
or server CPU. All 48 complete latency histograms use 1 µs bins without an overflow bucket.

| Population | CPU µs baseline → production | CPU change; adverse pairs | p99 µs baseline → production | p99 change; adverse pairs | p999 µs baseline → production | p999 change; adverse pairs |
|---|---:|---:|---:|---:|---:|---:|
| Range 8w | 93.5073 → 95.0592 | +1.66%; 4/6 | 993.647 → 957.723 | −3.62%; 2/6 | 1450.657 → 1404.960 | −3.15%; 1/6 |
| Range 32w | 59.6801 → 59.7072 | +0.045%; 3/6 | 2238.043 → 2173.501 | −2.88%; 1/6 | 4782.723 → 4719.768 | −1.32%; 3/6 |
| Get 8w | 47.7263 → 46.0877 | −3.43%; 1/6 | 534.477 → 537.578 | +0.58%; 5/6 | 867.529 → 948.181 | +9.30%; 4/6 |
| Set 8w | 55.1208 → 53.7403 | −2.50%; 1/6 | 1121.058 → 1133.001 | +1.07%; 4/6 | 1806.016 → 1967.437 | +8.94%; 5/6 |

Range 8w mean latency improves in all six pairs (median 472.866→467.119 µs, −1.22%); its
CPU run ranges overlap (baseline 91.439–95.731 µs, production 93.191–95.350 µs). Range 32w mean
changes −0.31%. Get 8w mean changes +0.36%; set 8w mean changes +0.104% with all six pairs
adverse. These are observed local results, not statistical equivalence or cluster-wide
qualification. At 20000 operations, each run's p999 is supported by only about 20 tail
samples; retain the full histograms, per-pair changes and negative results.

Timing ended at **15:46:49 UTC**; CSV/histogram analysis completed 15:47:13 UTC. All three
traces were decoded by 15:47:41 UTC without building during the measurement phase. The
full-state decoder reports **665970 / 670321 / 673439 events** and exactly **8 workers**
for baseline/R4/production. Unlike the earlier weak-only extractor, it retains every
goroutine state transition, wait reason, target and unblocker/event stacks, all ranges
including GC assists, synchronization clocks and lossless text for other event kinds.
This is a separate diagnostic population: instrumentation can change latency and GC.

## Attribution and artifact sealing

The full-state analysis uses each trace's actual eight worker creations/start barriers
and synchronization snapshots, not a guessed CSV epoch. Conservative epoch widths are
4487 ns/1446 ns/1119 ns for baseline/R4/production, with independent 64 ns event-edge margins.
All worker and cleanup state reconciliation counts are zero; all samples have unique,
complete indices and the expected worker assignment. Six nonempty guard groups exercise
interval values, uncertainty bounds, worker population/barriers, stack categories,
cleanup execution-versus-registration and exact sample populations. All three traces
contain 130 complete GC concurrent-mark ranges and no partial ranges.

| One instrumented range 8w trace | Baseline | R4 | Production |
|---|---:|---:|---:|
| Diagnostic p99 / p999, µs | 1014.842 / 1526.791 | 1010.112 / 1698.125 | 1254.405 / 1944.371 |
| Slowest 1%:200 operations, total latency ms | 245.475 | 253.878 | 306.885 |
| Direct select-wait overlap, ms (lower bound) | 163.183 | 156.635 | 180.494 |
| Direct runnable overlap, ms (lower bound) | 48.643 | 47.946 | 53.673 |
| GRV-batcher select overlap, ms (lower bound) | 98.282 | 86.083 | 114.996 |
| Range-reply select overlap, ms (lower bound) | 49.931 | 58.149 | 49.194 |
| Direct GC-assist waiting in slowest 1%, ms (lower bound) | 0.790 | 1.057 | 3.773 |
| STW overlap in slowest 1%, ms (nonadditive lower bound) | 22.612 | 18.639 | 37.787 |

These diagnostic tails are adverse for production, unlike the six-block unprofiled
range result. Each diagnostic arm is only one separately instrumented run; these are
**descriptive state contributions, not a balanced causal comparison** or an explanation
of the historical nonprofiled residual. The largest direct worker spans are normal GRV/
reply/write-admission select waits and subsequent runnable delay. A GRV select includes
upstream work/server/network time; it is not exclusive client CPU or proof of one
particular upstream cause. Running state is not CPU time. GC/STW/cleanup overlaps are
nonadditive associations and must not be added to direct state totals as extra latency.

Observed exclusions and limits:

- There are **zero weak-promotion waits** in all three complete unfiltered state
  streams; this is not R5's previously proven arm-path weak-to-strong GC gate.
- Production has **zero queue-path sync spans**; R4 has one approximately 1–2 µs span,
  outside its slowest 1%. A queue-path stack is not automatically ownership of a
  particular mutex. This does not support broad producer queue-lock parking as the
  cause of the observed production diagnostic tails.
- Exactly **one actual cleanup executor goroutine** is observed in each arm. Cleanup
  running exposure is 3.122/2.932/1.892 ms; production's is smaller, not an executor storm.
  Production and baseline have no direct cleanup-to-worker wake edges. Production
  cleanup-running overlap with its slowest 1% is at most 0.449 ms of 306.885 ms. This does
  not isolate indirect cleanup/retention/GC effects, but does not support direct cleanup
  executor time as the dominant worker-tail explanation in this trace.
- Production has 104 worker GC-assist parks versus 23/40, totaling 9.021 ms versus 1.305/
  2.259 ms across 20000 operations. That rise is real, but direct assist wait in the
  slowest 0.1% (20 operations) is only 0.0387–0.0390 ms, versus baseline 0.0429–0.0431 ms.
  Therefore the larger global count is **not** proof that assist parks explain p999.
- Production's largest ordinary sync/preempted tail stacks pass through
  `runtime.gcStart` at `cloneReadKVs`, `ParseKeyValueRefStringVector`, `copyKVs`/
  snapshot-cache insertion and the harness range path—not queue locks. Exact arm-frame
  inspection finds **zero production arm-path parks**. R4's one queue sync is 1.600 µs;
  baseline has a 60.736 µs GC-assist park and 316.672 µs GC-start sync at runtime timer
  allocation. This narrows the observed parked paths, but does **not** exclude running
  time, runtime-lock/sweep cost or indirect allocation/retention effects.
- No drainer callback stack is observed. That absence alone is not proof; the separate
  process-age/first 10-second-wake bound excludes drainer/frontier/sort/Gosched callback
  execution: all **39** 8-worker processes (three traces plus 36 timings) have conservative
  lifetimes strictly below 4 seconds, including 1-second timestamp quantization. Timer/cleanup registration can still matter
  before the first callback.

The first analysis attempt is preserved explicitly **INVALID**: classifying any
`mcleanup.go` frame as an executor incorrectly includes producer `AddCleanup`
registration, and an aggregate IPC serialization failed. No cleanup findings from that
attempt are used. The corrected detector recognizes actual `runtime.runCleanups`/
`runtime.callCleanup` execution frames, rejects registration as a negative control,
reruns all three immutable streams, and rejects worker state mismatches. Raw per-sample
bounds, worker spans, stack tables, source hashes, guards and invalid-attempt artifacts
are retained. No historical 32-worker trace is used as an 8-worker observation, and no
range diagnostic is used to attribute the get/set p999 findings.

### Sealed durable index

All **682 raw files (3,585,790,764 bytes)** were recursively hashed and then reverified;
the index excludes itself. Sealing finished **16:04:26 UTC**, within the owner's
30-minute slot. Paths below are relative to the durable epoch root above:

| Artifact | SHA256 |
|---|---|
| `evidence-files.sha256.json` | `d703eb92281b7042e4649b3360dcb293e904cabd2b062f23ec35a5d283e5e755` |
| `campaign/status.json` | `19500736b21b87b20ab1d8f5cf8edf35014d29db123e5276d021ab73cb54ae8b` |
| `rev6-trace-analysis/summary.json` | `f0c0b5d8731a81591615c23a15794a69ce36f63709ad0425039100d366d9400f` |
| `rev6-trace-analysis/inspection-findings.json` | `1d577ae22a1ab24bd2df98e0cc70b0ed911bb2e0e613746f164ca54a299d6119` |
| `setup/production-source.tar.gz` | `de813988009cfa1e9bd00229e828b5d31d4cb5ef5d646471c5193960f494b202` |
| `setup/eight-worker-process-age-bound.json` | `27ff14a4289964e2cb9e0f89658faadd0b54ce34f6abd399cbd23c5b4b8846ab` |
| `setup/post-timing-reporting-drift.json` | `8edf3496a3b5c011de2c2fdfccc340b357483df7237e371d338199a6ea73e58a` |

The final analyzer source is retained beside its raw results and has SHA256
`51f3ba6a4ed2c80a5ede01481f462406850a6e3d990be3fe05cc415a5d3dce03`.
The post-timing drift receipt distinguishes later reporting edits from measurement
inputs: **7,409 original paths outside three edited documents remain unchanged**,
external configuration matches, no original paths were removed, and the only added
paths are this results document and the analyzer. Every reporting edit/addition has
a modification time after the successful exact post-timing input verification.

The historical as-run tail runner SHA256 is `9b9b728f525926dfd4240ce8b4cd4fa95845647e7c9324d8ec2df1763670c655`. Portable replay uses the current `rev6-production-tail.py` and prebuilt
`rev6-trace-extract.py` decoder. The runner has positive/negative controls for process/
container gates, source-tree evidence paths, balanced/nonempty plans and exact completed
run populations. A process-group supervisor enforces the owner's absolute 30-minute
slot deadline and preserves partial failures. No implicit `go tool trace` build is used.

Exact portable/as-run source hashes and invocations for the tail runner, analyzer and trace decoder are in [reproduction.md](reproduction.md). Original sealed sources/indexes are not rewritten.
