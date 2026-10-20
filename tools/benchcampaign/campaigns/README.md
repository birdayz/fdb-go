# Per-campaign scripts

Scripts written for one campaign: round drivers, frozen runners, controller-side
execution and analysis adapters. They are the record of what ran and assume that
campaign's directory layout, hosts and SSH configuration, passed in or written as
the placeholders listed in [docs/perf/README.md](../../../docs/perf/README.md#paths-in-imported-documents).
Reusable tooling lives in [`../singlenode`](../singlenode/README.md) and
[`../multinode`](../multinode/README.md). No Bazel target builds or tests these
files, except `fdbmn-20261020/`'s offline controls
(`//tools/benchcampaign/campaigns/fdbmn-20261020:fdbmn_20261020_test`).
`check_imports.py` in the parent directory checks that every imported script
and JSON file still parses.

| Directory | Campaign | Write-up |
|---|---|---|
| `fdb-perf-20261009/` | Single-VM rounds R1–R18: launch/archive drivers, the frozen runner and analyzer (`frozen-protocol/`), per-round plans and analyzers (`rounds/`), the unrun M1 draft (`m1/`). | [docs/benchmarks/fdb-perf-20261009](../../../docs/benchmarks/fdb-perf-20261009/README.md) |
| `fdbmn-20261020/` | Multi-node baseline: controller execution, placement, collection, analysis adapter, build checks and preparation validators. `verify_frozen.sh` proves the pinned collector and analysis adapter. | [docs/benchmarks/fdbmn-20261020](../../../docs/benchmarks/fdbmn-20261020/README.md) |
| `hcloud-rebench-20261014/as-run/`, `hcloud-multinode-vpc-20261014/as-run/` | Controller scripts as they ran for the 2026-10-14 campaigns; historical, with their known defects. | [single-node](../../../docs/benchmarks/hcloud-rebench-20261014/README.md), [multi-node](../../../docs/benchmarks/hcloud-multinode-vpc-20261014/README.md) |
| `rfc-259/` | Screen and campaign wrappers around `servercost.py`/`wirediff.py` for RFC-259, with the script → result map. | [docs/benchmarks/rfc-259](../../../docs/benchmarks/rfc-259/README.md) |
| `rfc-263/` | Local screen and attribution analysis for RFC-263 phase 1 and revision 4. | [docs/benchmarks/rfc-263](../../../docs/benchmarks/rfc-263/README.md) |

The campaign scripts not in this tree, and why, are listed in
[docs/perf/EXCLUDED.md](../../../docs/perf/EXCLUDED.md).
