# Monitor-backlog-aborted V2 one-million-span pilot

**FAILED scale gate, not a completed 1M population.** Run `rw1m0285` requested
1,000,000 spans and acknowledged **284,544 rows in 167.995804204 seconds**. The
external monitor stopped this arm after its independent backlog conjunction
crossed the frozen gate. The loader's final `transaction_timed_out (1031)` was
reported **after shutdown**; it is not evidence that this error initiated the stop
or a precise attribution of the underlying bottleneck. No 1M/100M, CPU-capacity,
production, HA or competitive claim follows from this receipt.

Measured source was clean, frozen commit
`c7d00bf938edc6cb9a84df4c63b3bbd3ac22dc94`, with executable SHA-256
`e3d5f167aba6c671d31cd278dc16414dcdb784bb36a5f3a516d893f0668f762c`.
Source, executable and server/policy before/after manifests are preserved and
byte-identical; this is not a measurement of the later active working tree.
The fixture used one tenant, eight indexed attributes, batch saves of 32 and four
workers, with standalone-span-ID and intrinsic-time indexes. The four-process
FDB 7.3.77 Redwood profile has four CPUs aggregate, a 16-GiB container cap and
4-GiB per-process RSS ceilings, with single redundancy. It is not an engine-only
comparison with earlier one-process SQLite profiles.

## Stop evidence and limits

The complete frozen monitor receipt contains **23 records, including 17 raw-backed
status samples and the abort record**. The gate was **storage queue >536,870,912
bytes AND durability lag >60 seconds for six consecutive nominal-10-second
samples**. The final six records carry streaks 1 through 6. The last raw status
has controller timestamp **2026-10-27 17:30:48 UTC**, queue **1,005,195,732 bytes**
and durability lag **91.2467 seconds**. Its monitor request/record timestamp is
17:30:43; the abort log/record is 17:30:48, the stopped marker is 17:30:53, and the
loader end receipt is 17:31:01. These distinct timestamps are preserved, not
rewritten into one clock.

A narrowly admitted sole batch-priority probe warning did not waive the separate
backlog gate. The monitor recorded nine warning-exception samples among 16
bulk-status samples; aggregate false health remains unchanged in the raw data.
The ingest diagnostic counters are **8,951 source Runs, 8,972 attempts, 21 replays,
56 source timeouts and 55 fresh source retries**, with 8,892 committed batches.
These counters and acknowledged rows are not a stored-source census or proof
that every potentially ambiguous write was accounted for.

**No strict final drain, independent source audit, ready population or needle-query
campaign was obtained.** The stopped container could not supply acknowledged
FDB status or physical-size measurements; preserved empty outputs and their
stderr explain that absence. Missing post-stop measurements are **not zero**.
Container statistics, raw latency probes and pre-stop physical measurements are
not substitutes for a stored-data audit or final query measurements. Safeguarded
pilot data remains in its retained volume; archiving did not start, stop, drop,
read or modify that database or any service.

## Archive and portability

`evidence.tar.gz` losslessly retains `run/` (including driver, perf, load outcome,
before/after manifests and errors), `session/` (all monitor and readiness raws,
bootstrap/ownership receipts, policy ACK/pins and stopped marker), `policy/frozen/`
(the exact source matching the load's configuration hashes), and
`validation/policy285-249/`.

The latter is the separate **249/249 pure-file policy validation receipt**, with
48 expected successes, 201 expected rejections and 483 recorded subprocess
commands. Its frozen sources, harness, commands, fixtures, outputs and deliberate
symlink metadata are included, along with the 15 original retained policy-input
statuses needed to interpret that validation. This file-validation result does
not make the workload arm green. Earlier failed arms are already archived
separately and are not duplicated here.

Original raw fields and intentional whitespace remain byte-for-byte intact under
gzip. `SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` inside the bundle identify original
local sources and hashes. `archive-members.tsv.gz` enumerates every tar member;
`provenance.json`, `scan-report.json` and `SHA256SUMS` record selection, verification
and the full decompressed-archive address/credential/whitespace scan. Docker
inspect/environment dumps and process-state files were excluded, not sanitized.

Absolute `/tmp/` paths in receipts/commands and two deliberate adversarial fixture
symlinks are **local provenance**, not portable dependencies. Do not execute
extracted command receipts/drivers or follow fixture symlinks blindly. Reproduction
requires fresh explicitly owned paths and all original approval, instance/run,
source/binary/config pinning and safety guards; this archive grants no launch
authorization. Archive creation performed only file copying, integrity inspection
and compression: no builds, tests, DB/service actions, staging or commits.
