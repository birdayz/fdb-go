# R18 GOMAXPROCS diagnostic — v1

Frozen before any R18 observations. This is exploratory diagnosis, not a shipping-knob decision or confirmatory extension of R17. R17 observations, sample size, endpoints, and decisions remain separate. No R17 outcome is consulted to choose this protocol.

## Frozen implementations and host

Use the R17 reimaged existing Hetzner instance and fresh cluster after R17 acquisition and analysis have completed, never concurrently. This is not another host allocation. Native FDB server/API remain 7.3.77/730 and the cluster remains `/etc/foundationdb/fdb.cluster`.

NEW is the exact R16/R17 frozen base `7922a2e5a8e3a2d6cf17dfee8b8134181c88b298` plus patch SHA256 `d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85`, with source declaration BASE + `.patch.` + PATCH. Both Go arms use the same executable, SHA256 `93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f`. Native CGo executable SHA256 `4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c` declares its actual original source `8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead`. libfdb_c SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`; server SHA256 `179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f`.

Reviewed V7 harness archive SHA256 `48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203`; frozen `run.py` SHA256 `7ce9e96c4e736b2df559f62b00c4eacfd4ad4b1623ca380d94e07896d96669f2`. No rebuild, product-code edit, FDB option override, or change to the V7 forced-GC/finalizer/FIFO/StopNetwork lifecycle is permitted.

## Eighteen uninstrumented timing processes

Input arms are `[go8, go2, cgo]`: NEW with GOMAXPROCS=8, identical NEW with GOMAXPROCS=2, and CGo with GOMAXPROCS=8. No CGo@2 arm. Every client still has CPUs 8–15 available; server CPUs 2–7, controller/analysis CPUs 0–1. GOGC=100, GOMEMLIMIT=off, GODEBUG empty; unchanged clean V7 environment. No inherited FDB_NETWORK_OPTION_*.

One workload: read1000, 5,000 measured transactions plus 200 warmup transactions, one worker, transaction timeout 30,000 ms and callback retry limit 10. Every process receives a unique equal-length 68-byte prefix.

Exactly six blocks, three arms per block (18 fresh serial processes). The frozen V7 `make_schedule` uses seed **798001**, arm input order above and workload input order `[read1000]`. Each of the six three-arm permutations occurs exactly once. All commands, environment selections, plans, schedules and script/source/runtime hashes are frozen before the first process.

The scheduler produces the whole 18-entry order once. The diagnostic controller passes each fixed single-entry schedule to unmodified V7 `execute`, with a recorded copy of the plan whose environment selects that entry's declared GOMAXPROCS. This is explicit per-process orchestration, not a claim that unchanged V7 `main` supports arm-specific environments. Each process retains the frozen 2-second cooldown, 1-second pre-run quiescence gate, host telemetry, raw identity/data/lifecycle validation, and maximum 180-second runner cap. No phase instrument is active during timing.

Abort the diagnostic campaign on an identity mismatch, invalid result, lifecycle failure, nonzero callback retry/error, gate failure, or timeout; preserve the partial attempt, do not replace individual runs. A later rerun requires a named amendment. Completion counts and validity may be inspected during acquisition; no effect comparison is made before all 18 finish.

## Two separate syscall traces

Only after all 18 timing processes finish, run one go8 process and then one go2 process under strace. Each has **200 measured + 200 warmup** read1000 transactions and one worker, the same affinity/environment/lifecycle and new unique 68-byte prefix. CGo is not traced again in R18. The two traced processes are serial and are never mixed with uninstrumented timings.

Use the R16 sequence instrumentation: strace `-ff -ttt -T -v -s 128`, per-TID files, and the selected read/write/readv/writev/pread64/pwrite64/sendmsg/recvfrom/sendto/recvmsg/recvmmsg/sendmmsg/epoll_pwait/epoll_wait/epoll_ctl/futex/nanosleep/clock_nanosleep/sched_yield/getrusage syscall set. Match five A–E getrusage boundaries by the exact user/system/maxRSS tuple. Preserve thread-name observations and raw syscall lines. Measured-phase entry counts lie after B return and before C entry; they are not exact per-operation alignment. Report whole-process and phase counts explicitly, with measured-phase counts divided by 200 only. Tracing perturbs scheduling and cannot establish CPU or latency improvements or causal attribution by itself.

## Accounting and descriptive reporting

Retain every uninstrumented raw A–E snapshot and process statistic. Report window CPU `(C-B)/5000`, drained CPU `(D-B)/5000`, and shutdown-inclusive CPU `(E-A)/5000`, plus mean and V7-quantile p99 measured completion latency. E–A includes startup, fixture, warmup, validation, CSV, forced GC and shutdown, but excludes preceding identity hashing and final JSON/process exit. It is not isolated steady-state CPU. Absolute E/5000 is unpaired descriptive only.

For each block report go2/go8, go8/cgo and go2/cgo ratios. Across the six paired log ratios report geometric means and the individual values/range. No confirmatory interval, hypothesis test, equivalence declaration, optional sample-size extension, or threshold-selected shipping recommendation is made. Native Go allocation counters exclude the C++ heap. Secondary diagnostics cannot rescue any R17 endpoint.

Archive registration, full source/provenance references, all 18 timing and two trace artifacts, exact schedules, raw telemetry, all validity records and analysis. Verify all archive-file hashes on VM and after download. R18 precedes any separately authorized M1 work; no timing/profiling/analysis/build overlaps on the VM. The R18 timing-plus-tracing hard budget is 720 seconds; if an additional bounded process cannot fit, abort before starting it rather than exceeding the cap.
