# fdb-perf-20261009

Single dedicated cloud VM, 2026-10-09 to 2026-10-11: `fdbserver` 7.3.77 and the
closed-loop client benchmark on one host, comparing the pure-Go client against
Apple's Go binding over `libfdb_c` 7.3.77 (CGo). The campaign ran as numbered
rounds, each with a frozen plan, counterbalanced arm orders and a sealed archive.
The trees under test are the bases each round names (`affef937`, `8ab5c5a4d`,
`7922a2e5a`, …) plus the patches in [patches/](patches/); nothing here describes
current master.

## Contents

| Path | Content |
|---|---|
| [preregistrations/](preregistrations/) | R17 (confirmatory, cumulative stack on `7922a2e5a`) and its v2 addendum, R18 (GOMAXPROCS diagnostic), the completion record, and the pre-timing sha256 list. |
| [rounds/](rounds/) | Per-round scope, status and results write-ups, one folder per round archive (`pr1-r5` … `pr9-r15`, `stack-r11v2`, `fullstack-r16`–`r18`, `lifecycle-v7`, `hillclimb-*`, `quick-*`, `conflictbuf-*`). R17 results: [rounds/fullstack-r17/analysis/TABLES.md](rounds/fullstack-r17/analysis/TABLES.md) and the independent audit next to it. |
| [patches/](patches/) | The source patches measured as `NEW` arms, and the runner patches of R1–R4. |
| [pprof-breakdown.md](pprof-breakdown.md) | `batchset50` CPU breakdown, Go (`affef937`) vs CGo. |
| [m1/](m1/) | Draft (never frozen, never run) record-layer paired timing protocol. |
| [vm-queue-after-r18.md](vm-queue-after-r18.md) | Candidate rounds queued after R18. |
| [RAW-ARCHIVES.sha256](RAW-ARCHIVES.sha256) | sha256 of each sealed round archive (1.7 GB in total, not in Git). |

Round drivers, the frozen runner (`frozen-protocol/`) and the per-round plans are in
[`tools/benchcampaign/campaigns/fdb-perf-20261009`](../../../tools/benchcampaign/campaigns/fdb-perf-20261009/).
They ran against a VM layout (`/opt/fdbperf`) and an SSH configuration outside the
repository and are kept as the record of what ran, not as reusable tooling; the
maintained tooling is `tools/benchcampaign/singlenode` and `multinode`.

Path and identifier placeholders are explained in
[docs/perf/README.md](../../perf/README.md#paths-in-imported-documents).
