# Multi-node same-VPC campaign tooling

Tooling for a realistic remote FoundationDB benchmark: one client machine with no
`fdbserver`, three FoundationDB server machines, all on one isolated private
cloud network. Benchmarks themselves are the frozen harness
(`tools/clientbench`, `pkg/recordlayer/benchfetch`, `.../planskip`,
`pkg/relational/sqlbench/fetchmethod`); see
`docs/benchmarks/campaign-harness.md`. The protocol, frozen orders, provisioning
plan and every receipt live in a campaign directory **outside** this
repository. Results are written up in
`docs/benchmarks/<campaign id>/`, for this campaign
`docs/benchmarks/hcloud-multinode-vpc-20261014/`.

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
| `placement_receipt.py` | `receipt`: takes status json, the benchmark's own fixture receipt and a placement probe (v3) listing every physical key of the record `(1,)` and index `(2,)` data subspaces. Store metadata is out of scope, and leftover ballast fails. It returns two verdicts. `integrity_ok` is about the input: a positive shared read version; complete boundaries over `[prefix, strinc(prefix))`; a complete scan of exactly those two subspaces; unique keys; for Record Layer stores the exact canonical key set (unsplit suffix 0, split 1 and 2, no version keys or strays); exactly 2 replicas, storage roles only, one server set per shard. Provenance: addresses transactional at V1 (a location-cache probe always fails), V2 ≥ V1 with identical boundaries, data distribution off (mode 0) with the same move-keys lock owner (any UID: data distribution takes the lock with a random owner when it starts), the same write token and the same `keyServers` digest at both versions, the fixture receipt's hash and the cluster identity equal to `--fixture-receipt` and `--cluster-file`; `sealed_errors` rechecks all of it for seal and `placement_command`. `passed` is `integrity_ok` with every cell at the disjoint fraction. Per cell it reports the fraction of rows whose index entry and record chunks are on disjoint storage **processes**, with a label (`cross-server` or `partially colocated (f)`). Shards holding both kinds of key are counted descriptively. With double replication on three machines, machine-level disjointness is structurally 0 and is descriptive only. `compare`: both receipts have integrity and identical placement, cluster, `keyServers` digest and write token; partial placements compare normally. `controls`: the original must have integrity, and every injected defect must fail integrity. | Read-only on its inputs. `receipt` exits 0 passed, 3 integral but partial, 1 integrity failed. |
| `seal_multinode.py` | `orders`/`check-orders`: frozen arm orders from the protocol seeds, with the same derivation `run.py` uses. `client-plans`: per (phase, workload) plans through the frozen `matrix_plan.make_plan` and `run.make_schedule`, checked against `orders.json`. `testingb-plans`: per-phase job lists with the exact sub-benchmark regex and each case's placement label fixed from `--placement-receipt STORE=PATH`. It refuses receipts without integrity, for another store/layout/record format/preset, or whose cell bounds differ from the scanned preset bounds; integral partial receipts seal under a `partially colocated (f)` label. `client-plans --adapter --layout --source-receipt --vpc-cidr` seals each campaign's run as the R1 adapter command, which drivers fill with `render_run`. | Leaves `new_harness_*` workloads/cells unsealed unless `--allow-new-harness` names them. |
| `placementprobe/` (Go) | H5 `probe` (format v3): at one read version V1, the data-distribution mode, move-keys lock owner and write token, the store's `\xff/keyServers` entries, every physical key of the record `(1,)` and index `(2,)` subspaces and each key's storage addresses; a second read at V2 ≥ V1 must find the same mode, lock and `keyServers` bytes, or nothing is emitted. The production binary `placementprobe_cgo` (build tag `libfdbc`, needs libfdb_c) uses libfdb_c's `getAddressesForKey`, which is transactional at the read version. `placementprobe_go` answers addresses from the pure-Go client's location cache: its probes need `--diagnostic` and never pass receipt integrity. H6 `ballast-write`, `ballast-clear`, `ballast-verify-empty` (mode checked inside the committing transaction) and the scratch control fixture with its defects; receipts `fdb-bench-ballast-receipt/v2`. Every operation takes a store opened from the benchmark's own fixture receipt and records the cluster file's SHA-256 and `description:id`. | Owned stores only, revalidated on every call; refuses before touching the cluster (exit 2). Outputs are exclusive and outside Git. |
| `placement_command.py` | R2's `--placement-command`: H5 probe, `fdbcli status json`, then a receipt with the sealed preset, layout and record format, checked with `placement_receipt.sealed_errors`. `--expect-cluster-sha256` is a sealed literal. | Refuses symlinked work, case or `<when>` directories and outputs inside Git; exit 0 integral receipt, 1 no integrity, 2 refused. |
| `cluster_gate.py` | Shared by R1/R2: generated three-coordinator cluster-file guard, status-json verdict (data movement must be reported and zero with data distribution on; may be absent with it frozen), server gate and client CPU policy. | Missing status fields refuse the run. |
| `client_campaign_mn.py` | R1: multi-node client adapter around the frozen `run.py` for plans sealed by `client-plans --adapter`. Checks manifest, plan and schedule hashes, the engine from status json, the data-movement pre-arm gate and check-roles before the campaign, before every arm and after it. | Refuses a client running `fdbserver`, a changed role/generation baseline, or any unsealed input. |
| `bench_runner_mn.py` | R2: testing.B runner for `testingb-plans`. Rebuilds and compares the sealed plan, verifies each case's sealed placement receipt, takes fresh before/after receipts and compares them; a mismatch omits the case (raw data kept). | A correctness failure stops the phase. |
| `node_sampler.py` | R3: 1 Hz `/proc/stat` and `/proc/vmstat` sampler pinned to the housekeeping CPUs, realtime and monotonic stamps, start/stop receipts (systemd units, `chronyc tracking`, boot ID). | Writes only to a new directory outside Git. |
| `reconcile_telemetry.py` | R4: merges a phase's samples and receipts into one clean / contaminated / unverifiable verdict per case, with run windows widened by the sealed clock bound. Missing data makes a case unverifiable; a phase summary exists only once every case has a verdict. | Read-only on inputs; one exclusive report. Exit 0 all clean, 3 cases omitted, 2 input refused. |
| `build_reconcile_input.py` | Builds R4's `fdb-bench-reconcile-input/v1` for one phase from the sealing manifest (checked against `--manifest-sha256`), the R1/R2 results, client chrony receipts and node telemetry directories. Missing per-case evidence is referenced and listed so R4 marks the case unverifiable. | Refuses changed sealed inputs, unpinned layouts, mismatched receipt hashes and outputs inside Git. |

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

- New harness workloads and cells (`range1000`, `mixed80_20`, `rows100`, a
  persistent SQL fixture). Those need their own reviewed harness commit and a
  rebuild; the `sql/IDX_A` placement store depends on that fixture.
- On-cluster qualification: the protocol's H5/H6 original and mutation controls
  and the status-json field paths are checked on the real cluster before timing.

## Tests

`just test` runs `//tools/benchcampaign/multinode:multinode_test`
(stdlib `unittest`, temporary fixtures, fake runners). When run directly from a
checkout (`python3 -B tools/benchcampaign/multinode/test_multinode.py`), the
tests also seal plans through the real frozen `tools/clientbench/scripts`; the
Bazel target declares those scripts as data, so the test runs there too.
