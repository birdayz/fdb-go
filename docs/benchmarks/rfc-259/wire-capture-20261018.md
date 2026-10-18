# Wire capture: where the closed-loop gaps come from

2026-10-18. Two remaining gaps to libfdb_c show up only in closed loops: `set100` at
32 workers costs more on every metric, and `range100` at 32–64 workers costs the
servers more. To see why, [`wirecapture.sh`](../../../tools/clientbench/scripts/wirecapture.sh)
records one clientbench run's loopback traffic with tcpdump, and
[`wiretrace`](../../../tools/clientbench/cmd/wiretrace) decodes it. The decoder
reassembles each client connection, splits it into FDB frames and classifies GRV, commit
and storage range requests by file identifier. It matches each reply by reply token and
groups commits into server batches by the version their replies carry.

## Setup

As in the [writer-turn report](writer-turn-20261018.md): local three-process cluster,
clientbench on CPUs 0–7 (`GOMAXPROCS=8`, `GOGC=100` unless stated), drivers and tcpdump
(in a container with host networking) on CPUs 12–15. Binaries from master `e5209b8c6`:
Go `0d8fe4d5…56e2`, CGo with libfdb_c 7.3.77 `24729ecd…7a65`.

## All workloads, closed loop

[96 configurations](results-broad-closed-loop.jsonl), three blocks with alternating
order, `servercost.py --no-syscall-count`. Medians of the per-block Go ÷ libfdb_c ratios,
with their range:

| Workload | Workers | Client CPU | Server cycles | Throughput | Mean latency |
| --- | ---: | --- | --- | --- | --- |
| `get100` | 1 | 0.77 (0.73–0.81) | 0.98 (0.97–0.99) | 1.39 (1.37–1.40) | 0.72 (0.72–0.73) |
| `get100` | 8 | 0.80 (0.80–0.81) | 0.60 (0.57–0.61) | 1.00 (0.99–1.00) | 1.00 (1.00–1.01) |
| `get100` | 32 | 0.83 (0.83–0.85) | 0.72 (0.70–0.74) | 1.47 (1.46–1.49) | 0.68 (0.67–0.69) |
| `get100` | 64 | 0.87 (0.86–0.88) | 0.98 (0.98–0.99) | 2.14 (2.13–2.16) | 0.47 (0.46–0.47) |
| `grv` | 1 | 0.77 (0.74–0.83) | 0.98 (0.98–0.99) | 1.52 (1.49–1.53) | 0.66 (0.65–0.67) |
| `grv` | 8 | 0.73 (0.71–0.73) | 0.73 (0.66–0.78) | 0.98 (0.97–0.99) | 1.02 (1.01–1.03) |
| `grv` | 32 | 0.57 (0.56–0.57) | 0.68 (0.66–0.72) | 1.31 (1.30–1.32) | 0.77 (0.76–0.77) |
| `grv` | 64 | 0.56 (0.56–0.56) | 0.87 (0.85–0.93) | 1.94 (1.90–1.96) | 0.51 (0.51–0.53) |
| `range100` | 1 | 0.85 (0.76–0.87) | 1.03 (0.95–1.03) | 1.36 (1.35–1.40) | 0.73 (0.72–0.74) |
| `range100` | 8 | 1.05 (1.03–1.06) | 1.00 (0.97–1.02) | 1.16 (1.15–1.17) | 0.86 (0.85–0.87) |
| `range100` | 32 | 1.03 (1.02–1.04) | 1.15 (1.15–1.16) | 1.78 (1.75–1.78) | 0.56 (0.56–0.57) |
| `range100` | 64 | 1.01 (0.98–1.02) | 1.12 (1.11–1.12) | 1.66 (1.65–1.70) | 0.60 (0.59–0.61) |
| `set100` | 1 | 0.85 (0.69–0.86) | 1.00 (0.82–1.01) | 1.02 (1.00–1.55) | 0.98 (0.65–1.00) |
| `set100` | 8 | 0.97 (0.91–1.61) | 0.67 (0.61–2.68) | 1.33 (0.58–1.34) | 0.75 (0.75–1.72) |
| `set100` | 32 | 1.23 (1.10–1.26) | 1.30 (1.26–1.65) | 0.78 (0.68–0.80) | 1.27 (1.25–1.46) |
| `set100` | 64 | 1.06 (1.04–1.11) | 0.93 (0.88–1.30) | 1.40 (0.78–1.46) | 0.71 (0.68–1.28) |

Go uses less client CPU on reads and GRVs, and its throughput reaches 1.3–2.1× libfdb_c's
at 32–64 workers, where libfdb_c serializes every request through one network thread.
The wide `set100` ranges are the bimodal closed-loop write regime described below.

## `set100`: GC pauses split closed-loop rounds

[15 captures](results-wire-capture.jsonl), each of 600 rounds plus 10% warmup. In a
closed loop every worker waits for its commit, so a round of commits reaches the commit
proxy together and forms one batch. `-round` classifies each commit batch, in order, as
holding a whole round or not:

| Client, 32 workers | First split (batch index) | Batches holding a whole round | Commit latency p50 |
| --- | --- | --- | --- |
| Go, `GOGC=100` | 8, 16, 82 | 26, 177, 81 | 10.2–15.9 ms |
| Go, `GOGC=off` | none, 317, 328 | 660, 316, 327 | 6.4–10.3 ms |
| libfdb_c | none, 51, 386 | 660, 650, 385 | 6.4–7.1 ms |

- **Splits persist.** Once a round splits, the two halves stay half a commit cycle apart,
  for both clients. libfdb_c's run with first split 51 re-merged for most of the run;
  Go's runs with GC on spent 85–98% of their batches split.
- **Go splits early because it collects garbage.** With the collector off, Go behaves
  like libfdb_c: one run never split and two held about 320 rounds. With it on, the
  first split comes within the first rounds, where the first collections fall. A pause
  delays some workers' commits past the proxy's batch.
- **At 8 workers** neither client split.
- **Paced writes are not affected.** Paced load has no lockstep rounds to split, and the
  servers' cost per write is at parity there
  ([paced-load report](paced-load-20261018.md)).

Allocation cuts lower the collection rate but do not prevent a split. The
[commit allocation report](commit-alloc-20261018.md) found no change in when the first
split happens.

## `range100`: smaller GRV batches from a faster client

Captures of both clients at 32 workers (four of the rows above) show identical storage
traffic: one range read per transaction with the same parameters (`limit=101`, 80,000
bytes), the same 4,104-byte reply and no further pages. The difference is GRV
batching:

| Client | GRV requests per transaction | Transactions per request | GRV reply latency p50 at the wire | Gap between requests p50 |
| --- | --- | --- | --- | --- |
| Go | 0.115–0.120 | 8.3–8.7 | 93–98 µs | 174–196 µs |
| libfdb_c | 0.084–0.086 | 11.6–11.9 | 93–98 µs | 361–375 µs |

Both clients run `readVersionBatcher`'s adaptive window, half the measured reply latency
smoothed (NativeAPI.actor.cpp). The wire latencies are equal, so the difference is
consistent with libfdb_c measuring latency inside its network thread, including the
thread's queueing, and therefore waiting longer. Neither window was measured directly.
Go sends about 40% more GRV requests, which costs the GRV proxy more and comes with
lower latency.

At matched offered load the extra cost is small. [Four pairs](results-range-paced.jsonl)
of `range100` at 5,000 operations per second (64 workers, both arms on schedule) give
server cycles 1.030 (1.023–1.041), client CPU 1.012 and mean latency 0.860 (354 against
411 µs).
