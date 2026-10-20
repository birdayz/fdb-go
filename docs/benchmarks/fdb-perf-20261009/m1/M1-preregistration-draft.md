# M1 record-layer paired timing — pre-registration DRAFT v1 (combined variant)

Status: **DRAFT, not frozen.**
- Root has authorized the record-layer route and a conditional M1 VM extension. The terms: one combined NEW arm, and **45 min of total VM time** covering staging, setup, timing, analysis and archive.
- Freeze needs three things: final full SHAs, the built and hashed binaries, and astra-perf-measure's pre-review. Delivery goes via perf-lead2 to astra-perf-measure before R18 ends. If it isn't there, the VM is deleted within 30 min of R18 ending.
- Nothing has run on the VM.
- Once frozen, this file is never overwritten; changes before observations are separately named, dated amendments. After the first measured process: no rebase, no arm change, no change to N or pair count, no replacement of runs.

M1 is separate from R17/R18. It neither selects nor modifies their protocols and does not pool their data.

## 1. Arms and source identity

Exactly two arms. A lazy-only arm or a third arm is not permitted by root's terms.

| arm | identity | contents |
|---|---|---|
| base | `8ba75870489c9d1186d67f900ba867e77d1f0dd4` (master at #862; perf-lead2: do not chase later test-only master) | — |
| new | `FILL` full SHA of local branch `m1/new-combined` | base + (A) `perf/range-eager-start` + store-open-on-(A) commit + #864 final commit(s) |

`m1/new-combined` is constructed by `m1-make-combined.sh`. It is a detached worktree at (A)'s head, which must descend from BASE, followed by `git cherry-pick` of each listed commit in order. Any conflict or empty pick aborts loudly.

The committer identity and date are fixed (`m1-harness`, `2026-01-01T00:00:00Z`), so identical inputs give an identical combined SHA. The script writes the full commit list to `combined-<sha>.txt`.

Inputs at freeze (all full SHAs, filled in at freeze):
- (A) head: `FILL` (currently `b5cd046cb478dcf1503e20dcad8db74b17b88b69`, pending fm-impl's compile confirmation)
- store-open commit: `FILL` (rl-storeopen, rebased onto (A))
- #864, final per perf-lead2: `0170ca58eeab709a7bc67fb3715ca9bb03f073f3` then `ee59bfb3ab13762761b6a0132bf52fd2824883a9` ("drop continuation cache, pin row allocs"). Both are picked, in that order. #868, a separate correctness fix, is NOT included.

**Dry construction, done for conflict discovery only (not frozen).** Both picks applied cleanly onto `b5cd046cb`:
- `0170ca58e` alone;
- `0170ca58e` + `ee59bfb3a` + the pre-rebase store-open commit `5c2e94bb0`.

**Bench shim (identical in both arms).** `m1-bench-shim.patch` touches only `pkg/recordlayer/benchmark_test.go`. It adds:
- `M1_FDB_CLUSTER_FILE`, which opens the native cluster. Without the shim the benches start a Docker testcontainer, and the shim refuses that fallback in M1 mode.
- `M1_KEY_PREFIX`, prepended to `benchSubspace` as tuple element 0.
- `m1ProcCPU`, the process-CPU metric (§3).

No input branch touches this file. `git apply --check` must pass in every arm. Product code is unmodified.

**Build pins.** Each arm gets its own detached worktree, `worktrees/m1-<arm>`, and is built with `bazelisk build --compilation_mode=fastbuild //pkg/recordlayer:recordlayer_test`. This is the target `just bench-one` uses.
- Built locally. All Bazel work for an arm (build, `info bazel-bin`, shutdown) runs as one batch under `nice -n 15 flock --close fast.lock`, gated on availMB ≥ 8000, re-checked after the lock is acquired. Nothing is built on the VM.
- **Worktree integrity.** The arm's worktree must be exactly the commit plus the shim: `git diff --binary` byte-equal to the shim patch, nothing staged, no untracked files. A pre-existing worktree in any other state is refused, never cleaned. An exact-bytes snapshot of tracked + untracked files is taken before and after the build and must be identical.
- Pinned per arm by go.mod (go 1.26.9), go.sum, MODULE.bazel(.lock), .bazelversion and .bazelrc (global tags `bazelrunfiles,stress`). Their sha256s are recorded and compared to base in `pins-compare.txt`; a difference must be explained at freeze.
- Recorded per arm: `git diff BASE NEW`, the file list, the commit list, the source archive, the tree hash with the shim, and the binary's `file`/`ldd`/`go version`.
- Draft-build facts: ELF x86-64, go1.26.9, dynamically linked against libc/libresolv. The highest required symbol is GLIBC_2.34, so the VM needs glibc ≥ 2.34. No Bazel runfiles or Docker are needed.

## 2. Workloads, fixed counts, order

Each process runs exactly one benchmark leaf. The client is the pure-Go fdbgo; the server is native fdbserver 7.3.77 / API 730.

| key | `-test.bench` | fixed N (`-test.benchtime=Nx`) | per-op work | est. op (base) | est. window |
|---|---|---:|---|---:|---:|
| StoreOpen | `^BenchmarkStoreOpen$` | 2000 | txn: Open existing store | ~0.35 ms | ~0.7 s |
| SaveRecord | `^BenchmarkSaveRecord$` | 400 | txn: Open + SaveRecord + commit | ~2 ms | ~0.8 s |
| LoadRecord | `^BenchmarkLoadRecord$` | 2000 | txn: Open + LoadRecord(pk 1) | ~0.4 ms | ~0.8 s |
| ScanIndex | `^BenchmarkScanIndex$` | 1500 | txn: Open + scan 100-entry VALUE index | ~0.6 ms | ~0.9 s |
| ScanIdxRecSerial | `^BenchmarkScanIndexRecords$/^serial$` | 10 | txn: Open + 1000 entries + 1000 serial LoadRecord | ~80 ms | ~0.8 s |
| ScanIdxRecPipe | `^BenchmarkScanIndexRecords$/^pipelined$` | 50 | txn: Open + ScanIndexRecords 1000, pipelined | ~15 ms | ~0.75 s |

**Why these N.** There is no automatic b.N calibration. N is fixed so that each measured window is about 0.6–1 s at the estimated per-op latency. That is long enough that rusage's 1 µs resolution and process start are negligible, and short enough to fit the budget in §7.
- The latencies are **estimates**. Astra has no R17/R18 record-layer numbers, and no timing was run.
- NEW is expected to be faster, which only shortens windows.
- N is never changed after observation. Slower ops are absorbed by the caps (§7) and end the run as incomplete, never as a re-tuned one.

**Op counts per process (Go testing, `benchtime=Nx`).**
- **Leaf benches.** The function runs twice: `run1` with b.N=1, then `runN(N)`. Setup therefore runs twice and N+1 ops execute in total. ns/op and the CPU metric cover only the N ops after `b.ResetTimer()`.
- **ScanIndexRecords.** The parent runs once: one setup (ClearRange + 1000 saves in one txn), then the selected sub-benchmark runs 1 + N ops. The sibling sub-benchmark is filtered out.
- **Round trips.** StoreOpen reads the header and the index state: 2 sequential read round trips in base, overlapping into 1 in NEW. This is the locally established secondary fact.
- **Retries and errors.** Retries inside `FDBDatabase.Run` are not observable. Any error fails the process.

**Command.**

```
taskset --cpu-list 8-15 <bin> -test.run=^$ -test.bench=<re> -test.benchtime=<N>x -test.count=1 -test.cpu=8 -test.benchmem -test.timeout=20s --ginkgo.skip=.*
```

The process runs in its own session/process group under an **external wall watchdog**. `-test.timeout` does not cover benchmarks: Go stops its alarm before `runBenchmarks`. After `hard_wall_s=20` the watchdog sends TERM to the group, then KILL after `kill_grace_s=3`, and reaps the process with `wait4`, keeping its rusage. A watchdog firing makes the process invalid and stops the campaign. A controller signal (TERM/INT/HUP) to `m1-run.sh` or the runner kills the current group, reaps it and exits with rc=5: incomplete, no inference.

**Environment.** The process gets exactly the following, with nothing inherited:
- `PATH=/usr/bin:/bin`
- `HOME` and `TMPDIR` inside the campaign dir
- `GOMAXPROCS=8`, `GOGC=100`, `GODEBUG=` (empty), `GOMEMLIMIT` unset
- `M1_FDB_CLUSTER_FILE=/etc/foundationdb/fdb.cluster` (confirmed by astra)
- `M1_KEY_PREFIX`

The run is refused if `FDB_NETWORK_OPTION_*` is set. CPUs (confirmed by astra): fdbserver 2–7, client 8–15, runner/analysis 0–1.

**Warmup.** In-process warmup is `run1`. In addition, one global warmup block at the start runs every bench × arm once in random order; these processes are recorded as `kind=warmup` and never analyzed.

**Order.** There are **48 measured paired blocks**.
- Each block holds all six benches; each bench is an adjacent base/new process pair.
- Bench order is shuffled per block.
- Arm order is exactly balanced per bench: 24 base-first and 24 new-first, shuffled.
- The schedule comes from `m1.py plan` with `random.Random(919001)` and is sha256-pinned before transfer.
- It is never regenerated, resumed or appended to: `runs.jsonl` must not exist at start.

**Cooldown/quiescence.** Each process is preceded by 1.0 s; in the last 0.5 s, `/proc/stat` busy/steal is sampled on CPUs 8–15. Recorded only.

## 3. CPU clock boundaries and normalization

- **Primary `proc-cpu-ns/op`.** `getrusage(RUSAGE_SELF)` utime+stime for the whole client process: the bench goroutine, fdbgo network goroutines and GC workers.
  - B is taken immediately after `b.ResetTimer()`, or at sub-benchmark entry for ScanIndexRecords, whose subs contain no setup.
  - C is taken in a deferred call at function return after the N-th op of `runN(N)`. The value is (C−B)/N.
  - Only that call's value is reported; Go overwrites run1's value.
  - Includes GC work that runs during the loop, whatever garbage it collects. Excludes setup, run1, start and exit.
  - User and sys are reported separately as secondaries.
- **Wall `ns/op`.** Go's benchmark timer over the same N ops.
- **Process-lifetime CPU** (secondary, absolute only, never claimed). `wait4` rusage from exec to exit: runtime init, FDB connect, both setups, run1, the window, output and exit.
- **Shutdown scope.** The pure-Go client has no native network thread, the database is never closed, and process exit tears everything down. No drain or finalizer phase is measured or claimed; there is no R17-style A–E lifecycle.

## 4. Native FDB, key prefixes, data scope

The cluster is the native R17 Ubuntu 24 cluster at `/etc/foundationdb/fdb.cluster`. It is used only after R17/R18 have finished.

**Health.** Pre and post, `status json` must report `available` and `healthy`. A failure before timing stops the run; a failure after timing makes the campaign invalid. The cluster-file sha256 and the fdbserver/fdbcli versions are recorded.

**Prefixes.** Every process gets a unique, equal-length 17-byte prefix, `m1/<campaign8>/<seq5>`, packed as tuple element 0 before the benchmark name. Key lengths are identical across arms, and no process sees another process's data.

**Cleanup.** Nothing is cleared during timing. After timing, `clearrange \x02m1/<campaign>/ \x02m1/<campaign>0` removes all M1 data. A cleanup failure is reported but does not affect timing validity.

## 5. Primary endpoints and decision rules

All effects are paired new/base ratios. For each block, take log(new/base); the effect is exp(mean of the 48 logs), the geometric-mean ratio.

**Primary family (5):**
1. StoreOpen `ns/op`: the header/index-state read overlap from (A)+store-open (2→1 round trips).
2. LoadRecord `ns/op`: the same overlap, in an Open + 1-read txn.
3. ScanIndex `ns/op`: the same overlap, in an Open + 100-entry scan.
4. ScanIndex `proc-cpu-ns/op`: lazy continuation, 100 entries per op.
5. ScanIdxRecPipe `proc-cpu-ns/op`: lazy continuation, 1000 entries per op.

Everything else is secondary/descriptive and cannot replace a failed primary:
- the other bench × {ns/op, proc-cpu} cells, including SaveRecord ns/op and StoreOpen proc-cpu, which serves as a CPU-cost watch for (A)'s background GetRange;
- user/sys, B/op, allocs/op;

allocs/op and round trips per Open are deterministic facts established locally, not by M1.

**Intervals.** Two-sided **99%** percentile intervals (Bonferroni 0.05/5).
- **Primary:** circular moving-block bootstrap over blocks, length **8** (6 starts per resample), **100,000** resamples, `random.Random(919707)`. Indices are drawn jointly across all endpoints.
- **Mandatory sensitivity:** length **16** (3 starts), 100,000 resamples, seed `919708`.
- **Quantiles:** nearest-rank, `sorted[max(0, ceil(p·M)−1)]` at exactly p = 1/200 and 199/200, computed with `fractions.Fraction`. At M = 100,000 that is indices **499 / 99,499**; float rounding cannot move them.

**Decision.**
- Improvement is confirmed only if *both* upper bounds are < 1 and every gate passes.
- Regression is confirmed if both lower bounds are > 1.
- Anything else is **inconclusive**. Point estimates are always reported, and no equivalence claim is ever made.

No peeking: the runner prints only operational status (exit, row, N, PASS), never a timing value. `m1.py analyze` refuses an incomplete or invalid campaign. No optional stopping, endpoint substitution, trimming or run replacement.

## 6. Sample size rationale

There is no pilot sigma. With 48 pairs, a 99% two-sided interval and 90% power (normal approximation), the minimum detectable |log ratio| is (2.576+1.282)·σ/√48 = 0.557σ. That is about 1.7% at σ = 0.03 and about 3.3% at σ = 0.06 per-pair log-ratio SD.

The expected effects are larger on primaries 1–3: one fewer round trip of 2–3 in StoreOpen, LoadRecord and ScanIndex. They are a few percent on primaries 4–5. Pair count is bounded by root's 45-min VM slot, not chosen from data. If a CI crosses 1, the result is inconclusive and no blocks are added.

## 7. VM time budget (hard 45 min total) and abort rule

| phase | budget | notes |
|---|---:|---|
| transfer + tarball sha256 + extract | ≤ 3 min | ~90 MB (two 45 MB binaries) |
| preflight: `inputs.sha256`, competing-process scan, identity, pre health | ≤ 1 min | |
| warmup block + 48 measured blocks | **≤ 28 min hard** (`campaign_cap_s=1680`) | projected ~22.5 min: 49 blocks × 12 processes × ~2.3 s (0.3 s start+connect, 0.2 s setup+run1, ~0.8 s window, 1.0 s cooldown) |
| post: inputs re-hash, post health, cleanup, analysis (~20 s measured locally on synthetic data at 48×54 endpoints×2×100k), full inventory, results tarball | reserve **`post_reserve_s=360`** (6 min) | |
| download + remote/local sha256 verification | ≤ 3 min | |
| **total** | **≤ 41 min** + ~4 min slack | ≤ 45 |

Process-lifetime CPU and wall are reported as absolute per-arm summaries only (min/median/mean/max); they never get a paired ratio and are never claimed.

**Abort before the next run.** `m1-run.sh` requires `M1_DEADLINE_EPOCH`, the absolute end of the slot from astra's R18→M1 handoff. Before every process, the runner computes worst = cooldown + 20 s watchdog + 3 s kill grace + 2 s. It starts that process only if both conditions hold:
- elapsed timing + worst ≤ 1680 s;
- now + worst + 360 s ≤ deadline.

Otherwise it stops **before** the process, prints `ABORT_BEFORE_SEQ`, and the campaign is incomplete: no inference, but every artifact is archived. A process is never killed mid-run except by the 20 s external watchdog, which makes it invalid. The rule is outcome-blind.

`m1_selftest.py` checks the following offline (no FDB, no timing), all passing:
- exact quantile indices;
- the bootstrap matches a direct-reference implementation;
- strict-gate edge cases: an upper or lower bound exactly 1.0, and a synthetic campaign with identical arms gives inconclusive;
- a 0.9× effect is confirmed;
- missing, duplicate, invalid, wrong-N, missing-metric and order-swapped rows are refused;
- lifetime metrics are absent from the paired table;
- the watchdog TERMs a hung child, KILLs a TERM-ignoring child with rusage kept, and leaves a normal child alone;
- a controller SIGTERM kills the child group and the runner exits 5.

Also verified end-to-end on stubs (fake binaries and a fake fdbcli):
- the full pass produces `valid=1`, the analysis and the tarball;
- a deadline too close yields `ABORT_BEFORE_SEQ 00000` with rc=3 and no analysis;
- a TERM to `m1-run.sh` mid-process yields rc=5, no stray children, and an archive that is still produced.

## 8. Validity gates and reporting

The following stop the campaign or make it invalid:
- an `inputs.sha256` mismatch, pre or post;
- `runs.jsonl` already present;
- `FDB_NETWORK_OPTION_*` set;
- a competing process, found by scanning `/proc/*/exe` plus full argv0 for `recordlayer_test`, `clientbench` or `astra-perf-run`, or any process whose exe is a staged arm binary (comm truncation avoided);
- pre or post health failure;
- any process with a watchdog firing, a nonzero exit, a missing or duplicate result row, N ≠ planned, a missing metric, or no `PASS`;
- the cap or deadline abort.

**Host gate**, evaluated after completion: steal on CPUs 8–15 ≤ 1% over all measured processes and ≤ 5% in any single process. If it fails, every primary decision becomes `INVALID_HOST_GATE`.

**Archive.** `ALL-FILES.sha256` covers **every** file in the campaign dir:
- inputs: binaries, provenance, config/plan/schedule, harness;
- `pins.json`, `identity/*` (pre/post status, cleanup);
- `raw/*`, `runs.jsonl`;
- `analysis.*`, `m1-run.log`, `status.txt`.

The whole dir is tarred to `<campaign>.results.tar.gz` with its sha256, verified on the VM and again after download. Every process record, all 48 log ratios per endpoint, both intervals and the schedule are reported. Results are scoped to these benches, counts, software and this VM. A benchmark result is not evidence of correctness, CI health or merge permission.

## 9. Procedure and delivery format

Local:
1. `m1-build-arms.sh build BASE`. BASE is pre-built.
2. `m1-make-combined.sh m1/new-combined BASE <A-head> <storeopen> <864...>` produces NEW_SHA.
3. `m1-build-arms.sh build BASE new=NEW_SHA`. This reuses the base binary and builds new.
4. Fill in `config.json` from the template.
5. `m1-stage.sh build config.json m1comb01`. This produces `m1comb01.tar.gz` and its `.sha256`, and the tarball contains `inputs.sha256`.
6. Freeze this file, with every FILL replaced and the harness file hashes added. Hand off to astra via perf-lead2: tarball path, tarball sha256, `inputs.sha256` sha256 and this file's sha256.

VM (astra-perf-measure):
1. Verify the tarball sha256 and extract it to `/opt/fdbperf/m1/`.
2. Run `M1_DEADLINE_EPOCH=<slot end> m1-run.sh /opt/fdbperf/m1/m1comb01`.
3. Verify `m1comb01.results.tar.gz.sha256` on the VM, download, and verify again.

Harness files: `m1.py`, `m1_selftest.py`, `m1-run.sh`, `m1-build-arms.sh`, `m1-make-combined.sh`, `m1-stage.sh`, `m1-bench-shim.patch`, `m1-config.template.json`. Hashes are in `harness-draft.sha256` and are pinned here at freeze.

## 10. Open items before freeze

- Final (A) head (fm-impl), the store-open commit rebased onto (A) (rl-storeopen), and #864's final commit(s) (rl-lazycont).
- perf-lead2 signed off on the 5 primaries, the 41-min budget and the deadline-abort design. Astra's pre-review ACK of the final hash snapshot is still pending.
- If (A)'s final SHA or the store-open SHA is missing at R18's end, M1 is off and the m1-* worktrees are released.
- `M1_DEADLINE_EPOCH`, from astra's handoff at R18 end.
