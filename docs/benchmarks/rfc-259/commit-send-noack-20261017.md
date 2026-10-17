# Commit dispatch without a write acknowledgment

2026-10-17. The native commit now uses the existing `SendFrameEagerContext` path:
an idle socket still gets a direct-write attempt; otherwise the transport queues an
immutable copy and the native actor waits immediately for its registered reply,
proxy change, or native-owner cancellation. It no longer waits for a separate queued
write acknowledgment. Queue admission can still block; this is not a fully
nonblocking transport.

C++ 7.3.77 `tryGetReply` sends through `sendUnreliable` and waits for reply/disconnect,
without a separate writer acknowledgment (`fdbrpc/include/fdbrpc/fdbrpc.h:797–810`).
Commit additionally races proxy changes (`NativeAPI.actor.cpp:6633–6651`); uncertain
attempts still require the existing fence (`6730–6750`). Successful enqueue is not
successful commit. The change applies to original and dummy-fence commits.

## Setup and populations

- Baseline: `57ddc9a7773951dafbbb9669f819f2faf6f5d14d`, after #952. Its client and
  harness production source matches the retained `12a2628db` artifact.
- Candidate: that baseline with only the commit send call changed from
  `SendFrameContext` to `SendFrameEagerContext`. No scratch allocation, coalescing,
  GC-configuration, or extra-yield experiments are included. Candidate
  `commitpath.go` SHA-256:
  `98f157d64a4b24d6924e3a904e2c8935aa7d4fc9b4ff05271b9e875e472b92a0`.
- Same local Ryzen 9 3900X host and native FDB 7.3.77 `single memory` cluster:
  stateless on CPUs 8/20, transaction on 9/21, storage on 10/22. Clients use CPUs 0–7,
  `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`; Go 1.26.9.
- Driver/control commands use CPUs 12–15, as in the earlier campaigns. These are
  SMT siblings of client CPUs 0–3, not separate physical cores. This is shared-host
  evidence, not complete machine isolation. No builds, tests, or live profiling
  ran concurrently with timed comparisons. The final 64-worker confirmation also
  ran without concurrent reviewer work.
- Binary SHA-256, baseline / candidate / CGo reference:
  `78b3894b06198269c52dfc4796fd4b50960c4eb4b893339bb1f480afd90bf687`,
  `c567fac1711198e15768097fdd654e4cde5a7d2e39e188e65f5c4bddceb424db`,
  `82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c`.
  The CGo reference uses libfdb_c 7.3.77 and reports Go 1.26.9. Its exact historical
  source revision was not recorded; the digest identifies the artifact, not a
  reproducible current-source reference build.
- [Raw results](results-commit-send-noack.jsonl): **154 completed configurations**,
  each with a separate short and long invocation. Every configuration passed the
  harness's completion and byte-validation checks. Baseline/candidate order
  alternates by block; CGo anchors run every fifth block.

| Workers | Go pairs | CGo anchors | Short / long iterations |
| ---: | ---: | ---: | ---: |
| 1 | 10 | 2 | 100 / 700 |
| 8 | 20 | 4 | 500 / 6,000 |
| 32 | 10 | 2 | 1,000 / 15,000 |
| 64 | 30 | 6 | 1,000 / 20,000 |

The initial screen and extension supplied 110 configurations, including the first
10 pairs at 64 workers. A separate 44-configuration confirmation supplied 64-worker
blocks 10–29. All rows, including slow runs, are retained.

`tools/clientbench/scripts/servercost.py` attaches `perf stat` to all three server
PIDs. Server cost is the long-minus-short counter difference divided by the
transaction-count difference, including 5% warmup. Client process CPU and latency
come from the long invocation. Different convoy modes between short and long runs
remain a subtraction confound; these comparisons do not eliminate it.

Example, with locally supplied executable, cluster, and PID paths:

```sh
python3 tools/clientbench/scripts/servercost.py \
  --server-pid "$SERVER_PIDS" --binary "$CLIENTBENCH" \
  --cluster-file "$CLUSTER_FILE" --workload set100 --workers 32 \
  --short 1000 --long 15000 --reps 1 --label base-b0 --out results.jsonl
```

Repeat with the other artifact, reverse Go order in odd-numbered blocks, and add
reference anchors. CGo additionally needs the matching library directory through
`--ld-library-path`.

## Client and server cost

Table entries are medians across configurations. Server cycles are thousands per
transaction; client process CPU is microseconds per transaction. CGo anchors are
smaller context populations, not matched Go/CGo pairs.

| Workers | Base client CPU | Candidate client CPU | CGo client CPU | Base server cycles | Candidate server cycles | CGo server cycles |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 128.60 | 126.20 | 187.70 | 3,309.3 | 3,340.0 | 3,283.3 |
| 8 | 65.63 | 61.67 | 53.56 | 523.2 | 487.4 | 455.4 |
| 32 | 45.08 | 42.07 | 30.71 | 240.3 | 238.4 | 149.1 |
| 64 | 40.15 | 37.59 | 29.87 | 149.9 | 151.7 | 141.9 |

Paired ratios use `exp(mean(log(candidate/base)))`, not ratios of medians.

| Workers | Client CPU ratio | 95% interval | Server cycle ratio | 95% interval |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 0.991 | 0.941–1.052 | 1.004 | 0.982–1.024 |
| 8 | 0.952 | 0.895–1.014 | 0.972 | 0.818–1.156 |
| 32 | 0.927 | 0.877–0.965 | 1.002 | 0.976–1.028 |
| 64 | 0.930 | 0.918–0.942 | 1.005 | 0.988–1.025 |

The repeatable result is about **7% lower client CPU at 32 and 64 workers**. No
material server-cost improvement is established. Eight-worker writes remain noisy;
their interval does not establish a client-CPU win or server-cost equivalence.
Allocation counts barely change, as expected for removal of a synchronization
handoff rather than the queued-body copy.

Bootstrap: reset Python `random.Random(20261017)` for each cell and metric. Draw
10,000 paired replicates, each using `n` calls to `rng.randrange(n)` for that cell's
`n` pairs; compute the geometric-mean ratio, sort, and take indices 249 and 9749.
Intervals are nominal and do not adjust for multiple comparisons.

## Latency and the 64-worker confirmation

Arithmetic means below average each long invocation's mean latency. Invocation
budgets are identical within a worker cell.

| Workers | Base mean, µs | Candidate mean, µs | Change | Paired geometric ratio | 95% interval |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 10,368.4 | 10,061.0 | −3.0% | 0.974 | 0.910–1.027 |
| 8 | 12,368.0 | 12,201.3 | −1.3% | 0.974 | 0.834–1.143 |
| 32 | 11,353.2 | 11,181.3 | −1.5% | 0.985 | 0.965–1.002 |
| 64 | 14,270.2 | 14,853.5 | +4.1% | 1.021 | 0.968–1.085 |

The initial 10 pairs at 64 workers had a **10.0% increase** in arithmetic mean latency
(18,629.9→20,499.9 µs), including two slow candidate runs. Those rows remain in the
combined population. In the subsequent 20 alternating pairs, arithmetic mean latency
was 12,090.3→12,030.3 µs (−0.5%); the paired ratio was 0.994, interval 0.937–1.057.
The confirmation does not reproduce the initial increase, but the combined result
does not establish strict 64-worker latency equivalence or a 5% noninferiority bound.
The one-worker 5% latency guard is met by this write comparison; other workloads
were not remeasured for this patch.

## Correctness validation

Real-FDB wire gates exercise original and dummy-fence commits:

- a real commit reply completes the native actor while the socket writer's return
  is still withheld, including a later full-write error;
- the late-error branch is observed through connection failure before database
  shutdown, with post-join request counts and an injected-write byte positive control;
- failures before or inside the decoded target frame preserve uncertainty fencing;
- proxy change while original bytes are queued selects uncertainty handling before
  the original reply is released;
- an original attempt is not resent; legitimate 1020 fence retries are accounted for
  separately, and exactly one fence completes successfully;
- existing owner-retirement, public-cancellation, marshal-buffer reuse, and shared
  connection-health coverage remains enabled.

Wire request counts prove send attempts, not independently the number of data-plane
applications. Existing lost-reply atomic-operation coverage supplies the separate
no-double-application check.

Restoring the old acknowledgment call compiles and fails exactly the four
reply-before-ack cases. The corrected negative full lane had 189 passing targets
(1 executed, 188 cached), one expected failing target, and one target not completed
because of fail-fast. Restoring the candidate passed `just test-full`: **191 targets,
127 executed / 64 cached**. `just test --@rules_go//go/config:race` passed **138 targets,
132 executed / 6 cached**. Tracked and untracked input fingerprints matched around
each run. The fast-race lane excludes the heavy real-FDB client target; it is not a
claim that the new integration cases ran under the local race lane. PR client-race
CI remains a separate merge gate.

## Remaining gap

This is a client-CPU improvement, **not completed parity**. Candidate/CGo median
client CPU still differs by about 1.15×/1.37×/1.26× at 8/32/64 workers; the sparse
reference populations limit those comparisons. Server cost at 32 workers remains
about 1.60× the reference. There is no multi-node rerun.

Further attribution of the existing clean runtime trace found a different upstream
delay at the first persistent convoy split: a 15-request GRV batch had already been
admitted and detached, but its new flush goroutine remained runnable for 285.632 µs
before first execution, including 50.752 µs overlapping GC stop-the-world. It then
incurred a 63.104 µs assist. The adjacent nine-request cohort used a different RPC
and publisher; this was not one interrupted broadcast. This single diagnostic onset
is not an aggregate causal estimate. Investigate timer-expired GRV dispatch placement
while preserving overlapping RPCs and Close ownership, rather than widening the batch
window or inferring a remedy from allocation counts alone.
