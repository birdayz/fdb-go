# Single-node benchmark campaign tooling

Tools for a preregistered benchmark campaign on one dedicated VM: one owned
`fdbserver` and the benchmark clients on disjoint CPU sets, the client in a
private network namespace joined to the server by a veth link. They seal every
plan before timing, never retry, keep all adverse data and refuse any run whose
pinned inputs change. They were used for a real 16-vCPU campaign. The port
replaces that campaign's addresses, keys, paths and constants with the flags and
environment variables below. Apart from that parameterization, the logic is
unchanged except for these behaviour changes:

- In `bench_runner.py`, the loop that parses GNU time CPU fields reused the
  variable holding the job's case key, so every row in `metrics.csv` was
  labelled `process_system_cpu_seconds` instead of its case. The loop now uses
  its own variable, and `test_run_metrics_csv_round_trips_through_analyze` pins
  the fix.
- SQL physical prefixes are checked per measured invocation. The SQL
  benchmarks create a database per body invocation, and testing.B runs a body
  several times per repeat to size `b.N`, so calibration invocations may get
  another directory-layer prefix. `parse_output` used to compare every result
  row's `prefix-B` with every receipt of the case, a false correctness failure
  (phase D1: 9 receipts for 3 rows, one calibration at 5 bytes, every measured
  row 6). It now pairs each row with the last receipt of its case logged before
  that row's result line, the measured invocation's (`measured_prefix_bytes`),
  and requires only that pair to match: sql-read and sql-write, and sql-fetch
  through its `after` receipt, whose `before` receipt must match it.
  Record Layer cases keep the strict check: their store is a fixed subspace.
  `bench_analyze.py` uses the same pairing and no longer rejects a process
  whose receipts differ: differing measured prefixes are reported as
  `physical_prefix_confound`.

The benchmark binaries and their audit receipts are described in
[`docs/benchmarks/campaign-harness.md`](../../../docs/benchmarks/campaign-harness.md).
The client phases use the harness in [`tools/clientbench/scripts`](../../clientbench/scripts).

## Files

| File | Runs on | Purpose |
|---|---|---|
| `seal_design.py` | controller | Turns a version-1 `protocol.json` into the sealed design and writes `orders.json` (the arm orders for every phase). |
| `setup_vm.sh` | VM | Pinned downloads, masked FDB service, namespace and veth, cluster file, source trees, host receipts. |
| `build_harness_vm.sh`, `build_client_vm.sh` | VM | Build the testing.B binaries for every tree and the clientbench binaries. |
| `start_fdb_vm.sh` | VM | Starts the single owned `fdbserver` (`ssd` or `memory`) on the server CPUs. |
| `smoke_vm.sh`, `smoke_client_vm.sh`, `smoke_parser_all.py` | VM | Diagnostic runs, not timing samples: a 1x run of every binary, 32-op client runs, and a check that the parser accepts real logs. |
| `collect_runtime.py` | VM | Source verification, cluster status and runtime receipts (`provenance/runtime-<tag>.json`). |
| `seal_client_plans.py` | VM | Seals one clientbench plan and schedule for each workload of phases A/A2 against `orders.json`. |
| `client_campaign.py` | VM | Safety adapter around the harness's `run.py` for phases A/A2. |
| `run_client_phase.py` | controller | Runs one sealed client phase over SSH, one workload at a time, and copies the raw results back. |
| `watch_client.py` | controller | Analyzes each completed client workload with the frozen `analyze.py`. |
| `network_mode.py` | VM | Enables or restores the netem delay used by C3, writing verified tc and ping receipts. |
| `seal_generic_plans.py` | VM | Seals the testing.B phase plans (B, C1, D1, C2, D2, C3) against the memory-engine receipts. |
| `bench_runner.py` | VM | Makes and runs the testing.B phase plans. |
| `bench_analyze.py` | controller | Paired-block geometric-mean analysis of one testing.B phase. |
| `run_generic_memory_first.py` | controller | Runs the sealed testing.B phases over SSH, then copies and analyzes each one. |
| `render_dashboard.py` | controller | Builds a static HTML dashboard from the analyses. |
| `test_*.py`, `test.sh`, `testdata/` | anywhere | Hermetic unit tests (`//tools/benchcampaign/singlenode:singlenode_test`). |

## Environment variables and flags

No script defaults to an address, key or host. When a required variable is
missing, the script refuses to run. Examples use the RFC 5737 TEST-NET range.

| Variable | Default | Used by |
|---|---|---|
| `FDB_BENCH_ROOT` | `/srv/fdb-bench` | everything on the VM; `--remote-root` default on the controller |
| `FDB_BENCH_NETNS` | `fdb-bench-client` | client namespace name |
| `FDB_BENCH_HOST_VETH`, `FDB_BENCH_PEER_VETH` | `fdb-bench-host`, `fdb-bench-peer` | veth device names: `setup_vm.sh`, `network_mode.py`, `collect_runtime.py`, `seal_design.py` |
| `FDB_BENCH_SERVER_ADDR` | **required** | owned server `host:port` (e.g. `192.0.2.1:4500`): the cluster-file check in `bench_runner.py run` and `client_campaign.py run`, `setup_vm.sh`, `start_fdb_vm.sh`, `network_mode.py`, `seal_design.py`, `seal_generic_plans.py` (written into each sealed run command); `{server_addr}` in client run templates |
| `FDB_BENCH_HOST_VETH_ADDR`, `FDB_BENCH_PEER_VETH_ADDR` | **required** | veth addresses with prefix (`192.0.2.1/30`, `192.0.2.2/30`): `setup_vm.sh`, `seal_design.py`; `network_mode.py` needs the host address (its network must be a namespace route) |
| `FDB_BENCH_CLUSTER_DESCRIPTION`, `FDB_BENCH_CLUSTER_ID` | **required** | `setup_vm.sh` writes `DESCRIPTION:ID@SERVER_ADDR` |
| `FDB_BENCH_SERVER_CPUS` | **required** | `start_fdb_vm.sh` taskset list |
| `FDB_BENCH_CLIENT_CPUS`, `FDB_BENCH_GOMAXPROCS` | **required** | `smoke_vm.sh`, `smoke_client_vm.sh` |
| `FDB_BENCH_HOUSEKEEPING_CPUS` | **required** | `network_mode.py` (ping affinity) |
| `FDB_BENCH_GO_VERSION`, `FDB_BENCH_BEFORE_REVISION`, `FDB_BENCH_FINAL_REVISION`, `FDB_BENCH_CAMPAIGN_TAG` | **required** | `smoke_client_vm.sh` |
| `FDB_BENCH_BUILD_JOBS` | `16` | `build_*_vm.sh` |
| `FDB_BENCH_DIAGNOSTICS_DIR` | unset | optional test input: a real `results/smoke-parser-all` directory (69 logs); unset skips that one test |
| `SCRIPTS_DIR` | runfiles `tools/clientbench/scripts` | optional test input: another frozen harness copy for `test_seal_client_plans.py` |

Python tools that read `protocol.json` take their CPU sets (`runtime.housekeeping_cpus`,
`server_cpus`, `client_cpus`), `GOMAXPROCS`, Go version and harness revision
from it. `bench_runner.py` requires `GOMAXPROCS` to equal the client CPU count,
disjoint CPU sets and `GOGC=100`.

Required flags:

| Script | Flags |
|---|---|
| `seal_design.py` | `--dir CAMPAIGN_DIR` (`--order-seed-base`, default 641001) |
| `bench_runner.py plan` | `--protocol --orders --phase --pins --source-receipt --output`; `--network-receipt` for C3; `--harness-commit` only if `protocol.json` has no `harness_revision` |
| `bench_runner.py run` | `--plan --output --lifetime-deadline` (hard VM deletion time, ISO 8601 UTC) |
| `seal_generic_plans.py` | `--runtime-tag --phase ... --output-dir --lifetime-deadline` (`--network-receipt` for a separate C3 seal) |
| `seal_client_plans.py` | `--scripts-dir --protocol --orders --harness-archive --harness-revision --native-library --output-dir --results-dir`; `--run-template` for the adapter |
| `client_campaign.py run` | `--plan --output --phase --workload --protocol --orders --lifetime-deadline` |
| `run_client_phase.py` | `--root --phase --ssh-key --known-hosts --host` (`--remote-root`) |
| `run_generic_memory_first.py` | `--root --ssh-key --known-hosts --host` (`--remote-root`, `--phase-order`, `--analyzer`) |
| `watch_client.py` | `--raw --output --protocol --scripts-dir --analyze-sha256 --run-sha256` (`--phase`, `--remote-root`, `--once`) |
| `network_mode.py` | `enable|restore --output` |
| `render_dashboard.py` | `--campaign-dir` (`--output-dir`; `--vcpus` and `--machine` unless `protocol.json` has a `cloud` record) |
| `collect_runtime.py` | none (`--engine`, `--tag`, `--expected-smoke-manifests`) |

## Campaign lifecycle

The controller keeps a campaign directory, `$CAMPAIGN`, that holds
`protocol.json`, `orders.json`, `plans/`, `results/`, `analysis/` and
`private/`. On the VM everything lives under `$FDB_BENCH_ROOT`: these scripts
and `tools/clientbench/scripts` from the frozen harness tree, plus `protocol.json`, `orders.json`,
`sources/{legacy,before,pr2,final}.tar.gz`, `overlays/final/*.tar.gz` (including
`client-harness.tar.gz`), `bin/`, `cluster/`, `provenance/`, `plans/` and `results/`.

1. **Design.** Write the version-1 `protocol.json`: phases, arms, workloads,
   `runtime` CPU sets and versions, `production_revisions`. Then run
   `seal_design.py --dir $CAMPAIGN` with the veth and server variables set.
   Record any later amendment as a new protocol version, keeping the old file.
   Set `harness_revision` to the frozen harness commit before sealing plans.
2. **Provision.** Create a dedicated VM. On the controller, start a cleanup
   guard that deletes it by exact ID at its hard lifetime deadline. Pin the
   VM's host key in `private/known_hosts` before the first connection. The
   key, the cloud-init file and the cloud API token all stay in `private/`.
3. **Set up the VM.** Copy the scripts, the protocol, the orders, the source
   archives and the overlays to `$FDB_BENCH_ROOT`. Apply each overlay set to its
   trees as `campaign-harness.md` describes. Run `setup_vm.sh`.
4. **Build.** Run `build_harness_vm.sh` and `build_client_vm.sh`. Write by hand
   the receipts that `seal_generic_plans.py` pins:
   `provenance/go-buildinfo.txt`, `bazel-version.txt`,
   `final-harness-review.json`, `runner-raw-compatibility.json`.
5. **Start FDB (SSD).** Run `start_fdb_vm.sh ssd` in the background and
   configure the new database (`fdbcli --exec 'configure new single ssd'`).
6. **Smoke.** Run `smoke_vm.sh` and `smoke_client_vm.sh`. With the memory engine
   (step 10), `smoke_parser_all.py` checks the parser against real logs. None of
   these are timing samples.
7. **Receipts.** `collect_runtime.py --engine ssd` writes
   `provenance/source-receipt.json`, `actual-source-verification.json` and
   `runtime-ssd.json`.
8. **Seal client plans (A, A2).** Run
   `client_campaign.py self-test --protocol $FDB_BENCH_ROOT/protocol.json --orders $FDB_BENCH_ROOT/orders.json`,
   then `seal_client_plans.py`. Pin the adapter, `bench_runner.py`, the
   protocol, orders, receipts and archives with `--pin`/`--pin-glob`, and pass
   the adapter template:

   ```json
   ["env", "FDB_BENCH_ROOT={root}", "FDB_BENCH_NETNS={netns}", "FDB_BENCH_SERVER_ADDR={server_addr}",
    "timeout", "--signal=TERM", "--kill-after=10s", "{outer_timeout}s",
    "ip", "netns", "exec", "{netns}", "taskset", "--cpu-list", "{housekeeping_cpus}",
    "{python}", "-B", "{root}/client_campaign.py", "run", "--plan", "{plan}", "--output", "{output}",
    "--phase", "{phase}", "--workload", "{workload}",
    "--protocol", "{root}/protocol.json", "--orders", "{root}/orders.json",
    "--source-receipt", "{root}/provenance/source-receipt.json",
    "--lifetime-deadline", "2026-01-01T00:00:00Z"]
   ```
9. **Run the client phases.** From the controller, run
   `run_client_phase.py --root $CAMPAIGN --phase A --ssh-key ... --known-hosts $CAMPAIGN/private/known_hosts --host root@192.0.2.10`.
   Meanwhile, `watch_client.py` analyzes each workload as its raw data arrives.
   Repeat for phase A2.
10. **Switch to the memory engine.** Stop the SSD server and run
    `start_fdb_vm.sh memory`, then configure `single memory`. Run
    `collect_runtime.py --engine memory --tag memory-first` and
    `smoke_parser_all.py`.
11. **Seal the testing.B phases.** Run `seal_generic_plans.py --runtime-tag memory-first
    --phase B --phase C1 --phase D1 --phase C2 --phase D2 --output-dir $FDB_BENCH_ROOT/plans/generic-memory-first
    --lifetime-deadline ...`. It calls `bench_runner.py plan` for each phase and
    writes each run command with `FDB_BENCH_ROOT`, `FDB_BENCH_NETNS` and
    `FDB_BENCH_SERVER_ADDR` in it.
12. **Run the testing.B phases.** From the controller, run
    `run_generic_memory_first.py --root $CAMPAIGN --ssh-key ... --known-hosts ... --host ...`.
    It runs each phase, copies its raw directory and runs `bench_analyze.py`
    (`analysis/<phase>/report.{json,md}`). A phase with too little remaining
    lifetime is recorded as unexecuted, never shortened.
13. **C3 (synthetic latency).** Run `network_mode.py enable --output ...`, seal
    C3 alone with `--network-receipt <output>/receipt.json`, run it, then
    `network_mode.py restore --output ...`.
14. **Dashboard.** Run `render_dashboard.py --campaign-dir $CAMPAIGN`.
15. **Teardown.** Copy every raw artifact back to the controller and verify its
    hashes. Then delete only the campaign VM, by exact ID and labels, and stop
    the cleanup guard. Keep `private/` outside the repository.

## Never committed

The repository holds code, synthetic fixtures and TEST-NET examples only. Keep
each campaign's `private/` directory on the controller and never commit it:
SSH private keys, `known_hosts` and host-key fingerprints, cloud-init user data
(which embeds keys), cloud API tokens, and the real server and veth addresses.
Raw results, provenance and analyses stay in the controller campaign directory
too. `testdata/` holds a sanitized protocol (no cloud record; addresses
rewritten to TEST-NET), the frozen arm orders, and eleven unmodified diagnostic
logs used as parser fixtures.

## Tests

`just test` runs `//tools/benchcampaign/singlenode:singlenode_test`, which runs
every `test_*.py` and `client_campaign.py self-test`, parses every script, and
runs `bash -n` on the shell scripts. The tests are hermetic: temporary
directories and `testdata/` only. Outside Bazel, run them with `python3 -B` from
this directory. `test_seal_client_plans.py` uses the checkout's
`tools/clientbench/scripts` unless `SCRIPTS_DIR` is set.
