# OTel ingest scaling benchmark: provisioning and measurement library

The OTel ingest scaling benchmark runs on **Kubernetes**, not on FoundationDB
processes started directly on the hosts. The stack of the last good run
(`k8s-cilium4`, 2026-10-31, Hetzner Cloud `hel1`, six `ccx43` hosts):

- k3s `v1.31.5+k3s1`, Cilium (cilium-cli `v0.20.2`) in native-routing mode over
  Hetzner VPC routes;
- Apple's fdb-kubernetes-operator `v2.38.0` managing one `FoundationDBCluster`
  per stage, FoundationDB `7.3.77`, `single` redundancy;
- load producers run directly on the client hosts.

The reference run, its stack, results and limits are documented in
[otel-ingest-25k-k8s-cilium-scaling.md](../../../../../docs/benchmarks/otel-ingest-25k-k8s-cilium-scaling.md)
(see also [otel-genai-collector-ingest-k8s-scaling.md](../../../../../docs/benchmarks/otel-genai-collector-ingest-k8s-scaling.md)).
Use that report, not this directory, to learn how the benchmark is run.

## The native method is retired

An earlier generation ran FoundationDB as native systemd processes on the
cloud hosts (`otel_native_*` plans: horizontal, diagnostic, session, scaling,
managed watch). Do not plan new work on it. In the report's own words, that
method failed on database log memory: its 2x measured wave ended when the
transaction/log process on `node1` hit its 1 GiB resident-memory monitor
(`OutOfMemory`, exit 20; not a kernel OOM), and the follow-up with a 2 GiB log
budget then failed the strict storage-queue health gate (report section 3,
"Bottleneck 1" and "Bottleneck 2"). The runbooks for that method were removed
from this file; they remain in git history, and the measured outcomes remain in
the report and in `TODO.md`.

## What this directory still is

The Kubernetes runner (`k8s_operator_scaling.py`) imports `driver`, `policy`,
`provision` and `scaling` from this directory as a library: host provisioning
and teardown with exact-ownership checks, plan and source validation, producer
grants, ingest validation and the scaling summary. It also copies `driver.py`,
`policy.py`, `diagnostic.py`, `session.py`, `scaling.py` and `supervisor.py` to
the hosts, and `scaling.py` imports `diagnostic`, `session` and `supervisor`, so
all of them must stay importable.

**The Kubernetes driver script is not in this repository.** The reference run's
copy lives outside it (`/var/tmp/fdb-perf-scaling.v4O6xf/k8s_operator_scaling.py`
on the machine that ran it). There is no committed, reviewed way to rerun the
Kubernetes benchmark from a fresh clone yet, and this README does not provide
one. Importing the modules here has no cloud effects.

## Tests

The normal repository `just test` lane runs one Bazel `sh_test` per module
(`test_horizontal`, `test_provision`, `test_diagnostic`, `test_session`,
`test_forensics`, `test_scaling`, `test_scaling_forensics`,
`test_scaling_log_memory`, `test_supervisor`; see `BUILD.bazel`). Their
fixtures use fake clocks, procfs and providers (several modules patch
`subprocess` to fail on real use). `test.sh` syntax-checks every `*.py` in the
directory and rejects an empty test population, skips, failures and errors.
