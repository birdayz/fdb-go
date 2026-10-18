# Writer turn: client CPU of the write delay

2026-10-18. Since #954 the Go client waits 10–20 µs before writing to a connection, as
libfdb_c's connectionWriter does (FlowTransport.actor.cpp:704-739). Every frame went
through writeLoop: the sender queued it, writeLoop woke, waited, wrote, and woke the
sender again. At low load each of those handoffs is a scheduler round trip. A sender
that already waits for its write (`SendFrame`, `SendFrameContext`: GRVs, reads, location
and metrics requests) now takes the writer's turn when the connection is idle: it waits
out the delay itself and writes its frame, followed by the frames writeLoop handed it
during the wait, in one non-blocking write. Frame bytes and their order are unchanged.

## Behavior

- **Idle connection only.** A sender takes the turn only when no frame is queued and no
  batch is in progress, so its frame never overtakes an earlier one.
- **Batch boundary.** Frames sent during the wait reach the turn through writeLoop, in
  queue order. A frame queued but not yet handed over when the wait ends goes in the
  next batch, after another delay. libfdb_c serializes every request into the
  connection's unsent buffer as it is sent (FlowTransport.actor.cpp:1967-1977) and
  writes all of it when the delay ends (:718), so this boundary differs.
- **Full socket.** The turn writes without blocking. writeLoop writes whatever the socket
  refused before any later frame, as for a direct write, and finishes a partial write
  without a new delay, as connectionWriter keeps writing until nothing is unsent
  (:714-731). A flush marker in a partial batch is acknowledged only after the rest is
  written. One write may exceed libfdb_c's 128 KiB `MAX_PACKET_SEND_BYTES`; the bytes on
  the stream are the same.
- **Scope.** Commits, eager and deferred sends never wait for their write and keep the
  writeLoop path, as libfdb_c's sender never waits for connectionWriter. TLS and non-TCP
  connections also keep it, as does any configuration whose longest jittered delay
  exceeds 1 ms. A sender's wait does not observe its context.

## Setup

- AMD Ryzen 9 3900X, Linux 7.2. FoundationDB 7.3.77, three native processes
  (`stateless`, `transaction`, `storage`, `single memory`) pinned to CPUs 8/20, 9/21 and
  10/22. clientbench on CPUs 0–7 with `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`,
  Go 1.26.9; drivers on CPUs 12–15. No builds, tests or reviewer work ran during
  measurements.
- Binaries: master `eb9f4781a` (Go `966f1d9c…5dcf`, CGo with libfdb_c 7.3.77
  `24729ecd…7a65`) and this change at `cc410189d` (Go `790f01a4…919b`), write delay at
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
| `set100` | 1,000/s | 133.1 / 120.7 / 91.3 | 0.902 (0.869–0.934) | 1.315 (1.310–1.354) |
| `set100` | 2,000/s | 122.8 / 108.7 / 81.0 | 0.886 (0.841–0.926) | 1.336 (1.315–1.349) |
| `get100` | 10,000/s | 90.0 / 85.7 / 87.3 | 0.946 (0.945–0.958) | 0.981 (0.977–1.000) |
| `get100` | 8 workers, closed | 39.8 / 38.1 / 50.1 | 0.956 (0.928–0.969) | 0.763 (0.748–0.770) |
| `set100` | 8 workers, closed | 51.7 / 53.9 / 45.2 | 0.997 (0.872–1.272) | 1.212 (1.083–1.425) |
| `set100` | 32 workers, closed | 38.1 / 37.9 / 32.3 | 0.985 (0.943–1.025) | 1.162 (1.131–1.282) |

- **Paced writes** use about 10% less client CPU, leaving 1.32–1.34× libfdb_c (1.51–1.53×
  in the paced-load report).
- **Reads** use 4–5% less and match or beat libfdb_c.
- **Server cycles**, as observed median ratios to master: 1.004–1.007 on the paced rows,
  0.974 on closed `get100`, 0.986 on closed `set100` at 32 workers. Closed `set100` at
  8 workers is bimodal (0.995–2.048).
- **Closed-loop writes.** An 8-pair alternating screen of the same binaries
  ([24 configurations](results-writer-turn-screen.jsonl); geometric means of paired
  ratios with Student-t 95% intervals on their logarithms) puts closed `set100` at 32
  workers at 0.998× master client CPU (0.954–1.045), server cycles 1.022 (0.976–1.070),
  throughput 0.927 (0.810–1.062). Paced `set100` at 2,000/s in the same screen: client
  CPU 0.888 (0.820–0.962), server cycles 0.965 (0.813–1.145).
- **Pacing.** At `set100` 2,000/s each arm fell behind schedule in two of four blocks
  (1.6–4.7% of starts over 1 ms late); at 1,000/s the turn and libfdb_c arms each in one
  block (under 1%). At `get100` 10,000/s both Go arms stayed on schedule and libfdb_c's
  starts bunched in every block (1.3–1.5% over 1 ms late), as in the paced-load report.

## Rejected alternatives

- **The commit taking the turn too**, through a waiting variant of its send. Six
  alternating blocks of paced `set100` ([36 configurations](results-writer-turn-commit.jsonl),
  with master): median client CPU 111.9 against 116.7 µs at 1,000/s and 102.8 against
  103.3 µs at 2,000/s, paired ratios 0.955 and 0.988 with neither interval excluding 1.
  One row, `turn5eager-2000-b4` (39.3 µs, 1,833 operations/s), is an outlier, probably a
  disturbed run; it alone widens the 2,000/s interval to 0.754–1.772 (about 0.94–1.02
  without it). Without that block the 2,000/s medians are 102.1 and 103.4 µs and the
  median paired ratio 0.976, so the conclusion does not change.
  Both arms there are this change's tree, with and without the commit's turn. An
  earlier build with the commit's turn (`4fb975432`, before the review fixes) put
  closed `set100` at 32 workers at 1.030× master (1.008–1.052) in an 8-pair screen
  ([24 configurations](results-writer-turn-commit-screen.jsonl)). It would also have
  made the commit wait for the writer, which libfdb_c's `tryGetReply` does not
  (fdbrpc.h:797-810).
- **Reference costs** from [27 configurations](results-writer-turn-prototype.jsonl),
  three blocks of paced `set100` 2,000/s, paced `get100` 10,000/s and closed `set100` at
  32 workers:
  - With the delay off (`WithCoalesceDelay(0, 0)`) a paced write costs 86.6 µs. This is
    a reference measurement, not a bound: turning the wait off also changes batching and
    wire timing, so it is not an option.
  - A sender-as-writer prototype that also read writeCh saved 18 µs per paced write but
    could write a frame ahead of one queued earlier by the same goroutine.
- **A thread sleep** (`nanosleep` in writeLoop instead of the timerfd) saved about 12 µs
  per paced write but raised paced `get100` latency from 328 to 431 µs: the sleeping
  thread holds its P, and the runtime did not retake it within the wait. That run's file
  was overwritten by the next prototype.
