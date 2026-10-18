# Writer turn: client CPU of the write delay

2026-10-18. Since #954 the Go client waits 10–20 µs before writing to a connection, as
libfdb_c's connectionWriter does (FlowTransport.actor.cpp:704-739). Every frame went
through writeLoop: the sender queued it, writeLoop woke, waited, wrote, and woke the
sender again. At low load each of those handoffs is a scheduler round trip. A sender
that waits for its write anyway (`SendFrame`, `SendFrameContext`, and the commit through
`SendFrameAloneContext`) now takes the writer's turn when the connection is idle: it
waits out the delay itself and writes its frame, followed by every frame sent meanwhile,
in one non-blocking write. The bytes, their order and the delay are unchanged.

## Behavior

- **Idle connection only.** A sender takes the turn only when no frame is queued and no
  batch is in progress, so its frame never overtakes an earlier one.
- **Frames sent during the wait** reach the turn through writeLoop, which hands over
  every queued frame in queue order. A frame writeLoop has not handed over when the
  wait ends goes in the next batch, after the next delay.
- **Full socket.** The turn writes without blocking; writeLoop writes whatever the
  socket refused before any later frame, as for a direct write. The rest of a partial
  write now continues without a new delay, as connectionWriter keeps writing until
  nothing is unsent (FlowTransport.actor.cpp:714-731).
- **Scope.** TLS and non-TCP connections, and delays configured above 1 ms, keep the
  writeLoop path; a sender's wait does not observe its context. An eager send (the
  first read of a pipelined burst) never takes the turn, so the reads its caller sends
  next still join its batch.

## Setup

- AMD Ryzen 9 3900X, Linux 7.2. FoundationDB 7.3.77, three native processes
  (`stateless`, `transaction`, `storage`, `single memory`) pinned to CPUs 8/20, 9/21 and
  10/22. clientbench on CPUs 0–7 with `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`,
  Go 1.26.9; drivers on CPUs 12–15. No builds, tests or reviewer work ran during
  measurements.
- Binaries: master `eb9f4781a` (Go `966f1d9c…5dcf`, CGo with libfdb_c 7.3.77
  `24729ecd…7a65`) and this change at `4fb975432` (Go `b32161ca…eea2`), write delay at
  its default.
- [`servercost.py`](../../../tools/clientbench/scripts/servercost.py)
  `--no-syscall-count`, one short and one long run per configuration. Paced rows use
  `-rate` with seed = block + 1 and 64 workers.

## Results

[72 configurations](results-writer-turn.jsonl): four blocks, each running every row
for master, this change and libfdb_c in rotated order. Medians per operation; ratios
are medians of the per-block ratios, with their range.

| Workload | Load | Client CPU µs master / turn / libfdb_c | Turn ÷ master | Turn ÷ libfdb_c |
| --- | --- | ---: | --- | --- |
| `set100` | 1,000/s | 135.0 / 108.0 / 90.7 | 0.80 (0.77–0.83) | 1.17 (1.14–1.21) |
| `set100` | 2,000/s | 124.1 / 105.8 / 80.0 | 0.85 (0.84–0.89) | 1.33 (1.27–1.36) |
| `get100` | 10,000/s | 89.4 / 85.4 / 86.7 | 0.96 (0.94–0.96) | 0.98 (0.98–0.99) |
| `get100` | 8 workers, closed | 40.0 / 38.3 / 49.7 | 0.96 (0.91–0.99) | 0.78 (0.72–0.81) |
| `set100` | 8 workers, closed | 52.9 / 56.8 / 47.5 | 1.09 (0.91–1.16) | 1.13 (1.02–1.38) |
| `set100` | 32 workers, closed | 37.9 / 39.0 / 29.8 | 1.02 (0.95–1.15) | 1.31 (1.30–1.34) |

- **Paced writes** use 15–20% less client CPU; the remaining gap to libfdb_c is
  1.17–1.33× (1.51–1.53× before).
- **Reads** use 4% less and match or beat libfdb_c.
- **Server cycles** are unchanged on every paced row (turn ÷ master 0.99–1.01) and on
  `get100` closed.
- **Closed-loop writes** stay bimodal (throughput ranges 0.78–1.29 across blocks). An
  8-pair alternating screen at 32 workers ([24 configurations](results-writer-turn-screen.jsonl))
  puts client CPU at 1.030× master (95% interval 1.008–1.052), with server cycles
  1.001× and throughput 0.957× (0.878–1.042). Equal-work CPU profiles of both
  binaries show the same total and no single hot spot. The same screen confirms paced
  `set100` at 2,000/s: 0.869× (0.851–0.887).
- **Pacing.** At `set100` 2,000/s the turn arm fell behind schedule in three of four
  blocks (1–3% of starts over 1 ms late), master in one and libfdb_c in two. At
  1,000/s each arm was late in one or two blocks (about 1%). At `get100` 10,000/s
  both Go arms stayed on schedule and libfdb_c's starts bunched in every block
  (1.4–1.6% over 1 ms late), as in the paced-load report.

## Rejected alternatives

[27 configurations](results-writer-turn-prototype.jsonl), three blocks, paced
`set100` 2,000/s, `get100` 10,000/s and closed `set100` at 32 workers:

- **The delay off** (`WithCoalesceDelay(0, 0)`) costs 86.6 µs per paced write, the
  lower bound for any mechanism that keeps the wait. Turning the wait off changes the
  wire timing, so it is not an option.
- **A thread sleep** (`nanosleep` in writeLoop instead of the timerfd) saved 12 µs per
  paced write but raised paced `get100` latency from 328 to 431 µs: the sleeping thread
  holds its P, and the runtime does not retake it within the wait. That run's file was
  overwritten by the next prototype.
- **A sender-as-writer prototype that also read writeCh** saved 18 µs per paced write
  but could write a frame ahead of one queued earlier by the same goroutine. The
  merged design only takes frames from writeLoop.
