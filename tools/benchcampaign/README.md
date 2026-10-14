# Benchmark campaign tooling

Orchestration for preregistered, counterbalanced benchmark campaigns on
dedicated cloud machines. The benchmark sources themselves live elsewhere:
`tools/clientbench` (closed-loop client executables and the `run.py` /
`analyze.py` runner) and the testing.B targets listed in
`docs/benchmarks/campaign-harness.md`.

| Directory | Use |
|---|---|
| [`singlenode/`](singlenode/README.md) | One dedicated host: `fdbserver` and clients on disjoint CPU groups, the client in a network namespace. Protocol sealing, the client campaign adapter, the testing.B phase runner and analyzer, runtime/source receipts, and controller-side drivers. |
| [`multinode/`](multinode/README.md) | A private cloud network: one client machine and three FoundationDB server machines. Provisioning, teardown guard, cluster layout, shard-placement receipts and plan sealing. |

Results and insights are written up under `docs/benchmarks/`.

## Secrets stay out of the repository

Nothing in this tree contains credentials or machine identities. SSH key paths,
known-hosts files, host keys, cloud-init documents, API tokens, server
addresses and labels are always passed as flags or environment variables, and
campaign state (generated host keys, cloud-init, receipts, raw results) lives in
a campaign directory outside the repository. Unit tests use temporary fixtures
and RFC 5737 documentation addresses.
