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
| `placement_receipt.py` | `receipt`: takes status json, the benchmark's own fixture receipt and a placement probe (v2) listing every physical key of the record `(1,)` and index `(2,)` data subspaces. Store metadata is out of scope, and leftover ballast fails. It returns two verdicts. `integrity_ok` is about the input: a positive shared read version; complete boundaries over `[prefix, strinc(prefix))`; a complete scan of exactly those two subspaces; unique keys; for Record Layer stores the exact canonical key set (unsplit suffix 0, split 1 and 2, no version keys or strays); exactly 2 replicas, storage roles only, one server set per shard. `passed` is `integrity_ok` with every cell at the disjoint fraction. Per cell it reports the fraction of rows whose index entry and record chunks are on disjoint storage **processes**, with a label (`cross-server` or `partially colocated (f)`). Shards holding both kinds of key are counted descriptively. With double replication on three machines, machine-level disjointness is structurally 0 and is descriptive only. `compare`: both receipts have integrity and identical placement; partial placements compare normally. `controls`: the original must have integrity, and every injected defect must fail integrity. | Read-only on its inputs. `receipt` exits 0 passed, 3 integral but partial, 1 integrity failed. |
| `seal_multinode.py` | `orders`/`check-orders`: frozen arm orders from the protocol seeds, with the same derivation `run.py` uses. `client-plans`: per (phase, workload) plans through the frozen `matrix_plan.make_plan` and `run.make_schedule`, checked against `orders.json`. `testingb-plans`: per-phase job lists with the exact sub-benchmark regex and each case's placement label fixed from `--placement-receipt STORE=PATH`. It refuses receipts without integrity, for another store/layout/record format/preset, or whose cell bounds differ from the scanned preset bounds; integral partial receipts seal under a `partially colocated (f)` label. | Leaves `new_harness_*` workloads/cells unsealed unless `--allow-new-harness` names them. |

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

- The placement probe that writes `fdb-bench-placement-probe/v2`, qualified by on-cluster
  original and mutation controls
  (`LocalityGetBoundaryKeys`, `LocalityGetAddressesForKey`).
- The ballast writer for the placement gate.
- The multi-node client adapter and testing.B runner (derived from the
  single-node `client_campaign.py`/`bench_runner.py`).
- The per-node telemetry sampler and the server-telemetry reconciliation audit
  (whole-case omission; missing data omits).
- New harness workloads and cells (`range1000`, `mixed80_20`, `rows100`, a
  persistent SQL fixture). Those need their own reviewed harness commit and a
  rebuild.

## Tests

`just test` runs `//tools/benchcampaign/multinode:multinode_test`
(stdlib `unittest`, temporary fixtures, fake runners). When run directly from a
checkout (`python3 -B tools/benchcampaign/multinode/test_multinode.py`), the
tests also seal plans through the real frozen `tools/clientbench/scripts`; the
Bazel target declares those scripts as data, so the test runs there too.
