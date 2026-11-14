# Kubernetes benchmark runner for the OTel span-writer comparison

This directory is the committed way to run the OTel ingest benchmark on Kubernetes (the method; see
[../horizontal/README.md](../horizontal/README.md) for why the native method is retired). It provisions a Hetzner Cloud
lease, brings up k3s + Cilium + the FoundationDB Kubernetes operator (the stack of the reference run `k8s-cilium4`), runs
the Go own-client / Go C-client / Rust load generators against a fresh FoundationDB cluster per run, verifies every run
by full read-back, and tears everything down by label. Standard-library Python only; it needs the `hcloud` CLI, `ssh`,
`scp`, `systemctl --user` and the sealed executables (see "Inputs").

Results of the campaigns run with this tool, with their caveats:
[docs/benchmarks/otel-ingest-k8s-lean-campaigns.md](../../../../../docs/benchmarks/otel-ingest-k8s-lean-campaigns.md).

| File | What |
|---|---|
| `leanrun.py` | the runner: `cost`, `prepare-assets`, `up`, `sweep`, `down`, `status`, `selftest`, (internal) `watchdog` |
| `climb.py` | the parallelism ladder on one client host (`climb.py run`): fixed or searched steps, per-thread CPU of the generator processes |
| `threadcpu.py` | per-thread CPU sampler shipped to the client host by `climb.py` |
| `condition.py` | pre-split conditioning of a tenant's key range (snapshot, clear + restore in one transaction) |
| `summarize.py`, `summarize_climb.py` | result tables from a campaign's `results*.json` / `climb-results.json` |
| `make_builds_block.py` | turns the sealed executables into the `builds` block of a config |
| `config/*.template.json` | configs with every account-, host- and build-specific value left as `<...>` |
| `test_k8s_bench.py`, `fake_hcloud.py`, `test.sh`, `BUILD.bazel` | offline tests (`just test` runs them; no provider is touched) |

## Inputs (not in the repository)

- The final executables, built with Bazel from one revision: `control-pure` (Go, also corpus / setup / verify),
  `control-native` (Go with the C client), `otel-rust`, plus `libfdb_c.so` and `fdbcli` of the FoundationDB version used
  (`7.3.77`). `make_builds_block.py <dir> <config>...` makes one deployment tar per executable and writes the `builds` block.
- A copy of a template from `config/` filled in: controller IP as /32 (the only address the firewall lets in on SSH), build
  block, asset paths and hashes, campaign name. Keep the filled config, the state directory and the keys outside git.
- The `hcloud` CLI configured for the project (a context or `HCLOUD_TOKEN`); config key `hcloud` is the command to run.
- Corpora: `leanrun.py prepare-assets --config C` generates the fixed corpora with `control-pure` and prints their hashes;
  `climb.py` generates its own on the client host.

State (keys, `state.json`, `log.jsonl`, results, FDB samples, the watchdog copy) lives in `$LEAN_STATE_DIR/<campaign>/`
(config `state_dir`, default `./runs`).

## Runbook

```
python3 leanrun.py selftest                              # offline: manifests and job files equal the validated ones
python3 leanrun.py cost  --config C --cap 6.0            # prices from `hcloud api /pricing`, worst case, refuses above the cap
python3 leanrun.py up    --config C --cap 6.0 --yes      # arms the watchdog FIRST, then creates servers and the stack
python3 leanrun.py sweep --config C --keep-going         # the config's `plan` (or the 1x/2x/3x rotation), fresh cluster per run
python3 climb.py   run   --config C --variants rust_native,go_pure   # the ladder on one client host
python3 leanrun.py down  --config C --yes                # delete by label, prove nothing is left, disarm the watchdog
python3 summarize.py <campaign> results.json             # tables
```

Run `up`, `sweep` and `climb.py` as long-lived user units (`systemd-run --user --unit NAME /bin/bash script.sh`), not from an
interactive shell that can disappear. Add `--dry-run` to any command to print every provider / ssh / systemd command
without executing it.

- **A run** = fresh `FoundationDBCluster` (cluster and PVCs deleted first), tenant setup per producer, optional
  conditioning (`"condition": true`: the real population is ingested once with the Go own client so data distribution has
  split the shards, then the stores are cleared and restored to their fresh state in one transaction), the measured wave
  with all producers released together from a barrier, drain wait, full read-back + census per producer.
  A failed conditioning repeats the run unconditioned; two in a row switch conditioning off.
- **`plan`** entries: `{"stage": N, "arm": "rust_native|go_pure|go_native", "label": ..., "workload_params": {"consumers":
  32, "producers": 16}, "writer_workers": 4, "layout": "A|B|C"}`.
- **Layouts** (`render_fdb_cluster_yaml`): A = the reference cluster (6 storage processes per node, N resolvers, 2N commit
  proxies); B = 12 storage processes per node (3 GiB per process, 1.5 GiB cache; pod limit 44 GiB); C = B plus 2N resolvers and
  4N commit proxies (the stateless class grows by the same number).
- **Co-located generators** (`"colocate_clients": true`, roles `node1..N` and `client1..N`): `client<k>` is an alias of
  `node<k>`, no client hosts are created, and the generator shares the node with the database. Use it only when the account
  limits leave no room for client hosts, and say so next to the numbers.
- **Ladder** (`climb.py`): `climb_steps` maps an arm to a fixed list of `{procs, consumers, workers}` steps, `climb_count`
  gives the spans per process, `climb_stage` the number of database nodes, `presplit_round` how many tenants are pre-split
  at once (the pre-split is done once per variant, in rounds, with a drain wait between rounds; an overloaded database fails
  there). Without `climb_steps` it searches 8/16/32 in flight in one process and then 2..16 processes of 32. Every step
  records the per-thread CPU of each generator process (`/proc/<pid>/task/*/stat`, 1 Hz) and reports the busiest thread.
- **Verification** retries the read-back once after the storage lag has dropped when it hits `transaction_too_old`.

## Safety

- Everything created carries the label `campaign=<name>` and a `deadline` label. `down` and the watchdog delete only by that
  label and then list by label to prove nothing is left; resources of other campaigns or of the account are never touched.
- The watchdog is a user systemd unit (`lean-watch-<campaign>.service`) armed before the first server exists. At the
  deadline it deletes by label even if the controller is gone. `deadline_minutes` + `cleanup_margin_minutes` decides how many
  started hours are billed; plan the deadline so that no extra hour starts.
- `up` refuses without `--yes` and without a cap that covers the worst case (every started hour, servers and IPv4).
- After `down`, compare the account's server, volume, network, firewall, ssh-key, primary-ip and placement-group lists with
  a snapshot taken before `up`.

## Costs and account limits met so far

Hourly billing rounds every started hour up; a lease of 110 minutes + 10 minutes of cleanup margin is two billed hours.
Prices (EUR, gross, `hel1`, with the IPv4): `ccx43` 0.448 / h, `cpx62` 0.212 / h, `cx53` 0.057 / h.

- The dedicated-vCPU limit of the account was 96: six `ccx43` (16 vCPU each) and nothing else dedicated.
- A shared-vCPU limit exists too (between 34 and 37 vCPU in total on the account that was used, other servers included);
  it was hit with the second `cpx62`.
- `cx*` server types had no capacity in `hel1` at all; `server create` answers `resource_unavailable` and the runner retries
  12 times, 30 s apart.
- A spread placement group holds at most 10 servers (the runner splits them per 10).
- A burst of parallel SSH connections to one host is refused (`kex_exchange_identification`); the runner retries those.
- Per-process memory: storage processes of layout A sit at about 3-3.8 GB RSS against 4 GiB; do not reduce the pod limit.

## Tests

`test_k8s_bench.py` (run by `just test` through `test.sh`): manifest goldens and the layouts, cost and billed hours,
co-location aliasing, a full `up --dry-run`, teardown by label against a fake `hcloud` (including a resource that cannot be
deleted and the watchdog firing at its deadline), the conditioning command and parsing code, the per-thread sampler on a
real process, and the generated launch script of the ladder. None of them touch the provider or the network.
