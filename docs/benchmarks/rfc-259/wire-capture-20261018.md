# Wire capture: where the closed-loop gaps come from

2026-10-18. Two remaining gaps to libfdb_c show up only in closed loops: `set100` at
32 workers costs more on every metric, and `range100` at 32–64 workers costs the
servers more. To see why, [`wirecapture.sh`](../../../tools/clientbench/scripts/wirecapture.sh)
records one clientbench run's loopback traffic with tcpdump, and
[`wiretrace`](../../../tools/clientbench/cmd/wiretrace) decodes it.

- **Decoding.** The decoder reassembles each connection whose SYN was captured, checks
  frame checksums and splits the stream into FDB frames. It classifies GRV, commit and
  storage range requests by file identifier and matches each reply by reply token.
- **Commit batches.** Successful commits are grouped into server batches by the version
  their replies carry. A commit proxy gives every transaction of a batch the same version.
  GRV and storage replies carry observed read versions, which separate batches can share,
  so they are not grouped.
- **Completeness.** The script fails if tcpdump drops packets. The decoder refuses a
  capture with a stream hole or a frame cut off at the end.
- **Scope.** Every connection to a server port opened during the capture counts as a
  client connection; no other connections appeared in these runs.

## Setup

As in the [writer-turn report](writer-turn-20261018.md): local three-process cluster,
clientbench on CPUs 0–7 (`GOMAXPROCS=8`, `GOGC=100` unless stated), drivers and tcpdump
(in a container with host networking) on CPUs 12–15. Binaries from master `e5209b8c6`:
Go `0d8fe4d5…56e2`, CGo with libfdb_c 7.3.77 `24729ecd…7a65`.

## All workloads, closed loop

[96 runs](results-broad-closed-loop.jsonl): 16 configurations, each run for both clients
in three blocks with alternating order, `servercost.py --no-syscall-count`. Medians of
the three per-block Go ÷ libfdb_c ratios, with their range:

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

## `set100`: commit batches split after GC pauses

[15 captures](results-wire-capture.jsonl): 11 of `set100`, 600 measured transactions per
worker after 10% warmup, and 4 of `range100`. Go's runs recorded `GODEBUG=gctrace=1` and
their start time.

In these closed loops, successful commits mostly form batches of exactly the worker
count, one commit per worker. The tool counts such a batch as "whole"; that is a
grouping by size, not proof of which workers' commits it held. "First split" is the
first batch, counted from the start of the capture including warmup, that follows a
whole one and is not whole.

| Client, 32 workers | First split (batch index) | Whole batches | Split batches | Commit latency p50 |
| --- | --- | --- | --- | --- |
| Go, `GOGC=100` | 37, 36, 35 | 147, 71, 34 | 1,028, 1,180, 1,257 | 10.3–15.5 ms |
| Go, `GOGC=off` | none, none, none | 660, 660, 660 | 1, 1, 1 | 6.4 ms |
| libfdb_c | 381, none, 390 | 380, 600, 470 | 561, 121, 382 | 6.5–7.9 ms |

- **Once split, runs stay mostly split.** Go's GC-on runs spent 87–97% of their batches
  split. Two re-merged for a stretch (111 and 36 whole batches) before splitting for
  the rest of the run; the third never re-merged. libfdb_c's third run re-merged for
  its last 81 batches. One libfdb_c run split during warmup, then stayed whole for 600
  batches.
- **Every Go first split followed a GC start by 2.6–3.2 ms**, with one GC inside the
  preceding round ([`gcalign.py`](../../../tools/clientbench/scripts/gcalign.py); the
  data rows carry the GC start times and the first 80 batches). GCs started every
  232–348 ms (medians), so a window that short holds a GC start before only about 1% of
  batches. Two earlier GCs, during whole batches, did not split a round. gctrace reports
  whole milliseconds, and GC times are placed on the capture's clock from a timestamp
  taken just before the client was exec'd, an offset that was not calibrated.
- **With the collector off**, all three Go runs stayed whole throughout. Their four
  collections are at startup and phase boundaries.
- **At 8 workers** neither client split during the measured rounds.

This is a strong time association, consistent with a pause delaying some workers'
commits past a commit proxy batch. The proxy closes batches on timers, size and its
adaptive interval (CommitProxyServer.actor.cpp:429–453, 2524–2532), so the mechanism is
an interpretation, not a measurement. Paced load has no lockstep to split, and the
servers' cost per write is at parity there ([paced-load report](paced-load-20261018.md)).
The [commit allocation report](commit-alloc-20261018.md) found no change in the first
split from fewer collections.

## `range100`: smaller GRV batches from a faster client

At 32 workers, both clients send one range read per transaction with the same
parameters (`limit=101`, 80,000 bytes, selector offsets 1). They get 4,104-byte replies
with no further pages. The captures do not compare selector keys or returned rows. GRV
batching differs:

| Client | GRV requests per transaction | Transactions per request | GRV reply latency p50 at the wire |
| --- | --- | --- | --- |
| Go | 0.119–0.122 | 8.2–8.4 | 93–94 µs |
| libfdb_c | 0.081–0.082 | 12.2–12.3 | 93–94 µs |

- **Same algorithm, different measured latency.** Both clients run `readVersionBatcher`'s
  adaptive window: half the measured reply latency, smoothed, capped by `GRV_BATCH_TIMEOUT`
  (NativeAPI.actor.cpp:7363–7365). The wire latencies are equal, so the difference is
  consistent with libfdb_c measuring a longer latency inside its network thread, for
  example reply broadcast and scheduling (NativeAPI.actor.cpp:7380–7387), and so waiting
  longer. Neither window was measured directly.
- **The result.** Go sends 46–48% more GRV requests, which costs the GRV proxy more,
  and its range reads complete faster (226 µs against 312 µs at the wire).
- **At matched offered load the extra cost is small.** [Four pairs](results-range-paced.jsonl)
  of `range100` at 5,000 operations per second, 64 workers, both arms on schedule:
  server cycles 1.030, client CPU 1.012 and mean latency 0.860 (354 against 411 µs).
  Medians of the per-pair ratios; the server-cycle ratios range 1.023–1.041.
