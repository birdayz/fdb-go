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

The repository and saved receipts contain no API credentials. Guarded execution
resolves the pinned local hcloud profile only in controller memory; no token goes
to argv, logs, journals, user-data or VMs. Keys, IPs, labels, names, output
directories and binary paths are explicit inputs. Campaign state stays outside
Git in owned private directories.

| Script | Does | Safe default |
|---|---|---|
| `provision.py` | Dry-run command plans remain available. Execution is one registered seven-resource invocation: one new network, firewall and SSH key plus four stopped ccx43/fsn1 servers, attached to the new VPC before power-on. The image is pinned by numeric ID. | No execution without `--creation-ledger`, a live pinned supervised guard, fresh complete inventory, private inputs and the sealed plan/time policy. No existing-resource adoption, replacements, staged CLI mutation fallback or SDK retries. |
| `teardown_guard.py` | Immediate and deadline cleanup use the same exact-created-kind/ID journal, current nonce/name/labels/protection checks, complete fresh server cascade proof and dependency-ordered one-shot DELETEs. | Legacy selector dry runs are diagnostic only. Execution without the journal refuses. Unknown outcomes, unsafe IP associations, pending actions, owned survivors or unequal ten-kind inventory never count as completion. |
| `inventory.py` | `snapshot`: lists all ten resource kinds of the project and writes `before-<kind>.json` plus `protect.tsv` (`kind`, `id`, `name`, network range). `verify`: lists them again and passes only if every kind's ID set is unchanged. | Read-only. Both directories name the project's other resources: outside Git, 0700/0600. Exit 1 on any difference or failed list. |
| `primary_ip_check.py` | Reads fresh provider metadata for exact journal-owned server IDs, validates their complete allowed auto-delete IP graph and appends association evidence to that same journal. Historical snapshot/selector options do not authorize ownership. | Never mutates an IP. A failed check retains server provenance but grants no server-delete authority; exit 2 requires an owner decision. |
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

## Guarded execution boundary (RFC-283)

The current execution contract supersedes historical selector/staged-provisioning
runbooks. It does not change the frozen FoundationDB harness, workloads or CPU
accounting source. See `rfcs/283-guarded-campaign-creation-ledger.md` for the
reviewed conditional ownership model and complete fault matrix.

- **Paid approval comes first.** Offline qualification is not permission to pick a
  launch T0, initialize the live ledger, arm mutation or allocate anything. Only
  after explicit owner GO may the one foreground provisioner initialize a fresh
  `--creation-ledger`, with `--initialize-ledger`, the sealed `--inventory`, an
  owner-selected `--activation-time`, its live `--supervisor-pid`, accepted benign
  writers and the exclusive owned-resource/association graph reservation.
- The fixed policy is `H=T0+16h`, cleanup starts at `G=H-1h`, resource-create
  admission ends at `C=T0+30min`, and setup has a three-hour maximum. Pass the same
  G as `--teardown-deadline`, with `--max-lifetime-hours 16`. Neither restarts nor
  incident recovery extend these times. Actual resource/IP absence and provider
  evidence determine whether cleanup completed within the fixed local ceiling;
  provider `created` metadata never extends the local wall/monotonic budget.
- Ownership is conditional on the reviewed benign-writer/reserved-graph model,
  not a provider clock-accuracy or never-reused-ID guarantee. `created` must be
  well-formed and retain literal identity continuity; past/future provider values
  are not freshness authority. Observed typed-ID retirement is permanent.
  Whole-response-lost servers require a distinct recovered-server/IP association
  receipt with complete fresh cross-readbacks and typed absence proof. The
  original acknowledgement stays missing and unresolved actions stay pending.
- Initialization registers the existing producer PID/starttime and pins source,
  baseline, profile and private-input hashes. It waits at most60s for a separately
  supervised `teardown_guard.py --execute --creation-ledger ...` foreground process.
  A receipt without current PID/source/heartbeat/ancestry checks cannot admit a
  create. The producer is not replaceable and no mutation child is spawned.
- The immutable journal UUID/private anchor binds authenticated retained
  generations, not a permanently pinned mutable head inode. Each mutation holds
  its single send lease through final validation, one HTTPS request and durable
  acknowledgement/UNKNOWN publication. The API host is fixed, TLS validation is
  normal, redirects/replays are absent and credentials remain controller-only.
- hcloud profile precedence is explicit context/config flag, corresponding
  `HCLOUD_CONTEXT`/`HCLOUD_CONFIG`, then the Unix home CLI TOML defaults.
  `HCLOUD_TOKEN` overrides the selected context credential. Unknown/duplicate
  contexts, unsafe credential files, profile drift and nonofficial endpoints
  refuse; the one-shot transport never falls back to a mutating CLI command.
- `--immediate` requests the same live guard to stop the sole producer, prove
  quiescence and close admission. It is not a second cleanup sender. The deadline
  path uses that identical state machine. Each cleanup retry freshly validates
  exact ownership and every implicitly deleted IP; no direct IP DELETE, detach,
  reassignment or auto-delete flag workaround is authorized.
- A timed-out/lost response is UNKNOWN, never a create retry or terminal absence.
  Conditional recovery needs the durable random nonce, canonical request,
  complete pre-call exclusion, provider creation time and one unique candidate.
  Required known actions must also become terminal. Empty reads cannot discharge
  an issued request. Matching strangers are reported, never adopted.
- Original orphan/temp journal crashes require explicit `--recover-journal` with
  a new private `--recovery-evidence-dir`. The pinned recovery guard proves the
  producer quiescent and previous guard gone, archives adverse bytes, and may
  publish only CLOSING. Provably pre-SENDING work becomes NOT_DISPATCHED; issued
  or uncertain work stays UNKNOWN. Bad MACs/forks, changed pins/boot, ambiguous
  identity or an additional failure during one-shot repair remain incidents,
  not permission to edit files manually, reopen provisioning or use selectors.
- Before H, cleanup uses its remaining reserve, not the expired create cutoff.
  After reserve exhaustion/H, bounded authorized incident cleanup continues
  without benchmark extension or a successful lifetime/cost claim. Unbounded
  provider/local failure is not a provider TTL guarantee. The accepted model
  excludes hostile project writers and requires cooperative reservation of this
  campaign's resource/association graph; read-before-delete is not provider-atomic.

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

`just test` runs the declared multinode controls through Bazel: standard-library
fixtures, synthetic provider wire bytes/process identities/clocks and the real
journal/authorization paths. The HTTPS fixture counts parsed requests, not just
subprocess calls. All data and imported helpers are declared inputs. No control
reads a real cloud credential or allocates a resource. Run tests through the whole
Bazel lane, not standalone Python or `go test`; heavyweight validation and native
review remain separate final gates.
