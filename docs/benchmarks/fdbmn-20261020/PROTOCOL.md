# fdbmn-20261020 — current-master multi-node baseline

**Status: owner-authorized execution on2026-10-20. Local builds precede provisioning; only this campaign's resources may be created or removed.**

The owner explicitly replaced the RFC-263 waiting condition with current master **`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`** (root requests4155/4157). It contains #962–#969: wire fixes, allocation cuts, read-tag sampling, RFC-262 phase2 and its follow-up. **No RFC-263 merge is claimed.** The earlier `ae14b220883c4985031d5659f8b34cd7ecece8a9` build remains rehearsal evidence only; preparation documents and hashes are retained in `provenance/preparation-snapshot/`. The authorized SHA is now fixed in protocol.json/campaign.json and must pass the same frozen-harness/compiler gate before paid launch.

## Sources and campaign identity

- Previous campaign directory: `${FDB_PERF_HOME}/hcloud-multinode-vpc-20261014/`.
- Its `protocol.json`, `orders.json`, `provision-plan.md`, `plans/`, `provenance/`, `results/`, `reconcile/`, `telemetry/`, `analysis/` and `binaries/` are retained. `provenance/previous-{protocol,orders}.json` here preserve the inputs used for this preparation.
- Previous write-up: `docs/benchmarks/hcloud-multinode-vpc-20261014/{README,phase-m1-client,phase-m2-remotefetch}.md`, read through `git show origin/master:...`. Companion source/build archive: `${FDB_PERF_HOME}/hcloud-rebench-20261014/`.
- New private campaign directory: `${FDB_PERF_HOME}/campaigns/fdbmn-20261020/` (0700). All state, credentials, addresses, commands and receipts stay here, outside Git. No worktree edits are part of this preparation.
- Unique resource label: **`campaign=fdbmn-20261020-NONCE`**.
- Unique resource name prefix: **`fdbmn-20261020-NONCE`** (exact prefix or `prefix-*`). Do not use `purpose=fdb-perf`, adopt an existing key/firewall, or infer ownership from any legacy label.
- Private network/subnet: **`10.89.20.0/24`**, `eu-central`, `fsn1`; client `.10`, node1 `.11`, node2 `.12`, node3 `.13`. Current network inventory contains `<existing-cidr>`; it is explicitly passed as `--protected-cidr`. Re-list every network on run day and add every then-existing range. Refuse overlaps; do not renumber or attach existing resources.

`protocol.json` is the machine-readable protocol in the previous protocol's shape. `orders.json` is generated and checked with the repository's `seal_multinode.py`; orders do not depend on the pending binary SHA. They are not executable client or testing.B plans.

## Questions, hypotheses and contrasts

1. **M1:** does current-master Go change mean/p50/p99 latency, throughput, shutdown process CPU (`E−A`), Go bytes/tx or mallocs/tx relative to the previous campaign's final Go tree? Does its comparison with Apple/CGo change? Improvement is a hypothesis, not an inclusion criterion; regressions and unresolved intervals are equally reportable.
2. **M2:** on the new master, how does strict `USE_REMOTE_FETCH` compare with `SCAN_AND_FETCH` for the same Record Layer cases and observed placement labels? The previous campaign found large scans slower with remote fetch and some small cases faster. Repeating that pattern is a hypothesis, not a requirement. No default-policy flip is inferred automatically.
3. The old write-up is a **descriptive cross-campaign comparator**. Host allocation, RTT, placement, time and environment differ. Only concurrent, within-case arm contrasts are paired experiments. Do not attribute a historical absolute difference to these production changes, derive a scaling curve from fixed worker phases, or claim physical-machine disjointness.

### M1 arms (six complete three-arm permutations)

| Protocol name / existing sealer slot | Production identity | Meaning |
|---|---|---|
| `before-Go` / `before` | `267a41434a81723b07e7150869d702636a1b91a9` | **Previous campaign's final Go tree**, not the old allocation baseline. Stored in the sealer's required `production_revisions.before_allocation_work_after_read_parity` key solely for compatibility. |
| `final-Go` / `final` | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | Owner-approved current master, replacing the RFC-263 waiting condition. |
| `CGo-7.3.77` / `cgo` | Same new production tree as `final-Go` | Apple's Go binding against FoundationDB `libfdb_c` **7.3.77**, API 730. This is not a second Go-client implementation from a different source tree. |

Build all measured arms with identical frozen harness bytes and Go runtime. Do not mix a newly built Go arm with an old CGo binary merely because its workload names match.

### M2 arms

`final-ordinary` = `SCAN_AND_FETCH`; `final-remote` = strict `USE_REMOTE_FETCH`. Both methods run in **one new-master binary per benchmark package**, as supported by the existing sealer. M2 does not add a cross-tree production arm. Previous M2 results remain descriptive references.

## Frozen harness and build gate

The workload/timing harness base is **`d94b4c61377d173b7c6884fde426a227dab0f0bb`**; see `docs/benchmarks/campaign-harness.md`. **Declared owner-authorized deviation: “d94b4c613 + #948 lifecycle drain fix + matching run.py validator.”** Before any measurement, the new-master32-GRV capacity smoke completed32 transactions with0transaction errors, but d94 rejected38 queued cleanups even though all38 had executed. Current production legitimately uses `runtime.AddCleanup`; the frozen guard was incompatible, not a demonstrated database correctness failure. Original failed diagnostics are retained under `diagnostic-artifacts/capacity/` and no campaign timing preceded this amendment.

Root authorizations4166/4167/4170 permit only the exact reviewed lifecycle.go delta from **`066920ebb8d22c5618b31ef0a56fa7674c773cdb` (#948)**, its cleanup-chain companion test, and the matching campaign Python validation/prose corrections. The Go drain tracks previous queued cleanup and finalizer counts, requires both to be stable across consecutive drained GC rounds and queued==executed for both kinds, and retains GC-cycle checks,16-round/30s limits. Python run.py drops only its cleanup-counter-growth equality while retaining complete, monotonicity, GC/round and executed==queued checks; fixtures prove completed growth passes and pending cleanup fails. analyze.py changes stale scope prose only. Workloads, measured timing windows, CPU accounting, numeric analysis, pacing, BUILD files, main and output format remain frozen. Every client arm is rebuilt from identical amended harness bytes; per-run before/after cleanup counters remain recorded. Exact combined git diff, patch hash, amended archive hash, all binary hashes and compiler identity are in `build-check/CLEANUP-COMPAT-BUILDS.md` and `frozen/cleanup-compat/`.

Preserve the complete clientbench source/script inventory and original overlay tarballs, plus the declared compatibility overlay; do not substitute current harness HEAD. M2 benchmark source/BUILD files under benchfetch/planskip remain exactly d94. The original unamended executables, source and failed-smoke evidence are retained; amended results are never mixed with them.

Current master changed clientbench pacing/lifecycle code since this harness; **building current harness HEAD is not an acceptable repeat**. M2 benchmark source paths remain byte-identical at the rehearsal tree. Frozen `client-harness.tar.gz`, `common.tar.gz`, `remote.tar.gz`, `final-only.tar.gz` and `harness-doc.tar.gz` are retained in the previous rebench archive and copied under `frozen/overlays/` here. `build-check/README.md` records archive/hash checks, exact rehearsal build commands/results and toolchain limitations. **Both previous-final and current-master exports built all seven frozen-harness targets successfully**, and all14 outputs use the exact previous Go1.26.9 compiler. This establishes rehearsal source compatibility only. The separate authorized-fa8725f1d rebuild receipt is `build-check/EXECUTION-BUILDS.md` and must complete before paid launch.

**Run-day gate:** all three current-master/previous/CGo arms plus new-master fetch and planskip binaries, and `placementprobe_cgo`, must build before timing; archive build logs, `go version -m`, source/overlay SHA receipts, binary hashes, `ldd`, actual libfdb_c/server/CLI hashes, compiler and Bazel versions. Match the previous runtime: Go **1.26.9**, Bazel **9.0.1**, Bazelisk **1.29.0**. A compiler upgrade is a confound, not a zero-cost substitution. Build before paid launch where feasible; if a VM build is required, it consumes the same lifetime budget.

If the authorized current-master tree cannot compile the frozen harness, stop before timing. First identify whether only external BUILD/dependency metadata needs adaptation (keep timed Go sources byte-identical). If an API change requires a source shim, the minimal fallback is a separately reviewed **compatibility-only frozen harness revision**, overlaid identically into every affected compared tree, with all affected arms rebuilt and the protocol amended before any timing. Do not add workloads, change validation/timers/pacing, or patch historical production to obtain a number. The owner GO does not authorize any silent harness shim.

## Topology and runtime (same experimental shape)

Four `ccx43` hosts: 16 dedicated vCPUs, 64 GiB each, AMD EPYC Milan expected but verified from receipts; Ubuntu 24.04 x86 image **numeric ID selected and pinned before launch**. The read-only preparation snapshot is image<image-id> (`provenance/image-ubuntu-24.04.json`); revalidate availability and owner approval on run day. All four stages require the same numeric `IMAGE_ID`, never an alias. One isolated /24 VPC, zero `fdbserver` on the client. No CI runner workloads, no netem, and no contact with existing machines.

- Client: housekeeping/runner CPUs **0–1**, idle sentinel **2–7**, measured client **8–15**; `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, `GODEBUG=''`, `TZ=UTC`, `LC_ALL=C`.
- Server hosts: CPUs **0–1** reserved for housekeeping and 1 Hz telemetry. Every FDB process pinned to one full disjoint SMT sibling pair. Verify actual sibling maps before installing units; a different map is a stop, not permission to hand-edit affinities.
- FoundationDB **7.3.77**, API730, `configure new double ssd-2 commit_proxies=1 grv_proxies=1 resolvers=1 logs=2`.
- **17 processes, 9 storage servers** (3/node). node1: stateless/coordinator `4500` on2–3, commit_proxy4501 on4–5, grv_proxy4502 on6–7, resolution4503 on8–9, storage4504–4506 on10–11/12–13/14–15. node2/node3: coordinator4500 on2–3, transaction/tLog4501 on4–5, storage4502–4504 on6–7/8–9/10–11.
- Same 4/8GiB process memory policy, 2GiB storage cache, `Restart=no`, JSON traces and bounded trace retention generated by `fdb_cluster.py`. Private bind/advertise only. Three coordinators, machineid=zoneid=node name.
- Public inbound only controller SSH/ICMP; host nftables protect private traffic. Campaign-created controller public key and pinned per-host keys; no inherited project key. SSH uses `HostKeyAlias`, campaign known_hosts, `StrictHostKeyChecking=yes`, no trust-on-first-use.
- Record `lscpu`, CPU maps, unit affinities, chrony, routes, MTU, package/runtime hashes and 100-packet client→each-node VPC ping p50/p99 (0% loss). No package installation, compilation or profiling during timing.

## Phases, workloads and frozen sampling

**Fixed order, explicitly confirmed by the owner:** F0 → P0 → **M2rl → M2skip → DD on/settle → M1w1 → M1w64 → M1w8 → M1w32**. This matches the previous actual campaign. M1's R1 clears all user keys once per workload on this dedicated cluster; therefore never run M2 afterwards without a complete new P0 and reseal.

M1 workload order at every worker count: `grv`, `get100`, `get1000`, `range100`, `set100`, `batchset50`, `batchset50x1000`. Same payloads, overwrite-only write state, equal-length arm namespaces, warmup separation, exact persisted final-value validation. Iterations/warmup below are **totals across workers**, not counts to multiply again.

| Phase | Workers | Measured tx/run | Warmup tx/run | Order seed | Phase cap seconds |
|---|---:|---:|---:|---:|---:|
| M1w1 | 1 | 3,000 | 200 | 742001 | 3150 |
| M1w64 | 64 | 32,000 | 1,600 | 742301 | 3150 |
| M1w8 | 8 | 12,000 | 800 | 742101 | 2850 |
| M1w32 | 32 | 24,000 | 1,600 | 742201 | 2850 |

Six blocks per workload, **all six permutations of the three arms once**, shuffled with `random.Random(order_seed)` exactly as `run.py`/`seal_multinode.py`; 18 fresh processes per workload instead of the prior 12. Preserve 2s cooldown +1s quiescence, per-process timeout180s, per-workload campaign cap900s. A full M1 schedule is 504 measured arm processes over28 workloads; do not reduce blocks/transactions to meet a deadline. Because it is now three-arm, identical seeds do not imply identical old pairwise arm order.

M2rl: small (unsplit) / split (100000-byte deterministic padding, two chunks) × `limit1`, `rows10`, `rows1000` × forward/reverse: **12 cases**. M2skip: positive Skip3 × rows10/rows1000 × forward/reverse: **4 cases**. Same frozen1000 fixture Orders IDs0..999, price=id, VALUE price_idx, no record versions: fetch fixtures use quantity=id%7+1; the frozen planskip fixture uses quantity=1. This corrects the prior shorthand without changing any harness/fixture bytes. rows10 scans IDs100..109; skip rows10 selects[100,113) then returns10; skip rows1000 returns997. Preserve strict path/correctness audits, continuation/reason/row/chunk checks. No fallback timed.

M2 has six two-arm counterbalanced blocks, three testing.B repeats per process, `3s` benchtime: 144 processes/432 samples in M2rl (seed742401, cap4700s), 48/144 in M2skip (seed742501, cap1300s). Testing.B means are **not p99**. No rows100/range1000/mixed80_20/SQL/memory scope: those were not measured in the previous `reuse_only` campaign.

### P0 placement and qualification

Use the repository's **`placementprobe_cgo`**, built against pinned7.3.77, not diagnostic location-cache probes. Requalify original scratch small/split controls plus the missing-chunk/stray-suffix/version-key on-cluster mutations, and offline provenance/key mutation controls. Every original must have integrity; every injected defect must fail integrity for the intended reason. No-args/unowned-store probe refusals must exit2 without creating output.

For attempts k=1..3, ballast256/512/1024MiB per benchmark-owned store: DD on → benchmark fixture reseed + fixture receipt → ballast write → movement zero for120s → DD off → clear **only ballast**, verify empty → full canonical transactional probe and receipt/controls. Stop at first attempt fully process-disjoint for every store/cell; otherwise use **attempt3**, never the best-looking one. Integrity failures stop for investigation, not a placement retry. Preserve every attempt.

Receipts must prove exact canonical keys/chunks in record/index data subspaces, complete boundaries, two replicas on storage roles, transactional addresses at V1, V2≥V1 with unchanged DD-off mode, moveKeysLock owner/token and raw keyServers digest, positive read versions, benchmark fixture hash and cluster identity. Seal cell labels before timing. Integral partial placement is valid but reported separately with its fraction. With two replicas over three machines every pair of teams shares a machine: `cross-server` means **storage-process/address disjoint**, not machine-disjoint. Exact previous shard placement cannot be promised on fresh VMs.

R2 takes fresh before/after receipts per case and compares them; any instability contaminates the whole case. M2 keeps DD frozen; after M2, enable DD, settle movement to zero, check roles and run a reconciled R1+R3+R4 M1 diagnostic before M1.

## Gates, telemetry, analysis and adverse results

Retain the previous protocol's full `environment_policy` and R1–R4 contracts in `protocol.json`:

- Pre-arm health/roles/generation, recovery fully_recovered; data lag<1s, durability lag<60s, tLog queue<100MB; DD on requires numeric movement zero, DD off requires disabled and permits absent movement fields. Same CPU/quiescence limits and steal≤1%.
- R3 sampling at1Hz on every server with realtime+monotonic stamps, unit PID/restart/active receipts and before/after chrony; client live telemetry and before/after receipts. Missing data is not clean.
- R4 clock widening per node: `u = max_phase(|offset_client|+disp_client+delay_client/2) + max_phase(|offset_node|+disp_node+delay_node/2) + 1s + 0.001s`. Intersecting intervals count conservatively. Unsynchronised/missing clocks make the phase unverifiable. Same phase-wide malformed-sample versus local gap/clock-step rules.
- Reconcile the **complete expected case population** before any phase ratio or summary. Case verdicts: clean / contaminated / unverifiable. Omit an entire non-clean case from estimates; retain raw attempts, adverse results and unexecuted cases. Do not clean by hand, rerun an arm/case after adverse timing, or discard outliers.
- Previous multi-worker batchset50 cases exceeded the100MB tLog gate; batchset50x1000 then remained unexecuted. Keep both cases planned, the same threshold and conservative stop-on-runner-failure policy; do not tune these away for a clean table. A final-tree/CGo correctness failure stops the phase; historical production is not patched to become benchmarkable.
- Six balanced blocks are the resampling unit. M1: geometric mean paired block ratios new/previous, new/CGo, previous/CGo. M2: median of three within-process repeats, then paired block remote/ordinary ratios. **20,000 whole-block bootstrap resamples, seed492817, nominal95% percentile intervals.** Retain all arms together per sampled M1 block. Absolute values are medians of run-level statistics; no pooled transaction quantiles. Native C heap is not included in Go allocation metrics. `E−A` includes shutdown/finalizer drain/native network stop as before.
- Limited six-block exploratory intervals, no multiplicity adjustment/equivalence/capacity claim, no aggregate speedup across workloads. Publish arm order, both Go SHAs, harness/toolchain/runtime identities, placement fractions and all omissions.

## Time, cost and lifetime policy

Read-only `hcloud server-type describe ccx43 -o json` receipt: `provenance/price-ccx43.json`. fsn1 is **€0.4423 net / €0.526337 gross per host-hour**; four hosts **€1.7692 net / €2.105348 gross per fleet-hour**. Billing is per started host-hour, with IPv4 separate; refresh prices/quota on run day. No fifth build host is planned.

- Expected phases: M2rl3600 + M2skip960 + M1w1 1650 + M1w64 1500 + M1w8 1425 + M1w32 1290 =10425s.
- Fixed planning allowance: setup/F0/P0/controls/capacity4500s + collection600s×6 + final verification300s =8400s.
- **Expected total18825s =5h13m45s**, about20.92 machine-hours; synchronised launch/teardown rounds to24 billed host-hours (six started hours each): **€12.63 gross**, €10.62 net, plus IPv4/any billable extras.
- Conservative caps sum18000s + fixed8400s =**26400s =7h20m**. At eight started hours each (32 billed host-hours): **€16.84 gross**, €14.15 net, plus IPv4. The old protocol's expected totals contained stale arithmetic; these totals are recomputed, not copied.
- **Proposed hard lifetime8h** from the first resource-creation authorization/launch T0; fixed once as epoch `DEADLINE=T0+28800`, persisted in a private launch receipt and passed unchanged to provision labels, R1/R2 and teardown `--deadline`. Never reset between staged creates, between phases or when a build is slow. Owner approves this cost/lifetime before launch. Prebuild before launch; in-cloud builds consume the same8h and can leave later phases unexecuted.
- A phase starts only when remaining lifetime≥its phase cap+600s collection+300s verification. Otherwise record unexecuted-for-budget and continue evaluating in the frozen phase order. Never shrink samples or extend the guard. Capacity smokes must establish the three-arm900s workload cap remains feasible; amend before timing if not.
- Arm `commands/teardown-deadline.sh` in a **supervised foreground job before provisioning**, with tool background mode/lifetime timeout≥8h+15min; record supervisor/job receipt outside Git and verify it is alive. Its `--deadline "$DEADLINE" --execute` is prepared but has **not** been run. This is a controller-side guard, not a provider-side automatic expiry; controller supervision must survive the session. On completion/error, run the immediate campaign-only teardown early. Keep the deadline job until teardown and full inventory verification pass, then cancel it.

## Safety and prepared commands

`provision-dry-run.txt` is actual **printed output only**, no `--execute`; `provision-staged-dry-run.txt` and `commands/provision-dry-run-{client,node1,node2,node3}.sh` prepare the required one-server stages for primary-IP checks. The all-four output is an illustrative topology listing, not the approved execution staging. Its controller CIDR `192.0.2.1/32` is RFC5737 documentation space and its deadline is an example; neither is a live launch setting. Replace them with owner-approved inputs and regenerate the dry-run before authorization. Cloud-init keys/render commands are in `commands/cloud-init.sh`; no keys/cloud-init/private credentials were generated in preparation.

Teardown's exact label+prefix and all **17 `--protect-id` +17 `--protect-name` arguments** are in `commands/teardown-{deadline,immediate,report-only}.sh`. Every existing server, network, firewall, SSH key, volume and primary IP in `inventory/protect.tsv` is protected; duplicate names across kinds are intentional. The guard deletes only server/firewall/SSH-key/network IDs passing **both** label and prefix plus protections. No resource is selected by legacy purpose labels.

**Owner-approved implicit-primary-IP policy:** the implicitly allocated primary IPs need not carry the explicit campaign label/prefix. Stage provisioning **one server per provision.py invocation** (`--role client`, then individually `--role node1|node2|node3 --skip-shared`) so no subsequent server is created before the preceding check passes. Immediately after each server-creation invocation, use read-only server/primary-IP lists to assert every primary IP is assigned to a recorded campaign server ID and has `auto_delete=true`. Cross-check IDs referenced in the server's public_net against the primary-IP list; refuse any protected/pre-existing ID. If any check fails, stop and report its exact ID before continuing; the owner fixes it, never this campaign tooling. Record all campaign server and primary-IP IDs outside Git as campaign-owned. No primary-IP mutation is performed by these checks.

After teardown, the ten-kind inventory must still equal the original exactly. If a primary IP survives, verify its ID against the recorded campaign-owned primary-IP list, retain the failure/diff and report it to the owner; **do not delete it automatically**, even if campaign-owned. The owner may delete that exact ID after checking. The provisioner creates no volumes/floating IPs/load balancers/etc., and teardown_guard intentionally covers only its four explicit kinds. Unexpected resources stop completion; never broaden selectors or adopt existing resources. No repository provisioning code or safety flag was changed.

`verify_inventory.py` lists **all ten inventory kinds** and writes fresh JSON snapshots plus per-kind unified ID diffs against `inventory/before-*.json`. It exits nonzero loudly for missing IDs, extra IDs, failed lists, malformed/duplicate IDs or missing baseline files. Success means exactly the pre-existing resource-kind/ID set, not merely zero campaign-labelled servers. Any concurrent project change also fails; never rebaseline silently to make teardown green.

Preparation evidence: `provenance/verify-preparation.log` confirms **17 original IDs across10 kinds** unchanged; `provenance/teardown-report-only.log` and `work/teardown-report-only/` confirm zero matches for the new selector and no deletes; `provenance/teardown-refusal-controls.json` confirms all17 resources are refused by label/prefix/ID/name, including accidental relabel and isolated ID/name protection controls. This is live read-only discovery plus offline classification testing, not a destructive teardown rehearsal.

## Run-day and reproducibility handoff

Use `RUN-DAY.md` and prepared `commands/` files. Authorized current-master builds, fresh environment/placement, actual pinned cloud-init, executable client/testing.B seals, launch deadline and methodology gates remain mandatory. Owner provisioning authorization was supplied in root requests4155/4157. `REPRODUCIBILITY.md` lists retained/missing archive parts, tooling differences and the current build evidence. No benchmark measurement or full/fast repository test lane is claimed by this preparation.
