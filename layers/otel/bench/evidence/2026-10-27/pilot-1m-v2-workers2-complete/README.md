# Completed V2 two-worker one-million-span gate

**One completed 1M pilot, not a 100M, HA, production or competitive result.**
Run `rw1m0286` acknowledged exactly **1,000,000 spans in 908.534626470 seconds**
(approximately **1,100.67 acknowledged spans/s**) and completed strict drain,
immutable stored-source census and its single 1,600-query campaign. This is a
local closed-loop gate with an accompanying read-only I/O sampler, not an
approved sustainable offered-load/SLO or CPU-capacity measurement.

Measured source was clean, frozen commit
`c7d00bf938edc6cb9a84df4c63b3bbd3ac22dc94`, executable SHA-256
`e3d5f167aba6c671d31cd278dc16414dcdb784bb36a5f3a516d893f0668f762c`.
The source/binary/config before/after manifests are byte-identical. All **14**
prepared policy files are copied and checked against the frozen policy manifest;
these are not measurements of subsequent active-tree edits. The fixture used one
tenant, eight indexed attributes, batch saves of 32 and **two workers**. The
four-process FDB 7.3.77 Redwood profile has four CPUs aggregate, a 16-GiB container
cap, 4-GiB per-process RSS ceilings and single redundancy; it is not HA or an
engine-only comparison with earlier profiles.

## Load, strict drain and stored census

The ingest receipt reports **31,250 committed batches**, 31,282 source Runs,
31,300 attempts, **18 internal replays, 32 source timeouts and 32 fresh retries**.
The stored immutable-corpus audit reports **1,000,000 source records**, **8,000,000
equality-secondary postings**, **1,000,000 standalone-span-ID entries** and
**1,000,000 intrinsic-time entries**, with zero failures. This audit is not a
concurrent-mutation or arbitrary live-trace completeness proof.

All five drain raw statuses, the certificate, phase pin/ACK and completed marker
are preserved without changing original paths. The final three certified raw
samples have their recorded hashes independently matched to archived bytes.
Strict drain certifies those health/queue/lag observations; the separate census
provides stored-population evidence. The completed driver receipt is
`exit=0 end=2026-10-27T18:03:54Z`.

## Single query campaign: retain every class

`run/all.json` retains all **1,600 timed samples: 16 classes, 100 per class**,
result digests, work accounting and histograms, with **zero reported oracle
failures**. No later warm-query rounds are bundled. Times below are milliseconds
from this campaign, not a population-wide latency guarantee or cold-cache result.

| Class | p50 ms | p99 ms | Maximum ms |
|---|---:|---:|---:|
| trace | 0.636415 | 0.903011 | 1.603978 |
| trace_absent | 0.550793 | 0.863977 | 0.939971 |
| span | 0.569329 | 0.744751 | 0.764839 |
| span_absent | 0.561795 | 0.820655 | 0.873916 |
| genai_response | 0.578426 | 0.703062 | 0.711247 |
| genai_absent | 0.572144 | 0.715024 | 0.760501 |
| genai_model | 1.409250 | 1.769452 | 1.836750 |
| genai_operation | 1.368072 | 1.758762 | **7.923200** |
| genai_provider | 1.421133 | 1.996502 | 2.296571 |
| genai_scope_resource | 0.560752 | 0.772483 | 0.825544 |
| genai_scope_scope | 0.578276 | 0.961872 | 1.546630 |
| genai_type | 0.579167 | 0.905155 | 0.934701 |
| time_narrow | 0.623982 | 0.946673 | 1.004222 |
| time_broad | 0.939580 | 1.268684 | 1.348534 |
| time_absent | 0.556925 | 0.713892 | 0.842005 |
| time_empty | 0.452617 | 0.693614 | 0.711588 |

## Physical accounting and observer boundary

The preserved `/var/fdb/data` `du` readings are 7,557,361,664 bytes before and
10,367,668,224 bytes settled: **2,810,306,560 bytes delta (2.810 GB decimal)**.
This is not an isolated per-run disk footprint, a 100M capacity multiplier or
physical bytes/span. Retained earlier failed roots, trace growth, data-distribution
movement and transaction-log preallocation confound attribution. The source audit
separately reports logical KV bytes; those exclude replication/logs/engine
compression and must not be substituted for physical usage.

Monitor selection is an unmodified completed prefix through **18:03:54 UTC**:
128 records, including 120 raw-backed statuses. Later ongoing monitor records are
excluded. The read-only `.01` I/O sampler's corresponding completed prefix contains
**105 samples**, from 17:46:34 through 18:03:54, with zero sample exit codes. It
reads proc/cgroup counters as `fdb` every ten seconds, using guarded identity checks
and Docker exec; that observer overhead was present, not measured or subtracted.
It performs no database operation. Root intentionally cancelled it after pilot
success; no natural `collector.ended` receipt exists, so no exact cancellation
instant or natural completion is invented. Later I/O samples are excluded.
Raw permission/accounting diagnostics are preserved; counters alone do not identify
a precise storage/kernel bottleneck. Broader I/O/kernel analyses are not included
as if they were part of this completed pilot.

## Contents and portability

`evidence.tar.gz` losslessly contains `run/`, selected `session/`, `policy/frozen/`,
`validation/policy286-final-249/`, the fifteen retained policy-input statuses needed
by that validator, and the selected read-only `io/` receipts/source. The independent
**249/249 pure-file policy validation** includes its frozen sources, harness,
commands, positive/negative fixtures and outputs; it is not another DB workload.
Prior failed workload archives are not duplicated here.

`SOURCE-PATHS.jsonl` and `RAW-SHA256SUMS` inside the bundle identify original local
sources and hashes. `archive-members.tsv.gz` enumerates every member;
`provenance.json`, `scan-report.json` and `SHA256SUMS` record selection, integrity
and a full decompressed address/credential/whitespace scan. Raw fields, raw paths
and intentional whitespace are unchanged under lossless gzip. Other runtime
Docker-inspect/environment dumps and process-state files were excluded, with an
explicit retained negative-fixture exception:
`validation/policy286-final-249/stopped-container.input.inspect.json` is a **full
Docker inspect response**, including `Config.Env`, state, host paths, host
configuration and mounts. Its entire 10,505-byte payload was streamed and reviewed:
the ten environment entries are ordinary FDB configuration/PATH, the only IPv4
literal is loopback, and no credential or nonloopback destination was observed.
Local operational metadata is intentionally present; the fixture was not redacted.
Its raw bytes, validation checksums and the tar bundle remain unchanged. This
corrects the earlier unqualified exclusion claim, not the underlying evidence.

Absolute `/tmp/` paths and deliberate adversarial fixture symlinks are local
provenance, not portable dependencies. Do not blindly execute extracted receipts
or follow links. Reproduction requires fresh explicitly owned paths and all
original approval, instance/run and source/binary/config guards; this archive is
not launch authorization. Archive creation only copied, inspected, hashed and
compressed existing files: no builds, tests, jobs, services, DB actions, staging
or commits. Pilot data remains safeguarded in its retained volume.
