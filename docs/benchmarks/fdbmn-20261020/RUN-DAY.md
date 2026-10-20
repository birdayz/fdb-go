# Run-day checklist — preparation only

No item below authorizes cloud changes. Obtain the owner's **separate explicit instruction to provision**, after RFC-263 merges. All scripts and command files are outside Git; do not run the old archive's hard-coded orchestrator unchanged.

## 1. Freeze production, harness and build inputs (before paid launch)

- [ ] Record the full SHA of merged `origin/master` containing RFC-263, its merge/PR evidence and the read-only source reference. Replace `TBD_AFTER_RFC_263_MERGE` in `campaign.json`/`protocol.json`; retain the preparation version in provenance. Current rehearsal SHA `ae14b220883c4985031d5659f8b34cd7ecece8a9` is not the final arm.
- [ ] Read `PROTOCOL.md`, `REPRODUCIBILITY.md`, `build-check/README.md`, repository `AGENTS.md` and `CLAUDE.md`. Keep the original previous production SHA and d94b4c6 harness SHA unchanged.
- [ ] Export previous-final and merged-master source **outside all Git worktrees**. Overlay the original frozen harness tarballs byte-for-byte into each export using `build-check/build-frozen.sh` as the rehearsed pattern, with the future SHA substituted only in the run-day copy. No worktree patching. Build previous Go, new Go, new CGo, new fetch, new planskip and CGo placementprobe; no additional SQL/new workload scope.
- [ ] Use identical Go1.26.9/compiler identity for all measured arms, pinned FDB7.3.77 runtime payloads, consistent build flags and target linkage. Record actual versus cached build actions. Archive binaries, build metadata, ldd and SHA256 manifests. The exact old runtime is available; see build-check evidence. A future build incompatibility stops the launch pending the minimal reviewed harness decision described in PROTOCOL.
- [ ] Create run-day source archives for the two production trees and frozen overlay archive receipts. On the campaign client, `provenance/source-receipt.json` must contain `harness_commit`, **exactly the same `production_revisions` object as protocol.json**, and nonempty `source_artifacts_sha256` mapping **actual absolute client paths** to hashes. Those paths must all be included in the sealing pins; do not reuse the old receipt's old revisions/paths.
- [ ] Record a fresh tool hash manifest, including multinode tools, singlenode imports and frozen clientbench scripts. The prepared tooling copy is pinned to master ae14b220, not a moving checkout. Do not substitute new clientbench HEAD scripts.

## 2. Confirm isolation, budget and owner authorization

- [ ] Re-run `verify_inventory.py` with a fresh output directory; require exactly the original17 resource IDs across10 kinds. Recheck `inventory/protect.tsv` against all then-existing resources. If the project changed since preparation, STOP and reconcile with the owner; never overwrite/rebaseline the original inventory silently.
- [ ] Re-list all private networks. Prepared CIDR10.89.20.0/24 must overlap none; pass **every existing CIDR** as `--protected-cidr` and every existing protected name in provisioner's `--protected-name`. Extend teardown protection args for any owner-approved new baseline resources without dropping the original17.
- [ ] Confirm four ccx43 quota/availability in fsn1, select/approve **one immutable numeric Ubuntu24.04 x86 image ID** for all four creates (never the mutable alias), price/IPv4 extras and the **8h proposed hard lifetime**. Expected5h14m, conservative7h20m assuming prebuilt binaries. Approve the maximum≈€16.84 gross server cost plus IPv4, not just expected time.
- [ ] Apply the owner's approved implicit-primary-IP exception: these IPs need not carry the campaign label/prefix, but every one must have a new, recorded campaign-owned ID, assignment to a campaign server ID and `auto_delete=true`. No provisioner code change or primary-IP mutation is authorized. Read the per-server staged checks below.
- [ ] Obtain explicit owner provisioning approval and record it outside Git. Obtain owner controller public /32 and campaign controller public-key path; keep its private key solely on the controller. Never use the dry-run's documentation address192.0.2.1/32.

## 3. Render local assets, fix the deadline and arm teardown

On the controller, with actual owner-approved values:

```sh
C=${FDB_PERF_HOME}/campaigns/fdbmn-20261020
umask 077
# Set CTL_CIDR and SSH_PUBLIC_KEY privately; do not paste credentials into Git.
# Set/export IMAGE_ID to the one approved immutable numeric image ID (not ubuntu-24.04).
# commands/cloud-init.sh creates only local host keys and cloud-init, never cloud resources.
bash "$C/commands/cloud-init.sh"
# Set this ONCE immediately before launch, persist it, and never recompute it for later stages.
export DEADLINE=$(( $(date -u +%s) + 8*3600 ))
printf '%s\n' "$DEADLINE" > "$C/provenance/launch-deadline-epoch.txt"
# Regenerate each one-server stage; do not execute the illustrative all-four topology listing.
for role in client node1 node2 node3; do
  bash "$C/commands/provision-dry-run-$role.sh" > "$C/provenance/provision-run-day-dry-run-$role.txt"
done
bash "$C/commands/teardown-report-only.sh"
```

- [ ] Check every printed name/label/CIDR/key path against campaign.json; no legacy purpose selector. All four per-host cloud-init files0600 and directories0700; campaign known_hosts pinned before boot.
- [ ] Review **exact** `commands/teardown-deadline.sh`: label and prefix plus all17 protected IDs/names, `--deadline "$DEADLINE"`, `--execute`. Start it as a supervised **foreground command in a tracked background job**, timeout at least lifetime+15min. Save supervisor/job identity and proof it is alive in provenance. No shell `&`/nohup or unsupervised launch. A controller-side guard is not provider-side expiry; keep reliable supervision.
- [ ] Only after the owner's explicit approval and a live guard, execute the repository `provision.py` with the reviewed dry-run flags plus `--execute`, **one server per invocation**: `--role client`, then separately `--role node1 --skip-shared`, `--role node2 --skip-shared`, `--role node3 --skip-shared`. Preserve the same deadline throughout. Immediately after each invocation, before any next server create, read-only list that server and its primary IPs: every primary IP must be assigned to a campaign server ID and have `auto_delete=true`; cross-check server public_net IDs with the primary-IP list and refuse all protected IDs. Record the server and primary-IP IDs as campaign-owned outside Git. If any check fails, STOP and report exact IDs to the owner before continuing; the owner fixes it by exact ID. Never set auto_delete, relabel, or delete an IP yourself. Do not run printed hcloud commands piecemeal. Any client-only build consumes the same remaining lifetime.
- [ ] Any partial provision failure: invoke `commands/teardown-immediate.sh` under the approved scope, then full verification. Never adopt, detach, relabel, delete or log into existing resources.
- [ ] Read-only post-create inventory/selector check: exactly4 campaign servers,1 network,1 firewall,1 campaign SSH key; private network only these4 hosts; new implicit primaryIP IDs with `auto_delete=true`. No volumes or other extra resource kinds. Record every created ID. Fail on unexpected/mismatched ownership.

## 4. Qualify cluster and runtime (F0)

- [ ] Strict host-key first contact on all4 hosts; no known-hosts override. Match cloud-init completion and pinned keys. Disable apt timers/background upgrades; install hash-verified7.3.77 runtime and chrony; mask distribution `foundationdb.service` everywhere, especially the client.
- [ ] Verify cpu/SMT sibling maps, exact packages/runtime hashes and client has **zero fdbserver**. Record routes, MTU, lscpu, chrony, ping100 packets per node p50/p99 and0% loss. No external CI or build load.
- [ ] Set PRIVATE_IFACE to the observed private NIC and run `commands/render-cluster.sh` locally. Install generated nftables and systemd units **only on the newly created campaign machines**. Match CPUAffinity from MainPIDs; no overlapping sibling pairs; Restart=no.
- [ ] Configure new double ssd-2/1commit_proxy/1grv_proxy/1resolver/2logs. Save status JSON and run `fdb_cluster.py check-roles`; require17 processes,9 storage,3 coordinators, correct roles/engine/redundancy. Verify actual7.3.77 status paths, DD-on/off semantics and health thresholds.
- [ ] Install built binaries and source/provenance archives at final absolute client paths, conventionally `/srv/fdb-bench`; all sealing commands in `commands/` use that R layout. Recheck hashes/ldd against pinned runtime. No downloads/builds/profiling during phases.

## 5. Qualify placement and seal all plans before timing

- [ ] `placementprobe_cgo` build/link/refusal gate (exit2 for noargs and unowned store, no output); never substitute the diagnostic pure-Go address cache.
- [ ] H5/H6 original controls plus all three on-cluster physical defects, and offline controls. Preserve raw probes/status/fixture receipts. Original needs integrity, not full disjointness; every mutation must fail for its intended reason. Clear scratch controls and verify empty.
- [ ] P0 attempts256/512/1024MiB per owned store as protocol: DD on/fixture → ballast → movement zero120s → freezeDD → ballast-only clear → verify empty → canonical probe/receipt/controls. Stop first complete pass; otherwise select attempt3. Do not select placement using timing. Bind every receipt to fixture hash and generated cluster identity.
- [ ] Untimed R2+R3+R4 diagnostic and R1+R3+R4 diagnostic must reconcile clean. **Do the R1 clear diagnostic before the final M2 fixture placement**, or rerun final P0 after it. Capacity-smoke all M1 cases/worker counts with slowest arm, since three-arm schedule has18 processes. No weakening900s or thresholds; any necessary amendment precedes all timing.
- [ ] `seal_multinode.py orders` has already produced orders.json. Re-run `check-orders`; if any protocol arm/order change is approved, create a new version and regenerate before timing, retaining old artifacts.
- [ ] Seal on the client, where pinned absolute paths exist: `commands/seal-client-plans.sh` →28 workload campaigns×18 processes,0 unsealed; `commands/seal-testingb-plans.sh` with selected P0 receipt paths →M2rl12 cases/144 jobs and M2skip4/48,0 unsealed. Seal source archives, runtime/binaries, layout, scripts/imports, probe/fixture receipts and placement command inputs. No `--allow-new-harness` in this campaign.
- [ ] Prepare R2's JSON `--placement-command` using repository placement_command.py, with pinned cluster SHA literal, exact PP/fdbcli/fixture-receipts paths and fresh per-case workdirs. Preserve command and hash with the seals. Do not reuse old hard-coded paths/addresses/keys.
- [ ] Get methodology acknowledgement on the exact protocol, runtime, seals/manifest hashes and phase sequence before timing. Client/testing.B final seals cannot be completed now: merged SHA/real cluster/receipts do not yet exist.

## 6. Time and reconcile — M2 BEFORE M1

- [ ] Fixed sequence: **M2rl → M2skip → DD-on/settle → M1w1 → M1w64 → M1w8 → M1w32**. Before each phase require cap+600s collection+300s verification remaining; deterministic skip-and-continue otherwise. Never reduce samples or extend the lifetime.
- [ ] Before every phase start node_sampler.py on all3 server hosts, pinned0–1,1Hz; require all3 receipt-start files before client work. Take client before/after receipts, node unit/chrony/status receipts and stop receipts; collect/hash all raw data. Ensure a real nonempty expected telemetry population, not merely no errors.
- [ ] M2: invoke existing bench_runner_mn.py with acknowledged testingb manifest SHA, exact sealed placement command and original deadline. R2 performs before/after placement compare. Stop on correctness/runner failure, retain all artifacts. Keep DD off through M2.
- [ ] Run `commands/reconcile-phase.sh` for each phase after copying server telemetry to sealed client paths; capture its exit0/all-clean,3/omissions or2/refusal. No summary before full expected-case R4 reconciliation. Preserve non-clean case verdicts, do not override/retry.
- [ ] After M2, DD on and movement zero, check roles, chrony and R1 diagnostic; preserve final M2 artifacts before any R1 clear. M1 runs sealed R1 commands rendered by `seal_multinode.render_run(manifest_entry['run'], acknowledged_manifest_sha, DEADLINE)`, sequentially in sealed workload order. Do not invoke frozen run.py directly or continue a phase after runner failure.
- [ ] Run M1 with the three sealed arms and exact transactions/warmup; retain batchset50 contamination and subsequent unexecuted work if gates trip as last time. Final/CGo correctness failure stops phase; do not patch old production.
- [ ] Collect traces, results, sources, receipts, runtime and hashes to controller and verify manifests before deleting machines. No analysis needs the VMs to remain alive.

## 7. Tear down, prove inventory, then write up

- [ ] Invoke **campaign-only** `commands/teardown-immediate.sh` early once work/artifacts are complete or on failure; the fixed deadline guard is the fallback. Exact protected IDs/names and label+prefix must not be weakened. Review deletion receipts by ID and require no selected survivors.
- [ ] Run `commands/verify-inventory.sh`: list all10 resource kinds, compare exact ID sets to the original inventory/before-*.json, and require exit0. Extra primaryIPs or any missing protected ID mean **FAIL**, not finished. For a surviving primary IP, verify its exact ID is in the recorded campaign-owned primary-IP list and report it plus the diff to the owner. **Never delete a surviving primary IP automatically**, even when campaign-owned; the owner may delete that exact ID after checking. Do not auto-delete or silently rebaseline any leftover to make verification pass.
- [ ] Only after both campaign deletion and full inventory success cancel the deadline job; retain its final receipt and the verification snapshots/diffs.
- [ ] Analyze clean cases only,20,000 whole-balanced-block bootstrap resamples/seed492817. M1 ratios new/previous,new/CGo,previous/CGo; M2 remote/ordinary. Preserve all arms together when resampling. No pooled percentiles or selective clean sub-blocks.
- [ ] Write results later under `docs/benchmarks/fdbmn-20261020/`, with both production SHAs, harness/runtime/tool hashes, exact order/placement labels, cost/duration, adverse/unexecuted cases and cross-campaign confounds. The new master becomes the baseline only after this evidence is complete. No existing-machine IPs/credentials in the repository.
