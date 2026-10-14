# Multi-node same-VPC campaign tooling

Tooling for a realistic remote FoundationDB benchmark: one client machine with no
`fdbserver`, three FoundationDB server machines, all on one isolated private
cloud network. Benchmarks themselves are the frozen harness
(`tools/clientbench`, `pkg/recordlayer/benchfetch`, `.../planskip`,
`pkg/relational/sqlbench/fetchmethod`); see
`docs/benchmarks/campaign-harness.md`. The protocol, frozen orders, provisioning
plan and every receipt live in a campaign directory **outside** this
repository.

Nothing here holds a credential, an address of an existing machine or a label
value. Every script takes keys, IPs, labels, names, output directories and
binary paths as flags. Scripts that write campaign state refuse output paths
inside a Git work tree.

| Script | Does | Safe default |
|---|---|---|
| `provision.py` | hcloud commands for the network (one cloud subnet), the firewall (controller SSH/ICMP only), an optional campaign SSH key and four servers. Servers are created stopped, attached with fixed private IPs, then powered on. `--role client` then `--role node1..3 --skip-shared` stages it (for example, build on the client before the nodes exist). Every resource is labelled `campaign=<value>` and `deadline=<epoch>`. | Prints the commands. `--execute` also needs `--teardown-deadline` (at most `--max-lifetime-hours` ahead), 0600 cloud-init files, and passes read-only preflight checks: no name collision, no overlapping network, no `--protected-cidr` overlap. |
| `teardown_guard.py` | Deletes servers, firewalls, SSH keys, networks that carry the campaign label **and** whose name is the prefix or `prefix-*`, by ID, in dependency order, then lists again and fails on survivors. Labelled resources outside the prefix or matching `--protect-name`/`--protect-id` are reported and refused. | Reports only. `--deadline` waits, then acts; with `--execute` it is the hard lifetime guard. |
| `render_cloud_init.py` | `keys`: ed25519 host keys per host via `ssh-keygen`. `render`: cloud-config per host installing that key as the only host key (password login off, apt timers off) plus a `known_hosts` keyed by `HostKeyAlias`. | Refuses a private key passed as the controller public key, open key permissions, existing outputs, repo paths. |
| `fdb_cluster.py` | `render`: cluster file (three coordinators), `configure new double <engine> ...`, systemd units pinning each process to one SMT sibling pair with `Restart=no`, an fdbmonitor-format record, nftables (VPC + controller only), `install-units.sh`. `check-roles`: status json against the layout. | Writes only to a new output directory. |
| `placement_receipt.py` | `receipt`: from status json and a placement probe document, verifies replicas, storage roles, shard consistency, Record Layer key derivation, and per cell the fraction of rows whose index entry and record chunks are on disjoint storage servers (and machines). `compare`: placement unchanged. | Read-only on its inputs; exit 1 on a failed check. |
| `seal_multinode.py` | `orders`/`check-orders`: frozen arm orders from the protocol seeds, with the same derivation `run.py` uses. `client-plans`: per (phase, workload) plans through the frozen `matrix_plan.make_plan` and `run.make_schedule`, checked against `orders.json`. `testingb-plans`: per-phase job lists with the exact sub-benchmark regex. | Leaves `new_harness_*` workloads/cells unsealed unless `--allow-new-harness` names them. |

## Caveats carried from the frozen harness

- `matrix_plan.py` writes `workers=1`. `seal_multinode.py client-plans` sets the
  protocol's worker count afterwards. This data override is the only one, and
  iterations/warmup are totals across workers.
- `matrix_plan.py` defaults the campaign wall cap to 180 s and `run.py` refuses
  more than 900 s. Every protocol per-workload cap must lie between the plan's
  waits (blocks × arms × 3 s) and 900 s.
- `matrix_plan.py` requires a `server` CPU group. On the client machine it is
  an idle sentinel group (no `fdbserver` runs there) with the same quiescence
  thresholds.

## Not provided here

These need separate review before the campaign uses them:

- The placement probe that writes `fdb-bench-placement-probe/v1`
  (`LocalityGetBoundaryKeys`, `LocalityGetAddressesForKey`).
- The ballast writer for the placement gate.
- The multi-node client adapter and testing.B runner (derived from the
  single-node `client_campaign.py`/`bench_runner.py`).
- The per-node telemetry sampler.
- New harness workloads and cells (`range1000`, `mixed80_20`, `rows100`, a
  persistent SQL fixture). Those need their own reviewed harness commit and a
  rebuild.

## Tests

`just test` runs `//tools/benchcampaign/multinode:multinode_test`
(stdlib `unittest`, temporary fixtures, fake runners). When run directly from a
checkout (`python3 -B tools/benchcampaign/multinode/test_multinode.py`), the
tests also seal plans through the real frozen `tools/clientbench/scripts`. Under
Bazel those scripts are not runfiles, so that test is skipped.
