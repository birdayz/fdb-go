# Guarded ingestion strategy diagnostic

Source: `290d0d7b7ff07fc214ed0b9faf95faa11e259985` (`otelbench-v1`).
Same database images and 4-CPU/4-GiB limits as [the environment receipt](../environment.txt).
The database services were recovered on their retained volumes before this sweep.

Each arm requested 50,000 immutable spans, one tenant, eight indexed attributes,
seed 283, match every 100, FDB time-ordered attribute postings, maximum batch 32,
and opt-in source-batch diagnostics. All transaction guards remained enabled.
This is an ordered, single-pass strategy diagnostic, not a repeated controlled
capacity comparison: the existing database grew between arms and background
storage work was not reset. Run-namespace lengths also differ slightly.

| Save mode | Workers | Completed load time | Completed-load spans/s | Outcome |
|---|---:|---:|---:|---|
| serial | 1 | 112.590 s | 444 | source audit and queries passed |
| pipelined | 1 | 84.051 s | 595 | source audit and queries passed |
| batch | 1 | 92.946 s | 538 | source audit and queries passed |
| batch | 2 | 44.578 s | 1,122 | source audit and queries passed |
| batch | 4 | 26.967 s | 1,854 | source audit and queries passed |
| batch | 8 | — | — | failed; owned run remains loading |

The eight-worker arm returned `transaction_timed_out (1031)` while reading store
information for the batch starting at ordinal 10,624. Its failure receipt reports
10,624 acknowledged source rows after 6.062 s. That is neither a complete source
census nor successful ingest throughput. A subsequent public audit was correctly
rejected because the run was not ready. Ingest failure is represented by the
nonzero command exit and `error`; the receipt's `failures` field counts query
sample failures and must not be used alone to admit an ingest result.

All five completed loads passed full immutable source audits and 100 selective
plus 100 trace queries each: 1,000 timed, independently checked answers in total.
Those small query populations are correctness controls, not tail-SLO evidence.
No ClickHouse arm was run in this strategy sweep.

The JSON/JSONL files are unmodified gzip-compressed outputs. Perf counters cover
the owned client process, not the database server; JSON UTC timestamps are
authoritative because perf's header uses host local time. Diagnostics are
source-batch-only and phase totals overlap across workers and parent/child work;
normal save/load timer coverage differs for the batch API. Physical files record
whole-volume allocated bytes at sampling time, not per-arm isolated storage or
steady-state write amplification.

The driver stopped at the failed final arm. Post-failure source/binary checks,
container/physical snapshots, status and loading-audit rejection were captured
separately. `source-manifests.sha256` records matching before/after digests; full
manifests remain in the local runtime directory. No failure was discarded or
retried to turn the sweep green. This does not establish 1M/100M capacity, loaded
query latency, matched replicated durability, or production readiness.
