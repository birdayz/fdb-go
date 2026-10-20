# hcloud-multinode-vpc-20261014 as-run controller scripts

Historical, not for reuse. These are the controller-side driver and analyzer as they
ran for [`hcloud-multinode-vpc-20261014`](../../../../../docs/benchmarks/hcloud-multinode-vpc-20261014/README.md),
with only paths replaced by placeholders (see
[docs/perf/README.md](../../../../../docs/perf/README.md#paths-in-imported-documents)).
They are not fixed in place, because that would misrepresent what ran. They are
superseded by the phase controller and collector written for the fdbmn-20261020
campaign and the tooling in
[`../../../multinode`](../../../multinode/README.md).

| Script | Ran for |
|---|---|
| `run-multinode.py` | Sequential phase driver: node samplers, client receipts, the sealed phase on the client, artifact pull, R4 reconciliation. |
| `analyze_multinode.py` | Analysis of M2 (testing.B) and M1 (client) phases, gated by R4. |

`analyze_multinode.py` imported `tools/benchcampaign/singlenode` and
`tools/clientbench/scripts` from a local worktree (`${FDB_GO_WORKTREES}/fdb-go-bench267`)
whose HEAD at analysis time was the local commit `c87e92243`, later squashed into
#933 (`91f37edde`).

## Known defects

1. `analyze_multinode.py:30-32` (M2): a case missing from the R4 report, including
   `cases: []`, is never added to `omitted`, so it can yield ratios with no
   telemetry verdict.
2. `analyze_multinode.py:44-46` (M2): checks only each run's `result.json` status;
   it never compares `metrics.csv` with the raw stdout.
3. `run-multinode.py:32-33`: re-invoking an already-started phase deletes that
   phase's node telemetry before any reuse check could refuse.

Whether these affected the published results is checked in the campaign README.
