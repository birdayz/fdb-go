# Benchmark campaign tooling

Orchestration for preregistered, counterbalanced benchmark campaigns on
dedicated cloud machines. The benchmark sources themselves live elsewhere:
`tools/clientbench` (closed-loop client executables and the `run.py` /
`analyze.py` runner) and the testing.B targets listed in
`docs/benchmarks/campaign-harness.md`.

| Directory | Use |
|---|---|
| [`singlenode/`](singlenode/README.md) | One dedicated host: `fdbserver` and clients on disjoint CPU groups, the client in a network namespace. Protocol sealing, the client campaign adapter, the testing.B phase runner and analyzer, runtime/source receipts, and controller-side drivers. |
| [`campaigns/`](campaigns/README.md) | Scripts written for one campaign (round drivers, frozen runners, execution and analysis adapters), kept as the record of what ran. |
| `check_imports.py` | Checks that every file imported under `docs/perf/IMPORT-PROVENANCE.tsv` that is JSON, JSONL, Python or shell still parses; with `FDB_PERF_HOME` set it also parses the sources. Run by hand after an import. |
| [`multinode/`](multinode/README.md) | A private cloud network: one client machine and three FoundationDB server machines. Provisioning, teardown guard, cluster layout, plan sealing, the placement probe and ballast tool, shard-placement receipts, the client adapter and testing.B runner, the per-node sampler and telemetry reconciliation. |

Results and insights are written up under `docs/benchmarks/<campaign id>/`, one folder per campaign with a README index and one report per phase: [`hcloud-rebench-20261014`](../../docs/benchmarks/hcloud-rebench-20261014/README.md) for the single-node campaign, and `docs/benchmarks/hcloud-multinode-vpc-20261014/` for the multi-node one.

## Secrets stay out of the repository

Nothing in this tree contains credentials or machine identities. SSH key paths,
known-hosts files, host keys, cloud-init documents, API tokens, server
addresses and labels are always passed as flags or environment variables, and
campaign state (generated host keys, cloud-init, receipts, raw results) lives in
a campaign directory outside the repository. Unit tests use temporary fixtures
and RFC 5737 documentation addresses.
